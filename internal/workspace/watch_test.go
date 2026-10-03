package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/remoteoss/dexter/internal/parser"
)

type watchAddStub struct {
	mu      sync.Mutex
	failing map[string]bool
	calls   []string
}

func (s *watchAddStub) add(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, path)
	if s.failing[path] {
		return errors.New("watch limit reached")
	}
	return nil
}

func (s *watchAddStub) setFailing(path string, failing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[path] = failing
}

func (s *watchAddStub) resetCalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

func (s *watchAddStub) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func newTestWatcher(add func(string) error, changed func(bool)) *fsnotifyWatcher {
	return &fsnotifyWatcher{add: add, failed: make(map[string]struct{}), onCoverageChange: changed}
}

func TestWatcherRetriesOnlyFailedDirectoriesAndReportsRestoration(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good")
	failed := filepath.Join(root, "failed")
	stillFailed := filepath.Join(root, "still-failed")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(failed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stillFailed, 0o755); err != nil {
		t.Fatal(err)
	}

	stub := &watchAddStub{failing: map[string]bool{failed: true, stillFailed: true}}
	var transitions []bool
	w := newTestWatcher(stub.add, func(degraded bool) { transitions = append(transitions, degraded) })
	w.watchTree(root)
	if got := w.failedDirectories(); !slices.Equal(got, []string{failed, stillFailed}) {
		t.Fatalf("failed directories = %v, want [%s %s]", got, failed, stillFailed)
	}
	if !slices.Equal(transitions, []bool{true}) {
		t.Fatalf("coverage transitions = %v, want [true]", transitions)
	}

	stub.resetCalls()
	w.retryFailed()
	if got := stub.paths(); !slices.Equal(got, []string{failed, stillFailed}) {
		t.Fatalf("retry paths = %v, want [%s %s]", got, failed, stillFailed)
	}
	if !slices.Equal(transitions, []bool{true}) {
		t.Fatalf("repeated failure emitted another transition: %v", transitions)
	}

	stub.setFailing(failed, false)
	stub.resetCalls()
	w.retryFailed()
	if got := stub.paths(); !slices.Equal(got, []string{failed, stillFailed}) {
		t.Fatalf("restoration retry paths = %v, want [%s %s]", got, failed, stillFailed)
	}
	if got := w.failedDirectories(); !slices.Equal(got, []string{stillFailed}) {
		t.Fatalf("failed directories after partial restoration = %v", got)
	}
	if !slices.Equal(transitions, []bool{true, true}) {
		t.Fatalf("coverage transitions = %v, want [true true]", transitions)
	}

	stub.setFailing(stillFailed, false)
	w.retryFailed()
	if !slices.Equal(transitions, []bool{true, true, false}) {
		t.Fatalf("coverage transitions = %v, want [true true false]", transitions)
	}
}

func TestWatcherTracksStartupTotalFailure(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "lib")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	stub := &watchAddStub{failing: map[string]bool{root: true, child: true}}
	var transitions []bool
	w := newTestWatcher(stub.add, func(degraded bool) { transitions = append(transitions, degraded) })
	if watched := w.watchTree(root); watched != 0 {
		t.Fatalf("watched %d directories during total failure", watched)
	}
	if got := w.failedDirectories(); !slices.Equal(got, []string{root, child}) {
		t.Fatalf("failed directories = %v, want [%s %s]", got, root, child)
	}
	if !slices.Equal(transitions, []bool{true}) {
		t.Fatalf("coverage transitions = %v, want [true]", transitions)
	}

	stub.setFailing(root, false)
	stub.setFailing(child, false)
	w.retryFailed()
	if !slices.Equal(transitions, []bool{true, true, false}) {
		t.Fatalf("coverage transitions after recovery = %v, want [true true false]", transitions)
	}
}

