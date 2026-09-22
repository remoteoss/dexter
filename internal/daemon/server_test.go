package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/version"
)

func TestServerControlWatcherAndIdleShutdown(t *testing.T) {
	// Keep stdlib/version-manager detection out of this focused daemon test.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	requireSocketSupport(t)

	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, root, 150*time.Millisecond) }()

	client := waitForClient(t, root, runErr)
	var status Status
	if err := client.Call(context.Background(), "daemon/status", struct{}{}, &status); err != nil {
		t.Fatal(err)
	}
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if status.Root != endpoint.Root || status.Contract != ContractVersion {
		t.Fatalf("unexpected status: %+v", status)
	}

	assertLookup(t, client, "SharedLib.One", true)
	writeModule(t, root, "lib/two.ex", "SharedLib.Two")
	eventually(t, 5*time.Second, func() bool {
		return lookupFound(client, "SharedLib.Two")
	})

	// An open control client is a lease: the idle timeout must not stop the
	// daemon while an MCP-style long-lived connection is attached.
	time.Sleep(250 * time.Millisecond)
	assertLookup(t, client, "SharedLib.One", true)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("daemon exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not exit after idle timeout")
	}

	endpoint, err = ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(endpoint.Socket); !os.IsNotExist(err) {
		t.Fatalf("socket remains after shutdown: %v", err)
	}
}

func TestRunReplacesStaleSocketOnlyAfterTakingOwnership(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	requireSocketSupport(t)
	root := t.TempDir()
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(endpoint.Socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, root, time.Minute) }()
	client := waitForClient(t, root, runErr)
	_ = client.Close()
	cancel()
	if err := <-runErr; err != nil {
		t.Fatal(err)
	}
}

func waitForClient(t *testing.T, root string, runErr <-chan error) *Client {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-runErr:
			t.Fatalf("daemon exited before listening: %v", err)
		default:
		}
		client, err := Dial(context.Background(), root)
		if err == nil {
			return client
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("daemon did not listen: %v", lastErr)
	return nil
}

// writeModule writes one Elixir module into the workspace and returns its path.
func writeModule(t *testing.T, root, relative, module string) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	text := "defmodule " + module + " do\n  def run, do: :ok\nend\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertLookup(t *testing.T, client *Client, module string, want bool) {
	t.Helper()
	// The daemon answers the handshake while its initial reconciliation may
	// still be running, so a positive lookup is polled rather than sampled once.
	if want {
		deadline := time.Now().Add(5 * time.Second)
		for !lookupFound(client, module) {
			if time.Now().After(deadline) {
				t.Fatalf("lookup %s = false, want true", module)
			}
			time.Sleep(10 * time.Millisecond)
		}
		return
	}
	if lookupFound(client, module) {
		t.Fatalf("lookup %s = true, want false", module)
	}
}

func lookupFound(client *Client, module string) bool {
	var result LookupResult
	err := client.Call(context.Background(), "workspace/lookup", LookupParams{Module: module, FollowDelegates: true}, &result)
	return err == nil && len(result.Locations) > 0
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met")
}

func TestSecondServerCannotTakeOwnedWorkspace(t *testing.T) {
	root := t.TempDir()
	owner, _, err := AcquireOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Release() }()
	err = Run(context.Background(), root, time.Second)
	if !errors.Is(err, ErrWorkspaceOwned) {
		t.Fatalf("Run error = %v, want ErrWorkspaceOwned", err)
	}
}

func TestDaemonRejectsAnotherSymlinkSpelling(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	client := startDaemon(t, root, time.Minute)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Ensure(ctx, alias)
	var mismatch *RootMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Ensure through alias error = %v, want RootMismatchError", err)
	}
	if mismatch.Daemon != root || mismatch.Requested != alias {
		t.Fatalf("root mismatch = %+v", mismatch)
	}
}

