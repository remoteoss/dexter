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

	// Frame types for the multiplexed BEAM protocol
	frameRequest      byte = 0x00
	frameResponse     byte = 0x01
	frameNotification byte = 0x02
	frameReady        byte = 0x03

	formatterOpFormat byte = 0x00

	codeIntelOpErlangSource   byte = 0x00
	codeIntelOpErlangDocs     byte = 0x01
	codeIntelOpWarmOTPModules byte = 0x02
	codeIntelOpErlangExports  byte = 0x03
	codeIntelOpRuntimeInfo    byte = 0x04

	beamNotificationOTPModulesReady  byte = 0x00
	beamNotificationOTPModulesFailed byte = 0x01
)

type beamProcess struct {
	cmd       *commandHandle
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    *bytes.Buffer // rolling stderr capture for crash diagnostics
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[uint32]chan beamResponse
	nextReqID uint32
	startedAt time.Time     // when the process was launched
	ready     chan struct{} // closed when the BEAM has sent the ready signal
	startErr  error         // non-nil if startup failed; set before ready is closed
	startOnce sync.Once
	closed    chan struct{} // closed by Close(); makes alive() return false immediately
	notify    func(beamNotification)
}

type beamResponse struct {
	status  byte
	payload []byte
	err     error
}

type beamNotification struct {
	op      byte
	payload []byte
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

// recentStderr returns the tail of the captured stderr buffer (up to 512 bytes)
// for inclusion in error messages when the BEAM process crashes.
func (bp *beamProcess) recentStderr() string {
	if bp.stderr == nil {
		return ""
	}
	b := bp.stderr.Bytes()
	if len(b) > 512 {
		b = b[len(b)-512:]
	}
	return strings.TrimSpace(string(b))
}

// wrapError annotates a read/write error with recent stderr output if the
// BEAM process has died, making crash diagnostics visible in log messages.
func (bp *beamProcess) wrapError(err error) error {
	if err == nil {
		return nil
	}
	if !bp.alive() {
		if stderr := bp.recentStderr(); stderr != "" {
			return fmt.Errorf("%w\nBEAM stderr:\n%s", err, stderr)
		}
	}
	return err
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

func (bp *beamProcess) finishStartup(err error) {
	bp.startOnce.Do(func() {
		bp.startErr = err
		close(bp.ready)
	})
}

func (bp *beamProcess) addPending() (uint32, chan beamResponse) {
	bp.pendingMu.Lock()
	defer bp.pendingMu.Unlock()

	bp.nextReqID++
	reqID := bp.nextReqID
	respCh := make(chan beamResponse, 1)
	if bp.pending == nil {
		bp.pending = make(map[uint32]chan beamResponse)
	}
	bp.pending[reqID] = respCh
	return reqID, respCh
}

func (bp *beamProcess) removePending(reqID uint32) chan beamResponse {
	bp.pendingMu.Lock()
	defer bp.pendingMu.Unlock()

	respCh := bp.pending[reqID]
	delete(bp.pending, reqID)
	return respCh
}

func (bp *beamProcess) failPending(err error) {
	err = bp.wrapError(err)

	bp.pendingMu.Lock()
	pending := bp.pending
	bp.pending = make(map[uint32]chan beamResponse)
	bp.pendingMu.Unlock()

	for _, respCh := range pending {
		respCh <- beamResponse{err: err}
	}
}

func readByte(r io.Reader) (byte, error) {
	var value [1]byte
	if _, err := io.ReadFull(r, value[:]); err != nil {
		return 0, err
	}
	return value[0], nil
}

func readUint32(r io.Reader) (uint32, error) {
	var value uint32
	if err := binary.Read(r, binary.BigEndian, &value); err != nil {
		return 0, err
	}
	return value, nil
}

func readPayload(r io.Reader, size uint32) ([]byte, error) {
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func readStatusPayload(r io.Reader) (byte, []byte, error) {
	status, err := readByte(r)
	if err != nil {
		return 0, nil, err
	}
	size, err := readUint32(r)
	if err != nil {
		return 0, nil, err
	}
	payload, err := readPayload(r, size)
	if err != nil {
		return 0, nil, err
	}
	return status, payload, nil
}

func (bp *beamProcess) readLoop() {
	for {
		frameType, err := readByte(bp.stdout)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				err = fmt.Errorf("read frame type: %w", err)
			}
			bp.finishStartup(fmt.Errorf("BEAM read loop: %w", err))
			bp.failPending(err)
			return
		}

		switch frameType {
		case frameReady:
			status, payload, err := readStatusPayload(bp.stdout)
			if err != nil {
				startErr := fmt.Errorf("read ready frame: %w", err)
				bp.finishStartup(startErr)
				bp.failPending(startErr)
				return
			}
			if status != 0 {
				msg := "BEAM failed to initialize"
				if len(payload) > 0 {
					msg = fmt.Sprintf("%s: %s", msg, strings.TrimSpace(string(payload)))
				}
				startErr := errors.New(msg)
				bp.finishStartup(startErr)
				bp.failPending(startErr)
				return
			}
			log.Printf("BEAM: started persistent process (pid %d)", bp.cmd.process.Pid)
			bp.finishStartup(nil)

		case frameResponse:
			reqID, err := readUint32(bp.stdout)
			if err != nil {
				respErr := fmt.Errorf("read response request id: %w", err)
				bp.finishStartup(respErr)
				bp.failPending(respErr)
				return
			}
			status, payload, err := readStatusPayload(bp.stdout)
			if err != nil {
				respErr := fmt.Errorf("read response payload: %w", err)
				bp.finishStartup(respErr)
				bp.failPending(respErr)
				return
			}
			if respCh := bp.removePending(reqID); respCh != nil {
				respCh <- beamResponse{status: status, payload: payload}
			}

		case frameNotification:
			op, err := readByte(bp.stdout)
			if err != nil {
				notifErr := fmt.Errorf("read notification op: %w", err)
				bp.finishStartup(notifErr)
				bp.failPending(notifErr)
				return
			}
			size, err := readUint32(bp.stdout)
			if err != nil {
				notifErr := fmt.Errorf("read notification payload length: %w", err)
				bp.finishStartup(notifErr)
				bp.failPending(notifErr)
				return
			}
			payload, err := readPayload(bp.stdout, size)
			if err != nil {
				notifErr := fmt.Errorf("read notification payload: %w", err)
				bp.finishStartup(notifErr)
				bp.failPending(notifErr)
				return
			}
			if bp.notify != nil {
				bp.notify(beamNotification{op: op, payload: payload})
			}

		default:
			protocolErr := fmt.Errorf("unexpected BEAM frame type: %d", frameType)
			bp.finishStartup(protocolErr)
			bp.failPending(protocolErr)
			return
		}
	}
}

// doRequest sends a framed request to the BEAM and waits for the matching
// response. The permanent read loop demultiplexes responses by request ID and
// routes notifications to the cache layer.
func (bp *beamProcess) doRequest(ctx context.Context, service, op byte, payload []byte, handleResp func(status byte, payload []byte) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	reqID, respCh := bp.addPending()

	bp.writeMu.Lock()
	if ctx.Err() != nil {
		bp.writeMu.Unlock()
		bp.removePending(reqID)
		return ctx.Err()
	}
	if !bp.alive() {
		bp.writeMu.Unlock()
		bp.removePending(reqID)
		return bp.wrapError(fmt.Errorf("BEAM process is not alive"))
	}

	var frame bytes.Buffer
	frame.WriteByte(frameRequest)
	_ = binary.Write(&frame, binary.BigEndian, reqID)
	frame.WriteByte(service)
	frame.WriteByte(op)
	_ = binary.Write(&frame, binary.BigEndian, uint32(len(payload)))
	frame.Write(payload)

	_, err := bp.stdin.Write(frame.Bytes())
	bp.writeMu.Unlock()
	if err != nil {
		bp.removePending(reqID)
		return bp.wrapError(fmt.Errorf("write request: %w", err))
	}

	select {
	case resp := <-respCh:
		if resp.err != nil {
			return resp.err
		}
		return handleResp(resp.status, resp.payload)
	case <-ctx.Done():
		bp.removePending(reqID)
		return ctx.Err()
	case <-bp.closed:
		bp.removePending(reqID)
		return bp.wrapError(fmt.Errorf("BEAM process closed"))
	}
}

// Format sends a format request to the BEAM process. The formatterExs path
// tells the BEAM which .formatter.exs config to use (starting a new formatter
// child if needed).
func (bp *beamProcess) Format(ctx context.Context, content, filename, formatterExs string) (string, error) {
	var result string
	configPathBytes := []byte(formatterExs)
	filenameBytes := []byte(filename)
	contentBytes := []byte(content)
	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.BigEndian, uint16(len(configPathBytes)))
	payload.Write(configPathBytes)
	_ = binary.Write(&payload, binary.BigEndian, uint16(len(filenameBytes)))
	payload.Write(filenameBytes)
	_ = binary.Write(&payload, binary.BigEndian, uint32(len(contentBytes)))
	payload.Write(contentBytes)

	err := bp.doRequest(ctx, serviceFormatter, formatterOpFormat, payload.Bytes(), func(status byte, payload []byte) error {
		if status != 0 {
			return &FormatError{Message: string(payload)}
		}
		result = string(payload)
		return nil
	})
	return result, err
}

