package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
)

// The arity fixture: display_name/1 and display_name/2 are two functions, and
// label/1..2 is one (a default argument).
var arityGoal = bbFile{"lib/arity_goal.ex", `defmodule AG.Goal do
  def display_name(goal), do: display_name(goal, [])

  def display_name(goal, opts), do: {goal, opts}

  def label(goal, sep \\ "-"), do: {goal, sep}

  def all(goals) do
    Enum.map(goals, &display_name/1)
    Enum.map(goals, &display_name(&1, []))
    goals |> display_name()
    {label(1), label(1, ":")}
  end
end
`}

var arityUser = bbFile{"lib/arity_user.ex", `defmodule AG.User do
  alias AG.Goal
  import AG.Goal, only: [display_name: 2]

  def a(g), do: Goal.display_name(g)
  def b(g), do: Goal.display_name(g, [])
  def c(g), do: g |> Goal.display_name([])
  def d(g), do: "#{Goal.display_name(g)} and #{Goal.display_name(g, [])}"
  def e(g), do: display_name(g, [])
  def f(g), do: {Goal.label(g), Goal.label(g, ":")}
end
`}

func arityRefs(t *testing.T, f bbFile, marker, token string, includeDecl bool) []string {
	t.Helper()
	server, cleanup := bbServer(t, arityGoal, arityUser)
	defer cleanup()
	uri := bbOpen(t, server, f)
	l, c := bbPos(t, f.src, marker, token)
	result, err := server.References(context.Background(), &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: l, Character: c},
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: includeDecl},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := bbLines(result, server.projectRoot)
	sort.Strings(got)
	return got
}

