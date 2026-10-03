package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/version"
)

func TestOpenDoesNotWaitForNativeWatchSetup(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	previous := startNativeWatch
	startNativeWatch = func(string, WatchCallbacks) (*Watcher, error) {
		close(started)
		<-release
		return nil, errors.New("watch unavailable")
	}
	t.Cleanup(func() { startNativeWatch = previous })

	type openResult struct {
		runtime *Runtime
		err     error
	}
	opened := make(chan openResult, 1)
	go func() {
		runtime, err := Open(root)
		opened <- openResult{runtime: runtime, err: err}
	}()
	<-started

	var result openResult
	select {
	case result = <-opened:
	case <-time.After(100 * time.Millisecond):
		close(release)
		result = <-opened
		if result.runtime != nil {
			_ = result.runtime.Close()
		}
		t.Fatal("Open waited for native watcher setup")
	}
	if result.err != nil {
		close(release)
		t.Fatal(result.err)
	}
	t.Cleanup(func() { _ = result.runtime.Close() })

	writeTestModule(t, root, "lib/during_startup.ex", "SharedLib.DuringStartup")
	close(release)
	if err := result.runtime.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if countModule(t, result.runtime, "SharedLib.DuringStartup") == 0 {
		t.Fatal("initial reconciliation missed a file created during watcher setup")
	}
}

func TestRuntimeRetriesNativeWatcherCreation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	previousStart := startNativeWatch
	previousInterval := watchRetryInterval
	var attempts atomic.Int32
	startNativeWatch = func(root string, callbacks WatchCallbacks) (*Watcher, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("file descriptors exhausted")
		}
		return previousStart(root, callbacks)
	}
	watchRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		startNativeWatch = previousStart
		watchRetryInterval = previousInterval
	})

	rt, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	deadline := time.Now().Add(time.Second)
	for !rt.Watching() {
		if time.Now().After(deadline) {
			t.Fatalf("native watcher was not restored after %d attempts", attempts.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if attempts.Load() < 2 {
		t.Fatalf("watch creation attempts = %d, want at least 2", attempts.Load())
	}
}

// These tests open the runtime with NoWatch so the mutation queue is driven
// explicitly. With a native watcher running, every write also produces an event
// and batch composition stops being deterministic.

func newTestRuntime(t *testing.T) (*Runtime, string) {
	t.Helper()
	// Keep stdlib and version-manager detection from spawning shells.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	rt, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	return rt, root
}

func testContext(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

// setDebounce overrides the coalescing window for one test.
func setDebounce(t *testing.T, d time.Duration) {
	t.Helper()
	previous := eventDebounce
	eventDebounce = d
	t.Cleanup(func() { eventDebounce = previous })
}

func writeTestModule(t *testing.T, root, relative, module string) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "defmodule " + module + " do\n  def run, do: :ok\nend\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func countModule(t *testing.T, rt *Runtime, module string) int {
	t.Helper()
	results, err := rt.LanguageServices().LookupName(module, "", lsp.NameLookupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return len(results)
}

func readChange(t *testing.T, changes <-chan Change) Change {
	t.Helper()
	select {
	case change, ok := <-changes:
		if !ok {
			t.Fatal("subscription closed unexpectedly")
		}
		return change
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an index change")
	}
	return Change{}
}

// A manifest change in a nested worktree belongs to that checkout, not to this
// workspace, so it must not start a workspace reindex.
func TestManifestInNestedWorktreeDoesNotReindexWorkspace(t *testing.T) {
	rt, root := newTestRuntime(t)
	wt := filepath.Join(root, ".claude", "worktrees", "feature")
	makeLinkedWorktree(t, root, wt)
	manifest := filepath.Join(wt, "mix.exs")
	if err := os.WriteFile(manifest, []byte("defmodule Feature.MixProject do\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No event reports this file, so only a workspace reindex can index it.
	writeTestModule(t, root, "lib/unreported.ex", "SharedLib.Unreported")

	if err := rt.ReindexPath(testContext(t, 10*time.Second), manifest); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Unreported") != 0 {
		t.Fatal("a nested worktree's mix.exs reindexed the workspace")
	}
}

// cp -r can copy a worktree's files before its .git file, so some can be indexed
// first. When the watcher reports the directory, those rows go.
func TestDirectoryThatBecomesWorktreeLosesItsRows(t *testing.T) {
	rt, root := newTestRuntime(t)
	wt := filepath.Join(root, "copied")
	early := writeTestModule(t, wt, "lib/early.ex", "SharedLib.Early")
	if err := rt.ReindexPath(testContext(t, 10*time.Second), early); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Early") == 0 {
		t.Fatal("setup: the early file was not indexed")
	}
	makeLinkedWorktree(t, root, wt)
	main := filepath.Join(root, "lib", "main.ex")
	if err := rt.ReindexPath(testContext(t, 10*time.Second), main); err != nil {
		t.Fatal(err)
	}
	// The watcher reports the directory itself, which is not a full reindex.
	if err := rt.awaitPath(testContext(t, 10*time.Second), wt); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Early") != 0 {
		t.Fatal("rows indexed before the .git file appeared are still there")
	}
	if countModule(t, rt, "Main") == 0 {
		t.Fatal("rows outside the worktree went too")
	}
}

// An index built before nested worktrees were skipped holds their files. They
// still exist on disk, so the sweep must remove them for another reason.
func TestReindexRemovesRowsFromNestedWorktree(t *testing.T) {
	rt, root := newTestRuntime(t)
	wt := filepath.Join(root, ".claude", "worktrees", "feature")
	old := writeTestModule(t, wt, "lib/old.ex", "SharedLib.OldCopy")
	if err := rt.ReindexPath(testContext(t, 10*time.Second), old); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.OldCopy") == 0 {
		t.Fatal("setup: the worktree file was not indexed")
	}
	makeLinkedWorktree(t, root, wt)
	if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.OldCopy") != 0 {
		t.Fatal("the sweep kept a row from a nested worktree")
	}
	if countModule(t, rt, "Main") == 0 {
		t.Fatal("the sweep did not index the project")
	}
}

