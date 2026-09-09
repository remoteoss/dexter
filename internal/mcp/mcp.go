// Package mcp implements dexter's Model Context Protocol server. It exposes
// the index as a set of coarse, agent-oriented tools (modeled on gopls mcp),
// addressed by module/function name rather than file positions because Elixir
// modules are not tied to files.
package mcp

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
)

// Instructions is the agent-facing usage guide, offered to MCP clients via the
// server's instructions field and printable with `dexter mcp --instructions`.
//
//go:embed instructions.md
var Instructions string

// Handler carries the state shared by all tool handlers. In attached mode
// (`dexter lsp --mcp-listen`) and with an explicit CLI path the workspace is
// fixed at construction; in attached mode the lsp.Server is the live LSP
// session, so tools see open editor buffers and warm caches.
//
// A negotiating handler (headless `dexter mcp` with no explicit path) has no
// fixed workspace. Each session's root is obtained through MCP roots and
// resolved the way the LSP resolves its own, and every resolved root gets one
// workspace (a binding), shared by all sessions that resolve to it. Tool
// calls run against a per-call view of the session's binding.
type Handler struct {
	lsp         *lsp.Server
	store       *store.Store
	projectRoot string

	negotiate    bool
	fallbackRoot string // used by sessions that provide no usable root
	mu           sync.Mutex
	bindings     map[string]*binding             // resolved root → workspace
	sessions     map[*mcp.ServerSession]*binding // session → its workspace
	dirty        map[*mcp.ServerSession]bool     // roots changed; re-resolve on next call
	watched      map[*mcp.ServerSession]bool     // a Wait goroutine will detach this session
	closed       bool
}

type Config struct {
	LSP         *lsp.Server
	Store       *store.Store
	ProjectRoot string

	// NegotiateRoots serves one workspace per client-provided root instead of
	// the fixed LSP/Store pair, with ProjectRoot as the fallback for sessions
	// that provide none.
	NegotiateRoots bool
}

func NewHandler(cfg Config) *Handler {
	if cfg.NegotiateRoots {
		return &Handler{
			negotiate:    true,
			fallbackRoot: cfg.ProjectRoot,
			bindings:     make(map[string]*binding),
			sessions:     make(map[*mcp.ServerSession]*binding),
			dirty:        make(map[*mcp.ServerSession]bool),
			watched:      make(map[*mcp.ServerSession]bool),
		}
	}
	return &Handler{
		lsp:         cfg.LSP,
		store:       cfg.Store,
		projectRoot: cfg.ProjectRoot,
	}
}

var errClosed = errors.New("the MCP server is shutting down")

// handlerFor returns the Handler a tool call should run against: the fixed
// one, or a view of the workspace bound to the call's session, waiting out
// that workspace's initial index.
func (h *Handler) handlerFor(ctx context.Context, req *mcp.CallToolRequest) (*Handler, error) {
	if !h.negotiate {
		return h, nil
	}
	b, err := h.bindingFor(ctx, req.Session)
	if err != nil {
		return nil, err
	}
	if err := b.awaitIndex(ctx); err != nil {
		if b.initErr != nil {
			// A workspace that failed to open is forgotten so the next call
			// renegotiates from scratch instead of re-reporting a stale error.
			h.forget(b)
		}
		return nil, err
	}
	return &Handler{lsp: b.lsp, store: b.store, projectRoot: b.root}, nil
}

// bindingFor returns the session's workspace, negotiating its root first when
// the session is new or its roots changed.
func (h *Handler) bindingFor(ctx context.Context, ss *mcp.ServerSession) (*binding, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, errClosed
	}
	b, bound := h.sessions[ss]
	dirty := h.dirty[ss]
	h.mu.Unlock()
	if bound && !dirty {
		return b, nil
	}

	// Resolve outside the lock: ListRoots blocks on the client.
	root, ok, err := negotiatedRoot(ctx, ss)
	if err != nil {
		return nil, err
	}
	source := "client roots"
	if !ok {
		root = h.fallbackRoot
		source = "fallback"
	}

	var created, orphan *binding
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, errClosed
	}
	delete(h.dirty, ss)
	if cur, bound := h.sessions[ss]; bound && cur.root == root {
		h.mu.Unlock()
		return cur, nil
	}
	nb, exists := h.bindings[root]
	if !exists {
		nb = &binding{root: root, initDone: make(chan struct{}), indexed: make(chan struct{})}
		h.bindings[root] = nb
		created = nb
	}
	if cur, bound := h.sessions[ss]; bound {
		orphan = h.releaseLocked(ss, cur)
	}
	h.sessions[ss] = nb
	log.Printf("MCP session workspace: %s (%s)", root, source)
	if !h.watched[ss] {
		h.watched[ss] = true
		go func() {
			_ = ss.Wait()
			h.detachSession(ss)
		}()
	}
	h.mu.Unlock()

	if orphan != nil {
		go orphan.close()
	}
	if created != nil {
		created.init()
	}
	return nb, nil
}

