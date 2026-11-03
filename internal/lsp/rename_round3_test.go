package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	r3Alpha = "defmodule MyApp.Alpha do\n  def x, do: MyApp.Beta.run()\nend\n"
	r3Beta  = "defmodule MyApp.Beta do\n  def run, do: 1\nend\n"
)

// A rename waits for the index update of the rename before it: the first one
// moved alpha.ex to gamma.ex, and the second must find the call in gamma.ex.
func TestRename_WaitsForTheLastRenameIndex(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	core := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", r3Alpha)
	indexFile(t, st, dir, "lib/beta.ex", r3Beta)
	gPath := filepath.Join(dir, "lib/gamma.ex")

	core.index.reindexing.Lock() // the first rename's index update waits
	if _, err := core.RenameModuleHeadless(context.Background(), "MyApp.Alpha", "MyApp.Gamma"); err != nil {
		core.index.reindexing.Unlock()
		t.Fatal(err)
	}
	if _, err := os.Stat(gPath); err != nil {
		core.index.reindexing.Unlock()
		t.Fatalf("alpha.ex was not moved: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := core.RenameFunctionHeadless(context.Background(), "MyApp.Beta", "run", "go")
		done <- err
	}()
	select {
	case err := <-done:
		core.index.reindexing.Unlock()
		t.Fatalf("the second rename did not wait for the index (err %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	core.index.reindexing.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	core.index.backgroundWork.Wait()
	if got := readFile(t, gPath); !strings.Contains(got, "MyApp.Beta.go()") {
		t.Errorf("gamma.ex still calls run:\n%s", got)
	}
}

// The wait is bounded: a rename refuses when the last rename's index update
// does not finish in time.
func TestRename_IndexWaitIsBounded(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	core := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", r3Alpha)
	indexFile(t, st, dir, "lib/beta.ex", r3Beta)
	old := renameIndexWait
	renameIndexWait = 50 * time.Millisecond
	t.Cleanup(func() { renameIndexWait = old })

	core.index.reindexing.Lock()
	if _, err := core.RenameModuleHeadless(context.Background(), "MyApp.Alpha", "MyApp.Gamma"); err != nil {
		core.index.reindexing.Unlock()
		t.Fatal(err)
	}
	_, err := core.RenameFunctionHeadless(context.Background(), "MyApp.Beta", "run", "go")
	core.index.reindexing.Unlock()
	if err == nil || !strings.Contains(err.Error(), "the index is still being updated after the last rename") {
		t.Fatalf("want a refusal, got %v", err)
	}
}

// A file that was saved and not indexed yet (the watcher is behind) is
// indexed again by the rename, which then succeeds; the index afterwards
// equals a cold build of the files.
func TestRename_SavedFileNotIndexedYet(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	bPath := filepath.Join(dir, "lib/beta.ex")
	saved := "defmodule MyApp.Beta do\n  @moduledoc false\n\n  def run, do: MyApp.Alpha.foo(1)\n  def again, do: MyApp.Alpha.foo(2)\nend\n"
	writeFile(t, bPath, saved)
	later := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(bPath, later, later)

	if _, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar")); err != nil {
		t.Fatal(err)
	}
	a.index.backgroundWork.Wait()
	want := strings.ReplaceAll(saved, "Alpha.foo(", "Alpha.bar(")
	if got := readFile(t, bPath); got != want {
		t.Errorf("beta.ex:\n%s\nwant:\n%s", got, want)
	}
	expectColdBuild(t, st, dir, "lib/alpha.ex", "lib/beta.ex")
}

// expectColdBuild checks that st holds what a cold build of rels gives.
func expectColdBuild(t *testing.T, st *store.Store, dir string, rels ...string) {
	t.Helper()
	cold, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cold.Close() }()
	for _, rel := range rels {
		p := filepath.Join(dir, rel)
		defs, refs, err := parser.ParseFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := cold.IndexFileWithRefs(p, defs, refs); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.Stats()
	want, _ := cold.Stats()
	if got != want {
		t.Errorf("index %+v, cold build %+v", got, want)
	}
	gs, _ := st.FileStates()
	ws, _ := cold.FileStates()
	for p, w := range ws {
		if g, ok := gs[p]; !ok || g.Mtime != w.Mtime {
			t.Errorf("%s: mtime %d, cold build %d", p, g.Mtime, w.Mtime)
		}
	}
}

// A site file that is really gone is removed from the index, and the rename
// succeeds without it: its calls are gone with it.
func TestRename_GoneSiteFileRemovedFromIndex(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	bPath := filepath.Join(dir, "lib/beta.ex")
	_ = os.Remove(bPath)

	if _, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar")); err != nil {
		t.Fatal(err)
	}
	a.index.backgroundWork.Wait()
	if has, _ := st.HasPath(bPath); has {
		t.Error("the gone file is still in the index")
	}
	if got := readFile(t, filepath.Join(dir, "lib/alpha.ex")); !strings.Contains(got, "def bar(x)") {
		t.Errorf("alpha.ex not renamed:\n%s", got)
	}
}

// After an editor moved a file itself (a rename operation in an earlier
// reply), the index has the old path until the watcher sees the move. The
// next rename indexes the new path, removes the old one, and succeeds.
func TestRename_EditorMoveNotIndexedYet(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", r3Alpha)
	indexFile(t, st, dir, "lib/beta.ex", r3Beta)
	aPath, gPath := filepath.Join(dir, "lib/alpha.ex"), filepath.Join(dir, "lib/gamma.ex")
	openIn(t, a, aPath, r3Alpha)
	edit, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(aPath)), 0, 17, "Gamma"))
	if err != nil {
		t.Fatal(err)
	}
	expectClientRename(t, edit, aPath, gPath)
	a.index.backgroundWork.Wait()
	// The editor applies the edit and the move, and moves its buffer.
	writeFile(t, gPath, strings.Replace(r3Alpha, "MyApp.Alpha", "MyApp.Gamma", 1))
	_ = os.Remove(aPath)
	a.docs.Close(string(uri.File(aPath)))

	indexed, err := a.RenameFunctionHeadless(context.Background(), "MyApp.Beta", "run", "go")
	if err != nil {
		t.Fatal(err)
	}
	<-indexed
	if got := readFile(t, gPath); !strings.Contains(got, "MyApp.Beta.go()") {
		t.Errorf("gamma.ex still calls run:\n%s", got)
	}
	if has, _ := st.HasPath(aPath); has {
		t.Error("the old path is still in the index")
	}
	expectColdBuild(t, st, dir, "lib/gamma.ex", "lib/beta.ex")
}

