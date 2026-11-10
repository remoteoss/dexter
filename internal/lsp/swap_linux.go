//go:build linux

package lsp

import "golang.org/x/sys/unix"

// exchangeFiles swaps the files at a and b in one step (renameat2 with
// RENAME_EXCHANGE). See swapError for the errors.
func exchangeFiles(a, b string) error {
	return swapError(unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE))
}
