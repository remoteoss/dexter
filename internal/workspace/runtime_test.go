package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/notify/notifytest"
	"github.com/remoteoss/dexter/internal/store"
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
	deadline := time.Now().Add(10 * time.Second)
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
	// Keep stdlib and version-manager detection from spawning shells, and
	// from finding the user's Elixir in ~/.local/share/mise and the like: a
	// test must not depend on whether the machine has Elixir.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	t.Setenv("HOME", t.TempDir())
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

// Close cancels the initial reconciliation instead of waiting for it, so a
// daemon that is told to stop releases its workspace lock at once. The pass it
// cancels leaves a consistent index, and the next open finishes it.
func TestCloseCancelsInitialReconcileAndReopenFinishes(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	writeTestModule(t, root, "lib/seed.ex", "MyApp.Seed")
	rt, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}

	const files = 2000
	for i := 0; i < files; i++ {
		writeTestModule(t, root, fmt.Sprintf("lib/gen/mod%d.ex", i), fmt.Sprintf("MyApp.Gen%d", i))
	}

	rt, err = OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Close took %s during the initial reconciliation", took)
	}

	rt, err = OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.WaitReady(testContext(t, 60*time.Second)); err != nil {
		t.Fatal(err)
	}
	paths, err := rt.Store().ListFilePathsUnder(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != files+1 {
		t.Errorf("after reopen: %d project files indexed, want %d", len(paths), files+1)
	}
	for _, module := range []string{"MyApp.Seed", "MyApp.Gen0", fmt.Sprintf("MyApp.Gen%d", files-1)} {
		if got := countModule(t, rt, module); got != 1 {
			t.Errorf("%s: %d definitions after reopen, want 1", module, got)
		}
	}
}