// TestShutdownRefusesAttachedClientsUnlessForced covers both halves of
// `dexter stop`: a plain stop must not yank the workspace from under another
// frontend, and --force must, because that is the only manual way out when an
// editor session would otherwise hold the daemon alive forever.
func TestShutdownRefusesAttachedClientsUnlessForced(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	startDaemon(t, root, time.Minute) // holds one lease

	other, err := Dial(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()

	if err := other.Call(context.Background(), MethodShutdown, ShutdownParams{}, nil); err == nil {
		t.Fatal("shutdown without force was accepted while a client was attached")
	}
	if err := other.Call(context.Background(), MethodShutdown, ShutdownParams{Force: true}, nil); err != nil {
		t.Fatalf("forced shutdown: %v", err)
	}
	// startDaemon's cleanup waits for Run to return, so a forced stop that did
	// not work would fail this test's cleanup as well.
}

func TestDialRejectsNonSocketEndpoint(t *testing.T) {
	root := t.TempDir()
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(endpoint.Socket, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = net.DialTimeout("unix", endpoint.Socket, 20*time.Millisecond)
	if err == nil {
		t.Fatal("dial unexpectedly succeeded")
	}
}

// TestEnsureFailsFastWhenTheOwnerServesNoSocket covers a live workspace owner
// that never binds a socket, as `dexter init` does while it rebuilds: a spawned
// daemon could only lose the ownership race, so Ensure must report the reason
// promptly instead of waiting out its whole start timeout.
func TestEnsureFailsFastWhenTheOwnerServesNoSocket(t *testing.T) {
	quietEnv(t)
	root := t.TempDir()
	ownership, _, err := AcquireOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ownership.Release() }()

	start := time.Now()
	client, err := Ensure(context.Background(), root)
	if err == nil {
		_ = client.Close()
		t.Fatal("Ensure connected to a workspace whose owner serves no socket")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Ensure took %s, want it to fail once the owner is known", elapsed)
	}
	if !strings.Contains(err.Error(), "not serving its daemon socket") {
		t.Errorf("error = %q, want it to name the owner that serves no socket", err)
	}
}

// TestConnectionPanicDoesNotKillTheDaemon covers the shared-process blast
// radius: one session's handler panicking must end that connection, not the
// workspace every other editor is using.
func TestConnectionPanicDoesNotKillTheDaemon(t *testing.T) {
	socketTestEnv(t)
	kind := fmt.Sprintf("panic-%d", time.Now().UnixNano())
	RegisterFrontend(kind, panicFrontend{})
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	client := startDaemon(t, root, time.Minute)

	conn, _, _, err := dialKind(context.Background(), root, kind, "")
	if err == nil {
		_ = conn.Close()
	}
	// The panic unwinds after the handshake, so give it a moment and then check
	// that the same daemon is still answering on the control connection.
	eventually(t, 5*time.Second, func() bool {
		var status Status
		return client.Call(context.Background(), MethodStatus, struct{}{}, &status) == nil
	})
	assertLookup(t, client, "SharedLib.One", true)
}

type panicFrontend struct{}

func (panicFrontend) Serve(FrontendConn) error { panic("frontend exploded on purpose") }

// startDaemon runs a daemon for root in the background and returns a control
// client. Cleanup stops the daemon and waits for Run to return, so a test cannot
// leak a process holding the workspace lock.
func startDaemon(t *testing.T, root string, idle time.Duration) *Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, root, idle) }()
	client := waitForClient(t, root, runErr)
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		select {
		case err := <-runErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("daemon exit: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	return client
}

// quietEnv keeps stdlib and version-manager detection from spawning shells.
func quietEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
}

// requireSocketSupport skips a test where the OS or a sandbox refuses AF_UNIX
// binds. The protocol itself is covered without a socket in pipe_test.go, so a
// skip here costs only Run's listen, accept, and idle-exit behavior.
//
// The probe binds in the daemon's own runtime directory, not t.TempDir():
// sockaddr_un paths are capped near 104 bytes, and a temp dir is often long
// enough to fail on its own, which would skip these tests even where the daemon
// could listen fine.
func requireSocketSupport(t *testing.T) {
	t.Helper()
	dirs := platformRuntimeDirs()
	if len(dirs) == 0 {
		t.Skip("no daemon runtime directory candidates")
	}
	dir := dirs[0]
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Skipf("cannot create %s: %v", dir, err)
	}
	probe := filepath.Join(dir, fmt.Sprintf("probe-%d.sock", os.Getpid()))
	listener, err := net.Listen("unix", probe)
	if err != nil {
		t.Skipf("unix socket bind unavailable in %s: %v", dir, err)
	}
	_ = listener.Close()
	_ = os.Remove(probe)
}

