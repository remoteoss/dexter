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

// NewServer returns an MCP server with all dexter tools registered.
func NewServer(h *Handler) *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "dexter", Title: "Dexter Elixir language tools", Version: version.Version},
		&mcp.ServerOptions{Instructions: Instructions},
	)

	// The pointer hints distinguish explicit false from unset; clients must
	// treat unset pessimistically (destructive, open world).
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(bool)}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_workspace",
		Annotations: readOnly,
		Description: "Overview of the Elixir workspace: Mix projects, index size, stdlib status. Call once at the start of Elixir work.",
	}, h.workspaceHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_search",
		Annotations: readOnly,
		Description: "Locate Elixir modules and functions by fuzzy name match. More precise than grep for finding symbols: results are exact definitions with file:line.",
	}, h.searchHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_definition",
		Annotations: readOnly,
		Description: "Definition of an Elixir module or function by name: location, @doc/@spec, and source snippet, following defdelegate to the real implementation. Use instead of grep or reading files to answer where or what a symbol is.",
	}, h.definitionHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_references",
		Annotations: readOnly,
		Description: "All call sites of an Elixir module or function, resolved through aliases, imports, and use-chain injection that grep cannot see. Use for any 'who calls or uses X' question.",
	}, h.referencesHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_module_api",
		Annotations: readOnly,
		Description: "A module's public API in one call: moduledoc, functions with signatures and doc summaries, macros, delegates, types, callbacks, and submodules. Use before reading a module's source.",
	}, h.moduleAPIHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_file_outline",
		Annotations: readOnly,
		Description: "Everything an Elixir file defines: modules, functions, macros, and types with line numbers. Use instead of reading a file to map its contents; one Elixir file can define many modules.",
	}, h.fileOutlineHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_implementations",
		Annotations: readOnly,
		Description: "Implementations of an Elixir behaviour (@behaviour/use) or protocol (defimpl), optionally locating one callback in each implementor. Grep cannot resolve these relationships.",
	}, h.implementationsHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_call_hierarchy",
		Annotations: readOnly,
		Description: "Incoming callers and outgoing callees of an Elixir function, with file:line locations. Use to trace execution paths without reading files.",
	}, h.callHierarchyHandler)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "dexter_reindex",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: new(bool), IdempotentHint: true, OpenWorldHint: new(bool)},
		Description: "Update dexter's index after creating, editing, or deleting Elixir files so lookups stay accurate. Incremental and fast; the only tool that writes, and it writes only dexter's own index database.",
	}, h.reindexHandler)

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
