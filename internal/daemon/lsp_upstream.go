package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

// upstream is the proxy's connection to the daemon of one project, with what
// a replay of the session on a new daemon of that project needs: the
// documents of that project, the requests it has not answered, and the
// progress it began.
type upstream struct {
	p    *lspProxy
	root string
	// n is 0 for the primary (the start root's upstream), then 1, 2, ... in
	// the order the projects came.
	n int

	// connMu orders writes to the daemon. It is held during a write, so code
	// that holds it must never wait for the daemon reader.
	connMu sync.Mutex
	conn   net.Conn // nil while there is no daemon
	// replaying is the connection of an initialize exchange that runs now,
	// before it becomes conn. editorGone closes it too.
	replaying net.Conn
	queue     [][]byte // frames the editor sent while there was no daemon
	// failed is set when no daemon can serve this project; then its
	// requests get an error and its notifications are dropped, until the
	// first message after retryAt starts a new attempt. failures counts
	// the attempts that failed in a row.
	failed   bool
	failures int
	retryAt  time.Time

	// mu guards the state below. It is never held during I/O.
	mu       sync.Mutex
	gen      int // number of daemons this upstream has had
	docs     map[string]*proxyDoc
	docSeq   uint64
	pending  map[string]string // ids of requests the daemon has not answered -> method
	replayID string            // id of a replayed initialize whose answer is dropped
	// progress holds the work-done progress tokens, as the editor got them,
	// that the daemon began and did not end. A daemon that stops cannot end
	// them; the proxy does.
	progress map[string]struct{}
	// created holds the tokens that a secondary daemon created with
	// window/workDoneProgress/create, as the daemon wrote them. The proxy
	// gives the editor the token with a prefix of this upstream.
	created map[string]struct{}
}

func (p *lspProxy) newUpstream(root string, n int) *upstream {
	return &upstream{
		p:        p,
		root:     root,
		n:        n,
		docs:     make(map[string]*proxyDoc),
		pending:  make(map[string]string),
		progress: make(map[string]struct{}),
		created:  make(map[string]struct{}),
	}
}

func (u *upstream) isPrimary() bool { return u.n == 0 }

// addUpstream starts the upstream of a project that has none yet. Its
// messages wait in its queue until its daemon is reached. It returns nil when
// the session ends.
func (p *lspProxy) addUpstream(root, key string) *upstream {
	p.mu.Lock()
	if p.exiting || p.shutdown {
		p.mu.Unlock()
		return nil
	}
	if u := p.upstreams[key]; u != nil {
		p.mu.Unlock()
		return u
	}
	p.upstreamSeq++
	u := p.newUpstream(root, p.upstreamSeq)
	p.upstreams[key] = u
	p.upstreamList = append(p.upstreamList, u)
	p.loops.Add(1)
	p.mu.Unlock()
	log.Printf("LSP proxy: the files of %s go to the workspace daemon of that project", root)
	go u.loop(nil, nil)
	return u
}

// loop serves the upstream until the editor ends the session, or until no
// daemon can be reached. conn is nil for an upstream that has no daemon yet.
func (u *upstream) loop(conn net.Conn, reader *bufio.Reader) {
	defer u.p.loops.Done()
	var done <-chan struct{}
	var err error
	if conn != nil {
		done = u.startReadDaemon(conn, reader)
	} else if conn, done, err = u.reconnect(true); err != nil {
		u.stopped(err)
		return
	}
	for {
		<-done
		_ = conn.Close()
		if u.p.ending() {
			u.endSession()
			return
		}
		if conn, done, err = u.reconnect(false); err != nil {
			u.stopped(err)
			return
		}
	}
}

// stopped handles an upstream whose daemon cannot be reached. The editor was
// told why. Without the primary the session cannot go on; without another
// upstream it can, for the files of the other projects.
func (u *upstream) stopped(err error) {
	if u.p.ending() {
		u.endSession()
		return
	}
	if u.isPrimary() {
		select {
		case u.p.fatal <- err:
		default:
		}
	}
}

