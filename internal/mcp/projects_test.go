package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

// containerDir lays out a folder that holds other repositories: a clone of a
// monorepo and two worktrees of it. The folder is a repository itself, with
// no Mix project of its own.
func containerDir(t *testing.T) (root, clone, featureA, featureB string) {
	t.Helper()
	root = t.TempDir()
	clone = filepath.Join(root, "shop")
	featureA = filepath.Join(root, "shop-worktrees", "feature-a")
	featureB = filepath.Join(root, "shop-worktrees", "feature-b")
	for _, d := range []string{filepath.Join(root, ".git"), filepath.Join(clone, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{clone, featureA, featureB} {
		writeSource(t, d, "apps/shop/mix.exs", "defmodule Shop.MixProject do\nend\n")
		writeSource(t, d, "apps/shop/lib/shop.ex", "defmodule Shop do\nend\n")
	}
	for _, d := range []string{featureA, featureB} {
		writeSource(t, d, ".git", "gitdir: "+filepath.Join(clone, ".git", "worktrees", filepath.Base(d))+"\n")
	}
	return root, clone, featureA, featureB
}

func markIndexed(t *testing.T, project string) {
	t.Helper()
	writeSource(t, project, ".dexter/dexter.db", "")
}

// A session started in a folder of repositories connects to no daemon for
// the folder, and a call without a project lists the projects.
func TestContainerSession_NoBackendAndListsProjects(t *testing.T) {
	e := setupNegotiation(t)
	root, _, _, _ := containerDir(t)
	cs, _ := e.connect(nil, fileURI(root))

	out, ok := toolText(t, cs, "dexter_definition", map[string]any{"module": "Shop"})
	if ok {
		t.Fatalf("a call without a project succeeded in a container session: %s", out)
	}
	wantContains(t, out, "holds 3 projects")
	wantContains(t, out, "Pass project")
	wantContains(t, out, filepath.Join("shop-worktrees", "feature-a"))
	if n := e.connCount(); n != 0 {
		t.Errorf("%d daemon connections for a container session, want 0", n)
	}

	// The overview lists the projects instead of failing.
	overview := mustTool(t, cs, "dexter_workspace", nil)
	wantContains(t, overview, "holds 3 Elixir projects")
}

// A call names its project with any file or directory in it, absolute or
// relative to the session's root.
func TestContainerSession_ProjectArgumentRoutesTheCall(t *testing.T) {
	e := setupNegotiation(t)
	root, clone, featureA, featureB := containerDir(t)
	cs, _ := e.connect(nil, fileURI(root))

	wantContains(t, mustTool(t, cs, "dexter_definition", map[string]any{"module": "Shop", "project": featureA}), "root="+featureA)
	wantContains(t, mustTool(t, cs, "dexter_definition", map[string]any{"module": "Shop", "project": "shop-worktrees/feature-b/apps/shop/lib/shop.ex"}), "root="+featureB)
	wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "Shop", "project": "shop"}), "root="+clone)
	// The file of dexter_file_outline names its project.
	wantContains(t, mustTool(t, cs, "dexter_file_outline", map[string]any{"file": filepath.Join(featureB, "apps/shop/lib/shop.ex")}), "root="+featureB)

	if n := e.connCount(); n != 3 {
		t.Errorf("%d daemon connections, want 3 (one for each project)", n)
	}
	// The same project again shares its connection.
	mustTool(t, cs, "dexter_references", map[string]any{"module": "Shop", "project": featureA})
	if got := len(e.backendsFor(featureA)); got != 1 {
		t.Errorf("%d connections for feature-a, want 1", got)
	}

	// The session's end releases every project connection.
	_ = cs.Close()
	eventually(t, "the project connections close", func() bool { return e.connCount() == 0 })
}

// When one project in the folder has an index, a call without a project
// answers from it and says so.
func TestContainerSession_OnlyIndexedProjectAnswers(t *testing.T) {
	e := setupNegotiation(t)
	root, _, featureA, _ := containerDir(t)
	markIndexed(t, featureA)
	cs, _ := e.connect(nil, fileURI(root))

	out := mustTool(t, cs, "dexter_definition", map[string]any{"module": "Shop"})
	wantContains(t, out, "root="+featureA)
	wantContains(t, out, "the only one with a Dexter index")
}

// A monorepo with a worktree inside it is one project: no change.
func TestProjectSession_MonorepoWithNestedWorktreeIsUnchanged(t *testing.T) {
	e := setupNegotiation(t)
	root, uri := projectDir(t)
	writeSource(t, root, "apps/shop/mix.exs", "defmodule Shop.MixProject do\nend\n")
	nested := filepath.Join(root, ".agents", "worktrees", "a1")
	writeSource(t, nested, ".git", "gitdir: "+filepath.Join(root, ".git", "worktrees", "a1")+"\n")
	writeSource(t, nested, "apps/shop/mix.exs", "defmodule Shop.MixProject do\nend\n")
	cs, _ := e.connect(nil, uri)

	out := mustTool(t, cs, "dexter_definition", map[string]any{"module": "Shop"})
	wantContains(t, out, "root="+root)
	// The nested worktree is its own project when a call names it.
	wantContains(t, mustTool(t, cs, "dexter_definition", map[string]any{"module": "Shop", "project": nested}), "root="+nested)
}

// An index left in the folder of projects, or in a sub-project of a
// monorepo, does not make that folder the project of a path: the nearest
// clone or worktree is.
func TestContainerSession_LeftoverIndexDoesNotCaptureAProject(t *testing.T) {
	e := setupNegotiation(t)
	root, clone, _, _ := containerDir(t)
	markIndexed(t, root)
	markIndexed(t, filepath.Join(clone, "apps", "shop"))
	cs, _ := e.connect(nil, fileURI(root))

	wantContains(t, mustTool(t, cs, "dexter_definition", map[string]any{"module": "Shop", "project": filepath.Join(clone, "apps/shop/lib/shop.ex")}), "root="+clone)
}
