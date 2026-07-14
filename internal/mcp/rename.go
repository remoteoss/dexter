package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/diff"
	"github.com/remoteoss/dexter/internal/lsp"
)

type RenameParams struct {
	Module   string `json:"module" jsonschema:"module being renamed, or the module owning the function"`
	Function string `json:"function,omitempty" jsonschema:"if set, rename this function; otherwise rename the module itself (and its submodules)"`
	NewName  string `json:"new_name" jsonschema:"new function name (e.g. get_user), or new fully-qualified module name (e.g. MyApp.Clients)"`
}

func (h *Handler) renameHandler(ctx context.Context, req *mcp.CallToolRequest, args RenameParams) (*mcp.CallToolResult, any, error) {
	module := strings.TrimSpace(args.Module)
	function := strings.TrimSpace(args.Function)
	newName := strings.TrimSpace(args.NewName)
	if module == "" || newName == "" {
		return nil, nil, fmt.Errorf("module and new_name must not be empty")
	}

	var plan *lsp.RenamePlan
	var err error
	var target string
	if function != "" {
		target = fmt.Sprintf("function %s.%s to %s", module, function, newName)
		plan, err = h.lsp.FunctionRenamePlan(module, function, newName)
	} else {
		target = fmt.Sprintf("module %s to %s", module, newName)
		plan, err = h.lsp.ModuleRenamePlan(module, newName)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(plan.Changes) == 0 {
		return textResult(fmt.Sprintf("Renaming %s requires no changes (no occurrences found outside stdlib/deps).", target)), nil, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Rename %s: %d file(s) affected. Nothing has been written; apply the changes below, then call dexter_reindex.\n", target, len(plan.Changes))

	var renames []string
	for _, fc := range plan.Changes {
		if fc.NewPath != "" {
			renames = append(renames, fmt.Sprintf("RENAME %s → %s", h.relPath(fc.Path), h.relPath(fc.NewPath)))
		}
	}
	if len(renames) > 0 {
		fmt.Fprintf(&b, "\nFile renames (the diffs below are written against the NEW paths; move each file first, then apply):\n")
		for _, r := range renames {
			fmt.Fprintf(&b, "  %s\n", r)
		}
	}

	b.WriteString("\n")
	for _, fc := range plan.Changes {
		// For moved files the diff is addressed at the NEW path on both sides:
		// perform the file rename first, then the patch applies cleanly (plain
		// unified diffs can't express renames portably).
		name := h.relPath(fc.Path)
		if fc.NewPath != "" {
			name = h.relPath(fc.NewPath)
		}
		if d := diff.Unified(name, name, fc.OldText, fc.NewText); d != "" {
			b.WriteString(d)
		}
	}
	return textResult(b.String()), nil, nil
}