func TestWatcherTracksFailureForNewDirectory(t *testing.T) {
	root := t.TempDir()
	created := filepath.Join(root, "new")
	if err := os.Mkdir(created, 0o755); err != nil {
		t.Fatal(err)
	}

	stub := &watchAddStub{failing: map[string]bool{created: true}}
	var transitions []bool
	w := newTestWatcher(stub.add, func(degraded bool) { transitions = append(transitions, degraded) })
	w.watchTree(created)
	if got := w.failedDirectories(); !slices.Equal(got, []string{created}) {
		t.Fatalf("failed directories = %v, want [%s]", got, created)
	}
	nested := filepath.Join(created, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	stub.setFailing(created, false)
	stub.resetCalls()
	w.retryFailed()
	if got := stub.paths(); !slices.Equal(got, []string{created, nested}) {
		t.Fatalf("restoration watch paths = %v, want [%s %s]", got, created, nested)
	}
	if !slices.Equal(transitions, []bool{true, false}) {
		t.Fatalf("coverage transitions = %v, want [true false]", transitions)
	}
}

func TestIgnoredWatchPath(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, ".git", "HEAD"),
		filepath.Join(root, "deps", "library", "lib", "module.ex"),
		filepath.Join(root, "assets", "node_modules", "package", "index.js"),
		filepath.Join(root, ".dexter", "index.db"),
	} {
		if !ignoredWatchPath(root, path) {
			t.Errorf("ignoredWatchPath(%q) = false", path)
		}
	}
	if path := filepath.Join(root, "apps", "my_app", "lib", "module.ex"); ignoredWatchPath(root, path) {
		t.Errorf("ignoredWatchPath(%q) = true", path)
	}
}

// makeLinkedWorktree creates dir as a linked git worktree of the repository at
// root, with the admin directory and records that git worktree add writes.
func makeLinkedWorktree(t *testing.T, root, dir string) {
	t.Helper()
	admin := filepath.Join(root, ".git", "worktrees", filepath.Base(dir))
	for path, content := range map[string]string{
		filepath.Join(admin, "HEAD"):          "ref: refs/heads/feature\n",
		filepath.Join(admin, "commondir"):     "../..\n",
		filepath.Join(admin, "gitdir"):        filepath.Join(dir, ".git") + "\n",
		filepath.Join(dir, ".git"):            "gitdir: " + admin + "\n",
		filepath.Join(dir, "lib", "copy.ex"):  "defmodule Copy do\nend\n",
		filepath.Join(dir, "top_level.ex"):    "defmodule TopLevel do\nend\n",
		filepath.Join(root, "lib", "main.ex"): "defmodule Main do\nend\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func newRecordingWatcher(root string, stub *watchAddStub) (*fsnotifyWatcher, *[]string, *[]string) {
	var changed, removed []string
	w := newTestWatcher(stub.add, nil)
	w.root = root
	w.onChange = func(path string) { changed = append(changed, path) }
	w.remove = func(path string) error { removed = append(removed, path); return nil }
	w.watchList = stub.paths
	return w, &changed, &removed
}

// The watcher walk finds a nested worktree from the directory entries it reads
// anyway. It watches the top, for its .git file, and nothing below it.
func TestWatcherWatchesOnlyTopOfNestedWorktree(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, ".claude", "worktrees", "feature")
	makeLinkedWorktree(t, root, wt)

	stub := &watchAddStub{failing: map[string]bool{}}
	w, _, _ := newRecordingWatcher(root, stub)
	w.watchTree(root)
	got := stub.paths()
	slices.Sort(got)
	want := []string{root, filepath.Join(root, ".claude"), filepath.Join(root, ".claude", "worktrees"), wt, filepath.Join(root, "lib")}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("watched %v, want %v", got, want)
	}
	if !w.tops.has(wt) {
		t.Errorf("worktree top %s not recorded", wt)
	}
}

