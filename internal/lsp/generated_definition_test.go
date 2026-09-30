package lsp

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/beam"
	"github.com/remoteoss/dexter/internal/store"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// A DSL that declares functions inside a module, the way an Ash domain's
// `define` calls do. Nothing in the source is a def, so the index has no row for
// get_room_by_slug!; only the compiled module knows it exists.
const generatedDomainSource = `# The chat domain.
defmodule MyApp.Chat do
  use SharedLib.Domain

  resources do
    resource MyApp.Chat.Room do
      define :list_rooms, action: :read
      define :get_room_by_slug, action: :read, get_by: [:slug]
    end
  end

  def run_local, do: get_room_by_slug!("lounge")
end
`

const generatedCallerSource = `defmodule MyApp.Caller do
  alias MyApp.Chat

  def run do
    Chat.get_room_by_slug!("lounge")
  end
end
`

const (
	generatedDomainRel = "lib/my_app/chat.ex"
	generatedCallerRel = "lib/my_app/caller.ex"
	// generatedDefineLine is the 1-based line of `define :get_room_by_slug`.
	generatedDefineLine = 8
	// generatedModuleLine is the 1-based line of `defmodule MyApp.Chat`.
	generatedModuleLine = 2
)

// dbgiDefinition is one definition in a synthetic Dbgi chunk.
type dbgiDefinition struct {
	name     string
	arity    int
	line     int
	keepFile string
	keepLine int
}

// dbgiTerm encodes the {:debug_info_v1, :elixir_erl, {:elixir_v1, map, specs}}
// term the Elixir compiler writes, with empty clause lists.
func dbgiTerm(file, relativeFile string, definitions ...dbgiDefinition) []byte {
	var out []byte
	tuple := func(arity int) { out = append(out, 104, byte(arity)) }
	atom := func(name string) { out = append(out, etfAtom(name)...) }
	list := func(count int) { out = append(out, etfListHeader(count)...) }
	nilTerm := func() { out = append(out, 106) }
	integer := func(value int) {
		out = append(out, 98)
		out = binary.BigEndian.AppendUint32(out, uint32(value))
	}
	bin := func(text string) {
		out = append(out, 109)
		out = binary.BigEndian.AppendUint32(out, uint32(len(text)))
		out = append(out, text...)
	}

	tuple(3)
	atom("debug_info_v1")
	atom("elixir_erl")
	tuple(3)
	atom("elixir_v1")
	out = append(out, 116, 0, 0, 0, 3) // map with three pairs
	atom("definitions")
	list(len(definitions))
	for _, definition := range definitions {
		tuple(4)
		tuple(2)
		atom(definition.name)
		integer(definition.arity)
		atom("def")
		pairs := 1
		if definition.keepFile != "" {
			pairs++
		}
		list(pairs)
		tuple(2)
		atom("line")
		integer(definition.line)
		if definition.keepFile != "" {
			tuple(2)
			atom("file")
			tuple(2)
			bin(definition.keepFile)
			integer(definition.keepLine)
		}
		nilTerm()
		nilTerm() // clauses
	}
	nilTerm()
	atom("file")
	bin(file)
	atom("relative_file")
	bin(relativeFile)
	nilTerm() // specs
	return out
}

// newGeneratedDefinitionFixture indexes the domain and its caller, and writes
// the domain's BEAM with the given debug info.
func newGeneratedDefinitionFixture(t *testing.T, relativeFile string, definitions ...dbgiDefinition) (*Server, string) {
	t.Helper()
	return newGeneratedDefinitionFixtureWith(t, func(domainPath string) testBeamChunks {
		return testBeamChunks{dbgi: dbgiTerm(domainPath, relativeFile, definitions...)}
	})
}

