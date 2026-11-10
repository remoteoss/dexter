package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// clientCallTimeout bounds a request that Dexter sends to an editor, such as
// client/registerCapability. An editor answers at once; the bound only frees
// the goroutine that waits when an editor never does.
const clientCallTimeout = 10 * time.Second

// clientCallContext is the context of one request to this session's editor. It
// ends after clientCallTimeout, or when the session's connection closes: a
// jsonrpc2 call waits only for its answer or its context, so a session that
// closed before it answered would otherwise keep the goroutine, and the
// session it refers to, for ever.
func (s *Server) clientCallContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), clientCallTimeout)
	if s.conn != nil {
		done := s.conn.Done()
		go func() {
			select {
			case <-done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	return ctx, cancel
}

// OnFault sets what runs after a recovered panic in shared index work, whose
// state can be wrong afterwards: a write transaction that the panic left open,
// for example. The workspace restarts its index then.
func (c *IndexCoordinator) OnFault(fn func(what string)) {
	c.onFault.Store(&fn)
}

// faulted returns the follow-up for Reporter.Recover in shared index work.
func (c *IndexCoordinator) faulted(what string) func(any) {
	return func(any) {
		if fn := c.onFault.Load(); fn != nil {
			(*fn)(what)
		}
	}
}

// requestHook runs before each request and notification handler. Tests set it
// to put a panic in a handler.
var requestHook atomic.Pointer[func(method string)]

// SetRequestHookForTesting makes hook run before each handler, until the
// returned func restores the previous state. It is for tests only.
func SetRequestHookForTesting(hook func(method string)) (restore func()) {
	previous := requestHook.Swap(&hook)
	return func() { requestHook.Store(previous) }
}

// recoverHandler keeps a panic in one handler from stopping the process. The
// daemon serves every editor of a workspace from one process, and jsonrpc2
// runs handlers on its own goroutine, where no recover of the daemon reaches.
// The request gets an InternalError answer, the session goes on, and the
// panic is logged with its stack and reported to the editors once.
func (s *Server) recoverHandler(next jsonrpc2.Handler) jsonrpc2.Handler {
	return func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) (err error) {
		var replied atomic.Bool
		guarded := func(ctx context.Context, result any, replyErr error) error {
			replied.Store(true)
			return reply(ctx, result, replyErr)
		}
		defer func() {
			value := recover()
			if value == nil {
				// Every message must be answered once: the handler chain
				// runs the next message only after this one replied, and a
				// request without an answer leaves the editor waiting.
				if !replied.Load() {
					err = reply(ctx, nil, err)
				}
				return
			}
			s.index.reporter.Panicked("request", fmt.Sprintf("the %s handler", req.Method()), value, debug.Stack())
			err = nil
			if !replied.Load() {
				// A notification has no answer on the wire, but the reply
				// lets the next message run.
				err = reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InternalError,
					fmt.Sprintf("Dexter: internal error in %s: %v", req.Method(), value)))
			}
		}()
		return next(ctx, guarded, req)
	}
}

// hookHandler runs requestHook before each handler.
func hookHandler(next jsonrpc2.Handler) jsonrpc2.Handler {
	return func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		if hook := requestHook.Load(); hook != nil && *hook != nil {
			(*hook)(req.Method())
		}
		return next(ctx, reply, req)
	}
}

// cancelHandler makes $/cancelRequest cancel the context of the request that
// it names, and answers a canceled request with RequestCancelled (-32800). It
// wraps a serialDispatcher: a request that runs on the reader's goroutine
// cannot see a cancel until it ends. protocol.CancelHandler is not used: it accepts only int32 ids, and a
// JSON number decodes as float64, so it refused every numeric id.
func cancelHandler(next jsonrpc2.Handler) jsonrpc2.Handler {
	inner, canceller := jsonrpc2.CancelHandler(next)
	return func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		if req.Method() != protocol.MethodCancelRequest {
			if _, isCall := req.(*jsonrpc2.Call); isCall {
				answer := reply
				reply = func(replyCtx context.Context, result any, err error) error {
					// A handler that saw its context end may answer with
					// nothing or with the context's error; both mean that
					// the request was canceled.
					if (replyCtx.Err() != nil && err == nil) || errors.Is(err, context.Canceled) {
						result, err = nil, protocol.ErrRequestCancelled
					}
					return answer(context.WithoutCancel(replyCtx), result, err)
				}
			}
			return inner(ctx, reply, req)
		}
		var params struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(req.Params(), &params) == nil {
			var number int32
			var text string
			switch {
			case json.Unmarshal(params.ID, &number) == nil:
				canceller(jsonrpc2.NewNumberID(number))
			case json.Unmarshal(params.ID, &text) == nil:
				canceller(jsonrpc2.NewStringID(text))
			}
		}
		return nil
	}
}

