package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeDaemon answers handshakes at root with one canned rejection. It returns
// the listener so a test can close it to model a daemon that exits.
func fakeDaemon(t *testing.T, root string, res helloResponse) net.Listener {
	t.Helper()
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", endpoint.Socket)
	if err != nil {
		t.Skipf("unix socket bind unavailable: %v", err)
	}
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(endpoint.Socket)
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				var h hello
				if err := readJSONLine(bufio.NewReader(conn), &h); err != nil {
					return
				}
				_ = writeJSONLine(conn, res)
			}(conn)
		}
	}()
	return listener
}

// TestDialReportsIncompatibleDaemonDirection covers both upgrade directions. A
// newer daemon must be reported as newer so a frontend leaves it alone; an
// older one must be marked replaceable. The message also has to be actionable
// on its own, because a frontend that predates the typed error only sees it.
func TestDialReportsIncompatibleDaemonDirection(t *testing.T) {
	requireSocketSupport(t)

	cases := map[string]struct {
		res             helloResponse
		wantClientNewer bool
		wantReplaceable bool
	}{
		"daemon newer": {helloResponse{
			Error: "newer", Contract: ContractVersion + 1,
			PID: 4242, Incompatible: true,
		}, false, false},
		"client newer, already exiting": {helloResponse{
			Error: "older", Contract: ContractVersion - 1,
			PID: 4242, Incompatible: true, Exiting: true,
		}, true, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			fakeDaemon(t, root, tc.res)

			_, err := Dial(context.Background(), root)
			var incompatible *IncompatibleDaemonError
			if !errors.As(err, &incompatible) {
				t.Fatalf("Dial error = %v, want IncompatibleDaemonError", err)
			}
			if incompatible.ClientNewer != tc.wantClientNewer {
				t.Fatalf("ClientNewer = %v, want %v: %+v", incompatible.ClientNewer, tc.wantClientNewer, incompatible)
			}
			if incompatible.Exiting != tc.res.Exiting {
				t.Fatalf("Exiting = %v, want %v: %+v", incompatible.Exiting, tc.res.Exiting, incompatible)
			}
			if !strings.Contains(incompatible.Error(), tc.res.Error) {
				t.Fatalf("error does not carry the daemon's reason: %v", incompatible)
			}
		})
	}
}

// TestDialRejectsADaemonThatPredatesVersionChecks covers the same-build-but-old
// case: a daemon from a build before the contract traveled in the handshake
// answers OK with no index, which this build cannot trust.
func TestDialRejectsADaemonThatPredatesVersionChecks(t *testing.T) {
	requireSocketSupport(t)
	root := t.TempDir()
	fakeDaemon(t, root, helloResponse{OK: true, PID: 4242})

	_, err := Dial(context.Background(), root)
	var incompatible *IncompatibleDaemonError
	if !errors.As(err, &incompatible) || !incompatible.ClientNewer {
		t.Fatalf("Dial error = %v, want a client-newer mismatch", err)
	}
}

// TestEnsureLeavesANewerDaemonAlone: an old frontend must not replace the newer
// daemon that serves the workspace; it reports the mismatch and stops.
func TestEnsureLeavesANewerDaemonAlone(t *testing.T) {
	requireSocketSupport(t)
	root := t.TempDir()
	fakeDaemon(t, root, helloResponse{
		Error: "newer", Contract: ContractVersion + 1,
		PID: 4242, Incompatible: true,
	})

	origSpawn := spawnDaemon
	spawned := make(chan struct{}, 1)
	spawnDaemon = func(string) error { spawned <- struct{}{}; return errors.New("should not spawn") }
	t.Cleanup(func() { spawnDaemon = origSpawn })

	client, err := Ensure(context.Background(), root)
	if client != nil {
		_ = client.Close()
	}
	var incompatible *IncompatibleDaemonError
	if !errors.As(err, &incompatible) || incompatible.ClientNewer {
		t.Fatalf("Ensure error = %v, want a newer-daemon error", err)
	}
	select {
	case <-spawned:
		t.Fatal("Ensure tried to replace a newer daemon")
	default:
	}
}

