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

//go:embed beam_server.exs
var beamServerScript string

const (
	// How long to wait for the persistent BEAM to become ready before
	// falling back to mix format on a given request.
	beamWaitTimeout = 5 * time.Second
	// How long a not-ready BEAM process is allowed to live before being
	// killed and restarted. Also used as the hard cap inside the startup
	// goroutine to prevent leaked goroutines.
	beamStuckTimeout = 30 * time.Second

	// Service tags for the BEAM server protocol
	serviceFormatter byte = 0x00
	serviceCodeIntel byte = 0x01
)

type beamProcess struct {
	cmd       *commandHandle
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	mu        sync.Mutex
	startedAt time.Time     // when the process was launched
	ready     chan struct{} // closed when the BEAM has sent the ready signal
	startErr  error         // non-nil if startup failed; set before ready is closed
	closed    chan struct{} // closed by Close(); makes alive() return false immediately
}

// commandHandle wraps the process so we can check liveness.
type commandHandle struct {
	process *os.Process
	done    chan struct{}
}

func (bp *beamProcess) alive() bool {
	select {
	case <-bp.cmd.done:
		return false
	case <-bp.closed:
		return false
	default:
		return true
	}
}

// Ready blocks until the process has finished startup. Returns startErr if
// the BEAM failed to initialize, or ctx.Err() if the caller gives up first.
func (bp *beamProcess) Ready(ctx context.Context) error {
	select {
	case <-bp.ready:
		return bp.startErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Format sends a format request to the BEAM process. The formatterExs path
// tells the BEAM which .formatter.exs config to use (starting a new formatter
// child if needed).
func (bp *beamProcess) Format(ctx context.Context, content, filename, formatterExs string) (string, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	configPathBytes := []byte(formatterExs)
	filenameBytes := []byte(filename)
	contentBytes := []byte(content)
	var req bytes.Buffer
	req.WriteByte(serviceFormatter)
	_ = binary.Write(&req, binary.BigEndian, uint16(len(configPathBytes)))
	req.Write(configPathBytes)
	_ = binary.Write(&req, binary.BigEndian, uint16(len(filenameBytes)))
	req.Write(filenameBytes)
	_ = binary.Write(&req, binary.BigEndian, uint32(len(contentBytes)))
	req.Write(contentBytes)
	if _, err := bp.stdin.Write(req.Bytes()); err != nil {
		return "", fmt.Errorf("write request: %w", err)
	}

	type readResult struct {
		text string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		var status byte
		if err := binary.Read(bp.stdout, binary.BigEndian, &status); err != nil {
			ch <- readResult{err: fmt.Errorf("read status: %w", err)}
			return
		}
		var respLen uint32
		if err := binary.Read(bp.stdout, binary.BigEndian, &respLen); err != nil {
			ch <- readResult{err: fmt.Errorf("read length: %w", err)}
			return
		}
		buf := make([]byte, respLen)
		if _, err := io.ReadFull(bp.stdout, buf); err != nil {
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
		_ = bp.cmd.process.Kill()
		<-ch
		return "", ctx.Err()
	}
}

// ErlangSourceResult holds the resolved source location for an Erlang function.
type ErlangSourceResult struct {
	File string
	Line int
}

// ErlangSource asks the BEAM's CodeIntel service to resolve an Erlang module/function
// to its source file and line number.
func (bp *beamProcess) ErlangSource(ctx context.Context, module, function string, arity int) (*ErlangSourceResult, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	moduleBytes := []byte(module)
	functionBytes := []byte(function)
	arityByte := byte(255) // 255 = unspecified
	if arity >= 0 && arity < 255 {
		arityByte = byte(arity)
	}

	var req bytes.Buffer
	req.WriteByte(serviceCodeIntel) // service tag
	req.WriteByte(0)                // op: erlang_source
	_ = binary.Write(&req, binary.BigEndian, uint16(len(moduleBytes)))
	req.Write(moduleBytes)
	_ = binary.Write(&req, binary.BigEndian, uint16(len(functionBytes)))
	req.Write(functionBytes)
	req.WriteByte(arityByte)
	if _, err := bp.stdin.Write(req.Bytes()); err != nil {
		return nil, fmt.Errorf("write code_intel request: %w", err)
	}

	type readResult struct {
		result *ErlangSourceResult
		err    error
	}
	ch := make(chan readResult, 1)
	go func() {
		var status byte
		if err := binary.Read(bp.stdout, binary.BigEndian, &status); err != nil {
			ch <- readResult{err: fmt.Errorf("read status: %w", err)}
			return
		}
		var fileLen uint16
		if err := binary.Read(bp.stdout, binary.BigEndian, &fileLen); err != nil {
			ch <- readResult{err: fmt.Errorf("read file length: %w", err)}
			return
		}
		fileBuf := make([]byte, fileLen)
		if _, err := io.ReadFull(bp.stdout, fileBuf); err != nil {
			ch <- readResult{err: fmt.Errorf("read file: %w", err)}
			return
		}
		var line uint32
		if err := binary.Read(bp.stdout, binary.BigEndian, &line); err != nil {
			ch <- readResult{err: fmt.Errorf("read line: %w", err)}
			return
		}
		if status != 0 {
			ch <- readResult{err: fmt.Errorf("erlang source not found")}
			return
		}
		ch <- readResult{result: &ErlangSourceResult{File: string(fileBuf), Line: int(line)}}
	}()

	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		_ = bp.cmd.process.Kill()
		<-ch
		return nil, ctx.Err()
	}
}

// ErlangDocs asks the BEAM's CodeIntel service for the documentation of an
// Erlang module or function. Returns pre-formatted markdown, or empty string
// if no docs are available (e.g. OTP < 24 or undocumented function).
func (bp *beamProcess) ErlangDocs(ctx context.Context, module, function string, arity int) (string, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	moduleBytes := []byte(module)
	functionBytes := []byte(function)
	arityByte := byte(255)
	if arity >= 0 && arity < 255 {
		arityByte = byte(arity)
	}

	var req bytes.Buffer
	req.WriteByte(serviceCodeIntel)
	req.WriteByte(1) // op: erlang_docs
	_ = binary.Write(&req, binary.BigEndian, uint16(len(moduleBytes)))
	req.Write(moduleBytes)
	_ = binary.Write(&req, binary.BigEndian, uint16(len(functionBytes)))
	req.Write(functionBytes)
	req.WriteByte(arityByte)
	if _, err := bp.stdin.Write(req.Bytes()); err != nil {
		return "", fmt.Errorf("write code_intel request: %w", err)
	}

	type readResult struct {
		doc string
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		var status byte
		if err := binary.Read(bp.stdout, binary.BigEndian, &status); err != nil {
			ch <- readResult{err: fmt.Errorf("read status: %w", err)}
			return
		}
		var docLen uint32
		if err := binary.Read(bp.stdout, binary.BigEndian, &docLen); err != nil {
			ch <- readResult{err: fmt.Errorf("read doc length: %w", err)}
			return
		}
		docBuf := make([]byte, docLen)
		if _, err := io.ReadFull(bp.stdout, docBuf); err != nil {
			ch <- readResult{err: fmt.Errorf("read doc: %w", err)}
			return
		}
		if status != 0 {
			ch <- readResult{doc: ""}
			return
		}
		ch <- readResult{doc: string(docBuf)}
	}()

	select {
	case r := <-ch:
		return r.doc, r.err
	case <-ctx.Done():
		_ = bp.cmd.process.Kill()
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

func (bp *beamProcess) Close() {
	select {
	case <-bp.closed:
	default:
		close(bp.closed)
	}
	_ = bp.stdin.Close()
	_ = bp.cmd.process.Kill()
}

// startBeamProcess launches a BEAM process for the given build root and returns
// immediately. The returned process may not be ready yet — callers must check
// bp.Ready() before sending requests.
func (s *Server) startBeamProcess(buildRoot string) (*beamProcess, error) {
	scriptDir := filepath.Join(os.TempDir(), "dexter")
	if err := os.MkdirAll(scriptDir, 0755); err != nil {
		return nil, fmt.Errorf("create script dir: %w", err)
	}
	scriptPath := filepath.Join(scriptDir, "beam_server.exs")
	if existing, err := os.ReadFile(scriptPath); err != nil || string(existing) != beamServerScript {
		if err := os.WriteFile(scriptPath, []byte(beamServerScript), 0644); err != nil {
			return nil, fmt.Errorf("write beam server script: %w", err)
		}
	}

	elixirBin := filepath.Join(filepath.Dir(s.mixBin), "elixir")
	cmd := exec.Command(elixirBin, scriptPath, buildRoot)
	cmd.Dir = buildRoot

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
		return nil, fmt.Errorf("start BEAM: %w", err)
	}

	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()

	handle := &commandHandle{process: cmd.Process, done: done}

	bp := &beamProcess{
		cmd:       handle,
		stdin:     stdin,
		stdout:    stdout,
		startedAt: time.Now(),
		ready:     make(chan struct{}),
		closed:    make(chan struct{}),
	}

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
				bp.startErr = fmt.Errorf("BEAM ready: %w", r.err)
				_ = cmd.Process.Kill()
				<-done
				s.notifyOTPMismatch(stderrBuf.String())
			} else if r.status != 0 {
				bp.startErr = fmt.Errorf("BEAM failed to initialize (status %d)", r.status)
				_ = cmd.Process.Kill()
			} else {
				log.Printf("BEAM: started persistent process (pid %d)", cmd.Process.Pid)
			}
		case <-time.After(beamStuckTimeout):
			bp.startErr = fmt.Errorf("BEAM startup timed out")
			_ = cmd.Process.Kill()
		}
		close(bp.ready)
	}()

	return bp, nil
}