// git worktree remove deletes the .git file before the rest of the checkout,
// and its record last. A sweep in between must drop rows from the worktree, not
// keep them because the files still exist.
func TestReindexRemovesRowsFromWorktreeBeingRemoved(t *testing.T) {
	rt, root := newTestRuntime(t)
	wt := filepath.Join(root, ".claude", "worktrees", "feature")
	old := writeTestModule(t, wt, "lib/old.ex", "SharedLib.OldCopy")
	if err := rt.ReindexPath(testContext(t, 10*time.Second), old); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.OldCopy") == 0 {
		t.Fatal("setup: the worktree file was not indexed")
	}
	makeLinkedWorktree(t, root, wt)
	if err := os.Remove(filepath.Join(wt, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.OldCopy") != 0 {
		t.Fatal("the sweep kept a row from a worktree that git still records")
	}
	if countModule(t, rt, "Main") == 0 {
		t.Fatal("the sweep did not index the project")
	}
}

// In a linked worktree, .git is a file and HEAD is in the git directory it
// names. A branch switch there must still reconcile the workspace.
func TestGitWatchFollowsLinkedWorktreeHead(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	previous := gitHeadPollInterval
	gitHeadPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { gitHeadPollInterval = previous })

	base := t.TempDir()
	admin := filepath.Join(base, "app", ".git", "worktrees", "feature")
	head := filepath.Join(admin, "HEAD")
	root := filepath.Join(base, "feature")
	for path, content := range map[string]string{
		head:                              "ref: refs/heads/feature\n",
		filepath.Join(admin, "commondir"): "../..\n",
		filepath.Join(root, ".git"):       "gitdir: " + admin + "\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rt, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	changes, cancel := rt.Subscribe(8)
	defer cancel()

	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(head, later, later); err != nil {
		t.Fatal(err)
	}
	if change := readChange(t, changes); !change.Full {
		t.Fatalf("change = %+v, want a full reconcile", change)
	}
}

