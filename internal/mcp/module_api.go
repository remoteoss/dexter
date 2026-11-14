package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/store"
)

type ModuleAPIParams struct {
	Module         string `json:"module" jsonschema:"fully-qualified module name, e.g. MyApp.Accounts (aliases are not resolved)"`
	IncludePrivate bool   `json:"include_private,omitempty" jsonschema:"also list defp/defmacrop definitions (default false)"`
	ProjectParam
}

func (h *Handler) moduleAPI(ctx context.Context, args ModuleAPIParams) (string, error) {
	module := strings.TrimSpace(args.Module)
	if module == "" {
		return "", fmt.Errorf("module must not be empty")
	}

	modResults, err := h.store.LookupModule(module)
	if err != nil {
		return "", fmt.Errorf("looking up module: %w", err)
	}
	var moduleDef *store.LookupResult
	implCount := 0
	isProtocol := false
	for i := range modResults {
		switch modResults[i].Kind {
		case "defimpl":
			implCount++
		case "defprotocol":
			isProtocol = true
			moduleDef = &modResults[i]
		case "module":
			if moduleDef == nil {
				moduleDef = &modResults[i]
			}
		}
	}
	if moduleDef == nil {
		if h.settled(ctx) {
			return h.moduleAPI(ctx, args)
		}
		return fmt.Sprintf("Module %s is not in the index. Use dexter_search to find the right name.%s", module, h.notWatchingNote()), nil
	}

	var b strings.Builder
	kind := "module"
	if isProtocol {
		kind = "protocol"
	}
	fmt.Fprintf(&b, "%s %s - %s:%d\n", kind, module, h.relPath(moduleDef.FilePath), moduleDef.Line)
	if isProtocol && implCount > 0 {
		fmt.Fprintf(&b, "%d defimpl implementation(s); list them with dexter_implementations.\n", implCount)
	}

	if moduledoc := h.extractModuledoc(moduleDef.FilePath, moduleDef.Line); moduledoc != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(moduledoc, "\n"))
	}

	funcs, err := h.store.ListModuleFunctions(module, !args.IncludePrivate)
	if err != nil {
		return "", fmt.Errorf("listing functions: %w", err)
	}
	callbacks, err := h.store.ListModuleCallbacks(module)
	if err != nil {
		return "", fmt.Errorf("listing callbacks: %w", err)
	}

	// Bucket by section, preserving store order (name, arity).
	sections := map[string][]store.CompletionResult{}
	for _, f := range funcs {
		sections[sectionFor(f.Kind)] = append(sections[sectionFor(f.Kind)], f)
	}

	docs := h.newDocExtractor()
	writeSection := func(title string, entries []store.CompletionResult) {
		if len(entries) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", title)
		for _, e := range entries {
			sig := fmt.Sprintf("%s/%d", e.Function, e.Arity)
			if e.Params != "" {
				sig = fmt.Sprintf("%s(%s)", e.Function, e.Params)
			}
			line := fmt.Sprintf("  %s [%s:%d]", sig, h.relPath(e.FilePath), e.Line)
			if e.Kind == "defdelegate" {
				if target := h.delegateTarget(module, e.Function, e.Arity); target != "" {
					line += " → " + target
				}
			}
			if doc := docs.docFor(e.FilePath, e.Line); doc != "" {
				line += "\n    " + doc
			}
			b.WriteString(line + "\n")
		}
	}

	writeSection("Functions", sections["functions"])
	writeSection("Macros", sections["macros"])
	writeSection("Guards", sections["guards"])
	writeSection("Delegates", sections["delegates"])
	writeSection("Types", sections["types"])
	writeSection("Private functions", sections["private"])
	writeSection("Callbacks (this module is a behaviour)", callbacks)

	members := h.lsp.ModuleMembers(module)
	if len(members) > 0 {
		fmt.Fprintf(&b, "\nInjected or generated (not in this module's source; dexter_definition finds them too):\n")
		for i, m := range members {
			if i == maxMembersShown {
				fmt.Fprintf(&b, "  ... and %d more\n", len(members)-i)
				break
			}
			line := "  " + memberSignature(m)
			if m.Kind == "defmacro" {
				line += " (macro)"
			}
			if loc := h.memberLocation(module, m); loc != "" {
				line += " [" + loc + "]"
			}
			line += " - " + h.memberOrigin(module, m)
			doc := m.Doc
			if doc == "" && m.Source == lsp.MemberInjected && m.FilePath != "" {
				doc = docs.docFor(m.FilePath, m.Line)
			} else {
				doc = firstDocLine(doc)
			}
			if doc != "" {
				line += "\n    " + doc
			}
			b.WriteString(line + "\n")
		}
	}

	if subs, err := h.store.ListSubmodules(module); err == nil && len(subs) > 0 {
		fmt.Fprintf(&b, "\nSubmodules (%d):\n", len(subs))
		const maxSubs = 20
		for i, s := range subs {
			if i == maxSubs {
				fmt.Fprintf(&b, "  ... and %d more\n", len(subs)-maxSubs)
				break
			}
			fmt.Fprintf(&b, "  %s\n", s)
		}
	}

	if len(funcs) == 0 && len(callbacks) == 0 && len(members) == 0 {
		fmt.Fprintf(&b, "\nNo functions indexed for this module.\n")
	}
	return b.String(), nil
}

func sectionFor(kind string) string {
	switch kind {
	case "defmacro":
		return "macros"
	case "defguard":
		return "guards"
	case "defdelegate":
		return "delegates"
	case "type", "opaque":
		return "types"
	case "defp", "defmacrop", "defguardp":
		return "private"
	default:
		return "functions"
	}
}

// delegateTarget renders "Target.function" for a defdelegate entry.
func (h *Handler) delegateTarget(module, function string, arity int) string {
	results, err := h.store.LookupFunction(module, function)
	if err != nil {
		return ""
	}
	for _, r := range results {
		if r.Kind == "defdelegate" && r.Arity == arity && r.DelegateTo != "" {
			target := r.DelegateTo + "." + function
			if r.DelegateAs != "" {
				target = r.DelegateTo + "." + r.DelegateAs
			}
			return target
		}
	}
	return ""
}

func (h *Handler) extractModuledoc(filePath string, defLine int) string {
	a, ok := h.at(filePath, defLine)
	if !ok {
		return ""
	}
	return a.tokenized().ExtractModuledoc(a.line - 1)
}

// docExtractor extracts @doc summaries, tokenizing each source file at most once.
type docExtractor struct {
	h *Handler
}

func (h *Handler) newDocExtractor() *docExtractor {
	return &docExtractor{h: h}
}

func (d *docExtractor) docFor(filePath string, defLine int) string {
	a, ok := d.h.at(filePath, defLine)
	if !ok {
		return ""
	}
	doc, _ := a.tokenized().ExtractDocAbove(a.line - 1)
	return firstDocLine(doc)
}
