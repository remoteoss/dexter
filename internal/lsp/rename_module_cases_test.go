package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
)

// TestRenameModule_AliasCases checks module renames through the forms of
// alias that real code uses, with the compiler: after each rename the project
// must compile with no warning. A case can also say what a file must (or must
// not) hold after the rename, or that the rename is refused.
func TestRenameModule_AliasCases(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles Mix projects")
	}
	mix, err := exec.LookPath("mix")
	if err != nil {
		t.Skip("mix is not on PATH")
	}
	bar := "defmodule A.Foo.Bar do\n  def hi, do: :ok\nend\n"
	cases := []struct {
		name     string
		files    map[string]string
		from, to string
		refuse   bool              // a refusal is the right answer
		want     map[string]string // file → text it must hold
		notWant  map[string]string // file → text it must not hold
	}{
		{
			name: "alias __MODULE__.X, same namespace",
			files: map[string]string{
				"lib/a/svc.ex":      "defmodule A.Svc do\n  alias __MODULE__.Impl\n  def run, do: Impl.go()\nend\n",
				"lib/a/svc/impl.ex": "defmodule A.Svc.Impl do\n  def go, do: :ok\nend\n",
			},
			from: "A.Svc.Impl", to: "A.Svc.Engine",
			want: map[string]string{"lib/a/svc.ex": "alias __MODULE__.Engine"},
		},
		{
			name: "alias __MODULE__.X, other namespace",
			files: map[string]string{
				"lib/a/svc.ex":      "defmodule A.Svc do\n  alias __MODULE__.Impl\n  def run, do: Impl.go()\nend\n",
				"lib/a/svc/impl.ex": "defmodule A.Svc.Impl do\n  def go, do: :ok\nend\n",
			},
			from: "A.Svc.Impl", to: "A.Other.Impl",
		},
		{
			name: "__MODULE__.Sub call",
			files: map[string]string{
				"lib/a/foo.ex":     "defmodule A.Foo do\n  def x, do: __MODULE__.Bar.hi()\nend\n",
				"lib/a/foo/bar.ex": bar,
			},
			from: "A.Foo.Bar", to: "A.Zed.Qux",
		},
		{
			name: "as: alias of an ancestor namespace",
			files: map[string]string{
				"lib/a/u.ex":       "defmodule A.U do\n  alias A.Foo, as: F\n  def x, do: F.Bar.hi()\nend\n",
				"lib/a/foo/bar.ex": bar,
			},
			from: "A.Foo.Bar", to: "A.Foo.Baz",
			want: map[string]string{"lib/a/u.ex": "F.Baz.hi()"},
		},
		{
			name: "as: alias of an ancestor namespace, leaving it",
			files: map[string]string{
				"lib/a/u.ex":         "defmodule A.U do\n  alias A.Foo, as: F\n  def x, do: F.Bar.hi()\n  def y, do: F.Thing.t()\nend\n",
				"lib/a/foo/bar.ex":   bar,
				"lib/a/foo/thing.ex": "defmodule A.Foo.Thing do\n  def t, do: :ok\nend\n",
			},
			from: "A.Foo.Bar", to: "A.Zed.Qux",
		},
		{
			name: "aliases inside functions",
			files: map[string]string{
				"lib/a/u.ex":         "defmodule A.U do\n  def a do\n    alias A.Foo.Bar\n    Bar.hi()\n  end\n\n  def b do\n    alias A.Other.Bar\n    Bar.y()\n  end\nend\n",
				"lib/a/foo/bar.ex":   bar,
				"lib/a/other/bar.ex": "defmodule A.Other.Bar do\n  def y, do: :ok\nend\n",
			},
			from: "A.Foo.Bar", to: "A.Foo.Baz",
			want: map[string]string{"lib/a/u.ex": "alias A.Other.Bar\n    Bar.y()"},
		},
		{
			name: "short and full name on one line, and strings",
			files: map[string]string{
				"lib/a/u.ex":       "defmodule A.U do\n  alias A.Foo.Bar\n  def x, do: {Bar.hi(), \"see Bar and A.Foo.Bar\"}\n  def y, do: {A.Foo.Bar.hi(), Bar.hi()}\nend\n",
				"lib/a/foo/bar.ex": bar,
			},
			from: "A.Foo.Bar", to: "A.Zed.Qux",
			want: map[string]string{"lib/a/u.ex": "\"see Bar and A.Foo.Bar\""},
		},
		{
			name: "nested module with a sibling of the same short name",
			files: map[string]string{
				"lib/a/outer.ex": "defmodule A.Outer do\n  defmodule Inner do\n    def i, do: 1\n  end\n\n  def u, do: Inner.i()\nend\n\ndefmodule A.Other2 do\n  defmodule Inner do\n    def j, do: 2\n  end\n\n  def v, do: Inner.j()\nend\n",
			},
			from: "A.Outer.Inner", to: "A.Outer.Inner2",
			want: map[string]string{"lib/a/outer.ex": "def v, do: Inner.j()"},
		},
		{
			name: "group with the module and its submodule",
			files: map[string]string{
				"lib/a/u.ex":           "defmodule A.U do\n  alias A.Foo.{Bar, Bar.Sub, Thing}\n  def x, do: {Bar.hi(), Sub.s(), Thing.t()}\nend\n",
				"lib/a/foo/bar.ex":     bar,
				"lib/a/foo/bar/sub.ex": "defmodule A.Foo.Bar.Sub do\n  def s, do: :ok\nend\n",
				"lib/a/foo/thing.ex":   "defmodule A.Foo.Thing do\n  def t, do: :ok\nend\n",
			},
			from: "A.Foo.Bar", to: "A.Zed.Qux",
		},
		{
			name: "multi-line group, member leaves the namespace",
			files: map[string]string{
				"lib/a/u.ex":         "defmodule A.U do\n  alias A.Foo.{\n    Bar,\n    Thing\n  }\n\n  def x, do: {Bar.hi(), Thing.t()}\nend\n",
				"lib/a/foo/bar.ex":   bar,
				"lib/a/foo/thing.ex": "defmodule A.Foo.Thing do\n  def t, do: :ok\nend\n",
			},
			from: "A.Foo.Bar", to: "A.Zed.Qux",
			refuse: true,
		},
		{
			name: "multi-line alias with as: on the next line",
			files: map[string]string{
				"lib/a/u.ex":       "defmodule A.U do\n  # Qux is mentioned here\n  alias A.Foo.Bar,\n    as: B\n\n  def x, do: B.hi()\nend\n",
				"lib/a/foo/bar.ex": bar,
			},
			from: "A.Foo.Bar", to: "A.Zed.Qux",
		},
		{
			name: "defimpl with for: on the next line",
			files: map[string]string{
				"lib/a/p.ex":       "defprotocol A.P do\n  def p(x)\nend\n",
				"lib/a/impl.ex":    "defimpl A.P,\n  for: A.Foo.Bar do\n  def p(_), do: :ok\nend\n",
				"lib/a/u.ex":       "defmodule A.U do\n  def x, do: A.P.A.Foo.Bar.__impl__(:for)\nend\n",
				"lib/a/foo/bar.ex": "defmodule A.Foo.Bar do\n  defstruct []\n  def hi, do: :ok\nend\n",
			},
			from: "A.Foo.Bar", to: "A.Zed.Qux",
		},
		{
			name: "ancestor alias whose other use is a comment",
			files: map[string]string{
				"lib/a/u.ex":       "defmodule A.U do\n  alias A.Foo\n  # Foo is the namespace\n  def x, do: Foo.Bar.hi()\nend\n",
				"lib/a/foo/bar.ex": bar,
			},
			from: "A.Foo.Bar", to: "A.Zed.Qux",
		},
		{
			name: "ancestor alias in a group",
			files: map[string]string{
				"lib/a/u.ex":       "defmodule A.U do\n  alias A.{Foo, Svc}\n  def x, do: {Foo.Bar.hi(), Svc.s()}\nend\n",
				"lib/a/foo/bar.ex": bar,
				"lib/a/svc.ex":     "defmodule A.Svc do\n  def s, do: :ok\nend\n",
			},
			from: "A.Foo.Bar", to: "A.Zed.Bar",
		},
		{
			name: "full new name whose first segment is an alias",
			files: map[string]string{
				"lib/a/u.ex":       "defmodule A.U do\n  alias X.Zed\n  def x, do: {A.Foo.Bar.hi(), Zed.z()}\nend\n",
				"lib/x/zed.ex":     "defmodule X.Zed do\n  def z, do: :ok\nend\n",
				"lib/a/foo/bar.ex": bar,
			},
			from: "A.Foo.Bar", to: "Zed.Bar",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "mix.exs"), "defmodule Cases.MixProject do\n  use Mix.Project\n  def project, do: [app: :cases, version: \"0.1.0\", elixir: \"~> 1.14\"]\nend\n")
			for rel, src := range c.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(dir, rel), src)
			}
			servers, st := sessionsAt(t, dir, 1)
			s := servers[0]
			for rel := range c.files {
				p := filepath.Join(dir, rel)
				defs, refs, err := parser.ParseFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.IndexFileWithRefs(p, defs, refs); err != nil {
					t.Fatal(err)
				}
			}
			indexed, err := s.RenameModuleHeadless(context.Background(), c.from, c.to)
			if c.refuse {
				if err == nil {
					t.Fatalf("want a refusal, the rename was applied")
				}
				return
			}
			if err != nil {
				t.Fatalf("rename: %v", err)
			}
			if indexed != nil {
				<-indexed
			}
			s.index.backgroundWork.Wait()
			cmd := exec.Command(mix, "compile", "--force", "--warnings-as-errors")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "MIX_ENV=dev")
			out, err := cmd.CombinedOutput()
			if err != nil || strings.Contains(string(out), "warning:") {
				t.Errorf("mix compile failed: %v\n%s\n%s", err, out, dumpLib(t, dir))
			}
			for rel, text := range c.want {
				if got := readFile(t, filepath.Join(dir, rel)); !strings.Contains(got, text) {
					t.Errorf("%s does not hold %q:\n%s", rel, text, got)
				}
			}
			for rel, text := range c.notWant {
				if got := readFile(t, filepath.Join(dir, rel)); strings.Contains(got, text) {
					t.Errorf("%s holds %q:\n%s", rel, text, got)
				}
			}
		})
	}
}

// dumpLib returns the .ex files of the project, for a failure message.
func dumpLib(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(filepath.Join(dir, "lib"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".ex") {
			rel, _ := filepath.Rel(dir, p)
			b.WriteString("--- " + rel + "\n" + readFile(t, p))
		}
		return nil
	})
	return b.String()
}
