package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ReferencesParams struct {
	Module   string `json:"module" jsonschema:"fully-qualified module name, e.g. MyApp.Accounts (aliases are not resolved)"`
	Function string `json:"function,omitempty" jsonschema:"function name; omit to list references to the module itself (aliases, imports, uses, qualified calls)"`
}

const maxReferenceLines = 100

func (h *Handler) referencesHandler(ctx context.Context, req *mcp.CallToolRequest, args ReferencesParams) (*mcp.CallToolResult, any, error) {
	module := strings.TrimSpace(args.Module)
	if module == "" {
		return nil, nil, fmt.Errorf("module must not be empty")
	}
	function := strings.TrimSpace(args.Function)

	refs := h.lsp.CollectReferences(module, function)
	if len(refs) == 0 {
		target := module
		if function != "" {
			target = module + "." + function
		}
		return textResult(fmt.Sprintf("No references to %s found in the index. If files changed recently, call dexter_reindex first.", target)), nil, nil
	}

	target := module
	if function != "" {
		target = module + "." + function
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d reference(s) to %s:\n", len(refs), target)

	written := 0
	files := 0
	var lastFile string
	truncated := 0
	for _, r := range refs {
		if written >= maxReferenceLines {
			truncated++
			continue
		}
		if r.FilePath != lastFile {
			fmt.Fprintf(&b, "\n%s\n", h.relPath(r.FilePath))
			lastFile = r.FilePath
			files++
		}
		srcLine := ""
		if line, ok := h.lsp.FileLine(r.FilePath, r.Line); ok {
			srcLine = strings.TrimSpace(line)
		}
		fmt.Fprintf(&b, "  %d: %s\n", r.Line, srcLine)
		written++
	}
	if truncated > 0 {
		fmt.Fprintf(&b, "\n... and %d more reference(s) not shown. Narrow the search (e.g. pass a function name) to see the rest.\n", truncated)
	}
	return textResult(b.String()), nil, nil
}
