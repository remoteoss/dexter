package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/workspace"
)

// fastChecks makes the endpoint checks of a daemon run every few
// milliseconds.
func fastChecks(t *testing.T) {
	t.Helper()
	previous := socketCheckInterval
	socketCheckInterval = 20 * time.Millisecond
	t.Cleanup(func() { socketCheckInterval = previous })
}

// runDaemon runs Run for root and returns its result channel and a cancel.
func runDaemon(t *testing.T, root string) (chan error, *Client, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, root, time.Minute) }()
	client := waitForClient(t, root, runErr)
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		select {
		case <-runErr:
		case <-time.After(20 * time.Second):
		}
	})
	return runErr, client, cancel
}

func expectExit(t *testing.T, runErr chan error, want error) {
	t.Helper()
	select {
	case err := <-runErr:
		if want == nil && err != nil {
			t.Fatalf("daemon exit: %v", err)
		}
		if want != nil && !errors.Is(err, want) {
			t.Fatalf("daemon exit = %v, want %v", err, want)
		}
		runErr <- err // the cleanup reads it too
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon kept running")
	}
}

// TestDaemonSurvivesAPanicInAnLSPRequest is the audit's F1 case: a nil
// dereference in one editor's hover stopped the daemon for every editor and
// CLI call. Now the request fails and the daemon keeps serving.
func TestDaemonSurvivesAPanicInAnLSPRequest(t *testing.T) {
	socketTestEnv(t)
	root, caller, text := proxyFixture(t)
	_, client, _ := runDaemon(t, root)
	restore := lsp.SetRequestHookForTesting(func(method string) {
		if method == "textDocument/hover" {
			var nothing *Endpoint
			_ = nothing.Root
		}
	})
	defer restore()

	conn, reader, _, err := OpenLSP(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	editor := (&lspEditor{tb: t, w: conn, r: reader}).pump()
	initializeEditor(t, editor, root, caller, text)
	editor.request(2, "textDocument/hover", hoverParams(caller))
	res := editor.response(2)
	errObj, ok := res["error"].(map[string]any)
	if code, _ := idAsInt(errObj["code"]); !ok || code != -32603 {
		t.Fatalf("hover = %v, want an InternalError answer", res)
	}
	var status Status
	if err := client.Call(context.Background(), MethodStatus, struct{}{}, &status); err != nil {
		t.Fatalf("the daemon stopped answering: %v", err)
	}
	ws, err := client.WorkspaceStatus(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, note := range ws.Conditions {
		found = found || (note.Key == "internal.request" && note.Severity == "error")
	}
	if !found {
		t.Fatalf("the panic is not a condition: %+v", ws.Conditions)
	}
}

// TestDaemonLocksAgainWhenItsLockFileIsRemoved: an age-based temp cleaner or
// `rm` can remove the lock file of a running daemon. The daemon locks a new
// file at the path, so a second owner cannot start.
func TestDaemonLocksAgainWhenItsLockFileIsRemoved(t *testing.T) {
	socketTestEnv(t)
	fastChecks(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	runErr, _, _ := runDaemon(t, root)
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(endpoint.Lock); err != nil {
		t.Fatal(err)
	}
	eventually(t, 5*time.Second, func() bool {
		if _, err := os.Stat(endpoint.Lock); err != nil {
			return false
		}
		handle, acquired, err := acquirePlatformLock(endpoint.Lock)
		if err == nil && acquired {
			_ = releasePlatformLock(handle)
		}
		return err == nil && !acquired
	})
	select {
	case err := <-runErr:
		t.Fatalf("the daemon exited: %v", err)
	default:
	}
}

// TestDaemonExitsWhenAnotherProcessHoldsANewLockFile: after its lock file was
// removed, another process locked a new one at the path. That process owns the
// workspace now, so the daemon exits.
func TestDaemonExitsWhenAnotherProcessHoldsANewLockFile(t *testing.T) {
	socketTestEnv(t)
	fastChecks(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	runErr, _, _ := runDaemon(t, root)
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the file and hold the new one before the daemon looks.
	tmp := endpoint.Lock + ".new"
	if err := os.WriteFile(tmp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, endpoint.Lock); err != nil {
		t.Fatal(err)
	}
	handle, acquired, err := acquirePlatformLock(endpoint.Lock)
	if err != nil || !acquired {
		t.Fatalf("the new lock file: %v, %v", acquired, err)
	}
	defer func() { _ = releasePlatformLock(handle) }()
	expectExit(t, runErr, nil)
}

// TestDaemonExitsWhenItsSocketIsReplaced: a second daemon bound a new socket
// at the path. This one cannot be reached any more, so it exits, and it must
// not remove the socket of the other one.
func TestDaemonExitsWhenItsSocketIsReplaced(t *testing.T) {
	socketTestEnv(t)
	fastChecks(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	runErr, _, _ := runDaemon(t, root)
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(endpoint.Socket); err != nil {
		t.Fatal(err)
	}
	other, err := net.Listen("unix", endpoint.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	expectExit(t, runErr, nil)
	if _, err := os.Stat(endpoint.Socket); err != nil {
		t.Fatalf("the exiting daemon removed the other socket: %v", err)
	}
}

// TestOwnershipIsRefusedWhileADaemonAnswersWithoutItsLock is the audit's
// `dexter init --force` case: with the lock file removed, a second process
// got the lock on a new file and deleted the index under a running daemon.
func TestOwnershipIsRefusedWhileADaemonAnswersWithoutItsLock(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	runDaemon(t, root) // the default check interval: the daemon does not lock again during the test
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(endpoint.Lock); err != nil {
		t.Fatal(err)
	}
	if ownership, _, err := AcquireOwnership(root); !errors.Is(err, ErrWorkspaceOwned) {
		_ = ownership.Release()
		t.Fatalf("AcquireOwnership = %v, want ErrWorkspaceOwned while the daemon answers", err)
	}
	// An attached client keeps the daemon; maintenance is refused, not run.
	conn, _, _, err := OpenLSP(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if ownership, _, err := AcquireMaintenance(context.Background(), root); err == nil {
		_ = ownership.Release()
		t.Fatal("maintenance ran under a daemon with an attached client")
	}
}

// TestDaemonTouchesItsRuntimeFiles keeps age-based temp cleaners away.
func TestDaemonTouchesItsRuntimeFiles(t *testing.T) {
	socketTestEnv(t)
	fastChecks(t)
	previous := touchInterval
	touchInterval = 0
	t.Cleanup(func() { touchInterval = previous })
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	runDaemon(t, root)
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, path := range []string{endpoint.Lock, endpoint.Socket} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 5*time.Second, func() bool {
		for _, path := range []string{endpoint.Lock, endpoint.Socket} {
			info, err := os.Lstat(path)
			if err != nil || time.Since(info.ModTime()) > time.Hour {
				return false
			}
		}
		return true
	})
}

// workspaceConditions dials again until the daemon answers, and returns the
// active condition keys.
func workspaceConditions(root string) (map[string]string, bool) {
	client, err := Dial(context.Background(), root)
	if err != nil {
		return nil, false
	}
	defer func() { _ = client.Close() }()
	ws, err := client.WorkspaceStatus(context.Background(), 0)
	if err != nil {
		return nil, false
	}
	out := make(map[string]string)
	for _, note := range ws.Conditions {
		out[note.Key] = note.Severity
	}
	return out, true
}

// TestDaemonRestartsItsIndexWhenTheFileIsReplacedOrRemoved: an older Dexter
// build in an editor, or `git clean`, replaced or removed the index file while
// the daemon had it open. The daemon restarts its index on the file that is
// there now, tells the editors, and answers from it.
func TestDaemonRestartsItsIndexWhenTheFileIsReplacedOrRemoved(t *testing.T) {
	for _, mode := range []string{"replaced", "removed"} {
		t.Run(mode, func(t *testing.T) {
			socketTestEnv(t)
			fastChecks(t)
			root := t.TempDir()
			writeModule(t, root, "lib/one.ex", "SharedLib.One")
			runErr, client, _ := runDaemon(t, root)
			if _, err := client.WorkspaceStatus(context.Background(), 30_000); err != nil {
				t.Fatal(err)
			}
			dbPath := store.DBPath(root)
			if mode == "replaced" {
				other := t.TempDir()
				st, err := store.Open(other)
				if err != nil {
					t.Fatal(err)
				}
				_ = st.Close()
				if err := os.Rename(store.DBPath(other), dbPath); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
					_ = os.Remove(path)
				}
			}
			eventually(t, 15*time.Second, func() bool {
				conditions, ok := workspaceConditions(root)
				return ok && conditions[CondIndexReplaced] == "warning"
			})
			eventually(t, 15*time.Second, func() bool {
				client, err := Dial(context.Background(), root)
				if err != nil {
					return false
				}
				defer func() { _ = client.Close() }()
				if _, err := client.WorkspaceStatus(context.Background(), 30_000); err != nil {
					return false
				}
				return lookupFound(client, "SharedLib.One")
			})
			if _, err := os.Stat(dbPath); err != nil {
				t.Fatalf("no index file after the restart: %v", err)
			}
			select {
			case err := <-runErr:
				t.Fatalf("the daemon exited: %v", err)
			default:
			}
		})
	}
}

// TestDaemonExitsWithoutTheIndexWhenTheRootIsGone: an ejected volume takes the
// root and the index with it. The daemon exits without touching the index, and
// it does not create the root again.
func TestDaemonExitsWithoutTheIndexWhenTheRootIsGone(t *testing.T) {
	socketTestEnv(t)
	fastChecks(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	runErr, client, _ := runDaemon(t, root)
	if _, err := client.WorkspaceStatus(context.Background(), 30_000); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, filepath.Join(parent, "moved")); err != nil {
		t.Fatal(err)
	}
	expectExit(t, runErr, ErrRootGone)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the root was created again: %v", err)
	}
	if err := Run(context.Background(), root, time.Minute); err == nil {
		t.Fatal("a daemon started for a root that is not there")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("the root was created again: %v", err)
	}
}

// setCandidates replaces the runtime directory candidates for one test.
func setCandidates(t *testing.T, dirs ...string) {
	t.Helper()
	t.Setenv(RuntimeDirEnv, "")
	previous := platformDirs
	platformDirs = func() []string { return dirs }
	t.Cleanup(func() { platformDirs = previous })
}

// TestResolveEndpointFallsBackWhenTheFirstDirectoryIsUnusable: on a
// multi-user host another user can create /tmp/dexter-<uid> first. Dexter
// then uses the next candidate, and every process of the user agrees on it.
func TestResolveEndpointFallsBackWhenTheFirstDirectoryIsUnusable(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "dxf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	blocked := filepath.Join(base, "blocked")
	if err := os.Symlink(t.TempDir(), blocked); err != nil {
		t.Skipf("symlink: %v", err)
	}
	fallback := filepath.Join(base, "home")
	setCandidates(t, blocked, fallback)
	root := t.TempDir()
	first, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(first.Socket) != fallback {
		t.Fatalf("socket in %s, want the fallback %s", filepath.Dir(first.Socket), fallback)
	}
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	second, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if first.Socket != second.Socket || first.Lock != second.Lock {
		t.Fatalf("the environment changed the endpoint: %+v, %+v", first, second)
	}
}

// TestHomeCandidateComesFromTheUserDatabase: the fallback must not depend on
// $HOME, which a GUI editor and a shell can see differently.
func TestHomeCandidateComesFromTheUserDatabase(t *testing.T) {
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Skipf("no user database entry: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	dirs := platformRuntimeDirs()
	if len(dirs) != 2 {
		t.Fatalf("candidates = %v, want two", dirs)
	}
	if !strings.HasPrefix(dirs[1], u.HomeDir) {
		t.Fatalf("second candidate %s is not under the home directory %s of the user database", dirs[1], u.HomeDir)
	}
}

// TestLongRuntimeDirectoryUsesTheShortKey keeps the socket path inside
// sockaddr_un when the home directory is long.
func TestLongRuntimeDirectoryUsesTheShortKey(t *testing.T) {
	dir := "/tmp/" + strings.Repeat("d", maxSocketPath-1-len("/tmp/")-len("/0123456789abcdef.sock"))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	setCandidates(t, dir)
	endpoint, err := ResolveEndpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(endpoint.Socket)+1 > maxSocketPath {
		t.Fatalf("socket path %s is too long", endpoint.Socket)
	}
	if key := strings.TrimSuffix(filepath.Base(endpoint.Socket), ".sock"); len(key) != 16 {
		t.Fatalf("key %q, want the 16-character key", key)
	}
	setCandidates(t, dir+"dd")
	if _, err := ResolveEndpoint(t.TempDir()); err == nil {
		t.Fatal("a directory too long for any key was accepted")
	}
}

// TestOwnershipIsRefusedWhileAnotherCandidateHoldsTheWorkspace: when the
// directory choice changes, for example because a blocking /tmp/dexter-<uid>
// was removed, a daemon in the old directory still owns the workspace.
func TestOwnershipIsRefusedWhileAnotherCandidateHoldsTheWorkspace(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "dxo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	first, second := filepath.Join(base, "a"), filepath.Join(base, "b")
	root := t.TempDir()
	// The workspace is owned in the second candidate while the first one is
	// unusable.
	if err := os.WriteFile(first, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	setCandidates(t, first, second)
	held, _, err := AcquireOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	// The first candidate becomes usable.
	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(endpoint.Lock) != first {
		t.Fatalf("lock in %s, want %s", filepath.Dir(endpoint.Lock), first)
	}
	if ownership, _, err := AcquireOwnership(root); !errors.Is(err, ErrWorkspaceOwned) {
		_ = ownership.Release()
		t.Fatalf("AcquireOwnership = %v, want ErrWorkspaceOwned", err)
	}
}

// TestCleanRuntimeDirRemovesOnlyDeadWorkspaceFiles: runtime files of
// workspaces that no daemon serves pile up (2,723 entries on one machine);
// files of live daemons and files Dexter did not make stay.
func TestCleanRuntimeDirRemovesOnlyDeadWorkspaceFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	write := func(name string, mtime time.Time) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return path
	}
	dead := "0123456789abcdef0123456789abcdef"
	live := "fedcba9876543210fedcba9876543210"
	recent := "00112233445566778899aabbccddeeff"
	own := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	short := "0123456789abcdef"
	write(dead+".sock", old)
	write(dead+".lock", old)
	write(dead+".log", old)
	write(short+".lock", old)
	liveLock := write(live+".lock", old)
	write(live+".sock", old)
	write(live+".log", old)
	write(recent+".log", now)
	ownLock := write(own+".lock", old)
	write(own+".log", old)
	write("notes.txt", old)
	write("probe-1.sock", old)
	handle, acquired, err := acquirePlatformLock(liveLock)
	if err != nil || !acquired {
		t.Fatal(acquired, err)
	}
	defer func() { _ = releasePlatformLock(handle) }()

	cleanRuntimeDir(dir, ownLock, now)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, entry := range entries {
		left = append(left, entry.Name())
	}
	want := []string{
		recent + ".log",
		own + ".lock", own + ".log",
		live + ".lock", live + ".log", live + ".sock",
		"notes.txt", "probe-1.sock",
	}
	for _, name := range want {
		if !containsString(left, name) {
			t.Errorf("%s was removed", name)
		}
	}
	for _, name := range []string{dead + ".sock", dead + ".lock", dead + ".log", short + ".lock"} {
		if containsString(left, name) {
			t.Errorf("%s was kept", name)
		}
	}
	if len(left) != len(want) {
		t.Errorf("left %v", left)
	}
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

var _ = bufio.NewReader

// TestDaemonSurvivesADidOpenWithAnUntitledURI is a real F1 trigger: VS Code
// sends didOpen with an `untitled:` URI for a new buffer, and converting it to
// a path panicked on the jsonrpc2 handler goroutine, which stopped the daemon
// for every editor. The notification is dropped; the daemon, the session, and
// other sessions keep working.
func TestDaemonSurvivesADidOpenWithAnUntitledURI(t *testing.T) {
	socketTestEnv(t)
	root, caller, text := proxyFixture(t)
	_, client, _ := runDaemon(t, root)
	if _, err := client.WorkspaceStatus(context.Background(), 30_000); err != nil {
		t.Fatal(err)
	}
	open := func() *lspEditor {
		conn, reader, _, err := OpenLSP(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		editor := (&lspEditor{tb: t, w: conn, r: reader}).pump()
		initializeEditor(t, editor, root, caller, text)
		return editor
	}
	first := open()
	first.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": "untitled:Untitled-1", "languageId": "elixir", "version": 1, "text": "defmodule A do\nend\n"},
	})
	first.request(2, "textDocument/hover", hoverParams(caller))
	if res := first.response(2); !strings.Contains(fmtResult(res), "Gets a user") {
		t.Fatalf("the session stopped answering: %v", res)
	}
	second := open()
	second.request(2, "textDocument/hover", hoverParams(caller))
	if res := second.response(2); !strings.Contains(fmtResult(res), "Gets a user") {
		t.Fatalf("another session does not answer: %v", res)
	}
	var status Status
	if err := client.Call(context.Background(), MethodStatus, struct{}{}, &status); err != nil {
		t.Fatalf("the daemon stopped answering: %v", err)
	}
}

func fmtResult(res map[string]any) string {
	b, _ := json.Marshal(res["result"])
	return string(b)
}

// TestEnsureStopsWhenTheSpawnedDaemonFails: a daemon that exits at start used
// to leave the editor waiting the whole start timeout, with "context deadline
// exceeded" at the end. Ensure now stops when the process exits and says so.
func TestEnsureStopsWhenTheSpawnedDaemonFails(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	previous := spawnDaemon
	spawnDaemon = func(string) (<-chan error, error) {
		exited := make(chan error, 1)
		exited <- errors.New("exit status 1")
		return exited, nil
	}
	t.Cleanup(func() { spawnDaemon = previous })
	start := time.Now()
	_, err := Ensure(context.Background(), root)
	var exitErr *DaemonExitError
	if !errors.As(err, &exitErr) || time.Since(start) > 3*time.Second {
		t.Fatalf("Ensure = %v after %s, want a DaemonExitError at once", err, time.Since(start))
	}
}

// TestEnsureStartsAgainWhenTheDaemonFoundAMomentaryOwner: a cleanup or `dexter
// stop` holds the lock of a workspace for a moment. A daemon that starts then
// exits with status 0 (owned), and nobody serves; Ensure starts one again.
func TestEnsureStartsAgainWhenTheDaemonFoundAMomentaryOwner(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	previous := spawnDaemon
	spawns := 0
	spawnDaemon = func(root string) (<-chan error, error) {
		spawns++
		if spawns == 1 {
			exited := make(chan error, 1)
			exited <- nil // lost the lock to a momentary holder
			return exited, nil
		}
		runDaemonInProcess(t, root)
		return nil, nil
	}
	t.Cleanup(func() { spawnDaemon = previous })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Ensure(ctx, root)
	if err != nil {
		t.Fatalf("Ensure = %v after %d starts", err, spawns)
	}
	_ = client.Close()
	if spawns != 2 {
		t.Fatalf("%d starts, want 2", spawns)
	}
}

// TestDaemonWaitsForAMomentaryLock: a starting daemon tries again for a lock
// that another process holds only for a moment.
func TestDaemonWaitsForAMomentaryLock(t *testing.T) {
	socketTestEnv(t)
	root := t.TempDir()
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	handle, acquired, err := acquirePlatformLock(endpoint.Lock)
	if err != nil || !acquired {
		t.Fatal(acquired, err)
	}
	go func() {
		time.Sleep(40 * time.Millisecond)
		_ = releasePlatformLock(handle)
	}()
	runDaemon(t, root) // fails the test when Run returns ErrWorkspaceOwned
}

// TestCleanRuntimeDirKeepsALiveDaemonWithoutItsLockFile: a daemon whose lock
// file was removed still answers on its socket; its socket must stay.
func TestCleanRuntimeDirKeepsALiveDaemonWithoutItsLockFile(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "dxc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "0123456789abcdef0123456789abcdef.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	defer func() { _ = listener.Close() }()
	cleanRuntimeDir(dir, "", time.Now())
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("the socket of a live daemon was removed: %v", err)
	}
}

// TestOwnershipReleaseAndRelockDoNotRace: the endpoint check can lock the file
// again while Run releases the ownership at shutdown. Run with -race.
func TestOwnershipReleaseAndRelockDoNotRace(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		ownership, endpoint, err := AcquireOwnership(root)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(endpoint.Lock)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = ownership.state()
			_, _ = ownership.relock()
		}()
		_ = ownership.Release()
		<-done
		_ = ownership.Release()
	}
}

// TestRestartDoesNotOpenARuntimeWhileTheDaemonStops: a restart that waited
// for the connections while the daemon began to stop must not open a new
// runtime that nothing closes.
func TestRestartDoesNotOpenARuntimeWhileTheDaemonStops(t *testing.T) {
	quietEnv(t)
	root := t.TempDir()
	s, _ := pipeServer(t, root)
	opened := 0
	s.openRuntime = func(string) (*workspace.Runtime, error) {
		opened++
		return nil, errors.New("must not open")
	}
	s.restarts = map[string]time.Time{}
	s.stop()
	s.restartRuntime("test", "a test", nil)
	if opened != 0 {
		t.Fatal("a stopping daemon opened a new runtime")
	}
	s.runtime = nil // pipeServer's cleanup closes the runtime that it opened
}

// TestRuntimeDirEnvDoesNotChangeTheNamedDirectory: Dexter makes its own
// subdirectory private and cleans only that one, not a directory that the
// user named and may share with other programs.
func TestRuntimeDirEnvDoesNotChangeTheNamedDirectory(t *testing.T) {
	named, err := os.MkdirTemp("/tmp", "dxn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(named) })
	if err := os.Chmod(named, 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(named, "0123456789abcdef0123456789abcdef.lock")
	if err := os.WriteFile(foreign, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(RuntimeDirEnv, named)
	endpoint, err := ResolveEndpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(filepath.Dir(endpoint.Socket)) != named {
		t.Fatalf("socket %s is not in a subdirectory of %s", endpoint.Socket, named)
	}
	cleanRuntimeDirSafely(endpoint)
	info, err := os.Stat(named)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("the named directory changed: %v, %v", info.Mode(), err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("a file in the named directory was removed: %v", err)
	}
}
