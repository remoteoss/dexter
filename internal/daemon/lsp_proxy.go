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
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/remoteoss/dexter/internal/lsp"
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
//
// One editor session can hold files of several projects: an editor or an
// agent that starts in a directory with several clones and worktrees. Each
// file goes to the daemon of its own project (an upstream), and the proxy
// keeps one connection, with its own replay state, per project. The upstream
// of the start root is the primary; a session whose files all belong to the
// start root has only that one, and its messages pass as they did before.

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
	// rootKey is the start root without symlinks, to know a file of the
	// start root under another spelling.
	rootKey string
	// container is true when the start root only holds other projects
	// (store.IsProjectContainer). Then no daemon serves the start root, and
	// the proxy answers initialize itself.
	container bool
	in        *bufio.Reader
	out       io.Writer
	// dial reaches the daemon of a project root, starting one when necessary.
	dial func(ctx context.Context, root string) (net.Conn, *bufio.Reader, error)

	outMu sync.Mutex

	// ended is closed when the editor ends the session (shutdown, exit, or
	// the end of its input); it stops a reconnection. exited is closed on
	// exit or the end of the input only.
	ended    chan struct{}
	endOnce  sync.Once
	exited   chan struct{}
	exitOnce sync.Once
	// fatal gets the error of the primary upstream when no daemon can serve
	// the start root any more; the proxy then exits with it.
	fatal chan error
	// loops counts the upstream goroutines that still run.
	loops sync.WaitGroup

	// mu guards the session state. It is never held during I/O, and it is
	// taken after the locks of an upstream, never before them.
	mu           sync.Mutex
	initialize   []byte // the initialize request body
	initID       []byte
	initAnswered bool
	initialized  []byte // the initialized frame
	config       []byte // the last didChangeConfiguration frame
	// registered holds the registration ids of client/registerCapability
	// requests that went to the editor. The editor keeps a registration
	// across daemons and for all projects, so a second one is answered by
	// the proxy.
	registered map[string]bool
	// serverPending holds the ids of the daemons' requests that the editor
	// has not answered, as the editor got them. The proxy gives the editor
	// ids of its own for each daemon after the first daemon of the primary,
	// so that a late answer to a stopped daemon's request never answers the
	// new daemon's request of the same id, and two daemons' ids never meet.
	serverPending map[string]serverRequest
	staleServer   map[string]struct{}
	// answeredLocally holds requests that the proxy answered itself.
	answeredLocally map[string]struct{}
	exiting         bool // the editor ends the session
	inputGone       bool // the editor's input ended
	shutdown        bool // the editor asked for shutdown
	primary         *upstream
	upstreams       map[string]*upstream // by root key
	upstreamList    []*upstream          // in the order they came
	upstreamSeq     int
	// fanouts are requests that went to several upstreams (shutdown,
	// workspace/symbol), by the id of each part.
	fanouts     map[string]*fanout
	fanByEditor map[string]*fanout
	fanSeq      int

	// The routing state below is used only by the editor reader goroutine.
	roots    map[string]string // directory -> project root, "" for the start root's own files
	rootKeys map[string]string // project root -> root key
	lastURI  string            // the raw JSON string of the last routed URI
	lastUp   *upstream
	lastFall bool
	// fallbackUp is, for a container, the upstream of the last file that has
	// a project of its own. A file outside every project (a standard library
	// file that a definition opened) goes there, and its document stays on
	// the upstream that its didOpen went to (fallbackDocs).
	fallbackUp   *upstream
	fallbackDocs map[string]*upstream
	// completionUp answered the last completion; completionItem/resolve goes
	// there.
	completionUp *upstream
}

type serverRequest struct {
	up       *upstream
	daemonID []byte // nil when the editor got the daemon's own id
}

func newLSPProxy(ctx context.Context, root string, container bool, in io.Reader, out io.Writer, dial func(context.Context, string) (net.Conn, *bufio.Reader, error)) *lspProxy {
	return &lspProxy{
		ctx:             ctx,
		root:            root,
		rootKey:         resolveRootKey(root),
		container:       container,
		in:              bufio.NewReaderSize(in, 64<<10),
		out:             out,
		dial:            dial,
		ended:           make(chan struct{}),
		exited:          make(chan struct{}),
		fatal:           make(chan error, 1),
		registered:      make(map[string]bool),
		serverPending:   make(map[string]serverRequest),
		staleServer:     make(map[string]struct{}),
		answeredLocally: make(map[string]struct{}),
		upstreams:       make(map[string]*upstream),
		fanouts:         make(map[string]*fanout),
		fanByEditor:     make(map[string]*fanout),
		roots:           make(map[string]string),
		rootKeys:        make(map[string]string),
		fallbackDocs:    make(map[string]*upstream),
	}
}

