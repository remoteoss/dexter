package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/remoteoss/dexter/internal/version"
)

type WorkspaceParams struct{}

func (h *Handler) workspace(ctx context.Context, args WorkspaceParams) (string, error) {
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

	st, err := h.rt.IndexStatus()
	if err != nil {
		return "", fmt.Errorf("reading index status: %w", err)
	}
	if st.StdlibRoot != "" {
		fmt.Fprintf(&b, "\nElixir stdlib: %s (indexed; stdlib symbols resolve in lookups)\n", st.StdlibRoot)
	} else {
		fmt.Fprintf(&b, "\nElixir stdlib: not detected. Set DEXTER_ELIXIR_LIB_ROOT to enable stdlib lookups.\n")
	}

	fmt.Fprintf(&b, "\nIndex: %d files, %d definitions, %d references\n", st.Files, st.Definitions, st.References)
	if st.Ready {
		fmt.Fprintf(&b, "Index state: ready\n")
	} else {
		fmt.Fprintf(&b, "Index state: still building; answers can be incomplete until it is ready\n")
	}
	if st.IndexVersion != st.ExpectedIndexVersion && st.Ready {
		fmt.Fprintf(&b, "WARNING: index version %d does not match this binary (%d). Run `dexter stop --force` in the project so the next call starts a current daemon (a plain stop is refused while this MCP session is attached).\n", st.IndexVersion, st.ExpectedIndexVersion)
	}
	if st.Watching {
		fmt.Fprintf(&b, "\nThe index updates automatically as files change and on git branch switches; dexter_reindex forces an immediate update.\n")
	} else {
		fmt.Fprintf(&b, "\nFile watching is not active, so the index updates only on git branch switches, editor saves, and dexter_reindex.\n")
	}

	if conditions := h.rt.Reporter().Conditions(); len(conditions) > 0 {
		fmt.Fprintf(&b, "\nWorkspace conditions:\n")
		for _, c := range conditions {
			fmt.Fprintf(&b, "  %s: %s\n", c.Severity, strings.TrimPrefix(c.Message, "Dexter: "))
		}
	}
	return b.String(), nil
}

// findMixProjects lists mix.exs locations relative to root: the root itself,
// umbrella apps under apps/, and direct children with their own mix.exs.
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