// socketTestEnv is the setup for a test that needs both a quiet environment and
// a working socket.
func socketTestEnv(t *testing.T) {
	t.Helper()
	quietEnv(t)
	requireSocketSupport(t)
}

func TestWorkspaceStatusReportsReadinessAndIndex(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	client := startDaemon(t, root, time.Minute)

	// waitReadyMs replaces a frontend-side index barrier: the caller asks the
	// daemon to hold the request until the workspace can answer.
	status, err := client.WorkspaceStatus(context.Background(), 30_000)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if status.Root != endpoint.Root {
		t.Fatalf("status root %q, want %q", status.Root, endpoint.Root)
	}
	if !status.Ready {
		t.Fatal("workspace is not ready after waiting for it")
	}
	if !status.Watching {
		t.Fatal("daemon reports no native watcher; changes would only reach the index through editor notifications")
	}
	if status.Files < 1 || status.Definitions < 1 {
		t.Fatalf("index looks empty: %+v", status)
	}
	if status.IndexVersion != version.IndexVersion || status.ExpectedIndexVersion != version.IndexVersion {
		t.Fatalf("index version %d (expected %d) does not match the binary's %d",
			status.IndexVersion, status.ExpectedIndexVersion, version.IndexVersion)
	}
	if len(status.Sessions) != 0 {
		t.Fatalf("sessions with no editor attached: %+v", status.Sessions)
	}

	daemonStatus, err := client.DaemonStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if daemonStatus.PID != os.Getpid() {
		t.Fatalf("daemon pid %d, want this process %d", daemonStatus.PID, os.Getpid())
	}
	if !daemonStatus.Ready || daemonStatus.Contract != ContractVersion || daemonStatus.Clients < 1 {
		t.Fatalf("unexpected daemon status: %+v", daemonStatus)
	}
}

// An LSP connection is a named session, and only a frontend that was handed that
// name may attach to it: answering from the wrong editor's unsaved buffers would
// be worse than answering from disk.
func TestLSPSessionIsNamedAndAttachIsExplicit(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	client := startDaemon(t, root, time.Minute)
	ctx := context.Background()

	conn, _, helloResp, err := dialKind(ctx, root, kindLSP, "")
	if err != nil {
		t.Fatal(err)
	}
	if helloResp.Session == "" {
		t.Fatal("LSP connection was not given a session id")
	}

	var status WorkspaceStatus
	if err := client.Call(ctx, MethodWorkspaceStatus, StatusParams{WaitReadyMs: 30_000}, &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Sessions) != 1 || status.Sessions[0].ID != helloResp.Session {
		t.Fatalf("sessions = %+v, want only %q", status.Sessions, helloResp.Session)
	}

	attached, _, attachedHello, err := dialKind(ctx, root, kindControl, helloResp.Session)
	if err != nil {
		t.Fatalf("attaching to the editor session: %v", err)
	}
	if !attachedHello.OK {
		t.Fatal("attach handshake was refused")
	}
	_ = attached.Close()

	if _, _, _, err := dialKind(ctx, root, kindControl, "nosuch-999"); err == nil {
		t.Fatal("attaching to an unknown session succeeded")
	}

	_ = conn.Close()
	eventually(t, 10*time.Second, func() bool {
		var after WorkspaceStatus
		if err := client.Call(ctx, MethodWorkspaceStatus, StatusParams{}, &after); err != nil {
			return false
		}
		return len(after.Sessions) == 0
	})
}

