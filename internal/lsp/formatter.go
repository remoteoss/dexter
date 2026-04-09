package lsp

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.lsp.dev/protocol"
)

//go:embed formatter_server.exs
var formatterScript string

const (
	// How long to wait for the persistent formatter to become ready before
	// falling back to mix format on a given request.
	formatterWaitTimeout = 5 * time.Second
	// How long a not-ready formatter process is allowed to live before being
	// killed and restarted. Also used as the hard cap inside the startup
	// goroutine to prevent leaked goroutines.
	formatterStuckTimeout = 30 * time.Second
)

type formatterProcess struct {
	cmd            *commandHandle
	stdin          io.WriteCloser
	stdout         io.ReadCloser
	mu             sync.Mutex
	formatterMtime time.Time     // mtime of .formatter.exs when process started
	startedAt      time.Time     // when the process was launched
	ready          chan struct{} // closed when the BEAM has sent the ready signal
	startErr       error         // non-nil if startup failed; set before ready is closed
	closed         chan struct{} // closed by Close(); makes alive() return false immediately
}

// commandHandle wraps the process so we can check liveness.
type commandHandle struct {
	process *os.Process
	done    chan struct{}
}

func (fp *formatterProcess) alive() bool {
	select {
	case <-fp.cmd.done:
		return false
	case <-fp.closed:
		return false
	default:
		return true
	}
}