// newGeneratedDefinitionFixtureWith is newGeneratedDefinitionFixture with the
// domain BEAM's chunks chosen by the test.
func newGeneratedDefinitionFixtureWith(t *testing.T, chunks func(domainPath string) testBeamChunks) (*Server, string) {
	t.Helper()
	server, cleanup := setupTestServer(t)
	t.Cleanup(cleanup)

	indexFile(t, server.store, server.projectRoot, generatedDomainRel, generatedDomainSource)
	indexFile(t, server.store, server.projectRoot, generatedCallerRel, generatedCallerSource)
	domainPath := filepath.Join(server.projectRoot, generatedDomainRel)
	writeGeneratedBeam(t, server, "MyApp.Chat", chunks(domainPath),
		beamExport{"run_local", 0},
		beamExport{"list_rooms", 0},
		beamExport{"get_room_by_slug!", 1},
		beamExport{"get_room_by_slug!", 2},
	)
	return server, domainPath
}

func writeGeneratedBeam(t *testing.T, server *Server, module string, chunks testBeamChunks, exports ...beamExport) {
	t.Helper()
	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	if err := os.MkdirAll(ebin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ebin, "Elixir."+module+".beam"), buildTestBeam(chunks, exports...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// docsEntry is one function in a synthetic docs_v1 term.
type docsEntry struct {
	name  string
	arity int
	anno  int
}

// docsTerm encodes the docs_v1 term the compiler writes in the Docs chunk. Each
// entry's anno is the line its def was expanded at.
func docsTerm(entries ...docsEntry) string {
	var out []byte
	tuple := func(arity int) { out = append(out, 104, byte(arity)) }
	atom := func(name string) { out = append(out, etfAtom(name)...) }
	emptyMap := func() { out = append(out, 116, 0, 0, 0, 0) }
	integer := func(value int) {
		out = append(out, 98)
		out = binary.BigEndian.AppendUint32(out, uint32(value))
	}
	bin := func(text string) {
		out = append(out, 109)
		out = binary.BigEndian.AppendUint32(out, uint32(len(text)))
		out = append(out, text...)
	}

	tuple(7)
	atom("docs_v1")
	integer(1)
	atom("elixir")
	bin("text/markdown")
	atom("none")
	emptyMap()
	out = append(out, etfListHeader(len(entries))...)
	for _, entry := range entries {
		tuple(5)
		tuple(3)
		atom("function")
		atom(entry.name)
		integer(entry.arity)
		integer(entry.anno)
		out = append(out, etfListHeader(1)...)
		bin(entry.name + "()")
		out = append(out, 106)
		atom("none")
		emptyMap()
	}
	out = append(out, 106)
	return string(out)
}

// stampedDefinitions is what a generator writes when it stamps each function
// with `@file {file, line}` for the DSL call that declared it: :line is where
// the before-compile hook ran, and the file entry names the declaring line.
func stampedDefinitions() []dbgiDefinition {
	return []dbgiDefinition{
		{name: "list_rooms", arity: 0, line: 1, keepFile: generatedDomainRel, keepLine: 7},
		{name: "get_room_by_slug!", arity: 1, line: 1, keepFile: generatedDomainRel, keepLine: generatedDefineLine},
		{name: "get_room_by_slug!", arity: 2, line: 1, keepFile: generatedDomainRel, keepLine: generatedDefineLine},
	}
}

// lineDefinitions is what a generator writes when it expands each function at
// the line of the DSL call that declared it, as Ash's code interfaces do: :line
// is the declaring line, and the file entry still names the generator.
func lineDefinitions() []dbgiDefinition {
	const generator = "deps/ash/lib/ash/code_interface.ex"
	return []dbgiDefinition{
		{name: "list_rooms", arity: 0, line: 7, keepFile: generator, keepLine: 1112},
		{name: "get_room_by_slug!", arity: 1, line: generatedDefineLine, keepFile: generator, keepLine: 1112},
		{name: "get_room_by_slug!", arity: 2, line: generatedDefineLine, keepFile: generator, keepLine: 1112},
	}
}

func generatedDefinitionAt(t *testing.T, server *Server, rel string, source string, line, character int) []protocol.Location {
	t.Helper()
	docURI := string(uri.File(filepath.Join(server.projectRoot, rel)))
	server.docs.Set(docURI, source)
	locations, err := server.Definition(context.Background(), &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
			Position:     protocol.Position{Line: uint32(line), Character: uint32(character)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return locations
}

func expectSingleLocation(t *testing.T, locations []protocol.Location, path string, line int) {
	t.Helper()
	if len(locations) != 1 {
		t.Fatalf("expected one location at %s:%d, got %#v", path, line, locations)
	}
	if got := uriToPath(locations[0].URI); got != path || int(locations[0].Range.Start.Line) != line-1 {
		t.Fatalf("definition = %s:%d, want %s:%d", got, locations[0].Range.Start.Line+1, path, line)
	}
}

// Issue #108: a function a DSL generated resolved to the top of its module
// even when the compiled module records the line that declared it.
func TestDefinitionGeneratedFunctionUsesDebugInfoLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, stampedDefinitions()...)
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedDefineLine)
}

func TestDefinitionBareGeneratedFunctionUsesDebugInfoLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, stampedDefinitions()...)
	locations := generatedDefinitionAt(t, server, generatedDomainRel, generatedDomainSource, 11, 24)
	expectSingleLocation(t, locations, domainPath, generatedDefineLine)
}

