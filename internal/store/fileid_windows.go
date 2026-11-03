//go:build windows

package store

import "os"

// fileInode returns 0: Windows file information has no inode, so
// SameDatabaseFile only checks that the file is there.
func fileInode(os.FileInfo) uint64 { return 0 }