// ErlangSourceResult holds the resolved source location for an Erlang function.
type ErlangSourceResult struct {
	File string
	Line int
}

// ErlangSource asks the BEAM's CodeIntel service to resolve an Erlang module/function
// to its source file and line number.
func (bp *beamProcess) ErlangSource(ctx context.Context, module, function string, arity int) (*ErlangSourceResult, error) {
	var result *ErlangSourceResult
	arityByte := byte(255)
	if arity >= 0 && arity < 255 {
		arityByte = byte(arity)
	}

	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.BigEndian, uint16(len(module)))
	payload.WriteString(module)
	_ = binary.Write(&payload, binary.BigEndian, uint16(len(function)))
	payload.WriteString(function)
	payload.WriteByte(arityByte)

	err := bp.doRequest(ctx, serviceCodeIntel, codeIntelOpErlangSource, payload.Bytes(), func(status byte, payload []byte) error {
		reader := bytes.NewReader(payload)
		var fileLen uint16
		if err := binary.Read(reader, binary.BigEndian, &fileLen); err != nil {
			return fmt.Errorf("read file length: %w", err)
		}
		fileBuf := make([]byte, fileLen)
		if _, err := io.ReadFull(reader, fileBuf); err != nil {
			return fmt.Errorf("read file: %w", err)
		}
		var line uint32
		if err := binary.Read(reader, binary.BigEndian, &line); err != nil {
			return fmt.Errorf("read line: %w", err)
		}
		if status != 0 {
			return fmt.Errorf("erlang source not found")
		}
		result = &ErlangSourceResult{File: string(fileBuf), Line: int(line)}
		return nil
	})
	return result, err
}

