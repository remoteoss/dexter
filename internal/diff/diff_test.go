package diff

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnified_Equal(t *testing.T) {
	if got := Unified("a.ex", "a.ex", "same\n", "same\n"); got != "" {
		t.Errorf("equal texts produced a diff:\n%s", got)
	}
}

func TestUnified_SimpleChange(t *testing.T) {
	oldText := "defmodule Foo do\n  def bar, do: :ok\nend\n"
	newText := "defmodule Foo do\n  def baz, do: :ok\nend\n"
	got := Unified("lib/foo.ex", "lib/foo.ex", oldText, newText)
	want := `--- a/lib/foo.ex
+++ b/lib/foo.ex
@@ -1,3 +1,3 @@
 defmodule Foo do
-  def bar, do: :ok
+  def baz, do: :ok
 end
`
	if got != want {
		t.Errorf("diff mismatch.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnified_EmptyToContent(t *testing.T) {
	got := Unified("a.ex", "a.ex", "", "hello\n")
	want := `--- a/a.ex
+++ b/a.ex
@@ -0,0 +1 @@
+hello
`
	if got != want {
		t.Errorf("diff mismatch.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUnified_RenameHeader(t *testing.T) {
	got := Unified("lib/old.ex", "lib/new.ex", "defmodule Old do\nend\n", "defmodule New do\nend\n")
	if !strings.HasPrefix(got, "--- a/lib/old.ex\n+++ b/lib/new.ex\n") {
		t.Errorf("missing rename header:\n%s", got)
	}
}

func TestUnified_DistantHunks(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 0; i < 30; i++ {
		line := "line\n"
		oldB.WriteString(line)
		newB.WriteString(line)
	}
	oldText := "first_old\n" + oldB.String() + "last_old\n"
	newText := "first_new\n" + newB.String() + "last_new\n"
	got := Unified("a.ex", "a.ex", oldText, newText)
	if n := strings.Count(got, "@@ -"); n != 2 {
		t.Errorf("want 2 hunks, got %d:\n%s", n, got)
	}
}

// TestUnified_GitApply proves the diffs are valid patches: for a corpus of
// old/new pairs, `git apply` on the rendered diff must reproduce new exactly.
func TestUnified_GitApply(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	cases := []struct {
		name     string
		old, new string
	}{
		{"simple", "a\nb\nc\n", "a\nB\nc\n"},
		{"add lines", "a\nb\n", "a\nx\ny\nb\n"},
		{"delete lines", "a\nx\ny\nb\n", "a\nb\n"},
		{"change at start", "a\nb\nc\nd\ne\n", "A\nb\nc\nd\ne\n"},
		{"change at end", "a\nb\nc\nd\ne\n", "a\nb\nc\nd\nE\n"},
		{"multiple hunks", "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\n", "A\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nM\n"},
		{"no trailing newline old", "a\nb", "a\nb\nc\n"},
		{"no trailing newline new", "a\nb\n", "a\nb\nc"},
		{"no trailing newline both", "a\nb", "a\nc"},
		{"blank lines", "a\n\n\nb\n", "a\n\nb\n"},
		{"realistic rename", "defmodule MyApp.Accounts do\n  @doc \"fetch\"\n  def fetch_user(id) do\n    do_fetch(fetch_user_key(id))\n  end\n\n  defp fetch_user_key(id), do: {:user, id}\nend\n",
			"defmodule MyApp.Accounts do\n  @doc \"fetch\"\n  def get_user(id) do\n    do_fetch(fetch_user_key(id))\n  end\n\n  defp fetch_user_key(id), do: {:user, id}\nend\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file.txt")
			if err := os.WriteFile(path, []byte(tc.old), 0644); err != nil {
				t.Fatal(err)
			}
			patch := Unified("file.txt", "file.txt", tc.old, tc.new)
			patchPath := filepath.Join(dir, "change.patch")
			if err := os.WriteFile(patchPath, []byte(patch), 0644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("git", "apply", "--unsafe-paths", "change.patch")
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git apply failed: %v\n%s\npatch:\n%s", err, out, patch)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.new {
				t.Errorf("applied result mismatch.\ngot:\n%q\nwant:\n%q\npatch:\n%s", got, tc.new, patch)
			}
		})
	}
}
