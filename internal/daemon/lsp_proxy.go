package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"sync"
	"time"
)

// The LSP proxy keeps an editor session alive when its daemon stops: a crash,
// a kill, a replacement by a newer build, or a daemon that left because its
// runtime files or its index went away. The editor started `dexter lsp` and
// will not start it again on its own, so a proxy that only exited would leave
// the editor without a language server and without a word.
//
// To rebuild the session on a new daemon, the proxy keeps what the session
// state is made of: the initialize request, the initialized notification, the
// last configuration, and each open document as its didOpen and its last
// didChange (Dexter uses full document sync, so the last didChange carries the
// whole text). It keeps the raw messages, never decoded copies, and it scans
// only those messages for their URI. For every other message it reads only the
// method and the id, which it needs to answer requests that a stopped daemon
// can no longer answer.

// reconnectDelays are the waits before each attempt to reach a daemon again.
// Each attempt can start a daemon, so a few attempts cover a daemon that is
// being replaced, and the last ones a daemon that crashes at start.
var reconnectDelays = []time.Duration{0, 250 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

// reconnectBudget bounds the whole reconnection, including daemon starts.
var reconnectBudget = 90 * time.Second

// replayTimeout bounds the wait for the new daemon's answer to the replayed
// initialize request.
var replayTimeout = 30 * time.Second

// maxQueuedFrames bounds the messages that the proxy holds while it has no
// daemon. An editor sends a few messages per keystroke; past the bound a
// request is answered with an error and a notification is dropped.
const maxQueuedFrames = 4096

// maxReusedFrame is the largest daemon message buffer that the proxy keeps
// for the next message.
const maxReusedFrame = 256 << 10

// lspContentModified is the JSON-RPC code of the answer to a request that a
// stopped daemon did not answer. Editors take ContentModified as "ask again"
// and do not show it as a failure.
const lspContentModified = -32801

// lspRequestCancelled answers a request that the editor canceled.
const lspRequestCancelled = -32800

// LSPConnectionLostError is a session that the proxy could not carry to a new
// daemon. The editor already received Message as window/showMessage.
type LSPConnectionLostError struct {
	Message string
	Err     error
}

func (e *LSPConnectionLostError) Error() string { return e.Message }
func (e *LSPConnectionLostError) Unwrap() error { return e.Err }

type lspProxy struct {
	ctx  context.Context
	root string
	in   *bufio.Reader
	out  io.Writer
	// dial reaches a daemon, starting one when necessary.
	dial func(ctx context.Context) (net.Conn, *bufio.Reader, error)

	outMu sync.Mutex

	// connMu orders writes to the daemon. It is held during a write, so code
	// that holds it must never wait for the daemon reader.
	connMu sync.Mutex
	conn   net.Conn // nil while there is no daemon
	queue  [][]byte // frames the editor sent while there was no daemon
	gen    int      // number of daemons this session has had, minus one
	// ended is closed when the editor ends the session (shutdown, exit, or
	// the end of its input); it stops a reconnection. exited is closed on
	// exit or the end of the input only.
	ended    chan struct{}
	endOnce  sync.Once
	exited   chan struct{}
	exitOnce sync.Once

	// stateMu guards the session state. It is never held during I/O.
	stateMu      sync.Mutex
	initialize   []byte // the initialize request body
	initID       []byte
	initAnswered bool
	initialized  []byte // the initialized frame
	config       []byte // the last didChangeConfiguration frame
	docs         map[string]*proxyDoc
	docSeq       uint64
	pending      map[string]string // ids of requests the daemon has not answered → method
	// serverPending holds the ids of the daemon's requests that the editor
	// has not answered, as the editor got them → the daemon's id (nil when
	// they are the same). After a reconnect the proxy gives the editor ids
	// of its own, so that a late answer to a stopped daemon's request never
	// answers the new daemon's request of the same id.
	serverPending map[string][]byte
	staleServer   map[string]struct{}
	replayID      string // id of a replayed initialize whose answer is dropped
	// progress holds the work-done progress tokens that the daemon began and
	// did not end. A daemon that stops cannot end them; the proxy does.
	progress map[string]struct{}
	// answeredLocally holds requests that the proxy answered itself.
	answeredLocally map[string]struct{}
	exiting         bool // the editor ends the session
	shutdown        bool // the editor asked for shutdown
}

func newLSPProxy(ctx context.Context, root string, in io.Reader, out io.Writer, dial func(context.Context) (net.Conn, *bufio.Reader, error)) *lspProxy {
	return &lspProxy{
		ctx:             ctx,
		root:            root,
		in:              bufio.NewReaderSize(in, 64<<10),
		out:             out,
		dial:            dial,
		ended:           make(chan struct{}),
		exited:          make(chan struct{}),
		docs:            make(map[string]*proxyDoc),
		pending:         make(map[string]string),
		serverPending:   make(map[string][]byte),
		staleServer:     make(map[string]struct{}),
		progress:        make(map[string]struct{}),
		answeredLocally: make(map[string]struct{}),
	}
}

// run serves the session until the editor ends it, or until no daemon can be
// reached; then the editor has been told why.
func (p *lspProxy) run(conn net.Conn, reader *bufio.Reader) error {
	p.conn = conn
	go p.readEditor()
	daemonDone := p.startReadDaemon(conn, reader)
	for {
		<-daemonDone
		_ = conn.Close()
		if p.ending() {
			p.endSession()
			return nil
		}
		var err error
		conn, daemonDone, err = p.reconnect()
		if err != nil {
			if p.ending() {
				p.endSession()
				return nil
			}
			return err
		}
	}
}

// exitWait bounds the wait for the editor's exit notification after the
// proxy answered shutdown itself.
var exitWait = 10 * time.Second

// endSession finishes a session that the editor ends while no daemon serves
// it. Shutdown succeeds, because the daemon that had to shut down is gone;
// other requests, sent or queued, fail. Then the proxy waits for exit or the
// end of the editor's input, as a server does after shutdown.
func (p *lspProxy) endSession() {
	p.stateMu.Lock()
	pending := p.pending
	p.pending = make(map[string]string)
	p.stateMu.Unlock()
	p.connMu.Lock()
	queue := p.queue
	p.queue = nil
	p.connMu.Unlock()
	for _, frame := range queue {
		if _, body, ok := splitFrame(frame); ok {
			if msg := peekMessage(body); msg.isRequest() {
				pending[string(msg.id)] = msg.method
			}
		}
	}
	for id, method := range pending {
		if method == "shutdown" {
			p.answerNull([]byte(id))
			continue
		}
		p.answerError([]byte(id), lspContentModified, "Dexter: the session ended before this request was answered.")
	}
	select {
	case <-p.exited:
	case <-time.After(exitWait):
	}
}

func (p *lspProxy) ending() bool {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.exiting || p.shutdown
}

// readEditor forwards the editor's messages and keeps the session state.
func (p *lspProxy) readEditor() {
	defer p.editorGone()
	for {
		frame, body, err := readProxyFrame(p.in)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("LSP proxy: reading from the editor: %v", err)
			}
			return
		}
		p.connMu.Lock()
		frame, msg, covered, drop := p.inspect(frame, body)
		if !drop {
			p.forwardLocked(frame, msg, covered)
		}
		p.connMu.Unlock()
	}
}

