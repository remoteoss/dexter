package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"
)

const movedUsingSource = `defmodule MyApp.ExMachina do
  defmacro __using__(_opts) do
    quote do
      def insert(factory_name, attrs \\ []), do: :ok
    end
  end
end
`

// useChainFixture indexes a module whose __using__ injects insert/2, a
// factory that uses it, and a caller that imports the factory. It returns the
// caller's URI; insert is at line 4, column 4.
func useChainFixture(t *testing.T, server *Server, usingRel string) string {
	t.Helper()
	indexFile(t, server.store, server.projectRoot, usingRel, movedUsingSource)
	indexFile(t, server.store, server.projectRoot, "lib/factory.ex", "defmodule MyApp.Factory do\n  use MyApp.ExMachina\nend\n")
	callerURI := "file://" + filepath.Join(server.projectRoot, "test/some_test.exs")
	server.docs.Set(callerURI, "defmodule MyApp.SomeTest do\n  import MyApp.Factory\n\n  def run do\n    insert(:user)\n  end\nend\n")
	return callerURI
}

// A module whose file moves (git mv) must still resolve through use chains.
// The __using__ cache kept the old path: the stat failed, the entry was never
// dropped, and every request returned nothing for the life of the daemon.
func TestUsingCacheFollowsModuleThatMoved(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	callerURI := useChainFixture(t, server, "lib/old/ex_machina.ex")
	if locs := definitionAt(t, server, callerURI, 4, 4); len(locs) == 0 {
		t.Fatal("no definition through the use chain before the move")
	}

	oldPath := filepath.Join(server.projectRoot, "lib/old/ex_machina.ex")
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}
	if err := server.store.RemoveFile(oldPath); err != nil {
		t.Fatal(err)
	}
	indexFile(t, server.store, server.projectRoot, "lib/new/ex_machina.ex", movedUsingSource)

	locs := definitionAt(t, server, callerURI, 4, 4)
	if len(locs) == 0 {
		t.Fatal("no definition through the use chain after the module moved")
	}
	if want := filepath.Join(server.projectRoot, "lib/new/ex_machina.ex"); uriToPath(locs[0].URI) != want {
		t.Fatalf("definition at %s, want %s", locs[0].URI, want)
	}

	// The warm-up after a reindex knows the new path too.
	server.warmUsingCache()
	if locs := definitionAt(t, server, callerURI, 4, 4); len(locs) == 0 {
		t.Fatal("no definition through the use chain after the warm-up")
	}
}

// The warm-up passes the path that the index has now. An entry for another
// file is replaced, also when that file is unchanged on disk: the index can
// leave a file out (it went into a nested worktree or a skipped directory)
// while the module is now defined in another file.
func TestUsingCacheWarmUpReplacesEntryForAnotherFile(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	callerURI := useChainFixture(t, server, "lib/old/ex_machina.ex")
	if locs := definitionAt(t, server, callerURI, 4, 4); len(locs) == 0 {
		t.Fatal("no definition through the use chain before the move")
	}
	oldPath := filepath.Join(server.projectRoot, "lib/old/ex_machina.ex")
	if err := server.store.RemoveFile(oldPath); err != nil {
		t.Fatal(err)
	}
	indexFile(t, server.store, server.projectRoot, "lib/new/ex_machina.ex", movedUsingSource)
	server.warmUsingCache()
	entry, ok := server.usingCache.get("MyApp.ExMachina")
	if want := filepath.Join(server.projectRoot, "lib/new/ex_machina.ex"); !ok || entry.filePath != want {
		t.Fatalf("cached __using__ file = %+v, want %s", entry, want)
	}
}

// A file that changed can have lost the module: it is looked up again, not
// parsed again at the old path.
func TestUsingCacheLooksUpModuleAgainWhenItsFileChanged(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	callerURI := useChainFixture(t, server, "lib/old/ex_machina.ex")
	if locs := definitionAt(t, server, callerURI, 4, 4); len(locs) == 0 {
		t.Fatal("no definition through the use chain before the move")
	}
	indexFile(t, server.store, server.projectRoot, "lib/old/ex_machina.ex", "defmodule MyApp.Other do\nend\n")
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(server.projectRoot, "lib/old/ex_machina.ex"), later, later); err != nil {
		t.Fatal(err)
	}
	indexFile(t, server.store, server.projectRoot, "lib/new/ex_machina.ex", movedUsingSource)
	locs := definitionAt(t, server, callerURI, 4, 4)
	if want := filepath.Join(server.projectRoot, "lib/new/ex_machina.ex"); len(locs) == 0 || uriToPath(locs[0].URI) != want {
		t.Fatalf("definition = %v, want %s", locs, want)
	}
}

