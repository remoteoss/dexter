package lsp

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

// TestRename_CompilesAfterEachRename checks renames with the real compiler: a
// Mix project that uses aliases (grouped, as:, __MODULE__), imports with
// multi-line only:/except:, nested modules with the same function name,
// captures, pipes, defdelegate with as:, default arguments, __MODULE__ calls,
// the name in strings, sigils, and comments, #{} interpolation, and an import
// that a __using__ block injects. After each rename the project must compile
// with no warnings (an undefined or unused function is a warning), xref must
// run, and the index must equal a cold build.
func TestRename_CompilesAfterEachRename(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a Mix project")
	}
	mix, err := exec.LookPath("mix")
	if err != nil {
		t.Skip("mix is not on PATH")
	}
	if _, err := exec.LookPath("elixir"); err != nil {
		t.Skip("elixir is not on PATH")
	}
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", "rename_compile"), dir)
	compile := func(step string) {
		t.Helper()
		for _, args := range [][]string{{"compile", "--force", "--warnings-as-errors"}, {"xref", "graph", "--format", "stats"}} {
			cmd := exec.Command(mix, args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "MIX_ENV=dev")
			out, err := cmd.CombinedOutput()
			if err != nil || strings.Contains(string(out), "warning:") {
				t.Fatalf("%s: mix %s failed: %v\n%s", step, strings.Join(args, " "), err, out)
			}
		}
	}
	compile("before any rename")

	servers, st := sessionsAt(t, dir, 1)
	editor := servers[0]
	for _, p := range elixirFiles(t, dir) {
		defs, refs, err := parser.ParseFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.IndexFileWithRefs(p, defs, refs); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	lib := func(rel string) string { return filepath.Join(dir, "lib", "shop", rel) }
	after := func(step string, indexed <-chan struct{}) {
		t.Helper()
		if indexed != nil {
			<-indexed
		}
		editor.index.backgroundWork.Wait()
		compile(step)
		expectIndexIsColdBuild(t, st, dir)
	}

	// Headless renames (an MCP tool).
	headlessFn := func(module, from, to string) {
		t.Helper()
		indexed, err := editor.RenameFunctionHeadless(ctx, module, from, to)
		if err != nil {
			t.Fatalf("rename %s.%s: %v", module, from, err)
		}
		after("rename "+module+"."+from, indexed)
	}
	headlessMod := func(from, to string) {
		t.Helper()
		indexed, err := editor.RenameModuleHeadless(ctx, from, to)
		if err != nil {
			t.Fatalf("rename %s: %v", from, err)
		}
		after("rename "+from, indexed)
	}
	// A module's own fetch/3 next to an imported fetch/1: only the calls of
	// arity 3 are its own, also without parentheses.
	headlessFn("Shop.Plain", "fetch", "triple")
	plain := readFile(t, lib("plain.ex"))
	for _, want := range []string{"do: fetch(id)", "|> fetch()", "&fetch/1", "    fetch id\n", "do: triple(1, 2, 3)", "    triple 1, 2, 3\n", "defp triple(a, b, c)"} {
		if !strings.Contains(plain, want) {
			t.Errorf("plain.ex does not have %q:\n%s", want, plain)
		}
	}
	headlessFn("Shop.Accounts", "fetch", "load")
	// A plain import: the unqualified calls, the pipe, and the capture of
	// the imported fetch/1 change; the module's own function stays.
	plain = readFile(t, lib("plain.ex"))
	for _, want := range []string{"do: load(id)", "|> load()", "&load/1", "do: load(1, a: 1, b: 2)", "id |> load", "case load(id) do", "    load id\n", "do: triple(1, 2, 3)", "    triple 1, 2, 3\n", "defp triple(a, b, c)"} {
		if !strings.Contains(plain, want) {
			t.Errorf("plain.ex does not have %q:\n%s", want, plain)
		}
	}
	// The target of a defdelegate that a __using__ quote injects: only
	// as: changes (or is added); the facades keep their names, so the
	// modules that use the quote keep their calls.
	headlessFn("Shop.Delegating", "find_one", "find_first")
	delegating := readFile(t, lib("delegating.ex"))
	for _, want := range []string{
		"defdelegate lookup(id), to: Shop.Delegating, as: :find_first",
		"defdelegate find_one(id), to: Shop.Delegating, as: :find_first",
		"def find_first(id)",
	} {
		if !strings.Contains(delegating, want) {
			t.Errorf("delegating.ex does not have %q:\n%s", want, delegating)
		}
	}
	if got := readFile(t, lib("delegating_user.ex")); !strings.Contains(got, "{lookup(id), find_one(id)}") {
		t.Errorf("delegating_user.ex changed its facade calls:\n%s", got)
	}
	// `import unquote(__MODULE__)` in a __using__ quote imports the module
	// itself into each module that uses it.
	headlessFn("Shop.Tools", "tool", "use_tool")
	if got := readFile(t, lib("tool_user.ex")); !strings.Contains(got, "do: use_tool(x)") || !strings.Contains(got, "|> use_tool()") {
		t.Errorf("tool_user.ex calls not renamed:\n%s", got)
	}
	// A type of the same name is not the function.
	if got := readFile(t, lib("tools.ex")); !strings.Contains(got, "@type tool :: term") || !strings.Contains(got, "@spec use_tool(tool) :: tool") {
		t.Errorf("tools.ex after the rename:\n%s", got)
	}
	headlessFn("Shop.Accounts.Helpers", "wrap", "wrap_ok")
	headlessMod("Shop.Accounts.Token", "Shop.Accounts.Ticket")
	headlessMod("Shop.Repo", "Shop.Storage")
	headlessFn("Shop.Accounts", "load", "fetch")

	// A project behaviour's callback family: the @callback, the
	// implementations (Shop.Job by @behaviour, Shop.Mailer by a `use` whose
	// __using__ injects the @behaviour), their @spec lines, the calls, the
	// captures, and the calls through a variable in the behaviour.
	headlessFn("Shop.Worker", "perform", "execute")
	for path, want := range map[string]string{
		lib("worker.ex"): "impl.execute(arg)", lib("job.ex"): "def execute(_arg)",
		lib("mailer.ex"): "def execute(arg)", lib("scheduler.ex"): "&Mailer.execute/1",
	} {
		if got := readFile(t, path); !strings.Contains(got, want) {
			t.Errorf("%s does not have %q:\n%s", path, want, got)
		}
	}
	// The same family from an implementation, with @optional_callbacks.
	headlessFn("Shop.Job", "cancel", "abort")
	if got := readFile(t, lib("worker.ex")); !strings.Contains(got, "@optional_callbacks abort: 1") {
		t.Errorf("@optional_callbacks not renamed:\n%s", got)
	}
	// A callback whose implementation has a default argument: go/2 with
	// `opts \\ []` also defines go/1, whose calls change with it; the
	// unrelated go/3 stays.
	headlessFn("Shop.Worker", "go", "walk")
	sched := readFile(t, lib("scheduler.ex"))
	for _, want := range []string{"Job.walk(5)", "5 |> Job.walk()", "&Job.walk/1", "Job.walk(1, [])", "Job.go(1, 2, 3)"} {
		if !strings.Contains(sched, want) {
			t.Errorf("scheduler.ex does not have %q:\n%s", want, sched)
		}
	}
	// Arities per member (D.*): ImplA's `go(x, opts \\ [])` adds go/1 to
	// ImplA only; ImplB's own go/1 and ImplBoth's go/1 (a callback of
	// D.Other) stay.
	headlessFn("D.ImplA", "go", "walk")
	user := readFile(t, filepath.Join(dir, "lib", "d", "user.ex"))
	for _, want := range []string{
		"ImplA.walk(1)", "ImplA.walk(1, [])", "1 |> ImplA.walk()", "&ImplA.walk/1",
		"ImplB.go(1)", "ImplB.walk(1, [])", "&ImplB.go/1", "1 |> ImplB.go()",
		"ImplBoth.go(1)", "ImplBoth.walk(1, [])", "&ImplBoth.go/1", "&ImplBoth.walk/2",
	} {
		if !strings.Contains(user, want) {
			t.Errorf("user.ex does not have %q:\n%s", want, user)
		}
	}
	headlessFn("D.Beh", "walk", "go")
	headlessFn("D.ImplB", "go", "run")
	headlessFn("D.Beh", "run", "go")

	// A definition whose default arguments reach into another behaviour's
	// callback belongs to two families: the rename is refused.
	cross := filepath.Join(dir, "lib", "d", "impl_cross.ex")
	writeFile(t, cross, "defmodule D.ImplCross do\n  @moduledoc false\n  @behaviour D.Beh\n  @behaviour D.Other\n\n  @impl true\n  def go(x, opts \\\\ []), do: {x, opts}\nend\n")
	_ = editor.ReconcileFile(cross)
	compile("with D.ImplCross")
	for _, from := range []string{"D.Beh", "D.Other", "D.ImplA"} {
		if _, err := editor.RenameFunctionHeadless(ctx, from, "go", "walk"); err == nil ||
			!strings.Contains(err.Error(), "belongs to two callback families") {
			t.Fatalf("rename %s.go: want the two-families refusal, got %v", from, err)
		}
	}
	if err := os.Remove(cross); err != nil {
		t.Fatal(err)
	}
	editor.RemoveFile(cross)
	after("refused two-family renames", nil)

	// A rename to the name of another callback of the same behaviour, which
	// no member defines, is refused: the behaviour would lose a callback.
	beh4 := filepath.Join(dir, "lib", "d", "beh4.ex")
	const beh4Text = "defmodule D.Beh4 do\n  @moduledoc false\n  @callback a(term) :: term\n  @macrocallback m(term) :: Macro.t()\n  @callback b(term) :: term\n  @optional_callbacks b: 1, m: 1\nend\n"
	writeFile(t, beh4, beh4Text)
	_ = editor.ReconcileFile(beh4)
	compile("with D.Beh4")
	for _, to := range []string{"b", "m"} {
		if _, err := editor.RenameFunctionHeadless(ctx, "D.Beh4", "a", to); err == nil ||
			!strings.Contains(err.Error(), "callback D.Beh4."+to+"/1 already exists") {
			t.Fatalf("rename D.Beh4.a to %s: want the refusal, got %v", to, err)
		}
	}
	if got := readFile(t, beh4); got != beh4Text {
		t.Errorf("beh4.ex changed:\n%s", got)
	}
	after("refused callback-name collision", nil)

	// An implementation of a behaviour from a dependency is refused.
	if _, err := editor.RenameFunctionHeadless(ctx, "Shop.Plugin", "handle", "process"); err == nil ||
		!strings.Contains(err.Error(), "handle/1 implements the DepBehaviour behaviour from a dependency; Dexter cannot rename it") {
		t.Fatalf("rename Shop.Plugin.handle: want the dependency refusal, got %v", err)
	}
	after("refused dependency callback", nil)
	// A module rename of the behaviour: @behaviour lines (also in a
	// __using__ quote) and @impl Mod lines follow it.
	headlessMod("Shop.Worker", "Shop.Runner")
	headlessMod("Shop.Runner", "Shop.Worker")

	// A callback family through the editor, from the implementation that
	// is open.
	job := lib("job.ex")
	openIn(t, editor, job, readFile(t, job))
	line, col := position(t, readFile(t, job), "def execute", "execute")
	edit, err := editor.RenameEdit(ctx, renameParams(string(uri.File(job)), line, col, "perform"))
	if err != nil {
		t.Fatal(err)
	}
	applyAndSave(t, editor, edit)
	after("rename the Shop.Worker.execute family in the editor", nil)

	// Through the editor: web.ex is open; the reply is applied to the buffer
	// as an editor applies it, and the buffer is saved.
	web := lib("web.ex")
	openIn(t, editor, web, readFile(t, web))
	line, col = position(t, readFile(t, web), "Acc.count()", "count")
	edit, err = editor.RenameEdit(ctx, renameParams(string(uri.File(web)), line, col, "total"))
	if err != nil {
		t.Fatal(err)
	}
	applyAndSave(t, editor, edit)
	after("rename Shop.Accounts.count in the editor", nil)

	// A module rename that moves a file the editor has open: the editor
	// moves it.
	facade := lib("facade.ex")
	openIn(t, editor, facade, readFile(t, facade))
	line, col = position(t, readFile(t, facade), "defmodule Shop.Facade", "Facade")
	edit, err = editor.RenameEdit(ctx, renameParams(string(uri.File(facade)), line, col, "Front"))
	if err != nil {
		t.Fatal(err)
	}
	applyAndSave(t, editor, edit)
	if _, err := os.Stat(lib("front.ex")); err != nil {
		t.Fatalf("facade.ex was not moved to front.ex: %v", err)
	}
	after("rename Shop.Facade in the editor", nil)

	// Module renames into a deeper, a shallower, and another namespace,
	// headless and in the editor. The spellings through an alias follow the
	// alias (lib/ren/user_*.ex), and the files move to the conventional path
	// for the new name.
	ren := func(rel string) string { return filepath.Join(dir, "lib", "ren", filepath.FromSlash(rel)) }
	expect := func(step, rel string, want ...string) {
		t.Helper()
		got := readFile(t, ren(rel))
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %s does not have %q:\n%s", step, rel, w, got)
			}
		}
	}
	exists := func(step string, rels ...string) {
		t.Helper()
		for _, rel := range rels {
			if _, err := os.Stat(ren(rel)); err != nil {
				t.Errorf("%s: %s: %v", step, rel, err)
			}
		}
	}
	gone := func(step string, rels ...string) {
		t.Helper()
		for _, rel := range rels {
			if _, err := os.Stat(ren(rel)); !os.IsNotExist(err) {
				t.Errorf("%s: %s still exists (%v)", step, rel, err)
			}
		}
	}
	editorMod := func(rel, marker, token, newName string) {
		t.Helper()
		path := ren(rel)
		openIn(t, editor, path, readFile(t, path))
		line, col := position(t, readFile(t, path), marker, token)
		edit, err := editor.RenameEdit(ctx, renameParams(string(uri.File(path)), line, col, newName))
		if err != nil {
			t.Fatalf("rename %s in %s: %v", token, rel, err)
		}
		applyAndSave(t, editor, edit)
		after("rename "+token+" to "+newName+" in the editor", nil)
	}

	step := "Ren.Probe.Mcp1 to Ren.Probe.Renamed.Mcp"
	headlessMod("Ren.Probe.Mcp1", "Ren.Probe.Renamed.Mcp")
	exists(step, "probe/renamed/mcp.ex", "probe/renamed/mcp/child.ex")
	gone(step, "probe/mcp1.ex", "probe/mcp1")
	expect(step, "user_alias.ex", "alias Ren.Probe.Renamed.Mcp\n", "alias Ren.Probe.Renamed.Mcp.Child\n", "{Mcp.hello(), Mcp.Child.x(), Child.x()}")
	expect(step, "user_ancestor.ex", "alias Ren.Probe\n", "Probe.Renamed.Mcp.hello()")
	expect(step, "chars.ex", "defimpl String.Chars, for: Ren.Probe.Renamed.Mcp do")
	expect(step, "chars_user.ex", "Chars.Ren.Probe.Renamed.Mcp.to_string(%Ren.Probe.Renamed.Mcp{})")
	expect(step, "user_group.ex", "alias Ren.Probe.{Other}\n  alias Ren.Probe.Renamed.Mcp\n", "alias Ren.Probe.{Deep.Mod}\n  alias Ren.Probe.Renamed.Mcp.Child\n", "{Mcp.hello(), Other.y()")
	// Ren.Other.Mcp has the short name Mcp in this file: the alias keeps Mcp1.
	expect(step, "user_collision.ex", "alias Ren.Probe.Renamed.Mcp, as: Mcp1\n", "{Mcp1.hello(), Mcp.w()}")

	// Back: shallower, in the same namespace.
	step = "Ren.Probe.Renamed.Mcp to Ren.Probe.Mcp1"
	headlessMod("Ren.Probe.Renamed.Mcp", "Ren.Probe.Mcp1")
	exists(step, "probe/mcp1.ex", "probe/mcp1/child.ex")
	gone(step, "probe/renamed")
	expect(step, "user_alias.ex", "alias Ren.Probe.Mcp1\n", "{Mcp1.hello(), Mcp1.Child.x(), Child.x()}")
	expect(step, "user_ancestor_kept.ex", "{Probe.Mcp1.hello(), Probe.Mcp1.Child.x(), Probe.Other.y()}")

	// Out of the namespace that an alias names: the full new name, and an
	// alias with no use left goes.
	step = "Ren.Probe.Mcp1 to Ren.Elsewhere.Mcp1"
	headlessMod("Ren.Probe.Mcp1", "Ren.Elsewhere.Mcp1")
	exists(step, "elsewhere/mcp1.ex", "elsewhere/mcp1/child.ex", "probe/other.ex")
	gone(step, "probe/mcp1.ex", "probe/mcp1")
	expect(step, "user_ancestor.ex", "def run, do: Ren.Elsewhere.Mcp1.hello()")
	if got := readFile(t, ren("user_ancestor.ex")); strings.Contains(got, "alias Ren.Probe") {
		t.Errorf("%s: the unused alias stays:\n%s", step, got)
	}
	expect(step, "user_ancestor_kept.ex", "alias Ren.Probe\n", "{Ren.Elsewhere.Mcp1.hello(), Ren.Elsewhere.Mcp1.Child.x(), Probe.Other.y()}")
	expect(step, "user_alias.ex", "alias Ren.Elsewhere.Mcp1\n", "{Mcp1.hello(), Mcp1.Child.x(), Child.x()}")

	headlessMod("Ren.Elsewhere.Mcp1", "Ren.Probe.Mcp1")
	exists("back to Ren.Probe.Mcp1", "probe/mcp1.ex", "probe/mcp1/child.ex")
	gone("back to Ren.Probe.Mcp1", "elsewhere")

	// Shallower, out of the namespace.
	step = "Ren.Probe.Mcp1 to Ren.Top"
	headlessMod("Ren.Probe.Mcp1", "Ren.Top")
	exists(step, "top.ex", "top/child.ex")
	expect(step, "user_alias.ex", "alias Ren.Top\n", "alias Ren.Top.Child\n", "{Top.hello(), Top.Child.x(), Child.x()}")
	expect(step, "user_ancestor_kept.ex", "{Ren.Top.hello(), Ren.Top.Child.x(), Probe.Other.y()}")
	expect(step, "chars.ex", "for: Ren.Top do")
	expect(step, "chars_user.ex", "Chars.Ren.Top.to_string(%Ren.Top{})")

	// A member with two segments in a group.
	step = "Ren.Probe.Deep.Mod to Ren.Mod"
	headlessMod("Ren.Probe.Deep.Mod", "Ren.Mod")
	exists(step, "mod.ex")
	gone(step, "probe/deep")
	expect(step, "user_group.ex", "alias Ren.Mod\n", "Mod.z()")

	// A nested defmodule: the new name is written relative to the enclosing
	// module. A rename out of that module, or one that adds a segment, is
	// refused.
	for _, to := range []string{"Ren.Inner", "Ren.Outer.Deep.Inner"} {
		if _, err := editor.RenameModuleHeadless(ctx, "Ren.Outer.Inner", to); err == nil ||
			!strings.Contains(err.Error(), "Ren.Outer.Inner is defined inside Ren.Outer (lib/ren/outer.ex:3)") {
			t.Fatalf("rename of a nested module to %s: want the refusal, got %v", to, err)
		}
	}
	step = "Ren.Outer.Inner to Ren.Outer.Inner2"
	headlessMod("Ren.Outer.Inner", "Ren.Outer.Inner2")
	expect(step, "outer.ex", "defmodule Inner2 do", "{Inner2.i(), %{inner: Inner2}}")
	expect(step, "user_outer.ex", "Ren.Outer.Inner2.i()")
	headlessMod("Ren.Outer", "Ren.Shell")
	expect("Ren.Outer to Ren.Shell", "shell.ex", "defmodule Ren.Shell do", "defmodule Inner2 do", "{Inner2.i(), %{inner: Inner2}}")
	expect("Ren.Outer to Ren.Shell", "user_outer.ex", "Ren.Shell.Inner2.i()")

	// In the editor, the new name replaces the last segment, and can add
	// segments: deeper, from a use through the alias.
	editorMod("user_alias.ex", "{Top.hello()", "Top", "Renamed.Mcp")
	step = "Ren.Top to Ren.Renamed.Mcp"
	exists(step, "renamed/mcp.ex", "renamed/mcp/child.ex")
	gone(step, "top.ex", "top")
	expect(step, "user_alias.ex", "alias Ren.Renamed.Mcp\n", "alias Ren.Renamed.Mcp.Child\n", "{Mcp.hello(), Mcp.Child.x(), Child.x()}")
	expect(step, "user_collision.ex", "alias Ren.Renamed.Mcp, as: Mcp1\n", "{Mcp1.hello(), Mcp.w()}")
	editorMod("user_group.ex", "Mod.z()", "Mod", "Module2")
	exists("Ren.Mod to Ren.Module2", "module2.ex")
	expect("Ren.Mod to Ren.Module2", "user_group.ex", "alias Ren.Module2\n", "Module2.z()")
}

