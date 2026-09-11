package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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

	var summary lsp.RenameSummary
	var err error
	var target string
	if function != "" {
		target = fmt.Sprintf("%s.%s to %s", module, function, newName)
		summary, err = h.lsp.RenameFunction(module, function, newName)
	} else {
		target = fmt.Sprintf("%s to %s", module, newName)
		summary, err = h.lsp.RenameModule(module, newName)
	}
	if err != nil {
		return nil, nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Renamed %s across %d file(s). The index is updated.\n", target, len(summary.FilesChanged))
	if len(summary.FilesMoved) > 0 {
		fmt.Fprintf(&b, "\nFiles moved to follow the naming convention:\n")
		for from, to := range summary.FilesMoved {
			fmt.Fprintf(&b, "  %s → %s\n", h.relPath(from), h.relPath(to))
		}
	}
	fmt.Fprintf(&b, "\nChanged files:\n")
	for _, fp := range summary.FilesChanged {
		fmt.Fprintf(&b, "  %s\n", h.relPath(fp))
	}
	fmt.Fprintf(&b, "\nReview with git diff; revert with git checkout.\n")
	return textResult(b.String()), nil, nil
}