// A removal that is queued or running when Close starts is canceled with the
// rest of the index work, so a large removal (a rebuild of the symbol tables)
// cannot hold shutdown. The rows it did not remove stay consistent, and the
// next open prunes them.
func TestCloseCancelsQueuedRemoval(t *testing.T) {
	rt, root := newTestRuntime(t)
	gone := filepath.Join(root, "lib", "gone")
	for i := 0; i < 20; i++ {
		writeTestModule(t, root, fmt.Sprintf("lib/gone/mod%d.ex", i), fmt.Sprintf("MyApp.Gone%d", i))
	}
	if err := rt.Reindex(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := countModule(t, rt, "MyApp.Gone3"); got != 1 {
		t.Fatalf("MyApp.Gone3 indexed %d times before the removal, want 1", got)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	// Hold the mutation loop at the removal until Close has started.
	reached := make(chan struct{})
	release := make(chan struct{})
	testHookReconcilePath = func(path string) {
		if path == gone {
			close(reached)
			<-release
		}
	}
	t.Cleanup(func() { testHookReconcilePath = nil })
	rt.RemoveFile(gone)
	<-reached

	closed := make(chan error, 1)
	start := time.Now()
	go func() { closed <- rt.Close() }()
	time.Sleep(100 * time.Millisecond) // Close cancels index work first
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Close took %s", took)
	}
	testHookReconcilePath = nil

	// The canceled removal left the rows of the deleted files.
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	under, err := s.ListFilePathsUnder(gone)
	_ = s.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(under) != 20 {
		t.Errorf("%d rows under the removed directory after Close, want 20 (the removal was not canceled)", len(under))
	}

	// The next open prunes them.
	reopened, err := OpenWithOptions(root, Options{NoWatch: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := countModule(t, reopened, "MyApp.Gone3"); got != 0 {
		t.Errorf("MyApp.Gone3 still indexed after reopen: %d", got)
	}
}

func indexedUnder(t *testing.T, rt *Runtime, root string) int {
	t.Helper()
	paths, err := rt.Store().ListFilePathsUnder(root)
	if err != nil {
		t.Fatal(err)
	}
	return len(paths)
}

func isGone(rt *Runtime) bool {
	select {
	case <-rt.Gone():
		return true
	default:
		return false
	}
}

// A project root that is moved away must not empty the index, which moves with
// it. A full pass (what FSEvents starts on RootChanged or Unmount) and a path
// event for the root (what inotify reports for a moved root) both stop the
// runtime and change nothing. Before, the full pass pruned every file and the
// path event removed every file below the root.
func TestRootGoneStopsRuntimeAndKeepsIndex(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(ctx context.Context, rt *Runtime, root string) error
	}{
		{name: "full pass", run: func(ctx context.Context, rt *Runtime, _ string) error { return rt.Reindex(ctx) }},
		{name: "root path event", run: func(ctx context.Context, rt *Runtime, root string) error { return rt.awaitPath(ctx, root) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, root := newTestRuntime(t)
			for i := 0; i < 3; i++ {
				writeTestModule(t, root, fmt.Sprintf("lib/m%d.ex", i), fmt.Sprintf("SharedLib.M%d", i))
			}
			if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
				t.Fatal(err)
			}
			if indexedUnder(t, rt, root) != 3 {
				t.Fatal("setup: the project was not indexed")
			}
			if err := os.Rename(root, root+"-moved"); err != nil {
				t.Fatal(err)
			}

			err := tc.run(testContext(t, 10*time.Second), rt, root)
			if n := indexedUnder(t, rt, root); n != 3 {
				t.Fatalf("%d files in the index after the root moved, want 3", n)
			}
			if !errors.Is(err, lsp.ErrRootGone) {
				t.Fatalf("result = %v, want ErrRootGone", err)
			}
			if !isGone(rt) {
				t.Fatal("Gone is not closed")
			}
			// Later work changes nothing and reports why.
			if err := rt.awaitPath(testContext(t, 10*time.Second), filepath.Join(root, "lib", "m0.ex")); !errors.Is(err, lsp.ErrRootGone) {
				t.Fatalf("later path result = %v, want ErrRootGone", err)
			}
			if n := indexedUnder(t, rt, root); n != 3 {
				t.Fatalf("%d files in the index after a later event, want 3", n)
			}
			if err := rt.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
		})
	}
}

func setRetryDelays(t *testing.T, initial, maximum time.Duration) {
	t.Helper()
	prevInitial, prevMax := retryInitialDelay, retryMaxDelay
	retryInitialDelay, retryMaxDelay = initial, maximum
	t.Cleanup(func() { retryInitialDelay, retryMaxDelay = prevInitial, prevMax })
}

// A full disk makes writes fail. Before, the pass said every file was updated
// and nothing tried again until a manual reindex. Now the runtime retries the
// pass with a growing delay until the writes work, and the files get indexed
// with no other event.
func TestIncompletePassIsRetriedUntilWritesWork(t *testing.T) {
	setRetryDelays(t, 20*time.Millisecond, 80*time.Millisecond)
	var passes atomic.Int32
	var completed atomic.Bool // a pass ended with no error
	testHookFullPass = func(err error) {
		passes.Add(1)
		if err == nil {
			completed.Store(true)
		}
	}
	t.Cleanup(func() { testHookFullPass = nil })

	rt, root := newTestRuntime(t)
	// A successful pass over a first file, so that the store is not empty:
	// a pass over an empty store is a cold build, which does not use the
	// write fault.
	writeTestModule(t, root, "lib/first.ex", "SharedLib.First")
	if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		writeTestModule(t, root, fmt.Sprintf("lib/full%d.ex", i), fmt.Sprintf("SharedLib.Full%d", i))
	}
	var diskFull atomic.Bool
	diskFull.Store(true)
	restore := lsp.SetTestWriteFault(func(string) error {
		if diskFull.Load() {
			return errors.New("database or disk is full")
		}
		return nil
	})
	t.Cleanup(restore)

	passes.Store(0)
	completed.Store(false)
	if err := rt.Reindex(testContext(t, 10*time.Second)); !errors.Is(err, lsp.ErrIncomplete) {
		t.Fatalf("reindex on a full disk = %v, want ErrIncomplete", err)
	}
	if countModule(t, rt, "SharedLib.Full0") != 0 {
		t.Fatal("a failed write was indexed")
	}
	// The retries come with no other event.
	eventually(t, 10*time.Second, func() bool { return passes.Load() >= 4 })
	diskFull.Store(false)
	eventually(t, 10*time.Second, func() bool {
		for i := 0; i < 5; i++ {
			if countModule(t, rt, fmt.Sprintf("SharedLib.Full%d", i)) == 0 {
				return false
			}
		}
		return true
	})

	// After a complete pass, nothing is scheduled. The pass that wrote the
	// files can still run when they show (on a slow machine for longer than
	// any fixed wait), so wait until it has ended.
	eventually(t, 10*time.Second, completed.Load)
	n := passes.Load()
	time.Sleep(200 * time.Millisecond)
	if passes.Load() != n {
		t.Errorf("passes went on after the index was complete: %d, then %d", n, passes.Load())
	}
}

