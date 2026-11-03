//go:build darwin

package pathcase

import (
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DiskSpelling returns path as the file system stores it. The default macOS
// file system ignores case, and filepath.EvalSymlinks keeps the case the
// caller typed, so ~/Code/app and ~/code/app would get two identities, two
// locks, and two daemons that build one index at the same time. FSEvents also
// reports paths in the stored spelling, so the watcher compares event paths
// with this spelling of the root. F_GETPATH returns the stored spelling. The
// result is used only when it differs from path in case alone, so a firmlink
// or another alias cannot move an existing workspace to a new lock.
func DiskSpelling(path string) string {
	stored, ok := storedPath(path)
	if ok && stored != path && strings.EqualFold(stored, path) {
		return stored
	}
	return path
}

const caseInsensitive = true

// storedPath returns the path of the open file as the file system stores it
// (F_GETPATH): symlinks resolved, in the stored case.
func storedPath(path string) (string, bool) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", false
	}
	defer func() { _ = unix.Close(fd) }()
	buf := make([]byte, unix.PathMax)
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(unix.F_GETPATH), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", false
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), true
}
