// Package mcp implements dexter's Model Context Protocol frontend. It exposes
// the index as a set of coarse, agent-oriented tools (modeled on gopls mcp),
// addressed by module/function name rather than file positions because Elixir
// modules are not tied to files.
//
// The package has two halves. The frontend (frontend.go) speaks MCP to the
// agent and owns nothing of the workspace: for each workspace root it connects
// to the shared workspace daemon, like the editor and the CLI. The tool bodies
// (this file and one file per tool) run inside the daemon, as the control
// method MethodTool, against the daemon's store and headless language service.
package mcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/workspace"
)

// Instructions is the agent-facing usage guide, offered to MCP clients via the
// server's instructions field and printable with `dexter mcp --instructions`.
//
//go:embed instructions.md
var Instructions string

// Handler runs tool bodies against one workspace. In production it lives in
// the workspace daemon; tests build it over an in-process runtime.
type Handler struct {
	rt          *workspace.Runtime
	lsp         *lsp.Server
	store       *store.Store
	projectRoot string

	// sources caches the files that one tool call reads. Call gives each call
	// its own Handler, so the cache never outlives the call.
	sources sourceCache
}

// NewHandler returns a Handler over the runtime's store and the given language
// service.
func NewHandler(rt *workspace.Runtime, server *lsp.Server) *Handler {
	return &Handler{rt: rt, lsp: server, store: rt.Store(), projectRoot: rt.Root()}
}

const renameToolName = "dexter_rename_symbol"

// toolSpec is one tool: its MCP declaration for the frontend and its body for
// the daemon. The two halves share the parameter type, so the input schema the
// agent sees and the arguments the body decodes cannot drift apart.
type toolSpec struct {
	tool     mcp.Tool
	register func(srv *mcp.Server, f *Frontend, t mcp.Tool)
	run      func(h *Handler, ctx context.Context, raw json.RawMessage) (string, error)
	// ownStatus is true for a tool whose answer already describes the index
	// state, so Call adds no notes to it.
	ownStatus bool
}

func newToolSpec[In any](t mcp.Tool, body func(*Handler, context.Context, In) (string, error)) toolSpec {
	return toolSpec{
		tool:     t,
		register: func(srv *mcp.Server, f *Frontend, t mcp.Tool) { addTool[In](srv, f, t) },
		run: func(h *Handler, ctx context.Context, raw json.RawMessage) (string, error) {
			var in In
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", fmt.Errorf("invalid arguments for %s: %w", t.Name, err)
				}
			}
			return body(h, ctx, in)
		},
	}
}

// toolSpecs lists every tool in the order that tools/list shows them.
func toolSpecs() []toolSpec {
	// The pointer hints distinguish explicit false from unset; clients must
	// treat unset pessimistically (destructive, open world).
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(bool)}

	workspaceSpec := newToolSpec(mcp.Tool{
		Name:        "dexter_workspace",
		Annotations: readOnly,
		Description: "Overview of the Elixir workspace: Mix projects, index size and state, stdlib status, and active warnings. Call once at the start of Elixir work.",
	}, (*Handler).workspace)
	workspaceSpec.ownStatus = true

	return []toolSpec{
		workspaceSpec,
		newToolSpec(mcp.Tool{
			Name:        "dexter_search",
			Annotations: readOnly,
			Description: "Locate Elixir modules and functions by fuzzy name match. More precise than grep for finding symbols: results are exact definitions with file:line.",
		}, (*Handler).search),
		newToolSpec(mcp.Tool{
			Name:        "dexter_definition",
			Annotations: readOnly,
			Description: "Definition of an Elixir module or function by name: location, @doc/@spec, and source snippet, following defdelegate to the real implementation. Use instead of grep or reading files to answer where or what a symbol is.",
		}, (*Handler).definition),
		newToolSpec(mcp.Tool{
			Name:        "dexter_references",
			Annotations: readOnly,
			Description: "All call sites of an Elixir module or function, resolved through aliases, imports, and use-chain injection that grep cannot see. Use for any 'who calls or uses X' question.",
		}, (*Handler).references),
		newToolSpec(mcp.Tool{
			Name:        "dexter_module_api",
			Annotations: readOnly,
			Description: "A module's public API in one call: moduledoc, functions with signatures and doc summaries, macros, delegates, types, callbacks, and submodules. Use before reading a module's source.",
		}, (*Handler).moduleAPI),
		newToolSpec(mcp.Tool{
			Name:        "dexter_file_outline",
			Annotations: readOnly,
			Description: "Everything an Elixir file defines: modules, functions, macros, and types with line numbers. Use instead of reading a file to map its contents; one Elixir file can define many modules.",
		}, (*Handler).fileOutline),
		newToolSpec(mcp.Tool{
			Name:        "dexter_implementations",
			Annotations: readOnly,
			Description: "Implementations of an Elixir behaviour (@behaviour/use) or protocol (defimpl), optionally locating one callback in each implementor. Grep cannot resolve these relationships.",
		}, (*Handler).implementations),
		newToolSpec(mcp.Tool{
			Name:        "dexter_call_hierarchy",
			Annotations: readOnly,
			Description: "Incoming callers and outgoing callees of an Elixir function, with file:line locations. Use to trace execution paths without reading files.",
		}, (*Handler).callHierarchy),
		newToolSpec(mcp.Tool{
			Name:        "dexter_reindex",
			Annotations: &mcp.ToolAnnotations{DestructiveHint: new(bool), IdempotentHint: true, OpenWorldHint: new(bool)},
			Description: "Force an immediate incremental reindex. The index already updates automatically as files change; use this only when a lookup seems stale. It writes only dexter's own index database.",
		}, (*Handler).reindex),
		newToolSpec(mcp.Tool{
			Name:        renameToolName,
			Description: "Rename an Elixir module or function across the whole workspace, exactly like an editor rename: writes the changes to disk, moves files that follow the naming convention, and updates the index. Reports every file changed; review with git diff.",
			Annotations: &mcp.ToolAnnotations{OpenWorldHint: new(bool)},
		}, (*Handler).rename),
	}
}