// Events from a watched top are dropped, except the loss of its .git file, and
// a worktree moved into the project is watched only at its top.
func TestWatcherDropsEventsFromNestedWorktree(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, ".claude", "worktrees", "feature")
	makeLinkedWorktree(t, root, wt)
	moved := filepath.Join(root, "moved")
	makeLinkedWorktree(t, root, moved)

	stub := &watchAddStub{failing: map[string]bool{}}
	w, changed, _ := newRecordingWatcher(root, stub)
	w.tops.add(wt)

	w.handle(fsnotify.Event{Name: filepath.Join(wt, "lib"), Op: fsnotify.Create})
	w.handle(fsnotify.Event{Name: filepath.Join(wt, "top_level.ex"), Op: fsnotify.Write})
	w.handle(fsnotify.Event{Name: filepath.Join(wt, "mix.exs"), Op: fsnotify.Write})
	w.handle(fsnotify.Event{Name: moved, Op: fsnotify.Create})
	if got := stub.paths(); !slices.Equal(got, []string{moved}) {
		t.Errorf("watched %v, want only the moved top [%s]", got, moved)
	}
	if len(*changed) != 0 {
		t.Errorf("reported %v from nested worktrees", *changed)
	}
	if len(w.pending) != 0 {
		t.Errorf("pending = %v before any .git change", w.pending)
	}
	w.handle(fsnotify.Event{Name: filepath.Join(wt, ".git"), Op: fsnotify.Remove})
	if _, ok := w.pending[wt]; !ok {
		t.Error("the loss of a top's .git file was not queued for a check")
	}

	w.handle(fsnotify.Event{Name: wt, Op: fsnotify.Remove})
	if w.tops.has(wt) {
		t.Error("a removed top is still known")
	}
	if _, ok := w.pending[wt]; ok {
		t.Error("a removed top is still queued")
	}
}

// git worktree add creates the directory before its .git file, and cp -r can
// copy subdirectories first. Once the .git file appears, the watches below the
// top go and the runtime is told about the directory, to drop what it indexed.
func TestWatcherDropsWatchesWhenDirectoryBecomesWorktree(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	makeLinkedWorktree(t, root, wt)
	plain := filepath.Join(root, "vendor", "shared_lib")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, ".git"), []byte("gitdir: ../../.git/modules/shared_lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git", "modules", "shared_lib"), 0o755); err != nil {
		t.Fatal(err)
	}

	stub := &watchAddStub{failing: map[string]bool{}}
	w, changed, removed := newRecordingWatcher(root, stub)
	// Watched before its .git file appeared.
	for _, dir := range []string{root, wt, filepath.Join(wt, "lib")} {
		_ = stub.add(dir)
	}
	w.setFailed(filepath.Join(wt, "lib", "deep"), true)
	w.handle(fsnotify.Event{Name: filepath.Join(wt, ".git"), Op: fsnotify.Create})
	w.handle(fsnotify.Event{Name: filepath.Join(wt, ".git"), Op: fsnotify.Create})
	w.handle(fsnotify.Event{Name: filepath.Join(plain, ".git"), Op: fsnotify.Create})
	w.handle(fsnotify.Event{Name: filepath.Join(root, ".git"), Op: fsnotify.Create})
	if want := []string{filepath.Join(wt, "lib")}; !slices.Equal(*removed, want) {
		t.Errorf("removed watches %v, want %v", *removed, want)
	}
	if want := []string{wt}; !slices.Equal(*changed, want) {
		t.Errorf("reported %v, want %v once", *changed, want)
	}
	if !w.tops.has(wt) || w.tops.has(plain) {
		t.Errorf("tops = %v, want only %s", w.tops.list(), wt)
	}
	if failed := w.failedDirectories(); len(failed) != 0 {
		t.Errorf("failed directories %v below the new top were kept", failed)
	}

	// A Create for a directory deeper inside the top can still be queued
	// from before the top was known. It must not be watched or walked.
	deep := filepath.Join(wt, "lib", "sub")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "deep.ex"), []byte("defmodule Deep do\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.handle(fsnotify.Event{Name: deep, Op: fsnotify.Create})
	w.handle(fsnotify.Event{Name: filepath.Join(deep, "deep.ex"), Op: fsnotify.Create})
	if slices.Contains(stub.paths(), deep) || len(*changed) != 1 {
		t.Errorf("watched %v, reported %v after events from inside the top", stub.paths(), *changed)
	}
}

