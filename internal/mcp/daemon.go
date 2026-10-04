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
	closed bool
	// closeReason is the error that calls get after Close.
	closeReason error

	slots chan struct{}
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
	if !b.closed {
		return nil
	}
	if b.closeReason != nil {
		return b.closeReason
	}
	return errClosed
}

// connection returns the live control connection, connecting when there is
// none or the last one ended.
func (b *daemonBackend) connection(ctx context.Context) (controlClient, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		if b.closeReason != nil {
			return nil, b.closeReason
		}
		return nil, errClosed
	}
	if b.client != nil {
		select {
		case <-b.client.Done():
			_ = b.client.Close()
			b.client = nil
		default:
			return b.client, nil
		}
	}
	client, err := b.ensure(ctx, b.root)
	var mismatch *daemon.RootMismatchError
	if errors.As(err, &mismatch) {
		// A daemon already serves this workspace through another spelling of
		// the root, for example the one an editor used. Share it: the agent
		// gets paths in that spelling, which name the same files.
		log.Printf("MCP: workspace %s is served as %s; using that root", b.root, mismatch.Daemon)
		b.root = mismatch.Daemon
		client, err = b.ensure(ctx, b.root)
	}
	if err != nil {
		return nil, err
	}
	b.client = client
	return client, nil
}

func (b *daemonBackend) warm(ctx context.Context) {
	if _, err := b.connection(ctx); err != nil {
		log.Printf("MCP: cannot start the workspace daemon for %s: %v", b.root, err)
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
			return "", fmt.Errorf("the dexter daemon for %s was started by a build without MCP tools; run `dexter stop --force` in the project, then retry", b.root)
		}
		if name == renameToolName && ctx.Err() != nil {
			// A client that canceled may not read the answer, so the log
			// keeps it too.
			log.Printf("MCP: a rename was canceled while it ran in %s; it may have been applied, so check git status", b.root)
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

func (b *daemonBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.client == nil {
		return nil
	}
	err := b.client.Close()
	b.client = nil
	return err
}
