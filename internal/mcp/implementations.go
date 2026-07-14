package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ImplementationsParams struct {
	Module   string `json:"module" jsonschema:"behaviour or protocol module, fully qualified"`
	Function string `json:"function,omitempty" jsonschema:"callback name; when set, locate its definition in each implementor"`
}

func (h *Handler) implementationsHandler(ctx context.Context, req *mcp.CallToolRequest, args ImplementationsParams) (*mcp.CallToolResult, any, error) {
	module := strings.TrimSpace(args.Module)
	if module == "" {
		return nil, nil, fmt.Errorf("module must not be empty")
	}

	modResults, err := h.store.LookupModule(module)
	if err != nil {
		return nil, nil, fmt.Errorf("looking up module: %w", err)
	}

	// Protocol: implementations are the defimpl rows indexed under the protocol name.
	isProtocol := false
	var impls, decls []int
	for i, r := range modResults {
		switch r.Kind {
		case "defprotocol":
			isProtocol = true
			decls = append(decls, i)
		case "defimpl":
			impls = append(impls, i)
		}
	}
	if isProtocol {
		var b strings.Builder
		fmt.Fprintf(&b, "%s is a protocol (defprotocol at %s:%d).\n", module, h.relPath(modResults[decls[0]].FilePath), modResults[decls[0]].Line)
		if len(impls) == 0 {
			fmt.Fprintf(&b, "No defimpl implementations found in the index.\n")
			return textResult(b.String()), nil, nil
		}
		fmt.Fprintf(&b, "\nImplementations (%d):\n", len(impls))
		for _, i := range impls {
			r := modResults[i]
			fmt.Fprintf(&b, "  %s:%d\n", h.relPath(r.FilePath), r.Line)
		}
		fmt.Fprintf(&b, "\nNote: the defimpl target type is on the cited line (defimpl %s, for: Type).\n", module)
		return textResult(b.String()), nil, nil
	}

	// Behaviour: modules that declare @behaviour or `use` this module.
	implementors, err := h.store.LookupBehaviourImplementors(module)
	if err != nil {
		return nil, nil, fmt.Errorf("looking up implementors: %w", err)
	}
	if len(implementors) == 0 {
		if len(modResults) == 0 {
			return textResult(fmt.Sprintf("Module %s is not in the index. Use dexter_search to find the right name.", module)), nil, nil
		}
		return textResult(fmt.Sprintf("No modules declare @behaviour %s (or use it) in the index.", module)), nil, nil
	}

	var b strings.Builder

	if args.Function != "" {
		// Locate the callback's implementation in each implementor.
		function := strings.TrimSpace(args.Function)
		cbs, err := h.store.LookupCallbackDef(module, function)
		if err != nil {
			return nil, nil, fmt.Errorf("looking up callback: %w", err)
		}
		if len(cbs) == 0 {
			return textResult(fmt.Sprintf("%s does not define a @callback named %s. List its callbacks with dexter_module_api.", module, function)), nil, nil
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
		return textResult(b.String()), nil, nil
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
	return textResult(b.String()), nil, nil
}