// A top whose .git file went away is checked later. If it is gone, a worktree
// again, or still recorded by git, as during git worktree remove, nothing
// happens; if it is now a plain directory, it is watched and indexed.
func TestWatcherChecksTopsThatLostTheirGitFile(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "became_plain")
	again := filepath.Join(root, "worktree_again")
	gone := filepath.Join(root, "gone")
	removing := filepath.Join(root, "being_removed")
	for _, dir := range []string{plain, again, gone, removing} {
		makeLinkedWorktree(t, root, dir)
	}
	for _, path := range []string{filepath.Join(plain, ".git"), filepath.Join(root, ".git", "worktrees", "became_plain"), filepath.Join(removing, ".git")} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	stub := &watchAddStub{failing: map[string]bool{}}
	w, changed, _ := newRecordingWatcher(root, stub)
	for _, dir := range []string{plain, again, gone, removing} {
		w.tops.add(dir)
		w.markPending(dir)
	}
	w.checkPending()

	got := stub.paths()
	slices.Sort(got)
	if want := []string{plain, filepath.Join(plain, "lib")}; !slices.Equal(got, want) {
		t.Errorf("watched %v, want %v", got, want)
	}
	slices.Sort(*changed)
	if want := []string{filepath.Join(plain, "lib", "copy.ex"), filepath.Join(plain, "top_level.ex")}; !slices.Equal(*changed, want) {
		t.Errorf("reported %v, want %v", *changed, want)
	}
	tops := w.tops.list()
	slices.Sort(tops)
	if want := []string{removing, again}; !slices.Equal(tops, want) {
		t.Errorf("tops = %v, want %v", tops, want)
	}
	if _, ok := w.pending[removing]; !ok || len(w.pending) != 1 {
		t.Errorf("pending = %v after the check, want only %s", w.pending, removing)
	}

	// Once git's record goes, as after git worktree prune, the next check
	// indexes it.
	if err := os.RemoveAll(filepath.Join(root, ".git", "worktrees", "being_removed")); err != nil {
		t.Fatal(err)
	}
	*changed = (*changed)[:0]
	w.checkPending()
	slices.Sort(*changed)
	if want := []string{filepath.Join(removing, "lib", "copy.ex"), filepath.Join(removing, "top_level.ex")}; !slices.Equal(*changed, want) {
		t.Errorf("after the record went: reported %v, want %v", *changed, want)
	}
	if w.tops.has(removing) || len(w.pending) != 0 {
		t.Errorf("after the record went: tops = %v, pending = %v", w.tops.list(), w.pending)
	}
}

// A directory that failed to watch and is inside a nested worktree is not
// retried; it no longer counts against coverage either.
func TestWatcherDoesNotRetryFailedDirectoryInNestedWorktree(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	makeLinkedWorktree(t, root, wt)
	failedDir := filepath.Join(wt, "lib")

	stub := &watchAddStub{failing: map[string]bool{failedDir: true}}
	var transitions []bool
	w := newTestWatcher(stub.add, func(degraded bool) { transitions = append(transitions, degraded) })
	w.root = root
	w.tops.add(wt)
	w.setFailed(failedDir, true)

	stub.resetCalls()
	w.retryFailed()
	if got := stub.paths(); len(got) != 0 {
		t.Errorf("retried %v inside a nested worktree", got)
	}
	if w.Degraded() {
		t.Errorf("still degraded by %v", w.failedDirectories())
	}
	if !slices.Equal(transitions, []bool{true, false}) {
		t.Errorf("coverage transitions = %v, want [true false]", transitions)
	}
}

// The Create event of a new .git file can be read before git writes the file.
// Its Write event makes the directory a top then.
func TestWatcherFindsWorktreeFromGitFileWrite(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, ".claude", "worktrees", "feature")
	makeLinkedWorktree(t, root, wt)
	gitFile := filepath.Join(wt, ".git")
	content, err := os.ReadFile(gitFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	stub := &watchAddStub{failing: map[string]bool{}}
	w, changed, _ := newRecordingWatcher(root, stub)

	w.handle(fsnotify.Event{Name: gitFile, Op: fsnotify.Create})
	if w.tops.has(wt) {
		t.Fatal("an empty .git file made a top")
	}
	if err := os.WriteFile(gitFile, content, 0o644); err != nil {
		t.Fatal(err)
	}
	w.handle(fsnotify.Event{Name: gitFile, Op: fsnotify.Write})
	if !w.tops.has(wt) || !slices.Equal(*changed, []string{wt}) {
		t.Errorf("tops = %v, reported %v; want %s known and reported once", w.tops.list(), *changed, wt)
	}
}

