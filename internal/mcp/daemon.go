package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/remoteoss/dexter/internal/daemon"
)

// MethodTool is the daemon control method that runs one MCP tool body. The
// frontend keeps the MCP protocol; the daemon, which owns the store, the
// watchers, and the language caches, answers the tool.
const MethodTool = "mcp/tool"

// ToolParams is the payload of MethodTool.
type ToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// WaitReadyMs is how long the call waits for the initial index before it
	// answers from what is indexed, with a note that the index is still
	// building.
	WaitReadyMs int `json:"waitReadyMs,omitempty"`
}

// ToolResult is the answer of MethodTool.
type ToolResult struct {
	Text string `json:"text"`
}

// indexWaitLimit caps how long a tool call waits for a workspace's initial
// index. The daemon caps its own index waits at the same value. A variable so
// tests can shrink it.
var indexWaitLimit = 30 * time.Second

func init() {
	daemon.RegisterMethod(MethodTool, serveTool)
}

// serveTool runs in the workspace daemon. It answers from the headless
// language service, or from an editor session only when the connection
// explicitly named one.
func serveTool(mc daemon.MethodContext, raw json.RawMessage) (any, error) {
	var params ToolParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	h := NewHandler(mc.Runtime, mc.LSP())
	text, err := h.Call(mc.Context, params.Name, params.Arguments, time.Duration(params.WaitReadyMs)*time.Millisecond)
	if err != nil {
		return nil, err
	}
	return ToolResult{Text: text}, nil
}

// Backend answers tool calls for one workspace root.
type Backend interface {
	CallTool(ctx context.Context, name string, args json.RawMessage) (string, error)
	Close() error
}

// warmer is a Backend that can start its workspace before the first call, so
// that the index is already building when the agent asks.
type warmer interface {
	warm(ctx context.Context)
}

// DaemonBackend returns a Backend that forwards each tool call to the
// workspace daemon for root, starting the daemon when necessary. It holds one
// control connection, which keeps the daemon alive while the MCP session is
// open, and connects again when the daemon goes away.
func DaemonBackend(root string) Backend {
	return newDaemonBackend(root, func(ctx context.Context, root string) (controlClient, error) {
		client, err := daemon.Ensure(ctx, root)
		if err != nil {
			// Not a typed nil in an interface.
			return nil, err
		}
		return client, nil
	})
}

func newDaemonBackend(root string, ensure func(ctx context.Context, root string) (controlClient, error)) *daemonBackend {
	return &daemonBackend{root: root, ensure: ensure, slots: make(chan struct{}, maxConcurrentToolCalls)}
}

// controlClient is the part of *daemon.Client that a daemonBackend uses.
type controlClient interface {
	Call(ctx context.Context, method string, params, result any) error
	Done() <-chan struct{}
	Close() error
}

// maxConcurrentToolCalls bounds the tool calls that one workspace connection
// runs at the same time. It is below the daemon's limit of concurrent
// requests per connection, so the MCP frontend waits for a slot instead of
// getting refusals from the daemon.
const maxConcurrentToolCalls = 32

type daemonBackend struct {
	mu     sync.Mutex
	root   string
	ensure func(ctx context.Context, root string) (controlClient, error)
	client controlClient
	// connecting is the connect in progress, or nil. The connect runs
	// without mu, because a daemon start can take 30 seconds or more, and
	// Close must not wait for it.
	connecting *connectAttempt
	closed     bool
	// closeReason is the error that calls get after Close.
	closeReason error

	slots chan struct{}
}

// connectAttempt is one connect to the workspace daemon. Calls that arrive
// while it runs wait for its result instead of starting a second connect.
type connectAttempt struct {
	// done closes when client and err are set.
	done chan struct{}
	// cancel stops the connect. Close calls it.
	cancel context.CancelFunc
	client controlClient
	err    error
}

// retire closes the backend with the error that its calls in flight get.
func (b *daemonBackend) retire(reason error) error {
	b.mu.Lock()
	b.closeReason = reason
	b.mu.Unlock()
	return b.Close()
}

func (b *daemonBackend) closedErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closedErrLocked()
}

func (b *daemonBackend) closedErrLocked() error {
	if !b.closed {
		return nil
	}
	if b.closeReason != nil {
		return b.closeReason
	}
	return errClosed
}

