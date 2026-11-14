package lsp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
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
	if os.Geteuid() == 0 {
		t.Skip("running as root, a file with mode 0 is still readable")
	}
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
	if err := server.RemoveFiles(paths); err != nil {
		t.Fatal(err)
	}
	if err := server.RemoveFilesUnderRoot(server.projectRoot); err != nil {
		t.Fatal(err)
	}
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
	restore := SetTestWriteFault(func(path string) error {
		if path == bad {
			return fmt.Errorf("injected write failure")
		}
		return nil
	})
	t.Cleanup(restore)
	reindexOnce(t, server)
	restore()

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

// reindexResult runs one background pass to completion and returns its result.
func reindexResult(t *testing.T, s *Server) error {
	t.Helper()
	select {
	case err := <-s.startBackgroundReindex():
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("reindex did not finish")
	}
	return nil
}

// A project root that is moved, deleted, or unmounted while the workspace runs
// walks as an empty project. Before, the walk error was ignored and the prune
// deleted every stored file: the database moved with the root, and it was left
// empty. Now the pass stops and changes nothing, whenever the root goes.
func TestReconcile_RootGoneLeavesIndexUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gone makes the root go away. It runs before the pass, or, with
		// duringPass, while the pass parses a changed file: after the walk and
		// before the prune.
		gone       func(t *testing.T, root string)
		duringPass bool
		noIdle     bool
	}{
		{name: "moved", gone: moveRoot},
		{name: "replaced by a new directory", gone: func(t *testing.T, root string) {
			moveRoot(t, root)
			if err := os.Mkdir(root, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "moved during the pass", gone: moveRoot, duringPass: true},
		// With no idle connection, the prune's read opens a new one by the
		// index's path, which went with the root, and fails: the result is
		// still ErrRootGone.
		{name: "moved during the pass, no idle connection", gone: moveRoot, duringPass: true, noIdle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			server, cleanup := setupTestServer(t)
			defer cleanup()
			root := server.projectRoot
			for i := 0; i < 5; i++ {
				writeTestFile(t, root, fmt.Sprintf("lib/gen%d.ex", i), moduleSource(i, 0))
			}
			if err := reindexResult(t, server); err != nil {
				t.Fatal(err)
			}
			before, _ := server.store.ListFilePaths()
			if len(before) != 5 {
				t.Fatalf("setup: %d files indexed, want 5", len(before))
			}

			if tc.noIdle {
				server.store.SetMaxIdleConns(0)
				t.Cleanup(func() { server.store.SetMaxIdleConns(2) })
			}
			if tc.duringPass {
				// A deleted file makes the prune delete something, and a
				// changed file makes the pass parse it, after the walk.
				if err := os.Remove(filepath.Join(root, "lib/gen4.ex")); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, root, "lib/gen0.ex", moduleSource(0, 1))
				future := time.Now().Add(time.Hour)
				if err := os.Chtimes(filepath.Join(root, "lib/gen0.ex"), future, future); err != nil {
					t.Fatal(err)
				}
				var once sync.Once
				testHookParse = func(string) { once.Do(func() { tc.gone(t, root) }) }
				t.Cleanup(func() { testHookParse = nil })
			} else {
				tc.gone(t, root)
			}

			err := reindexResult(t, server)
			testHookParse = nil
			after, _ := server.store.ListFilePaths()
			if tc.noIdle {
				// The pool cannot open the index by its old path any more:
				// read it where it went.
				moved, openErr := store.Open(root + "-moved")
				if openErr != nil {
					t.Fatal(openErr)
				}
				after, _ = moved.ListFilePaths()
				_ = moved.Close()
			}
			if len(after) != len(before) {
				t.Fatalf("%d files in the index after the root went, want %d", len(after), len(before))
			}
			if !errors.Is(err, ErrRootGone) {
				t.Fatalf("pass result = %v, want ErrRootGone", err)
			}
			if !strings.Contains(logs.String(), "project root is gone") {
				t.Errorf("log does not say the root is gone:\n%s", logs.String())
			}
		})
	}
}

