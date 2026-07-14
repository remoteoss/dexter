// Package mcp implements dexter's Model Context Protocol server. It exposes
// the index as a set of coarse, agent-oriented tools (modeled on gopls mcp),
// addressed by module/function name rather than file positions because Elixir
// modules are not tied to files.
package mcp

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"

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

// Handler carries the state shared by all tool handlers. In headless mode
// (`dexter mcp`) the lsp.Server is constructed without a client connection; in
// attached mode (`dexter lsp --mcp-listen`) it is the live LSP session, so
// tools see open editor buffers and warm caches.
type Handler struct {
	lsp         *lsp.Server
	store       *store.Store
	projectRoot string
}

type Config struct {
	LSP         *lsp.Server
	Store       *store.Store
	ProjectRoot string
}

func NewHandler(cfg Config) *Handler {
	return &Handler{
		lsp:         cfg.LSP,
		store:       cfg.Store,
		projectRoot: cfg.ProjectRoot,
	}
}

// stdlibRoot reads the Elixir stdlib location from the shared LSP server. It
// is read dynamically because in attached mode the LSP session resolves the
// stdlib during Initialize, after this Handler is constructed.
func (h *Handler) stdlibRoot() string {
	return h.lsp.StdlibRoot()
}

// NewServer returns an MCP server with all dexter tools registered.
func NewServer(h *Handler) *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "dexter", Title: "Dexter Elixir language tools", Version: version.Version},
		&mcp.ServerOptions{Instructions: Instructions},
	)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_workspace",
		Description: "Summarize the Elixir workspace: project root, Mix projects, Elixir stdlib location, and index statistics. Call this first to orient yourself.",
	}, h.workspaceHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_search",
		Description: "Fuzzy-search modules and functions across the workspace by name. Use this to locate symbols when you don't know the defining module.",
	}, h.searchHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_definition",
		Description: "Show where a module or function is defined, with its @doc/@spec and source snippet. Follows defdelegate chains to the real implementation.",
	}, h.definitionHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_references",
		Description: "List all references to a module or function across the workspace, including call sites reached through use-chain injected imports.",
	}, h.referencesHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_module_api",
		Description: "Summarize a module's API: moduledoc, public functions with signatures and doc summaries, macros, delegates, types, callbacks, and submodules.",
	}, h.moduleAPIHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_file_outline",
		Description: "Outline an Elixir source file: every module it defines and each module's functions, macros, and types with line numbers. Elixir files can define many modules; module names do not have to match file paths.",
	}, h.fileOutlineHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_implementations",
		Description: "Find implementations of a behaviour (@behaviour/use declarations) or protocol (defimpl blocks). Optionally locate a specific callback in each implementor.",
	}, h.implementationsHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_call_hierarchy",
		Description: "Show incoming callers and/or outgoing callees of a function, with file:line locations.",
	}, h.callHierarchyHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_reindex",
		Description: "Incrementally reindex the workspace so the index reflects recent edits. This is the only tool that mutates anything, and it only writes dexter's own index database. Call it after creating, editing, or deleting Elixir files.",
	}, h.reindexHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_rename_symbol",
		Description: "Compute a workspace-wide rename of a module or function and return it as a unified diff plus any file renames. Nothing is written: apply the diff yourself, perform the listed file renames, then call dexter_reindex.",
	}, h.renameHandler)

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
