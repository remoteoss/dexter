//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type lockHandle struct {
	file *os.File
}

// platformRuntimeDirs returns the runtime directory candidates. Every process
// for one user must resolve the same lock and socket or a GUI editor and a shell
// can each start a daemon over the same index, so no candidate depends on the
// environment: /tmp/dexter-<uid> first, and a directory under the home directory
// that the user database (not $HOME) names for the case where another user
// created /tmp/dexter-<uid> first.
func platformRuntimeDirs() []string {
	dirs := []string{filepath.Join("/tmp", fmt.Sprintf("dexter-%d", os.Getuid()))}
	if home := passwdHome(); home != "" {
		dirs = append(dirs, homeRuntimeDir(home))
	}
	return dirs
}

var (
	passwdHomeOnce  sync.Once
	passwdHomeValue string
)

// passwdHome is the home directory that the user database records for this
// uid. It is read once per process: the lookup can ask a directory service.
func passwdHome() string {
	passwdHomeOnce.Do(func() {
		if u, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil && filepath.IsAbs(u.HomeDir) {
			passwdHomeValue = u.HomeDir
		}
	})
	return passwdHomeValue
}

// homeRuntimeDir is the per-user cache location of the runtime files.
func homeRuntimeDir(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches", "dexter", "run")
	}
	return filepath.Join(home, ".cache", "dexter", "run")
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

// acquirePlatformLock locks the file at path. After the lock is taken it checks
// that path still names the locked file: a process that removed a stale lock
// file between this open and this flock would otherwise leave this process
// holding a lock that nobody else can see.
func acquirePlatformLock(path string) (*lockHandle, bool, error) {
	for attempt := 0; ; attempt++ {
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
		held, heldErr := f.Stat()
		current, currentErr := os.Stat(path)
		if heldErr == nil && currentErr == nil && os.SameFile(held, current) {
			return &lockHandle{file: f}, true, nil
		}
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
		if attempt >= 3 {
			return nil, false, fmt.Errorf("lock file %s keeps changing", path)
		}
	}
}

func releasePlatformLock(handle *lockHandle) error {
	err := unix.Flock(int(handle.file.Fd()), unix.LOCK_UN)
	return errors.Join(err, handle.file.Close())
}

func configureDetachedProcess(attr **syscall.SysProcAttr) {
	*attr = &syscall.SysProcAttr{Setsid: true}
}

// findDaemonProcesses locates the workspace daemons by command line. It is the
// fallback for a daemon whose refusal predates the pid field, and the way
// `dexter stop --force` finds every daemon of a root: `daemon <root>` is a
// specific enough match to identify them safely.
func findDaemonProcesses(root string) []int {
	out, err := exec.Command("ps", "-A", "-o", "uid=,pid=,args=").Output()
	if err != nil {
		return nil
	}
	return parseDaemonPids(string(out), root)
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
