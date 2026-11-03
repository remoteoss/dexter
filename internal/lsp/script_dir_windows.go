//go:build windows

package lsp

import (
	"fmt"
	"os"
)

// checkPrivateDir refuses a directory that is a symlink or not a directory.
// The temp directory is per user on Windows.
func checkPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}

// privateFile reports whether nobody else can write a file. The temp and
// cache directories are per user on Windows.
func privateFile(info os.FileInfo) bool { return info.Mode().Perm()&0o022 == 0 }