// Each new retry waits twice as long as the one before, up to the bound. A
// retry that is still pending is not moved, and a complete pass resets the
// delay.
func TestRetryDelayGrowsToBound(t *testing.T) {
	setRetryDelays(t, time.Hour, 4*time.Hour)
	r := &Runtime{events: make(chan event, 16)}
	defer r.stopRetry()
	for _, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		r.scheduleRetry()
		if r.retryDelay != want {
			t.Fatalf("retry delay = %s, want %s", r.retryDelay, want)
		}
		due := r.retryDue
		r.scheduleRetry() // still pending
		if r.retryDelay != want || r.retryDue != due {
			t.Fatalf("a pending retry was moved: delay %s, due %s", r.retryDelay, r.retryDue)
		}
		r.stopRetry() // as if it ran
	}
	_ = r.fullPassDone(nil)
	r.scheduleRetry()
	if r.retryDelay != time.Hour {
		t.Fatalf("retry delay after a complete pass = %s, want %s", r.retryDelay, time.Hour)
	}
}

// A single-file write that fails (a save on a full disk) schedules the same
// retry, and the returned error tells the caller.
func TestFailedFileWriteIsRetried(t *testing.T) {
	setRetryDelays(t, 20*time.Millisecond, 80*time.Millisecond)
	rt, root := newTestRuntime(t)
	path := writeTestModule(t, root, "lib/saved.ex", "SharedLib.Saved")
	var diskFull atomic.Bool
	diskFull.Store(true)
	restore := lsp.SetTestWriteFault(func(string) error {
		if diskFull.Load() {
			return errors.New("database or disk is full")
		}
		return nil
	})
	t.Cleanup(restore)

	if err := rt.ReindexPath(testContext(t, 10*time.Second), path); !errors.Is(err, lsp.ErrIncomplete) {
		t.Fatalf("reindex of one file on a full disk = %v, want ErrIncomplete", err)
	}
	diskFull.Store(false)
	eventually(t, 10*time.Second, func() bool { return countModule(t, rt, "SharedLib.Saved") == 1 })
}