// A worktree that git records but whose .git file is gone is a top from the
// start, as the walkers skip it: its subdirectories are not watched.
func TestWatcherStartsWithRecordedWorktreeAsTop(t *testing.T) {
	root := t.TempDir()
	wt := filepath.Join(root, ".claude", "worktrees", "feature")
	makeLinkedWorktree(t, root, wt)
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	backend, err := startFSNotifyWatcher(root, WatchCallbacks{PathChanged: func(string) {}, FullReconcile: func() {}, CoverageChanged: func(bool) {}})
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	w := backend.(*fsnotifyWatcher)
	t.Cleanup(func() { _ = w.Close() })
	watched := w.fsw.WatchList()
	for _, path := range watched {
		if strings.HasPrefix(path, wt+string(filepath.Separator)) {
			t.Errorf("watching %s inside a recorded worktree", path)
		}
	}
	if !slices.Contains(watched, wt) || !w.tops.has(wt) {
		t.Errorf("the recorded worktree's top is not a watched top: tops = %v", w.tops.list())
	}
}

func TestNestedWorktreeTopsReadsGitRecords(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, ".claude", "worktrees", "feature")
	makeLinkedWorktree(t, root, inside)
	outside := filepath.Join(t.TempDir(), "sibling")
	makeLinkedWorktree(t, root, outside)

	if got := parser.NestedWorktreeTops(root); !slices.Equal(got, []string{inside}) {
		t.Errorf("nestedWorktreeTops = %v, want [%s]", got, inside)
	}
	// From inside a linked worktree, commondir leads to the same records.
	if got := parser.NestedWorktreeTops(inside); len(got) != 0 {
		t.Errorf("NestedWorktreeTops(worktree) = %v, want none below it", got)
	}
	if got := parser.NestedWorktreeTops(t.TempDir()); len(got) != 0 {
		t.Errorf("NestedWorktreeTops(no repo) = %v", got)
	}
}

