package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
)

// TestReferences_MatchRenameSites checks that references and rename agree:
// for each function of the rename fixture, the lines that find-references
// lists (with the defdelegate facades, as the editor and MCP ask) are the
// lines that a rename of the function changes. Only a rename changes the
// declarations (the def heads, @spec, @callback, defoverridable, and except:
// keys); only references list the calls of a facade, which keeps its name.
// A rename reads the files and the compiler checks it
// (TestRename_CompilesAfterEachRename); references come from the index,
// which must record the same sites. A callback family is left out: its
// rename changes every member.
func TestReferences_MatchRenameSites(t *testing.T) {
	type fn struct{ module, name string }
	var fns []fn
	seen := map[fn]bool{}
	for _, p := range elixirFiles(t, filepath.Join("testdata", "rename_compile")) {
		defs, _, err := parser.ParseFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range defs {
			switch d.Kind {
			case "def", "defp", "defmacro", "defmacrop":
				if f := (fn{d.Module, d.Function}); !seen[f] && !strings.HasPrefix(d.Function, "__") {
					seen[f] = true
					fns = append(fns, f)
				}
			}
		}
	}
	sort.Slice(fns, func(i, j int) bool {
		return fns[i].module+"."+fns[i].name < fns[j].module+"."+fns[j].name
	})

	for _, f := range fns {
		// Each rename runs on a fresh copy: it writes the files.
		dir := t.TempDir()
		copyTree(t, filepath.Join("testdata", "rename_compile"), dir)
		servers, st := sessionsAt(t, dir, 1)
		s := servers[0]
		before := map[string][]string{}
		for _, p := range elixirFiles(t, dir) {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			before[p] = strings.Split(string(data), "\n")
			defs, refs, err := parser.ParseFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.IndexFileWithRefs(p, defs, refs); err != nil {
				t.Fatal(err)
			}
		}
		if family, err := s.renameFamily(f.module, f.name); err != nil || family.behaviour != "" {
			continue
		}
		locs, err := s.ReferenceNames(f.module, f.name, NameReferenceOptions{FollowDelegates: true, ExcludeStdlib: true})
		if err != nil {
			t.Fatal(err)
		}
		facadeCall := func(string) bool { return false }
		delegates, _ := st.LookupDelegatesTo(f.module, f.name)
		delegates = append(delegates, s.injectedDelegatesTo(f.module, f.name)...)
		if len(delegates) > 0 {
			names := make([]string, 0, len(delegates))
			for _, d := range delegates {
				names = append(names, regexp.QuoteMeta(d.Function))
			}
			facadeCall = regexp.MustCompile(`(^|[^.\w])(` + strings.Join(names, "|") + `)(\(|/)`).MatchString
		}
		indexed, err := s.RenameFunctionHeadless(context.Background(), f.module, f.name, f.name+"_zz")
		if err != nil {
			// A refused rename (a callback family that Dexter cannot
			// change) has no sites to compare.
			continue
		}
		<-indexed
		renamed := map[string]string{}
		for p, lines := range before {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			after := strings.Split(string(data), "\n")
			for i, line := range lines {
				if i < len(after) && after[i] != line {
					renamed[fmt.Sprintf("%s:%d", p, i+1)] = line
				}
			}
		}
		listed := map[string]bool{}
		for _, l := range locs {
			listed[fmt.Sprintf("%s:%d", l.FilePath, l.Line)] = true
		}
		declaration := declarationLine(f.name)
		for site, text := range renamed {
			if !listed[site] && !declaration(text) {
				t.Errorf("%s.%s: a rename changes %s (%q), references do not list it", f.module, f.name, rel(dir, site), strings.TrimSpace(text))
			}
		}
		for site := range listed {
			if _, ok := renamed[site]; !ok && !facadeCall(siteText(t, site)) {
				t.Errorf("%s.%s: references list %s (%q), a rename does not change it", f.module, f.name, rel(dir, site), strings.TrimSpace(siteText(t, site)))
			}
		}
	}
}

// declarationLine reports whether a line declares name and does not use it.
func declarationLine(name string) func(string) bool {
	q := regexp.QuoteMeta(name)
	re := regexp.MustCompile(`^\s*(def|defp|defmacro|defmacrop|defguard|defguardp|defdelegate)\s+` + q + `\b|^\s*@(spec|callback|optional_callbacks)\b|^\s*defoverridable\b|except:|^\s*` + q + `:\s*\d`)
	return re.MatchString
}

func rel(dir, site string) string {
	if r, err := filepath.Rel(dir, site); err == nil {
		return r
	}
	return site
}

// siteText returns the text of a "path:line" site.
func siteText(t *testing.T, site string) string {
	t.Helper()
	i := strings.LastIndexByte(site, ':')
	data, err := os.ReadFile(site[:i])
	if err != nil {
		t.Fatal(err)
	}
	var n int
	_, _ = fmt.Sscanf(site[i+1:], "%d", &n)
	lines := strings.Split(string(data), "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}
