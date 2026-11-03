package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renameFiles indexes files (rel → before), renames module.foo to bar
// headless, and checks each file against after.
func renameFiles(t *testing.T, module string, files map[string][2]string) {
	t.Helper()
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	core := servers[0]
	for rel, texts := range files {
		indexFile(t, st, dir, rel, texts[0])
	}
	indexed, err := core.RenameFunctionHeadless(context.Background(), module, "foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	<-indexed
	for rel, texts := range files {
		if got := readFile(t, filepath.Join(dir, rel)); got != texts[1] {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", rel, got, texts[1])
		}
	}
}

// After a nested module's end, the outer module's unqualified calls are its
// own again: the module at a line comes from do/end matching, not from the
// last defmodule above it.
func TestRename_NestedModuleEndsBeforeTheCall(t *testing.T) {
	renameFiles(t, "MyApp.Alpha", map[string][2]string{"lib/alpha.ex": {
		"defmodule MyApp.Alpha do\n  def foo(x), do: x\n\n  defmodule Inner do\n    def foo(x), do: x\n    def i, do: foo(0)\n  end\n\n  def m, do: foo(1)\n  def t, do: {__MODULE__.foo(1), __MODULE__.Inner.foo(2)}\nend\n",
		"defmodule MyApp.Alpha do\n  def bar(x), do: x\n\n  defmodule Inner do\n    def foo(x), do: x\n    def i, do: foo(0)\n  end\n\n  def m, do: bar(1)\n  def t, do: {__MODULE__.bar(1), __MODULE__.Inner.foo(2)}\nend\n",
	}})
}

// Captures without a module, in the defining module and under an import.
func TestRename_BareCaptures(t *testing.T) {
	renameFiles(t, "MyApp.Alpha", map[string][2]string{
		"lib/alpha.ex": {
			"defmodule MyApp.Alpha do\n  def foo(x), do: x\n  def a, do: Enum.map([1], &foo/1)\n  def b, do: &foo(&1)\n  def c, do: &__MODULE__.foo/1\n  def d(x), do: x |> Enum.map(&foo/1)\nend\n",
			"defmodule MyApp.Alpha do\n  def bar(x), do: x\n  def a, do: Enum.map([1], &bar/1)\n  def b, do: &bar(&1)\n  def c, do: &__MODULE__.bar/1\n  def d(x), do: x |> Enum.map(&bar/1)\nend\n",
		},
		"lib/importer.ex": {
			"defmodule MyApp.Importer do\n  import MyApp.Alpha\n\n  def run, do: Enum.map([1], &foo/1)\n  def go, do: foo(2)\nend\n",
			"defmodule MyApp.Importer do\n  import MyApp.Alpha\n\n  def run, do: Enum.map([1], &bar/1)\n  def go, do: bar(2)\nend\n",
		},
		"lib/excluder.ex": {
			"defmodule MyApp.Excluder do\n  import MyApp.Alpha, except: [foo: 1]\n\n  def foo(x), do: x\n  def run, do: Enum.map([1], &foo/1)\nend\n",
			"defmodule MyApp.Excluder do\n  import MyApp.Alpha, except: [bar: 1]\n\n  def foo(x), do: x\n  def run, do: Enum.map([1], &foo/1)\nend\n",
		},
	})
}

// Only code changes: not the name in a string, a sigil, a charlist, a
// comment, or a heredoc. Code in a #{} interpolation changes.
func TestRename_OnlyCodeTokens(t *testing.T) {
	renameFiles(t, "MyApp.Alpha", map[string][2]string{
		"lib/alpha.ex": {
			"defmodule MyApp.Alpha do\n  @doc \"\"\"\n  Call __MODULE__.foo(1) to start.\n  \"\"\"\n  def foo(x), do: x\n  # __MODULE__.foo(2) is the old name\n  def s, do: \"__MODULE__.foo(3)\"\nend\n",
			"defmodule MyApp.Alpha do\n  @doc \"\"\"\n  Call __MODULE__.foo(1) to start.\n  \"\"\"\n  def bar(x), do: x\n  # __MODULE__.foo(2) is the old name\n  def s, do: \"__MODULE__.foo(3)\"\nend\n",
		},
		"lib/user.ex": {
			"defmodule MyApp.User do\n  def s, do: {MyApp.Alpha.foo(1), \"MyApp.Alpha.foo(2)\", ~s(MyApp.Alpha.foo), 'foo'}\n  def i, do: \"#{MyApp.Alpha.foo(3)} and MyApp.Alpha.foo\"\nend\n",
			"defmodule MyApp.User do\n  def s, do: {MyApp.Alpha.bar(1), \"MyApp.Alpha.foo(2)\", ~s(MyApp.Alpha.foo), 'foo'}\n  def i, do: \"#{MyApp.Alpha.bar(3)} and MyApp.Alpha.foo\"\nend\n",
		},
	})
}

// A file that appears at the destination of a move after the collision
// check stops the rename; it is not overwritten.
func TestRenameModule_DestinationAppearedAborts(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	core := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	gPath := filepath.Join(dir, "lib/gamma.ex")
	const theirs = "# someone else's file\n"
	t.Cleanup(func() { renameTestHookBeforeCommit = nil })
	renameTestHookBeforeCommit = func() { writeFile(t, gPath, theirs) }
	_, err := core.RenameModuleHeadless(context.Background(), "MyApp.Alpha", "MyApp.Gamma")
	if err == nil || !strings.Contains(err.Error(), filepath.Join("lib", "gamma.ex")+" appeared while the rename ran") {
		t.Fatalf("want a refusal that names lib/gamma.ex, got %v", err)
	}
	if got := readFile(t, gPath); got != theirs {
		t.Errorf("gamma.ex was overwritten:\n%s", got)
	}
	if got := readFile(t, filepath.Join(dir, "lib/alpha.ex")); got != safetyDefA {
		t.Errorf("alpha.ex changed:\n%s", got)
	}
	noTempFiles(t, dir)
}

// A change of the same size within one mtime tick, after the planning read,
// is found: stage compares the content with the read.
func TestRename_SameSizeSameMtimeChangeFound(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	core := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	bPath := filepath.Join(dir, "lib/beta.ex")
	info, err := os.Stat(bPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(safetyCallB, "foo(1)", "foo(9)", 1)
	t.Cleanup(func() { renameTestHookBeforeStage = nil })
	renameTestHookBeforeStage = func() {
		writeFile(t, bPath, changed)
		_ = os.Chtimes(bPath, info.ModTime(), info.ModTime())
	}
	_, err = core.RenameFunctionHeadless(context.Background(), "MyApp.Alpha", "foo", "bar")
	if err == nil || !strings.Contains(err.Error(), filepath.Join("lib", "beta.ex")+" changed on disk while the rename ran") {
		t.Fatalf("want a refusal that names lib/beta.ex, got %v", err)
	}
	if got := readFile(t, bPath); got != changed {
		t.Errorf("the change was lost:\n%s", got)
	}
}

// Import options over several lines: an only: list that names the function
// brings it in, one that leaves it out does not, and an except: list that
// names it keeps it out. The function's key is renamed in either list.
func TestRename_MultiLineImportOptions(t *testing.T) {
	renameFiles(t, "MyApp.Alpha", map[string][2]string{
		"lib/alpha.ex": {
			"defmodule MyApp.Alpha do\n  def foo(x), do: x\n  def other(x), do: x\nend\n",
			"defmodule MyApp.Alpha do\n  def bar(x), do: x\n  def other(x), do: x\nend\n",
		},
		"lib/only_in.ex": {
			"defmodule MyApp.OnlyIn do\n  import MyApp.Alpha,\n    only: [\n      other: 1,\n      foo: 1\n    ]\n\n  def run, do: foo(1) + other(2)\nend\n",
			"defmodule MyApp.OnlyIn do\n  import MyApp.Alpha,\n    only: [\n      other: 1,\n      bar: 1\n    ]\n\n  def run, do: bar(1) + other(2)\nend\n",
		},
		"lib/only_out.ex": {
			"defmodule MyApp.OnlyOut do\n  import MyApp.Alpha,\n    only: [\n      other: 1\n    ]\n\n  def foo(x), do: x\n  def run, do: foo(1) + other(2)\nend\n",
			"defmodule MyApp.OnlyOut do\n  import MyApp.Alpha,\n    only: [\n      other: 1\n    ]\n\n  def foo(x), do: x\n  def run, do: foo(1) + other(2)\nend\n",
		},
		"lib/except.ex": {
			"defmodule MyApp.Except do\n  import MyApp.Alpha,\n    except: [\n      foo: 1\n    ]\n\n  def run(foo), do: foo(1) + other(2)\nend\n",
			"defmodule MyApp.Except do\n  import MyApp.Alpha,\n    except: [\n      bar: 1\n    ]\n\n  def run(foo), do: foo(1) + other(2)\nend\n",
		},
		"lib/functions.ex": {
			"defmodule MyApp.Functions do\n  import MyApp.Alpha, only: :functions\n\n  def run, do: foo(1)\nend\n",
			"defmodule MyApp.Functions do\n  import MyApp.Alpha, only: :functions\n\n  def run, do: bar(1)\nend\n",
		},
		"lib/macros.ex": {
			"defmodule MyApp.Macros do\n  import MyApp.Alpha, only: :macros\n\n  def foo(x), do: x\n  def run, do: foo(1)\nend\n",
			"defmodule MyApp.Macros do\n  import MyApp.Alpha, only: :macros\n\n  def foo(x), do: x\n  def run, do: foo(1)\nend\n",
		},
	})
}