// releaseLocked unbinds ss from b and reports b when no other session uses it
// any more, removing it from the handler; the caller closes it outside the
// lock. Callers must hold h.mu.
func (h *Handler) releaseLocked(ss *mcp.ServerSession, b *binding) (orphan *binding) {
	delete(h.sessions, ss)
	for _, sb := range h.sessions {
		if sb == b {
			return nil
		}
	}
	if h.bindings[b.root] == b {
		delete(h.bindings, b.root)
	}
	return b
}

// detachSession drops everything the handler holds for a closed session,
// tearing down its workspace when no other session shares it.
func (h *Handler) detachSession(ss *mcp.ServerSession) {
	h.mu.Lock()
	var orphan *binding
	if b, bound := h.sessions[ss]; bound {
		orphan = h.releaseLocked(ss, b)
	}
	delete(h.dirty, ss)
	delete(h.watched, ss)
	h.mu.Unlock()
	if orphan != nil {
		orphan.close()
	}
}

// forget removes a workspace that failed to open, with every session bound to
// it, so subsequent calls renegotiate.
func (h *Handler) forget(b *binding) {
	h.mu.Lock()
	if h.bindings[b.root] == b {
		delete(h.bindings, b.root)
	}
	for ss, sb := range h.sessions {
		if sb == b {
			delete(h.sessions, ss)
		}
	}
	h.mu.Unlock()
}

// onInitialized warms up a new session's workspace so the first tool call
// finds the index already building. Best-effort: failures surface on that
// first call, which renegotiates on its own context.
func (h *Handler) onInitialized(ctx context.Context, req *mcp.InitializedRequest) {
	_, _ = h.bindingFor(ctx, req.Session)
}

// onRootsChanged marks the session for renegotiation. The re-resolve happens
// on the session's next tool call, whose request context reaches the client
// reliably on every transport; if the roots still resolve to the same project
// the workspace is kept as is.
func (h *Handler) onRootsChanged(_ context.Context, req *mcp.RootsListChangedRequest) {
	h.mu.Lock()
	if _, bound := h.sessions[req.Session]; bound {
		h.dirty[req.Session] = true
	}
	h.mu.Unlock()
}

// Close tears down every workspace a negotiating handler holds. Fixed-mode
// handlers own nothing: their store and server belong to the caller.
func (h *Handler) Close() {
	h.mu.Lock()
	if h.closed || !h.negotiate {
		h.mu.Unlock()
		return
	}
	h.closed = true
	bindings := make([]*binding, 0, len(h.bindings))
	for _, b := range h.bindings {
		bindings = append(bindings, b)
	}
	h.bindings = nil
	h.sessions = nil
	h.mu.Unlock()
	for _, b := range bindings {
		b.close()
	}
}

// addTool registers a tool handler so each call runs against the workspace
// bound to its session (in fixed mode, always the handler itself).
func addTool[In any](srv *mcp.Server, h *Handler, t *mcp.Tool, f func(*Handler, context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error)) {
	mcp.AddTool(srv, t, func(ctx context.Context, req *mcp.CallToolRequest, args In) (*mcp.CallToolResult, any, error) {
		hh, err := h.handlerFor(ctx, req)
		if err != nil {
			return nil, nil, err
		}
		return f(hh, ctx, req, args)
	})
}

