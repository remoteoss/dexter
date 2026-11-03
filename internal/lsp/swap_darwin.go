//go:build darwin

package lsp

import "golang.org/x/sys/unix"

// exchangeFiles swaps the files at a and b in one step (renamex_np with
// RENAME_SWAP). See swapError for the errors.
func exchangeFiles(a, b string) error {
	return swapError(unix.RenamexNp(a, b, unix.RENAME_SWAP))
}