// ErlangDocs asks the BEAM's CodeIntel service for the documentation of an
// Erlang module or function. Returns pre-formatted markdown, or empty string
// if no docs are available (e.g. OTP < 24 or undocumented function).
func (bp *beamProcess) ErlangDocs(ctx context.Context, module, function string, arity int) (string, error) {
	var doc string
	arityByte := byte(255)
	if arity >= 0 && arity < 255 {
		arityByte = byte(arity)
	}

	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.BigEndian, uint16(len(module)))
	payload.WriteString(module)
	_ = binary.Write(&payload, binary.BigEndian, uint16(len(function)))
	payload.WriteString(function)
	payload.WriteByte(arityByte)

	err := bp.doRequest(ctx, serviceCodeIntel, codeIntelOpErlangDocs, payload.Bytes(), func(status byte, payload []byte) error {
		reader := bytes.NewReader(payload)
		var docLen uint32
		if err := binary.Read(reader, binary.BigEndian, &docLen); err != nil {
			return fmt.Errorf("read doc length: %w", err)
		}
		docBuf := make([]byte, docLen)
		if _, err := io.ReadFull(reader, docBuf); err != nil {
			return fmt.Errorf("read doc: %w", err)
		}
		if status == 0 {
			doc = string(docBuf)
		}
		return nil
	})
	return doc, err
}

// ErlangExport represents a single exported function from an Erlang module.
type ErlangExport struct {
	Function string
	Arity    int
	Params   string
}

