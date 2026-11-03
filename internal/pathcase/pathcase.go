// Package pathcase finds the spelling that a case-insensitive file system
// stores for a path. The default macOS file system ignores case: a stat of
// lib/Area succeeds after `mv lib/Area lib/area`, so a path that the index
// keeps in the old spelling looks present, and the event paths of FSEvents
// use the stored spelling, not the one that the caller typed.
package pathcase

import (
	"path/filepath"
	"strings"
)

// SpelledBelow reports whether the components of path below root have the
// spelling that the file system stores. It is false only when the stored
// spelling is known and differs in case: a path that cannot be opened, that is
// not below root, or whose stored path cannot be compared (a symlink to
// another place) counts as spelled right. root itself is not checked; it keeps
// the spelling that the caller typed. On a case-sensitive file system it is
// always true and costs nothing. On macOS it costs one open and one fcntl, so
// callers use it only for paths that are already suspect.
func SpelledBelow(root, path string) bool {
	if !caseInsensitive {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	stored, ok := storedPath(path)
	if !ok || len(stored) <= len(rel) {
		return true
	}
	tail := stored[len(stored)-len(rel):]
	if stored[len(stored)-len(rel)-1] != filepath.Separator || tail == rel {
		return true
	}
	return !strings.EqualFold(tail, rel)
}
