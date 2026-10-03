package beam

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testDefinition is one entry of the definitions list in a synthetic Dbgi chunk.
type testDefinition struct {
	name  string
	arity int
	kind  string // def, defp, defmacro, defmacrop
	line  int    // the :line meta; zero omits it

	// keepFile/keepLine write a `file: {path, line}` meta entry, which is what
	// both `@file {path, line}` and `quote location: :keep` leave on a def.
	keepFile string
	keepLine int
}

// buildDebugInfoTerm encodes {:debug_info_v1, :elixir_erl, {:elixir_v1, map, specs}}
// the way the Elixir compiler writes a Dbgi chunk. Each definition carries a
// clause body so the reader has to step over real AST shapes to reach the next.
func buildDebugInfoTerm(file, relativeFile string, definitions ...testDefinition) []byte {
	return buildDebugInfoTermWithModuleLine(file, relativeFile, "", 0, definitions...)
}

// buildDebugInfoTermWithModuleLine also writes the module's own line under
// moduleKey: "anno" as Elixir 1.18 and later do, or "line" as earlier
// versions do. An empty key writes none.
func buildDebugInfoTermWithModuleLine(file, relativeFile, moduleKey string, moduleLine int, definitions ...testDefinition) []byte {
	var w etfTestWriter
	w.smallTuple(3)
	w.atom("debug_info_v1")
	w.atom("elixir_erl")
	w.smallTuple(3)
	w.atom("elixir_v1")

	// Keys in the order ERTS sorts a small map's atom keys, which puts
	// definitions before file and relative_file: the reader must not depend on
	// having seen the file first.
	if moduleKey != "" {
		w.mapHeader(5)
	} else {
		w.mapHeader(4)
	}
	w.atom("attributes")
	w.nil()
	w.atom("definitions")
	w.listHeader(len(definitions))
	for _, definition := range definitions {
		w.smallTuple(4)
		w.smallTuple(2)
		w.atom(definition.name)
		w.smallInt(definition.arity)
		w.atom(definition.kind)

		pairs := 1 // generated: true
		if definition.line > 0 {
			pairs++
		}
		if definition.keepFile != "" {
			pairs++
		}
		w.listHeader(pairs)
		if definition.line > 0 {
			w.smallTuple(2)
			w.atom("line")
			w.smallInt(definition.line)
		}
		if definition.keepFile != "" {
			w.smallTuple(2)
			w.atom("file")
			w.smallTuple(2)
			w.binary(definition.keepFile)
			w.smallInt(definition.keepLine)
		}
		w.smallTuple(2)
		w.atom("generated")
		w.atom("true")
		w.nil()

		// One clause, {meta, args, guards, body}, with a body shaped like a
		// remote call: {{:., [], [Mod, :fun]}, [line: 1], [arg]}.
		w.listHeader(1)
		w.smallTuple(4)
		w.nil()
		w.listHeader(1)
		w.smallTuple(3)
		w.atom("value")
		w.nil()
		w.atom("nil")
		w.nil()
		w.nil()
		w.smallTuple(3)
		w.smallTuple(3)
		w.atom(".")
		w.nil()
		w.listHeader(2)
		w.atom("Elixir.Enum")
		w.atom("map")
		w.nil()
		w.nil()
		w.listHeader(1)
		w.binary("a string literal in the body")
		w.nil()
		w.nil()
	}
	w.nil()
	w.atom("file")
	w.binary(file)
	w.atom("relative_file")
	w.binary(relativeFile)
	if moduleKey != "" {
		w.atom(moduleKey)
		w.smallInt(moduleLine)
	}

	w.nil() // specs
	return w.buf
}

func writeDebugInfoBEAM(t *testing.T, path string, dbgi []byte) {
	t.Helper()
	writeTestBEAMOpts(t, path, testBEAMOptions{
		atomNames: defaultTestAtoms,
		exports:   defaultTestExports,
		dbgi:      dbgi,
	})
}

