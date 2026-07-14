package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/version"
)

type WorkspaceParams struct{}

func (h *Handler) workspaceHandler(ctx context.Context, req *mcp.CallToolRequest, args WorkspaceParams) (*mcp.CallToolResult, any, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Dexter %s\n", version.Version)
	fmt.Fprintf(&b, "Project root: %s\n", h.projectRoot)

	if projects := findMixProjects(h.projectRoot); len(projects) > 0 {
		fmt.Fprintf(&b, "\nMix projects:\n")
		for _, p := range projects {
			fmt.Fprintf(&b, "  %s\n", p)
		}
	} else {
		fmt.Fprintf(&b, "\nNo mix.exs found at the project root. The index may cover a plain directory of Elixir files.\n")
	}

	if stdlibRoot := h.stdlibRoot(); stdlibRoot != "" {
		fmt.Fprintf(&b, "\nElixir stdlib: %s (indexed; stdlib symbols resolve in lookups)\n", stdlibRoot)
	} else {
		fmt.Fprintf(&b, "\nElixir stdlib: not detected. Set DEXTER_ELIXIR_LIB_ROOT to enable stdlib lookups.\n")
	}

	st, err := h.store.Stats()
	if err != nil {
		return nil, nil, fmt.Errorf("reading index stats: %w", err)
	}
	fmt.Fprintf(&b, "\nIndex: %d files, %d definitions, %d references\n", st.Files, st.Definitions, st.References)

	if stored := h.store.GetIndexVersion(); stored != version.IndexVersion {
		fmt.Fprintf(&b, "WARNING: index version %d does not match this binary (%d). Restart dexter mcp to rebuild.\n", stored, version.IndexVersion)
	}
	fmt.Fprintf(&b, "\nThe index updates automatically on git branch switches. After you edit, create, or delete Elixir files, call dexter_reindex before trusting references or renames.\n")

	return textResult(b.String()), nil, nil
}

// findMixProjects lists mix.exs locations relative to root: the root itself,
// umbrella apps under apps/, and direct children (common monorepo layout).
// The scan is deliberately shallow; no full tree walk.
func findMixProjects(root string) []string {
	var projects []string
	if _, err := os.Stat(filepath.Join(root, "mix.exs")); err == nil {
		projects = append(projects, "mix.exs")
	}
	for _, pattern := range []string{"apps/*/mix.exs", "*/mix.exs"} {
		matches, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, m := range matches {
			if rel, err := filepath.Rel(root, m); err == nil && rel != "mix.exs" {
				projects = append(projects, rel)
			}
		}
	}
	sort.Strings(projects)
	return dedupeStrings(projects)
}

func dedupeStrings(in []string) []string {
	out := in[:0]
	var prev string
	for i, s := range in {
		if i == 0 || s != prev {
			out = append(out, s)
		}
		prev = s
	}
	return out
}
