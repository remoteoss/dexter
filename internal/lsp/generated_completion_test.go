package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/beam"
	"go.lsp.dev/uri"
)

// The delta must come back sorted by name so completion can binary-search a
// prefix range. Exports arrive in ExpT order, which is not sorted.
func TestGeneratedFunctionsSortedByName(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	const module = "MyApp.Accounts.User"
	writeTestModuleBEAM(t, server, module, "lib/my_app/accounts/user.ex",
		beamExport{"zebra", 0},
		beamExport{"create!", 2},
		beamExport{"apple", 1},
		beamExport{"create", 1},
		beamExport{"create", 3},
	)

	got := server.generatedFunctionsForModule(module)
	want := []string{"apple/1", "create/1", "create/3", "create!/2", "zebra/0"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", keysOf(got), want)
	}
	for i, key := range keysOf(got) {
		if key != want[i] {
			t.Fatalf("position %d = %q, want %q (full: %v)", i, key, want[i], keysOf(got))
		}
	}
}

// Export visibility is the boundary even without a Docs chunk. Public
// introspection functions such as Ecto's __schema__ are legitimate API when a
// caller knows it needs them, just like source-indexed __dunder__ functions.
func TestGeneratedFunctionsWithoutDocsIncludesPublicInternals(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	const module = "MyApp.Accounts.User"
	writeTestModuleBEAM(t, server, module, "lib/my_app/accounts/user.ex",
		beamExport{"__struct__", 1},
		beamExport{"__changeset__", 0},
		beamExport{"get_by_id", 1},
	)

	got := keysOf(server.generatedFunctionsForModule(module))
	want := []string{"__changeset__/0", "__struct__/1", "get_by_id/1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want all public exports %v", got, want)
	}
}

// writeTestModuleBEAM indexes a one-function module and writes a synthetic BEAM
// for it that exports the given functions, so the delta is exactly those.
func writeTestModuleBEAM(t *testing.T, server *Server, module, relSource string, exports ...beamExport) {
	t.Helper()
	indexAndCompile(t, server, module, relSource,
		"defmodule "+module+" do\n  def source_function, do: :ok\nend\n", exports...)
}