func TestDefinitionGeneratedFunctionUsesLineWhenFileIsGenerator(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, lineDefinitions()...)
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedDefineLine)
}

// `dexter lookup` resolves through LookupName, and a recorded line is the
// function's own definition, so a strict lookup takes it too.
func TestLookupNameGeneratedFunctionUsesDebugInfoLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts NameLookupOptions
	}{
		{"fallback", NameLookupOptions{FallbackToModule: true}},
		{"strict", NameLookupOptions{ExactModule: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, lineDefinitions()...)
			locations, err := server.LookupName("MyApp.Chat", "get_room_by_slug!", tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(locations) != 1 || locations[0].FilePath != domainPath || locations[0].Line != generatedDefineLine {
				t.Fatalf("expected %s:%d, got %#v", domainPath, generatedDefineLine, locations)
			}
		})
	}
}

// Without a recorded line, the call that declared the function is found in
// the module, and a strict lookup takes it: it is the function's own place.
func TestLookupNameStrictGeneratedFunctionWithoutLineFindsDeclaration(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel,
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 1, keepFile: "deps/shared_lib/lib/interface.ex", keepLine: 1112},
	)
	locations, err := server.LookupName("MyApp.Chat", "get_room_by_slug!", NameLookupOptions{ExactModule: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || locations[0].FilePath != domainPath || locations[0].Line != generatedDefineLine {
		t.Fatalf("expected %s:%d, got %#v", domainPath, generatedDefineLine, locations)
	}
}

// A library's `quote location: :keep` names the library's own file. That is
// the generator, not the declaration, and :line only says where the
// before-compile hook ran. The module's body still has the declaring call.
func TestDefinitionGeneratedFunctionForeignLocationFindsDeclaration(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel,
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 1, keepFile: "deps/shared_lib/lib/interface.ex", keepLine: 1112},
	)
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedDefineLine)
}

// Dexter cannot compile the project, so a BEAM older than its source is the
// usual state while editing. Its line is from the last compile, which is
// still closer than the module line.
func TestDefinitionGeneratedFunctionStaleBEAMUsesRecordedLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, stampedDefinitions()...)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(domainPath, future, future); err != nil {
		t.Fatal(err)
	}
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedDefineLine)
}

// A module compiled without debug info still has its Docs chunk, whose anno is
// the line the def was expanded at. The compile info says which file that line
// is in.
func TestDefinitionGeneratedFunctionUsesDocsLineWithoutDebugInfo(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixtureWith(t, func(domainPath string) testBeamChunks {
		return testBeamChunks{
			docs:   docsTerm(docsEntry{name: "get_room_by_slug!", arity: 1, anno: generatedDefineLine}),
			source: domainPath,
		}
	})
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedDefineLine)
}

// Without compile info, a Docs anno does not say which file it is in.
func TestDefinitionGeneratedFunctionDocsLineWithoutSourceKeepsModuleLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixtureWith(t, func(string) testBeamChunks {
		return testBeamChunks{docs: docsTerm(docsEntry{name: "get_room_by_slug!", arity: 1, anno: generatedDefineLine})}
	})
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedModuleLine)
}

