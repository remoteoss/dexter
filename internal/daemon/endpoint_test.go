package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// Two spellings of one directory must reach one daemon: two writers on a single
// index file is the failure the ownership lock exists to prevent. But the root
// the daemon indexes keeps the caller's spelling, because stored paths are
// matched against the URIs an editor sends, and canonicalizing them would break
// every path-keyed lookup for a project reached through a symlink.
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

// TestResolveEndpointFallsBackWhenTheFirstCandidateIsUnusable: a broken
// XDG_RUNTIME_DIR must not take dexter down; the next stable candidate is used.
func TestResolveEndpointFallsBackWhenTheFirstCandidateIsUnusable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", blocker)

	endpoint, err := ResolveEndpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(endpoint.Socket, blocker) {
		t.Fatalf("socket %s used the unusable XDG candidate", endpoint.Socket)
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