// run serves the session until the editor ends it, or until no daemon can
// serve the start root; then the editor has been told why. conn is the
// connection to the start root's daemon, or nil for a container.
func (p *lspProxy) run(conn net.Conn, reader *bufio.Reader) error {
	if conn != nil {
		u := p.newUpstream(p.root, 0)
		u.conn = conn
		p.primary = u
		p.upstreams[p.rootKey] = u
		p.upstreamList = append(p.upstreamList, u)
		p.loops.Add(1)
		go u.loop(conn, reader)
	}
	go p.readEditor()
	select {
	case err := <-p.fatal:
		return err
	case <-p.ended:
	}
	p.loops.Wait()
	select {
	case err := <-p.fatal:
		if !p.ending() {
			return err
		}
	default:
	}
	select {
	case <-p.exited:
	case <-time.After(exitWait):
	}
	return nil
}

// exitWait bounds the wait for the editor's exit notification after the
// proxy answered shutdown itself.
var exitWait = 10 * time.Second

func (p *lspProxy) ending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
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
		p.dispatch(frame, body)
	}
}

// dispatch sends one editor message where it belongs and keeps what the
// session state needs. A message that the scan cannot read goes to the
// primary as it is.
func (p *lspProxy) dispatch(frame []byte, body int) {
	msg, ok := safePeek(frame[body:])
	if !ok {
		if u := p.primary; u != nil {
			u.connMu.Lock()
			u.forwardLocked(frame, peeked{}, false)
			u.connMu.Unlock()
		}
		return
	}
	if msg.isResponse() {
		p.editorResponse(frame, body, msg)
		return
	}
	switch msg.method {
	case "initialize":
		if u := p.primary; u != nil {
			u.connMu.Lock()
			covered := p.recordSession(frame, body, msg)
			u.forwardLocked(frame, msg, covered)
			u.connMu.Unlock()
			return
		}
		p.recordSession(frame, body, msg)
		p.answerInitialize(msg.id)
		return
	case "initialized", "workspace/didChangeConfiguration", "exit":
		p.broadcast(frame, body, msg, true)
		return
	case "workspace/didChangeWorkspaceFolders", "$/setTrace", "$/logTrace":
		p.broadcast(frame, body, msg, false)
		return
	case "shutdown", "workspace/symbol":
		p.fanOut(frame, body, msg)
		return
	case "$/cancelRequest":
		p.cancel(frame, body, msg)
		return
	case "workspace/didChangeWatchedFiles":
		p.splitWatchedFiles(frame, body, msg)
		return
	case "window/workDoneProgress/cancel":
		p.cancelProgress(frame, body, msg)
		return
	}
	var u *upstream
	switch {
	case msg.rawURI != nil:
		u = p.upstreamFor(msg.rawURI, msg.method)
	case msg.method == "completionItem/resolve" && p.completionUp != nil:
		u = p.completionUp
	default:
		u = p.primary
	}
	if msg.method == "textDocument/completion" {
		p.completionUp = u
	}
	if u == nil {
		// A container with no project for this message: an empty answer, as
		// a server with nothing to say gives.
		if msg.isRequest() {
			p.answerNull(msg.id)
		}
		return
	}
	u.connMu.Lock()
	covered := u.safeRecord(frame, body, msg)
	u.forwardLocked(frame, msg, covered)
	u.connMu.Unlock()
}

// safePeek is peekMessage that reports false on a panic, which is a bug in
// the scan.
func safePeek(body []byte) (msg peeked, ok bool) {
	defer func() {
		if value := recover(); value != nil {
			log.Printf("Error: LSP proxy: panic while reading an editor message: %v\n%s", value, debug.Stack())
			msg, ok = peeked{}, false
		}
	}()
	return peekMessage(body), true
}

// safeRecord is record that keeps no state on a panic. The message is then
// forwarded as it is, because losing the editor's message would be worse.
func (u *upstream) safeRecord(frame []byte, body int, msg peeked) (covered bool) {
	defer func() {
		if value := recover(); value != nil {
			log.Printf("Error: LSP proxy: panic while keeping the state of an editor message: %v\n%s", value, debug.Stack())
			covered = false
		}
	}()
	return u.record(frame, body, msg)
}

