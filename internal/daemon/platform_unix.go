//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

type lockHandle struct {
	file *os.File
}

// platformRuntimeDirs lists where one workspace's runtime files may live, best
// first. The order is about stability as much as privacy: every process of one
// user has to resolve the same directory, or two daemons could hold two locks
// over one index. XDG_RUNTIME_DIR is a per-session value set by the system, and
// /tmp/dexter-<uid> is environment-independent; $TMPDIR comes last because it
// is per process, so preferring it would let `TMPDIR=x dexter lookup` split the
// lock from the editor's daemon. It is still the last-resort fallback for the
// shared-machine case where another user pre-created the /tmp name.
func platformRuntimeDirs() []string {
	var dirs []string
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		dirs = append(dirs, filepath.Join(dir, "dexter"))
	}
	uid := os.Getuid()
	dirs = append(dirs, filepath.Join("/tmp", fmt.Sprintf("dexter-%d", uid)))
	if tmp := os.TempDir(); tmp != "" {
		dirs = append(dirs, filepath.Join(tmp, fmt.Sprintf("dexter-%d", uid)))
	}
	return dirs
}

// checkRuntimeDir rejects a candidate that is not a real directory owned by
// this user. On a shared machine another user can pre-create the predictable
// /tmp name; trusting it would either deny the victim a runtime directory or
// let the attacker redirect the socket through a symlink.
func checkRuntimeDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s exists but is not a directory", dir)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by uid %d, not %d", dir, stat.Uid, os.Getuid())
	}
	return nil
}

func acquirePlatformLock(path string) (*lockHandle, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &lockHandle{file: f}, true, nil
}

func releasePlatformLock(handle *lockHandle) error {
	err := unix.Flock(int(handle.file.Fd()), unix.LOCK_UN)
	return errors.Join(err, handle.file.Close())
}

func configureDetachedProcess(attr **syscall.SysProcAttr) {
	*attr = &syscall.SysProcAttr{Setsid: true}
}

// findDaemonProcess locates a workspace daemon by command line. It is the
// fallback for a daemon whose refusal predates the pid field: the workspace is
// known to be owned (that is why the caller is here), and `daemon <root>` is a
// specific enough match to identify it safely.
func findDaemonProcess(root string) (int, bool) {
	out, err := exec.Command("ps", "-A", "-o", "uid=,pid=,args=").Output()
	if err != nil {
		return 0, false
	}
	return parseDaemonPid(string(out), root)
}

// processAlive reports whether pid names a live process. A pid this user may
// not signal still counts as alive; only ESRCH means gone.
func processAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// killPlatformProcess is the escalation for a daemon that ignores SIGTERM. The
// index is a derived cache, so an uncatchable kill is safe: SQLite recovers its
// WAL on the next open, and the next daemon rebuilds anything a torn write lost.
func killPlatformProcess(pid int) error {
	err := unix.Kill(pid, unix.SIGKILL)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// terminatePlatformProcess asks a process to exit. The daemon installs a
// SIGTERM handler, so this is its graceful path: connections close, accepted
// mutations drain, and the store checkpoints before the lock is released. A pid
// that is already gone is success, not an error.
func terminatePlatformProcess(pid int) error {
	err := unix.Kill(pid, unix.SIGTERM)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}