// sourcelessModule is a generated module with no source row, as Spark creates
// for each DSL entity. It was compiled from a framework file on another machine.
const (
	sourcelessModule   = "MyApp.Chat.Define"
	sourcelessRecorded = "/build/agent/deps/shared_lib/lib/shared_lib/dsl/extension.ex"
)

// The recorded path does not exist here, so it is rebased onto the project's
// deps, where the same file is.
func TestLookupNameSourcelessGeneratedModuleUsesCompiledSource(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks testBeamChunks
	}{
		{"debug info", testBeamChunks{
			source: sourcelessRecorded,
			dbgi:   dbgiTerm(sourcelessRecorded, "lib/shared_lib/dsl/extension.ex", dbgiDefinition{name: "build", arity: 1, line: 42}),
		}},
		{"docs", testBeamChunks{
			source: sourcelessRecorded,
			docs:   docsTerm(docsEntry{name: "build", arity: 1, anno: 42}),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newGeneratedDefinitionFixture(t, generatedDomainRel, stampedDefinitions()...)
			source := filepath.Join(server.projectRoot, "deps", "shared_lib", "lib", "shared_lib", "dsl", "extension.ex")
			if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source, []byte("defmodule SharedLib.Dsl.Extension do\n"+strings.Repeat("\n", 50)+"end\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			writeGeneratedBeam(t, server, sourcelessModule, tc.chunks, beamExport{"build", 1})

			locations, err := server.LookupName(sourcelessModule, "build", NameLookupOptions{ExactModule: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(locations) != 1 || locations[0].FilePath != source || locations[0].Line != 42 {
				t.Fatalf("expected %s:42, got %#v", source, locations)
			}
		})
	}
}

// If the compiled source is not in this checkout, the lexical parent is still
// the answer.
func TestLookupNameSourcelessGeneratedModuleMissingSourceKeepsParent(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, stampedDefinitions()...)
	writeGeneratedBeam(t, server, sourcelessModule, testBeamChunks{
		source: sourcelessRecorded,
		docs:   docsTerm(docsEntry{name: "build", arity: 1, anno: 42}),
	}, beamExport{"build", 1})

	locations, err := server.LookupName(sourcelessModule, "build", NameLookupOptions{FallbackToModule: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || locations[0].FilePath != domainPath || locations[0].Line != generatedModuleLine {
		t.Fatalf("expected %s:%d, got %#v", domainPath, generatedModuleLine, locations)
	}
}

// The debug info's line belongs to the file it was compiled from. If that is
// not the file the index has for the module, the line means nothing there.
func TestDefinitionGeneratedFunctionOtherSourceKeepsModuleLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, "lib/my_app/other_chat.ex",
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 1, keepFile: "lib/my_app/other_chat.ex", keepLine: generatedDefineLine},
	)
	// The absolute path must not match either.
	beamPath := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin", "Elixir.MyApp.Chat.beam")
	beamFile := minimalBeamWithDbgi(dbgiTerm(filepath.Join(server.projectRoot, "lib/my_app/other_chat.ex"), "lib/my_app/other_chat.ex",
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 1, keepFile: "lib/my_app/other_chat.ex", keepLine: generatedDefineLine}),
		beamExport{"get_room_by_slug!", 1})
	if err := os.WriteFile(beamPath, beamFile, 0o644); err != nil {
		t.Fatal(err)
	}
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedModuleLine)
}

// Call hierarchy names the same place definition goes to.
func TestPrepareCallHierarchyGeneratedFunctionUsesDebugInfoLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, stampedDefinitions()...)
	docURI := string(uri.File(filepath.Join(server.projectRoot, generatedCallerRel)))
	server.docs.Set(docURI, generatedCallerSource)
	items, err := server.PrepareCallHierarchy(context.Background(), &protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
			Position:     protocol.Position{Line: 4, Character: 10},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || uriToPath(items[0].URI) != domainPath || int(items[0].Range.Start.Line) != generatedDefineLine-1 {
		t.Fatalf("expected the call hierarchy item at %s:%d, got %#v", domainPath, generatedDefineLine, items)
	}
}

