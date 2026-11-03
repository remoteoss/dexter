package mcp

import (
	"context"
	"fmt"
	"strings"
)

type SearchParams struct {
	Query         string `json:"query" jsonschema:"fuzzy symbol query, e.g. 'Accounts.fetch' or 'fetch_user'; a space works as a dot"`
	IncludeStdlib bool   `json:"include_stdlib,omitempty" jsonschema:"also match Elixir stdlib symbols (default false)"`
}

// maxSearchResults is how many index rows one search answer shows.
const maxSearchResults = 50

func (h *Handler) search(ctx context.Context, args SearchParams) (string, error) {
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "", fmt.Errorf("query must not be empty")
	}
	found, err := h.lsp.SearchNames(query, args.IncludeStdlib, maxSearchResults)
	if err != nil {
		return "", fmt.Errorf("searching symbols: %w", err)
	}
	if len(found.Results) == 0 {
		return fmt.Sprintf("No symbols matched %q. Matching is fuzzy on module and function names, not file names; a space works as a dot (Module function). Try a shorter query.", query), nil
	}

	var b strings.Builder
	if found.Query != strings.Join(strings.Fields(query), ".") {
		fmt.Fprintf(&b, "No symbols matched %q; these match %q:\n", query, found.Query)
	}
	for _, r := range found.Results {
		if r.Member != nil {
			fmt.Fprintf(&b, "%s (%s, %s) - %s:%d\n", symbolName(r.Module, r.Function, r.Arity), r.Kind, h.memberOrigin(r.Module, *r.Member), h.relPath(r.FilePath), r.Line)
			continue
		}
		clauses := ""
		if r.Clauses > 1 {
			clauses = fmt.Sprintf(", %d clauses", r.Clauses)
		}
		fmt.Fprintf(&b, "%s (%s%s) - %s:%d\n", symbolName(r.Module, r.Function, r.Arity), r.Kind, clauses, h.relPath(r.FilePath), r.Line)
	}
	if found.More {
		fmt.Fprintf(&b, "\nThe first %d matches are shown; there are more. Use a longer query, for example Module.function.\n", maxSearchResults)
	}
	return b.String(), nil
}