// A branch switch in an umbrella project changes every apps/*/mix.exs at once.
// The batch gets one full pass; before, each manifest started its own.
func TestManifestBatchRunsOneFullPass(t *testing.T) {
	rt, root := newTestRuntime(t)
	var manifests []string
	for i := 0; i < 20; i++ {
		p := filepath.Join(root, "apps", fmt.Sprintf("app%d", i), "mix.exs")
		writeTestModule(t, root, filepath.Join("apps", fmt.Sprintf("app%d", i), "mix.exs"), fmt.Sprintf("App%d.MixProject", i))
		manifests = append(manifests, p)
	}
	// Indexed only by a full pass: no event reports it.
	writeTestModule(t, root, "lib/unreported.ex", "SharedLib.Unreported")

	// Hold the loop in a full pass while the manifest events queue, so that
	// they arrive as one batch, as a burst from the watcher does.
	var passes atomic.Int32
	hold := make(chan struct{})
	held := make(chan struct{})
	var holdOnce sync.Once
	testHookFullPass = func(error) {
		holdOnce.Do(func() {
			close(held)
			<-hold
		})
		passes.Add(1)
	}
	t.Cleanup(func() { testHookFullPass = nil })
	// The caches that depend on the Mix projects are cleared, and failed
	// BEAMs forgotten, once for the batch too.
	var manifestCalls atomic.Int32
	testHookManifestChanged = func() { manifestCalls.Add(1) }
	t.Cleanup(func() { testHookManifestChanged = nil })
	go func() { _ = rt.Reindex(context.Background()) }()
	<-held
	for _, p := range manifests {
		rt.ReconcileFile(p)
	}
	done := make(chan error, 1)
	go func() { done <- rt.awaitPath(context.Background(), manifests[0]) }()
	// Let the barrier queue behind the manifests before the loop goes on.
	eventually(t, 10*time.Second, func() bool { return len(rt.events) == len(manifests)+1 })
	close(hold)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the manifest batch did not finish")
	}

	if got := passes.Load() - 1; got != 1 {
		t.Fatalf("%d full passes for a batch of %d manifests, want 1", got, len(manifests))
	}
	if countModule(t, rt, "SharedLib.Unreported") == 0 {
		t.Fatal("the manifest batch did not reindex the workspace")
	}
	if got := manifestCalls.Load(); got != 1 {
		t.Fatalf("ManifestChanged ran %d times for a batch of %d manifests, want 1", got, len(manifests))
	}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met")
}

// A root that cannot be read for a while (EACCES here; EPERM from macOS
// privacy controls, or EIO and ESTALE on a network mount) is not gone. Before,
// the first such stat stopped the workspace for good, and the daemon exited
// with every editor session. Now nothing changes until the error clears, and
// then the work goes on.
func TestUnreadableRootRecovers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, a directory with mode 0 is still searchable")
	}
	rt, root := newTestRuntime(t)
	keep := writeTestModule(t, root, "lib/keep.ex", "SharedLib.Keep")
	if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(root)
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	if err := rt.Reindex(testContext(t, 10*time.Second)); !errors.Is(err, lsp.ErrIncomplete) {
		t.Fatalf("reindex with an unreadable root = %v, want ErrIncomplete", err)
	}
	if err := rt.awaitPath(testContext(t, 10*time.Second), keep); !errors.Is(err, lsp.ErrIncomplete) {
		t.Fatalf("path event with an unreadable root = %v, want ErrIncomplete", err)
	}
	if isGone(rt) {
		t.Fatal("an unreadable root stopped the workspace")
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if n := indexedUnder(t, rt, root); n != 1 {
		t.Fatalf("%d files in the index after the root was unreadable, want 1", n)
	}
	writeTestModule(t, root, "lib/later.ex", "SharedLib.Later")
	if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
		t.Fatalf("reindex after the error cleared = %v", err)
	}
	if countModule(t, rt, "SharedLib.Later") == 0 || countModule(t, rt, "SharedLib.Keep") == 0 {
		t.Fatal("the workspace did not recover")
	}
}

