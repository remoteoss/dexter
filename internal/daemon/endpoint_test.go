package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireOwnershipIsExclusiveAndCrashSafeFileMayRemain(t *testing.T) {
	root := t.TempDir()
	first, endpoint, err := AcquireOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(endpoint.Lock); err != nil {
		t.Fatalf("lock file: %v", err)
	}

	second, _, err := AcquireOwnership(root)
	if !errors.Is(err, ErrWorkspaceOwned) {
		t.Fatalf("second acquire error = %v, want ErrWorkspaceOwned", err)
	}
	if second != nil {
		t.Fatal("second acquire unexpectedly returned ownership")
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}

	// The persistent file is not a stale lock: kernel ownership disappeared
	// when the descriptor closed, just as it does after a process crash.
	third, _, err := AcquireOwnership(root)
	if err != nil {
		t.Fatalf("acquire with persistent lock file: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatal(err)
	}
}

// Two spellings of one directory must resolve the same ownership endpoint: two
// writers on one index file is the failure the lock exists to prevent. The
// handshake separately rejects an alias that does not match the indexed root.
func TestResolveEndpointSharesOneDaemonAcrossSymlinks(t *testing.T) {
	physical := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(physical, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	a, err := ResolveEndpoint(physical)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolveEndpoint(link)
	if err != nil {
		t.Fatal(err)
	}
	if a.Socket != b.Socket || a.Lock != b.Lock || a.Log != b.Log {
		t.Fatalf("aliases resolved to different endpoints:\n%+v\n%+v", a, b)
	}
	if a.Identity != b.Identity {
		t.Fatalf("identity %q differs from %q", a.Identity, b.Identity)
	}
	if a.Root != physical || b.Root != link {
		t.Fatalf("indexed roots were canonicalized: %q and %q", a.Root, b.Root)
	}
}

// Ownership follows the identity, not the spelling, so an alias cannot take a
// second lock on a workspace a daemon already owns.
func TestOwnershipIsSharedAcrossSymlinks(t *testing.T) {
	physical := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(physical, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	first, _, err := AcquireOwnership(physical)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Release() }()

	second, _, err := AcquireOwnership(link)
	if !errors.Is(err, ErrWorkspaceOwned) {
		t.Fatalf("ownership through an alias = %v, want ErrWorkspaceOwned", err)
	}
	if second != nil {
		t.Fatal("alias unexpectedly acquired ownership")
	}
}

// GUI and shell sessions can carry different runtime environment variables, but
// they must still derive one ownership lock and socket.
func TestResolveEndpointDoesNotDependOnSessionEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	first, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("TMPDIR", t.TempDir())
	second, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	if first.Socket != second.Socket || first.Lock != second.Lock {
		t.Fatalf("session environments resolved different ownership paths:\n%+v\n%+v", first, second)
	}
}

// TestCheckRuntimeDirRejectsASymlink: a shared machine can pre-create the
// predictable temp name as a symlink; it must be rejected rather than trusted.
func TestCheckRuntimeDirRejectsASymlink(t *testing.T) {
	real := t.TempDir()
	if err := checkRuntimeDir(real); err != nil {
		t.Fatalf("real directory rejected: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := checkRuntimeDir(link); err == nil {
		t.Fatal("symlinked runtime directory accepted")
	}
}
