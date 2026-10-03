//go:build darwin && cgo

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

	"github.com/fsnotify/fsevents"

	"github.com/remoteoss/dexter/internal/parser"
)

func TestFSEventsTranslatesEvents(t *testing.T) {
	root := t.TempDir()
	var paths []string
	full := 0
	w := &fseventsWatcher{
		root:      root,
		eventRoot: root,
		callbacks: WatchCallbacks{
			PathChanged:   func(path string) { paths = append(paths, path) },
			FullReconcile: func() { full++ },
		},
	}

	source := filepath.Join(root, "lib", "worker.ex")
	w.handle(fsevents.Event{Path: source, Flags: fsevents.ItemIsFile | fsevents.ItemModified})
	w.handle(fsevents.Event{Path: filepath.Join(root, "deps", "library", "lib", "ignored.ex"), Flags: fsevents.ItemIsFile | fsevents.ItemModified})
	w.handle(fsevents.Event{Path: filepath.Join(root, "lib"), Flags: fsevents.ItemIsDir | fsevents.ItemRenamed})
	w.handle(fsevents.Event{Path: root, Flags: fsevents.MustScanSubDirs | fsevents.UserDropped})

	if len(paths) != 1 || paths[0] != source {
		t.Fatalf("changed paths = %v, want [%s]", paths, source)
	}
	if full != 2 {
		t.Fatalf("full reconciliations = %d, want 2", full)
	}
}