func TestLineDeclaresAtom(t *testing.T) {
	names := []string{"get_room_by_slug!", "get_room_by_slug"}
	for _, text := range []string{
		`    define :get_room_by_slug, action: :read`,
		`  SharedLib.Interface.define(:get_room_by_slug)`,
		`define :get_room_by_slug`,
	} {
		if !lineDeclaresAtom(text, names) {
			t.Errorf("lineDeclaresAtom(%q) = false, want true", text)
		}
	}
	// A call, a typespec, a keyword key or value, a longer atom, and an atom
	// that is not the first argument are not declarations.
	for _, text := range []string{
		`get_room_by_slug!("x")`,
		`def typed(x :: :get_room_by_slug), do: x`,
		`[key:get_room_by_slug]`,
		`read action: :get_room_by_slug`,
		`define :get_room_by_slug_extra`,
		`define :other, as: :get_room_by_slug`,
		`:get_room_by_slug`,
		`define:get_room_by_slug`,
		`@tag :get_room_by_slug`,
	} {
		if lineDeclaresAtom(text, names) {
			t.Errorf("lineDeclaresAtom(%q) = true, want false", text)
		}
	}
}

// Lines added above the declaration after the compile move it, but Dexter
// cannot compile: the line from the last compile is the answer until the next.
func TestDefinitionGeneratedFunctionStaleSourceKeepsCompiledLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, lineDefinitions()...)
	indexFile(t, server.store, server.projectRoot, generatedDomainRel, "# one\n# two\n"+generatedDomainSource)
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedDefineLine)
}

// A current BEAM's lines are exact, even when the declaring line does not
// spell the name and another line does.
func TestDefinitionGeneratedFunctionCurrentBEAMKeepsRecordedLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel,
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 6, keepFile: "deps/ash/lib/ash/code_interface.ex", keepLine: 1112},
	)
	future := time.Now().Add(time.Hour)
	beamPath := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin", "Elixir.MyApp.Chat.beam")
	if err := os.Chtimes(beamPath, future, future); err != nil {
		t.Fatal(err)
	}
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, 6)
}

func TestDefinitionGeneratedFunctionPastEndOfFileKeepsModuleLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, lineDefinitions()...)
	short := "defmodule MyApp.Chat do\nend\n"
	indexFile(t, server.store, server.projectRoot, generatedDomainRel, short)
	server.docs.Set(string(uri.File(domainPath)), short)
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, 1)
}

// A `_build` copied from another checkout records that checkout's path, which
// usually still exists. The copy in this project wins; a path outside the
// project is used only when the project has no copy, as for a `path:`
// dependency.
func TestLookupNameSourcelessGeneratedModulePrefersThisCheckout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		localCopy bool
	}{
		{"other checkout", true},
		{"path dependency", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newGeneratedDefinitionFixture(t, generatedDomainRel, stampedDefinitions()...)
			outside := filepath.Join(t.TempDir(), "other_checkout", "lib", "shared_lib", "dsl.ex")
			local := filepath.Join(server.projectRoot, "lib", "shared_lib", "dsl.ex")
			paths := []string{outside}
			if tc.localCopy {
				paths = append(paths, local)
			}
			for _, path := range paths {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("defmodule SharedLib.Dsl do\n"+strings.Repeat("\n", 50)+"end\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			writeGeneratedBeam(t, server, sourcelessModule, testBeamChunks{
				source: outside,
				docs:   docsTerm(docsEntry{name: "build", arity: 1, anno: 42}),
			}, beamExport{"build", 1})

			want := outside
			if tc.localCopy {
				want = local
			}
			locations, err := server.LookupName(sourcelessModule, "build", NameLookupOptions{ExactModule: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(locations) != 1 || locations[0].FilePath != want || locations[0].Line != 42 {
				t.Fatalf("expected %s:42, got %#v", want, locations)
			}
		})
	}
}