// recordSession updates the session state with one editor message. It
// reports whether a replay of the state covers the message, so that it need
// not be queued while there is no daemon.
func (p *lspProxy) recordSession(frame []byte, body int, msg peeked) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch msg.method {
	case "initialize":
		if msg.id == nil {
			return false
		}
		p.initialize = append([]byte(nil), frame[body:]...)
		p.initID = msg.id
		p.initAnswered = false
		// The replay sends the kept initialize, so a queued copy would
		// reach the next daemon twice.
		return true
	case "initialized":
		p.initialized = frame
		return true
	case "workspace/didChangeConfiguration":
		p.config = frame
		return true
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

// initializeResult is the answer to initialize that a daemon gives.
var initializeResult = sync.OnceValue(func() []byte {
	raw, err := json.Marshal(lsp.NewInitializeResult())
	if err != nil {
		panic(err)
	}
	return raw
})

// answerInitialize answers the editor's initialize for a container, where no
// daemon serves the start root. The answer is the one that every daemon
// gives, so the editor sees no difference.
func (p *lspProxy) answerInitialize(id []byte) {
	if id == nil {
		return
	}
	p.mu.Lock()
	p.initAnswered = true
	p.mu.Unlock()
	body := make([]byte, 0, 64+len(initializeResult()))
	body = append(body, `{"jsonrpc":"2.0","id":`...)
	body = append(body, id...)
	body = append(body, `,"result":`...)
	body = append(body, initializeResult()...)
	body = append(body, '}')
	p.writeOut(frameMessage(body))
}

// allUpstreams returns the upstreams in the order they came.
func (p *lspProxy) allUpstreams() []*upstream {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*upstream(nil), p.upstreamList...)
}

// broadcast sends one message to every upstream. With record, the session
// state keeps it, under the locks of every upstream, so that no replay can
// send it a second time.
func (p *lspProxy) broadcast(frame []byte, body int, msg peeked, record bool) {
	ups := p.allUpstreams()
	for _, u := range ups {
		u.connMu.Lock()
	}
	covered := false
	if record {
		covered = p.recordSession(frame, body, msg)
	}
	for _, u := range ups {
		u.forwardLocked(frame, msg, covered)
	}
	for i := len(ups) - 1; i >= 0; i-- {
		ups[i].connMu.Unlock()
	}
	if len(ups) == 0 && msg.isRequest() {
		p.answerNull(msg.id)
	}
}

// editorGone ends the session from the editor side: each daemon sees the end
// of its input and closes the session, and a reconnection stops.
//
// An upstream can be in its initialize exchange now (a reconnect, or the first
// connect of a new upstream): that connection is closed too, and a replay that
// ends after this closes the connection that it made current (see replay).
// Else that daemon would never see the end of its input, and the proxy would
// wait for it for ever.
func (p *lspProxy) editorGone() {
	p.mu.Lock()
	p.exiting = true
	p.inputGone = true
	p.mu.Unlock()
	p.endOnce.Do(func() { close(p.ended) })
	p.exitOnce.Do(func() { close(p.exited) })
	for _, u := range p.allUpstreams() {
		u.connMu.Lock()
		closeWrite(u.conn)
		closeWrite(u.replaying)
		u.connMu.Unlock()
	}
}

// editorInputGone reports whether the editor's input ended.
func (p *lspProxy) editorInputGone() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inputGone
}

// closeWrite ends the daemon's input on conn, so that the daemon closes the
// session. A nil conn is ignored.
func closeWrite(conn net.Conn) {
	if conn == nil {
		return
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		_ = conn.Close()
	}
}