// ErlangRuntimeInfo identifies the BEAM runtime backing a process.
type ErlangRuntimeInfo struct {
	OTPRelease  string
	CodeRootDir string
}

// ErlangRuntimeInfo asks the BEAM's CodeIntel service for a stable runtime
// fingerprint. This lets the LSP share OTP completion caches across build
// roots that resolve to the same OTP install.
func (bp *beamProcess) ErlangRuntimeInfo(ctx context.Context) (*ErlangRuntimeInfo, error) {
	var info *ErlangRuntimeInfo
	err := bp.doRequest(ctx, serviceCodeIntel, codeIntelOpRuntimeInfo, nil, func(status byte, payload []byte) error {
		if status != 0 {
			if len(payload) > 0 {
				return fmt.Errorf("runtime info failed: %s", strings.TrimSpace(string(payload)))
			}
			return fmt.Errorf("runtime info failed")
		}

		reader := bytes.NewReader(payload)
		var releaseLen uint16
		if err := binary.Read(reader, binary.BigEndian, &releaseLen); err != nil {
			return fmt.Errorf("read otp release length: %w", err)
		}
		releaseBuf := make([]byte, releaseLen)
		if _, err := io.ReadFull(reader, releaseBuf); err != nil {
			return fmt.Errorf("read otp release: %w", err)
		}

		var rootLen uint16
		if err := binary.Read(reader, binary.BigEndian, &rootLen); err != nil {
			return fmt.Errorf("read code root length: %w", err)
		}
		rootBuf := make([]byte, rootLen)
		if _, err := io.ReadFull(reader, rootBuf); err != nil {
			return fmt.Errorf("read code root: %w", err)
		}

		info = &ErlangRuntimeInfo{
			OTPRelease:  string(releaseBuf),
			CodeRootDir: string(rootBuf),
		}
		return nil
	})
	return info, err
}

// WarmOTPModuleNames asks the BEAM's CodeIntel service to ensure OTP Erlang
// modules are loaded. Completion data is pushed back asynchronously via a
// notification frame once the background warmup finishes.
func (bp *beamProcess) WarmOTPModuleNames(ctx context.Context) error {
	return bp.doRequest(ctx, serviceCodeIntel, codeIntelOpWarmOTPModules, nil, func(status byte, payload []byte) error {
		if status != 0 {
			if len(payload) > 0 {
				return fmt.Errorf("warm OTP modules: %s", strings.TrimSpace(string(payload)))
			}
			return fmt.Errorf("warm OTP modules failed")
		}
		return nil
	})
}

// ErlangExports asks the BEAM's CodeIntel service for the exported functions
// of a single Erlang module.
func (bp *beamProcess) ErlangExports(ctx context.Context, module string) ([]ErlangExport, error) {
	var exports []ErlangExport
	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.BigEndian, uint16(len(module)))
	payload.WriteString(module)

	err := bp.doRequest(ctx, serviceCodeIntel, codeIntelOpErlangExports, payload.Bytes(), func(status byte, payload []byte) error {
		if status != 0 {
			if len(payload) > 0 {
				return fmt.Errorf("erlang exports failed: %s", strings.TrimSpace(string(payload)))
			}
			return fmt.Errorf("erlang exports failed")
		}

		reader := bytes.NewReader(payload)
		var exportCount uint16
		if err := binary.Read(reader, binary.BigEndian, &exportCount); err != nil {
			return fmt.Errorf("read export count: %w", err)
		}
		exports = make([]ErlangExport, 0, exportCount)
		for i := 0; i < int(exportCount); i++ {
			var funcLen uint16
			if err := binary.Read(reader, binary.BigEndian, &funcLen); err != nil {
				return fmt.Errorf("read func name length: %w", err)
			}
			funcBuf := make([]byte, funcLen)
			if _, err := io.ReadFull(reader, funcBuf); err != nil {
				return fmt.Errorf("read func name: %w", err)
			}
			var arity uint8
			if err := binary.Read(reader, binary.BigEndian, &arity); err != nil {
				return fmt.Errorf("read arity: %w", err)
			}
			var paramsLen uint16
			if err := binary.Read(reader, binary.BigEndian, &paramsLen); err != nil {
				return fmt.Errorf("read params length: %w", err)
			}
			paramsBuf := make([]byte, paramsLen)
			if _, err := io.ReadFull(reader, paramsBuf); err != nil {
				return fmt.Errorf("read params: %w", err)
			}
			exports = append(exports, ErlangExport{
				Function: string(funcBuf),
				Arity:    int(arity),
				Params:   string(paramsBuf),
			})
		}
		return nil
	})
	return exports, err
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
	bp.closeWithReason("caller did not provide a reason")
}