// inspect reads one editor message and keeps what the session state needs.
// A panic here is a bug in the scan: the message is then forwarded as it is,
// without state, because losing the editor's message would be worse. The
// caller holds connMu.
func (p *lspProxy) inspect(frame []byte, body int) (out []byte, msg peeked, covered, drop bool) {
	defer func() {
		if value := recover(); value != nil {
			log.Printf("Error: LSP proxy: panic while reading an editor message: %v\n%s", value, debug.Stack())
			out, msg, covered, drop = frame, peeked{}, false, false
		}
	}()
	msg = peekMessage(frame[body:])
	if msg.isResponse() {
		stale, daemonID := p.serverResponse(msg.id)
		if stale {
			return frame, msg, false, true
		}
		if daemonID != nil {
			rewritten, err := withID(frame[body:], string(daemonID))
			if err != nil {
				return frame, msg, false, true
			}
			frame = frameMessage(rewritten)
		}
		return frame, msg, false, false
	}
	return frame, msg, p.record(frame, body, msg), false
}

// editorGone ends the session from the editor side: the daemon sees the end of
// its input and closes the session, and a reconnection stops.
func (p *lspProxy) editorGone() {
	p.stateMu.Lock()
	p.exiting = true
	p.stateMu.Unlock()
	p.endOnce.Do(func() { close(p.ended) })
	p.exitOnce.Do(func() { close(p.exited) })
	p.connMu.Lock()
	defer p.connMu.Unlock()
	if p.conn != nil {
		if cw, ok := p.conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = p.conn.Close()
		}
	}
}