// position returns the 0-based line and column of token in the first line of
// text that contains marker.
func position(t *testing.T, text, marker, token string) (uint32, uint32) {
	t.Helper()
	for i, line := range strings.Split(text, "\n") {
		if m := strings.Index(line, marker); m >= 0 {
			return uint32(i), uint32(m + strings.Index(marker, token))
		}
	}
	t.Fatalf("%q is not in the text", marker)
	return 0, 0
}

// applyAndSave applies a rename reply as an editor does: the text edits go to
// the buffers (the file text when it is not open), a rename operation moves
// the file, and every changed buffer is saved and reported.
func applyAndSave(t *testing.T, s *Server, edit *WorkspaceEdit) {
	t.Helper()
	ctx := context.Background()
	save := func(path, text string) {
		writeFile(t, path, text)
		_ = s.ReconcileFile(path)
	}
	editFile := func(u protocol.DocumentURI, edits []protocol.TextEdit) {
		path := uri.URI(u).Filename()
		text, ok := s.docs.GetIfOpen(string(u))
		if !ok {
			text = readFile(t, path)
		}
		text = bufferAfterEdits(t, &WorkspaceEdit{Changes: map[protocol.DocumentURI][]protocol.TextEdit{pathToURI(path): edits}}, path, text)
		if _, open := s.docs.GetIfOpen(string(u)); open {
			s.docs.Set(string(u), text)
		}
		save(path, text)
	}
	for u, edits := range edit.Changes {
		editFile(u, edits)
	}
	for _, change := range edit.DocumentChanges {
		switch c := change.(type) {
		case TextDocumentEdit:
			editFile(c.TextDocument.URI, c.Edits)
		case RenameFile:
			from, to := uri.URI(c.OldURI).Filename(), uri.URI(c.NewURI).Filename()
			text := readFile(t, from)
			if err := os.Remove(from); err != nil {
				t.Fatal(err)
			}
			s.RemoveFile(from)
			_ = s.DidClose(ctx, &protocol.DidCloseTextDocumentParams{TextDocument: protocol.TextDocumentIdentifier{URI: c.OldURI}})
			openIn(t, s, to, text)
			save(to, text)
		}
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		target := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func elixirFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, root := range []string{"lib", "deps"} {
		_ = filepath.WalkDir(filepath.Join(dir, root), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() != "mix.exs" && parser.IsElixirFile(p) && !strings.Contains(p, "_build") {
				out = append(out, p)
			}
			return nil
		})
	}
	sort.Strings(out)
	return out
}

// expectIndexIsColdBuild checks that st holds exactly what a cold build of
// the project's files gives.
func expectIndexIsColdBuild(t *testing.T, st *store.Store, dir string) {
	t.Helper()
	files := elixirFiles(t, dir)
	rels := make([]string, len(files))
	for i, f := range files {
		rels[i], _ = filepath.Rel(dir, f)
	}
	expectColdBuild(t, st, dir, rels...)
	got, _ := st.FileStates()
	if len(got) != len(files) {
		var paths []string
		for p := range got {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		t.Errorf("the index has %d files, the project %d: %v", len(got), len(files), paths)
	}
}
