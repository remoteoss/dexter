//go:build !unix

package lsp

import (
	"io/fs"
	"os"
)

// checkWritable reports why the process cannot write path, or nil when it
// can. Windows has no access(2); the read-only attribute shows as a missing
// owner write bit.
func checkWritable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o200 == 0 {
		return &os.PathError{Op: "write", Path: path, Err: fs.ErrPermission}
	}
	return nil
}

func syncFile(f *os.File) error { return f.Sync() }

func keepOwner(string, os.FileInfo) {}

// inodeOf is 0 where the system gives no inode.
func inodeOf(os.FileInfo) uint64 { return 0 }