// More stale files than the rename indexes itself: a large change is being
// indexed, and the rename asks the user to wait.
func TestRename_ManyStaleFilesRefused(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	later := time.Now().Add(2 * time.Second)
	for _, name := range []string{"b1", "b2", "b3"} {
		rel := "lib/" + name + ".ex"
		indexFile(t, st, dir, rel, strings.Replace(safetyCallB, "MyApp.Beta", "MyApp."+strings.ToUpper(name), 1))
		_ = os.Chtimes(filepath.Join(dir, rel), later, later)
	}
	old := renameStaleLimit
	renameStaleLimit = 2
	t.Cleanup(func() { renameStaleLimit = old })
	_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
	if err == nil || !strings.Contains(err.Error(), "3 files changed since Dexter indexed them") {
		t.Fatalf("want the wait refusal, got %v", err)
	}
}

// A buffer of the requesting editor whose unsaved lines moved since the
// index read the file: a reindex from disk does not help, so the rename
// refuses instead of editing the wrong place. The same holds for a facade.
func TestRename_BufferLinesMovedRefused(t *testing.T) {
	want := func(rel string) string {
		return filepath.Join("lib", rel) + " changed in this editor since it was saved"
	}
	t.Run("call site", func(t *testing.T) {
		dir := t.TempDir()
		servers, st := sessionsAt(t, dir, 1)
		a := servers[0]
		indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
		indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
		openIn(t, a, filepath.Join(dir, "lib/beta.ex"), "# one\n# two\n"+safetyCallB)
		_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
		if err == nil || !strings.Contains(err.Error(), want("beta.ex")) {
			t.Fatalf("want a refusal that names lib/beta.ex, got %v", err)
		}
	})
	t.Run("facade", func(t *testing.T) {
		dir := t.TempDir()
		servers, st := sessionsAt(t, dir, 1)
		a := servers[0]
		indexFile(t, st, dir, "lib/impl.ex", facadeImpl)
		indexFile(t, st, dir, "lib/facade.ex", facadeFile)
		openIn(t, a, filepath.Join(dir, "lib/facade.ex"), "defmodule MyApp.Facade do\n  @moduledoc false\n  defdelegate foo(x), to: MyApp.Impl\nend\n")
		_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/impl.ex"))), 1, 6, "bar"))
		if err == nil || !strings.Contains(err.Error(), want("facade.ex")) {
			t.Fatalf("want a refusal that names lib/facade.ex, got %v", err)
		}
	})
}

