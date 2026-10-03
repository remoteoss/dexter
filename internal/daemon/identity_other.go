//go:build !darwin

package daemon

// diskSpelling returns path unchanged. Linux file systems are case-sensitive
// in normal use, and Windows identities go through the same path.
func diskSpelling(path string) string { return path }
