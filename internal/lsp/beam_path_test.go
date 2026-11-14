package lsp

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppForSource(t *testing.T) {
	root := "/home/dev/myproject"
	tests := []struct {
		name       string
		sourcePath string
		want       string
	}{
		{"project source is not its own app directory", root + "/lib/accounts/user.ex", ""},
		{"dependency", root + "/deps/ash/lib/ash/resource.ex", "ash"},
		{"umbrella child", root + "/apps/billing/lib/invoice.ex", "billing"},
		{"nested dependency path", root + "/deps/ash/lib/ash/actions/action.ex", "ash"},
		{"outside the build root", "/usr/lib/elixir/lib/enum.ex", ""},
		{"empty source", "", ""},
		// A directory merely named "deps" deeper in the tree is not the
		// dependency root; anchoring on buildRoot keeps it from being read as one.
		{"decoy deps directory", root + "/lib/deps/helpers.ex", ""},
		{"build root itself", root, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appForSource(root, tt.sourcePath); got != tt.want {
				t.Errorf("appForSource(%q) = %q, want %q", tt.sourcePath, got, tt.want)
			}
		})
	}
}

// The negative result a developer hits while iterating — module indexed, not yet
// compiled — must be invalidated by the compile itself, not by a timer expiring.
// Creating the BEAM moves its ebin directory's mtime, which is the signal.
func TestGeneratedFunctionsPicksUpNewlyCompiledModule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	const module = "MyApp.Accounts.User"
	indexFile(t, server.store, server.projectRoot, "lib/my_app/accounts/user.ex", `defmodule MyApp.Accounts.User do
  def source_function, do: :ok
end
`)

	if got := server.generatedFunctionsForModule(module); got != nil {
		t.Fatalf("expected nothing before the module is compiled, got %v", got)
	}
	before := server.generatedCacheEntryForTest(module)
	if before == nil || before.beamPath != "" {
		t.Fatalf("expected a cached negative, got %#v", before)
	}
	if before.watchDir == "" {
		t.Fatal("a compiled-application negative must record the directory to watch")
	}

	// Compile it: write a BEAM exporting one function the source does not define.
	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	if err := os.MkdirAll(ebin, 0o755); err != nil {
		t.Fatal(err)
	}
	beamPath := filepath.Join(ebin, "Elixir."+module+".beam")
	if err := os.WriteFile(beamPath, minimalBeam(beamExport{"generated_action", 2}), 0o644); err != nil {
		t.Fatal(err)
	}
	// Directory mtimes can be coarse; make the change unambiguous the way a real
	// compile landing a moment later would.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(ebin, future, future); err != nil {
		t.Fatal(err)
	}

	got := server.generatedFunctionsForModule(module)
	if len(got) != 1 || got[0].Name != "generated_action" || got[0].Arity != 2 {
		t.Fatalf("expected the freshly compiled function without waiting on a timer, got %v", got)
	}
	after := server.generatedCacheEntryForTest(module)
	if after == nil || after.beamPath != beamPath {
		t.Fatalf("expected the entry to resolve to %s, got %#v", beamPath, after)
	}
}

// Dexter must never require a successful compile to be useful. A BEAM older than
// its source is stale, not unusable: it still describes the last known set of
// generated functions, which beats offering nothing at all while a developer
// iterates. Anything the source index disagrees with is handled by the index
// winning, not by discarding the BEAM.
func TestGeneratedFunctionsIgnoreStaleBEAM(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	const module = "MyApp.Accounts.User"
	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	if err := os.MkdirAll(ebin, 0o755); err != nil {
		t.Fatal(err)
	}
	beamPath := filepath.Join(ebin, "Elixir."+module+".beam")
	// Export both a generated function and one Dexter already indexed from
	// source, so the delta has to exclude the latter.
	if err := os.WriteFile(beamPath, minimalBeam(
		beamExport{"generated_action", 2},
		beamExport{"source_function", 0},
	), 0o644); err != nil {
		t.Fatal(err)
	}

	sourcePath := filepath.Join(server.projectRoot, "lib", "my_app", "accounts", "user.ex")
	indexFile(t, server.store, server.projectRoot, "lib/my_app/accounts/user.ex", `defmodule MyApp.Accounts.User do
  def source_function, do: :ok
end
`)
	// Make the source unambiguously newer than the compiled BEAM.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(sourcePath, future, future); err != nil {
		t.Fatal(err)
	}

	got := server.generatedFunctionsForModule(module)
	if len(got) != 1 || got[0].Name != "generated_action" || got[0].Arity != 2 {
		t.Fatalf("a stale BEAM must still yield generated functions, got %v", got)
	}
	// The index still wins: a function Dexter indexed from source is not
	// re-offered from the BEAM just because the BEAM also exports it.
	for _, function := range got {
		if function.Name == "source_function" {
			t.Error("an indexed function must not be reported as generated")
		}
	}
}