func moveRoot(t *testing.T, root string) {
	t.Helper()
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatal(err)
	}
}

// Write failures (a full disk) are counted apart from the files written, the
// log says how many failed, and the pass reports ErrIncomplete so that the
// workspace schedules a retry. Before, the pass reported success. Once writes
// work again, the next pass writes the files that failed.
func TestReconcile_WriteFailuresAreReported(t *testing.T) {
	setReconcileVars(t, 4, 1<<30, 4)
	logs := captureLog(t)
	server, cleanup := setupTestServer(t)
	defer cleanup()
	writeTestFile(t, server.projectRoot, "lib/gen0.ex", moduleSource(0, 0))
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}

	failing := make(map[string]bool)
	for i := 1; i <= 10; i++ {
		p := writeTestFile(t, server.projectRoot, fmt.Sprintf("lib/gen%d.ex", i), moduleSource(i, 0))
		if i%4 == 0 || i == 1 {
			failing[p] = true // 1, 4, 8
		}
	}
	restore := SetTestWriteFault(func(path string) error {
		if failing[path] {
			return errors.New("database or disk is full")
		}
		return nil
	})
	t.Cleanup(restore)
	err := reindexResult(t, server)
	restore()
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("pass result = %v, want ErrIncomplete", err)
	}
	if !strings.Contains(logs.String(), "Background reindex: 7 files updated, 3 failed") {
		t.Errorf("log does not count the failed writes:\n%s", logs.String())
	}

	if err := reindexResult(t, server); err != nil {
		t.Fatalf("pass after the disk was freed = %v, want nil", err)
	}
	for i := 1; i <= 10; i++ {
		if r, _ := server.store.LookupFunction(fmt.Sprintf("MyApp.Gen%d", i), "run_v0"); len(r) != 1 {
			t.Errorf("file %d not indexed after writes work again", i)
		}
	}
}

// A directory the index holds can become a symlink to another directory of the
// tree while no daemon runs, as a checkout can make it. The walk does not
// follow a symlinked directory, but the old paths still stat through the link,
// so the prune kept them, and their rows duplicated the files at their real
// path forever. A warm pass must leave the same index as a cold build.
//
// The project root itself can be spelled through a symlink; the walk follows
// it, so that link must not count.
func TestReconcile_DirectoryReplacedBySymlinkMatchesColdBuild(t *testing.T) {
	t.Run("real root", func(t *testing.T) { testDirectoryReplacedBySymlink(t, false) })
	t.Run("root spelled through a symlink", func(t *testing.T) { testDirectoryReplacedBySymlink(t, true) })
}

func testDirectoryReplacedBySymlink(t *testing.T, linkedRoot bool) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	if linkedRoot {
		link := filepath.Join(t.TempDir(), "project")
		if err := os.Symlink(server.projectRoot, link); err != nil {
			t.Fatal(err)
		}
		server.projectRoot = link
	}
	root := server.projectRoot
	writeTestFile(t, root, "lib/main.ex", moduleSource(0, 0))
	for i := 1; i <= 4; i++ {
		writeTestFile(t, root, fmt.Sprintf("tools/skills/skill%d.exs", i), moduleSource(i, 0))
	}
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}

	// tools/skills -> ../agents/skills, with the same files.
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "tools", "skills"), filepath.Join(root, "agents", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "agents", "skills"), filepath.Join(root, "tools", "skills")); err != nil {
		t.Fatal(err)
	}
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}

	coldStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = coldStore.Close() }()
	cold := NewServer(coldStore, root)
	if err := reindexResult(t, cold); err != nil {
		t.Fatal(err)
	}
	cold.index.backgroundWork.Wait()

	warmPaths, _ := server.store.ListFilePaths()
	coldPaths, _ := coldStore.ListFilePaths()
	sort.Strings(warmPaths)
	sort.Strings(coldPaths)
	if strings.Join(warmPaths, "\n") != strings.Join(coldPaths, "\n") {
		t.Fatalf("warm index files differ from a cold build:\nwarm: %v\ncold: %v", warmPaths, coldPaths)
	}
	warmStats, _ := server.store.Stats()
	coldStats, _ := coldStore.Stats()
	if warmStats != coldStats {
		t.Fatalf("warm index %+v, cold build %+v", warmStats, coldStats)
	}
}