// slowMethods are the requests that can run long enough for an editor to
// cancel them. They run off the reader's goroutine, so that the reader sees
// $/cancelRequest while they run.
var slowMethods = map[string]bool{
	protocol.MethodTextDocumentReferences:     true,
	protocol.MethodWorkspaceSymbol:            true,
	protocol.MethodTextDocumentRename:         true,
	protocol.MethodTextDocumentFormatting:     true,
	protocol.MethodTextDocumentImplementation: true,
	protocol.MethodCallHierarchyIncomingCalls: true,
	protocol.MethodCallHierarchyOutgoingCalls: true,
}

// maxQueuedMessages bounds the messages that wait behind a slow request. When
// the queue is full, the connection's reader waits, as it did when every
// message ran on it, so a client that sends without end cannot make the
// daemon queue without end. An editor sends a few messages per keystroke
// (didChange, completion, and some of hover, signature help, highlights, code
// actions); 1024 holds about 20 seconds of fast typing behind one slow
// request, far more than a references search takes on a large project.
var maxQueuedMessages = 1024

// serialDispatcher runs messages one after the other, in the order they
// arrive, as the reader's goroutine did before. A slow request, and every
// message after it until the queue is empty, runs on a worker goroutine
// instead, so that the reader goes on and can cancel it. Other messages run on
// the reader's goroutine when nothing waits: a hover or a completion pays no
// goroutine hand-off, which costs tens of microseconds when a thread must be
// woken. After close, queued messages are dropped: the session they belong to
// is gone.
type serialDispatcher struct {
	next    jsonrpc2.Handler
	mu      sync.Mutex
	cond    *sync.Cond // signaled when the queue shrinks or the dispatcher closes
	queue   []queuedMessage
	running bool
	closed  bool
	idle    chan struct{} // closed when no worker runs; replaced when one starts
}

type queuedMessage struct {
	ctx   context.Context
	reply jsonrpc2.Replier
	req   jsonrpc2.Request
}

func newSerialDispatcher(next jsonrpc2.Handler) *serialDispatcher {
	d := &serialDispatcher{next: next, idle: make(chan struct{})}
	close(d.idle)
	d.cond = sync.NewCond(&d.mu)
	return d
}

func (d *serialDispatcher) handle(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	if !d.running && len(d.queue) == 0 && !slowMethods[req.Method()] {
		d.mu.Unlock()
		return d.next(ctx, reply, req)
	}
	for len(d.queue) >= maxQueuedMessages && !d.closed {
		d.cond.Wait()
	}
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.queue = append(d.queue, queuedMessage{ctx: ctx, reply: reply, req: req})
	if !d.running {
		d.running = true
		d.idle = make(chan struct{})
		go d.drain(d.idle)
	}
	d.mu.Unlock()
	return nil
}

func (d *serialDispatcher) drain(idle chan struct{}) {
	defer close(idle)
	for {
		d.mu.Lock()
		if len(d.queue) == 0 || d.closed {
			d.queue = nil
			d.running = false
			d.mu.Unlock()
			return
		}
		m := d.queue[0]
		d.queue[0] = queuedMessage{}
		d.queue = d.queue[1:]
		d.cond.Broadcast()
		d.mu.Unlock()
		_ = d.next(m.ctx, m.reply, m.req)
	}
}

// close drops the queued messages and waits up to wait for the message that
// runs now. Its context is canceled by then, so a handler that checks it ends
// at once.
func (d *serialDispatcher) close(wait time.Duration) bool {
	d.mu.Lock()
	d.closed = true
	d.queue = nil
	idle := d.idle
	d.cond.Broadcast()
	d.mu.Unlock()
	select {
	case <-idle:
		return true
	case <-time.After(wait):
		return false
	}
}
