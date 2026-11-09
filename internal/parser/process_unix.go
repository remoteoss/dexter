//go:build unix

package parser

import (
	"errors"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// processRunning reports whether a process with pid exists. A process that
// another user owns exists too (EPERM).
func processRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// changedAt returns the time of the last change of path's inode, which a
// rename sets. A rename backup is the target renamed, so its modification
// time is the target's, which can be years old.
func changedAt(path string, info os.FileInfo) time.Time {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return info.ModTime()
	}
	return time.Unix(st.Ctim.Unix())
}
