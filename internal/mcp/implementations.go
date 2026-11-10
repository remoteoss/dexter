package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/remoteoss/dexter/internal/lsp"
)

const maxProtocolImpls = 100

type ImplementationsParams struct {
	Module   string `json:"module" jsonschema:"behaviour or protocol module, fully qualified"`
	Function string `json:"function,omitempty" jsonschema:"callback or protocol function name; when set, locate its definition in each implementor"`
}

func (h *Handler) implementations(ctx context.Context, args ImplementationsParams) (string, error) {
	module := strings.TrimSpace(args.Module)
	if module == "" {
		return "", fmt.Errorf("module must not be empty")
	}

	modResults, err := h.store.LookupModule(module)
	if err != nil {
		return "", fmt.Errorf("looking up module: %w", err)
	}

	// Protocol: defimpl blocks and @derive, as the editor finds them.
	var decl *int
	for i, r := range modResults {
		if r.Kind == "defprotocol" {
			decl = &i
			break
		}
	}
	if decl != nil {
		impls, _ := h.lsp.ProtocolImplementations(module)
		var defimpls, derives []lsp.ProtocolImpl
		for _, impl := range impls {
			if impl.Kind == "derive" {
				derives = append(derives, impl)
			} else {
				defimpls = append(defimpls, impl)
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s is a protocol (defprotocol at %s:%d).\n", module, h.relPath(modResults[*decl].FilePath), modResults[*decl].Line)
		if function := strings.TrimSpace(args.Function); function != "" {
			defs, declared := h.lsp.ProtocolFunctionImpls(module, function)
			if !declared {
				return fmt.Sprintf("%s does not define a protocol function named %s. List its functions with dexter_module_api.", module, function), nil
			}
			fmt.Fprintf(&b, "\nImplementations of protocol function %s.%s:\n", module, function)
			for _, d := range defs {
				fmt.Fprintf(&b, "  %s - %s:%d\n", symbolName(module, function, d.Arity), h.relPath(d.FilePath), d.Line)
			}
			if len(defs) == 0 {
				fmt.Fprintf(&b, "  (no indexed defimpl defines %s)\n", function)
			}
			h.writeDerives(&b, derives)
			return b.String(), nil
		}
		if len(defimpls) == 0 && len(derives) == 0 {
			fmt.Fprintf(&b, "No defimpl or @derive implementations found in the index.\n")
			return b.String(), nil
		}
		if len(defimpls) > 0 {
			fmt.Fprintf(&b, "\ndefimpl implementations (%d):\n", len(defimpls))
			for i, impl := range defimpls {
				if i == maxProtocolImpls {
					fmt.Fprintf(&b, "  ... and %d more\n", len(defimpls)-maxProtocolImpls)
					break
				}
				fmt.Fprintf(&b, "  %s:%d\n", h.relPath(impl.FilePath), impl.Line)
			}
			fmt.Fprintf(&b, "Note: the defimpl target type is on the cited line (defimpl %s, for: Type).\n", module)
		}
		h.writeDerives(&b, derives)
		return b.String(), nil
	}

	// Behaviour: modules that declare @behaviour or `use` this module.
	implementors, err := h.store.LookupBehaviourImplementors(module)
	if err != nil {
		return "", fmt.Errorf("looking up implementors: %w", err)
	}
	if len(implementors) == 0 {
		if len(modResults) == 0 {
			return fmt.Sprintf("Module %s is not in the index. Use dexter_search to find the right name.", module), nil
		}
		return fmt.Sprintf("No modules declare @behaviour %s (or use it) in the index.", module), nil
	}

	var b strings.Builder

	if args.Function != "" {
		// Locate the callback's implementation in each implementor.
		function := strings.TrimSpace(args.Function)
		cbs, err := h.store.LookupCallbackDef(module, function)
		if err != nil {
			return "", fmt.Errorf("looking up callback: %w", err)
		}
		if len(cbs) == 0 {
			return fmt.Sprintf("%s does not define a @callback named %s. List its callbacks with dexter_module_api.", module, function), nil
		}
		fmt.Fprintf(&b, "Implementations of callback %s.%s:\n", module, function)
		arities := make(map[int]bool, len(cbs))
		for _, cb := range cbs {
			arities[cb.Arity] = true
		}
		found := 0
		for _, impl := range implementors {
			defs, err := h.store.LookupFunction(impl.Module, function)
			if err != nil {
				continue
			}
			for _, d := range defs {
				if !arities[d.Arity] {
					continue
				}
				fmt.Fprintf(&b, "  %s - %s:%d\n", symbolName(impl.Module, function, d.Arity), h.relPath(d.FilePath), d.Line)
				found++
			}
		}
		if found == 0 {
			fmt.Fprintf(&b, "  (none of the %d implementor(s) define %s; they may rely on a default implementation injected via use)\n", len(implementors), function)
		}
		return b.String(), nil
	}

	fmt.Fprintf(&b, "Modules implementing behaviour %s (%d):\n", module, len(implementors))
	const maxImpls = 50
	for i, impl := range implementors {
		if i == maxImpls {
			fmt.Fprintf(&b, "  ... and %d more\n", len(implementors)-maxImpls)
			break
		}
		fmt.Fprintf(&b, "  %s - %s\n", impl.Module, h.relPath(impl.FilePath))
	}
	if cbs, err := h.store.ListModuleCallbacks(module); err == nil && len(cbs) > 0 {
		fmt.Fprintf(&b, "\nCallbacks defined by %s:\n", module)
		for _, cb := range cbs {
			fmt.Fprintf(&b, "  @%s %s/%d\n", cb.Kind, cb.Function, cb.Arity)
		}
	}
	return b.String(), nil
}

// writeDerives lists the modules that derive a protocol. The protocol
// generates their implementation, so it has no source in the module; the line
// cited is the @derive, or the `use` that brings one in.
func (h *Handler) writeDerives(b *strings.Builder, derives []lsp.ProtocolImpl) {
	if len(derives) == 0 {
		return
	}
	fmt.Fprintf(b, "\nDerived with @derive (%d; the protocol generates these implementations, through its __deriving__ macro or its Any implementation):\n", len(derives))
	for i, d := range derives {
		if i == maxProtocolImpls {
			fmt.Fprintf(b, "  ... and %d more\n", len(derives)-maxProtocolImpls)
			break
		}
		unsure := ""
		switch {
		case d.Conditional:
			unsure = " (unsure: the @derive is in a branch of the __using__ body, which the options of the use decide)"
		case d.Unsure:
			unsure = " (unsure: no struct found in its source and no compiled module; Elixir ignores a @derive with no struct after it)"
		}
		if d.Via != "" {
			fmt.Fprintf(b, "  %s - %s:%d (`use %s`, whose __using__ has the @derive)%s\n", d.Module, h.relPath(d.FilePath), d.Line, d.Via, unsure)
			continue
		}
		fmt.Fprintf(b, "  %s - %s:%d%s\n", d.Module, h.relPath(d.FilePath), d.Line, unsure)
	}
}
