package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/store"
)

// TestSendNeverWaitsOnAFullQueue: the FSEvents callback and the editor's
// didSave handler queue events. During a long reconcile the queue can fill;
// a sender that then waited would stall the watcher and the editor session.
// The overflow becomes one full reconcile, and barriers in it are answered.
func TestSendNeverWaitsOnAFullQueue(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	previous := eventQueueSize
	eventQueueSize = 8
	t.Cleanup(func() { eventQueueSize = previous })
	root := t.TempDir()
	path := writeTestModule(t, root, "lib/late.ex", "SharedLib.Late")
	hold := make(chan struct{})
	rt, err := OpenWithOptions(root, Options{NoWatch: true, BeforeInitialReconcile: func() { <-hold }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < 100; i++ {
			rt.ReconcileFile(filepath.Join(root, "lib", "late.ex"))
		}
	}()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		close(hold)
		t.Fatal("ReconcileFile waited on a full queue")
	}
	// A barrier that overflows is answered after the loop catches up.
	barrier := make(chan error, 1)
	go func() { barrier <- rt.ReindexPath(testContext(t, 30*time.Second), path) }()
	time.Sleep(50 * time.Millisecond)
	close(hold)
	if err := <-barrier; err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Late") != 1 {
		t.Fatal("the overflowed change was not indexed")
	}
}

// TestLoopSurvivesAPanicAndAsksForARestart: a panic in index work must not
// stop the process. The barrier gets an error instead of waiting for ever, the
// editors are told, the owner is asked to restart the runtime, and later work
// still runs.
func TestLoopSurvivesAPanicAndAsksForARestart(t *testing.T) {
	rt, root := newTestRuntime(t)
	rt.Reporter().SetLogf(func(string, ...any) {})
	bad := writeTestModule(t, root, "lib/bad.ex", "SharedLib.Bad")
	good := writeTestModule(t, root, "lib/good.ex", "SharedLib.Good")
	testHookReconcilePath = func(path string) {
		if path == bad {
			panic("reconcile bug")
		}
	}
	t.Cleanup(func() { testHookReconcilePath = nil })

	err := rt.ReindexPath(testContext(t, 10*time.Second), bad)
	if err == nil || !strings.Contains(err.Error(), "reconcile bug") {
		t.Fatalf("barrier error = %v, want the recovered panic", err)
	}
	select {
	case what := <-rt.Faults():
		if !strings.Contains(what, "index update") {
			t.Fatalf("fault = %q", what)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the owner was not asked to restart the runtime")
	}
	if !rt.Reporter().Active(notify.InternalPrefix + "index") {
		t.Fatal("the editors were not told")
	}
	if err := rt.ReindexPath(testContext(t, 10*time.Second), good); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Good") != 1 {
		t.Fatal("work after the panic did not run")
	}
}

// TestCheckIndexFileSeesAReplacedOrRemovedIndex: an older Dexter build or `git
// clean` can replace or remove the index file under the daemon, which then
// writes to a file that nobody reads again.
func TestCheckIndexFileSeesAReplacedOrRemovedIndex(t *testing.T) {
	rt, root := newTestRuntime(t)
	if got := rt.CheckIndexFile(); got != IndexFileSame {
		t.Fatalf("fresh runtime: %v", got)
	}
	dbPath := store.DBPath(root)
	other := filepath.Join(t.TempDir(), "other")
	st, err := store.Open(other)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if err := os.Rename(store.DBPath(other), dbPath); err != nil {
		t.Fatal(err)
	}
	if got := rt.CheckIndexFile(); got != IndexFileReplaced {
		t.Fatalf("after a replacement: %v, want IndexFileReplaced", got)
	}
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	if got := rt.CheckIndexFile(); got != IndexFileRemoved {
		t.Fatalf("after a removal: %v, want IndexFileRemoved", got)
	}
}

// TestRuntimeStopsTouchingTheIndexWhenTheRootIsGone: when the volume of the
// project is ejected, any access to the index's mapped files can end the
// process with SIGBUS, and a reconcile would prune every file. The runtime
// stops its index work instead.
func TestRuntimeStopsTouchingTheIndexWhenTheRootIsGone(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	path := writeTestModule(t, root, "lib/a.ex", "SharedLib.A")
	rt, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if rt.CheckRoot() {
		t.Fatal("root reported gone while it is there")
	}
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(moved, root) })
	if !rt.CheckRoot() {
		t.Fatal("root not reported gone")
	}
	select {
	case <-rt.Gone():
	default:
		t.Fatal("Gone is not closed")
	}
	if err := rt.ReindexPath(testContext(t, 10*time.Second), path); err == nil {
		t.Fatal("index work ran after the root went away")
	}
}

// TestWatcherReportsTheRootGone covers the watcher side of the same case.
func TestWatcherReportsTheRootGone(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	writeTestModule(t, root, "lib/a.ex", "SharedLib.A")
	gone := make(chan struct{}, 1)
	w, err := Watch(root, WatchCallbacks{
		PathChanged:     func(string) {},
		FullReconcile:   func() {},
		CoverageChanged: func(bool) {},
		RootGone: func() {
			select {
			case gone <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Skipf("no native watcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	time.Sleep(100 * time.Millisecond)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gone:
	case <-time.After(10 * time.Second):
		t.Fatal("the watcher did not report the root gone")
	}
}

// TestSymlinkedIndexIsNotReplaced: a .dexter/dexter.db that is a symlink to
// another disk must be compared through the link both times, or it always
// looks replaced and the daemon shows a sticky error.
func TestSymlinkedIndexIsNotReplaced(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	elsewhere := t.TempDir()
	st, err := store.Open(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if err := os.MkdirAll(filepath.Dir(store.DBPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(store.DBPath(elsewhere), store.DBPath(root)); err != nil {
		t.Skipf("symlink: %v", err)
	}
	rt, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if got := rt.CheckIndexFile(); got != IndexFileSame {
		t.Fatalf("symlinked index: %v, want IndexFileSame", got)
	}
}

// TestCloseWithAFullQueueDoesNotDeadlock: Close sent its stop event while it
// held sendMu, and the loop takes sendMu to read the overflow. When the queue
// was full at that moment, neither could go on.
func TestCloseWithAFullQueueDoesNotDeadlock(t *testing.T) {
	rt, root := newTestRuntime(t)
	path := writeTestModule(t, root, "lib/a.ex", "SharedLib.A")
	closed := make(chan error, 1)
	var once sync.Once
	testHookBeforeOverflow = func() {
		once.Do(func() {
			// Fill the queue, then let Close wait on it before the loop
			// reads the overflow.
			for len(rt.events) < cap(rt.events) {
				rt.ReconcileFile(path)
			}
			go func() { closed <- rt.Close() }()
			time.Sleep(100 * time.Millisecond)
		})
	}
	t.Cleanup(func() { testHookBeforeOverflow = nil })
	rt.ReconcileFile(path)
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close and the loop wait on each other")
	}
}
