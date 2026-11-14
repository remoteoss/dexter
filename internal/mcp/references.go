package mcp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/remoteoss/dexter/internal/lsp"
)

type ReferencesParams struct {
	Module   string `json:"module" jsonschema:"fully-qualified module name, e.g. MyApp.Accounts (aliases are not resolved)"`
	Function string `json:"function,omitempty" jsonschema:"function name; omit to list references to the module itself (aliases, imports, uses, qualified calls)"`
	Callees  bool   `json:"callees,omitempty" jsonschema:"also list what the function calls (its callees) with the lines of the calls; needs function"`
	ProjectParam
}

const maxCallees = 100

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
		Context:         ctx, // a tool call that is canceled stops the search
		FollowDelegates: true,
		ExcludeStdlib:   true,
	})
	if err != nil {
		return "", fmt.Errorf("finding references: %w", err)
	}
	if len(refs) == 0 && h.settled(ctx) {
		return h.references(ctx, args)
	}

	target := module
	if function != "" {
		target = module + "." + function
	}
	if args.Callees && function == "" {
		return "", fmt.Errorf("callees needs a function")
	}
	var b strings.Builder
	if len(refs) == 0 {
		fmt.Fprintf(&b, "No references to %s found in the index.%s\n", target, h.notWatchingNote())
	} else {
		h.writeReferences(&b, target, refs)
	}
	if args.Callees {
		if err := h.writeCallees(&b, module, function); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

// writeReferences lists the references grouped by file, each with the code
// that holds it: the function, or the test block or module body when the
// reference is outside every def.
func (h *Handler) writeReferences(b *strings.Builder, target string, refs []lsp.NameLocation) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].FilePath != refs[j].FilePath {
			return refs[i].FilePath < refs[j].FilePath
		}
		return refs[i].Line < refs[j].Line
	})

	fmt.Fprintf(b, "%d reference(s) to %s, each with the function or block that holds it:\n", len(refs), target)
	callers := h.lsp.NewCallerResolver()

	written := 0
	var lastFile string
	truncated := 0
	for _, r := range refs {
		if written >= maxReferenceLines {
			truncated++
			continue
		}
		if r.FilePath != lastFile {
			fmt.Fprintf(b, "\n%s\n", h.relPath(r.FilePath))
			lastFile = r.FilePath
		}
		srcLine, line, label, ok := h.sourceLine(r.FilePath, r.Line)
		if !ok {
			line = r.Line
		}
		caller := callers.CallerAt(r.FilePath, r.Line)
		fmt.Fprintf(b, "  %d: %s%s  [in %s]\n", line, strings.TrimSpace(srcLine), label, caller.Name())
		written++
	}
	if truncated > 0 {
		fmt.Fprintf(b, "\n... and %d more reference(s) not shown. Narrow the search (e.g. pass a function name) to see the rest.\n", truncated)
	}
}

// writeCallees lists what module.function calls, through the same code as the
// editor's outgoing calls.
func (h *Handler) writeCallees(b *strings.Builder, module, function string) error {
	callees, err := h.lsp.CalleesOf(module, function)
	if err != nil {
		return fmt.Errorf("finding callees: %w", err)
	}
	fmt.Fprintf(b, "\n%s.%s calls %d function(s) (stdlib left out):\n", module, function, len(callees))
	for i, c := range callees {
		if i == maxCallees {
			fmt.Fprintf(b, "  ... and %d more\n", len(callees)-maxCallees)
			break
		}
		lines := make([]string, len(c.Calls))
		for j, l := range c.Calls {
			lines[j] = strconv.Itoa(l)
		}
		fmt.Fprintf(b, "  %s.%s - defined at %s:%d; called at line %s\n", c.Module, c.Function, h.relPath(c.FilePath), c.Line, strings.Join(lines, ", "))
	}
	return nil
}