// When two files define one module, the body that a use chain follows must not
// depend on which request or warm-up worker came first: it is the first path
// in byte order, both on a request and in the warm-up.
func TestUsingCacheChoosesOneFileForModuleDefinedTwice(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	second := filepath.Join(server.projectRoot, "test/support/z_copy/ex_machina.ex")
	first := filepath.Join(server.projectRoot, "lib/a/ex_machina.ex")
	// The later path is indexed first, so that index order would pick it.
	indexFile(t, server.store, server.projectRoot, "test/support/z_copy/ex_machina.ex", movedUsingSource)
	indexFile(t, server.store, server.projectRoot, "lib/a/ex_machina.ex", movedUsingSource)

	if entry := server.cachedUsing("MyApp.ExMachina"); entry == nil || entry.filePath != first {
		t.Fatalf("request chose %+v, want %s", entry, first)
	}
	for i := 0; i < 5; i++ {
		server.usingCache.drop("MyApp.ExMachina", mustUsing(t, server))
		server.warmUsingCache()
		if entry := mustUsing(t, server); entry.filePath != first {
			t.Fatalf("warm-up %d chose %s, want %s (not %s)", i, entry.filePath, first, second)
		}
	}
}

func mustUsing(t *testing.T, server *Server) *usingCacheEntry {
	t.Helper()
	entry, ok := server.usingCache.get("MyApp.ExMachina")
	if !ok {
		t.Fatal("no cached __using__ entry")
	}
	return entry
}

// Every session of a workspace shares the __using__ cache, so the warm-up of
// the daemon's core serves the first References of each editor.
func TestSessionsShareTheUsingCacheOfTheCore(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	indexFile(t, server.store, server.projectRoot, "lib/ex_machina.ex", movedUsingSource)
	index := NewIndexCoordinator()
	core := NewServerWithOptions(server.store, server.projectRoot, ServerOptions{Index: index})
	session := NewServerWithOptions(server.store, server.projectRoot, ServerOptions{Index: index})
	core.warmUsingCache()
	if n := core.usingCache.len(); n != 1 {
		t.Fatalf("core cache has %d entries, want 1", n)
	}
	if n := session.usingCache.len(); n != 1 {
		t.Fatalf("session cache has %d entries after the core warm-up, want 1", n)
	}
	if session.generatedCache != core.generatedCache || session.depsCache != core.depsCache {
		t.Fatal("the session does not share the disk-backed caches of the core")
	}
}

// Whether a file is in a dependency depends on where the mix.exs files are. A
// mix.exs that appears later must change the answer, which was cached for the
// life of the daemon.
func TestDepsCacheFollowsMixExsChanges(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	app := filepath.Join(server.projectRoot, "apps", "billing")
	dep := writeTestFile(t, app, "deps/library/lib/library.ex", "defmodule Library do\nend\n")
	if server.isDepsFile(dep) {
		t.Fatal("a file is a dependency before its project has a mix.exs")
	}
	mixExs := writeTestFile(t, app, "mix.exs", "defmodule Billing.MixProject do\nend\n")
	if err := server.DidChangeWatchedFiles(context.Background(), &protocol.DidChangeWatchedFilesParams{
		Changes: []*protocol.FileEvent{{URI: protocol.DocumentURI("file://" + mixExs), Type: protocol.FileChangeTypeCreated}},
	}); err != nil {
		t.Fatal(err)
	}
	if !server.isDepsFile(dep) {
		t.Fatal("a file below deps/ of a new Mix project is not a dependency")
	}

	// A full reconcile, as after lost events or a branch switch, clears it too.
	if err := os.Remove(mixExs); err != nil {
		t.Fatal(err)
	}
	if err := server.ReindexWorkspace(); err != nil {
		t.Fatal(err)
	}
	if server.isDepsFile(dep) {
		t.Fatal("a file is still a dependency after its project lost its mix.exs")
	}
}