func TestReferences_SeparateArities(t *testing.T) {
	tests := []struct {
		name          string
		file          bbFile
		marker, token string
		want          []string
	}{
		{
			name: "display_name/1 from a call", file: arityUser,
			marker: "def a(g), do: Goal.display_name(g)", token: "display_name",
			want: []string{
				"lib/arity_goal.ex:11", // goals |> display_name()
				"lib/arity_goal.ex:9",  // &display_name/1
				"lib/arity_user.ex:5",
				"lib/arity_user.ex:8", // inside #{}
			},
		},
		{
			name: "display_name/2 from its head", file: arityGoal,
			marker: "def display_name(goal, opts)", token: "display_name",
			want: []string{
				"lib/arity_goal.ex:10", // &display_name(&1, [])
				"lib/arity_goal.ex:2",  // the call in display_name/1
				"lib/arity_user.ex:3",  // only: [display_name: 2]
				"lib/arity_user.ex:6",
				"lib/arity_user.ex:7", // piped: g |> display_name([])
				"lib/arity_user.ex:8", // inside #{}
				"lib/arity_user.ex:9", // imported
			},
		},
		{
			name: "a default argument makes one function of two arities", file: arityGoal,
			marker: "{label(1), label(1, \":\")}", token: "label",
			want: []string{"lib/arity_goal.ex:12", "lib/arity_user.ex:10"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := arityRefs(t, tt.file, tt.marker, tt.token, false)
			want := append([]string(nil), tt.want...)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("references = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("references = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestReferences_DeclarationOfTheArityOnly(t *testing.T) {
	got := arityRefs(t, arityUser, "def a(g), do: Goal.display_name(g)", "display_name", true)
	for _, line := range got {
		if line == "lib/arity_goal.ex:4" {
			t.Fatalf("references of display_name/1 list the head of display_name/2: %v", got)
		}
	}
	found := false
	for _, line := range got {
		found = found || line == "lib/arity_goal.ex:2"
	}
	if !found {
		t.Fatalf("references of display_name/1 with declarations miss its head lib/arity_goal.ex:2: %v", got)
	}
}

// Hover takes the definition of the call's arity: a pipe adds one argument,
// and a def with a default argument covers each arity it accepts.
func TestHover_DefinitionOfTheCallsArity(t *testing.T) {
	lib := bbFile{"lib/arity_hover.ex", `defmodule AH.Jobs do
  @doc "Runs one job."
  def run(job), do: job

  @doc false
  def run(job, opts), do: {job, opts}

  @doc "Steps a job, if the job can be stepped."
  def step(job, opts \\ []), do: {job, opts}
end
`}
	user := bbFile{"lib/arity_hover_user.ex", `defmodule AH.User do
  def a(job), do: job |> AH.Jobs.run()
  def b(job), do: AH.Jobs.run(job, [])
  def c(job), do: job |> AH.Jobs.step()
  def d(job), do: AH.Jobs.step(job, [])
end
`}
	server, cleanup := bbServer(t, lib, user)
	defer cleanup()
	uri := bbOpen(t, server, user)
	text := func(marker string) string {
		l, c := bbPos(t, user.src, marker, marker[strings.LastIndex(marker, ".")+1:strings.Index(marker, "(")])
		h := hoverAt(t, server, uri, l, c)
		if h == nil {
			return ""
		}
		return h.Contents.Value
	}
	if got := text("AH.Jobs.run()"); !strings.Contains(got, "Runs one job.") {
		t.Errorf("hover on a piped run/1 = %q, want its doc", got)
	}
	if got := text("AH.Jobs.run(job, [])"); strings.Contains(got, "Runs one job.") || !strings.Contains(got, "def run(job, opts)") {
		t.Errorf("hover on run/2 = %q, want run/2 without the doc of run/1", got)
	}
	for _, marker := range []string{"AH.Jobs.step()", "AH.Jobs.step(job, [])"} {
		if got := text(marker); !strings.Contains(got, "if the job can be stepped") {
			t.Errorf("hover on %s = %q, want the doc of the def with a default", marker, got)
		}
	}
}

// A call whose arity cannot be read is listed for every arity, and a request
// without an arity (as MCP and the CLI make) lists all of them.
func TestReferences_UnknownArityStays(t *testing.T) {
	lib := bbFile{"lib/arity_unknown.ex", `defmodule AU.Mod do
  def h(a), do: a
  def h(a, b), do: {a, b}
end
`}
	user := bbFile{"lib/arity_unknown_user.ex", `defmodule AU.User do
  def a(x), do: AU.Mod.h(x)
  def b(x), do: AU.Mod.h(x, x)
  def c(x), do: AU.Mod.h(x,
`}
	server, cleanup := bbServer(t, lib, user)
	defer cleanup()
	for _, tt := range []struct {
		arity int
		want  []string
	}{
		{1, []string{"lib/arity_unknown_user.ex:2", "lib/arity_unknown_user.ex:4"}},
		{2, []string{"lib/arity_unknown_user.ex:3", "lib/arity_unknown_user.ex:4"}},
	} {
		locs, err := server.ReferenceNames("AU.Mod", "h", NameReferenceOptions{Kind: NameKindCallable, Arity: tt.arity, HasArity: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := nameLines(locs, server.projectRoot); strings.Join(got, " ") != strings.Join(tt.want, " ") {
			t.Errorf("references of h/%d = %v, want %v", tt.arity, got, tt.want)
		}
	}
	locs, err := server.ReferenceNames("AU.Mod", "h", NameReferenceOptions{Kind: NameKindCallable})
	if err != nil {
		t.Fatal(err)
	}
	if got := nameLines(locs, server.projectRoot); len(got) != 3 {
		t.Errorf("references of h with no arity = %v, want all three calls", got)
	}
}

func nameLines(locs []NameLocation, root string) []string {
	var out []string
	for _, l := range locs {
		rel, _ := filepath.Rel(root, l.FilePath)
		out = append(out, fmt.Sprintf("%s:%d", rel, l.Line))
	}
	sort.Strings(out)
	return out
}

// Rename does not separate arities yet: a rename of display_name changes
// display_name/1 and display_name/2, their heads and all their calls.
func TestRename_StillChangesEveryArity(t *testing.T) {
	server, cleanup := bbServer(t, arityGoal, arityUser)
	defer cleanup()
	indexed, err := server.RenameFunctionHeadless(context.Background(), "AG.Goal", "display_name", "title")
	if err != nil {
		t.Fatal(err)
	}
	<-indexed
	for _, f := range []bbFile{arityGoal, arityUser} {
		data, err := os.ReadFile(filepath.Join(server.projectRoot, f.rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "display_name") {
			t.Errorf("%s after the rename still has display_name:\n%s", f.rel, data)
		}
	}
}

// Call hierarchy keeps the arity of the item: the callers of display_name/2
// are not those of display_name/1.
func TestCallHierarchy_IncomingCallsOfTheArity(t *testing.T) {
	server, cleanup := bbServer(t, arityGoal, arityUser)
	defer cleanup()
	uri := bbOpen(t, server, arityGoal)
	l, c := bbPos(t, arityGoal.src, "def display_name(goal, opts)", "display_name")
	items, err := server.PrepareCallHierarchy(context.Background(), &protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: l, Character: c},
		},
	})
	if err != nil || len(items) != 1 {
		t.Fatalf("PrepareCallHierarchy = %v, %v", items, err)
	}
	if items[0].Name != "AG.Goal.display_name/2" {
		t.Errorf("item = %s, want AG.Goal.display_name/2", items[0].Name)
	}
	calls, err := server.IncomingCalls(context.Background(), &protocol.CallHierarchyIncomingCallsParams{Item: items[0]})
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	for _, call := range calls {
		callers = append(callers, call.From.Name)
	}
	sort.Strings(callers)
	for _, name := range callers {
		if strings.HasSuffix(name, ".a/1") {
			t.Errorf("incoming calls of display_name/2 include AG.User.a/1, which calls display_name/1: %v", callers)
		}
	}
	found := false
	for _, name := range callers {
		found = found || strings.HasSuffix(name, ".b/1")
	}
	if !found {
		t.Errorf("incoming calls of display_name/2 miss AG.User.b/1: %v", callers)
	}
}

// Hover on a local call takes the head of the call's arity from the buffer.
func TestHover_LocalCallOfTheArity(t *testing.T) {
	lib := bbFile{"lib/arity_local_hover.ex", `defmodule AL.Jobs do
  @doc "Runs one job."
  def run(job), do: run(job, [])

  defmodule Early do
    @doc "The early run/2."
    def run(job, opts), do: {job, opts}
  end

  @doc "Runs a job with options."
  def run(job, opts), do: {job, opts}

  def go(job), do: run(job, [])

  defmodule Inner do
    @doc "The inner run/2."
    def run(job, opts), do: {job, opts}
  end
end
`}
	server, cleanup := bbServer(t, lib)
	defer cleanup()
	uri := bbOpen(t, server, lib)
	l, c := bbPos(t, lib.src, "def go(job), do: run(job, [])", "run")
	h := hoverAt(t, server, uri, l, c)
	if h == nil || !strings.Contains(h.Contents.Value, "Runs a job with options.") {
		t.Errorf("hover on a local run/2 = %v, want the doc of run/2", h)
	}
	// Definition too: the module's own run/2, not run/1 or a nested module's.
	if got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot); len(got) != 1 || got[0] != "lib/arity_local_hover.ex:11" {
		t.Errorf("definition of a local run/2 = %v, want lib/arity_local_hover.ex:11", got)
	}
}

// A head with trailing keyword pairs takes one keyword list: def f(a, b: 1,
// c: 2) is f/2, and the call f(x, b: 2) is one of its references.
func TestReferences_KeywordHead(t *testing.T) {
	lib := bbFile{"lib/arity_kw.ex", `defmodule AK.Mod do
  def f(a), do: a
  def f(a, b: 1, c: 2), do: a
end
`}
	user := bbFile{"lib/arity_kw_user.ex", `defmodule AK.User do
  def a(x), do: AK.Mod.f(x, b: 2)
  def b(x), do: AK.Mod.f(x)
end
`}
	server, cleanup := bbServer(t, lib, user)
	defer cleanup()
	locs, err := server.ReferenceNames("AK.Mod", "f", NameReferenceOptions{Kind: NameKindCallable, Arity: 2, HasArity: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := nameLines(locs, server.projectRoot); strings.Join(got, " ") != "lib/arity_kw_user.ex:2" {
		t.Errorf("references of f/2 = %v, want lib/arity_kw_user.ex:2", got)
	}
}