func (bp *beamProcess) closeWithReason(reason string) {
	if reason == "" {
		reason = "no reason provided"
	}

	bp.writeMu.Lock()
	defer bp.writeMu.Unlock()

	select {
	case <-bp.closed:
		return // already closed
	default:
		close(bp.closed)
	}
	bp.finishStartup(fmt.Errorf("BEAM closed"))
	log.Printf("BEAM: closing process (pid %d): %s", bp.cmd.process.Pid, reason)
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
		stderr:    &stderrBuf,
		pending:   make(map[uint32]chan beamResponse),
		startedAt: time.Now(),
		ready:     make(chan struct{}),
		closed:    make(chan struct{}),
		notify: func(notification beamNotification) {
			s.handleBeamNotification(buildRoot, notification)
		},
	}

	go func() {
		select {
		case <-bp.ready:
			if bp.startErr != nil {
				_ = cmd.Process.Kill()
				<-done
				s.notifyOTPMismatch(stderrBuf.String())
			}
		case <-time.After(beamStuckTimeout):
			bp.finishStartup(fmt.Errorf("BEAM startup timed out"))
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	go bp.readLoop()

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

	if bp, ok := s.beams[buildRoot]; ok {
		if bp.alive() {
			return bp
		}
		log.Printf("BEAM: process for %s is dead, restarting", buildRoot)
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
			s.evictBeam(bp, fmt.Sprintf("formatContent: startup finished with error: %v", bp.startErr))
			log.Printf("Formatting: BEAM process failed to start, falling back to mix format: %v", bp.startErr)
			return s.formatWithMixFormat(ctx, mixRoot, path, content)
		}
	default:
		// Not ready yet — decide based on how long it's been starting
		age := time.Since(bp.startedAt)
		switch {
		case age > beamStuckTimeout:
			log.Printf("Formatting: BEAM process stuck (started %s ago), restarting", age.Truncate(time.Second))
			s.evictBeam(bp, fmt.Sprintf("formatContent: startup exceeded %s without becoming ready", beamStuckTimeout))
			return s.formatWithMixFormat(ctx, mixRoot, path, content)

		case age > beamWaitTimeout:
			log.Printf("Formatting: BEAM process not ready after %s, falling back to mix format", age.Truncate(time.Millisecond))
			return s.formatWithMixFormat(ctx, mixRoot, path, content)

		default:
			if err := bp.Ready(ctx); err != nil {
				if ctx.Err() != nil {
					return "", err
				}
				s.evictBeam(bp, fmt.Sprintf("formatContent: Ready failed: %v", err))
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
		} else if ctx.Err() != nil {
			// Context cancelled — the BEAM is fine, the editor just moved on.
		} else {
			s.evictBeam(bp, fmt.Sprintf("formatContent: Format request failed: %v", err))
			log.Printf("Formatting: BEAM process crashed: %v", err)
		}
		return "", err
	}

	log.Printf("Formatting: %s (%s, persistent)", path, time.Since(start))
	return result, nil
}

func (s *Server) evictBeam(bp *beamProcess, reason string) {
	if reason == "" {
		reason = "no reason provided"
	}

	buildRoot := ""
	s.beamMu.Lock()
	for key, b := range s.beams {
		if b == bp {
			delete(s.beams, key)
			buildRoot = key
			break
		}
	}
	s.beamMu.Unlock()

	if buildRoot != "" {
		log.Printf("BEAM: evicting process for %s (pid %d): %s", buildRoot, bp.cmd.process.Pid, reason)
	} else {
		log.Printf("BEAM: evicting untracked process (pid %d): %s", bp.cmd.process.Pid, reason)
	}

	bp.closeWithReason("evicted: " + reason)
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
		bp.closeWithReason("server shutdown")
	}
	s.beams = nil
}