// A generator that stamps `@file {path, line}` on the functions it defines
// points each one at the source line that asked for it, such as an Ash
// `define :get_by_slug` inside a domain. When that path is the module's own
// source, that line is the definition.
func TestReadDefinitionLinesPrefersOwnFileLocation(t *testing.T) {
	beamPath := filepath.Join(t.TempDir(), "Elixir.MyApp.Accounts.beam")
	writeDebugInfoBEAM(t, beamPath, buildDebugInfoTerm(
		"/src/my_app/lib/my_app/accounts.ex", "lib/my_app/accounts.ex",
		testDefinition{name: "get_user!", arity: 1, kind: "def", line: 1, keepFile: "lib/my_app/accounts.ex", keepLine: 7},
		testDefinition{name: "get_user!", arity: 2, kind: "def", line: 1, keepFile: "lib/my_app/accounts.ex", keepLine: 7},
	))

	info, err := ReadDefinitionLines(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.File != "/src/my_app/lib/my_app/accounts.ex" || info.RelativeFile != "lib/my_app/accounts.ex" {
		t.Fatalf("source = %q / %q", info.File, info.RelativeFile)
	}
	for _, arity := range []int{1, 2} {
		if got := info.Lines[FunctionKey{Name: "get_user!", Arity: arity}]; got != 7 {
			t.Errorf("get_user!/%d line = %d, want 7", arity, got)
		}
	}
}

// Elixir 1.17 and earlier record the module's line under :line, and later
// versions under :anno.
func TestReadDefinitionLinesModuleLineKeys(t *testing.T) {
	for _, key := range []string{"anno", "line"} {
		t.Run(key, func(t *testing.T) {
			beamPath := filepath.Join(t.TempDir(), "Elixir.MyApp.Accounts.beam")
			writeDebugInfoBEAM(t, beamPath, buildDebugInfoTermWithModuleLine(
				"/src/my_app/lib/my_app/accounts.ex", "lib/my_app/accounts.ex", key, 3,
				testDefinition{name: "get_user", arity: 1, kind: "def", line: 5},
			))
			info, err := ReadDefinitionLines(beamPath)
			if err != nil {
				t.Fatal(err)
			}
			if info.ModuleLine != 3 {
				t.Errorf("ModuleLine = %d, want 3", info.ModuleLine)
			}
		})
	}
}

// A location in the module's own file at or before the module's line is a
// `location: :keep` quote in a macro defined above the module, not a stamp
// that names where the module asked for the function, so :line is used.
func TestReadDefinitionLinesOwnFileLocationBeforeModule(t *testing.T) {
	beamPath := filepath.Join(t.TempDir(), "Elixir.MyApp.Accounts.beam")
	writeDebugInfoBEAM(t, beamPath, buildDebugInfoTermWithModuleLine(
		"/src/my_app/lib/my_app/accounts.ex", "lib/my_app/accounts.ex", "anno", 9,
		testDefinition{name: "kept", arity: 0, kind: "def", line: 12, keepFile: "lib/my_app/accounts.ex", keepLine: 4},
		testDefinition{name: "stamped", arity: 0, kind: "def", line: 9, keepFile: "lib/my_app/accounts.ex", keepLine: 14},
	))
	info, err := ReadDefinitionLines(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int{"kept": 12, "stamped": 14} {
		if got := info.Lines[FunctionKey{Name: name, Arity: 0}]; got != want {
			t.Errorf("%s/0 line = %d, want %d", name, got, want)
		}
	}
}

// `quote location: :keep` inside a library records the library's own file.
// That line belongs to the generator, not to the module, so the module-side
// :line is used instead.
func TestReadDefinitionLinesIgnoresForeignFileLocation(t *testing.T) {
	beamPath := filepath.Join(t.TempDir(), "Elixir.MyApp.Accounts.beam")
	writeDebugInfoBEAM(t, beamPath, buildDebugInfoTerm(
		"/src/my_app/lib/my_app/accounts.ex", "lib/my_app/accounts.ex",
		testDefinition{name: "list_users", arity: 0, kind: "def", line: 12, keepFile: "deps/shared_lib/lib/shared_lib/interface.ex", keepLine: 1112},
		testDefinition{name: "count_users", arity: 0, kind: "def", line: 1, keepFile: "deps/shared_lib/lib/shared_lib/interface.ex", keepLine: 1112},
	))

	info, err := ReadDefinitionLines(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Lines[FunctionKey{Name: "list_users", Arity: 0}]; got != 12 {
		t.Errorf("list_users/0 line = %d, want the module-side line 12", got)
	}
	if got := info.Lines[FunctionKey{Name: "count_users", Arity: 0}]; got != 1 {
		t.Errorf("count_users/0 line = %d, want 1", got)
	}
}

func TestReadDefinitionLinesPlainLineAndMacros(t *testing.T) {
	beamPath := filepath.Join(t.TempDir(), "Elixir.MyApp.Worker.beam")
	writeDebugInfoBEAM(t, beamPath, buildDebugInfoTerm(
		"/src/my_app/lib/my_app/worker.ex", "lib/my_app/worker.ex",
		testDefinition{name: "new", arity: 1, kind: "def", line: 2},
		testDefinition{name: "build", arity: 1, kind: "defmacro", line: 9},
		testDefinition{name: "helper", arity: 0, kind: "defp", line: 4},
		testDefinition{name: "no_line", arity: 0, kind: "def"},
	))

	info, err := ReadDefinitionLines(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[FunctionKey]int{
		{Name: "new", Arity: 1}:   2,
		{Name: "build", Arity: 1}: 9,
	}
	if len(info.Lines) != len(want) {
		t.Fatalf("lines = %v, want only public definitions with a line %v", info.Lines, want)
	}
	for key, line := range want {
		if info.Lines[key] != line {
			t.Errorf("%v line = %d, want %d", key, info.Lines[key], line)
		}
	}
}

func TestReadDefinitionLinesRejectsMissingOrForeignDebugInfo(t *testing.T) {
	dir := t.TempDir()

	noChunk := filepath.Join(dir, "Elixir.NoChunk.beam")
	writeTestBEAM(t, noChunk, buildDocsTerm())
	if _, err := ReadDefinitionLines(noChunk); err == nil {
		t.Error("expected an error for a BEAM without a Dbgi chunk")
	}

	// `debug_info: false` still writes the chunk, with :none as the payload.
	var none etfTestWriter
	none.smallTuple(3)
	none.atom("debug_info_v1")
	none.atom("elixir_erl")
	none.atom("none")
	stripped := filepath.Join(dir, "Elixir.Stripped.beam")
	writeDebugInfoBEAM(t, stripped, none.buf)
	if _, err := ReadDefinitionLines(stripped); err == nil {
		t.Error("expected an error for stripped debug info")
	}

	// An Erlang module's abstract code is a different backend entirely.
	var erlang etfTestWriter
	erlang.smallTuple(3)
	erlang.atom("debug_info_v1")
	erlang.atom("erl_abstract_code")
	erlang.smallTuple(2)
	erlang.atom("none")
	erlang.nil()
	erlangBeam := filepath.Join(dir, "erlang_mod.beam")
	writeDebugInfoBEAM(t, erlangBeam, erlang.buf)
	if _, err := ReadDefinitionLines(erlangBeam); err == nil {
		t.Error("expected an error for a non-Elixir debug info backend")
	}
}

// A corrupt build artifact must fail the read, never the server.
func TestReadDefinitionLinesTruncated(t *testing.T) {
	full := buildDebugInfoTerm("/src/a.ex", "a.ex",
		testDefinition{name: "run", arity: 0, kind: "def", line: 3, keepFile: "a.ex", keepLine: 5})
	dir := t.TempDir()
	for cut := 1; cut < len(full); cut += 7 {
		path := filepath.Join(dir, "Elixir.Cut.beam")
		writeDebugInfoBEAM(t, path, full[:cut])
		if _, err := ReadDefinitionLines(path); err == nil {
			t.Fatalf("expected an error when the term is cut at %d of %d bytes", cut, len(full))
		}
	}
}

// Clause bodies are arbitrary AST and nest far deeper than a Docs chunk. A long
// pipeline or nested case is stepped over without recursion, so it costs no
// stack and does not lose the module.
func TestReadDefinitionLinesDeepClauseBody(t *testing.T) {
	var w etfTestWriter
	w.smallTuple(3)
	w.atom("debug_info_v1")
	w.atom("elixir_erl")
	w.smallTuple(3)
	w.atom("elixir_v1")
	w.mapHeader(2)
	w.atom("definitions")
	w.listHeader(2)
	for i, name := range []string{"deep", "after_deep"} {
		w.smallTuple(4)
		w.smallTuple(2)
		w.atom(name)
		w.smallInt(0)
		w.atom("def")
		w.listHeader(1)
		w.smallTuple(2)
		w.atom("line")
		w.smallInt(10 + i)
		w.nil()
		// 100,000 nested {:|>, [], [left, ...]} levels: 200,000 ETF levels.
		const depth = 100_000
		for range depth {
			w.smallTuple(3)
			w.atom("|>")
			w.nil()
			w.listHeader(1)
		}
		w.atom("nil")
		for range depth {
			w.nil()
		}
	}
	w.nil()
	w.atom("relative_file")
	w.binary("lib/deep.ex")
	w.nil()

	path := filepath.Join(t.TempDir(), "Elixir.Deep.beam")
	writeDebugInfoBEAM(t, path, w.buf)
	info, err := ReadDefinitionLines(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Lines[FunctionKey{Name: "after_deep", Arity: 0}] != 11 {
		t.Fatalf("definition after a deep body = %v, want line 11", info.Lines)
	}
}

// The synthetic fixtures encode what the compiler is believed to write. This
// compiles real modules so a change in Elixir's debug info layout fails here
// instead of silently sending every definition back to the module line.
func TestReadDefinitionLinesFromElixirCompiler(t *testing.T) {
	elixirc, err := exec.LookPath("elixirc")
	if err != nil {
		t.Skip("elixirc not installed")
	}
	dir := t.TempDir()
	write := func(name, source string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// SharedLib.Interface generates functions three ways: with `@file` pointing
	// at the line of the call that asked for it (as a DSL records it), from a
	// `location: :keep` quote, and plainly.
	write("interface.ex", `defmodule SharedLib.Interface do
  defmacro define(name) do
    line = __CALLER__.line

    quote bind_quoted: [name: name, line: line], location: :keep do
      @file {__ENV__.file, line}
      def unquote(name)(), do: :stamped
    end
  end

  defmacro define_kept(name) do
    quote bind_quoted: [name: name], location: :keep do
      def unquote(name)(), do: :kept
    end
  end

  defmacro define_plain(name) do
    quote do
      def unquote(name)(), do: :plain
    end
  end
end
`)
	write("accounts.ex", `defmodule MyApp.Accounts do
  require SharedLib.Interface

  SharedLib.Interface.define(:stamped)

  SharedLib.Interface.define_kept(:kept)

  SharedLib.Interface.define_plain(:plain)
end
`)
	cmd := exec.Command(elixirc, "-o", dir, "interface.ex", "accounts.ex")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("elixirc: %v\n%s", err, out)
	}

	info, err := ReadDefinitionLines(filepath.Join(dir, "Elixir.MyApp.Accounts.beam"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(info.File), "/accounts.ex") {
		t.Errorf("File = %q, want the compiled source", info.File)
	}
	want := map[string]int{"stamped": 4, "kept": 6, "plain": 8}
	for name, line := range want {
		if got := info.Lines[FunctionKey{Name: name, Arity: 0}]; got != line {
			t.Errorf("%s/0 line = %d, want %d (all: %v)", name, got, line, info.Lines)
		}
	}

	// The Docs anno is the fallback for a module without debug info, and the
	// compile info says which file it is in.
	source, ok := ReadSourcePath(filepath.Join(dir, "Elixir.MyApp.Accounts.beam"))
	if !ok || !strings.HasSuffix(filepath.ToSlash(source), "/accounts.ex") {
		t.Errorf("ReadSourcePath = %q, %v; want the compiled source", source, ok)
	}
	documented, err := ReadDocumentedFunctions(filepath.Join(dir, "Elixir.MyApp.Accounts.beam"))
	if err != nil {
		t.Fatal(err)
	}
	for _, function := range documented {
		if line, ok := want[function.Name]; ok && function.Line != line {
			t.Errorf("%s/0 Docs line = %d, want %d", function.Name, function.Line, line)
		}
	}
	if len(documented) != len(want) {
		t.Errorf("documented = %+v, want %d functions", documented, len(want))
	}
}

// A macro defined above its caller in the same file, with `location: :keep`,
// records that file and the line inside the macro. The call is the answer.
func TestReadDefinitionLinesSameFileKeepFromElixirCompiler(t *testing.T) {
	elixirc, err := exec.LookPath("elixirc")
	if err != nil {
		t.Skip("elixirc not installed")
	}
	dir := t.TempDir()
	source := `defmodule Same.Dsl do
  defmacro kept(name) do
    quote location: :keep do
      def unquote(name)(), do: :kept
    end
  end
end

defmodule Same.User do
  require Same.Dsl

  Same.Dsl.kept(:one)
end
`
	if err := os.WriteFile(filepath.Join(dir, "same.ex"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(elixirc, "-o", dir, "same.ex")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("elixirc: %v\n%s", err, out)
	}
	info, err := ReadDefinitionLines(filepath.Join(dir, "Elixir.Same.User.beam"))
	if err != nil {
		t.Fatal(err)
	}
	if info.ModuleLine != 9 {
		t.Errorf("ModuleLine = %d, want 9", info.ModuleLine)
	}
	if got := info.Lines[FunctionKey{Name: "one", Arity: 0}]; got != 12 {
		t.Errorf("one/0 line = %d, want 12, the call (all: %v)", got, info.Lines)
	}
}

// A corrupt definitions count must fail the parse, not allocate for it first.
func TestParseDebugInfoHugeDefinitionCount(t *testing.T) {
	var w etfTestWriter
	w.smallTuple(3)
	w.atom("debug_info_v1")
	w.atom("elixir_erl")
	w.smallTuple(3)
	w.atom("elixir_v1")
	w.mapHeader(1)
	w.atom("definitions")
	w.listHeader(1 << 30)
	if _, err := parseDebugInfo(w.buf); err == nil {
		t.Fatal("parseDebugInfo accepted a truncated definitions list")
	}
}