// An autofs, NFS, or FUSE remount can give the same root directory a new
// device and inode. The database file is still at its path, so the root is not
// gone: the pass goes on, and the index stays. Before, any new identity
// stopped the daemon and every editor session.
func TestReconcile_RootWithNewIdentityButSameIndexIsNotGone(t *testing.T) {
	logs := captureLog(t)
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	writeTestFile(t, root, "lib/gen0.ex", moduleSource(0, 0))
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}
	// Simulate the remount: the recorded identity is another directory's.
	other, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server.index.rootMu.Lock()
	server.index.rootInfo = other
	server.index.rootMu.Unlock()

	writeTestFile(t, root, "lib/gen1.ex", moduleSource(1, 0))
	if err := reindexResult(t, server); err != nil {
		t.Fatalf("pass after a remount = %v, want nil", err)
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen1", "run_v0"); len(r) != 1 {
		t.Error("the pass after a remount did not index a new file")
	}
	if !strings.Contains(logs.String(), "has a new file identity") {
		t.Errorf("log does not tell about the new identity:\n%s", logs.String())
	}
}

// A stat error that is not ENOENT or ENOTDIR (EACCES here; EPERM, EIO, ESTALE
// on other systems) can clear again. The pass changes nothing and reports
// ErrIncomplete, so a retry comes, and the next pass works.
func TestReconcile_UnreadableRootIsIncompleteNotGone(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	writeTestFile(t, root, "lib/gen0.ex", moduleSource(0, 0))
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}
	prev := statRoot
	statRoot = func(string) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "stat", Path: root, Err: syscall.EACCES}
	}
	t.Cleanup(func() { statRoot = prev })
	writeTestFile(t, root, "lib/gen1.ex", moduleSource(1, 0))
	if err := os.Remove(filepath.Join(root, "lib/gen0.ex")); err != nil {
		t.Fatal(err)
	}
	err := reindexResult(t, server)
	statRoot = prev
	if errors.Is(err, ErrRootGone) || !errors.Is(err, ErrIncomplete) {
		t.Fatalf("pass with an unreadable root = %v, want ErrIncomplete", err)
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen0", "run_v0"); len(r) != 1 {
		t.Error("a pass with an unreadable root pruned a file")
	}
	if err := reindexResult(t, server); err != nil {
		t.Fatalf("pass after the error cleared = %v, want nil", err)
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen1", "run_v0"); len(r) != 1 {
		t.Error("the pass after the error cleared did not index the new file")
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen0", "run_v0"); len(r) != 0 {
		t.Error("the pass after the error cleared did not prune the deleted file")
	}
}