// The batch checks the root once, before its first path. When the root goes
// after that check, every later path is missing. A removal now checks the root
// again, so it does not delete the rows of a whole subtree.
func TestRootGoneDuringBatchRemovesNothing(t *testing.T) {
	rt, root := newTestRuntime(t)
	for i := 0; i < 3; i++ {
		writeTestModule(t, root, fmt.Sprintf("lib/sub/m%d.ex", i), fmt.Sprintf("SharedLib.M%d", i))
	}
	if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	testHookReconcilePath = func(string) {
		once.Do(func() {
			if err := os.Rename(root, root+"-moved"); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(func() { testHookReconcilePath = nil })
	err := rt.awaitPath(testContext(t, 10*time.Second), filepath.Join(root, "lib", "sub"))
	if n := indexedUnder(t, rt, root); n != 3 {
		t.Fatalf("%d files in the index after the root went during a batch, want 3", n)
	}
	if !errors.Is(err, lsp.ErrRootGone) {
		t.Fatalf("result = %v, want ErrRootGone", err)
	}
}

// A delete that fails (a full disk fails a delete too) was only logged, and
// the row stayed until the next start. Now it schedules the retry, whose prune
// removes the row.
func TestFailedRemovalIsRetried(t *testing.T) {
	setRetryDelays(t, 20*time.Millisecond, 80*time.Millisecond)
	rt, root := newTestRuntime(t)
	path := writeTestModule(t, root, "lib/gone.ex", "SharedLib.Gone")
	if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	var diskFull atomic.Bool
	diskFull.Store(true)
	restore := lsp.SetTestWriteFault(func(string) error {
		if diskFull.Load() {
			return errors.New("database or disk is full")
		}
		return nil
	})
	t.Cleanup(restore)
	if err := rt.awaitPath(testContext(t, 10*time.Second), path); !errors.Is(err, lsp.ErrIncomplete) {
		t.Fatalf("removal on a full disk = %v, want ErrIncomplete", err)
	}
	diskFull.Store(false)
	eventually(t, 10*time.Second, func() bool { return countModule(t, rt, "SharedLib.Gone") == 0 })
}

// The index holds only paths below the project root and the stdlib root. A
// path outside is not indexed, by an event or by `dexter reindex`; before, it
// stayed in the index for good, because the prune finds that it exists.
func TestPathOutsideRootIsNotIndexed(t *testing.T) {
	rt, _ := newTestRuntime(t)
	outside := writeTestModule(t, t.TempDir(), "lib/scratch.ex", "SharedLib.Scratch")
	if err := rt.ReindexPath(testContext(t, 10*time.Second), outside); err == nil {
		t.Fatal("reindex of a path outside the workspace did not fail")
	}
	if err := rt.awaitPath(testContext(t, 10*time.Second), outside); err != nil {
		t.Fatal(err)
	}
	if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Scratch") != 0 {
		t.Fatal("a file outside the root was indexed")
	}
}

// A path below a directory that became a file fails to stat with ENOTDIR.
// The event must remove it, as ENOENT does.
func TestPathBelowDirectoryThatBecameFileIsRemoved(t *testing.T) {
	rt, root := newTestRuntime(t)
	path := writeTestModule(t, root, "lib/a/b.ex", "SharedLib.Below")
	if err := rt.ReindexPath(testContext(t, 10*time.Second), path); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Below") == 0 {
		t.Fatal("setup: the file was not indexed")
	}
	dir := filepath.Join(root, "lib", "a")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rt.ReindexPath(testContext(t, 10*time.Second), path); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Below") != 0 {
		t.Fatal("a path below a directory that is now a file is still indexed")
	}
}

// A manifest batch is one full pass. The pass covers every other path of the
// batch, because the index holds only paths that the walk yields: a changed
// source file in the same batch gets its new rows.
func TestManifestBatchKeepsOtherChanges(t *testing.T) {
	rt, root := newTestRuntime(t)
	manifest := writeTestModule(t, root, "mix.exs", "Root.MixProject")
	dep := writeTestModule(t, root, "lib/dep.ex", "SharedLib.DepOld")
	if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}

	hold := make(chan struct{})
	held := make(chan struct{})
	var holdOnce sync.Once
	testHookFullPass = func(error) {
		holdOnce.Do(func() {
			close(held)
			<-hold
		})
	}
	t.Cleanup(func() { testHookFullPass = nil })
	go func() { _ = rt.Reindex(context.Background()) }()
	<-held
	writeTestModule(t, root, "lib/dep.ex", "SharedLib.DepNew")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(dep, future, future); err != nil {
		t.Fatal(err)
	}
	rt.ReconcileFile(dep)
	done := make(chan error, 1)
	go func() { done <- rt.awaitPath(context.Background(), manifest) }()
	eventually(t, 10*time.Second, func() bool { return len(rt.events) == 2 })
	close(hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.DepOld") != 0 || countModule(t, rt, "SharedLib.DepNew") == 0 {
		t.Fatal("the manifest batch lost the change to another file")
	}
}

