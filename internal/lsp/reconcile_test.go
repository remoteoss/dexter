package lsp

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func moduleSource(i, version int) string {
	return fmt.Sprintf(`defmodule MyApp.Gen%d do
  def run_v%d(id), do: SharedLib.Worker.perform(id)
end
`, i, version)
}

func setReconcileVars(t *testing.T, batchFiles, minFiles, share int) {
	t.Helper()
	oldBatch, oldMin, oldShare := reconcileBatchFiles, rebuildMinFiles, rebuildShare
	reconcileBatchFiles, rebuildMinFiles, rebuildShare = batchFiles, minFiles, share
	t.Cleanup(func() {
		reconcileBatchFiles, rebuildMinFiles, rebuildShare = oldBatch, oldMin, oldShare
	})
}

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// reindexOnce runs one background pass to completion.
func reindexOnce(t *testing.T, s *Server) {
	t.Helper()
	select {
	case <-s.startBackgroundReindex():
	case <-time.After(30 * time.Second):
		t.Fatal("reindex did not finish")
	}
}

// A warm pass writes a large change set through the rebuild and a small one
// through batches. Both must leave changed files with only their new rows and
// add every new file.
func TestReconcile_WarmChangeSetPaths(t *testing.T) {
	for _, tc := range []struct {
		name       string
		batchFiles int
		minFiles   int
		wantLog    string
	}{
		{name: "batched", batchFiles: 3, minFiles: 1 << 30, wantLog: ""},
		{name: "rebuild", batchFiles: 3, minFiles: 1, wantLog: "Rebuilt the index with 21 changed files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setReconcileVars(t, tc.batchFiles, tc.minFiles, 4)
			logs := captureLog(t)
			server, cleanup := setupTestServer(t)
			defer cleanup()

			writeTestFile(t, server.projectRoot, "lib/gen0.ex", moduleSource(0, 0))
			writeTestFile(t, server.projectRoot, "lib/kept.ex", "defmodule MyApp.Kept do\n  def here, do: :ok\nend\n")
			reindexOnce(t, server) // cold: full build

			// Change gen0 (with a different mtime) and add 20 files.
			path := writeTestFile(t, server.projectRoot, "lib/gen0.ex", moduleSource(0, 1))
			future := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, future, future); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 20; i++ {
				writeTestFile(t, server.projectRoot, fmt.Sprintf("lib/gen%d.ex", i), moduleSource(i, 0))
			}
			reindexOnce(t, server)

			if r, _ := server.store.LookupFunction("MyApp.Gen0", "run_v1"); len(r) != 1 {
				t.Error("changed file does not have its new definition")
			}
			if r, _ := server.store.LookupFunction("MyApp.Gen0", "run_v0"); len(r) != 0 {
				t.Error("changed file kept its old definition")
			}
			for i := 1; i <= 20; i++ {
				if r, _ := server.store.LookupFunction(fmt.Sprintf("MyApp.Gen%d", i), "run_v0"); len(r) != 1 {
					t.Errorf("new file %d: %d definitions, want 1", i, len(r))
				}
			}
			if r, _ := server.store.LookupFunction("MyApp.Kept", "here"); len(r) != 1 {
				t.Error("an unchanged file lost its definition")
			}
			if refs, _ := server.store.LookupReferences("SharedLib.Worker", "perform"); len(refs) != 21 {
				t.Errorf("references = %d, want 21", len(refs))
			}
			if tc.wantLog != "" && !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("log does not say %q:\n%s", tc.wantLog, logs.String())
			}

			// A pass with nothing changed writes nothing.
			before := logs.String()
			reindexOnce(t, server)
			if !strings.Contains(strings.TrimPrefix(logs.String(), before), "Background reindex: 0 files updated") {
				t.Errorf("a pass without changes wrote files:\n%s", strings.TrimPrefix(logs.String(), before))
			}
		})
	}
}

