package diff

import (
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

func TestUnified_NoTrailingNewline(t *testing.T) {
	cases := []struct {
		name     string
		old, new string
		want     string
	}{
		{
			name: "old lacks newline",
			old:  "a\nb",
			new:  "a\nb\nc\n",
			want: `--- a/f
+++ b/f
@@ -1,2 +1,3 @@
 a
-b
\ No newline at end of file
+b
+c
`,
		},
		{
			name: "new lacks newline",
			old:  "a\nb\n",
			new:  "a\nb\nc",
			want: `--- a/f
+++ b/f
@@ -1,2 +1,3 @@
 a
 b
+c
\ No newline at end of file
`,
		},
		{
			name: "both lack newline",
			old:  "a\nb",
			new:  "a\nc",
			want: `--- a/f
+++ b/f
@@ -1,2 +1,2 @@
 a
-b
\ No newline at end of file
+c
\ No newline at end of file
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Unified("f", "f", tc.old, tc.new); got != tc.want {
				t.Errorf("diff mismatch.\ngot:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}