func TestFSEventsReportsFileChange(t *testing.T) {
	root := t.TempDir()
	changes := make(chan string, 8)
	w, err := startFSEventsWatcher(root, WatchCallbacks{
		PathChanged:   func(path string) { changes <- path },
		FullReconcile: func() {},
	})
	if err != nil {
		t.Skipf("FSEvents unavailable: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	path := filepath.Join(root, "created.ex")
	if err := os.WriteFile(path, []byte("defmodule Created do\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case changed := <-changes:
			if changed == path {
				return
			}
		case <-deadline:
			t.Fatal("FSEvents did not report the created file")
		}
	}
}

func TestFSEventsFailureFallsBackToFSNotify(t *testing.T) {
	previous := newFSEventsBackend
	newFSEventsBackend = func(string, WatchCallbacks) (watchBackend, error) {
		return nil, errors.New("FSEvents unavailable")
	}
	t.Cleanup(func() { newFSEventsBackend = previous })

	watcher, kind, err := startPlatformWatcher(t.TempDir(), WatchCallbacks{
		PathChanged:     func(string) {},
		FullReconcile:   func() {},
		CoverageChanged: func(bool) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	if kind != "fsnotify" {
		t.Fatalf("watcher kind = %q, want fsnotify", kind)
	}
}

// Checking out a nested worktree creates many directories. They are outside the
// index, so none of them may reconcile the whole workspace, and no event from a
// known top may reach the runtime.
func TestFSEventsIgnoresNestedWorktree(t *testing.T) {
	root := t.TempDir()
	known := filepath.Join(root, ".claude", "worktrees", "known")
	makeLinkedWorktree(t, root, known)
	fresh := filepath.Join(root, ".claude", "worktrees", "fresh")
	makeLinkedWorktree(t, root, fresh)
	var paths []string
	full := 0
	w := &fseventsWatcher{
		root:      root,
		eventRoot: root,
		callbacks: WatchCallbacks{PathChanged: func(path string) { paths = append(paths, path) }, FullReconcile: func() { full++ }},
	}
	w.tops.add(known)

	w.handle(fsevents.Event{Path: filepath.Join(known, "lib"), Flags: fsevents.ItemIsDir | fsevents.ItemCreated})
	w.handle(fsevents.Event{Path: filepath.Join(known, "lib", "copy.ex"), Flags: fsevents.ItemIsFile | fsevents.ItemModified})
	w.handle(fsevents.Event{Path: filepath.Join(known, "mix.exs"), Flags: fsevents.ItemIsFile | fsevents.ItemModified})
	w.handle(fsevents.Event{Path: filepath.Join(known, "lib", "copy.ex"), Flags: fsevents.ItemIsFile | fsevents.ItemRemoved})
	if full != 0 || len(paths) != 0 {
		t.Fatalf("full reconciliations = %d, paths = %v; want none", full, paths)
	}

	// Not known yet, but its .git file is on disk when the directory's event
	// is handled: fresh becomes a known top, and the runtime is told once,
	// however its directory and .git events arrive.
	w.handle(fsevents.Event{Path: fresh, Flags: fsevents.ItemIsDir | fsevents.ItemCreated})
	w.handle(fsevents.Event{Path: filepath.Join(fresh, "lib"), Flags: fsevents.ItemIsDir | fsevents.ItemCreated})
	w.handle(fsevents.Event{Path: filepath.Join(fresh, ".git"), Flags: fsevents.ItemIsFile | fsevents.ItemCreated})
	w.handle(fsevents.Event{Path: filepath.Join(fresh, ".git"), Flags: fsevents.ItemIsFile | fsevents.ItemModified})
	if !w.tops.has(fresh) || len(paths) != 1 || paths[0] != fresh {
		t.Fatalf("tops = %v, paths = %v; want %s known and reported once", w.tops.list(), paths, fresh)
	}

	// Removing a top needs no reconcile: nothing was indexed from it.
	if err := os.RemoveAll(known); err != nil {
		t.Fatal(err)
	}
	w.handle(fsevents.Event{Path: filepath.Join(known, "lib"), Flags: fsevents.ItemIsDir | fsevents.ItemRemoved})
	w.handle(fsevents.Event{Path: known, Flags: fsevents.ItemIsDir | fsevents.ItemRemoved})
	if full != 0 || w.tops.has(known) {
		t.Fatalf("full reconciliations = %d, tops = %v after removal", full, w.tops.list())
	}

	w.handle(fsevents.Event{Path: filepath.Join(root, "lib"), Flags: fsevents.ItemIsDir | fsevents.ItemCreated})
	if full != 1 {
		t.Fatalf("full reconciliations = %d for a plain directory, want 1", full)
	}
}

// A worktree renamed within the project is a known top under its new name.
func TestFSEventsKnowsRenamedTop(t *testing.T) {
	root := t.TempDir()
	moved := filepath.Join(root, ".claude", "worktrees", "moved")
	makeLinkedWorktree(t, root, moved)
	var paths []string
	full := 0
	w := &fseventsWatcher{
		root:      root,
		eventRoot: root,
		callbacks: WatchCallbacks{PathChanged: func(path string) { paths = append(paths, path) }, FullReconcile: func() { full++ }},
	}
	old := filepath.Join(root, ".claude", "worktrees", "old")
	w.tops.add(old)
	w.handle(fsevents.Event{Path: old, Flags: fsevents.ItemIsDir | fsevents.ItemRenamed})
	w.handle(fsevents.Event{Path: moved, Flags: fsevents.ItemIsDir | fsevents.ItemRenamed})
	w.handle(fsevents.Event{Path: filepath.Join(moved, "lib", "copy.ex"), Flags: fsevents.ItemIsFile | fsevents.ItemModified})
	if w.tops.has(old) || !w.tops.has(moved) {
		t.Errorf("tops = %v, want only %s", w.tops.list(), moved)
	}
	if full != 0 || !slices.Equal(paths, []string{moved}) {
		t.Errorf("full reconciliations = %d, paths = %v; want none and %s once", full, paths, moved)
	}
}

// A top whose .git file goes away stays a top while git records it, as during
// git worktree remove.
func TestFSEventsKeepsRecordedTop(t *testing.T) {
	previous := watchRetryInterval
	watchRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { watchRetryInterval = previous })

	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	makeLinkedWorktree(t, root, wt)
	changes := make(chan string, 64)
	w := &fseventsWatcher{
		root:      root,
		eventRoot: root,
		callbacks: WatchCallbacks{PathChanged: sendWithoutBlocking(changes), FullReconcile: func() {}},
	}
	w.tops.add(wt)
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	w.handle(fsevents.Event{Path: filepath.Join(wt, ".git"), Flags: fsevents.ItemIsFile | fsevents.ItemRemoved})
	select {
	case path := <-changes:
		t.Fatalf("reported %s from a worktree that git still records", path)
	case <-time.After(20 * watchRetryInterval):
	}
	if !w.tops.has(wt) {
		t.Fatal("a worktree that git still records is no longer a top")
	}

	// Once git's record goes, as after git worktree prune, a later check
	// indexes it.
	if err := os.RemoveAll(filepath.Join(root, ".git", "worktrees", "wt")); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(wt, "lib", "copy.ex")
	deadline := time.After(10 * time.Second)
	for {
		select {
		case path := <-changes:
			if path == want {
				return
			}
		case <-deadline:
			t.Fatal("the directory was not indexed after its record went")
		}
	}
}

// A top whose .git file and git record go away is indexed as a plain directory.
func TestFSEventsIndexesTopThatBecomesPlain(t *testing.T) {
	previous := watchRetryInterval
	watchRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() { watchRetryInterval = previous })

	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	makeLinkedWorktree(t, root, wt)
	if err := os.RemoveAll(filepath.Join(root, ".git", "worktrees", "wt")); err != nil {
		t.Fatal(err)
	}
	changes := make(chan string, 64)
	w := &fseventsWatcher{
		root:      root,
		eventRoot: root,
		callbacks: WatchCallbacks{PathChanged: sendWithoutBlocking(changes), FullReconcile: func() {}},
	}
	w.tops.add(wt)
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	w.handle(fsevents.Event{Path: filepath.Join(wt, ".git"), Flags: fsevents.ItemIsFile | fsevents.ItemRemoved})

	want := filepath.Join(wt, "lib", "copy.ex")
	deadline := time.After(10 * time.Second)
	for {
		select {
		case path := <-changes:
			if path == want {
				if w.tops.has(wt) {
					t.Fatal("a plain directory is still a known top")
				}
				return
			}
		case <-deadline:
			t.Fatal("the plain directory's files were not reported")
		}
	}
}

// TestFSEventsWorktreeLifecycle runs real git worktree commands under a real
// FSEvents stream: adding and removing a nested worktree must not reconcile the
// workspace or report the worktree's files.
func TestFSEventsWorktreeLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	for i := range 20 {
		dir := filepath.Join(root, "lib", fmt.Sprintf("area_%d", i), "nested")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mod.ex"), []byte("defmodule M do\nend\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, ".claude", "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-qm", "init")

	var mu sync.Mutex
	var paths []string
	full := 0
	w, err := startFSEventsWatcher(root, WatchCallbacks{
		PathChanged: func(path string) {
			mu.Lock()
			paths = append(paths, path)
			mu.Unlock()
		},
		FullReconcile: func() {
			mu.Lock()
			full++
			mu.Unlock()
		},
	})
	if err != nil {
		t.Skipf("FSEvents unavailable: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	settled := 0
	settle := func() {
		t.Helper()
		settled++
		sentinel := filepath.Join(w.root, "lib", fmt.Sprintf("sentinel_%d.ex", settled))
		if err := os.WriteFile(sentinel, []byte("defmodule S do\nend\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			seen := slices.Contains(paths, sentinel)
			mu.Unlock()
			if seen {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("FSEvents did not report the sentinel")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	settle()
	mu.Lock()
	full = 0
	mu.Unlock()

	wt := filepath.Join(w.root, ".claude", "worktrees", "feature")
	git("worktree", "add", "-q", wt)
	settle()
	git("worktree", "remove", wt)
	settle()

	mu.Lock()
	defer mu.Unlock()
	if full != 0 {
		t.Errorf("full reconciliations = %d, want 0", full)
	}
	prefix := wt + string(filepath.Separator)
	for _, path := range paths {
		if strings.HasPrefix(path, prefix) && parser.IsElixirFile(path) {
			t.Errorf("reported %s from the nested worktree", path)
		}
	}
}

// sendWithoutBlocking is a PathChanged callback that never blocks the timer
// goroutine that calls it, even after the test stops reading.
func sendWithoutBlocking(changes chan<- string) func(string) {
	return func(path string) {
		select {
		case changes <- path:
		default:
		}
	}
}
