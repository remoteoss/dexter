package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/remoteoss/dexter/internal/lsp"
)

type DefinitionParams struct {
	Module   string `json:"module" jsonschema:"fully-qualified module name, e.g. MyApp.Accounts (aliases are not resolved)"`
	Function string `json:"function,omitempty" jsonschema:"function/macro/type name without arity; omit to look up the module itself"`
}

func (h *Handler) definition(ctx context.Context, args DefinitionParams) (string, error) {
	module := strings.TrimSpace(args.Module)
	if module == "" {
		return "", fmt.Errorf("module must not be empty")
	}
	function := strings.TrimSpace(args.Function)

	if function == "" {
		return h.moduleDefinition(module)
	}

	// The same name navigation as go-to-definition and `dexter lookup`: use
	// chains, generated functions, and the lines that declared them. Direct
	// definitions come first because they show whether this is a defdelegate
	// facade.
	direct, err := h.lsp.LookupName(module, function, lsp.NameLookupOptions{})
	if err != nil {
		return "", fmt.Errorf("looking up function: %w", err)
	}

	var b strings.Builder
	if len(direct) == 0 {
		// No direct definition. The function can still resolve through a
		// defdelegate chain.
		resolved, err := h.lsp.LookupName(module, function, lsp.NameLookupOptions{FollowDelegates: true})
		if err != nil {
			return "", fmt.Errorf("looking up function: %w", err)
		}
		if len(resolved) == 0 {
			return fmt.Sprintf("%s.%s is not in the index. It can be defined in a quote block that the index cannot see, or misspelled. Try dexter_search or dexter_module_api %s.", module, function, module), nil
		}
		for _, r := range resolved {
			h.writeDefinition(&b, module, function, r)
		}
		return b.String(), nil
	}

	for _, r := range direct {
		h.writeDefinition(&b, module, function, r)
		if r.Kind != "defdelegate" {
			continue
		}
		targetModule, targetFunction, ok := h.delegateTargetAt(module, function, r)
		if !ok {
			continue
		}
		targets, err := h.lsp.LookupName(module, function, lsp.NameLookupOptions{FollowDelegates: true})
		if err != nil || len(targets) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\nDelegates to %s.%s:\n", targetModule, targetFunction)
		for _, t := range targets {
			if t.FilePath == r.FilePath && t.Line == r.Line {
				continue
			}
			h.writeDefinition(&b, targetModule, targetFunction, t)
		}
	}
	return b.String(), nil
}

// delegateTargetAt names the function that the defdelegate at r forwards to.
func (h *Handler) delegateTargetAt(module, function string, r lsp.NameLocation) (targetModule, targetFunction string, ok bool) {
	defs, err := h.store.LookupFunction(module, function)
	if err != nil {
		return "", "", false
	}
	for _, d := range defs {
		if d.FilePath != r.FilePath || d.Line != r.Line || d.DelegateTo == "" {
			continue
		}
		targetFunction = function
		if d.DelegateAs != "" {
			targetFunction = d.DelegateAs
		}
		return d.DelegateTo, targetFunction, true
	}
	return "", "", false
}

func (h *Handler) moduleDefinition(module string) (string, error) {
	results, err := h.lsp.LookupName(module, "", lsp.NameLookupOptions{})
	if err != nil {
		return "", fmt.Errorf("looking up module: %w", err)
	}
	if len(results) == 0 {
		return fmt.Sprintf("Module %s is not in the index. Use dexter_search to find the right name, or dexter_reindex if it was just created.", module), nil
	}

	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%s %s - %s:%d\n", moduleKindLabel(r.Kind), module, h.relPath(r.FilePath), r.Line)
		if r.Kind != "defimpl" {
			if text, _, ok := h.lsp.ReadFileText(r.FilePath); ok {
				if doc := lsp.NewTokenizedFile(text).ExtractModuledoc(r.Line - 1); doc != "" {
					fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(doc, "\n"))
				}
			}
		}
	}
	return b.String(), nil
}

// writeDefinition renders one definition with location, @spec/@doc, and the
// definition head line.
func (h *Handler) writeDefinition(b *strings.Builder, module, function string, r lsp.NameLocation) {
	kind := r.Kind
	if kind == "" {
		kind = "def"
	}
	fmt.Fprintf(b, "%s (%s) - %s:%d\n", symbolName(module, function, r.Arity), kind, h.relPath(r.FilePath), r.Line)

	text, _, ok := h.lsp.ReadFileText(r.FilePath)
	if !ok {
		return
	}
	tf := lsp.NewTokenizedFile(text)
	doc, spec := tf.ExtractDocAbove(r.Line - 1)
	if spec != "" {
		fmt.Fprintf(b, "%s\n", spec)
	}
	if head, ok := h.lsp.FileLine(r.FilePath, r.Line); ok {
		fmt.Fprintf(b, "%s\n", strings.TrimRight(head, " \t"))
	}
	if doc != "" {
		fmt.Fprintf(b, "\n%s\n", strings.TrimRight(doc, "\n"))
	}
}
