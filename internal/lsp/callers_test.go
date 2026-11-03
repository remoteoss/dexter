package lsp

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func setupCallersProject(t *testing.T) (*Server, string) {
	t.Helper()
	server, cleanup := setupTestServer(t)
	t.Cleanup(cleanup)
	root := server.projectRoot
	indexFile(t, server.store, root, "lib/shared_lib/base_worker.ex", `defmodule SharedLib.BaseWorker do
  defmacro __using__(_opts) do
    quote do
      def new(args, opts \\ []), do: {args, opts}
    end
  end
end
`)
	indexFile(t, server.store, root, "lib/my_app/mailer.ex", `defmodule MyApp.Mailer do
  use SharedLib.BaseWorker
end
`)
	indexFile(t, server.store, root, "lib/my_app/alpha.ex", `defmodule MyApp.Alpha do
  def old_name(x), do: x
  def hello, do: :hi
end
`)
	indexFile(t, server.store, root, "lib/my_app/flow.ex", `defmodule MyApp.Flow do
  import MyApp.Alpha, only: [old_name: 1]
  alias MyApp.Mailer

  def run(x) do
    a = old_name(x)
    b = Mailer.new(%{a: a})
    c = helper(b)
    MyApp.Alpha.hello()
    {c, Enum.map([1], & &1)}
  end

  defp helper(x), do: x
end
`)
	indexFile(t, server.store, root, "test/my_app/alpha_test.exs", `defmodule MyApp.AlphaTest do
  use ExUnit.Case
  alias MyApp.Alpha

  def helper, do: :ok

  describe "old_name/1" do
    test "returns its argument" do
      assert Alpha.old_name(1) == 1
    end
  end

  test "other" do
    assert MyApp.Alpha.old_name(2) == 2
  end

  def later, do: Alpha.old_name(3)
end
`)
	return server, root
}

