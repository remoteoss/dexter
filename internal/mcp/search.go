package mcp

import (
	"context"
	"fmt"
	"strings"
)

type SearchParams struct {
	Query         string `json:"query" jsonschema:"fuzzy symbol query, e.g. 'Accounts.fetch' or 'fetch_user'"`
	IncludeStdlib bool   `json:"include_stdlib,omitempty" jsonschema:"also match Elixir stdlib symbols (default false)"`
}

func (h *Handler) search(ctx context.Context, args SearchParams) (string, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "", fmt.Errorf("query must not be empty")
	}

	var exclude []string
	if stdlibRoot := h.lsp.StdlibRoot(); !args.IncludeStdlib && stdlibRoot != "" {
		exclude = append(exclude, stdlibRoot)
	}
	results, err := h.store.SearchSymbols(query, exclude...)
	if err != nil {
		return "", fmt.Errorf("searching symbols: %w", err)
	}
	if len(results) == 0 {
		return fmt.Sprintf("No symbols matched %q. Try a shorter or less specific query; matching is fuzzy on module and function names.", query), nil
	}

	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%s (%s) - %s:%d\n", symbolName(r.Module, r.Function, r.Arity), r.Kind, h.relPath(r.FilePath), r.Line)
	}
	return b.String(), nil
}