// endSession finishes an upstream whose daemon is gone while the editor ends
// the session. Shutdown succeeds, because the daemon that had to shut down is
// gone; other requests, sent or queued, fail.
func (u *upstream) endSession() {
	u.mu.Lock()
	pending := u.pending
	u.pending = make(map[string]string)
	u.mu.Unlock()
	u.connMu.Lock()
	queue := u.queue
	u.queue = nil
	u.connMu.Unlock()
	for _, frame := range queue {
		if _, body, ok := splitFrame(frame); ok {
			if msg := peekMessage(body); msg.isRequest() {
				pending[string(msg.id)] = msg.method
			}
		}
	}
	for id, method := range pending {
		if method == "shutdown" {
			u.p.answerOpenNull([]byte(id))
			continue
		}
		u.p.answerOpen([]byte(id), lspContentModified, "Dexter: the session ended before this request was answered.", false)
	}
}

// record updates the documents of this upstream with one editor message. It
// reports whether a replay covers the message, so that it need not be queued
// while there is no daemon. The caller holds connMu.
func (u *upstream) record(frame []byte, body int, msg peeked) bool {
	switch msg.method {
	case "textDocument/didOpen", "textDocument/didChange", "textDocument/didClose", "textDocument/didSave":
	default:
		return false
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	switch msg.method {
	case "textDocument/didOpen":
		if msg.uri == "" {
			return false
		}
		u.docSeq++
		u.docs[msg.uri] = &proxyDoc{seq: u.docSeq, open: frame}
		return true
	case "textDocument/didChange":
		if doc := u.docs[msg.uri]; doc != nil {
			return doc.applyChange(frame, body)
		}
		return false
	case "textDocument/didClose":
		if msg.uri == "" {
			return false
		}
		delete(u.docs, msg.uri)
		return true
	default: // didSave
		doc := u.docs[msg.uri]
		if doc == nil {
			return false
		}
		// The replay sends the buffer as saved or changed. A queued save
		// after the replayed changes would mark newer text as saved.
		doc.changed = false
		return true
	}
}

// forwardLocked sends one editor message to the daemon, or queues it while
// there is none. The caller holds connMu.
func (u *upstream) forwardLocked(frame []byte, msg peeked, covered bool) {
	if u.failed && (time.Now().Before(u.retryAt) || !u.p.retryLocked(u)) {
		if msg.isRequest() {
			u.p.answerOpen(msg.id, lspInternalError, fmt.Sprintf("Dexter cannot serve the files of %s now; it tries again soon.", u.root), true)
		}
		return
	}
	if u.conn == nil {
		if covered {
			return
		}
		if len(u.queue) >= maxQueuedFrames {
			if msg.isRequest() {
				u.p.answerOpen(msg.id, lspContentModified, "Dexter: the workspace daemon is restarting; repeat the request.", false)
			}
			return
		}
		u.queue = append(u.queue, frame)
		return
	}
	if msg.isRequest() {
		u.mu.Lock()
		u.pending[string(msg.id)] = msg.method
		u.mu.Unlock()
	}
	// A failed write means the daemon is gone. The daemon reader sees the end
	// of the stream and reconnects; a request is in pending and gets an answer
	// then, and the state covers the notifications that matter.
	_, _ = u.conn.Write(frame)
}

// failedRetryDelay is the wait after the first failed attempt to reach the
// daemon of a project that is not the start root. It doubles after each
// failed attempt, up to maxFailedRetryDelay.
var (
	failedRetryDelay    = 15 * time.Second
	maxFailedRetryDelay = 2 * time.Minute
)

// retryLocked starts a new attempt to reach the daemon of a failed upstream.
// The message that caused it waits in the queue for that daemon. It reports
// false when the session ends. The caller holds u.connMu.
func (p *lspProxy) retryLocked(u *upstream) bool {
	p.mu.Lock()
	if p.exiting || p.shutdown {
		p.mu.Unlock()
		return false
	}
	p.loops.Add(1)
	p.mu.Unlock()
	u.failed = false
	log.Printf("LSP proxy: trying again to reach the workspace daemon of %s", u.root)
	go u.loop(nil, nil)
	return true
}

// cancelLocked handles a $/cancelRequest for a request of this upstream. It
// reports false when this upstream does not hold the request. The caller
// holds connMu.
func (u *upstream) cancelLocked(frame []byte, id []byte) bool {
	for i, queued := range u.queue {
		_, queuedBody, ok := splitFrame(queued)
		if !ok {
			continue
		}
		if q := peekMessage(queuedBody); q.isRequest() && bytes.Equal(q.id, id) {
			u.queue = append(u.queue[:i:i], u.queue[i+1:]...)
			u.p.answerOpen(id, lspRequestCancelled, "Dexter: the request was canceled.", false)
			return true
		}
	}
	u.mu.Lock()
	_, sent := u.pending[string(id)]
	u.mu.Unlock()
	if !sent {
		return false
	}
	if u.conn != nil {
		_, _ = u.conn.Write(frame)
	}
	return true
}

// startReadDaemon runs readDaemon and returns a channel that is closed when
// the daemon stream ends.
func (u *upstream) startReadDaemon(conn net.Conn, reader *bufio.Reader) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		u.readDaemon(conn, reader)
	}()
	return done
}