// editorResponse passes the editor's answer to a daemon request to the
// daemon that asked, with that daemon's id.
func (p *lspProxy) editorResponse(frame []byte, body int, msg peeked) {
	key := string(msg.id)
	p.mu.Lock()
	req, ok := p.serverPending[key]
	if ok {
		delete(p.serverPending, key)
	} else if _, stale := p.staleServer[key]; stale {
		// An answer to a request of a daemon that is gone.
		delete(p.staleServer, key)
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	u := req.up
	if u == nil {
		if u = p.primary; u == nil {
			return
		}
	}
	if req.daemonID != nil {
		rewritten, err := withID(frame[body:], string(req.daemonID))
		if err != nil {
			return
		}
		frame = frameMessage(rewritten)
	}
	u.connMu.Lock()
	u.forwardLocked(frame, msg, false)
	u.connMu.Unlock()
}

// maxAnsweredLocally bounds the ids of requests that the proxy answered
// itself; a cancel for an older one is only dropped later than it could be.
const maxAnsweredLocally = 1024

// markAnswered records a request that the proxy answered itself, so that a
// later $/cancelRequest for it does not go to a daemon that never had it.
func (p *lspProxy) markAnswered(id []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.answeredLocally) >= maxAnsweredLocally {
		p.answeredLocally = make(map[string]struct{})
	}
	p.answeredLocally[string(id)] = struct{}{}
}

// cancel passes a $/cancelRequest to the upstream that holds the request. A
// cancel for a request that waits in a queue removes it and answers
// RequestCancelled; a cancel for a request that the proxy already answered
// is dropped.
func (p *lspProxy) cancel(frame []byte, body int, msg peeked) {
	id := cancelID(frame[body:])
	if id == nil {
		if u := p.primary; u != nil {
			u.connMu.Lock()
			u.forwardLocked(frame, msg, false)
			u.connMu.Unlock()
		}
		return
	}
	if p.cancelFanout(id) {
		return
	}
	for _, u := range p.allUpstreams() {
		u.connMu.Lock()
		handled := u.cancelLocked(frame, id)
		u.connMu.Unlock()
		if handled {
			return
		}
	}
	p.mu.Lock()
	_, answered := p.answeredLocally[string(id)]
	delete(p.answeredLocally, string(id))
	p.mu.Unlock()
	if answered {
		return
	}
	// A request that a daemon answered already; it ignores the cancel.
	if u := p.primary; u != nil {
		u.connMu.Lock()
		if u.conn != nil {
			_, _ = u.conn.Write(frame)
		}
		u.connMu.Unlock()
	}
}

// cancelID reads params.id of a $/cancelRequest.
func cancelID(body []byte) []byte {
	var msg struct {
		Params struct {
			ID json.RawMessage `json:"id"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil || len(msg.Params.ID) == 0 {
		return nil
	}
	return bytes.TrimSpace(msg.Params.ID)
}

// cancelFrame is a $/cancelRequest for id.
func cancelFrame(id string) []byte {
	return frameMessage([]byte(`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":` + id + `}}`))
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

// answerOpen answers a request that no daemon will answer. A part of a
// request that went to several daemons completes that request instead.
func (p *lspProxy) answerOpen(id []byte, code int, message string, mark bool) {
	if p.completePart(id, nil) {
		return
	}
	p.answerError(id, code, message)
	if mark {
		p.markAnswered(id)
	}
}

// answerOpenNull is answerOpen with a null result, for shutdown.
func (p *lspProxy) answerOpenNull(id []byte) {
	if p.completePart(id, []byte(`{"result":null}`)) {
		return
	}
	p.answerNull(id)
}

func (p *lspProxy) showError(message string) { p.showMessage(1, message) }

// showMessage sends window/showMessage of the given type (1 Error, 3 Info).
func (p *lspProxy) showMessage(kind int, message string) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "window/showMessage",
		"params":  map[string]any{"type": kind, "message": message},
	})
	if err == nil {
		p.writeOut(frameMessage(body))
	}
}

func nullResult(id []byte) []byte {
	return frameMessage([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, id)))
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

// splitFrame returns the body of a frame that readProxyFrame made.
func splitFrame(frame []byte) ([]byte, []byte, bool) {
	for i := 0; i+3 < len(frame); i++ {
		if frame[i] == '\r' && frame[i+1] == '\n' && frame[i+2] == '\r' && frame[i+3] == '\n' {
			return frame[:i+4], frame[i+4:], true
		}
	}
	return nil, nil, false
}

func errorResponse(body []byte) bool {
	var msg struct {
		Error json.RawMessage `json:"error"`
	}
	return json.Unmarshal(body, &msg) == nil && len(msg.Error) > 0 && string(msg.Error) != "null"
}

// withID returns a message body with another id.
func withID(body []byte, id string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	fields["id"] = json.RawMessage(id)
	return json.Marshal(fields)
}

// quoteID makes a proxy id: a JSON string, which no daemon id is.
func quoteID(format string, args ...any) string {
	return strconv.Quote(fmt.Sprintf(format, args...))
}
