package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/remoteoss/dexter/internal/lsp"
)

type ReferencesParams struct {
	Module   string `json:"module" jsonschema:"fully-qualified module name, e.g. MyApp.Accounts (aliases are not resolved)"`
	Function string `json:"function,omitempty" jsonschema:"function name; omit to list references to the module itself (aliases, imports, uses, qualified calls)"`
}

const maxReferenceLines = 100

func (h *Handler) references(ctx context.Context, args ReferencesParams) (string, error) {
	module := strings.TrimSpace(args.Module)
	if module == "" {
		return "", fmt.Errorf("module must not be empty")
	}
	function := strings.TrimSpace(args.Function)

	// The same reference search as find-references and `dexter references`:
	// use chains, aliases injected by __using__, bare calls in the defining
	// module, and calls through defdelegate facades.
	refs, err := h.lsp.ReferenceNames(module, function, lsp.NameReferenceOptions{
		FollowDelegates: true,
		ExcludeStdlib:   true,
	})
	if err != nil {
		return "", fmt.Errorf("finding references: %w", err)
	}

	target := module
	if function != "" {
		target = module + "." + function
	}
	if len(refs) == 0 {
		return fmt.Sprintf("No references to %s found in the index. If files changed recently, call dexter_reindex first.", target), nil
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].FilePath != refs[j].FilePath {
			return refs[i].FilePath < refs[j].FilePath
		}
		return refs[i].Line < refs[j].Line
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%d reference(s) to %s:\n", len(refs), target)

	written := 0
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
	return b.String(), nil
}
