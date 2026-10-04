package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/version"
)

var errClosed = errors.New("the MCP server is shutting down")

// Config configures a Frontend.
type Config struct {
	// Root is the workspace root for sessions that give no usable MCP root.
	// With Fixed it is the root of every session.
	Root  string
	Fixed bool
	// FallbackErr, when set, refuses sessions that give no usable MCP root
	// instead of using Root.
	FallbackErr error

	// ResolveRoot finds the project root for a directory that a client gives
	// as an MCP root. It must resolve the way the CLI and the editor do, so
	// that every frontend attaches to the same daemon. Nil uses the store's
	// marker search.
	ResolveRoot func(dir string) (string, error)

	// Connect returns the Backend for a resolved root. Nil connects to the
	// shared workspace daemon. Tests replace it.
	Connect func(root string) Backend
}

// Frontend is the MCP side of `dexter mcp`. It owns no workspace state: each
// session's root is negotiated through MCP roots (or fixed), and every tool
// call goes to the Backend of that root, normally the shared workspace daemon.
// Sessions whose roots resolve to the same project share one Backend.
type Frontend struct {
	cfg Config

	mu       sync.Mutex
	closed   bool
	conns    map[string]*workspaceConn
	sessions map[*mcp.ServerSession]*sessionState
}

type workspaceConn struct {
	backend Backend
	users   int
}

type sessionState struct {
	root  string
	dirty bool // the client's roots changed; resolve again on the next call
}

// NewFrontend returns a Frontend for cfg.
func NewFrontend(cfg Config) *Frontend {
	if cfg.Connect == nil {
		cfg.Connect = DaemonBackend
	}
	return &Frontend{
		cfg:      cfg,
		conns:    make(map[string]*workspaceConn),
		sessions: make(map[*mcp.ServerSession]*sessionState),
	}
}

// backendFor returns the Backend of the session's workspace, negotiating the
// root first when the session is new or its roots changed.
func (f *Frontend) backendFor(ctx context.Context, ss *mcp.ServerSession) (Backend, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, errClosed
	}
	if st, ok := f.sessions[ss]; ok && !st.dirty {
		b := f.conns[st.root].backend
		f.mu.Unlock()
		return b, nil
	}
	f.mu.Unlock()

	// Outside the lock: ListRoots waits on the client.
	root, source, err := f.rootFor(ctx, ss)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil, errClosed
	}
	st, bound := f.sessions[ss]
	if bound && st.root == root {
		st.dirty = false
		b := f.conns[root].backend
		f.mu.Unlock()
		return b, nil
	}
	var orphan Backend
	if bound {
		orphan = f.releaseLocked(st.root)
	}
	c, exists := f.conns[root]
	if !exists {
		c = &workspaceConn{backend: f.cfg.Connect(root)}
		f.conns[root] = c
	}
	c.users++
	f.sessions[ss] = &sessionState{root: root}
	f.mu.Unlock()

	log.Printf("MCP session workspace: %s (%s)", root, source)
	if !bound {
		go func() {
			_ = ss.Wait()
			f.detach(ss)
		}()
	}
	if orphan != nil {
		closeBackend(orphan)
	}
	if w, ok := c.backend.(warmer); ok && !exists {
		go w.warm(context.WithoutCancel(ctx))
	}
	return c.backend, nil
}

// rootFor resolves the workspace root of a session.
func (f *Frontend) rootFor(ctx context.Context, ss *mcp.ServerSession) (root, source string, err error) {
	if f.cfg.Fixed {
		return f.cfg.Root, "command line", nil
	}
	root, ok, err := negotiatedRoot(ctx, ss, f.resolve)
	if err != nil {
		return "", "", err
	}
	if !ok {
		if f.cfg.FallbackErr != nil {
			return "", "", f.cfg.FallbackErr
		}
		return f.cfg.Root, "fallback", nil
	}
	return root, "client roots", nil
}

func (f *Frontend) resolve(dir string) (string, error) {
	if f.cfg.ResolveRoot != nil {
		return f.cfg.ResolveRoot(dir)
	}
	return defaultResolveRoot(dir)
}

// releaseLocked drops one user of root and returns its Backend when no session
// uses it any more. The caller closes it outside the lock.
func (f *Frontend) releaseLocked(root string) Backend {
	c, ok := f.conns[root]
	if !ok {
		return nil
	}
	c.users--
	if c.users > 0 {
		return nil
	}
	delete(f.conns, root)
	return c.backend
}

// detach drops a closed session, and closes its workspace connection when no
// other session uses it.
func (f *Frontend) detach(ss *mcp.ServerSession) {
	f.mu.Lock()
	var orphan Backend
	if st, ok := f.sessions[ss]; ok {
		delete(f.sessions, ss)
		orphan = f.releaseLocked(st.root)
	}
	f.mu.Unlock()
	if orphan != nil {
		closeBackend(orphan)
	}
}

func closeBackend(b Backend) {
	if err := b.Close(); err != nil {
		log.Printf("MCP: closing workspace connection: %v", err)
	}
}

// onInitialized starts the new session's workspace, so the index is already
// building at the first tool call. Failures show at that first call, which
// negotiates again with its own context.
func (f *Frontend) onInitialized(ctx context.Context, req *mcp.InitializedRequest) {
	_, _ = f.backendFor(ctx, req.Session)
}

// onRootsChanged marks the session for a new negotiation. It happens at the
// session's next tool call, whose request context reaches the client on every
// transport. Roots that still resolve to the same project keep the workspace.
func (f *Frontend) onRootsChanged(_ context.Context, req *mcp.RootsListChangedRequest) {
	f.mu.Lock()
	if st, ok := f.sessions[req.Session]; ok {
		st.dirty = true
	}
	f.mu.Unlock()
}

// Close closes every workspace connection. The daemons stay up for their
// other clients, and stop on their idle timeout.
func (f *Frontend) Close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	conns := f.conns
	f.conns = nil
	f.sessions = nil
	f.mu.Unlock()
	for _, c := range conns {
		closeBackend(c.backend)
	}
}

// addTool declares one tool on srv. Its handler sends the typed arguments to
// the Backend of the session's workspace.
func addTool[In any](srv *mcp.Server, f *Frontend, t mcp.Tool) {
	mcp.AddTool(srv, &t, func(ctx context.Context, req *mcp.CallToolRequest, args In) (*mcp.CallToolResult, any, error) {
		b, err := f.backendFor(ctx, req.Session)
		if err != nil {
			return nil, nil, err
		}
		raw, err := json.Marshal(args)
		if err != nil {
			return nil, nil, err
		}
		text, err := b.CallTool(ctx, t.Name, raw)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}

// NewServer returns an MCP protocol server with all dexter tools declared.
func NewServer(f *Frontend) *mcp.Server {
	opts := &mcp.ServerOptions{Instructions: Instructions, InitializedHandler: f.onInitialized}
	if !f.cfg.Fixed {
		opts.RootsListChangedHandler = f.onRootsChanged
	}
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "dexter", Title: "Dexter Elixir language tools", Version: version.Version},
		opts,
	)
	for _, spec := range toolSpecs() {
		spec.register(srv, f, spec.tool)
	}
	return srv
}
