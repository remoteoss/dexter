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
	summary, err := renameSymbol(h.lsp, module, function, newName)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Renamed %s across %d file(s). The index is updated.\n", target, len(summary.FilesChanged))
	if len(summary.FilesFailed) > 0 {
		fmt.Fprintf(&b, "\nWARNING: the rename could not change %d file(s) (%s). These files still use the old name; fix them by hand or revert the rename:\n", len(summary.FilesFailed), summary.FailureReason)
		for _, fp := range summary.FilesFailed {
			fmt.Fprintf(&b, "  %s\n", h.relPath(fp))
		}
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
// machinery in internal/lsp.
//
// TODO: The workspace daemon is shared with editors. When the shared rule for
// renames from a frontend without an editor lands in internal/lsp (do not
// write over unsaved editor buffers: write closed files and clean open
// files, and refuse with an actionable error when an affected file has unsaved
// changes in an editor), call its headless entry point here instead.
func renameSymbol(server *lsp.Server, module, function, newName string) (*lsp.RenameSummary, error) {
	if function != "" {
		return server.RenameFunction(module, function, newName)
	}
	return server.RenameModule(module, newName)
}
