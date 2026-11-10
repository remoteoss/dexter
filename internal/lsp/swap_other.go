//go:build !darwin && !linux

package lsp

// exchangeFiles has no atomic swap to use on this system.
func exchangeFiles(string, string) error { return errSwapUnsupported }
