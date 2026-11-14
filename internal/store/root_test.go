package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRootTree makes a tree for the project root rule. Besides the forms of
// writeTree, "<dir>/.git@" makes <dir> a linked worktree of the clone at the
// root of the tree: a .git file that names <root>/.git/worktrees/<name>.
func writeRootTree(t *testing.T, root string, paths ...string) {
	t.Helper()
	var plain []string
	for _, p := range paths {
		if !strings.HasSuffix(p, ".git@") {
			plain = append(plain, p)
			continue
		}
		dir := filepath.Join(root, strings.TrimSuffix(p, "/.git@"))
		admin := filepath.Join(root, ".git", "worktrees", filepath.Base(dir))
		if err := os.MkdirAll(admin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeTree(t, root, plain...)
}

func TestResolveRoot(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		from  string
		want  string
		kind  RootKind
	}{
		{"a Mix project", []string{".git/", "mix.exs", "lib/a.ex"}, "lib", ".", OneProject},
		{"an umbrella app anchors on the repository", []string{".git/", "mix.exs", "apps/shop/mix.exs", "apps/shop/lib/a.ex"}, "apps/shop/lib", ".", OneProject},
		{"a monorepo without a root mix.exs", []string{".git/", "apps/shop/mix.exs", "libs/util/mix.exs", "apps/shop/lib/a.ex"}, "apps/shop/lib", ".", OneProject},
		{"a monorepo with a worktree inside it", []string{".git/", "apps/shop/mix.exs", ".agents/wt/.git@", ".agents/wt/apps/shop/mix.exs", ".agents/wt/apps/shop/lib/a.ex"}, ".agents/wt/apps/shop/lib", ".agents/wt", OneProject},
		{"a clone in a container", []string{".git/", "shop/.git/", "shop/mix.exs", "shop/lib/a.ex"}, "shop/lib", "shop", OneProject},
		{"a worktree in a container", []string{".git/", "shop/.git/", "shop/mix.exs", "trees/feature/.git@", "trees/feature/mix.exs", "trees/feature/lib/a.ex"}, "trees/feature/lib", "trees/feature", OneProject},
		{"the container itself", []string{".git/", "README.md", "shop/.git/", "shop/mix.exs", "web/.git/", "web/mix.exs"}, ".", ".", Container},
		{"a folder of the container outside its repositories", []string{".git/", "shop/.git/", "shop/mix.exs", "notes/today/x.md"}, "notes/today", ".", Container},
		{"a container that is not in git", []string{"shop/.git/", "shop/mix.exs", "shop/lib/a.ex"}, "shop/lib", "shop", OneProject},
		{"an old index above a clone is not used", []string{".git/", ".dexter/dexter.db", "shop/.git/", "shop/mix.exs", "shop/lib/a.ex"}, "shop/lib", "shop", OneProject},
		{"an old index in a folder without a project is not used", []string{".dexter/dexter.db", "shop/.git/", "shop/mix.exs", "shop/lib/a.ex"}, "shop/lib", "shop", OneProject},
		{"an index that a user made in a Mix app is used", []string{".git/", "apps/shop/mix.exs", "apps/shop/.dexter/dexter.db", "apps/shop/lib/a.ex"}, "apps/shop/lib", "apps/shop", OneProject},
		{"the nearest index in a Mix project wins", []string{".git/", "mix.exs", ".dexter/dexter.db", "apps/shop/mix.exs", "apps/shop/.dexter/dexter.db", "apps/shop/lib/a.ex"}, "apps/shop/lib", "apps/shop", OneProject},
		{"another Mix app without an index uses the repository", []string{".git/", "apps/shop/mix.exs", "apps/shop/.dexter/dexter.db", "apps/web/mix.exs", "apps/web/lib/a.ex"}, "apps/web/lib", ".", OneProject},
		{"an index without a mix.exs in a monorepo is not used", []string{".git/", "apps/shop/mix.exs", "apps/shop/lib/.dexter/dexter.db", "apps/shop/lib/a.ex"}, "apps/shop/lib", ".", OneProject},
		{"a submodule is a part of its repository", []string{".git/", "mix.exs", "vendor/lib/.git", "vendor/lib/a.ex"}, "vendor/lib", ".", OneProject},
		{"a git dependency belongs to its project", []string{".git/", "mix.exs", "deps/dep/.git/", "deps/dep/mix.exs", "deps/dep/lib/a.ex"}, "deps/dep/lib", ".", OneProject},
		{"build output belongs to its project", []string{"shop/.git/", "shop/mix.exs", "shop/_build/dev/lib/shop/ebin/x.beam"}, "shop/_build/dev/lib/shop/ebin", "shop", OneProject},
		{"an umbrella that is not in git", []string{"mix.exs", "apps/shop/mix.exs", "apps/shop/lib/a.ex"}, "apps/shop/lib", ".", OneProject},
		{"a .git file outside a clone", []string{".git", "lib/a.ex"}, "lib", ".", OneProject},
		{"a repository without Elixir", []string{".git/", "cmd/main.go"}, "cmd", ".", OneProject},
		{"no project", []string{"lib/a.ex"}, "lib", "lib", NotAProject},
		{"an index alone is not a project", []string{".dexter/dexter.db", "lib/a.ex"}, "lib", "lib", NotAProject},
		{"a file resolves as its directory", []string{".git/", "mix.exs", "lib/a.ex"}, "lib/a.ex", ".", OneProject},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeRootTree(t, root, c.paths...)
			got := ResolveRoot(filepath.Join(root, c.from))
			want := filepath.Join(root, c.want)
			if got.Dir != want || got.Kind != c.kind {
				rel, _ := filepath.Rel(root, got.Dir)
				t.Errorf("ResolveRoot = %s (kind %d), want %s (kind %d)", rel, got.Kind, c.want, c.kind)
			}
		})
	}
}

func TestResolveRootStopsAtTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRootTree(t, home, ".git/", "scratch/lib/a.ex", "code/shop/mix.exs", "code/shop/lib/a.ex")

	// A dotfiles repository in the home directory is not a project.
	if got := ResolveRoot(filepath.Join(home, "scratch", "lib")); got.Kind != NotAProject {
		t.Errorf("a folder below a home with .git resolves to %s (kind %d), want no project", got.Dir, got.Kind)
	}
	// The mix.exs below home is the project.
	if got := ResolveRoot(filepath.Join(home, "code", "shop", "lib")); got.Dir != filepath.Join(home, "code", "shop") {
		t.Errorf("ResolveRoot = %s, want code/shop", got.Dir)
	}
	if NonProjectRootError(home) == nil {
		t.Error("the home directory is accepted as a workspace")
	}
}

func TestNestedProjectAgreesWithResolveRoot(t *testing.T) {
	root := t.TempDir()
	writeRootTree(t, root,
		".git/", "README.md",
		"shop/.git/", "shop/mix.exs", "shop/lib/a.ex", "shop/deps/dep/.git/", "shop/deps/dep/lib/b.ex",
		"trees/feature/.git@", "trees/feature/mix.exs", "trees/feature/lib/a.ex",
		"mono/.git/", "mono/apps/x/mix.exs", "mono/apps/x/lib/a.ex", "mono/vendor/sub/.git", "mono/vendor/sub/c.ex",
	)
	for _, dir := range []string{"shop/lib", "shop/deps/dep/lib", "trees/feature/lib", "mono/apps/x/lib", "mono/vendor/sub"} {
		full := filepath.Join(root, dir)
		nested := NestedProject(full, root)
		resolved := ResolveRoot(full).Dir
		if nested != resolved {
			t.Errorf("%s: NestedProject = %s, ResolveRoot = %s", dir, nested, resolved)
		}
	}
}

func TestStrayIndexesAbove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRootTree(t, home, ".dexter/dexter.db", "code/.dexter/dexter.db", "code/ws/.git/", "code/ws/.dexter/dexter.db", "code/ws/shop/.git/", "code/ws/shop/mix.exs", "code/ws/shop/.dexter/dexter.db")
	if err := os.WriteFile(DBPath(filepath.Join(home, "code", "ws"))+"-wal", make([]byte, 3<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	stray := StrayIndexesAbove(filepath.Join(home, "code", "ws", "shop"))
	var dirs []string
	for _, s := range stray {
		rel, _ := filepath.Rel(home, s.Dir)
		dirs = append(dirs, rel)
	}
	if got, want := strings.Join(dirs, ","), "code/ws,code,."; got != want {
		t.Fatalf("stray indexes = %s, want %s", got, want)
	}
	if stray[0].Bytes < 3<<20 {
		t.Errorf("the size of %s leaves out its journal: %d bytes", stray[0].Dir, stray[0].Bytes)
	}
	msg := StrayIndexMessage(stray)
	for _, want := range []string{filepath.Join(home, "code", "ws", ".dexter") + " (3 MB)", "No session uses them", "dexter prune --delete"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not say %q", msg, want)
		}
	}
	if StrayIndexMessage(StrayIndexesAbove(filepath.Join(home, "code", "ws", "shop", "lib"))) == "" {
		t.Error("no message for a directory below the project")
	}
	if StrayIndexMessage(nil) != "" {
		t.Error("a message with no stray index")
	}
}