// findBuildRoot walks up from dir looking for a _build directory, bounded by
// projectRoot. Returns the directory containing _build, or projectRoot if none
// is found.
func (s *Server) findBuildRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "_build")); err == nil {
			return dir
		}
		if dir == s.projectRoot {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return s.projectRoot
}

// getBeamProcess returns a BEAM process for the given build root, starting one
// if needed. Files sharing the same _build share the same BEAM process.
func (s *Server) getBeamProcess(ctx context.Context, buildRoot string) *beamProcess {
	s.beamMu.Lock()
	defer s.beamMu.Unlock()

	if bp, ok := s.beams[buildRoot]; ok && bp.alive() {
		return bp
	}

	if s.mixBin == "" {
		return nil
	}

	bp, err := s.startBeamProcess(buildRoot)
	if err != nil {
		log.Printf("BEAM: failed to start for %s: %v", buildRoot, err)
		return nil
	}
	if s.beams == nil {
		s.beams = make(map[string]*beamProcess)
	}
	s.beams[buildRoot] = bp
	return bp
}

// findFormatterConfig walks from the file's directory up to the project root,
// returning the path to the nearest .formatter.exs. This handles subdirectory
// configs (e.g. config/.formatter.exs with different rules than the root) and
// umbrella projects where .formatter.exs lives at the umbrella root above the
// app's mix root.
func findFormatterConfig(filePath, projectRoot string) string {
	dir := filepath.Dir(filePath)
	for {
		candidate := filepath.Join(dir, ".formatter.exs")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if dir == projectRoot {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(projectRoot, ".formatter.exs")
}

// formatContent tries the persistent BEAM process, falling back to mix format.
//
// Startup-age policy:
//   - <5s old: wait for the process to become ready, then use it
//   - 5s–30s old: don't wait, fall back to mix format immediately
//   - >30s old and still not ready: kill and restart the stuck process
func (s *Server) formatContent(ctx context.Context, mixRoot, path, content string) (string, error) {
	formatterExs := findFormatterConfig(path, s.projectRoot)
	buildRoot := s.findBuildRoot(filepath.Dir(path))
	bp := s.getBeamProcess(ctx, buildRoot)
	if bp == nil {
		log.Printf("Formatting: BEAM process unavailable, falling back to mix format")
		return s.formatWithMixFormat(ctx, mixRoot, path, content)
	}

	// Check if already ready (non-blocking)
	select {
	case <-bp.ready:
		if bp.startErr != nil {
			s.evictBeam(bp)
			log.Printf("Formatting: BEAM process failed to start, falling back to mix format: %v", bp.startErr)
			return s.formatWithMixFormat(ctx, mixRoot, path, content)
		}
	default:
		// Not ready yet — decide based on how long it's been starting
		age := time.Since(bp.startedAt)
		switch {
		case age > beamStuckTimeout:
			log.Printf("Formatting: BEAM process stuck (started %s ago), restarting", age.Truncate(time.Second))
			s.evictBeam(bp)
			return s.formatWithMixFormat(ctx, mixRoot, path, content)

		case age > beamWaitTimeout:
			log.Printf("Formatting: BEAM process not ready after %s, falling back to mix format", age.Truncate(time.Millisecond))
			return s.formatWithMixFormat(ctx, mixRoot, path, content)

		default:
			if err := bp.Ready(ctx); err != nil {
				if ctx.Err() != nil {
					return "", err
				}
				s.evictBeam(bp)
				log.Printf("Formatting: BEAM process failed to start, falling back to mix format: %v", err)
				return s.formatWithMixFormat(ctx, mixRoot, path, content)
			}
		}
	}

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	start := time.Now()
	result, err := bp.Format(ctx, content, path, formatterExs)
	if err != nil {
		var formatErr *FormatError
		if errors.As(err, &formatErr) {
			log.Printf("Formatting: %s failed: %s", path, formatErr.Message)
		} else {
			s.evictBeam(bp)
			log.Printf("Formatting: BEAM process crashed: %v", err)
		}
		return "", err
	}

	log.Printf("Formatting: %s (%s, persistent)", path, time.Since(start))
	return result, nil
}

func (s *Server) evictBeam(bp *beamProcess) {
	s.beamMu.Lock()
	for key, b := range s.beams {
		if b == bp {
			delete(s.beams, key)
			break
		}
	}
	s.beamMu.Unlock()
	bp.Close()
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

func (s *Server) closeBeams() {
	s.beamMu.Lock()
	defer s.beamMu.Unlock()
	for _, bp := range s.beams {
		bp.Close()
	}
	s.beams = nil
}