// readDaemon forwards the daemon's messages to the editor until the daemon
// stream ends.
func (u *upstream) readDaemon(conn net.Conn, reader *bufio.Reader) {
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
		action, rewritten := u.fromDaemon(frame, body)
		switch action {
		case toEditor:
			if rewritten != nil {
				frame = rewritten
			}
			u.p.writeOut(frame)
		case answerNull:
			// The daemon reader must never wait on connMu, which a write to
			// the daemon holds.
			id := append([]byte(nil), peekMessage(frame[body:]).id...)
			go func() {
				u.connMu.Lock()
				defer u.connMu.Unlock()
				if u.conn == conn {
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

// fromDaemon updates the state with one daemon message and says what to do
// with it. rewritten, when not nil, is the frame to send instead.
func (u *upstream) fromDaemon(frame []byte, body int) (action daemonAction, rewritten []byte) {
	// A panic in the scan is a bug; the message still reaches the editor.
	defer func() {
		if value := recover(); value != nil {
			log.Printf("Error: LSP proxy: panic while reading a daemon message: %v\n%s", value, debug.Stack())
			action, rewritten = toEditor, nil
		}
	}()
	p := u.p
	msg := peekMessage(frame[body:])
	switch {
	case msg.isResponse():
		key := string(msg.id)
		u.mu.Lock()
		if u.replayID != "" && key == u.replayID {
			u.mu.Unlock()
			return dropIt, nil
		}
		delete(u.pending, key)
		u.mu.Unlock()
		p.mu.Lock()
		part := len(p.fanouts) > 0 && p.fanouts[key] != nil
		if !part && u.isPrimary() && p.initID != nil && key == string(p.initID) {
			p.initAnswered = true
		}
		p.mu.Unlock()
		if part {
			p.completePart(msg.id, frame[body:])
			return dropIt, nil
		}
		return toEditor, nil
	case msg.isRequest():
		if msg.method == "client/registerCapability" && p.alreadyRegistered(frame[body:]) {
			// The editor holds this registration from another daemon
			// already, and a second one with the same id is an error in
			// some editors. The proxy answers it.
			return answerNull, nil
		}
		u.mu.Lock()
		gen := u.gen
		u.mu.Unlock()
		if u.isPrimary() && gen == 0 {
			p.mu.Lock()
			p.serverPending[string(msg.id)] = serverRequest{up: u}
			p.mu.Unlock()
			return toEditor, nil
		}
		var editorID string
		if u.isPrimary() {
			editorID = quoteID("dexter-proxy-%d-%s", gen, msg.id)
		} else {
			editorID = quoteID("dexter-proxy-u%d-%d-%s", u.n, gen, msg.id)
		}
		var out []byte
		var err error
		if !u.isPrimary() && msg.method == "window/workDoneProgress/create" {
			out, err = u.withProgressToken(frame[body:], editorID, true)
		} else {
			out, err = withID(frame[body:], editorID)
		}
		if err != nil {
			p.mu.Lock()
			p.serverPending[string(msg.id)] = serverRequest{up: u}
			p.mu.Unlock()
			return toEditor, nil
		}
		p.mu.Lock()
		p.serverPending[editorID] = serverRequest{up: u, daemonID: append([]byte(nil), msg.id...)}
		p.mu.Unlock()
		return toEditor, frameMessage(out)
	case msg.method == "$/progress":
		if out := u.trackProgress(frame[body:]); out != nil {
			return toEditor, frameMessage(out)
		}
	}
	return toEditor, nil
}

// alreadyRegistered reports whether the editor holds every registration of a
// client/registerCapability request, and records the ones it does not hold:
// they are sent now.
func (p *lspProxy) alreadyRegistered(body []byte) bool {
	var msg struct {
		Params struct {
			Registrations []struct {
				ID string `json:"id"`
			} `json:"registrations"`
		} `json:"params"`
	}
	var ids []string
	if json.Unmarshal(body, &msg) == nil {
		for _, r := range msg.Params.Registrations {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		ids = []string{""}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	all := true
	for _, id := range ids {
		if !p.registered[id] {
			p.registered[id] = true
			all = false
		}
	}
	return all
}

// trackProgress keeps the tokens of work-done progress that is open. For a
// token that a secondary daemon created, it returns the message with the
// token that the editor got.
func (u *upstream) trackProgress(body []byte) []byte {
	var msg struct {
		Params struct {
			Token json.RawMessage `json:"token"`
			Value struct {
				Kind string `json:"kind"`
			} `json:"value"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil || len(msg.Params.Token) == 0 {
		return nil
	}
	token := string(bytes.TrimSpace(msg.Params.Token))
	var out []byte
	u.mu.Lock()
	_, ours := u.created[token]
	if ours && msg.Params.Value.Kind == "end" {
		delete(u.created, token)
	}
	u.mu.Unlock()
	if ours {
		rewritten, err := u.withProgressToken(body, "", false)
		if err == nil {
			out = rewritten
			token = upstreamToken(u.n, token)
		}
	}
	u.mu.Lock()
	switch msg.Params.Value.Kind {
	case "begin":
		u.progress[token] = struct{}{}
	case "end":
		delete(u.progress, token)
	}
	u.mu.Unlock()
	return out
}

// withProgressToken returns a message of a secondary daemon with the token
// in its params made unique for the editor, and with id when it is not "".
// With create, the token is recorded as one that the daemon created.
func (u *upstream) withProgressToken(body []byte, id string, create bool) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(fields["params"], &params); err != nil {
		return nil, err
	}
	token := string(bytes.TrimSpace(params["token"]))
	if token == "" {
		return nil, errors.New("no progress token")
	}
	if create {
		u.mu.Lock()
		u.created[token] = struct{}{}
		u.mu.Unlock()
	}
	params["token"] = json.RawMessage(upstreamToken(u.n, token))
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	fields["params"] = raw
	if id != "" {
		fields["id"] = json.RawMessage(id)
	}
	return json.Marshal(fields)
}

// endProgress ends, in the editor, the progress that a stopped daemon left
// open, so that the editor does not show it for ever.
func (u *upstream) endProgress() {
	u.mu.Lock()
	tokens := u.progress
	u.progress = make(map[string]struct{})
	u.created = make(map[string]struct{})
	u.mu.Unlock()
	for token := range tokens {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","method":"$/progress","params":{"token":%s,"value":{"kind":"end","message":"the workspace daemon stopped"}}}`, token)
		u.p.writeOut(frameMessage([]byte(body)))
	}
}

// reconnect answers the requests that the stopped daemon left open, reaches a
// daemon again, and replays the session on it. first is the first daemon of
// an upstream that is not the primary. On failure it tells the editor why.
func (u *upstream) reconnect(first bool) (net.Conn, <-chan struct{}, error) {
	p := u.p
	u.connMu.Lock()
	u.conn = nil
	u.connMu.Unlock()

	u.mu.Lock()
	pending := u.pending
	u.pending = make(map[string]string)
	u.mu.Unlock()
	p.mu.Lock()
	for id, req := range p.serverPending {
		if req.up == u {
			p.staleServer[id] = struct{}{}
			delete(p.serverPending, id)
		}
	}
	initOpen := u.isPrimary() && p.initialize != nil && !p.initAnswered
	initID := string(p.initID)
	p.mu.Unlock()
	for id, method := range pending {
		if method == "initialize" && initOpen && id == initID {
			continue // the replay sends it again with its own id
		}
		p.answerOpen([]byte(id), lspContentModified,
			"Dexter: the workspace daemon stopped while this request was open; Dexter reconnects. Repeat the request.", true)
	}
	u.endProgress()
	if !first {
		log.Printf("LSP proxy: the connection to the workspace daemon of %s ended; reconnecting", u.root)
	}

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
		if _, err := os.Stat(u.root); err != nil {
			return nil, nil, u.fail(fmt.Sprintf(
				"Dexter stopped: the project directory %s is no longer available (%v). The volume may have been ejected, or the directory moved or removed. Restart the editor when the project is available again.",
				u.root, err), err)
		}
		conn, reader, err := p.dial(ctx, u.root)
		if err != nil {
			// The cause of a failed start is what the user needs; the end of
			// the reconnect budget is not.
			if lastErr == nil || !isContextError(err) {
				lastErr = err
			}
			log.Printf("LSP proxy: connect attempt %d for %s: %v", attempt+1, u.root, err)
			if permanentDialError(err) {
				break
			}
			continue
		}
		daemonDone, err := u.replay(conn, reader)
		if err != nil {
			_ = conn.Close()
			lastErr = err
			log.Printf("LSP proxy: replaying the session for %s, attempt %d: %v", u.root, attempt+1, err)
			continue
		}
		if first {
			log.Printf("LSP proxy: connected to the workspace daemon of %s", u.root)
			u.connMu.Lock()
			again := u.failures > 0
			u.failures = 0
			u.connMu.Unlock()
			if again {
				p.showMessage(3, fmt.Sprintf("Dexter reaches the workspace daemon of %s again; the files of this project get answers again.", u.root))
			}
		} else {
			log.Printf("LSP proxy: reconnected to the workspace daemon of %s", u.root)
		}
		return conn, daemonDone, nil
	}
	if p.ending() {
		return nil, nil, errors.New("the editor ended the session")
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	if first {
		return nil, nil, u.fail(ExplainStartupFailure(u.root, lastErr)+" Until it starts, Dexter does not answer for the files of this project; it tries again after a short wait.", lastErr)
	}
	return nil, nil, u.fail("Dexter lost the connection to its workspace daemon and could not reconnect. "+ExplainStartupFailure(u.root, lastErr), lastErr)
}

// fail tells the editor that this upstream cannot continue, answers what is
// still open, and returns the error to exit with. What is open includes an
// initialize that reconnect kept for the replay, and requests that a failed
// replay sent. An upstream that is not the primary is marked failed: the
// session goes on for the other projects.
func (u *upstream) fail(message string, err error) error {
	p := u.p
	show := true
	if !u.isPrimary() {
		// The editor is told once; later attempts that fail only log.
		u.connMu.Lock()
		show = u.failures == 0
		u.connMu.Unlock()
	}
	if show {
		p.showError(message)
	} else {
		log.Printf("LSP proxy: %s", message)
	}
	u.mu.Lock()
	open := u.pending
	u.pending = make(map[string]string)
	u.mu.Unlock()
	if u.isPrimary() {
		p.mu.Lock()
		if p.initialize != nil && !p.initAnswered {
			open[string(p.initID)] = "initialize"
		}
		p.mu.Unlock()
	}
	u.connMu.Lock()
	queue := u.queue
	u.queue = nil
	if !u.isPrimary() {
		u.failed = true
		u.failures++
		delay := failedRetryDelay
		for i := 1; i < u.failures && delay < maxFailedRetryDelay; i++ {
			delay *= 2
		}
		u.retryAt = time.Now().Add(min(delay, maxFailedRetryDelay))
	}
	u.connMu.Unlock()
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
		p.answerOpen([]byte(id), lspInternalError, message, false)
	}
	return &LSPConnectionLostError{Message: message, Err: err}
}

// replay rebuilds the session on a new daemon connection and then makes it
// the current one. The initialize exchange runs first, while the editor's
// messages for this upstream wait in its queue; then, under connMu, the rest
// of the state and the queue are sent, so none is lost and none overtakes
// the replay. After the answer to initialize, the daemon's messages are read
// while the replay writes (the returned channel closes when they end): a
// daemon that answers each document while the proxy does not read would fill
// the socket and block both sides.
func (u *upstream) replay(conn net.Conn, reader *bufio.Reader) (<-chan struct{}, error) {
	p := u.p
	u.connMu.Lock()
	if p.editorInputGone() {
		u.connMu.Unlock()
		return nil, errors.New("the editor ended the session")
	}
	u.replaying = conn
	u.connMu.Unlock()
	u.mu.Lock()
	u.gen++
	gen := u.gen
	u.mu.Unlock()
	sent, err := u.replayInitialize(conn, reader, gen)
	if err != nil {
		u.connMu.Lock()
		u.replaying = nil
		u.connMu.Unlock()
		return nil, err
	}

	u.connMu.Lock()
	defer u.connMu.Unlock()
	u.replaying = nil
	if !sent {
		// The editor's initialize came during the exchange above.
		if _, err := u.replayInitialize(conn, reader, gen); err != nil {
			return nil, err
		}
	}
	p.mu.Lock()
	initialized := p.initialized
	config := p.config
	p.mu.Unlock()
	u.mu.Lock()
	docs := make([]*proxyDoc, 0, len(u.docs))
	for _, doc := range u.docs {
		docs = append(docs, doc)
	}
	u.mu.Unlock()
	sort.Slice(docs, func(i, j int) bool { return docs[i].seq < docs[j].seq })

	daemonDone := u.startReadDaemon(conn, reader)
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
	queue := u.queue
	u.queue = nil
	for i, frame := range queue {
		_, body, ok := splitFrame(frame)
		if ok {
			if msg := peekMessage(body); msg.isRequest() {
				u.mu.Lock()
				u.pending[string(msg.id)] = msg.method
				u.mu.Unlock()
			}
		}
		if _, err := conn.Write(frame); err != nil {
			// The new daemon is gone too. This frame is pending, so the next
			// reconnect answers it; the frames after it were not sent, so
			// they wait in the queue for the next daemon.
			u.queue = append(queue[i+1:len(queue):len(queue)], u.queue...)
			break
		}
	}
	u.conn = conn
	if p.editorInputGone() {
		// The editor's input ended during the replay, after editorGone
		// closed the connections it saw.
		closeWrite(conn)
	}
	return daemonDone, nil
}

// replayInitialize sends the session's initialize to a new daemon and waits
// for its answer. It reports false when the editor has not sent initialize
// yet. The primary sends the editor's own request while the editor has no
// answer to it; every other initialize has an id of the proxy, and its answer
// is dropped. An upstream that is not the primary gets the editor's request
// with the roots set to its own project.
func (u *upstream) replayInitialize(conn net.Conn, reader *bufio.Reader, gen int) (bool, error) {
	p := u.p
	p.mu.Lock()
	initialize := p.initialize
	initID := p.initID
	answered := p.initAnswered || !u.isPrimary()
	p.mu.Unlock()
	if initialize == nil {
		return false, nil
	}
	waitID := initID
	if answered {
		// The editor has its answer already; this one is for the proxy.
		var replayID string
		var body []byte
		var err error
		if u.isPrimary() {
			replayID = quoteID("dexter-proxy-reconnect-%d", gen)
			body, err = withID(initialize, replayID)
		} else {
			replayID = quoteID("dexter-proxy-u%d-initialize-%d", u.n, gen)
			body, err = upstreamInitialize(initialize, u.root, replayID)
		}
		if err != nil {
			return true, err
		}
		u.mu.Lock()
		u.replayID = replayID
		u.mu.Unlock()
		waitID = []byte(replayID)
		if _, err := conn.Write(frameMessage(body)); err != nil {
			return true, err
		}
	} else {
		u.mu.Lock()
		u.pending[string(initID)] = "initialize"
		u.mu.Unlock()
		if _, err := conn.Write(frameMessage(initialize)); err != nil {
			return true, err
		}
	}
	return true, u.awaitResponse(conn, reader, waitID, answered)
}

// awaitResponse reads the new daemon's messages until the answer to the
// replayed initialize arrives, and passes the others to the editor. An answer
// that the editor never had is passed on too. Nothing else writes to conn
// yet, so it may write to it.
func (u *upstream) awaitResponse(conn net.Conn, reader *bufio.Reader, id []byte, drop bool) error {
	p := u.p
	_ = conn.SetReadDeadline(time.Now().Add(replayTimeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		frame, body, err := readProxyFrame(reader)
		if err != nil {
			return fmt.Errorf("waiting for the answer to initialize: %w", err)
		}
		msg := peekMessage(frame[body:])
		if msg.isResponse() && string(msg.id) == string(id) {
			if drop {
				u.mu.Lock()
				u.replayID = ""
				u.mu.Unlock()
			} else {
				u.mu.Lock()
				delete(u.pending, string(id))
				u.mu.Unlock()
				p.mu.Lock()
				p.initAnswered = true
				p.mu.Unlock()
			}
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
		action, rewritten := u.fromDaemon(frame, body)
		switch action {
		case toEditor:
			if rewritten != nil {
				frame = rewritten
			}
			p.writeOut(frame)
		case answerNull:
			if _, err := conn.Write(nullResult(msg.id)); err != nil {
				return err
			}
		}
	}
}
