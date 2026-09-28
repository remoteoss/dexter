package lsp

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"

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
// the domain's BEAM with the given debug info. The source is backdated so the
// BEAM is the newer of the two, as it is right after `mix compile`.
func newGeneratedDefinitionFixture(t *testing.T, relativeFile string, definitions ...dbgiDefinition) (*Server, string) {
	t.Helper()
	server, cleanup := setupTestServer(t)
	t.Cleanup(cleanup)

	indexFile(t, server.store, server.projectRoot, generatedDomainRel, generatedDomainSource)
	indexFile(t, server.store, server.projectRoot, generatedCallerRel, generatedCallerSource)
	domainPath := filepath.Join(server.projectRoot, generatedDomainRel)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(domainPath, past, past); err != nil {
		t.Fatal(err)
	}

	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	if err := os.MkdirAll(ebin, 0o755); err != nil {
		t.Fatal(err)
	}
	beam := minimalBeamWithDbgi(dbgiTerm(domainPath, relativeFile, definitions...),
		beamExport{"run_local", 0},
		beamExport{"list_rooms", 0},
		beamExport{"get_room_by_slug!", 1},
		beamExport{"get_room_by_slug!", 2},
	)
	if err := os.WriteFile(filepath.Join(ebin, "Elixir.MyApp.Chat.beam"), beam, 0o644); err != nil {
		t.Fatal(err)
	}
	return server, domainPath
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

// Without a recorded line, a strict lookup still answers with the module.
func TestLookupNameStrictGeneratedFunctionWithoutLineKeepsModule(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel,
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 1, keepFile: "deps/shared_lib/lib/interface.ex", keepLine: 1112},
	)
	locations, err := server.LookupName("MyApp.Chat", "get_room_by_slug!", NameLookupOptions{ExactModule: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || locations[0].FilePath != domainPath || locations[0].Line != generatedModuleLine {
		t.Fatalf("expected %s:%d, got %#v", domainPath, generatedModuleLine, locations)
	}
}

// A library's `quote location: :keep` names the library's own file. That is
// the generator, not the declaration, and :line only says where the
// before-compile hook ran, which is no better than the module itself.
func TestDefinitionGeneratedFunctionForeignLocationKeepsModuleLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel,
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 1, keepFile: "deps/shared_lib/lib/interface.ex", keepLine: 1112},
	)
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedModuleLine)
}

// A BEAM older than its source describes lines that may have moved. The
// function still exists, so the module is still the answer, but not the line.
func TestDefinitionGeneratedFunctionStaleBEAMKeepsModuleLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, generatedDomainRel, stampedDefinitions()...)
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(domainPath, future, future); err != nil {
		t.Fatal(err)
	}
	locations := generatedDefinitionAt(t, server, generatedCallerRel, generatedCallerSource, 4, 10)
	expectSingleLocation(t, locations, domainPath, generatedModuleLine)
}

// The debug info's line belongs to the file it was compiled from. If that is
// not the file the index has for the module, the line means nothing there.
func TestDefinitionGeneratedFunctionOtherSourceKeepsModuleLine(t *testing.T) {
	server, domainPath := newGeneratedDefinitionFixture(t, "lib/my_app/other_chat.ex",
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 1, keepFile: "lib/my_app/other_chat.ex", keepLine: generatedDefineLine},
	)
	// The absolute path must not match either.
	beamPath := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin", "Elixir.MyApp.Chat.beam")
	beam := minimalBeamWithDbgi(dbgiTerm(filepath.Join(server.projectRoot, "lib/my_app/other_chat.ex"), "lib/my_app/other_chat.ex",
		dbgiDefinition{name: "get_room_by_slug!", arity: 1, line: 1, keepFile: "lib/my_app/other_chat.ex", keepLine: generatedDefineLine}),
		beamExport{"get_room_by_slug!", 1})
	if err := os.WriteFile(beamPath, beam, 0o644); err != nil {
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