// References read every __using__ module through one row for each (module,
// file) pair. A module defined in two files (one copy in the deps/ of each
// project of a monorepo) made each row replace the entry of the other, so each
// References parsed it again, and the cached file alternated.
func TestUsingCacheDoesNotThrashForModuleDefinedTwice(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	indexFile(t, server.store, server.projectRoot, "deps_b/ex_machina/lib/ex_machina.ex", movedUsingSource)
	indexFile(t, server.store, server.projectRoot, "deps_a/ex_machina/lib/ex_machina.ex", movedUsingSource)
	want := filepath.Join(server.projectRoot, "deps_a/ex_machina/lib/ex_machina.ex")

	server.loadUsingEntries()
	first := mustUsing(t, server)
	for i := 0; i < 20; i++ {
		server.loadUsingEntries()
		if entry := mustUsing(t, server); entry != first {
			t.Fatalf("References %d replaced the cached __using__ entry (file %s)", i+2, entry.filePath)
		}
	}
	if first.filePath != want {
		t.Fatalf("cached file = %s, want %s", first.filePath, want)
	}
}

// A request between a change on disk and its reindex reads a stale index. The
// module moved from a.ex to b.ex, and a.ex now holds another module with its
// own __using__: the body of a.ex must not be cached for the moved module.
func TestUsingCacheNeverCachesFileThatNoLongerDefinesModule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	useChainFixture(t, server, "lib/a.ex")
	if entry := server.cachedUsing("MyApp.ExMachina"); entry == nil {
		t.Fatal("no __using__ entry before the move")
	}
	// The disk changes; the index is not updated yet.
	other := `defmodule MyApp.Other do
  defmacro __using__(_opts) do
    quote do
      def other_helper, do: :ok
    end
  end
end
`
	aPath := writeTestFile(t, server.projectRoot, "lib/a.ex", other)
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(aPath, later, later); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, server.projectRoot, "lib/b.ex", movedUsingSource)
	if entry := server.cachedUsing("MyApp.ExMachina"); entry != nil {
		t.Fatalf("the body of another module was used: %+v", entry.usingBody)
	}

	// The reindex comes: the module resolves to its new file.
	indexFile(t, server.store, server.projectRoot, "lib/a.ex", other)
	indexFile(t, server.store, server.projectRoot, "lib/b.ex", movedUsingSource)
	entry := server.cachedUsing("MyApp.ExMachina")
	if want := filepath.Join(server.projectRoot, "lib/b.ex"); entry == nil || entry.filePath != want {
		t.Fatalf("entry = %+v, want one for %s", entry, want)
	}
}

// The entry keeps the file that the index gave. When the index gives another
// file later, while the cached file is unchanged (a second definition that
// sorts first was indexed), the next request follows the index.
func TestUsingCacheFollowsTheIndexAfterAWrite(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	indexFile(t, server.store, server.projectRoot, "z/ex_machina.ex", movedUsingSource)
	if entry := server.cachedUsing("MyApp.ExMachina"); entry == nil {
		t.Fatal("no __using__ entry")
	}
	indexFile(t, server.store, server.projectRoot, "a/ex_machina.ex", movedUsingSource)
	entry := server.cachedUsing("MyApp.ExMachina")
	if want := filepath.Join(server.projectRoot, "a/ex_machina.ex"); entry == nil || entry.filePath != want {
		t.Fatalf("entry = %+v, want one for %s", entry, want)
	}
}

// defmodule can be written as a call with parentheses. The check that a file
// still defines the module must accept that form, and also reject it when it
// names another module.
func TestUsingCacheAcceptsDefmoduleWithParentheses(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	for _, form := range []string{"defmodule(MyApp.ExMachina) do", "defmodule( MyApp.ExMachina ) do", "defmodule (MyApp.ExMachina) do"} {
		source := strings.Replace(movedUsingSource, "defmodule MyApp.ExMachina do", form, 1)
		path := writeTestFile(t, server.projectRoot, "lib/ex_machina.ex", source)
		if entry := server.parseUsingFile(path, "MyApp.ExMachina"); entry == nil || entry.inlineDefs["insert"] == nil {
			t.Errorf("no __using__ body for %q: %+v", form, entry)
		}
		if entry := server.parseUsingFile(path, "MyApp.Other"); entry != nil {
			t.Errorf("%q gave a body for another module", form)
		}
	}
}
