package mcp

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/parser"
)

type FileOutlineParams struct {
	File string `json:"file" jsonschema:"path to a .ex/.exs file, absolute or relative to the project root"`
}

func (h *Handler) fileOutlineHandler(ctx context.Context, req *mcp.CallToolRequest, args FileOutlineParams) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(args.File) == "" {
		return nil, nil, fmt.Errorf("file must not be empty")
	}
	path := h.resolvePath(args.File)
	if _, err := os.Stat(path); err != nil {
		return textResult(fmt.Sprintf("File not found: %s", h.relPath(path))), nil, nil
	}

	// Parse fresh from disk so the outline is correct even when the index is stale.
	defs, _, err := parser.ParseFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", h.relPath(path), err)
	}
	if len(defs) == 0 {
		return textResult(fmt.Sprintf("%s defines no modules or functions.", h.relPath(path))), nil, nil
	}

	// Split into module declarations (in line order) and their members.
	type moduleEntry struct {
		def     parser.Definition
		members []parser.Definition
	}
	var modules []*moduleEntry
	byName := make(map[string]*moduleEntry)
	var orphans []parser.Definition

	sorted := make([]parser.Definition, len(defs))
	copy(sorted, defs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Line < sorted[j].Line })

	for _, d := range sorted {
		if d.Function == "" {
			e := &moduleEntry{def: d}
			modules = append(modules, e)
			byName[d.Module] = e
		}
	}
	for _, d := range sorted {
		if d.Function == "" {
			continue
		}
		if e, ok := byName[d.Module]; ok {
			e.members = append(e.members, d)
		} else {
			orphans = append(orphans, d)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", h.relPath(path))
	for _, e := range modules {
		fmt.Fprintf(&b, "\n%s %s (line %d)\n", moduleKindLabel(e.def.Kind), e.def.Module, e.def.Line)
		for _, m := range e.members {
			b.WriteString("  " + memberLine(m) + "\n")
		}
	}
	for _, m := range orphans {
		b.WriteString(memberLine(m) + "\n")
	}
	return textResult(b.String()), nil, nil
}

func moduleKindLabel(kind string) string {
	switch kind {
	case "module":
		return "defmodule"
	default: // defprotocol, defimpl
		return kind
	}
}

func memberLine(d parser.Definition) string {
	label := d.Kind
	switch d.Kind {
	case "type", "opaque", "callback", "macrocallback":
		label = "@" + d.Kind
	}
	line := fmt.Sprintf("%4d: %s %s/%d", d.Line, label, d.Function, d.Arity)
	if d.Params != "" {
		line += fmt.Sprintf(" (%s)", d.Params)
	}
	if d.DelegateTo != "" {
		target := d.DelegateTo
		if d.DelegateAs != "" {
			target += "." + d.DelegateAs
		}
		line += " → " + target
	}
	return line
}
