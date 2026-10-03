//go:build darwin

package daemon

import (
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// diskSpelling returns path as the file system stores it. The default macOS
// file system ignores case, and filepath.EvalSymlinks keeps the case the
// caller typed, so ~/Code/app and ~/code/app would get two identities, two
// locks, and two daemons that build one index at the same time. F_GETPATH
// returns the stored spelling. The result is used only when it differs from
// path in case alone, so a firmlink or another alias cannot move an existing
// workspace to a new lock.
func diskSpelling(path string) string {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return path
	}
	defer func() { _ = unix.Close(fd) }()
	buf := make([]byte, unix.PathMax)
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(unix.F_GETPATH), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return path
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	stored := string(buf[:n])
	if stored != path && strings.EqualFold(stored, path) {
		return stored
	}
	return path
}