// record updates the session state with one editor message. It reports
// whether a replay of the state covers the message, so that it need not be
// queued while there is no daemon.
func (p *lspProxy) record(frame []byte, body int, msg peeked) bool {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	switch msg.method {
	case "initialize":
		if msg.id != nil {
			p.initialize = append([]byte(nil), frame[body:]...)
			p.initID = msg.id
			p.initAnswered = false
		}
		return false
	case "initialized":
		p.initialized = frame
		return true
	case "workspace/didChangeConfiguration":
		p.config = frame
		return true
	case "textDocument/didOpen":
		if msg.uri == "" {
			return false
		}
		p.docSeq++
		p.docs[msg.uri] = &proxyDoc{seq: p.docSeq, open: frame}
		return true
	case "textDocument/didChange":
		if doc := p.docs[msg.uri]; doc != nil {
			return doc.applyChange(frame, body)
		}
		return false
	case "textDocument/didClose":
		if msg.uri == "" {
			return false
		}
		delete(p.docs, msg.uri)
		return true
	case "textDocument/didSave":
		if doc := p.docs[msg.uri]; doc != nil {
			doc.changed = false
		}
		return false
	case "shutdown":
		p.shutdown = true
		p.endOnce.Do(func() { close(p.ended) })
	case "exit":
		p.exiting = true
		p.endOnce.Do(func() { close(p.ended) })
		p.exitOnce.Do(func() { close(p.exited) })
	}
	return false
}

// forwardLocked sends one editor message to the daemon, or queues it while
// there is none. The caller holds connMu.
func (p *lspProxy) forwardLocked(frame []byte, msg peeked, covered bool) {
	if msg.method == "$/cancelRequest" && !p.passCancel(frame) {
		return
	}
	if p.conn == nil {
		if covered {
			return
		}
		if len(p.queue) >= maxQueuedFrames {
			if msg.isRequest() {
				p.answerError(msg.id, lspContentModified, "Dexter: the workspace daemon is restarting; repeat the request.")
			}
			return
		}
		p.queue = append(p.queue, frame)
		return
	}
	if msg.isRequest() {
		p.stateMu.Lock()
		p.pending[string(msg.id)] = msg.method
		p.stateMu.Unlock()
	}
	// A failed write means the daemon is gone. The daemon reader sees the end
	// of the stream and reconnects; a request is in pending and gets an answer
	// then, and the state covers the notifications that matter.
	_, _ = p.conn.Write(frame)
}

// maxAnsweredLocally bounds the ids of requests that the proxy answered
// itself; a cancel for an older one is only dropped later than it could be.
const maxAnsweredLocally = 1024

// markAnswered records a request that the proxy answered itself, so that a
// later $/cancelRequest for it does not go to a daemon that never had it.
func (p *lspProxy) markAnswered(id []byte) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if len(p.answeredLocally) >= maxAnsweredLocally {
		p.answeredLocally = make(map[string]struct{})
	}
	p.answeredLocally[string(id)] = struct{}{}
}