// Ready blocks until the process has finished startup. Returns startErr if
// the BEAM failed to initialize, or ctx.Err() if the caller gives up first.
func (fp *formatterProcess) Ready(ctx context.Context) error {
	select {
	case <-fp.ready:
		return fp.startErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (fp *formatterProcess) Format(ctx context.Context, content, filename string) (string, error) {
	fp.mu.Lock()
	defer fp.mu.Unlock()

	// Build the entire request as a single buffer to avoid partial writes
	filenameBytes := []byte(filename)
	contentBytes := []byte(content)
	var req bytes.Buffer
	_ = binary.Write(&req, binary.BigEndian, uint16(len(filenameBytes)))
	req.Write(filenameBytes)
	_ = binary.Write(&req, binary.BigEndian, uint32(len(contentBytes)))
	req.Write(contentBytes)
	if _, err := fp.stdin.Write(req.Bytes()); err != nil {
		return "", fmt.Errorf("write request: %w", err)
	}

	type readResult struct {
		text string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		var status byte
		if err := binary.Read(fp.stdout, binary.BigEndian, &status); err != nil {
			ch <- readResult{err: fmt.Errorf("read status: %w", err)}
			return
		}
		var respLen uint32
		if err := binary.Read(fp.stdout, binary.BigEndian, &respLen); err != nil {
			ch <- readResult{err: fmt.Errorf("read length: %w", err)}
			return
		}
		buf := make([]byte, respLen)
		if _, err := io.ReadFull(fp.stdout, buf); err != nil {
			ch <- readResult{err: fmt.Errorf("read data: %w", err)}
			return
		}
		if status != 0 {
			ch <- readResult{err: &FormatError{Message: string(buf)}}
			return
		}
		ch <- readResult{text: string(buf)}
	}()

	select {
	case r := <-ch:
		return r.text, r.err
	case <-ctx.Done():
		// Kill the process to unblock the reader goroutine — the pipe reads
		// will fail once the process exits, preventing a leaked goroutine.
		_ = fp.cmd.process.Kill()
		<-ch
		return "", ctx.Err()
	}
}

// FormatError represents a formatting failure (e.g. syntax error in the source).
// The persistent process is still alive — this is not a protocol/crash error.
type FormatError struct {
	Message string
}

func (e *FormatError) Error() string {
	return e.Message
}

func (fp *formatterProcess) Close() {
	select {
	case <-fp.closed:
	default:
		close(fp.closed)
	}
	_ = fp.stdin.Close()
	_ = fp.cmd.process.Kill()
}

// startFormatterProcess launches the BEAM process and returns immediately.
// The returned process may not be ready yet — callers must check fp.Ready()
// before calling fp.Format(). Returns error only for immediate launch failures
// (missing binary, can't create pipes).
func (s *Server) startFormatterProcess(mixRoot, formatterExs string) (*formatterProcess, error) {
	scriptDir := filepath.Join(os.TempDir(), "dexter")
	if err := os.MkdirAll(scriptDir, 0755); err != nil {
		return nil, fmt.Errorf("create script dir: %w", err)
	}
	scriptPath := filepath.Join(scriptDir, "formatter_server.exs")
	if existing, err := os.ReadFile(scriptPath); err != nil || string(existing) != formatterScript {
		if err := os.WriteFile(scriptPath, []byte(formatterScript), 0644); err != nil {
			return nil, fmt.Errorf("write formatter script: %w", err)
		}
	}

	var mtime time.Time
	if info, err := os.Stat(formatterExs); err == nil {
		mtime = info.ModTime()
	}

	elixirBin := filepath.Join(filepath.Dir(s.mixBin), "elixir")
	cmd := exec.Command(elixirBin, scriptPath, mixRoot, formatterExs, s.projectRoot)
	cmd.Dir = mixRoot

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start formatter: %w", err)
	}

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	handle := &commandHandle{process: cmd.Process, done: done}

	fp := &formatterProcess{
		cmd:            handle,
		stdin:          stdin,
		stdout:         stdout,
		formatterMtime: mtime,
		startedAt:      time.Now(),
		ready:          make(chan struct{}),
		closed:         make(chan struct{}),
	}

	// Wait for the BEAM's ready signal asynchronously. Callers use fp.Ready()
	// to wait with their own timeout
	go func() {
		type readyResult struct {
			status byte
			err    error
		}
		readyCh := make(chan readyResult, 1)
		go func() {
			var status byte
			if err := binary.Read(stdout, binary.BigEndian, &status); err != nil {
				readyCh <- readyResult{err: err}
				return
			}
			var readyLen uint32
			if err := binary.Read(stdout, binary.BigEndian, &readyLen); err != nil {
				readyCh <- readyResult{err: err}
				return
			}
			readyCh <- readyResult{status: status}
		}()

		select {
		case r := <-readyCh:
			if r.err != nil {
				fp.startErr = fmt.Errorf("formatter ready: %w", r.err)
				_ = cmd.Process.Kill()
				<-done // wait for cmd.Wait() to finish copying stderr
				s.notifyOTPMismatch(stderrBuf.String())
			} else if r.status != 0 {
				fp.startErr = fmt.Errorf("formatter failed to initialize (status %d)", r.status)
				_ = cmd.Process.Kill()
			} else {
				log.Printf("Formatter: started persistent process for %s (pid %d)", formatterExs, cmd.Process.Pid)
			}
		case <-time.After(formatterStuckTimeout):
			fp.startErr = fmt.Errorf("formatter startup timed out")
			_ = cmd.Process.Kill()
		}
		close(fp.ready)
	}()

	return fp, nil
}

// getFormatter returns a cached formatter process (which may still be starting
// up). If none exists, it launches one and caches it immediately. The mutex is
// only held briefly — the slow BEAM startup happens asynchronously. Callers
// must call fp.Ready() before fp.Format() to wait for the process to be usable.
func (s *Server) getFormatter(mixRoot, formatterExs string) (*formatterProcess, error) {
	s.formattersMu.Lock()
	defer s.formattersMu.Unlock()

	if fp, ok := s.formatters[formatterExs]; ok && fp.alive() {
		// Restart if .formatter.exs has changed
		if info, err := os.Stat(formatterExs); err == nil && info.ModTime().After(fp.formatterMtime) {
			fp.Close()
			delete(s.formatters, formatterExs)
		} else {
			return fp, nil
		}
	}

	fp, err := s.startFormatterProcess(mixRoot, formatterExs)
	if err != nil {
		return nil, err
	}
	if s.formatters == nil {
		s.formatters = make(map[string]*formatterProcess)
	}
	s.formatters[formatterExs] = fp
	return fp, nil
}

// findFormatterConfig walks from the file's directory up to the mix root,
// returning the path to the nearest .formatter.exs. This handles subdirectory
// configs (e.g. config/.formatter.exs with different rules than the root).
func findFormatterConfig(filePath, mixRoot string) string {
	dir := filepath.Dir(filePath)
	for {
		candidate := filepath.Join(dir, ".formatter.exs")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if dir == mixRoot {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(mixRoot, ".formatter.exs")
}

// formatContent tries the persistent formatter, falling back to mix format.
//
// Startup-age policy:
//   - <5s old: wait for the process to become ready, then use it
//   - 5s–30s old: don't wait, fall back to mix format immediately
//   - >30s old and still not ready: kill and restart the stuck process
func (s *Server) formatContent(ctx context.Context, mixRoot, path, content string) (string, error) {
	formatterExs := findFormatterConfig(path, mixRoot)
	fp, err := s.getFormatter(mixRoot, formatterExs)
	if err != nil {
		log.Printf("Formatting: persistent formatter unavailable, falling back to mix format: %v", err)
		return s.formatWithMixFormat(ctx, mixRoot, path, content)
	}

	// Check if already ready (non-blocking)
	select {
	case <-fp.ready:
		if fp.startErr != nil {
			s.evictFormatter(formatterExs, fp)
			log.Printf("Formatting: persistent formatter failed to start, falling back to mix format: %v", fp.startErr)
			return s.formatWithMixFormat(ctx, mixRoot, path, content)
		}
	default:
		// Not ready yet — decide based on how long it's been starting
		age := time.Since(fp.startedAt)
		switch {
		case age > formatterStuckTimeout:
			// Stuck — kill and restart so the next request gets a fresh process
			log.Printf("Formatting: persistent formatter stuck (started %s ago), restarting", age.Truncate(time.Second))
			s.evictFormatter(formatterExs, fp)
			return s.formatWithMixFormat(ctx, mixRoot, path, content)

		case age > formatterWaitTimeout:
			// Taking too long — fall back without waiting
			log.Printf("Formatting: persistent formatter not ready after %s, falling back to mix format", age.Truncate(time.Millisecond))
			return s.formatWithMixFormat(ctx, mixRoot, path, content)

		default:
			// Recently started — wait for it
			if err := fp.Ready(ctx); err != nil {
				if ctx.Err() != nil {
					return "", err
				}
				s.evictFormatter(formatterExs, fp)
				log.Printf("Formatting: persistent formatter failed to start, falling back to mix format: %v", err)
				return s.formatWithMixFormat(ctx, mixRoot, path, content)
			}
		}
	}

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	start := time.Now()
	result, err := fp.Format(ctx, content, path)
	if err != nil {
		var formatErr *FormatError
		if errors.As(err, &formatErr) {
			log.Printf("Formatting: %s failed: %s", path, formatErr.Message)
		} else {
			s.evictFormatter(formatterExs, fp)
			log.Printf("Formatting: persistent formatter crashed: %v", err)
		}
		return "", err
	}

	log.Printf("Formatting: %s (%s, persistent)", path, time.Since(start))
	return result, nil
}

func (s *Server) evictFormatter(formatterExs string, fp *formatterProcess) {
	s.formattersMu.Lock()
	if s.formatters[formatterExs] == fp {
		delete(s.formatters, formatterExs)
	}
	s.formattersMu.Unlock()
	fp.Close()
}

func (s *Server) formatWithMixFormat(ctx context.Context, mixRoot, path, content string) (string, error) {
	if s.mixBin == "" {
		return "", fmt.Errorf("mix binary not found")
	}
	start := time.Now()
	cmd := s.mixCommand(ctx, mixRoot, "format", "--stdin-filename", path, "-")
	cmd.Stdin = strings.NewReader(content)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Printf("Formatting: mix format failed for %s (%s): %v\n%s", path, time.Since(start), err, stderr.String())
		s.notifyOTPMismatch(stderr.String())
		return "", err
	}
	log.Printf("Formatting: %s (%s, mix format)", path, time.Since(start))
	return stdout.String(), nil
}

// parseFormatError extracts line, column, and a clean message from an Elixir
// formatter error. Example input:
//
//	token missing on lib/foo.ex:246:4:\n     error: missing terminator: end
var formatErrorLineCol = regexp.MustCompile(`:(\d+):(\d+)`)
var formatErrorHintLine = regexp.MustCompile(`on line (\d+)`)

type formatErrorInfo struct {
	line, col uint32
	message   string
	hintLine  uint32 // 0 if no hint
	hint      string
}

func parseFormatError(msg string) formatErrorInfo {
	var info formatErrorInfo

	if m := formatErrorLineCol.FindStringSubmatch(msg); m != nil {
		if l, err := strconv.ParseUint(m[1], 10, 32); err == nil {
			info.line = uint32(l)
		}
		if c, err := strconv.ParseUint(m[2], 10, 32); err == nil {
			info.col = uint32(c)
		}
	}

	for _, part := range strings.Split(msg, "\n") {
		trimmed := strings.TrimSpace(part)
		if strings.HasPrefix(trimmed, "error:") && info.message == "" {
			info.message = trimmed
		}
		if strings.HasPrefix(trimmed, "hint:") {
			info.hint = trimmed
			if m := formatErrorHintLine.FindStringSubmatch(trimmed); m != nil {
				if l, err := strconv.ParseUint(m[1], 10, 32); err == nil {
					info.hintLine = uint32(l)
				}
			}
		}
	}

	if info.message == "" {
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			info.message = msg[:i]
		} else {
			info.message = msg
		}
	}
	return info
}

func (s *Server) publishFormatDiagnostic(uri protocol.DocumentURI, formatErr *FormatError) {
	if s.client == nil {
		return
	}
	info := parseFormatError(formatErr.Message)

	// LSP lines/cols are 0-based, Elixir's are 1-based
	line := info.line
	col := info.col
	if line > 0 {
		line--
	}
	if col > 0 {
		col--
	}

	diagnostics := []protocol.Diagnostic{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: line, Character: col},
				End:   protocol.Position{Line: line, Character: col},
			},
			Severity: protocol.DiagnosticSeverityError,
			Source:   "dexter",
			Message:  info.message,
		},
	}

	if info.hintLine > 0 {
		hintLine := info.hintLine - 1
		diagnostics = append(diagnostics, protocol.Diagnostic{
			Range: protocol.Range{
				Start: protocol.Position{Line: hintLine, Character: 0},
				End:   protocol.Position{Line: hintLine, Character: 0},
			},
			Severity: protocol.DiagnosticSeverityWarning,
			Source:   "dexter",
			Message:  info.hint,
		})
	}

	_ = s.client.PublishDiagnostics(context.Background(), &protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: diagnostics,
	})
}