// A call in a test block has no enclosing def. The caller is the test block,
// not the def above it, and the call is not dropped.
func TestIncomingCalls_TestBlocksAreCallers(t *testing.T) {
	server, _ := setupCallersProject(t)
	calls, err := server.IncomingCalls(context.Background(), &protocol.CallHierarchyIncomingCallsParams{
		Item: protocol.CallHierarchyItem{Data: map[string]string{"module": "MyApp.Alpha", "function": "old_name"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range calls {
		lines := make([]string, 0, len(c.FromRanges))
		for _, r := range c.FromRanges {
			lines = append(lines, strconv.Itoa(int(r.Start.Line)+1))
		}
		got = append(got, c.From.Name+"@"+strings.Join(lines, ","))
	}
	sort.Strings(got)
	want := []string{
		`MyApp.AlphaTest test "other"@14`,
		`MyApp.AlphaTest test "returns its argument"@9`,
		`MyApp.AlphaTest.later/0@17`,
		`MyApp.Flow.run/1@6`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("incoming callers:\n got %v\nwant %v", got, want)
	}
}

// Outgoing calls list the injected function, the local private function, and
// the imported bare call, as definition resolves them; the stdlib is left out.
func TestOutgoingCalls_InjectedLocalAndImported(t *testing.T) {
	server, root := setupCallersProject(t)
	calls, err := server.OutgoingCalls(context.Background(), &protocol.CallHierarchyOutgoingCallsParams{
		Item: protocol.CallHierarchyItem{Data: map[string]string{"module": "MyApp.Flow", "function": "run"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range calls {
		rel, _ := filepath.Rel(root, c.To.URI.Filename())
		got[c.To.Name] = rel + ":" + strconv.Itoa(int(c.To.Range.Start.Line)+1)
	}
	want := map[string]string{
		"MyApp.Alpha.old_name": "lib/my_app/alpha.ex:2",
		"MyApp.Mailer.new":     "lib/shared_lib/base_worker.ex:4",
		"MyApp.Flow.helper":    "lib/my_app/flow.ex:13",
		"MyApp.Alpha.hello":    "lib/my_app/alpha.ex:3",
	}
	if len(got) != len(want) {
		t.Errorf("callees = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
}

// The call hierarchy starts on a function that `use` injects, as definition
// does, with no compiled module.
func TestPrepareCallHierarchy_InjectedFunction(t *testing.T) {
	server, root := setupCallersProject(t)
	path := filepath.Join(root, "lib/my_app/flow.ex")
	text, _, _ := server.readFileText(path)
	docURI := string(uri.File(path))
	server.docs.Set(docURI, text)
	items, err := server.PrepareCallHierarchy(context.Background(), &protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
			Position:     protocol.Position{Line: 6, Character: 16}, // on "new" in Mailer.new
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "MyApp.Mailer.new/1" || !strings.HasSuffix(items[0].URI.Filename(), "base_worker.ex") {
		t.Fatalf("items = %#v", items)
	}
}

// `Module.` completes what `use` injects into the module from source, before
// any compile, as definition finds it.
func TestCompletion_InjectedFunctionsWithoutBeam(t *testing.T) {
	server, root := setupCallersProject(t)
	path := filepath.Join(root, "lib/my_app/comp.ex")
	text := "defmodule MyApp.Comp do\n  alias MyApp.Mailer\n  def x, do: Mailer.\nend\n"
	indexFile(t, server.store, root, "lib/my_app/comp.ex", text)
	docURI := string(uri.File(path))
	server.docs.Set(docURI, text)
	items := completionAt(t, server, docURI, 2, 20)
	var labels []string
	for _, item := range items {
		labels = append(labels, item.Label+"/"+item.Detail)
	}
	if !hasCompletionItem(items, "new") {
		t.Errorf("Mailer. should complete the injected new: %v", labels)
	}
}

// Workspace symbols: one row for a function with several clauses, and the
// functions that `use` injects for a Module.function query.
func TestWorkspaceSymbols_ClausesAndInjected(t *testing.T) {
	server, root := setupCallersProject(t)
	indexFile(t, server.store, root, "lib/my_app/multi.ex", `defmodule MyApp.Multi do
  def humanize(:a), do: "A"
  def humanize(:b), do: "B"
end
`)
	names := func(query string) []string {
		symbols, err := server.Symbols(context.Background(), &protocol.WorkspaceSymbolParams{Query: query})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range symbols {
			out = append(out, s.Name)
		}
		return out
	}
	if got := names("humanize"); strings.Join(got, " ") != "MyApp.Multi.humanize/1" {
		t.Errorf("humanize: %v", got)
	}
	got := strings.Join(names("MyApp.Mailer.ne"), " ")
	if !strings.Contains(got, "MyApp.Mailer.new/1") || !strings.Contains(got, "MyApp.Mailer.new/2") {
		t.Errorf("MyApp.Mailer.ne: %v", got)
	}
}

// Callees include calls without parentheses that are surely calls (after a
// pipe, and `name arg` at the start of a statement), not keywords or
// variables; and a function that a `use` injects has the callees of its def
// in the quote.
func TestCalleesOf_NoParensAndInjected(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	indexFile(t, server.store, root, "lib/flow.ex", `defmodule MyApp.Flow do
  def run(x) do
    helper x
    if x do
      other(x)
    end
    value = x
    value |> third
  end

  def helper(x), do: x
  def other(x), do: x
  def third(x), do: x
end
`)
	indexFile(t, server.store, root, "lib/base.ex", `defmodule MyApp.Base do
  defmacro __using__(_) do
    quote do
      def build(x), do: MyApp.Flow.helper(x)
    end
  end
end
`)
	indexFile(t, server.store, root, "lib/user.ex", "defmodule MyApp.User do\n  use MyApp.Base\nend\n")
	names := func(module, function string) []string {
		callees, err := server.CalleesOf(module, function)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range callees {
			out = append(out, c.Module+"."+c.Function)
		}
		sort.Strings(out)
		return out
	}
	if got := strings.Join(names("MyApp.Flow", "run"), " "); got != "MyApp.Flow.helper MyApp.Flow.other MyApp.Flow.third" {
		t.Errorf("callees of run = %s", got)
	}
	if got := strings.Join(names("MyApp.User", "build"), " "); got != "MyApp.Flow.helper" {
		t.Errorf("callees of the injected build = %s", got)
	}
}