// When every changed file fails to parse, a rebuild has nothing to write. It
// must not copy and swap the tables, and the old rows of those files stay.
func TestReconcile_RebuildWithNoParsedFileChangesNothing(t *testing.T) {
	setReconcileVars(t, 3, 1, 4)
	logs := captureLog(t)
	server, cleanup := setupTestServer(t)
	defer cleanup()

	var paths []string
	for i := 0; i < 3; i++ {
		paths = append(paths, writeTestFile(t, server.projectRoot, fmt.Sprintf("lib/gen%d.ex", i), moduleSource(i, 0)))
	}
	reindexOnce(t, server) // cold: full build

	future := time.Now().Add(time.Hour)
	for _, path := range paths {
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	}
	before := logs.String()
	reindexOnce(t, server)
	pass := strings.TrimPrefix(logs.String(), before)
	if strings.Contains(pass, "Rebuilt the index") {
		t.Errorf("a rebuild with no parsed file swapped the tables:\n%s", pass)
	}
	if !strings.Contains(pass, "Index rebuild skipped") {
		t.Errorf("the log does not say that the rebuild was skipped:\n%s", pass)
	}
	for i := 0; i < 3; i++ {
		if r, _ := server.store.LookupFunction(fmt.Sprintf("MyApp.Gen%d", i), "run_v0"); len(r) != 1 {
			t.Errorf("file %d lost its old definition: %d rows", i, len(r))
		}
	}
}

// CancelWork must end a warm pass in flight promptly, leave no file half
// written, and leave the rest for the next start, which finishes it from the
// stored mtimes. This is what shutdown relies on.
func TestReconcile_CancelWorkStopsPassAndNextStartFinishes(t *testing.T) {
	const files = 40
	for _, tc := range []struct {
		name     string
		minFiles int
	}{
		{name: "batched", minFiles: 1 << 30},
		{name: "rebuild", minFiles: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setReconcileVars(t, 4, tc.minFiles, 4)
			server, cleanup := setupTestServer(t)
			defer cleanup()

			writeTestFile(t, server.projectRoot, "lib/gen0.ex", moduleSource(0, 0))
			reindexOnce(t, server)
			for i := 1; i <= files; i++ {
				writeTestFile(t, server.projectRoot, fmt.Sprintf("lib/gen%d.ex", i), moduleSource(i, 0))
			}

			// Let some files through, then hold every parse until the pass is
			// canceled, as a long pass would be at shutdown.
			var parsed sync.WaitGroup
			parsed.Add(10)
			var mu sync.Mutex
			count := 0
			testHookParse = func(string) {
				mu.Lock()
				count++
				n := count
				mu.Unlock()
				if n <= 10 {
					parsed.Done()
					return
				}
				<-server.index.work.Done()
			}
			t.Cleanup(func() { testHookParse = nil })

			done := server.startBackgroundReindex()
			parsed.Wait()
			canceled := time.Now()
			server.index.CancelWork()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the pass did not stop after CancelWork")
			}
			if waited := time.Since(canceled); waited > 2*time.Second {
				t.Errorf("the pass took %s to stop", waited)
			}
			testHookParse = nil

			// No half-written file: every stored file has its definition.
			paths, err := server.store.ListFilePaths()
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range paths {
				mods, err := server.store.LookupModulesInFile(p)
				if err != nil || len(mods) != 1 {
					t.Errorf("%s is stored without its module (%v, %v)", p, mods, err)
				}
			}
			if len(paths) > files {
				t.Errorf("a canceled pass stored all %d files", len(paths))
			}

			// The next start, with a new coordinator, finishes the work.
			next := NewServer(server.store, server.projectRoot)
			reindexOnce(t, next)
			for i := 0; i <= files; i++ {
				if r, _ := next.store.LookupFunction(fmt.Sprintf("MyApp.Gen%d", i), "run_v0"); len(r) != 1 {
					t.Errorf("file %d not indexed after the next start", i)
				}
			}
		})
	}
}

// A canceled cold build leaves an empty index, which the next start builds.
func TestReconcile_CancelWorkBeforeColdBuild(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	writeTestFile(t, server.projectRoot, "lib/gen1.ex", moduleSource(1, 0))

	server.index.CancelWork()
	reindexOnce(t, server)
	if !server.store.IsEmpty() {
		t.Error("a canceled cold build stored files")
	}
	if names, err := server.store.IndexNames(); err != nil || len(names) != 6 {
		t.Errorf("indexes after a canceled cold build: %v (%v)", names, err)
	}

	next := NewServer(server.store, server.projectRoot)
	reindexOnce(t, next)
	if r, _ := next.store.LookupFunction("MyApp.Gen1", "run_v0"); len(r) != 1 {
		t.Error("the next start did not build the index")
	}
}