// beamExport is one entry of a synthetic BEAM's export table.
type beamExport struct {
	name  string
	arity int
}

// minimalBeam builds a BEAM carrying only an AtU8 atom table and an ExpT export
// table, which is all ReadExports needs. Atom lengths use the pre-OTP 28
// one-byte form; the OTP 28+ varint form has its own test in internal/beam.
func minimalBeam(exports ...beamExport) []byte {
	return minimalBeamWithDocs("", exports...)
}

// testAttr is one attribute persisted in a synthetic Attr chunk.
type testAttr struct {
	name   string
	values []string // encoded as atoms, as the compiler persists module names
}

// minimalBeamWithAttrs adds an Attr chunk to minimalBeamWithDocs. It is how a test
// makes a compiled module record facts its source does not state, which is the
// only way a framework's macro provider can be discovered.
func minimalBeamWithAttrs(attrs []testAttr, docs string, exports ...beamExport) []byte {
	return buildTestBeam(testBeamChunks{attrs: attrs, docs: docs}, exports...)
}

// minimalBeamWithDocs is minimalBeam plus a Docs chunk whose inflated payload is
// docs. ReadDocBody inflates that payload and slices it, so a test can put one
// function's prose at a known offset without encoding a real docs_v1 term.
func minimalBeamWithDocs(docs string, exports ...beamExport) []byte {
	return buildTestBeam(testBeamChunks{docs: docs}, exports...)
}

// minimalBeamWithDbgi adds a Dbgi chunk holding the encoded debug info term,
// which is where a compiled module records the line of each definition.
func minimalBeamWithDbgi(dbgi []byte, exports ...beamExport) []byte {
	return buildTestBeam(testBeamChunks{dbgi: dbgi}, exports...)
}

// testBeamChunks are the optional chunks of a synthetic BEAM.
type testBeamChunks struct {
	attrs []testAttr
	docs  string
	dbgi  []byte
	// source is written as the CInf chunk's :source, the file the module was
	// compiled from.
	source string
}

func buildTestBeam(chunks testBeamChunks, exports ...beamExport) []byte {
	names := make([]string, 0, len(exports)+1)
	names = append(names, "Elixir.Minimal")
	for _, export := range exports {
		names = append(names, export.name)
	}

	var atoms bytes.Buffer
	writeBE32(&atoms, uint32(len(names)))
	for _, name := range names {
		atoms.WriteByte(byte(len(name)))
		atoms.WriteString(name)
	}

	var table bytes.Buffer
	writeBE32(&table, uint32(len(exports)))
	for index, export := range exports {
		writeBE32(&table, uint32(index+2)) // atom indexes are one-based; 1 is the module
		writeBE32(&table, uint32(export.arity))
		writeBE32(&table, uint32(index+1)) // label
	}

	var body bytes.Buffer
	writeBeamChunk(&body, "AtU8", atoms.Bytes())
	writeBeamChunk(&body, "ExpT", table.Bytes())
	if chunks.docs != "" {
		writeBeamChunk(&body, "Docs", docsChunk(chunks.docs))
	}
	if len(chunks.attrs) > 0 {
		writeBeamChunk(&body, "Attr", attrChunk(chunks.attrs))
	}
	if len(chunks.dbgi) > 0 {
		// Uncompressed, like docsChunk; the reader accepts both forms.
		writeBeamChunk(&body, "Dbgi", append([]byte{131}, chunks.dbgi...))
	}
	if chunks.source != "" {
		writeBeamChunk(&body, "CInf", compileInfoChunk(chunks.source))
	}

	var file bytes.Buffer
	file.WriteString("FOR1")
	writeBE32(&file, uint32(body.Len()+4))
	file.WriteString("BEAM")
	file.Write(body.Bytes())
	return file.Bytes()
}