// NewServer returns an MCP server with all dexter tools registered.
func NewServer(h *Handler) *mcp.Server {
	opts := &mcp.ServerOptions{Instructions: Instructions}
	if h.negotiate {
		opts.InitializedHandler = h.onInitialized
		opts.RootsListChangedHandler = h.onRootsChanged
	}
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "dexter", Title: "Dexter Elixir language tools", Version: version.Version},
		opts,
	)

	// The pointer hints distinguish explicit false from unset; clients must
	// treat unset pessimistically (destructive, open world).
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(bool)}

	addTool(srv, h, &mcp.Tool{
		Name:        "dexter_workspace",
		Annotations: readOnly,
		Description: "Overview of the Elixir workspace: Mix projects, index size, stdlib status. Call once at the start of Elixir work.",
	}, (*Handler).workspaceHandler)

	addTool(srv, h, &mcp.Tool{
		Name:        "dexter_search",
		Annotations: readOnly,
		Description: "Locate Elixir modules and functions by fuzzy name match. More precise than grep for finding symbols: results are exact definitions with file:line.",
	}, (*Handler).searchHandler)

	addTool(srv, h, &mcp.Tool{
		Name:        "dexter_definition",
		Annotations: readOnly,
		Description: "Definition of an Elixir module or function by name: location, @doc/@spec, and source snippet, following defdelegate to the real implementation. Use instead of grep or reading files to answer where or what a symbol is.",
	}, (*Handler).definitionHandler)

	addTool(srv, h, &mcp.Tool{
		Name:        "dexter_references",
		Annotations: readOnly,
		Description: "All call sites of an Elixir module or function, resolved through aliases, imports, and use-chain injection that grep cannot see. Use for any 'who calls or uses X' question.",
	}, (*Handler).referencesHandler)

	addTool(srv, h, &mcp.Tool{
		Name:        "dexter_module_api",
		Annotations: readOnly,
		Description: "A module's public API in one call: moduledoc, functions with signatures and doc summaries, macros, delegates, types, callbacks, and submodules. Use before reading a module's source.",
	}, (*Handler).moduleAPIHandler)

	addTool(srv, h, &mcp.Tool{
		Name:        "dexter_file_outline",
		Annotations: readOnly,
		Description: "Everything an Elixir file defines: modules, functions, macros, and types with line numbers. Use instead of reading a file to map its contents; one Elixir file can define many modules.",
	}, (*Handler).fileOutlineHandler)

	addTool(srv, h, &mcp.Tool{
		Name:        "dexter_implementations",
		Annotations: readOnly,
		Description: "Implementations of an Elixir behaviour (@behaviour/use) or protocol (defimpl), optionally locating one callback in each implementor. Grep cannot resolve these relationships.",
	}, (*Handler).implementationsHandler)

	addTool(srv, h, &mcp.Tool{
		Name:        "dexter_call_hierarchy",
		Annotations: readOnly,
		Description: "Incoming callers and outgoing callees of an Elixir function, with file:line locations. Use to trace execution paths without reading files.",
	}, (*Handler).callHierarchyHandler)

	addTool(srv, h, &mcp.Tool{
		Name:        "dexter_reindex",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(bool), IdempotentHint: true, OpenWorldHint: new(bool)},
		Description: "Force an immediate incremental reindex. The index already updates automatically as files change; use this only when a lookup seems stale. The only tool that writes, and it writes only dexter's own index database.",
	}, (*Handler).reindexHandler)

	addTool(srv, h, &mcp.Tool{
		Name:        "dexter_rename_symbol",
		Description: "Rename an Elixir module or function across the whole workspace, exactly like an editor rename: writes the changes to disk, moves files that follow the naming convention, and updates the index. Reports every file changed; review with git diff.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: new(bool)},
	}, (*Handler).renameHandler)

	return srv
}

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}
}

// relPath renders p relative to the project root when it is inside it.
func (h *Handler) relPath(p string) string {
	if rel, err := filepath.Rel(h.projectRoot, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}

// resolvePath interprets a user-supplied path against the project root.
func (h *Handler) resolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(h.projectRoot, p)
}

// symbolName renders Module.function/arity (or just the module name).
func symbolName(module, function string, arity int) string {
	if function == "" {
		return module
	}
	return fmt.Sprintf("%s.%s/%d", module, function, arity)
}

// firstDocLine returns the first non-empty line of a doc string, truncated.
func firstDocLine(doc string) string {
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const max = 120
		if len(line) > max {
			return line[:max-3] + "..."
		}
		return line
	}
	return ""
}