func (s *Server) clearFormatDiagnostics(uri protocol.DocumentURI) {
	if s.client == nil {
		return
	}
	_ = s.client.PublishDiagnostics(context.Background(), &protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: []protocol.Diagnostic{},
	})
}

// computeMinimalEdits returns a minimal set of TextEdits to transform original
// into formatted. Instead of replacing the whole document (which causes the
// cursor to jump to the end of the file), this trims the common prefix and
// suffix lines and returns a single edit covering only the changed region.
func computeMinimalEdits(original, formatted string) []protocol.TextEdit {
	if original == formatted {
		return nil
	}

	oldLines := strings.SplitAfter(original, "\n")
	newLines := strings.SplitAfter(formatted, "\n")

	// Common prefix lines
	prefixLen := 0
	for prefixLen < len(oldLines) && prefixLen < len(newLines) && oldLines[prefixLen] == newLines[prefixLen] {
		prefixLen++
	}

	// Common suffix lines (not overlapping with prefix)
	suffixLen := 0
	for suffixLen < len(oldLines)-prefixLen && suffixLen < len(newLines)-prefixLen &&
		oldLines[len(oldLines)-1-suffixLen] == newLines[len(newLines)-1-suffixLen] {
		suffixLen++
	}

	startLine := prefixLen
	endLine := len(oldLines) - suffixLen

	var newText strings.Builder
	for i := prefixLen; i < len(newLines)-suffixLen; i++ {
		newText.WriteString(newLines[i])
	}

	return []protocol.TextEdit{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(startLine), Character: 0},
				End:   protocol.Position{Line: uint32(endLine), Character: 0},
			},
			NewText: newText.String(),
		},
	}
}

func (s *Server) closeFormatters() {
	s.formattersMu.Lock()
	defer s.formattersMu.Unlock()
	for _, fp := range s.formatters {
		fp.Close()
	}
	s.formatters = nil
}
