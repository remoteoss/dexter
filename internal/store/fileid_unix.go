//go:build !windows

package store

import (
	"os"
	"syscall"
)

// fileInode returns the inode of info, or 0 when it has none.
func fileInode(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino) //nolint:unconvert // Ino is uint32 on some systems
	}
	return 0
}