func TestContainerRootError(t *testing.T) {
	root := t.TempDir()
	writeRootTree(t, root, ".git/", "shop/.git/", "shop/mix.exs", "web/.git/", "web/mix.exs")
	err := ContainerRootError(root)
	if err == nil || !strings.Contains(err.Error(), "holds 2 projects") || !strings.Contains(err.Error(), "for example shop") {
		t.Errorf("ContainerRootError = %v", err)
	}
	mono := t.TempDir()
	writeRootTree(t, mono, ".git/", "apps/x/mix.exs")
	if err := ContainerRootError(mono); err != nil {
		t.Errorf("a monorepo is refused: %v", err)
	}
}

// The home directory under another spelling (HOME is a symlink, or the path
// uses other letter case) is still the home directory.
func TestResolveRootKnowsTheHomeDirectoryInAnotherSpelling(t *testing.T) {
	real := t.TempDir()
	writeRootTree(t, real, ".git/", "scratch/lib/a.ex", ".dexter/dexter.db")
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", link)
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got := ResolveRoot(filepath.Join(resolved, "scratch", "lib")); got.Kind != NotAProject {
		t.Errorf("a folder below the resolved home resolves to %s (kind %d), want no project", got.Dir, got.Kind)
	}
	upper := strings.ToUpper(resolved[:2]) + resolved[2:]
	if upper != resolved && SameDir(upper, resolved) { // a file system without case
		if got := ResolveRoot(filepath.Join(upper, "scratch", "lib")); got.Kind != NotAProject {
			t.Errorf("a folder below the home in other letter case resolves to %s (kind %d), want no project", got.Dir, got.Kind)
		}
	}
	// The index in the home directory is listed once, in the resolved spelling.
	if stray := StrayIndexesAbove(filepath.Join(resolved, "scratch")); len(stray) != 1 {
		t.Errorf("stray indexes = %v, want the home index", stray)
	}
}

func TestRemoveIndexDeletesOnlyDexterFiles(t *testing.T) {
	dir := t.TempDir()
	writeRootTree(t, dir, ".dexter/dexter.db", ".dexter/dexter.db-wal", ".dexter/dexter.db-shm", ".dexter/.gitignore", "lib/a.ex")
	if err := RemoveIndex(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(DBDir(dir)); !os.IsNotExist(err) {
		t.Errorf(".dexter is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "lib", "a.ex")); err != nil {
		t.Errorf("a file outside .dexter is gone: %v", err)
	}

	// A file that Dexter did not make stays, and so does its directory.
	other := t.TempDir()
	writeRootTree(t, other, ".dexter/dexter.db", ".dexter/notes.txt")
	err := RemoveIndex(other)
	var otherFiles *OtherFilesError
	if !errors.As(err, &otherFiles) || len(otherFiles.Names) != 1 || otherFiles.Names[0] != "notes.txt" {
		t.Errorf("RemoveIndex = %v, want an OtherFilesError that names notes.txt", err)
	}
	if regularFile(DBPath(other)) {
		t.Error("the database is still there")
	}
	if !regularFile(filepath.Join(DBDir(other), "notes.txt")) {
		t.Error("a file that Dexter did not make was deleted")
	}

	// No index: nothing to do.
	if err := RemoveIndex(t.TempDir()); err != nil {
		t.Errorf("RemoveIndex of a directory without an index = %v", err)
	}
}

// An index above the project that a session started in its own directory
// uses (a monorepo above one of its apps, or above a clone in it) is not
// stray. The index of a container, or of a folder that is not a project, is.
func TestStrayIndexesAboveKeepsIndexesInUse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeRootTree(t, home,
		"code/.dexter/dexter.db",
		"code/ws/.git/", "code/ws/.dexter/dexter.db",
		"code/ws/mono/.git/", "code/ws/mono/.dexter/dexter.db",
		"code/ws/mono/apps/shop/mix.exs", "code/ws/mono/apps/shop/.dexter/dexter.db",
		"code/ws/mono/tools/clone/.git/", "code/ws/mono/tools/clone/mix.exs",
	)
	for _, from := range []string{"code/ws/mono/apps/shop", "code/ws/mono/tools/clone"} {
		var dirs []string
		for _, s := range StrayIndexesAbove(filepath.Join(home, from)) {
			rel, _ := filepath.Rel(home, s.Dir)
			dirs = append(dirs, rel)
		}
		if got, want := strings.Join(dirs, ","), "code/ws,code"; got != want {
			t.Errorf("stray indexes above %s = %s, want %s (not the monorepo's)", from, got, want)
		}
	}
}
