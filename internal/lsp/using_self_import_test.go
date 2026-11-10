package lsp

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/parser"
)

// `import unquote(__MODULE__)` in a __using__ quote imports the module that
// defines __using__ (unquote evaluates __MODULE__ where the macro is
// defined): a module that uses it calls its functions unqualified. The same
// holds for `alias unquote(__MODULE__)`, for a helper body of an atom
// dispatch, and for `unquote(__MODULE__).Sub`.
func TestUsing_ImportUnquoteModule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	indexFile(t, server.store, root, "lib/helpers.ex", `defmodule MyApp.Helpers do
  defmacro __using__(_opts) do
    quote do
      import unquote(__MODULE__)
      import unquote(__MODULE__).Extra
      alias unquote(__MODULE__).Extra, as: More
    end
  end

  def helper(x), do: x
end
`)
	indexFile(t, server.store, root, "lib/extra.ex", "defmodule MyApp.Helpers.Extra do\n  def extra(x), do: x\nend\n")
	indexFile(t, server.store, root, "lib/web.ex", `defmodule MyAppWeb do
  def controller do
    quote do
      import unquote(__MODULE__)
    end
  end

  def web_helper(x), do: x

  defmacro __using__(which) when is_atom(which) do
    apply(__MODULE__, which, [])
  end
end
`)
	userSrc := `defmodule MyApp.User do
  use MyApp.Helpers

  def a, do: helper(1)
  def b, do: extra(1)
  def c, do: More.extra(1)
end
`
	indexFile(t, server.store, root, "lib/user.ex", userSrc)
	ctrlSrc := "defmodule MyAppWeb.PageController do\n  use MyAppWeb, :controller\n  def a, do: web_helper(1)\nend\n"
	indexFile(t, server.store, root, "lib/page_controller.ex", ctrlSrc)

	userURI := string(uri.File(filepath.Join(root, "lib/user.ex")))
	server.docs.Set(userURI, userSrc)
	ctrlURI := string(uri.File(filepath.Join(root, "lib/page_controller.ex")))
	server.docs.Set(ctrlURI, ctrlSrc)
	where := func(docURI string, line, col uint32) string {
		locs := definitionAt(t, server, docURI, line, col)
		if len(locs) == 0 {
			return ""
		}
		rel, _ := filepath.Rel(root, locs[0].URI.Filename())
		return rel
	}
	for _, c := range []struct {
		uri       string
		line, col uint32
		want      string
	}{
		{userURI, 3, 14, "lib/helpers.ex"},
		{userURI, 4, 14, "lib/extra.ex"},
		{userURI, 5, 19, "lib/extra.ex"},
		{ctrlURI, 2, 15, "lib/web.ex"},
	} {
		if got := where(c.uri, c.line, c.col); got != c.want {
			t.Errorf("definition at %d:%d = %q, want %q", c.line, c.col, got, c.want)
		}
	}

	// The modules whose __using__ imports MyApp.Helpers include itself, so
	// references and a rename of helper/1 reach the unqualified calls.
	if got := server.findModulesWhoseUsingImports("MyApp.Helpers"); !slices.Contains(got, "MyApp.Helpers") {
		t.Errorf("findModulesWhoseUsingImports = %v, want MyApp.Helpers in it", got)
	}
	var labels []string
	for _, item := range completionAt(t, server, userURI, 3, 15) {
		labels = append(labels, item.Label)
	}
	if !slices.Contains(labels, "helper/1") {
		t.Errorf("completion of hel… = %s, want helper", strings.Join(labels, " "))
	}
}

