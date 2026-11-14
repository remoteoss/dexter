package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/remoteoss/dexter/internal/lsp"
)

type RenameParams struct {
	Module   string `json:"module" jsonschema:"module being renamed, or the module owning the function"`
	Function string `json:"function,omitempty" jsonschema:"if set, rename this function; otherwise rename the module itself (and its submodules)"`
	NewName  string `json:"new_name" jsonschema:"new function name (e.g. get_user), or new fully-qualified module name (e.g. MyApp.Clients)"`
}

func (h *Handler) rename(ctx context.Context, args RenameParams) (string, error) {
	module := strings.TrimSpace(args.Module)
	function := strings.TrimSpace(args.Function)
	newName := strings.TrimSpace(args.NewName)
	if module == "" || newName == "" {
		return "", fmt.Errorf("module and new_name must not be empty")
	}
	if err := h.renameAllowed(); err != nil {
		return "", err
	}

	target := fmt.Sprintf("%s to %s", module, newName)
	if function != "" {
		target = fmt.Sprintf("%s.%s to %s", module, function, newName)
	}
	// The last point where a canceled rename has changed nothing.
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("the rename was canceled before it changed any file: %w", err)
	}
	summary, err := renameSymbol(ctx, h.lsp, module, function, newName)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if summary.IndexPending {
		fmt.Fprintf(&b, "Renamed %s across %d file(s). The files are written; the index update waits behind other index work, so lookups can show the old name until it ends.\n", target, len(summary.FilesChanged))
	} else {
		fmt.Fprintf(&b, "Renamed %s across %d file(s). The index is updated.\n", target, len(summary.FilesChanged))
	}
	if len(summary.FilesFailed) > 0 {
		fmt.Fprintf(&b, "\nWARNING: the rename could not change %d file(s) (%s). These files still use the old name; fix them by hand or revert the rename:\n", len(summary.FilesFailed), summary.FailureReason)
		for _, fp := range summary.FilesFailed {
			fmt.Fprintf(&b, "  %s\n", h.relPath(fp))
		}
	}
	for _, w := range summary.Warnings {
		fmt.Fprintf(&b, "\nWARNING: %s\n", w)
	}
	if len(summary.FilesMoved) > 0 {
		fmt.Fprintf(&b, "\nFiles moved to follow the naming convention:\n")
		from := make([]string, 0, len(summary.FilesMoved))
		for path := range summary.FilesMoved {
			from = append(from, path)
		}
		sort.Strings(from)
		for _, path := range from {
			fmt.Fprintf(&b, "  %s → %s\n", h.relPath(path), h.relPath(summary.FilesMoved[path]))
		}
	}
	fmt.Fprintf(&b, "\nChanged files:\n")
	for _, fp := range summary.FilesChanged {
		fmt.Fprintf(&b, "  %s\n", h.relPath(fp))
	}
	fmt.Fprintf(&b, "\nReview with git diff; revert with git checkout.\n")
	return b.String(), nil
}

// renameSymbol is the only place where the MCP rename calls the rename
// machinery in internal/lsp. RenameFunctionContext and RenameModuleContext are
// the headless entry points: the rename gets every rule of the editor rename
// (one rename at a time, an index that is brought up to date first, atomic
// writes that are all or nothing, callback families) and the rule for a
// caller without an editor: a file that an editor has open without unsaved
// changes is written (the editor reloads it), and a file with unsaved changes
// in an editor, or a file to move that an editor has open, stops the whole
// rename before any change, with an error that names the file.
func renameSymbol(ctx context.Context, server *lsp.Server, module, function, newName string) (*lsp.RenameSummary, error) {
	if function != "" {
		return server.RenameFunctionContext(ctx, module, function, newName)
	}
	return server.RenameModuleContext(ctx, module, newName)
}