// A symlinked .ex file has the target's mtime stored, as every write path
// records it. The walk compared the link's own mtime with it, so every pass
// parsed and wrote the file again.
func TestReconcile_SymlinkedFileIsNotWrittenAgain(t *testing.T) {
	logs := captureLog(t)
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	writeTestFile(t, root, "lib/gen0.ex", moduleSource(0, 0))
	target := writeTestFile(t, t.TempDir(), "shared.ex", moduleSource(1, 0))
	if err := os.Symlink(target, filepath.Join(root, "lib", "shared.ex")); err != nil {
		t.Fatal(err)
	}
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 3; pass++ {
		before := logs.String()
		if err := reindexResult(t, server); err != nil {
			t.Fatal(err)
		}
		if out := strings.TrimPrefix(logs.String(), before); !strings.Contains(out, "Background reindex: 0 files updated") {
			t.Fatalf("pass %d with no change wrote files:\n%s", pass, out)
		}
	}
	// A change to the target is still seen.
	future := time.Now().Add(time.Hour)
	if err := os.WriteFile(target, []byte(moduleSource(1, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatal(err)
	}
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen1", "run_v1"); len(r) != 1 {
		t.Error("a change to the symlink's target was not indexed")
	}
}

// A directory replaced by a file makes the old paths below it fail with
// ENOTDIR, not ENOENT. The prune kept them, and their modules still resolved.
func TestReconcile_DirectoryReplacedByFileIsPruned(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	writeTestFile(t, root, "lib/gen0.ex", moduleSource(0, 0))
	writeTestFile(t, root, "lib/a/b.ex", moduleSource(1, 0))
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "lib", "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "a"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen1", "run_v0"); len(r) != 0 {
		t.Error("a file below a directory that is now a file is still indexed")
	}
}

// The index holds only paths that the walk yields. A single-file write of a
// path outside the root, of the root spelled through a symlink, or below a
// symlinked directory wrote rows that no prune removed (outside) or that
// duplicated the walk's rows. The prune also removes such rows that an older
// version wrote.
func TestServer_SingleFileWriteOnlyIndexesWalkablePaths(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	writeTestFile(t, root, "lib/real/gen0.ex", moduleSource(0, 0))
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}

	outside := writeTestFile(t, t.TempDir(), "scratch.ex", moduleSource(1, 0))
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "lib", "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		outside,
		filepath.Join(alias, "lib", "real", "gen0.ex"),
		filepath.Join(root, "lib", "link", "gen0.ex"),
	} {
		if err := server.indexOneFile(path); err != nil {
			t.Fatal(err)
		}
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen1", "run_v0"); len(r) != 0 {
		t.Error("a file outside the root was indexed")
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen0", "run_v0"); len(r) != 1 {
		t.Errorf("MyApp.Gen0 has %d definitions, want 1", len(r))
	}

	// Rows that an older version wrote for a path outside the root go.
	if err := server.store.IndexFileWithRefs(outside, []parser.Definition{{Module: "MyApp.Gen1", Function: "run_v0", Kind: "def", Line: 2, FilePath: outside}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen1", "run_v0"); len(r) != 0 {
		t.Error("the prune kept a row outside the root")
	}
}

type recordedEvents struct {
	mu        sync.Mutex
	reconcile []string
}

func (e *recordedEvents) ReconcileFile(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reconcile = append(e.reconcile, path)
}
func (e *recordedEvents) ReconcileSavedFile(path string)              { e.ReconcileFile(path) }
func (e *recordedEvents) RemoveFile(string)                           {}
func (e *recordedEvents) SetStdlibRoot(context.Context, string) error { return nil }

// A rename writes the changed files itself. A write that failed was ignored,
// so the file kept its old rows with nothing to repair them. Now the session
// gives the failed paths to the workspace, which retries them.
func TestServer_RenameReportsFailedWrites(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	events := &recordedEvents{}
	server.workspaceEvents = events
	disk := writeTestFile(t, server.projectRoot, "lib/gen0.ex", moduleSource(0, 0))
	text := writeTestFile(t, server.projectRoot, "lib/gen1.ex", moduleSource(1, 0))
	gone := filepath.Join(server.projectRoot, "lib", "old.ex")
	restore := SetTestWriteFault(func(string) error { return errors.New("database or disk is full") })
	t.Cleanup(restore)
	server.reindexAfterRename([]string{gone}, []string{disk}, []textReindex{{path: text, text: moduleSource(1, 1)}})
	server.index.backgroundWork.Wait()
	restore()

	events.mu.Lock()
	defer events.mu.Unlock()
	sort.Strings(events.reconcile)
	want := []string{disk, text, gone}
	sort.Strings(want)
	if strings.Join(events.reconcile, ",") != strings.Join(want, ",") {
		t.Fatalf("paths given to the workspace = %v, want %v", events.reconcile, want)
	}
}