// The workspace owns the only watchers, so a frontend subscribes for changes
// instead of watching the tree a second time.
func TestWatchReportsCoalescedChangesUntilUnwatched(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	client := startDaemon(t, root, time.Minute)
	ctx := context.Background()

	changes := make(chan Changed, 64)
	unwatch, err := client.Watch(ctx, 16, func(change Changed) {
		select {
		case changes <- change:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	// The daemon's own watcher sees this write; the client does nothing else.
	writeModule(t, root, "lib/two.ex", "SharedLib.Two")
	select {
	case change := <-changes:
		if change.Subscription == "" {
			t.Fatalf("change without a subscription id: %+v", change)
		}
		if !change.Full && len(change.Paths) == 0 {
			t.Fatalf("change names nothing: %+v", change)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no change notification for a new file")
	}

	var reindexed ReindexResult
	if err := client.Call(ctx, MethodReindex, ReindexParams{}, &reindexed); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(20 * time.Second)
	sawFull := false
	for !sawFull {
		select {
		case change := <-changes:
			sawFull = change.Full
		case <-deadline:
			t.Fatal("an explicit reindex did not report a full change")
		}
	}

	unwatch()
	writeModule(t, root, "lib/three.ex", "SharedLib.Three")
	select {
	case change := <-changes:
		t.Fatalf("notification after unwatch: %+v", change)
	case <-time.After(500 * time.Millisecond):
	}
}

// One connection carries many in-flight calls, so a response must reach the
// caller that asked. A cross-wired id would answer one module's lookup with
// another module's locations.
func TestConcurrentCallsReachTheirOwnCallers(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	const modules = 5
	for i := 0; i < modules; i++ {
		writeModule(t, root, fmt.Sprintf("lib/mod%d.ex", i), fmt.Sprintf("SharedLib.Mod%d", i))
	}
	client := startDaemon(t, root, time.Minute)
	ctx := context.Background()
	if _, err := client.WorkspaceStatus(ctx, 30_000); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	failures := make(chan error, modules*8)
	for i := 0; i < modules; i++ {
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				module := fmt.Sprintf("SharedLib.Mod%d", i)
				for n := 0; n < 3; n++ {
					var result LookupResult
					if err := client.Call(ctx, MethodLookup, LookupParams{Module: module}, &result); err != nil {
						failures <- fmt.Errorf("%s: %w", module, err)
						return
					}
					if len(result.Locations) == 0 {
						failures <- fmt.Errorf("%s: no locations", module)
						return
					}
					for _, loc := range result.Locations {
						if want := fmt.Sprintf("mod%d.ex", i); !strings.HasSuffix(loc.FilePath, want) {
							failures <- fmt.Errorf("%s resolved to %s", module, loc.FilePath)
							return
						}
					}
					if !result.Ready {
						failures <- fmt.Errorf("%s: lookup reported a workspace that is not ready", module)
						return
					}
				}
			}(i)
		}
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

// srvtestFrontendServed reports the connection a registered adapter was handed.
var srvtestFrontendServed = make(chan FrontendConn, 1)

type blockingMethodState struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

var (
	blockingMethodMu sync.Mutex
	blockingMethod   *blockingMethodState
)

type srvtestFrontend struct{}

func (srvtestFrontend) Serve(fc FrontendConn) error {
	srvtestFrontendServed <- fc
	return nil
}

func init() {
	RegisterFrontend("srvtest", srvtestFrontend{})
	RegisterMethod("srvtest/ping", func(mc MethodContext, _ json.RawMessage) (any, error) {
		return map[string]any{
			"root":    mc.Runtime.Root(),
			"hasLSP":  mc.LSP() != nil,
			"ready":   mc.Runtime.IsReady(),
			"session": mc.Session,
		}, nil
	})
	RegisterMethod("srvtest/panic", func(MethodContext, json.RawMessage) (any, error) {
		panic("method exploded on purpose")
	})
	RegisterMethod("srvtest/block", func(mc MethodContext, _ json.RawMessage) (any, error) {
		blockingMethodMu.Lock()
		state := blockingMethod
		blockingMethodMu.Unlock()
		close(state.started)
		<-mc.Done
		close(state.canceled)
		<-state.release
		_, err := mc.Runtime.IndexStatus()
		return nil, err
	})
}

func TestControlMethodPanicDoesNotStopDaemon(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	client := startDaemon(t, root, time.Minute)
	if err := client.Call(context.Background(), "srvtest/panic", struct{}{}, nil); err == nil {
		t.Fatal("panicking method returned no error")
	}
	if _, err := client.DaemonStatus(context.Background()); err != nil {
		t.Fatalf("daemon stopped after method panic: %v", err)
	}
}

func TestConnectionWaitsForControlRequestsBeforeRuntimeClose(t *testing.T) {
	quietEnv(t)
	root := t.TempDir()
	s, endpoint := pipeServer(t, root)
	serverConn, clientConn := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- s.serveConn(serverConn) }()
	reader := bufio.NewReader(clientConn)
	if err := writeJSONLine(clientConn, hello{
		Contract: ContractVersion,
		Kind:     kindControl,
		Root:     endpoint.Root,
		Identity: endpoint.Identity,
	}); err != nil {
		t.Fatal(err)
	}
	var response helloResponse
	if err := readJSONLine(reader, &response); err != nil || !response.OK {
		t.Fatalf("handshake = %+v, %v", response, err)
	}
	client := newClient(clientConn, reader)
	state := &blockingMethodState{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
	}
	blockingMethodMu.Lock()
	blockingMethod = state
	blockingMethodMu.Unlock()
	t.Cleanup(func() {
		blockingMethodMu.Lock()
		blockingMethod = nil
		blockingMethodMu.Unlock()
	})
	callDone := make(chan error, 1)
	go func() { callDone <- client.Call(context.Background(), "srvtest/block", struct{}{}, nil) }()
	<-state.started
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	<-state.canceled
	select {
	case err := <-served:
		t.Fatalf("connection returned before its request completed: %v", err)
	default:
	}
	close(state.release)
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("connection did not wait for its request")
	}
	select {
	case <-callDone:
	case <-time.After(time.Second):
		t.Fatal("client call did not finish")
	}
}

// Adapters register instead of editing the daemon, so a protocol added later
// (MCP) reaches the same workspace without touching dispatch or ownership.
func TestRegisteredFrontendAndMethodAreServed(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	client := startDaemon(t, root, time.Minute)
	ctx := context.Background()
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}

	var ping struct {
		Root    string `json:"root"`
		HasLSP  bool   `json:"hasLSP"`
		Ready   bool   `json:"ready"`
		Session string `json:"session"`
	}
	if _, err := client.WorkspaceStatus(ctx, 30_000); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "srvtest/ping", struct{}{}, &ping); err != nil {
		t.Fatal(err)
	}
	if ping.Root != endpoint.Root {
		t.Fatalf("registered method saw root %q, want %q", ping.Root, endpoint.Root)
	}
	if !ping.HasLSP {
		t.Fatal("registered method could not resolve a language service")
	}
	if !ping.Ready {
		t.Fatal("registered method saw a workspace that is not ready")
	}

	frontendConn, _, helloResp, err := dialKind(ctx, root, "srvtest", "")
	if err != nil {
		t.Fatalf("handshake for a registered kind: %v", err)
	}
	defer func() { _ = frontendConn.Close() }()
	if !helloResp.OK {
		t.Fatal("registered frontend handshake was refused")
	}
	select {
	case fc := <-srvtestFrontendServed:
		if fc.Conn == nil || fc.Reader == nil || fc.Context == nil || fc.Runtime == nil || fc.LSP == nil || fc.Notify == nil || fc.Done == nil {
			t.Fatalf("frontend connection is incomplete: %+v", fc)
		}
		if fc.Runtime.Root() != endpoint.Root {
			t.Fatalf("frontend runtime root %q, want %q", fc.Runtime.Root(), endpoint.Root)
		}
		if fc.LSP() == nil {
			t.Fatal("frontend cannot resolve a language service")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("registered frontend was never served")
	}

	if _, _, _, err := dialKind(ctx, root, "nosuchfrontend", ""); err == nil {
		t.Fatal("an unregistered connection kind was accepted")
	}
}

// TestDaemonExitsWhenItsSocketDisappears: a temp cleaner removing the socket
// must not leave a daemon holding the workspace lock while unreachable; exiting
// releases the lock so the next frontend starts a replacement.
func TestDaemonExitsWhenItsSocketDisappears(t *testing.T) {
	socketTestEnv(t)
	previous := socketCheckInterval
	socketCheckInterval = 25 * time.Millisecond
	t.Cleanup(func() { socketCheckInterval = previous })

	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, root, time.Minute) }()
	client := waitForClient(t, root, runErr)
	defer func() { _ = client.Close() }()

	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(endpoint.Socket); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("daemon exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("daemon kept running after its socket disappeared")
	}
}