// connection returns the live control connection, connecting when there is
// none or the last one ended. Only one connect runs at a time. Each caller
// waits for its result or for its own context, so a caller that stops
// waiting does not stop the connect for the others.
func (b *daemonBackend) connection(ctx context.Context) (controlClient, error) {
	b.mu.Lock()
	if err := b.closedErrLocked(); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if b.client != nil {
		select {
		case <-b.client.Done():
			_ = b.client.Close()
			b.client = nil
		default:
			client := b.client
			b.mu.Unlock()
			return client, nil
		}
	}
	attempt := b.connecting
	if attempt == nil {
		// Only Close stops the connect. daemon.Ensure has its own time
		// limits, and a connect that no caller waits for any more gives a
		// connection to the next call.
		connectCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		attempt = &connectAttempt{done: make(chan struct{}), cancel: cancel}
		b.connecting = attempt
		go b.connect(connectCtx, attempt, b.root)
	}
	b.mu.Unlock()
	select {
	case <-attempt.done:
		return attempt.client, attempt.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// connect runs one connect attempt without the lock, and then installs its
// result under the lock. When the backend closed during the connect, the new
// connection is closed and the attempt gets the close reason.
func (b *daemonBackend) connect(ctx context.Context, attempt *connectAttempt, root string) {
	defer attempt.cancel()
	client, err := b.ensure(ctx, root)
	var mismatch *daemon.RootMismatchError
	if errors.As(err, &mismatch) {
		// A daemon already serves this workspace through another spelling of
		// the root, for example the one an editor used. Share it: the agent
		// gets paths in that spelling, which name the same files.
		log.Printf("MCP: workspace %s is served as %s; using that root", root, mismatch.Daemon)
		root = mismatch.Daemon
		b.mu.Lock()
		b.root = root
		b.mu.Unlock()
		client, err = b.ensure(ctx, root)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.connecting = nil
	if closedErr := b.closedErrLocked(); closedErr != nil {
		if err == nil {
			_ = client.Close()
		}
		client, err = nil, closedErr
	}
	if err == nil {
		b.client = client
	} else {
		client = nil
	}
	attempt.client, attempt.err = client, err
	close(attempt.done)
}

// rootName returns the root, which a connect can change to the spelling that
// the daemon uses.
func (b *daemonBackend) rootName() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.root
}

func (b *daemonBackend) warm(ctx context.Context) {
	if _, err := b.connection(ctx); err != nil {
		log.Printf("MCP: cannot start the workspace daemon for %s: %v", b.rootName(), err)
	}
}

func (b *daemonBackend) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	params := ToolParams{Name: name, Arguments: args, WaitReadyMs: int(indexWaitLimit.Milliseconds())}
	for attempt := 0; ; attempt++ {
		client, err := b.connection(ctx)
		if err != nil {
			return "", err
		}
		var res ToolResult
		err = client.Call(ctx, MethodTool, params, &res)
		if err == nil {
			return res.Text, nil
		}
		if strings.Contains(err.Error(), fmt.Sprintf("unknown daemon method %q", MethodTool)) {
			return "", fmt.Errorf("the dexter daemon for %s was started by a build without MCP tools; run `dexter stop --force` in the project, then retry", b.rootName())
		}
		if name == renameToolName && ctx.Err() != nil {
			// A client that canceled may not read the answer, so the log
			// keeps it too.
			log.Printf("MCP: a rename was canceled while it ran in %s; it may have been applied, so check git status", b.rootName())
			return "", fmt.Errorf("the rename was canceled while it ran, so it may have been applied; check git status (%w)", ctx.Err())
		}
		if reason := b.closedErr(); reason != nil {
			// The frontend closed this connection during the call, for
			// example because the client's roots changed.
			if name == renameToolName {
				return "", fmt.Errorf("%w; the rename may have been applied before the connection closed, so check git status", reason)
			}
			return "", reason
		}
		select {
		case <-client.Done():
			// The daemon went away, for example because a newer build
			// replaced it. Connect again once, but never repeat a rename: it
			// may have been applied before the connection ended.
			if attempt == 0 && name != renameToolName {
				continue
			}
		default:
		}
		return "", err
	}
}

// Close closes the connection and stops a connect in progress. It does not
// wait for that connect to return.
func (b *daemonBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.connecting != nil {
		b.connecting.cancel()
	}
	if b.client == nil {
		return nil
	}
	err := b.client.Close()
	b.client = nil
	return err
}
