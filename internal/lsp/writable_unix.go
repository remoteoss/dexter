//go:build unix

package lsp

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// checkWritable reports why the process cannot write path, or nil when it
// can. access(2) asks the kernel without opening the file: opening a file for
// writing costs measurably more on macOS, and a rename can touch hundreds.
func checkWritable(path string) error {
	if err := unix.Access(path, unix.W_OK); err != nil {
		return &os.PathError{Op: "write", Path: path, Err: err}
	}
	return nil
}

// syncFile flushes f to the device. On macOS, os.File.Sync uses F_FULLFSYNC,
// which also empties the drive cache and costs milliseconds for each file; a
// rename writes hundreds of files. fsync(2) orders the data before the rename
// that publishes it, which is what the atomic replace needs.
func syncFile(f *os.File) error { return unix.Fsync(int(f.Fd())) }

// keepOwner gives path the owner and group of info, as far as the process may.
func keepOwner(path string, info os.FileInfo) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if int(st.Uid) == os.Geteuid() && int(st.Gid) == os.Getegid() {
			return // the new file has them already
		}
		_ = os.Lchown(path, int(st.Uid), int(st.Gid))
	}
}

// inodeOf is the inode of info, or 0.
func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
