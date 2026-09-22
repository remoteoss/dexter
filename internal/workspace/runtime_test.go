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

	"github.com/remoteoss/dexter/internal/version"
)

func TestOpenDoesNotWaitForNativeWatchSetup(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	previous := startNativeWatch
	startNativeWatch = func(string, func(string), func(bool)) (*Watcher, error) {
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
	startNativeWatch = func(root string, onChange func(string), onCoverageChange func(bool)) (*Watcher, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("file descriptors exhausted")
		}
		return previousStart(root, onChange, onCoverageChange)
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
	results, err := rt.Lookup(module, "", false)
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
