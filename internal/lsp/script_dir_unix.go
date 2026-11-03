//go:build !windows

package lsp

import (
	"fmt"
	"os"
	"syscall"
)

// checkPrivateDir refuses a directory that is a symlink, is not a directory,
// or belongs to another user: it holds scripts that this user runs.
func checkPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to uid %d, not %d", dir, stat.Uid, os.Getuid())
	}
	if info.Mode().Perm()&0o022 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// privateFile reports whether a file belongs to this user and nobody else can
// write it.
func privateFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid() && info.Mode().Perm()&0o022 == 0
}