// A file that changes on disk between the planning read and the replace (a
// git checkout, a formatter) stops the rename, and keeps its change.
func TestRename_DiskChangeBeforeCommitAborts(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	bPath := filepath.Join(dir, "lib/beta.ex")
	external := safetyCallB + "# formatted\n"
	t.Cleanup(func() { renameTestHookBeforeCommit = nil })
	renameTestHookBeforeCommit = func() { writeFile(t, bPath, external) }

	_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
	if err == nil || !strings.Contains(err.Error(), filepath.Join("lib", "beta.ex")+" changed on disk while the rename ran") {
		t.Fatalf("want a refusal that names lib/beta.ex, got %v", err)
	}
	if got := readFile(t, bPath); got != external {
		t.Errorf("the external change was lost:\n%s", got)
	}
	if got := readFile(t, filepath.Join(dir, "lib/alpha.ex")); got != safetyDefA {
		t.Errorf("alpha.ex changed:\n%s", got)
	}
	noTempFiles(t, dir)
}

// Dexter writes no file in place: a directory where no temp file can be made
// stops the rename, even when the file itself can be written.
func TestRename_NoTempFileRefusedNotWrittenInPlace(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory")
	}
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/ro/beta.ex", safetyCallB)
	roDir := filepath.Join(dir, "lib/ro")
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
	if err == nil || !strings.Contains(err.Error(), "cannot create a temporary file in "+filepath.Join("lib", "ro")) {
		t.Fatalf("want a refusal that names lib/ro, got %v", err)
	}
	if got := readFile(t, filepath.Join(roDir, "beta.ex")); got != safetyCallB {
		t.Errorf("beta.ex was written in place:\n%s", got)
	}
	if got := readFile(t, filepath.Join(dir, "lib/alpha.ex")); got != safetyDefA {
		t.Errorf("alpha.ex changed:\n%s", got)
	}
}

