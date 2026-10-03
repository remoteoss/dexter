package store

import (
	"os"
	"path/filepath"
)

// LooksLikeProject reports whether dir shows the cheap signs of a Dexter
// workspace: a mix.exs, a .git, or a Dexter database. They match what
// FindProjectRoot trusts, and a Dexter marker means an actual database, not an
// empty directory left by an interrupted operation.
func LooksLikeProject(dir string) bool {
	return regularFile(filepath.Join(dir, "mix.exs")) ||
		gitMarker(filepath.Join(dir, ".git")) ||
		HasIndex(dir)
}

// HasIndex reports whether dir holds a Dexter database.
func HasIndex(dir string) bool {
	return regularFile(DBPath(dir)) || regularFile(LegacyDBPath(dir))
}

// IsHomeDir reports whether dir is the user's home directory.
func IsHomeDir(dir string) bool {
	home, err := os.UserHomeDir()
	return err == nil && SameDir(dir, home)
}

// SameDir reports whether two paths name the same directory. Stat is the
// authority so a symlinked spelling (or a case-insensitive filesystem) cannot
// sneak a home directory past a check.
func SameDir(a, b string) bool {
	ai, aErr := os.Stat(a)
	bi, bErr := os.Stat(b)
	if aErr != nil || bErr != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return os.SameFile(ai, bi)
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func gitMarker(path string) bool {
	info, err := os.Stat(path)
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}