// A `use` inside a quote is code for the module that calls the macro: it
// brings its functions into the code of that quote, not into the module that
// holds the quote. Go-to-definition follows the same scope as the use chain.
func TestDefinition_UseInQuoteIsScopedToTheQuote(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	indexFile(t, server.store, root, "lib/other.ex", `defmodule MyApp.Other do
  defmacro __using__(_) do
    quote do
      import MyApp.Other
    end
  end

  def other_fn(x), do: x
end
`)
	src := `defmodule MyApp.Base do
  defmacro __using__(_) do
    quote do
      use MyApp.Other
      def wrapped(x), do: other_fn(x)
    end
  end

  def outside(x), do: other_fn(x)
end
`
	indexFile(t, server.store, root, "lib/base.ex", src)
	docURI := string(uri.File(filepath.Join(root, "lib/base.ex")))
	server.docs.Set(docURI, src)
	if locs := definitionAt(t, server, docURI, 4, 28); len(locs) == 0 || !strings.HasSuffix(locs[0].URI.Filename(), "other.ex") {
		t.Errorf("other_fn in the quote = %v, want lib/other.ex", locs)
	}
	if locs := definitionAt(t, server, docURI, 8, 24); len(locs) != 0 {
		t.Errorf("other_fn outside the quote = %v, want nothing: the quote's use does not reach it", locs)
	}
}

// In a quote, __MODULE__ is the module that calls the macro: a rename of the
// quote module's function does not change `__MODULE__.handle()` in its quote,
// and references do not list it.
func TestRename_ModuleCallInQuoteIsTheCaller(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	lib := `defmodule MyLib do
  defmacro __using__(_) do
    quote do
      def run, do: __MODULE__.handle()
    end
  end

  def handle, do: __MODULE__.other()
  def other, do: :ok
end
`
	indexFile(t, server.store, root, "lib/my_lib.ex", lib)
	indexFile(t, server.store, root, "lib/user.ex", "defmodule MyApp.User do\n  use MyLib\n  def handle, do: :mine\nend\n")
	locs, err := server.ReferenceNames("MyLib", "handle", NameReferenceOptions{ExcludeStdlib: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range locs {
		if strings.HasSuffix(l.FilePath, "my_lib.ex") && l.Line == 4 {
			t.Errorf("references list the quote's __MODULE__.handle() (the caller's)")
		}
	}
	if _, err := server.RenameFunctionHeadless(context.Background(), "MyLib", "handle", "handle_new"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, filepath.Join(root, "lib/my_lib.ex"))
	if !strings.Contains(got, "def run, do: __MODULE__.handle()") || !strings.Contains(got, "def handle_new, do: __MODULE__.other()") {
		t.Errorf("my_lib.ex after the rename:\n%s", got)
	}
	// Outside the quote, __MODULE__ is the module itself.
	locs, _ = server.ReferenceNames("MyLib", "other", NameReferenceOptions{ExcludeStdlib: true})
	found := false
	for _, l := range locs {
		found = found || (strings.HasSuffix(l.FilePath, "my_lib.ex") && l.Line == 8)
	}
	if !found {
		t.Errorf("references of MyLib.other miss __MODULE__.other() at line 8: %v", locs)
	}
}

// A file indexed from an editor buffer has the same indexed mtime in every
// version, so what its `use` calls inject is not kept by that mtime: after
// the file changes, the injected functions follow it.
func TestInjectedMembers_BufferIndexedFileFollowsChanges(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	for _, lib := range []string{"A", "B"} {
		indexFile(t, server.store, root, "lib/lib_"+strings.ToLower(lib)+".ex",
			"defmodule Lib"+lib+" do\n  defmacro __using__(_) do\n    quote do\n      def from_"+strings.ToLower(lib)+", do: :ok\n    end\n  end\nend\n")
	}
	path := filepath.Join(root, "lib/user.ex")
	index := func(src string) {
		writeFile(t, path, src)
		defs, refs, err := parser.ParseText(path, src)
		if err != nil {
			t.Fatal(err)
		}
		if err := server.store.IndexBufferWithRefs(path, defs, refs); err != nil {
			t.Fatal(err)
		}
	}
	names := func() string {
		var out []string
		for _, m := range server.injectedMembers("MyApp.User") {
			out = append(out, m.Name)
		}
		return strings.Join(out, " ")
	}
	index("defmodule MyApp.User do\n  use LibA\nend\n")
	if got := names(); got != "from_a" {
		t.Fatalf("injected = %q, want from_a", got)
	}
	index("defmodule MyApp.User do\n  use LibB\nend\n")
	if got := names(); got != "from_b" {
		t.Errorf("after the change, injected = %q, want from_b", got)
	}
}