// Removals take the coordinator's context, so CancelWork stops them as it
// stops a pass. Before, they ran with no context and could hold shutdown for
// a whole rebuild of the symbol tables.
func TestServer_RemoveFilesStopsAfterCancelWork(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	var paths []string
	for i := 0; i < 5; i++ {
		paths = append(paths, writeTestFile(t, server.projectRoot, fmt.Sprintf("lib/gen%d.ex", i), moduleSource(i, 0)))
	}
	reindexOnce(t, server)

	server.index.CancelWork()
	server.RemoveFiles(paths)
	server.RemoveFilesUnderRoot(server.projectRoot)
	if stored, _ := server.store.ListFilePaths(); len(stored) != len(paths) {
		t.Errorf("%d files left after canceled removals, want %d", len(stored), len(paths))
	}
}

// One file that fails to write must not take the other files of its batch
// with it: the batch is written again one file at a time.
func TestReconcile_FailedWriteLosesOnlyThatFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		minFiles int
		wantLog  []string
	}{
		{name: "batched", minFiles: 1 << 30, wantLog: []string{"writing them one by one"}},
		// A failed rebuild falls back to batches, which then retry one by one.
		{name: "rebuild", minFiles: 1, wantLog: []string{"writing 10 changed files in batches", "writing them one by one"}},
	} {
		t.Run(tc.name, func(t *testing.T) { testFailedWrite(t, tc.minFiles, tc.wantLog) })
	}
}

func testFailedWrite(t *testing.T, minFiles int, wantLog []string) {
	setReconcileVars(t, 1000, minFiles, 4)
	logs := captureLog(t)
	server, cleanup := setupTestServer(t)
	defer cleanup()
	writeTestFile(t, server.projectRoot, "lib/gen0.ex", moduleSource(0, 0))
	reindexOnce(t, server)

	var bad string
	for i := 1; i <= 10; i++ {
		p := writeTestFile(t, server.projectRoot, fmt.Sprintf("lib/gen%d.ex", i), moduleSource(i, 0))
		if i == 5 {
			bad = p
		}
	}
	testHookWrite = func(path string) error {
		if path == bad {
			return fmt.Errorf("injected write failure")
		}
		return nil
	}
	t.Cleanup(func() { testHookWrite = nil })
	reindexOnce(t, server)
	testHookWrite = nil

	for i := 1; i <= 10; i++ {
		want := 1
		if i == 5 {
			want = 0
		}
		if r, _ := server.store.LookupFunction(fmt.Sprintf("MyApp.Gen%d", i), "run_v0"); len(r) != want {
			t.Errorf("file %d: %d definitions, want %d", i, len(r), want)
		}
	}
	for _, want := range wantLog {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log does not say %q:\n%s", want, logs.String())
		}
	}

	// The failed file kept no stored mtime, so the next pass writes it.
	reindexOnce(t, server)
	if r, _ := server.store.LookupFunction("MyApp.Gen5", "run_v0"); len(r) != 1 {
		t.Error("the next pass did not write the file that failed")
	}
}

// In an embedded server, a watched-file change waits for the coordinator's
// mutation lock, as the daemon's events do. Before, it took only the read side
// of the write lock and raced the pass's batches for SQLite's write lock: it
// could fail after busy_timeout and stay unindexed until the next pass.
func TestServer_EmbeddedFileChangeWaitsForReconcile(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	writeTestFile(t, server.projectRoot, "lib/gen0.ex", moduleSource(0, 0))
	reindexOnce(t, server)

	path := writeTestFile(t, server.projectRoot, "lib/saved.ex", "defmodule MyApp.Saved do\n  def ok, do: :ok\nend\n")
	server.index.reindexing.Lock() // a pass in flight
	if err := server.DidChangeWatchedFiles(context.Background(), &protocol.DidChangeWatchedFilesParams{
		Changes: []*protocol.FileEvent{{URI: uri.File(path), Type: protocol.FileChangeTypeChanged}},
	}); err != nil {
		server.index.reindexing.Unlock()
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	early, _ := server.store.LookupFunction("MyApp.Saved", "ok")
	server.index.reindexing.Unlock()
	if len(early) != 0 {
		t.Error("the change was written while a pass held the mutation lock")
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if r, _ := server.store.LookupFunction("MyApp.Saved", "ok"); len(r) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the change was not indexed after the pass")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