// An isolated editor save must not pay the coalescing window: that window exists
// for bursts, and adding it to every save would be a straight latency tax on the
// most common mutation there is.
func TestIsolatedChangeIsNotDelayedByCoalescingWindow(t *testing.T) {
	setDebounce(t, 2*time.Second)
	rt, root := newTestRuntime(t)
	path := writeTestModule(t, root, "lib/worker.ex", "SharedLib.Worker")

	start := time.Now()
	if err := rt.ReindexPath(testContext(t, 10*time.Second), path); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= eventDebounce {
		t.Fatalf("one isolated change waited %v, at least the %v window", elapsed, eventDebounce)
	}
	if countModule(t, rt, "SharedLib.Worker") == 0 {
		t.Fatal("reconciled file is missing from the index")
	}
}

// A burst is coalesced into one bounded window instead of one window per event,
// and every path in it still lands in the index.
func TestBurstIsCoalescedNotSerializedPerEvent(t *testing.T) {
	setDebounce(t, 100*time.Millisecond)
	rt, root := newTestRuntime(t)
	changes, cancel := rt.Subscribe(64)
	defer cancel()

	const count = 5
	var paths []string
	for i := 0; i < count; i++ {
		paths = append(paths, writeTestModule(t, root, fmt.Sprintf("lib/mod%d.ex", i), fmt.Sprintf("SharedLib.Mod%d", i)))
	}

	start := time.Now()
	for _, path := range paths {
		rt.ReconcileFile(path)
	}
	ctx := testContext(t, 20*time.Second)
	// Barrier on a path, not a full reindex: an eventFull queued behind the
	// burst is absorbed into the same batch, and a full batch reports only
	// Change.Full, which would hide the per-path coalescing under test.
	if err := rt.ReindexPath(ctx, paths[0]); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= time.Duration(count)*eventDebounce {
		t.Fatalf("burst of %d changes took %v; the window was paid per event", count, elapsed)
	}
	for i := 0; i < count; i++ {
		if module := fmt.Sprintf("SharedLib.Mod%d", i); countModule(t, rt, module) == 0 {
			t.Fatalf("%s missing after the burst", module)
		}
	}

	seen := make(map[string]struct{}, len(paths))
	for len(seen) < len(paths) {
		change := readChange(t, changes)
		if change.Full {
			t.Fatalf("a path-only burst published a full change: %+v", change)
		}
		if len(change.Paths) == 0 {
			t.Fatalf("change with no paths: %+v", change)
		}
		for _, path := range change.Paths {
			seen[path] = struct{}{}
		}
	}
}

