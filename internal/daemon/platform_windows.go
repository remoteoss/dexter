// This file is not exercised: the repository's cgo dependencies
// (mattn/go-sqlite3) and the tree-sitter-elixir Go binding do not build for
// GOOS=windows, and the release matrix is Linux and Darwin only. It exists so
// workspace ownership is already correct if Windows support lands, and it
// mirrors the Unix flock semantics rather than inventing a PID-file scheme.

//go:build windows

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

type lockHandle struct {
	file *os.File
}

// platformRuntimeDirs lists where one workspace's runtime files may live.
// Windows has a per-user cache directory already, so there is one candidate.
func platformRuntimeDirs() []string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(dir, "dexter", "run")}
}

// checkRuntimeDir rejects a candidate that is not a real directory. The cache
// directory is already per-user, so ownership is not in question here.
func checkRuntimeDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s exists but is not a directory", dir)
	}
	return nil
}

// acquirePlatformLock takes the same kind of kernel-released advisory lock the
// Unix path uses: LockFileEx over one byte of the lock file, held for the
// process lifetime. Windows drops it when the process exits for any reason, so
// a crash cannot leave a workspace permanently locked and file existence never
// implies ownership.
func acquirePlatformLock(path string) (*lockHandle, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &lockHandle{file: f}, true, nil
}

func releasePlatformLock(handle *lockHandle) error {
	ol := new(windows.Overlapped)
	err := windows.UnlockFileEx(windows.Handle(handle.file.Fd()), 0, 1, 0, ol)
	return errors.Join(err, handle.file.Close())
}

func configureDetachedProcess(attr **syscall.SysProcAttr) {
	*attr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}

// findDaemonProcess is not implemented on Windows: the platform is best-effort,
// and the pid in a refusal or the idle timeout covers this case.
func findDaemonProcess(string) (int, bool) {
	return 0, false
}

// processAlive reports whether pid names a live process.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == windows.STILL_ACTIVE
}

// killPlatformProcess is the escalation for a process that ignores a terminate
// request. Windows has no softer signal, so this is the same call.
func killPlatformProcess(pid int) error {
	return terminatePlatformProcess(pid)
}

// terminatePlatformProcess ends a process this build cannot talk to. Windows
// has no graceful signal to a detached process, so this is a hard terminate.
// A pid that is already gone is success, not an error.
func terminatePlatformProcess(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}
