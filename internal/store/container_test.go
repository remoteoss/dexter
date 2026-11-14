package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree makes each path below root: a path that ends in "/" is a
// directory (a clone's .git/), any other path is a file (a worktree's .git).
func writeTree(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		path := filepath.Join(root, p)
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsProjectContainer(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  bool
	}{
		{"clones and worktrees of other repositories", []string{
			".git/", "README.md",
			"shop/.git/", "shop/apps/shop/mix.exs",
			"shop-worktrees/feature/.git@", "shop-worktrees/feature/apps/shop/mix.exs",
		}, true},
		{"a monorepo with Mix projects in apps and libs", []string{
			".git/", "apps/shop/mix.exs", "libs/util/mix.exs",
		}, false},
		{"a monorepo with an agent worktree inside it", []string{
			".git/", "apps/shop/mix.exs", ".agents/worktrees/a1/.git", ".agents/worktrees/a1/apps/shop/mix.exs",
		}, false},
		{"a Mix project at the root", []string{".git/", "mix.exs", "deps/dep/.git/", "deps/dep/mix.exs"}, false},
		{"nested repositories without Elixir", []string{".git/", "web/.git/", "web/package.json"}, false},
		{"a Mix project that is not in a repository of its own", []string{".git/", "tools/script/mix.exs", "shop/.git/", "shop/mix.exs"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			writeRootTree(t, root, c.paths...)
			if got := IsProjectContainer(root); got != c.want {
				t.Errorf("IsProjectContainer = %t, want %t", got, c.want)
			}
		})
	}
}

// A clone that is a symlink is a repository of the container too.
func TestIsProjectContainerWithASymlinkedClone(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	writeRootTree(t, root, ".git/", "README.md")
	writeRootTree(t, elsewhere, "shop/.git/", "shop/mix.exs")
	if err := os.Symlink(filepath.Join(elsewhere, "shop"), filepath.Join(root, "shop")); err != nil {
		t.Fatal(err)
	}
	// A link loop is not followed, and a link back into the container is
	// not a repository of it.
	if err := os.Symlink(root, filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	if scan := scanRepoUncached(root); len(scan.nested) != 1 {
		t.Errorf("nested repositories = %v, want only shop", scan.nested)
	}
	if !IsProjectContainer(root) {
		t.Error("a folder with a symlinked clone is not a container")
	}
	if projects, ok := ContainerProjects(root); !ok || len(projects) != 1 || projects[0] != filepath.Join(root, "shop") {
		t.Errorf("ContainerProjects = %v, %t", projects, ok)
	}
}