// docsChunk wraps docs in the uncompressed ETF form, which inflateDocsTerm
// returns verbatim, so a Function's DocOffset is a plain index into docs.
func docsChunk(docs string) []byte {
	chunk := make([]byte, 0, len(docs)+1)
	chunk = append(chunk, 131) // ETF version byte; 80 would mean COMPRESSED
	return append(chunk, docs...)
}

// compileInfoChunk encodes the CInf chunk ReadSourcePath reads, with :source
// as the compact string term the compiler writes for a charlist.
func compileInfoChunk(source string) []byte {
	out := []byte{131} // ETF version byte
	out = append(out, etfListHeader(1)...)
	out = append(out, 104, 2) // small tuple of arity 2
	out = append(out, etfAtom("source")...)
	out = append(out, 107) // STRING_EXT
	out = binary.BigEndian.AppendUint16(out, uint16(len(source)))
	out = append(out, source...)
	return append(out, 106) // NIL_EXT
}

// attrChunk encodes the Attr chunk ReadModuleAttributes reads: an ETF list of
// {name, [values]} pairs, with the values as atoms because that is how the
// compiler persists module names.
func attrChunk(attrs []testAttr) []byte {
	var out []byte
	out = append(out, 131) // ETF version byte
	out = append(out, etfListHeader(len(attrs))...)
	for _, attr := range attrs {
		out = append(out, 104, 2) // small tuple of arity 2
		out = append(out, etfAtom(attr.name)...)
		out = append(out, etfListHeader(len(attr.values))...)
		for _, value := range attr.values {
			out = append(out, etfAtom(value)...)
		}
		out = append(out, 106) // NIL, the list tail
	}
	out = append(out, 106)
	return out
}

func etfListHeader(count int) []byte {
	header := []byte{108} // LIST_EXT
	var field [4]byte
	binary.BigEndian.PutUint32(field[:], uint32(count))
	return append(header, field[:]...)
}

func etfAtom(name string) []byte {
	if len(name) > 255 {
		panic("test atom too long for the one-byte length form")
	}
	return append([]byte{119, byte(len(name))}, name...) // SMALL_ATOM_UTF8_EXT
}

func writeBeamChunk(chunks *bytes.Buffer, name string, data []byte) {
	chunks.WriteString(name)
	writeBE32(chunks, uint32(len(data)))
	chunks.Write(data)
	for chunks.Len()%4 != 0 {
		chunks.WriteByte(0)
	}
}

func writeBE32(buf *bytes.Buffer, value uint32) {
	var field [4]byte
	binary.BigEndian.PutUint32(field[:], value)
	buf.Write(field[:])
}