// TestFSNotifyWatcherSkipsWorktreeAddedWhileRunning runs real git worktree
// commands under the native watcher. However a worktree arrives, only its top
// may be watched and none of its source files may be reported. When it goes
// away, the watcher forgets it; when only its .git file goes, it is indexed.
func TestFSNotifyWatcherSkipsWorktreeAddedWhileRunning(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	previous := watchRetryInterval
	watchRetryInterval = 20 * time.Millisecond
	t.Cleanup(func() { watchRetryInterval = previous })

	cases := []struct {
		name string
		// arrive puts a linked worktree at wt while the watcher runs.
		arrive func(t *testing.T, env *watchedRepo, wt string)
	}{
		{"add", func(t *testing.T, env *watchedRepo, wt string) {
			env.git("worktree", "add", "-q", wt)
		}},
		{"add into an existing watched directory", func(t *testing.T, env *watchedRepo, wt string) {
			if err := os.MkdirAll(wt, 0o755); err != nil {
				t.Fatal(err)
			}
			env.settle(t)
			env.git("worktree", "add", "-q", wt)
		}},
		{"move in from outside the project", func(t *testing.T, env *watchedRepo, wt string) {
			outside := filepath.Join(t.TempDir(), "feature")
			env.git("worktree", "add", "-q", outside)
			if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
				t.Fatal(err)
			}
			env.settle(t)
			env.git("worktree", "move", outside, wt)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newWatchedRepo(t)
			wt := filepath.Join(env.root, ".claude", "worktrees", "feature")
			tc.arrive(t, env, wt)
			env.settle(t)

			prefix := wt + string(filepath.Separator)
			for _, path := range env.w.fsw.WatchList() {
				if strings.HasPrefix(path, prefix) {
					t.Errorf("still watching %s inside the nested worktree", path)
				}
			}
			for _, path := range env.changedPaths() {
				if strings.HasPrefix(path, prefix) && parser.IsElixirFile(path) {
					t.Errorf("reported %s from the nested worktree", path)
				}
			}

			env.git("worktree", "remove", wt)
			env.settle(t)
			time.Sleep(5 * watchRetryInterval)
			env.settle(t)
			if tops := env.w.tops.list(); len(tops) != 0 {
				t.Errorf("tops after git worktree remove = %v", tops)
			}
			for _, path := range env.changedPaths() {
				if strings.HasPrefix(path, prefix) && parser.IsElixirFile(path) {
					t.Errorf("reported %s while the worktree was removed", path)
				}
			}
		})
	}

	t.Run("delete only the .git file", func(t *testing.T) {
		env := newWatchedRepo(t)
		wt := filepath.Join(env.root, ".claude", "worktrees", "feature")
		env.git("worktree", "add", "-q", wt)
		env.settle(t)
		if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
			t.Fatal(err)
		}
		env.settle(t)
		time.Sleep(5 * watchRetryInterval)
		env.settle(t)
		// git still records the worktree, as it does during git worktree
		// remove, so it stays out like it does for the walkers.
		for _, path := range env.changedPaths() {
			if strings.HasPrefix(path, wt+string(filepath.Separator)) && parser.IsElixirFile(path) {
				t.Errorf("reported %s from a worktree that git still records", path)
			}
		}
		if !env.w.tops.has(wt) {
			t.Error("a worktree that git still records is no longer a top")
		}
	})

	t.Run("delete the .git file and git's record", func(t *testing.T) {
		env := newWatchedRepo(t)
		wt := filepath.Join(env.root, ".claude", "worktrees", "feature")
		env.git("worktree", "add", "-q", wt)
		env.settle(t)
		if err := os.RemoveAll(filepath.Join(env.root, ".git", "worktrees", "feature")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(wt, "lib", "area_0", "nested", "mod.ex")
		deadline := time.Now().Add(10 * time.Second)
		for !slices.Contains(env.changedPaths(), source) {
			if time.Now().After(deadline) {
				t.Fatal("the plain directory's files were not reported")
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !slices.Contains(env.w.fsw.WatchList(), filepath.Join(wt, "lib")) {
			t.Error("the plain directory's subdirectories are not watched")
		}
	})
}

// watchedRepo is a committed git repository under a running native watcher.
type watchedRepo struct {
	t       *testing.T
	root    string
	w       *fsnotifyWatcher
	mu      sync.Mutex
	changed map[string]bool
	settled int
}

func newWatchedRepo(t *testing.T) *watchedRepo {
	t.Helper()
	env := &watchedRepo{t: t, root: t.TempDir(), changed: map[string]bool{}}
	for i := range 20 {
		dir := filepath.Join(env.root, "lib", fmt.Sprintf("area_%d", i), "nested")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mod.ex"), []byte("defmodule M do\nend\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env.git("init", "-q")
	env.git("add", ".")
	env.git("commit", "-qm", "init")

	backend, err := startFSNotifyWatcher(env.root, WatchCallbacks{
		PathChanged: func(path string) {
			env.mu.Lock()
			env.changed[path] = true
			env.mu.Unlock()
		},
		FullReconcile:   func() {},
		CoverageChanged: func(bool) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.w = backend.(*fsnotifyWatcher)
	t.Cleanup(func() { _ = env.w.Close() })
	return env
}

func (env *watchedRepo) git(args ...string) {
	env.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = env.root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if out, err := cmd.CombinedOutput(); err != nil {
		env.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// settle waits until every earlier event has been handled. Events from one
// watcher arrive in order, so a later write being reported is enough.
func (env *watchedRepo) settle(t *testing.T) {
	t.Helper()
	env.settled++
	sentinel := filepath.Join(env.root, "lib", fmt.Sprintf("sentinel_%d.ex", env.settled))
	if err := os.WriteFile(sentinel, []byte("defmodule S do\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		env.mu.Lock()
		seen := env.changed[sentinel]
		env.mu.Unlock()
		if seen {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("watcher did not report the sentinel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (env *watchedRepo) changedPaths() []string {
	env.mu.Lock()
	defer env.mu.Unlock()
	paths := make([]string, 0, len(env.changed))
	for path := range env.changed {
		paths = append(paths, path)
	}
	return paths
}