// indexAndCompile indexes arbitrary source for a module and writes a synthetic
// BEAM exporting the given functions, mimicking a compiled project.
func indexAndCompile(t *testing.T, server *Server, module, relSource, source string, exports ...beamExport) {
	t.Helper()
	indexFile(t, server.store, server.projectRoot, relSource, source)
	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	if err := os.MkdirAll(ebin, 0o755); err != nil {
		t.Fatal(err)
	}
	beamPath := filepath.Join(ebin, "Elixir."+module+".beam")
	if err := os.WriteFile(beamPath, minimalBeam(exports...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func keysOf(functions []beam.Function) []string {
	keys := make([]string, len(functions))
	for i, function := range functions {
		keys[i] = function.Name + "/" + strconv.Itoa(function.Arity)
	}
	return keys
}

// The delta against the source index has to be complete. ListModuleFunctions
// caps at 100 rows because it feeds completion, so a module with more public
// functions than that loses its alphabetically later ones — which then look
// unindexed and get re-offered as generated, duplicating them.
func TestGeneratedFunctionsDeltaUsesCompleteIndex(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	const module = "MyApp.BigContext"
	const total = 120

	var source strings.Builder
	source.WriteString("defmodule " + module + " do\n")
	for i := range total {
		// Zero-padded so lexical order matches creation order and the tail of the
		// alphabet lands past the 100-row cap.
		fmt.Fprintf(&source, "  def fn_%03d(value), do: value\n", i)
	}
	source.WriteString("end\n")

	indexAndCompile(t, server, module, "lib/my_app/big_context.ex", source.String(),
		beamExport{"fn_119", 1},           // indexed, but beyond the cap
		beamExport{"generated_action", 2}, // indexed nowhere
	)

	got := server.generatedFunctionsForModule(module)
	if len(got) != 1 || got[0].Name != "generated_action" || got[0].Arity != 2 {
		t.Fatalf("expected only the genuinely generated function, got %v", keysOf(got))
	}
}

func (s *Server) generatedCacheEntryForTest(module string) *generatedFunctionCacheEntry {
	entry, ok := s.generatedCache.get(module)
	if !ok {
		return nil
	}
	return &entry
}

func (s *Server) expireGeneratedCacheEntryForTest(module string) {
	entry, ok := s.generatedCache.get(module)
	if !ok {
		return
	}
	entry.retryAfter = time.Now().Add(-time.Nanosecond)
	s.generatedCache.put(module, entry)
}

// Eviction must drop one entry, not the cache. A monorepo touches far more
// modules than the cap holds, and clearing everything at once would make the
// next completions re-inflate every Docs chunk from scratch.
func TestGeneratedFunctionCacheEvictsLeastRecentlyUsed(t *testing.T) {
	cache := newGeneratedFunctionCache()

	fill := func(module string) {
		cache.put(module, generatedFunctionCacheEntry{sourcePath: module})
	}
	for i := range maxGeneratedFunctionCacheEntries {
		fill(strconv.Itoa(i))
	}
	if cache.order.Len() != maxGeneratedFunctionCacheEntries {
		t.Fatalf("expected %d entries, got %d", maxGeneratedFunctionCacheEntries, cache.order.Len())
	}

	// Touch the oldest entry so it becomes the most recently used.
	if _, ok := cache.get("0"); !ok {
		t.Fatal("entry 0 should still be cached")
	}
	fill("overflow")

	if _, ok := cache.get("0"); !ok {
		t.Error("the most recently used entry must survive eviction")
	}
	if _, ok := cache.get("1"); ok {
		t.Error("the least recently used entry should have been evicted")
	}
	if cache.order.Len() != maxGeneratedFunctionCacheEntries {
		t.Errorf("cache grew past its cap: %d", cache.order.Len())
	}
}

// A module the index does not define has no source file to stat, so nothing can
// invalidate its answer but time. Dep modules, unknown aliases and half-typed
// module names all land here, and completion walks them on every keystroke —
// an uncached miss is a permanent SQLite query per keypress.
func TestGeneratedFunctionsCachesStoreMiss(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	const module = "SharedLib.NotIndexedYet"

	if got := server.generatedFunctionsForModule(module); got != nil {
		t.Fatalf("expected no functions for an unindexed module, got %v", got)
	}

	entry := server.generatedCacheEntryForTest(module)
	if entry == nil {
		t.Fatal("expected the store miss to be cached")
	}
	if entry.sourcePath != "" {
		t.Fatalf("a negative entry must not carry a source path, got %q", entry.sourcePath)
	}
	if !time.Now().Before(entry.retryAfter) {
		t.Fatal("a negative entry must set a retry deadline")
	}

	// Indexing the module must stay invisible until the deadline passes; that
	// bounded staleness is the trade for not querying on every keystroke.
	indexFile(t, server.store, server.projectRoot, "lib/shared_lib/not_indexed_yet.ex", `defmodule SharedLib.NotIndexedYet do
  def found_it, do: :ok
end
`)
	if got := server.generatedFunctionsForModule(module); got != nil {
		t.Fatalf("expected the cached miss to still be served, got %v", got)
	}
	if entry := server.generatedCacheEntryForTest(module); entry == nil || entry.sourcePath != "" {
		t.Fatal("the cached miss must survive a store change inside its deadline")
	}

	server.expireGeneratedCacheEntryForTest(module)

	// No BEAM exists for this fixture, so the result is still empty — but the
	// entry must now carry the source path, proving the store was re-queried.
	if got := server.generatedFunctionsForModule(module); got != nil {
		t.Fatalf("expected no functions without a compiled BEAM, got %v", got)
	}
	entry = server.generatedCacheEntryForTest(module)
	if entry == nil || entry.sourcePath == "" {
		t.Fatal("expected the store to be re-queried once the deadline passed")
	}
}

// Frameworks can generate an entire module rather than adding functions to a
// source-backed consumer. Phoenix route helpers are the common case: an alias
// such as `alias MyApp.Router.Helpers, as: Routes` names a module that exists
// only as a BEAM, so there is no module row for the store to resolve first.
func TestCompletionGeneratedModuleThroughAsAlias(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	const generatedModule = "SharedLib.Router.Helpers"
	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	if err := os.MkdirAll(ebin, 0o755); err != nil {
		t.Fatal(err)
	}
	beamPath := filepath.Join(ebin, "Elixir."+generatedModule+".beam")
	if err := os.WriteFile(beamPath, minimalBeam(beamExport{"generated_path", 2}), 0o644); err != nil {
		t.Fatal(err)
	}

	const source = `defmodule MyApp.Caller do
  alias SharedLib.Router.Helpers, as: Routes

  def run, do: Routes.gen
end
`
	indexFile(t, server.store, server.projectRoot, "lib/my_app/caller.ex", source)
	docURI := string(uri.File(filepath.Join(server.projectRoot, "lib", "my_app", "caller.ex")))
	server.docs.Set(docURI, source)

	items := completionAt(t, server, docURI, 3, uint32(len("  def run, do: Routes.gen")))
	if !hasCompletionItem(items, "generated_path/2") {
		t.Errorf("expected a function from the generated aliased module, got %#v", items)
	}
}