func TestSourceRebaseCandidates(t *testing.T) {
	root := filepath.FromSlash("/work/copy")
	for _, tc := range []struct {
		name, recorded, want string
	}{
		{"moved umbrella app", "/work/orig/apps/gen_lib/lib/gen_lib.ex", "/work/copy/apps/gen_lib/lib/gen_lib.ex"},
		{"moved project", "/work/orig/lib/my_app/chat.ex", "/work/copy/lib/my_app/chat.ex"},
		{"dependency built elsewhere", "/build/agent/deps/spark/lib/spark/dsl/extension.ex", "/work/copy/deps/spark/lib/spark/dsl/extension.ex"},
		{"dependency built in its own repository", "/build/spark/lib/spark/dsl/extension.ex", "/work/copy/deps/spark/lib/spark/dsl/extension.ex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sourceRebaseCandidates(filepath.FromSlash(tc.recorded), root)
			if !slices.Contains(got, filepath.FromSlash(tc.want)) {
				t.Errorf("candidates = %v, want %s among them", got, tc.want)
			}
		})
	}
	// A bare file name must not match a file at the project root.
	if got := sourceRebaseCandidates(filepath.FromSlash("/elsewhere/mix.exs"), root); slices.Contains(got, filepath.Join(root, "mix.exs")) {
		t.Errorf("candidates = %v, must not include the root's mix.exs", got)
	}
}

// An example in a @moduledoc is not a declaration, and neither is the same
// call in a sibling module of the same file.
func TestUniqueDeclarationSkipsHeredocsAndSiblings(t *testing.T) {
	text := `defmodule MyApp.Chat do
  @moduledoc """
  Example:

      define :get_room_by_slug
  """

  resources do
    define :get_room_by_slug
  end
end

defmodule MyApp.Archive do
  define :get_room_by_slug
end
`
	src := &currentSource{text: text, lines: blankHeredocs(strings.Split(text, "\n"))}
	functions := []beam.Function{{Name: "get_room_by_slug!", Arity: 1}}
	if got, ok := src.uniqueDeclaration("MyApp.Chat", functions); !ok || got != 9 {
		t.Errorf("uniqueDeclaration = %d, %v; want 9, the one call in MyApp.Chat's body", got, ok)
	}
}

// A file that a request read from disk, without opening it, is only a cache.
// If the file then changes and is compiled again, the cache is old text, not
// unsaved edits, and a current BEAM's line must not be corrected against it.
func TestDefinitionGeneratedFunctionIgnoresCachedFile(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, lineDefinitions()...)
	if _, ok := server.docs.GetOrLoad(string(uri.File(domainPath))); !ok {
		t.Fatal("GetOrLoad did not cache the file")
	}
	edited := strings.Replace(generatedDomainSource, "  resources do\n", "  resources do\n    # added\n", 1)
	if err := os.WriteFile(domainPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	indexFile(t, server.store, server.projectRoot, generatedDomainRel, edited)
	// Compiled after the edit: the define is now on line 9, and the BEAM says so.
	const generator = "deps/ash/lib/ash/code_interface.ex"
	writeGeneratedBeam(t, server, "MyApp.Chat", testBeamChunks{dbgi: dbgiTerm(domainPath, generatedDomainRel,
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: generatedDefineLine + 1, keepFile: generator, keepLine: 1112},
	)}, beamExport{"get_room_by_slug!", 1})
	future := time.Now().Add(time.Hour)
	beamPath := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin", "Elixir.MyApp.Chat.beam")
	if err := os.Chtimes(beamPath, future, future); err != nil {
		t.Fatal(err)
	}
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedDefineLine+1)
}

func TestModuleOwnersByLine(t *testing.T) {
	source := `defmodule A do
  @moduledoc """
  defmodule NotAModule do
  """
  defmodule Inner do
    def x, do: :ok
  end

  def y, do: "end"
end

defmodule B do
end
`
	owners := moduleOwnersByLine([]byte(source), strings.Count(source, "\n")+1)
	for line, want := range map[int]string{1: "", 2: "A", 3: "A", 5: "A", 6: "A.Inner", 7: "A.Inner", 9: "A", 10: "A", 11: "", 13: "B"} {
		if owners[line] != want {
			t.Errorf("line %d owner = %q, want %q", line, owners[line], want)
		}
	}
}