// TestEnsureReplacesAnOlderDaemon: a newer frontend reaches the spawn path once
// the old daemon's lock is free. The daemon here never held the lock, which is
// exactly the state a self-exiting daemon leaves behind.
func TestEnsureReplacesAnOlderDaemon(t *testing.T) {
	requireSocketSupport(t)
	root := t.TempDir()
	fakeDaemon(t, root, helloResponse{
		Error: "older", Contract: ContractVersion - 1,
		PID: 4242, Incompatible: true, Exiting: true,
	})

	origSpawn := spawnDaemon
	spawned := make(chan struct{}, 1)
	spawnDaemon = func(string) error { spawned <- struct{}{}; return errors.New("spawn reached") }
	t.Cleanup(func() { spawnDaemon = origSpawn })

	_, err := Ensure(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "spawn reached") {
		t.Fatalf("Ensure error = %v, want it to reach the replacement spawn", err)
	}
	select {
	case <-spawned:
	case <-time.After(5 * time.Second):
		t.Fatal("Ensure never tried to start a replacement")
	}
}

// TestReplaceIncompatibleSignalsPreContractDaemon covers the bootstrap path: a
// daemon that predates the restart contract cannot shut itself down, so the pid
// from its refusal is signaled and the workspace is waited for.
func TestReplaceIncompatibleSignalsPreContractDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sleep helper on windows")
	}
	root := t.TempDir()
	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	exited := make(chan struct{})
	go func() { _, _ = child.Process.Wait(); close(exited) }()
	defer func() { _ = child.Process.Kill() }()

	err := ReplaceIncompatible(context.Background(), root, &IncompatibleDaemonError{
		DaemonContract: ContractVersion - 1,
		DaemonPID:      child.Process.Pid,
	})
	if err != nil {
		t.Fatalf("ReplaceIncompatible: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the pre-contract daemon was not signaled")
	}
}

// TestParseDaemonPid covers the fallback that finds a daemon whose refusal
// predates the pid field: only a `daemon <root>` command line may match, so a
// lookup against the same workspace cannot be mistaken for it.
func TestParseDaemonPid(t *testing.T) {
	root := "/Users/someone/code/project"
	uid := fmt.Sprintf("%d", os.Getuid())
	other := fmt.Sprintf("%d", os.Getuid()+1)
	line := func(id string, pid int, rest string) string {
		return fmt.Sprintf("  %s %5d %s\n", id, pid, rest)
	}
	ps := line(uid, 111, "/usr/bin/sleep 60") +
		line(uid, 222, "/Users/someone/.bin/dexter daemon "+root) +
		line(uid, 333, "dexter lookup Enum reduce -C "+root) +
		line(uid, 444, "dexter daemon /Users/someone/code/other") +
		line(uid, 555, "dexter daemon "+root+" extra") +
		line(other, 666, "dexter daemon "+root)

	if pid, ok := parseDaemonPid(ps, root); !ok || pid != 222 {
		t.Fatalf("parseDaemonPid = %d, %v; want 222", pid, ok)
	}
	if _, ok := parseDaemonPid(ps, "/Users/someone/code/nothing"); ok {
		t.Fatal("parseDaemonPid matched an unrelated workspace")
	}
}

// TestStopForcedFindsAndSignals covers the no-handshake recovery path: a
// process whose command line is `daemon <root>` is located and terminated, the
// same way a wedged daemon or one whose refusal predates the pid is handled.
func TestStopForcedFindsAndSignals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shell helper on windows")
	}
	root := t.TempDir()
	script := filepath.Join(t.TempDir(), "daemon-helper.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(script, "daemon", root)
	if err := child.Start(); err != nil {
		t.Skipf("cannot start helper: %v", err)
	}
	exited := make(chan struct{})
	go func() { _, _ = child.Process.Wait(); close(exited) }()
	defer func() { _ = child.Process.Kill() }()

	handled, err := StopForced(context.Background(), root)
	if !handled {
		t.Fatal("StopForced did not find the helper process")
	}
	if err != nil {
		t.Fatalf("StopForced: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("helper process was not terminated")
	}
}

// TestStopForcedEscalatesToKill covers a daemon that ignores SIGTERM (for
// example one blocked in a store call): after the grace period it is killed
// outright, because the index is a derived cache and an uncatchable kill is
// recoverable.
func TestStopForcedEscalatesToKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no shell helper on windows")
	}
	previous := signalGrace
	signalGrace = 50 * time.Millisecond
	t.Cleanup(func() { signalGrace = previous })

	root := t.TempDir()
	script := filepath.Join(t.TempDir(), "stubborn-daemon.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(script, "daemon", root)
	if err := child.Start(); err != nil {
		t.Skipf("cannot start helper: %v", err)
	}
	exited := make(chan struct{})
	go func() { _, _ = child.Process.Wait(); close(exited) }()
	defer func() { _ = child.Process.Kill() }()

	handled, err := StopForced(context.Background(), root)
	if !handled {
		t.Fatal("StopForced did not find the helper process")
	}
	if err != nil {
		t.Fatalf("StopForced: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon that ignored SIGTERM was not killed")
	}
}