// git worktree move can leave a directory with an empty .git file for a moment,
// and the watcher reports the directory then. Its rows must go, as for a
// settled worktree: the watcher does not report it again when the file
// settles.
func TestDirectoryWithUnsettledGitFileLosesItsRows(t *testing.T) {
	rt, root := newTestRuntime(t)
	wt := filepath.Join(root, "wt")
	dup := writeTestModule(t, wt, "lib/dup.ex", "SharedLib.Dup")
	if err := rt.ReindexPath(testContext(t, 10*time.Second), dup); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Dup") == 0 {
		t.Fatal("setup: the file was not indexed")
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rt.awaitPath(testContext(t, 10*time.Second), wt); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.Dup") != 0 {
		t.Fatal("rows below a directory with an unsettled .git file stayed")
	}
}

// Before the daemon exits because its root is gone, every attached editor is
// told why. Before, the sessions ended with no message.
func TestRootGoneTellsEditors(t *testing.T) {
	rt, root := newTestRuntime(t)
	client := notifytest.New()
	detach := rt.Reporter().Attach(client, false)
	defer detach()
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := rt.Reindex(testContext(t, 10*time.Second)); !errors.Is(err, lsp.ErrRootGone) {
		t.Fatalf("reindex = %v, want ErrRootGone", err)
	}
	rt.Reporter().Flush(5 * time.Second)
	if got := client.Messages(); len(got) == 0 || !strings.Contains(got[len(got)-1].Message, "was moved or deleted") {
		t.Fatalf("editor was not told that the root is gone:\n%s", client.Dump())
	}
}

// A batch that waits because the root cannot be read changed nothing, so it is
// not published as a change. Before, subscribers heard of it as applied.
func TestBatchThatWaitsIsNotPublished(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, a directory with mode 0 is still searchable")
	}
	setRetryDelays(t, time.Hour, time.Hour)
	rt, root := newTestRuntime(t)
	keep := writeTestModule(t, root, "lib/keep.ex", "SharedLib.Keep")
	if err := rt.Reindex(testContext(t, 10*time.Second)); err != nil {
		t.Fatal(err)
	}
	changes, cancel := rt.Subscribe(8)
	defer cancel()
	parent := filepath.Dir(root)
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if err := rt.awaitPath(testContext(t, 10*time.Second), keep); !errors.Is(err, lsp.ErrIncomplete) {
		t.Fatalf("path event with an unreadable root = %v, want ErrIncomplete", err)
	}
	select {
	case c := <-changes:
		t.Fatalf("a batch that changed nothing was published: %+v", c)
	case <-time.After(100 * time.Millisecond):
	}
}

// The same with real git: after git worktree move, a directory made at the
// old path is indexed by a single-file reindex, not only by a full pass.
func TestReindexPathAfterWorktreeMove(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	rt, root := newTestRuntime(t) // clears PATH, so git is called by its path
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	main := writeTestModule(t, root, "lib/main.ex", "SharedLib.Main")
	git("init", "-q")
	git("add", "lib/main.ex")
	git("commit", "-q", "-m", "init")
	wt := filepath.Join(root, "wt")
	git("worktree", "add", "-q", "-b", "feature", wt)
	// Past the racy window, so the cache trusts what it reads.
	records := filepath.Join(root, ".git", "worktrees")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(records, old, old); err != nil {
		t.Fatal(err)
	}
	// One save fills the cache.
	if err := rt.ReindexPath(testContext(t, 10*time.Second), main); err != nil {
		t.Fatal(err)
	}
	if err := rt.ReindexPath(testContext(t, 10*time.Second), filepath.Join(wt, "lib", "main.ex")); err != nil {
		t.Fatal(err)
	}
	git("worktree", "move", wt, filepath.Join(t.TempDir(), "moved"))
	if err := os.Chtimes(records, old, old); err != nil {
		t.Fatal(err)
	}
	b := writeTestModule(t, root, "wt/lib/b.ex", "SharedLib.B")
	if err := rt.ReindexPath(testContext(t, 10*time.Second), b); err != nil {
		t.Fatal(err)
	}
	if countModule(t, rt, "SharedLib.B") != 1 {
		t.Fatal("a file at the old path of a moved worktree was not indexed")
	}
}

