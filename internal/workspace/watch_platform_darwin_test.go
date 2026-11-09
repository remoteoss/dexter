//go:build darwin && cgo

package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

	// The renamed directory is gone, so it is reconciled as one path, not
	// with a walk of the whole workspace. Only lost events need that.
	if want := []string{source, filepath.Join(root, "lib")}; !slices.Equal(paths, want) {
		t.Fatalf("changed paths = %v, want %v", paths, want)
	}
	if full != 1 {
		t.Fatalf("full reconciliations = %d, want 1", full)
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

	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(plain, "plain.ex")
	if err := os.WriteFile(source, []byte("defmodule Plain do\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths = nil
	w.handle(fsevents.Event{Path: plain, Flags: fsevents.ItemIsDir | fsevents.ItemCreated})
	if full != 0 || !slices.Equal(paths, []string{source}) {
		t.Fatalf("full reconciliations = %d, paths = %v for a plain directory; want 0 and [%s]", full, paths, source)
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

	// git worktree move renames the directory, then writes its .git file again
	// in place, so the watcher can find the file empty.
	outside := filepath.Join(t.TempDir(), "feature")
	git("worktree", "add", "-q", outside)
	content, err := os.ReadFile(filepath.Join(outside, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, ".git"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(outside, wt); err != nil {
		t.Fatal(err)
	}
	settle()
	if err := os.WriteFile(filepath.Join(wt, ".git"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	git("worktree", "repair", wt)
	settle()
	git("worktree", "remove", "--force", wt)
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
// A renamed file is checked for its spelling on disk even when it is new: a
// tool that writes a file and renames it in another case at once would leave
// a row under the old spelling.
func TestFSEventsSendsARenamedFileToTheSpellingCheck(t *testing.T) {
	root := t.TempDir()
	var changed, renamed []string
	w := &fseventsWatcher{
		root:      root,
		eventRoot: root,
		callbacks: WatchCallbacks{
			PathChanged: func(path string) { changed = append(changed, path) },
			PathRenamed: func(path string) { renamed = append(renamed, path) },
		},
	}
	edited := filepath.Join(root, "lib", "edited.ex")
	old := filepath.Join(root, "lib", "fresh.ex")
	w.handle(fsevents.Event{Path: edited, Flags: fsevents.ItemIsFile | fsevents.ItemModified})
	w.handle(fsevents.Event{Path: old, Flags: fsevents.ItemIsFile | fsevents.ItemCreated | fsevents.ItemRenamed})
	if !slices.Equal(changed, []string{edited}) || !slices.Equal(renamed, []string{old}) {
		t.Fatalf("changed = %v, renamed = %v; want [%s] and [%s]", changed, renamed, edited, old)
	}
}

func sendWithoutBlocking(changes chan<- string) func(string) {
	return func(path string) {
		select {
		case changes <- path:
		default:
		}
	}
}

// newRecordingFSEvents returns an FSEvents watcher for handle tests that
// records what it asks of the runtime.
func newRecordingFSEvents(root string) (*fseventsWatcher, *[]string, *int) {
	var paths []string
	full := 0
	w := &fseventsWatcher{
		root:      root,
		eventRoot: root,
		callbacks: WatchCallbacks{PathChanged: func(path string) { paths = append(paths, path) }, FullReconcile: func() { full++ }},
	}
	return w, &paths, &full
}

// A directory that is created, moved in, renamed or removed is reconciled as
// its own subtree. A walk of the whole workspace for each one made a burst of
// new directories cost one full walk each on a large repository.
func TestFSEventsReconcilesOnlyTheChangedSubtree(t *testing.T) {
	root := t.TempDir()
	w, paths, full := newRecordingFSEvents(root)

	// An empty directory: nothing to index, and no full reconcile.
	empty := filepath.Join(root, "tmp", "empty")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	w.handle(fsevents.Event{Path: empty, Flags: fsevents.ItemIsDir | fsevents.ItemCreated})
	if *full != 0 || len(*paths) != 0 {
		t.Fatalf("empty directory: full = %d, paths = %v; want none", *full, *paths)
	}

	// A tree moved into place has no file events of its own. Its files are
	// found by a walk of the tree only, with the rules of the index walks:
	// build output and nested worktrees are skipped, dependencies are not.
	moved := filepath.Join(root, "apps", "billing")
	source := writeTestModule(t, moved, "lib/billing/invoice.ex", "Billing.Invoice")
	script := writeTestModule(t, moved, "test/invoice_test.exs", "Billing.InvoiceTest")
	writeTestModule(t, moved, "_build/dev/lib/billing/invoice.ex", "Billing.Build")
	writeTestModule(t, moved, "deps/library/lib/library.ex", "Library")
	nested := filepath.Join(moved, "worktrees", "feature")
	makeLinkedWorktree(t, root, nested)
	writeTestModule(t, nested, "lib/feature.ex", "Billing.Feature")
	*paths = nil
	w.handle(fsevents.Event{Path: moved, Flags: fsevents.ItemIsDir | fsevents.ItemRenamed})
	got := slices.Clone(*paths)
	slices.Sort(got)
	// The oracle is the walk of a full reconcile: what it indexes, the
	// watcher must report, and nothing more. It walks deps/ and skips
	// _build/ and the nested worktree.
	want := []string{moved, nested}
	_ = parser.WalkElixirFiles(moved, func(path string, _ fs.DirEntry) error {
		want = append(want, path)
		return nil
	})
	slices.Sort(want)
	if !slices.Contains(want, source) || !slices.Contains(want, script) || !slices.Contains(want, filepath.Join(moved, "deps/library/lib/library.ex")) {
		t.Fatalf("the oracle walk missed files: %v", want)
	}
	if *full != 0 || !slices.Equal(got, want) {
		t.Fatalf("moved-in tree: full = %d, paths = %v; want 0 and %v", *full, got, want)
	}
	if !w.tops.has(nested) {
		t.Fatalf("the nested worktree %s is not a top: %v", nested, w.tops.list())
	}

	// A removed tree is one path: the runtime removes what was indexed below it.
	if err := os.RemoveAll(moved); err != nil {
		t.Fatal(err)
	}
	*paths = nil
	w.handle(fsevents.Event{Path: moved, Flags: fsevents.ItemIsDir | fsevents.ItemRemoved})
	if *full != 0 || !slices.Equal(*paths, []string{moved}) {
		t.Fatalf("removed tree: full = %d, paths = %v; want 0 and [%s]", *full, *paths, moved)
	}

	// The root itself and lost events still need the full reconcile.
	w.handle(fsevents.Event{Path: root, Flags: fsevents.ItemIsDir | fsevents.ItemRenamed})
	w.handle(fsevents.Event{Path: root, Flags: fsevents.MustScanSubDirs})
	if *full != 2 {
		t.Fatalf("full reconciliations = %d for the root and lost events, want 2", *full)
	}
}

// The same with the real FSEvents stream: a burst of new directories must not
// ask for full reconciles, and a module in a new directory is reported.
func TestFSEventsBurstOfDirectoriesNeedsNoFullReconcile(t *testing.T) {
	root := t.TempDir()
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

	for i := 0; i < 10; i++ {
		if err := os.MkdirAll(filepath.Join(root, "tmp", fmt.Sprintf("dir%d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	staged := filepath.Join(t.TempDir(), "staged")
	source := writeTestModule(t, staged, "lib/staged.ex", "Staged.Module")
	target := filepath.Join(root, "staged")
	if err := os.Rename(staged, target); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(target, "lib", "staged.ex")
	_ = source
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		found := slices.Contains(paths, want)
		mu.Unlock()
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("FSEvents did not report %s in a directory moved into place", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Give late events time to arrive.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if full != 0 {
		t.Fatalf("a burst of new directories asked for %d full reconciles, want 0", full)
	}
}

// FSEvents reports paths in the case that the file system stores. A root typed
// in another case on a case-insensitive volume must still match them; else
// every event is dropped and watching is broken in silence.
func TestFSEventsWatchesRootTypedInAnotherCase(t *testing.T) {
	parent := t.TempDir()
	disk := filepath.Join(parent, "CaseRoot")
	if err := os.Mkdir(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	typed := filepath.Join(parent, "caseroot")
	if _, err := os.Stat(typed); err != nil {
		t.Skip("the volume is case-sensitive")
	}
	changes := make(chan string, 64)
	w, err := startFSEventsWatcher(typed, WatchCallbacks{
		PathChanged:   sendWithoutBlocking(changes),
		FullReconcile: func() {},
	})
	if err != nil {
		t.Skipf("FSEvents unavailable: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	if err := os.WriteFile(filepath.Join(disk, "created.ex"), []byte("defmodule Created do\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Paths come back under the root that the runtime indexes: the typed one.
	want := filepath.Join(typed, "created.ex")
	deadline := time.After(10 * time.Second)
	for {
		select {
		case changed := <-changes:
			if changed == want {
				return
			}
		case <-deadline:
			t.Fatalf("FSEvents did not report %s for a root typed in another case", want)
		}
	}
}

// After a rename that changes only the case, FSEvents sends the old spelling
// too, and a stat of it succeeds. It must be sent as gone, not walked: a walk
// would report its files in the old spelling again.
func TestFSEventsCaseOnlyRenameIsNotWalkedInTheOldSpelling(t *testing.T) {
	root := t.TempDir()
	skipCaseSensitive(t, root)
	w, paths, full := newRecordingFSEvents(root)
	writeTestModule(t, root, "lib/Area/kept.ex", "SharedLib.Area.Kept")
	oldDir, newDir := filepath.Join(root, "lib", "Area"), filepath.Join(root, "lib", "area")
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatal(err)
	}
	w.handle(fsevents.Event{Path: oldDir, Flags: fsevents.ItemIsDir | fsevents.ItemRenamed})
	if *full != 0 || !slices.Equal(*paths, []string{oldDir}) {
		t.Fatalf("old spelling: full = %d, paths = %v; want 0 and [%s]", *full, *paths, oldDir)
	}
	*paths = nil
	w.handle(fsevents.Event{Path: newDir, Flags: fsevents.ItemIsDir | fsevents.ItemRenamed})
	if want := []string{newDir, filepath.Join(newDir, "kept.ex")}; !slices.Equal(*paths, want) {
		t.Fatalf("new spelling: paths = %v, want %v", *paths, want)
	}
}

// A directory moved into place under another name keeps its dependencies:
// the full walk indexes deps/ of a directory without mix.exs, so the subtree
// walk must report them as well. The oracle is the walk of a full reconcile.
func TestFSEventsMovedDirectoryMatchesTheFullWalk(t *testing.T) {
	root := t.TempDir()
	billing := filepath.Join(root, "apps", "billing")
	writeTestModule(t, billing, "lib/billing.ex", "Billing")
	writeTestModule(t, billing, "deps/libx/lib/libx.ex", "Libx")
	var mu sync.Mutex
	got := map[string]bool{}
	w, err := startFSEventsWatcher(root, WatchCallbacks{
		PathChanged: func(path string) {
			mu.Lock()
			got[path] = true
			mu.Unlock()
		},
		FullReconcile: func() {},
	})
	if err != nil {
		t.Skipf("FSEvents unavailable: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	time.Sleep(100 * time.Millisecond)
	payments := filepath.Join(root, "apps", "payments")
	if err := os.Rename(billing, payments); err != nil {
		t.Fatal(err)
	}
	var want []string
	_ = parser.WalkElixirFiles(payments, func(path string, _ fs.DirEntry) error {
		want = append(want, path)
		return nil
	})
	if len(want) != 2 {
		t.Fatalf("oracle = %v, want 2 files", want)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		missing := []string{}
		for _, path := range want {
			if !got[path] {
				missing = append(missing, path)
			}
		}
		mu.Unlock()
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("FSEvents did not report %v after the rename", missing)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReindexPathUsesTheSpellingOnDisk: after a rename that changed only the
// case, an editor can save its buffer under the old spelling, which a stat
// finds. The file must keep one row, under the spelling on disk.
func TestReindexPathUsesTheSpellingOnDisk(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	skipCaseSensitive(t, root)
	old := filepath.Join(root, "lib", "Foo.ex")
	renamed := filepath.Join(root, "lib", "foo.ex")
	writeTestModule(t, root, "lib/Foo.ex", "SharedLib.Foo")
	rt, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rt.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}
	if err := rt.ReindexPath(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("defmodule SharedLib.Foo do\n  def x, do: 1\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rt.ReindexPath(ctx, old); err != nil {
		t.Fatal(err)
	}
	paths, err := rt.Store().ListFilePathsUnder(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{renamed}) {
		t.Fatalf("indexed paths = %v, want only %s", paths, renamed)
	}
	if defs, _ := rt.Store().LookupFunction("SharedLib.Foo", "x"); len(defs) != 1 {
		t.Fatalf("SharedLib.Foo.x = %v, want the saved text indexed", defs)
	}
}

// TestSaveUnderTheOldSpellingKeepsOneRow: an editor that still has a buffer
// under the old spelling saves it, and no watcher reported the rename. The
// old row is removed, and the saved text is indexed under the spelling on
// disk.
func TestSaveUnderTheOldSpellingKeepsOneRow(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	skipCaseSensitive(t, root)
	old := filepath.Join(root, "lib", "Foo.ex")
	renamed := filepath.Join(root, "lib", "foo.ex")
	writeTestModule(t, root, "lib/Foo.ex", "SharedLib.Foo")
	rt, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rt.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("defmodule SharedLib.Foo do\n  def x, do: 1\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt.ReconcileSavedFile(old)
	var paths []string
	for deadline := time.Now().Add(5 * time.Second); ; {
		paths, err = rt.Store().ListFilePathsUnder(root)
		if err != nil {
			t.Fatal(err)
		}
		defs, _ := rt.Store().LookupFunction("SharedLib.Foo", "x")
		if slices.Equal(paths, []string{renamed}) && len(defs) == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("indexed paths = %v, SharedLib.Foo.x = %v; want only %s, with the saved text", paths, defs, renamed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