// A module defined in two files, as two umbrella apps can, has two rows. The
// recorded path decides between them; when it cannot, no line is trusted.
func TestBestDescribedResult(t *testing.T) {
	a := store.LookupResult{FilePath: "/new/apps/a/lib/dup.ex", Line: 1}
	b := store.LookupResult{FilePath: "/new/apps/b/lib/dup.ex", Line: 1}
	lines := map[beam.FunctionKey]int{{Name: "f", Arity: 0}: 5}
	for _, tc := range []struct {
		name          string
		info          beam.DebugInfo
		want          string
		wantAmbiguous bool
	}{
		{"exact path", beam.DebugInfo{File: b.FilePath, RelativeFile: "lib/dup.ex", Lines: lines}, b.FilePath, false},
		{"moved umbrella", beam.DebugInfo{File: "/old/apps/b/lib/dup.ex", RelativeFile: "lib/dup.ex", Lines: lines}, b.FilePath, false},
		{"only the relative path", beam.DebugInfo{RelativeFile: "lib/dup.ex", Lines: lines}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := generatedDefinitionSources{debugInfo: tc.info}
			got, found, ambiguous := sources.bestDescribedResult([]store.LookupResult{a, b})
			if ambiguous != tc.wantAmbiguous || found != (tc.want != "") || (found && got.FilePath != tc.want) {
				t.Errorf("bestDescribedResult = %s, found=%v, ambiguous=%v; want %q, ambiguous=%v", got.FilePath, found, ambiguous, tc.want, tc.wantAmbiguous)
			}
		})
	}
}

// The declaration was deleted, and a module below declares the same name. The
// only declaration left is in another module, so the recorded line stays.
func TestDefinitionGeneratedFunctionDriftIgnoresModuleBelow(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, lineDefinitions()...)
	withoutDefine := strings.Replace(generatedDomainSource, "      define :get_room_by_slug, action: :read, get_by: [:slug]\n", "", 1)
	edited := withoutDefine + `
defmodule MyApp.Archive do
  resources do
    define :get_room_by_slug, action: :read
  end
end
`
	indexFile(t, server.store, server.projectRoot, generatedDomainRel, edited)
	server.docs.Set(string(uri.File(domainPath)), edited)
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedDefineLine)
}

// A generator can give a def any line, such as `quote line: 99`. Even from a
// current BEAM, a line past the end of the file is not a place to go; the
// declaring call is.
func TestDefinitionGeneratedFunctionLinePastEndOfFileFindsDeclaration(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel,
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 99, keepFile: "deps/shared_lib/lib/interface.ex", keepLine: 1112},
	)
	future := time.Now().Add(time.Hour)
	beamPath := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin", "Elixir.MyApp.Chat.beam")
	if err := os.Chtimes(beamPath, future, future); err != nil {
		t.Fatal(err)
	}
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedDefineLine)
}

// With no line from the BEAM and no call that declares the name, the module
// is still the answer. Two declaring calls are no better than none.
func TestDefinitionGeneratedFunctionWithoutDeclarationKeepsModuleLine(t *testing.T) {
	for _, tc := range []struct {
		name, source string
	}{
		{"no declaring call", strings.Replace(generatedDomainSource, "      define :get_room_by_slug, action: :read, get_by: [:slug]\n", "", 1)},
		{"two declaring calls", strings.Replace(generatedDomainSource, "      define :list_rooms, action: :read\n", "      define :get_room_by_slug, action: :read\n", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel,
				dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 1, keepFile: "deps/shared_lib/lib/interface.ex", keepLine: 1112},
			)
			indexFile(t, server.store, server.projectRoot, generatedDomainRel, tc.source)
			locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
			expectSingleLocation(t, locations, domainPath, generatedModuleLine)
		})
	}
}