// A subscriber that cannot keep up loses detail rather than stalling indexing,
// and is then told to refresh coarsely instead of silently missing a change.
func TestSubscriberThatFallsBehindIsToldToRefresh(t *testing.T) {
	rt, root := newTestRuntime(t)
	changes, cancel := rt.Subscribe(1)
	defer cancel()
	ctx := testContext(t, 20*time.Second)

	first := writeTestModule(t, root, "lib/first.ex", "SharedLib.First")
	second := writeTestModule(t, root, "lib/second.ex", "SharedLib.Second")
	third := writeTestModule(t, root, "lib/third.ex", "SharedLib.Third")

	if err := rt.ReindexPath(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Delivered while the buffer is full, so it is dropped and marks the
	// subscriber as having lost detail.
	if err := rt.ReindexPath(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got := readChange(t, changes); len(got.Paths) != 1 || got.Paths[0] != first {
		t.Fatalf("first change = %+v, want only %s", got, first)
	}
	if err := rt.ReindexPath(ctx, third); err != nil {
		t.Fatal(err)
	}
	if got := readChange(t, changes); !got.Full {
		t.Fatalf("change after a drop = %+v, want a full refresh", got)
	}
}

// The reindex barrier must reflect everything accepted before the call, which is
// what a CLI or MCP reindex promises its caller.
func TestReindexBarrierReflectsEarlierEvents(t *testing.T) {
	rt, root := newTestRuntime(t)
	ctx := testContext(t, 20*time.Second)
	path := writeTestModule(t, root, "lib/pending.ex", "SharedLib.Pending")
	rt.ReconcileFile(path)
	if err := rt.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Pending") == 0 {
		t.Fatal("an event queued before the barrier was not indexed when it returned")
	}
}

// Deletion goes through the same queue as a change, so a removed file stops
// resolving instead of lingering as a stale definition.
func TestDeletedFileIsPruned(t *testing.T) {
	rt, root := newTestRuntime(t)
	ctx := testContext(t, 20*time.Second)
	path := writeTestModule(t, root, "lib/gone.ex", "SharedLib.Gone")
	if err := rt.ReindexPath(ctx, path); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Gone") == 0 {
		t.Fatal("file was never indexed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := rt.ReindexPath(ctx, path); err != nil {
		t.Fatalf("reconciling a deleted path: %v", err)
	}
	if got := countModule(t, rt, "SharedLib.Gone"); got != 0 {
		t.Fatalf("deleted file still resolves (%d results)", got)
	}
}

// Editor sessions share the workspace but never each other's state, and a
// frontend can only reach a session it was explicitly given.
func TestSessionRegistryIsExplicit(t *testing.T) {
	rt, _ := newTestRuntime(t)
	firstID, first, releaseFirst := rt.AttachLSPSession()
	secondID, _, releaseSecond := rt.AttachLSPSession()
	defer releaseSecond()
	if first == nil {
		t.Fatal("no session server returned")
	}
	if firstID == secondID {
		t.Fatalf("two sessions share id %q", firstID)
	}
	if got, ok := rt.Session(firstID); !ok || got != first {
		t.Fatalf("Session(%q) = %v, %v", firstID, got, ok)
	}
	sessions := rt.Sessions()
	if len(sessions) != 2 || sessions[0].ID != firstID || sessions[1].ID != secondID {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}
	releaseFirst()
	if _, ok := rt.Session(firstID); ok {
		t.Fatal("a released session is still reachable")
	}
	if len(rt.Sessions()) != 1 {
		t.Fatalf("sessions after release: %+v", rt.Sessions())
	}
	// Releasing twice must be safe: transports defer release on paths that can
	// also fail early.
	releaseFirst()
}

func TestDaemonSessionExplicitStdlibPathUpdatesRuntimeIndex(t *testing.T) {
	initialStdlib := t.TempDir()
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", initialStdlib)
	writeTestModule(t, initialStdlib, "initial/lib/initial.ex", "InitialStdlib.Module")

	root := t.TempDir()
	rt, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}

	explicitStdlib := t.TempDir()
	writeTestModule(t, explicitStdlib, "explicit/lib/explicit.ex", "ExplicitStdlib.Module")
	_, existingSession, releaseExisting := rt.AttachLSPSession()
	defer releaseExisting()
	_, session, release := rt.AttachLSPSession()
	defer release()
	if _, err := session.Initialize(context.Background(), &protocol.InitializeParams{
		InitializationOptions: map[string]interface{}{"stdlibPath": explicitStdlib},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Reindex(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}

	if got := rt.StdlibRoot(); got != explicitStdlib {
		t.Fatalf("runtime stdlib root = %q, want %q", got, explicitStdlib)
	}
	if got := existingSession.StdlibRoot(); got != explicitStdlib {
		t.Fatalf("existing session stdlib root = %q, want %q", got, explicitStdlib)
	}
	if countModule(t, rt, "ExplicitStdlib.Module") == 0 {
		t.Fatal("explicit session stdlib was not indexed by the runtime")
	}
	if got := countModule(t, rt, "InitialStdlib.Module"); got != 0 {
		t.Fatalf("old runtime stdlib remains indexed (%d results)", got)
	}
}

func TestDaemonSessionExplicitStdlibPathDoesNotWaitForInitialReconcile(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	previous := startNativeWatch
	startNativeWatch = func(string, WatchCallbacks) (*Watcher, error) {
		close(started)
		<-release
		return nil, errors.New("watch unavailable")
	}
	t.Cleanup(func() { startNativeWatch = previous })

	rt, err := Open(root)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	<-started
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		_ = rt.Close()
	})
	_, existing, releaseExisting := rt.AttachLSPSession()
	defer releaseExisting()
	_, session, releaseSession := rt.AttachLSPSession()
	defer releaseSession()
	explicitStdlib := t.TempDir()

	initialized := make(chan error, 1)
	go func() {
		_, initErr := session.Initialize(context.Background(), &protocol.InitializeParams{
			InitializationOptions: map[string]interface{}{"stdlibPath": explicitStdlib},
		})
		initialized <- initErr
	}()
	select {
	case err := <-initialized:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		close(release)
		released = true
		<-initialized
		t.Fatal("Initialize waited for the initial workspace reconciliation")
	}
	if got := existing.StdlibRoot(); got != explicitStdlib {
		t.Fatalf("existing session stdlib root = %q, want %q", got, explicitStdlib)
	}
	close(release)
	released = true
	if err := rt.Reindex(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRemovesRowsFromPreviousStdlibRoot(t *testing.T) {
	oldStdlib := t.TempDir()
	writeTestModule(t, oldStdlib, "lib/old.ex", "OldStdlib.Module")
	root := t.TempDir()
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", oldStdlib)
	first, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if countModule(t, first, "OldStdlib.Module") == 0 {
		t.Fatal("first stdlib root was not indexed")
	}
	if err := first.store.SetStdlibRoot(oldStdlib); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	newStdlib := t.TempDir()
	writeTestModule(t, newStdlib, "lib/new.ex", "NewStdlib.Module")
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", newStdlib)
	second, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := second.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if countModule(t, second, "NewStdlib.Module") == 0 {
		t.Fatal("new stdlib root was not indexed")
	}
	if got := countModule(t, second, "OldStdlib.Module"); got != 0 {
		t.Fatalf("previous stdlib root remains indexed (%d results)", got)
	}
}

func TestIndexStatusReportsReadinessAndSize(t *testing.T) {
	rt, root := newTestRuntime(t)
	ctx := testContext(t, 20*time.Second)
	path := writeTestModule(t, root, "lib/status.ex", "SharedLib.Status")
	if err := rt.ReindexPath(ctx, path); err != nil {
		t.Fatal(err)
	}
	status, err := rt.IndexStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready {
		t.Fatal("runtime is not ready after the initial reconcile")
	}
	if status.Root != root {
		t.Fatalf("status root %q, want %q", status.Root, root)
	}
	if status.Files < 1 || status.Definitions < 1 {
		t.Fatalf("index looks empty: %+v", status)
	}
	if status.ExpectedIndexVersion != version.IndexVersion {
		t.Fatalf("expected index version %d, want %d", status.ExpectedIndexVersion, version.IndexVersion)
	}
	if status.Watching {
		t.Fatal("a NoWatch runtime reports native watching")
	}
}

// Shutdown is ordered and idempotent, and a mutation accepted after it must fail
// loudly instead of blocking a caller forever.
func TestCloseIsIdempotentAndRejectsLaterWork(t *testing.T) {
	rt, root := newTestRuntime(t)
	path := writeTestModule(t, root, "lib/closed.ex", "SharedLib.Closed")
	if err := rt.ReindexPath(testContext(t, 20*time.Second), path); err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := rt.Reindex(testContext(t, 2*time.Second)); err == nil {
		t.Fatal("reindex after close succeeded")
	}
	stdlibRoot := rt.StdlibRoot()
	if err := rt.SetStdlibRoot(context.Background(), t.TempDir()); err == nil {
		t.Fatal("stdlib root update after close succeeded")
	}
	if got := rt.StdlibRoot(); got != stdlibRoot {
		t.Fatalf("stdlib root changed after close from %q to %q", stdlibRoot, got)
	}
	// Fire-and-forget notifications must neither block nor panic after close.
	rt.ReconcileFile(path)
	rt.RemoveFile(path)
}

func TestWatchCoverageTransitionsTriggerOneFullReconcileEach(t *testing.T) {
	rt, _ := newTestRuntime(t)
	changes, cancel := rt.Subscribe(8)
	defer cancel()

	rt.watchCoverageChanged(true)
	if change := readChange(t, changes); !change.Full {
		t.Fatalf("degradation change = %+v, want full", change)
	}
	rt.watchCoverageChanged(false)
	if change := readChange(t, changes); !change.Full {
		t.Fatalf("restoration change = %+v, want full", change)
	}

	select {
	case change := <-changes:
		t.Fatalf("coverage transitions caused a recurring change: %+v", change)
	case <-time.After(100 * time.Millisecond):
	}
}
