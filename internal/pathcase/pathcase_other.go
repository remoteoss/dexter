//go:build !darwin

package pathcase

const caseInsensitive = false

// DiskSpelling returns path unchanged. Linux file systems are case-sensitive
// in normal use, and Windows identities go through the same path.
func DiskSpelling(path string) string { return path }

func storedPath(string) (string, bool) { return "", false }
