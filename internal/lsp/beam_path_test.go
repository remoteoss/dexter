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

// minimalBeamWithDocs is minimalBeam plus a Docs chunk whose inflated payload is
// docs. ReadDocBody inflates that payload and slices it, so a test can put one
// function's prose at a known offset without encoding a real docs_v1 term.
func minimalBeamWithDocs(docs string, exports ...beamExport) []byte {
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

	var chunks bytes.Buffer
	writeBeamChunk(&chunks, "AtU8", atoms.Bytes())
	writeBeamChunk(&chunks, "ExpT", table.Bytes())
	if docs != "" {
		writeBeamChunk(&chunks, "Docs", docsChunk(docs))
	}

	var file bytes.Buffer
	file.WriteString("FOR1")
	writeBE32(&file, uint32(chunks.Len()+4))
	file.WriteString("BEAM")
	file.Write(chunks.Bytes())
	return file.Bytes()
}

// docsChunk wraps docs in the uncompressed ETF form, which inflateDocsTerm
// returns verbatim, so a Function's DocOffset is a plain index into docs.
func docsChunk(docs string) []byte {
	chunk := make([]byte, 0, len(docs)+1)
	chunk = append(chunk, 131) // ETF version byte; 80 would mean COMPRESSED
	return append(chunk, docs...)
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