// passCancel decides what happens to a $/cancelRequest. A cancel for a
// request that waits in the queue removes it and answers RequestCancelled; a
// cancel for a request that the proxy already answered is dropped. Others go
// to the daemon. The caller holds connMu.
func (p *lspProxy) passCancel(frame []byte) bool {
	_, body, ok := splitFrame(frame)
	if !ok {
		return true
	}
	var msg struct {
		Params struct {
			ID json.RawMessage `json:"id"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil || len(msg.Params.ID) == 0 {
		return true
	}
	id := bytes.TrimSpace(msg.Params.ID)
	for i, queued := range p.queue {
		_, queuedBody, ok := splitFrame(queued)
		if !ok {
			continue
		}
		if q := peekMessage(queuedBody); q.isRequest() && bytes.Equal(q.id, id) {
			p.queue = append(p.queue[:i:i], p.queue[i+1:]...)
			p.answerError(id, lspRequestCancelled, "Dexter: the request was canceled.")
			return false
		}
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if _, answered := p.answeredLocally[string(id)]; answered {
		delete(p.answeredLocally, string(id))
		return false
	}
	return p.conn != nil
}

// serverResponse reads the id of an editor response. stale reports an answer
// to a request of a daemon that is gone, which is dropped. daemonID, when not
// nil, is the id that the current daemon gave the request.
func (p *lspProxy) serverResponse(id []byte) (stale bool, daemonID []byte) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	key := string(id)
	if daemonID, ok := p.serverPending[key]; ok {
		delete(p.serverPending, key)
		return false, daemonID
	}
	if _, ok := p.staleServer[key]; ok {
		delete(p.staleServer, key)
		return true, nil
	}
	return false, nil
}

// startReadDaemon runs readDaemon and returns a channel that is closed when
// the daemon stream ends.
func (p *lspProxy) startReadDaemon(conn net.Conn, reader *bufio.Reader) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.readDaemon(conn, reader)
	}()
	return done
}

// readDaemon forwards the daemon's messages to the editor until the daemon
// stream ends.
func (p *lspProxy) readDaemon(conn net.Conn, reader *bufio.Reader) {
	// Nothing keeps a daemon frame after it is written, so one buffer serves
	// every message; answers of a few hundred KB are common.
	var buf []byte
	for {
		frame, body, err := readProxyFrameInto(reader, buf)
		if err != nil {
			return
		}
		if cap(frame) <= maxReusedFrame {
			buf = frame
		}
		action, rewritten := p.fromDaemon(frame, body)
		switch action {
		case toEditor:
			if rewritten != nil {
				frame = rewritten
			}
			p.writeOut(frame)
		case answerNull:
			// The daemon reader must never wait on connMu, which a write to
			// the daemon holds.
			id := append([]byte(nil), peekMessage(frame[body:]).id...)
			go func() {
				p.connMu.Lock()
				defer p.connMu.Unlock()
				if p.conn == conn {
					_, _ = conn.Write(nullResult(id))
				}
			}()
		}
	}
}

type daemonAction int

const (
	toEditor daemonAction = iota
	dropIt
	answerNull
)

func nullResult(id []byte) []byte {
	return frameMessage([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, id)))
}

// fromDaemon updates the state with one daemon message and says what to do
// with it. rewritten, when not nil, is the frame to send instead.
func (p *lspProxy) fromDaemon(frame []byte, body int) (action daemonAction, rewritten []byte) {
	// A panic in the scan is a bug; the message still reaches the editor.
	defer func() {
		if value := recover(); value != nil {
			log.Printf("Error: LSP proxy: panic while reading a daemon message: %v\n%s", value, debug.Stack())
			action, rewritten = toEditor, nil
		}
	}()
	msg := peekMessage(frame[body:])
	switch {
	case msg.isResponse():
		key := string(msg.id)
		p.stateMu.Lock()
		defer p.stateMu.Unlock()
		if p.replayID != "" && key == p.replayID {
			return dropIt, nil
		}
		delete(p.pending, key)
		if p.initID != nil && key == string(p.initID) {
			p.initAnswered = true
		}
		return toEditor, nil
	case msg.isRequest():
		p.stateMu.Lock()
		defer p.stateMu.Unlock()
		if p.gen > 0 && msg.method == "client/registerCapability" {
			// The editor holds this registration from the first session
			// already, and a second one with the same id is an error in some
			// editors. The proxy answers it.
			return answerNull, nil
		}
		if p.gen == 0 {
			p.serverPending[string(msg.id)] = nil
			return toEditor, nil
		}
		editorID := strconv.Quote(fmt.Sprintf("dexter-proxy-%d-%s", p.gen, msg.id))
		body, err := withID(frame[body:], editorID)
		if err != nil {
			p.serverPending[string(msg.id)] = nil
			return toEditor, nil
		}
		p.serverPending[editorID] = append([]byte(nil), msg.id...)
		return toEditor, frameMessage(body)
	case msg.method == "$/progress":
		p.trackProgress(frame[body:])
	}
	return toEditor, nil
}

// trackProgress keeps the tokens of work-done progress that is open.
func (p *lspProxy) trackProgress(body []byte) {
	var msg struct {
		Params struct {
			Token json.RawMessage `json:"token"`
			Value struct {
				Kind string `json:"kind"`
			} `json:"value"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil || len(msg.Params.Token) == 0 {
		return
	}
	token := string(msg.Params.Token)
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	switch msg.Params.Value.Kind {
	case "begin":
		p.progress[token] = struct{}{}
	case "end":
		delete(p.progress, token)
	}
}

// endProgress ends, in the editor, the progress that a stopped daemon left
// open, so that the editor does not show it for ever.
func (p *lspProxy) endProgress() {
	p.stateMu.Lock()
	tokens := p.progress
	p.progress = make(map[string]struct{})
	p.stateMu.Unlock()
	for token := range tokens {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"$/progress","params":{"token":%s,"value":{"kind":"end","message":"the workspace daemon stopped"}}}`, token)
		p.writeOut(frameMessage([]byte(body)))
	}
}

func (p *lspProxy) writeOut(frame []byte) {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	_, _ = p.out.Write(frame)
}

func (p *lspProxy) answerError(id []byte, code int, message string) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	})
	if err == nil {
		p.writeOut(frameMessage(body))
	}
}

func (p *lspProxy) answerNull(id []byte) {
	p.writeOut(nullResult(id))
}

func (p *lspProxy) showError(message string) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "window/showMessage",
		"params":  map[string]any{"type": 1, "message": message},
	})
	if err == nil {
		p.writeOut(frameMessage(body))
	}
}

// reconnect answers the requests that the stopped daemon left open, reaches a
// daemon again, and replays the session on it. On failure it tells the editor
// why.
func (p *lspProxy) reconnect() (net.Conn, <-chan struct{}, error) {
	p.connMu.Lock()
	p.conn = nil
	p.connMu.Unlock()

	p.stateMu.Lock()
	pending := p.pending
	p.pending = make(map[string]string)
	for id := range p.serverPending {
		p.staleServer[id] = struct{}{}
	}
	p.serverPending = make(map[string][]byte)
	initOpen := p.initialize != nil && !p.initAnswered
	initID := string(p.initID)
	p.stateMu.Unlock()
	for id, method := range pending {
		if method == "initialize" && initOpen && id == initID {
			continue // the replay sends it again with its own id
		}
		p.answerError([]byte(id), lspContentModified,
			"Dexter: the workspace daemon stopped while this request was open; Dexter reconnects. Repeat the request.")
		p.markAnswered([]byte(id))
	}
	p.endProgress()
	log.Printf("LSP proxy: the connection to the workspace daemon ended; reconnecting")

	ctx, cancel := context.WithTimeout(p.ctx, reconnectBudget)
	defer cancel()
	go func() {
		select {
		case <-p.ended:
			cancel()
		case <-ctx.Done():
		}
	}()

	var lastErr error
	for attempt, delay := range reconnectDelays {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			break
		}
		if p.ending() {
			return nil, nil, errors.New("the editor ended the session")
		}
		if _, err := os.Stat(p.root); err != nil {
			return nil, nil, p.fail(fmt.Sprintf(
				"Dexter stopped: the project directory %s is no longer available (%v). The volume may have been ejected, or the directory moved or removed. Restart the editor when the project is available again.",
				p.root, err), err)
		}
		conn, reader, err := p.dial(ctx)
		if err != nil {
			// The cause of a failed start is what the user needs; the end of
			// the reconnect budget is not.
			if lastErr == nil || !isContextError(err) {
				lastErr = err
			}
			log.Printf("LSP proxy: reconnect attempt %d: %v", attempt+1, err)
			if permanentDialError(err) {
				break
			}
			continue
		}
		daemonDone, err := p.replay(conn, reader)
		if err != nil {
			_ = conn.Close()
			lastErr = err
			log.Printf("LSP proxy: replaying the session, attempt %d: %v", attempt+1, err)
			continue
		}
		log.Printf("LSP proxy: reconnected to the workspace daemon")
		return conn, daemonDone, nil
	}
	if p.ending() {
		return nil, nil, errors.New("the editor ended the session")
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return nil, nil, p.fail("Dexter lost the connection to its workspace daemon and could not reconnect. "+ExplainStartupFailure(p.root, lastErr), lastErr)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// permanentDialError reports an error that another attempt cannot fix: a
// daemon that is newer than this proxy, or another spelling of the root.
func permanentDialError(err error) bool {
	var incompatible *IncompatibleDaemonError
	if errors.As(err, &incompatible) && !incompatible.ClientNewer {
		return true
	}
	var mismatch *RootMismatchError
	return errors.As(err, &mismatch)
}

// fail tells the editor that the session cannot continue, answers what is
// still open, and returns the error to exit with. What is open includes an
// initialize that reconnect kept for the replay, and requests that a failed
// replay sent.
func (p *lspProxy) fail(message string, err error) error {
	p.showError(message)
	p.stateMu.Lock()
	open := p.pending
	p.pending = make(map[string]string)
	if p.initialize != nil && !p.initAnswered {
		open[string(p.initID)] = "initialize"
	}
	p.stateMu.Unlock()
	p.connMu.Lock()
	queue := p.queue
	p.queue = nil
	p.connMu.Unlock()
	for _, frame := range queue {
		_, body, ok := splitFrame(frame)
		if !ok {
			continue
		}
		if msg := peekMessage(body); msg.isRequest() {
			open[string(msg.id)] = msg.method
		}
	}
	for id := range open {
		p.answerError([]byte(id), lspInternalError, message)
	}
	return &LSPConnectionLostError{Message: message, Err: err}
}

// splitFrame returns the body of a frame that readProxyFrame made.
func splitFrame(frame []byte) ([]byte, []byte, bool) {
	for i := 0; i+3 < len(frame); i++ {
		if frame[i] == '\r' && frame[i+1] == '\n' && frame[i+2] == '\r' && frame[i+3] == '\n' {
			return frame[:i+4], frame[i+4:], true
		}
	}
	return nil, nil, false
}

// replay rebuilds the session on a new daemon connection and then makes it
// the current one. The editor's messages wait on connMu meanwhile, so none is
// lost and none overtakes the replay. After the answer to initialize, the
// daemon's messages are read while the replay writes (the returned channel
// closes when they end): a daemon that answers each document while the
// proxy does not read would fill the socket and block both sides.
func (p *lspProxy) replay(conn net.Conn, reader *bufio.Reader) (<-chan struct{}, error) {
	p.connMu.Lock()
	defer p.connMu.Unlock()

	p.stateMu.Lock()
	p.gen++
	gen := p.gen
	initialize := p.initialize
	initID := p.initID
	answered := p.initAnswered
	initialized := p.initialized
	config := p.config
	docs := make([]*proxyDoc, 0, len(p.docs))
	for _, doc := range p.docs {
		docs = append(docs, doc)
	}
	p.stateMu.Unlock()
	sort.Slice(docs, func(i, j int) bool { return docs[i].seq < docs[j].seq })

	if initialize != nil {
		waitID := initID
		if answered {
			// The editor has its answer already; this one is for the proxy.
			replayID := strconv.Quote(fmt.Sprintf("dexter-proxy-reconnect-%d", gen))
			body, err := withID(initialize, replayID)
			if err != nil {
				return nil, err
			}
			p.stateMu.Lock()
			p.replayID = replayID
			p.stateMu.Unlock()
			waitID = []byte(replayID)
			if _, err := conn.Write(frameMessage(body)); err != nil {
				return nil, err
			}
		} else {
			p.stateMu.Lock()
			p.pending[string(initID)] = "initialize"
			p.stateMu.Unlock()
			if _, err := conn.Write(frameMessage(initialize)); err != nil {
				return nil, err
			}
		}
		if err := p.awaitResponse(conn, reader, waitID, answered); err != nil {
			return nil, err
		}
	}
	daemonDone := p.startReadDaemon(conn, reader)
	for _, frame := range [][]byte{initialized, config} {
		if frame == nil {
			continue
		}
		if _, err := conn.Write(frame); err != nil {
			return nil, err
		}
	}
	for _, doc := range docs {
		frames, err := doc.replayFrames()
		if err != nil {
			log.Printf("LSP proxy: a document cannot be replayed: %v", err)
			continue
		}
		for _, frame := range frames {
			if _, err := conn.Write(frame); err != nil {
				return nil, err
			}
		}
	}
	queue := p.queue
	p.queue = nil
	for i, frame := range queue {
		_, body, ok := splitFrame(frame)
		if ok {
			if msg := peekMessage(body); msg.isRequest() {
				p.stateMu.Lock()
				p.pending[string(msg.id)] = msg.method
				p.stateMu.Unlock()
			}
		}
		if _, err := conn.Write(frame); err != nil {
			// The new daemon is gone too. This frame is pending, so the next
			// reconnect answers it; the frames after it were not sent, so
			// they wait in the queue for the next daemon.
			p.queue = append(queue[i+1:len(queue):len(queue)], p.queue...)
			break
		}
	}
	p.conn = conn
	return daemonDone, nil
}

// awaitResponse reads the new daemon's messages until the answer to the
// replayed initialize arrives, and passes the others to the editor. An answer
// that the editor never had is passed on too.
func (p *lspProxy) awaitResponse(conn net.Conn, reader *bufio.Reader, id []byte, drop bool) error {
	_ = conn.SetReadDeadline(time.Now().Add(replayTimeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		frame, body, err := readProxyFrame(reader)
		if err != nil {
			return fmt.Errorf("waiting for the answer to initialize: %w", err)
		}
		msg := peekMessage(frame[body:])
		if msg.isResponse() && string(msg.id) == string(id) {
			p.stateMu.Lock()
			if drop {
				p.replayID = ""
			} else {
				delete(p.pending, string(id))
				p.initAnswered = true
			}
			p.stateMu.Unlock()
			if errorResponse(frame[body:]) {
				if !drop {
					p.writeOut(frame)
				}
				return errors.New("the new daemon refused the replayed initialize")
			}
			if !drop {
				p.writeOut(frame)
			}
			return nil
		}
		action, rewritten := p.fromDaemon(frame, body)
		switch action {
		case toEditor:
			if rewritten != nil {
				frame = rewritten
			}
			p.writeOut(frame)
		case answerNull:
			// The caller holds connMu, so this goroutine may write.
			if _, err := conn.Write(nullResult(msg.id)); err != nil {
				return err
			}
		}
	}
}

func errorResponse(body []byte) bool {
	var msg struct {
		Error json.RawMessage `json:"error"`
	}
	return json.Unmarshal(body, &msg) == nil && len(msg.Error) > 0 && string(msg.Error) != "null"
}

// withID returns an initialize body with another id.
func withID(body []byte, id string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	fields["id"] = json.RawMessage(id)
	return json.Marshal(fields)
}