// A rename changes only the calls of the renamed module's function, not a
// function of the same name in another module on the same line.
func TestRename_OnlyTheTargetModulesCalls(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	core := servers[0]
	files := map[string][2]string{
		"lib/alpha.ex": {
			"defmodule MyApp.Alpha do\n  def foo(x), do: x\n  def both(x), do: foo(x) + MyApp.Facade.foo(x)\n  def self_call(x), do: __MODULE__.foo(x)\nend\n",
			"defmodule MyApp.Alpha do\n  def bar(x), do: x\n  def both(x), do: bar(x) + MyApp.Facade.foo(x)\n  def self_call(x), do: __MODULE__.bar(x)\nend\n",
		},
		"lib/facade.ex": {
			"defmodule MyApp.Facade do\n  def foo(x), do: x\nend\n",
			"defmodule MyApp.Facade do\n  def foo(x), do: x\nend\n",
		},
		"lib/caller.ex": {
			"defmodule MyApp.Caller do\n  alias MyApp.Alpha\n  alias MyApp.Facade\n  alias MyApp.Alpha, as: A\n\n" +
				"  def mixed, do: {Alpha.foo(1), Facade.foo(2), MyApp.Alpha.foo(3), MyApp.Facade.foo(4)}\n" +
				"  def captures, do: {&Alpha.foo/1, &Facade.foo/1}\n" +
				"  def pipes(x), do: x |> Alpha.foo() |> Facade.foo()\n" +
				"  def field(y), do: y.foo + Alpha.foo(4)\n" +
				"  def as_alias, do: A.foo(1) + Facade.foo(1)\n" +
				"end\n",
			"defmodule MyApp.Caller do\n  alias MyApp.Alpha\n  alias MyApp.Facade\n  alias MyApp.Alpha, as: A\n\n" +
				"  def mixed, do: {Alpha.bar(1), Facade.foo(2), MyApp.Alpha.bar(3), MyApp.Facade.foo(4)}\n" +
				"  def captures, do: {&Alpha.bar/1, &Facade.foo/1}\n" +
				"  def pipes(x), do: x |> Alpha.bar() |> Facade.foo()\n" +
				"  def field(y), do: y.foo + Alpha.bar(4)\n" +
				"  def as_alias, do: A.bar(1) + Facade.foo(1)\n" +
				"end\n",
		},
		"lib/importer.ex": {
			"defmodule MyApp.Importer do\n  import MyApp.Alpha, only: [foo: 1]\n\n  def run(x), do: foo(x) + MyApp.Facade.foo(x)\nend\n",
			"defmodule MyApp.Importer do\n  import MyApp.Alpha, only: [bar: 1]\n\n  def run(x), do: bar(x) + MyApp.Facade.foo(x)\nend\n",
		},
		"lib/local.ex": {
			"defmodule MyApp.Local do\n  def foo(x), do: x\n  def run(x), do: foo(x) + MyApp.Alpha.foo(x)\nend\n",
			"defmodule MyApp.Local do\n  def foo(x), do: x\n  def run(x), do: foo(x) + MyApp.Alpha.bar(x)\nend\n",
		},
	}
	for rel, texts := range files {
		indexFile(t, st, dir, rel, texts[0])
	}
	indexed, err := core.RenameFunctionHeadless(context.Background(), "MyApp.Alpha", "foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	<-indexed
	for rel, texts := range files {
		if got := readFile(t, filepath.Join(dir, rel)); got != texts[1] {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", rel, got, texts[1])
		}
	}
}

// The rename waits for the watcher only when a file it plans from is gone and
// no earlier reply moved it; the common case does not touch the barrier, and
// a barrier that does not finish refuses instead of dropping the file.
func TestRename_FlushForUnrecordedMoves(t *testing.T) {
	setup := func(t *testing.T) (*Server, string, *int) {
		dir := t.TempDir()
		servers, st := sessionsAt(t, dir, 1)
		a := servers[0]
		indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
		indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
		calls := new(int)
		a.index.SetEventFlusher(func(context.Context) error { *calls++; return nil })
		return a, dir, calls
	}
	rename := func(a *Server, dir string) error {
		_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
		a.index.backgroundWork.Wait()
		return err
	}

	t.Run("common case", func(t *testing.T) {
		a, dir, calls := setup(t)
		if err := rename(a, dir); err != nil {
			t.Fatal(err)
		}
		if *calls != 0 {
			t.Errorf("the barrier ran %d times for a rename with no gone file", *calls)
		}
	})
	t.Run("saved file", func(t *testing.T) {
		a, dir, calls := setup(t)
		p := filepath.Join(dir, "lib/beta.ex")
		writeFile(t, p, "# saved\n"+safetyCallB)
		later := time.Now().Add(2 * time.Second)
		_ = os.Chtimes(p, later, later)
		if err := rename(a, dir); err != nil {
			t.Fatal(err)
		}
		if *calls != 0 {
			t.Errorf("the barrier ran %d times for a saved file", *calls)
		}
	})
	t.Run("gone file", func(t *testing.T) {
		a, dir, calls := setup(t)
		_ = os.Remove(filepath.Join(dir, "lib/beta.ex"))
		// The watcher reports nothing (this flusher only counts): the file
		// is unexplained, so the rename stops once, with the index updated.
		err := rename(a, dir)
		if err == nil || !strings.Contains(err.Error(), filepath.Join("lib", "beta.ex")+" is gone, and the file watcher has not reported it") {
			t.Fatalf("want the gone-file refusal, got %v", err)
		}
		if *calls != 1 {
			t.Errorf("the barrier ran %d times, want 1", *calls)
		}
		if got := readFile(t, filepath.Join(dir, "lib/alpha.ex")); got != safetyDefA {
			t.Errorf("alpha.ex changed:\n%s", got)
		}
		if err := rename(a, dir); err != nil {
			t.Fatalf("the second try: %v", err)
		}
		if *calls != 1 {
			t.Errorf("the second try ran the barrier again")
		}
	})
	t.Run("barrier times out", func(t *testing.T) {
		a, dir, _ := setup(t)
		old := renameFlushWait
		renameFlushWait = 50 * time.Millisecond
		t.Cleanup(func() { renameFlushWait = old })
		a.index.SetEventFlusher(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
		_ = os.Remove(filepath.Join(dir, "lib/beta.ex"))
		err := rename(a, dir)
		if err == nil || !strings.Contains(err.Error(), "Dexter is still indexing a change") {
			t.Fatalf("want the still-indexing refusal, got %v", err)
		}
		if got := readFile(t, filepath.Join(dir, "lib/alpha.ex")); got != safetyDefA {
			t.Errorf("alpha.ex changed:\n%s", got)
		}
	})
}