// Call runs one tool. It first waits up to wait for the workspace's initial
// index, and then adds a note when the answer comes from an index that is
// still building or that is degraded, so the agent does not take an
// incomplete answer as complete.
func (h *Handler) Call(ctx context.Context, name string, args json.RawMessage, wait time.Duration) (string, error) {
	var spec *toolSpec
	for _, s := range toolSpecs() {
		if s.tool.Name == name {
			spec = &s
			break
		}
	}
	if spec == nil {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	if wait > 0 && !h.rt.IsReady() {
		waitCtx, cancel := context.WithTimeout(ctx, wait)
		_ = h.rt.WaitReady(waitCtx)
		cancel()
	}

	call := &Handler{rt: h.rt, lsp: h.lsp, store: h.store, projectRoot: h.projectRoot}
	text, err := spec.run(call, ctx, args)
	var noteList []string
	if !spec.ownStatus {
		if n := h.indexNotes(); n != "" {
			noteList = append(noteList, n)
		}
	}
	if n := call.unsavedNote(); n != "" {
		noteList = append(noteList, n)
	}
	notes := strings.Join(noteList, "\n")
	if err != nil {
		if notes != "" {
			return "", fmt.Errorf("%w\n%s", err, notes)
		}
		return "", err
	}
	if notes != "" {
		text = strings.TrimRight(text, "\n") + "\n\n" + notes
	}
	return text, nil
}

// indexNotes describes an index that is still building, or that has a warning
// or an error, the same conditions that an editor shows and that `dexter
// lookup` prints. It is empty for a ready index with no such condition.
func (h *Handler) indexNotes() string {
	var notes []string
	if !h.rt.IsReady() {
		notes = append(notes, "Note: the workspace index is still building, so this answer can be incomplete. Retry shortly for complete results.")
	}
	for _, c := range h.rt.IndexConditions() {
		if c.Severity == notify.Info {
			continue
		}
		notes = append(notes, fmt.Sprintf("Note (%s): %s", c.Severity, strings.TrimPrefix(c.Message, "Dexter: ")))
	}
	return strings.Join(notes, "\n")
}

// renameAllowed refuses a rename while the index is incomplete: the rename
// finds its sites in the index, so it would change some call sites and leave
// others with the old name.
func (h *Handler) renameAllowed() error {
	if !h.rt.IsReady() {
		return errors.New("the workspace index is still building, so a rename now could miss call sites. Nothing was changed. Retry when dexter_workspace shows the index as ready")
	}
	for _, c := range h.rt.IndexConditions() {
		switch c.Key {
		case lsp.CondIndexBuild, lsp.CondIndexRebuild, lsp.CondIndexUnavailable, lsp.CondIndexFallback:
			return fmt.Errorf("the index is not complete, so a rename now could miss call sites. Nothing was changed. %s", strings.TrimPrefix(c.Message, "Dexter: "))
		}
	}
	return nil
}

// relPath renders p relative to the project root when it is inside it.
func (h *Handler) relPath(p string) string {
	if rel, err := filepath.Rel(h.projectRoot, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
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