// A watcher sends a directory that was renamed or removed and is there again,
// such as one replaced by `mv new lib/area`. The files of the old directory
// that are gone must leave the index; the files that are still there stay.
func TestReconcileReplacedDirectoryRemovesFilesThatAreGone(t *testing.T) {
	rt, root := newTestRuntime(t)
	ctx := testContext(t, 20*time.Second)
	kept := writeTestModule(t, root, "lib/area/kept.ex", "SharedLib.Area.Kept")
	gone := writeTestModule(t, root, "lib/area/nested/gone.ex", "SharedLib.Area.Gone")
	for _, path := range []string{kept, gone} {
		if err := rt.ReindexPath(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Dir(gone)); err != nil {
		t.Fatal(err)
	}
	if err := rt.awaitPath(ctx, filepath.Join(root, "lib", "area")); err != nil {
		t.Fatal(err)
	}
	if got := countModule(t, rt, "SharedLib.Area.Gone"); got != 0 {
		t.Fatalf("a file that is gone from the replaced directory is still indexed (%d results)", got)
	}
	if got := countModule(t, rt, "SharedLib.Area.Kept"); got == 0 {
		t.Fatal("a file that is still in the directory left the index")
	}
}

// skipCaseSensitive skips a test on a volume that does not ignore case.
func skipCaseSensitive(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(probe) }()
	if _, err := os.Stat(filepath.Join(dir, "caseprobe")); err != nil {
		t.Skip("the volume is case-sensitive")
	}
}

// indexedPaths returns the indexed files that define module.
func indexedPaths(t *testing.T, rt *Runtime, module string) []string {
	t.Helper()
	results, err := rt.Store().LookupModule(module)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range results {
		paths = append(paths, r.FilePath)
	}
	return paths
}

// A rename that changes only the case of a directory (mv lib/Area lib/area)
// left both spellings indexed, because a stat of the old one still succeeds.
// The watcher reports both spellings; only the new one may stay.
func TestCaseOnlyRenameOfDirectoryLeavesOneSpelling(t *testing.T) {
	rt, root := newTestRuntime(t)
	skipCaseSensitive(t, root)
	ctx := testContext(t, 20*time.Second)
	old := writeTestModule(t, root, "lib/Area/kept.ex", "SharedLib.Area.Kept")
	if err := rt.ReindexPath(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "lib", "Area"), filepath.Join(root, "lib", "area")); err != nil {
		t.Fatal(err)
	}
	// What FSEvents sends: both spellings of the directory, and the walk of
	// the new one.
	for _, path := range []string{filepath.Join(root, "lib", "Area"), filepath.Join(root, "lib", "area"), filepath.Join(root, "lib", "area", "kept.ex")} {
		if err := rt.awaitPath(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	want := filepath.Join(root, "lib", "area", "kept.ex")
	if got := indexedPaths(t, rt, "SharedLib.Area.Kept"); !slices.Equal(got, []string{want}) {
		t.Fatalf("indexed = %v, want [%s]", got, want)
	}
}

// The prune of a full reconcile must see the old spelling as gone too.
func TestCaseOnlyRenameIsPrunedByFullReconcile(t *testing.T) {
	rt, root := newTestRuntime(t)
	skipCaseSensitive(t, root)
	ctx := testContext(t, 20*time.Second)
	old := writeTestModule(t, root, "lib/Area/kept.ex", "SharedLib.Area.Kept")
	if err := rt.ReindexPath(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "lib", "Area"), filepath.Join(root, "lib", "area")); err != nil {
		t.Fatal(err)
	}
	if err := rt.Reindex(ctx); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "lib", "area", "kept.ex")
	if got := indexedPaths(t, rt, "SharedLib.Area.Kept"); !slices.Equal(got, []string{want}) {
		t.Fatalf("indexed = %v, want [%s]", got, want)
	}
}