// A root that was moved and replaced by a copy has a copy of the index at the
// index path, another file than the open one. That is gone, not a remount:
// before, the check only looked for some file at the path, and the writes went
// on into the moved index.
func TestReconcile_RootReplacedByCopyIsGone(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	writeTestFile(t, root, "lib/gen0.ex", moduleSource(0, 0))
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}
	moveRoot(t, root)
	if out, err := exec.Command("cp", "-R", root+"-moved", root).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}
	writeTestFile(t, root, "lib/gen1.ex", moduleSource(1, 0))
	if err := reindexResult(t, server); !errors.Is(err, ErrRootGone) {
		t.Fatalf("pass after the root was replaced by a copy = %v, want ErrRootGone", err)
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen1", "run_v0"); len(r) != 0 {
		t.Error("the copy's new file was written into the moved index")
	}
}

// The walk skips _build, .git, and node_modules, so a single-file write must
// too, and the prune removes such rows that an older version wrote.
func TestServer_SingleFileWriteSkipsWalkSkippedDirs(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	writeTestFile(t, root, "lib/gen0.ex", moduleSource(0, 0))
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}
	dep := writeTestFile(t, root, "node_modules/pkg/lib/x.ex", moduleSource(1, 0))
	build := writeTestFile(t, root, "_build/dev/lib/y.ex", moduleSource(2, 0))
	for _, path := range []string{dep, build} {
		if err := server.indexOneFile(path); err != nil {
			t.Fatal(err)
		}
	}
	for _, mod := range []string{"MyApp.Gen1", "MyApp.Gen2"} {
		if r, _ := server.store.LookupFunction(mod, "run_v0"); len(r) != 0 {
			t.Errorf("%s from a skipped directory was indexed", mod)
		}
	}
	if err := server.store.IndexFileWithRefs(dep, []parser.Definition{{Module: "MyApp.Gen1", Function: "run_v0", Kind: "def", Line: 2, FilePath: dep}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := reindexResult(t, server); err != nil {
		t.Fatal(err)
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen1", "run_v0"); len(r) != 0 {
		t.Error("the prune kept a row from a skipped directory")
	}
}

// git keeps the record of a worktree after its .git file is gone, as during
// git worktree remove, and the walk skips the checkout until git worktree
// prune. A single-file write must skip it too. The records are cached, and a
// new record is seen at once.
func TestServer_SingleFileWriteSkipsRecordedWorktree(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	if err := os.MkdirAll(filepath.Join(root, ".git", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	records := filepath.Join(root, ".git", "worktrees")
	setMtime := func(ago time.Duration) {
		t.Helper()
		at := time.Now().Add(-ago)
		if err := os.Chtimes(records, at, at); err != nil {
			t.Fatal(err)
		}
	}
	// Old mtimes, so that the cache trusts what it read (see racyWindow).
	setMtime(time.Hour)
	path := writeTestFile(t, root, "wt/lib/x.ex", moduleSource(1, 0))
	if !server.Indexable(path) {
		t.Fatal("setup: a path with no worktree record is not indexable")
	}
	// The record of wt, with no .git file in wt.
	writeTestFile(t, root, ".git/worktrees/wt/gitdir", filepath.Join(root, "wt", ".git")+"\n")
	setMtime(30 * time.Minute)
	if server.Indexable(path) {
		t.Fatal("a path in a recorded worktree is indexable")
	}
	if err := server.indexOneFile(path); err != nil {
		t.Fatal(err)
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen1", "run_v0"); len(r) != 0 {
		t.Error("a file in a recorded worktree was indexed")
	}
}

// git worktree move rewrites only the gitdir file in the record, so the
// records directory keeps its mtime and the cached set still names the old
// path. A plain directory made later at that path was rejected for good. A
// rejection now reads the records again first.
func TestServer_IndexableSeesWorktreeMovedAway(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	records := filepath.Join(root, ".git", "worktrees")
	gitdir := writeTestFile(t, root, ".git/worktrees/wt/gitdir", filepath.Join(root, "wt", ".git")+"\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(records, old, old); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "wt", "lib", "b.ex")
	if server.Indexable(path) {
		t.Fatal("setup: a path in a recorded worktree is indexable")
	}
	// The move: the record now names a checkout outside the root.
	if err := os.WriteFile(gitdir, []byte(filepath.Join(t.TempDir(), "wt", ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(records, old, old); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "wt/lib/b.ex", moduleSource(1, 0))
	if err := server.indexOneFile(path); err != nil {
		t.Fatal(err)
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen1", "run_v0"); len(r) != 1 {
		t.Fatal("a file at the old path of a moved worktree was not indexed")
	}
}

// A panic in the rebuild of a large change set ran the caller's deferred
// RUnlock on a lock that the rebuild had given up: "fatal error: sync: RUnlock
// of unlocked RWMutex", which no recover stops. The changed files kept their
// mtimes, so every new daemon did it again. Now the pass recovers, the open
// transaction rolls back, and the next pass writes the files.
func TestReconcile_PanicInRebuildIsRecoveredAndTheNextPassFinishes(t *testing.T) {
	setReconcileVars(t, 3, 1, 4)
	captureLog(t)
	server, cleanup := setupTestServer(t)
	defer cleanup()
	server.index.reporter.SetLogf(func(string, ...any) {})
	faults := make(chan string, 4)
	server.index.OnFault(func(what string) { faults <- what })

	writeTestFile(t, server.projectRoot, "lib/gen0.ex", moduleSource(0, 0))
	reindexOnce(t, server) // cold: full build
	for i := 1; i <= 20; i++ {
		writeTestFile(t, server.projectRoot, fmt.Sprintf("lib/gen%d.ex", i), moduleSource(i, 0))
	}
	var armed sync.Once
	restore := SetTestWriteFault(func(string) error {
		armed.Do(func() { panic("rebuild bug") })
		return nil
	})
	t.Cleanup(restore)
	reindexOnce(t, server)
	select {
	case <-faults:
	case <-time.After(5 * time.Second):
		t.Fatal("the panic was not reported as a fault")
	}
	if r, _ := server.store.LookupFunction("MyApp.Gen5", "run_v0"); len(r) != 0 {
		t.Fatal("the rebuild that panicked committed rows")
	}
	reindexOnce(t, server)
	for i := 1; i <= 20; i++ {
		if r, _ := server.store.LookupFunction(fmt.Sprintf("MyApp.Gen%d", i), "run_v0"); len(r) != 1 {
			t.Fatalf("file %d after the next pass: %d definitions, want 1", i, len(r))
		}
	}
}

// After the workspace waited for its index work, no new work starts: a rename
// that outlives its session cannot add work to a group that is being waited
// for, or write to a closed store.
func TestServer_NoIndexWorkAfterWaitForIndexWork(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	path := writeTestFile(t, server.projectRoot, "lib/late.ex", moduleSource(7, 0))
	server.WaitForIndexWork()

	select {
	case err := <-server.startBackgroundReindex():
		if err != nil {
			t.Errorf("a pass after the wait: %v, want a canceled pass", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a pass after the wait did not end")
	}
	select {
	case <-server.reindexAfterRename(nil, []string{path}, nil):
	case <-time.After(5 * time.Second):
		t.Fatal("an index update after the wait did not end")
	}
	if has, _ := server.store.HasPath(path); has {
		t.Error("index work ran after the wait")
	}
	server.index.backgroundWork.Wait()
}