// monorepoFixture lays out a monorepo: the workspace root is not a Mix project
// and has no _build, apps/main compiles everything, and
// libs/remote-library is a path dependency whose application is `remote`.
func monorepoFixture(t *testing.T) (*Server, string) {
	t.Helper()
	server, cleanup := setupTestServer(t)
	t.Cleanup(cleanup)
	root := server.projectRoot
	writeFile := func(relative, content string) {
		t.Helper()
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("apps/main/mix.exs", "defmodule Main.MixProject do\n  def project, do: [app: :main, deps: [{:remote, path: \"../../libs/remote-library\"}]]\nend\n")
	writeFile("libs/remote-library/mix.exs", "defmodule Remote.MixProject do\n  @app :remote\n  def project, do: [app: @app]\nend\n")
	indexFile(t, server.store, root, "libs/remote-library/lib/remote/thing.ex", "defmodule Remote.Thing do\n  defmacro __using__(_), do: nil\nend\n")
	return server, root
}

func writeBeamAt(t *testing.T, ebin, module string, modified time.Time, exports ...beamExport) {
	t.Helper()
	if err := os.MkdirAll(ebin, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ebin, "Elixir."+module+".beam")
	if err := os.WriteFile(path, minimalBeam(exports...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

// A library with no _build of its own is found in the build that compiles it as
// a path dependency, both for its own modules and for a generated module
// beneath one of them.
func TestGeneratedFunctionsFromLibraryCompiledByAnotherProject(t *testing.T) {
	server, root := monorepoFixture(t)
	ebin := filepath.Join(root, "apps/main/_build/dev/lib/remote/ebin")
	writeBeamAt(t, ebin, "Remote.Thing", time.Now(), beamExport{"generated_fn", 1})
	writeBeamAt(t, ebin, "Remote.Thing.Helpers", time.Now(), beamExport{"helper_path", 2})

	if _, found := server.generatedSymbol("Remote.Thing", "", "generated_fn"); !found {
		t.Error("generated function of a library compiled by another project was not found")
	}
	if _, found := server.generatedSymbol("Remote.Thing.Helpers", "", "helper_path"); !found {
		t.Error("generated module beneath a library module was not found")
	}
}

// When a library is also compiled on its own, the most recently compiled BEAM
// wins, whichever build holds it.
func TestGeneratedFunctionsPreferMostRecentBuild(t *testing.T) {
	older, newer := time.Now().Add(-time.Hour), time.Now()
	for _, tc := range []struct {
		name             string
		consumer, ownLib time.Time
		want, notWant    string
	}{
		{"consumer compiled last", newer, older, "consumer_fn", "standalone_fn"},
		{"library compiled last", older, newer, "standalone_fn", "consumer_fn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, root := monorepoFixture(t)
			if err := os.MkdirAll(filepath.Join(root, "libs/remote-library/_build"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeBeamAt(t, filepath.Join(root, "apps/main/_build/dev/lib/remote/ebin"), "Remote.Thing", tc.consumer, beamExport{"consumer_fn", 0})
			writeBeamAt(t, filepath.Join(root, "libs/remote-library/_build/dev/lib/remote/ebin"), "Remote.Thing", tc.ownLib, beamExport{"standalone_fn", 0})

			if _, found := server.generatedSymbol("Remote.Thing", "", tc.want); !found {
				t.Errorf("%s from the newest build was not found", tc.want)
			}
			if _, found := server.generatedSymbol("Remote.Thing", "", tc.notWant); found {
				t.Errorf("%s from the older build was used", tc.notWant)
			}
		})
	}
}

// separateBuildsFixture lays out a monorepo where each project has its own
// build: apps/app_one compiles itself and its path dependency libs/lib_one,
// and libs/lib_two is compiled on its own. The workspace root has no _build.
func separateBuildsFixture(t *testing.T) (*Server, string) {
	t.Helper()
	server, cleanup := setupTestServer(t)
	t.Cleanup(cleanup)
	root := server.projectRoot
	for relative, content := range map[string]string{
		"apps/app_one/mix.exs": "defmodule AppOne.MixProject do\n  def project, do: [app: :app_one, deps: [{:lib_one, path: \"../../libs/lib_one\"}]]\nend\n",
		"libs/lib_one/mix.exs": "defmodule LibOne.MixProject do\n  def project, do: [app: :lib_one]\nend\n",
		"libs/lib_two/mix.exs": "defmodule LibTwo.MixProject do\n  def project, do: [app: :lib_two]\nend\n",
	} {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBeamAt(t, filepath.Join(root, "apps/app_one/_build/dev/lib/app_one/ebin"), "AppOne.User", time.Now(), beamExport{"generated_fn", 0})
	writeBeamAt(t, filepath.Join(root, "apps/app_one/_build/dev/lib/lib_one/ebin"), "LibOne.Thing", time.Now(), beamExport{"lib_fn", 0})
	writeBeamAt(t, filepath.Join(root, "libs/lib_two/_build/dev/lib/lib_two/ebin"), "LibTwo.Thing", time.Now(), beamExport{"other_fn", 0})
	indexFile(t, server.store, root, "apps/app_one/lib/app_one/user.ex", "defmodule AppOne.User do\n  def source_function, do: :ok\nend\n")
	indexFile(t, server.store, root, "libs/lib_one/lib/lib_one/thing.ex", "defmodule LibOne.Thing do\n  def source_function, do: :ok\nend\n")
	return server, root
}

// A module that no source defines, and that has no indexed parent, is found in
// the build of the project that generates it, also when the workspace root has
// no build of its own.
func TestGeneratedModuleWithoutSourceFoundInProjectBuild(t *testing.T) {
	server, root := separateBuildsFixture(t)
	writeBeamAt(t, filepath.Join(root, "apps/app_one/_build/dev/lib/app_one/ebin"), "AppOne.Generated.Thing", time.Now(), beamExport{"run", 0})

	if _, found := server.generatedSymbol("AppOne.Generated.Thing", "", "run"); !found {
		t.Error("function of a generated module in a project build was not found")
	}
	if _, found := server.generatedSymbol("AppOne.User", "", "generated_fn"); !found {
		t.Error("generated function of a project module was not found")
	}
}

// A project with path dependencies holds more than one application that is
// not in deps/. Its own application comes from its mix.exs, so the negative
// result for a module that is not compiled yet watches that application's
// ebin, and the compile that adds the module clears it.
func TestGeneratedFunctionsPicksUpNewlyCompiledModuleInProjectWithPathDeps(t *testing.T) {
	server, root := separateBuildsFixture(t)
	const module = "AppOne.Later"
	indexFile(t, server.store, root, "apps/app_one/lib/app_one/later.ex", "defmodule AppOne.Later do\n  def source_function, do: :ok\nend\n")

	if got := server.generatedFunctionsForModule(module); got != nil {
		t.Fatalf("expected nothing before the module is compiled, got %v", got)
	}
	ebin := filepath.Join(root, "apps/app_one/_build/dev/lib/app_one/ebin")
	if entry := server.generatedCacheEntryForTest(module); entry == nil || entry.watchDir != ebin {
		t.Fatalf("negative result must watch %s, got %#v", ebin, entry)
	}

	// Compile it. Only the ebin changes: the lib directory above it does not.
	writeBeamAt(t, ebin, module, time.Now(), beamExport{"later_fn", 0})
	// Directory mtimes can be coarse; make the change unambiguous the way a
	// real compile landing a moment later would.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(ebin, future, future); err != nil {
		t.Fatal(err)
	}

	if _, found := server.generatedSymbol(module, "", "later_fn"); !found {
		t.Error("generated function of a newly compiled module was not found")
	}
	if _, found := server.generatedSymbol("LibOne.Thing", "", "lib_fn"); !found {
		t.Error("generated function of a path dependency was not found")
	}
}

// A build with no lib directory for the current profile is read once, not by
// each lookup that asks it, until the directory appears.
func TestLibIndexCachesMissingLibDirectory(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	buildRoot := filepath.Join(server.projectRoot, "libs/test_only")
	if err := os.MkdirAll(filepath.Join(buildRoot, "_build", "test", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := server.libIndex(buildRoot)
	if first.loaded {
		t.Fatal("a missing lib directory must not load")
	}
	if again := server.libIndex(buildRoot); again != first {
		t.Error("a missing lib directory was read again")
	}
	writeBeamAt(t, filepath.Join(buildRoot, "_build", "dev", "lib", "test_only", "ebin"), "TestOnly", time.Now())
	if fresh := server.libIndex(buildRoot); fresh == first || !fresh.loaded {
		t.Error("the lib directory appeared but the snapshot was not rebuilt")
	}
}

func TestParseMixApp(t *testing.T) {
	for source, want := range map[string]string{
		"def project, do: [app: :remote, version: \"0.1.0\"]":      "remote",
		"@app :fields_inventory\ndef project, do: [app: @app]":     "fields_inventory",
		"def project, do: [app: @app]":                             "",
		"def project, do: [version: \"0.1.0\"]":                    "",
		"def project do\n  [\n    app:   :ex_jsf_logic,\n  ]\nend": "ex_jsf_logic",
	} {
		if got := parseMixApp([]byte(source)); got != want {
			t.Errorf("parseMixApp(%q) = %q, want %q", source, got, want)
		}
	}
}