// The same for a file: after mv kept.ex Kept.ex, a stat of the old spelling
// succeeds, and the old spelling must leave the index.
func TestCaseOnlyRenameOfFileLeavesOneSpelling(t *testing.T) {
	rt, root := newTestRuntime(t)
	skipCaseSensitive(t, root)
	ctx := testContext(t, 20*time.Second)
	old := writeTestModule(t, root, "lib/kept.ex", "SharedLib.Kept")
	if err := rt.ReindexPath(ctx, old); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(root, "lib", "Kept.ex")
	// A file that is renamed was usually written long before; a rename
	// keeps its mtime.
	earlier := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, earlier, earlier); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{old, renamed} {
		if err := rt.awaitPath(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	if got := indexedPaths(t, rt, "SharedLib.Kept"); !slices.Equal(got, []string{renamed}) {
		t.Fatalf("indexed = %v, want [%s]", got, renamed)
	}
}

// The Warning about a BEAM that cannot start says that a change of mix.lock
// makes Dexter try again. The editor reports only .ex and .exs files, so the
// change comes from the native watcher, and the runtime must forget the
// failure too, not only reindex.
func TestManifestChangeFromTheWatcherRetriesFailedBeam(t *testing.T) {
	rt, root := newTestRuntime(t)
	ctx := testContext(t, 30*time.Second)
	install := t.TempDir()
	starts := filepath.Join(install, "starts")
	if err := os.MkdirAll(filepath.Join(install, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, script := range map[string]string{
		"elixir": "#!/bin/sh\necho start >> " + starts + "\necho '** (RuntimeError) could not load the formatter plugin' >&2\nexit 1\n",
		"mix":    "#!/bin/sh\ncat\n",
	} {
		if err := os.WriteFile(filepath.Join(install, "bin", name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(install, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, session, release := rt.AttachLSPSession()
	defer release()
	if _, err := session.Initialize(ctx, &protocol.InitializeParams{
		InitializationOptions: map[string]interface{}{"stdlibPath": filepath.Join(install, "lib")},
	}); err != nil {
		t.Fatal(err)
	}
	source := writeTestModule(t, root, "lib/worker.ex", "SharedLib.Worker")
	open := func() {
		t.Helper()
		if err := session.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
			URI: protocol.DocumentURI("file://" + source), LanguageID: "elixir", Text: "defmodule SharedLib.Worker do\nend\n",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		data, _ := os.ReadFile(starts)
		return strings.Count(string(data), "start")
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out: %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	failed := func() bool {
		for _, c := range rt.Reporter().Conditions() {
			if strings.HasPrefix(c.Key, "formatter.beam:") {
				return true
			}
		}
		return false
	}

	open()
	waitFor("the first BEAM start", func() bool { return count() == 1 })
	waitFor("the startup failure report", failed)
	open()
	time.Sleep(200 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("the failing BEAM started %d times before the fix, want 1", n)
	}

	lock := filepath.Join(root, "mix.lock")
	if err := os.WriteFile(lock, []byte("%{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rt.awaitPath(ctx, lock); err != nil {
		t.Fatal(err)
	}
	open()
	waitFor("a new BEAM start after the mix.lock change", func() bool { return count() == 2 })
}
