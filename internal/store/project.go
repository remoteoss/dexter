package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// LooksLikeProject reports whether dir shows the cheap signs of a Dexter
// workspace: a mix.exs, a .git, or a Dexter database that a user chose to make
// there (ChosenRoot). They match what FindProjectRoot trusts. A database
// alone is not a sign: any frontend that was started in a directory makes
// one, and a database that a frontend made in the wrong directory (a parent
// of many projects) would then make that directory the workspace of every
// later session below it.
func LooksLikeProject(dir string) bool {
	return regularFile(filepath.Join(dir, "mix.exs")) ||
		gitMarker(filepath.Join(dir, ".git")) ||
		(HasIndex(dir) && ChosenRoot(dir))
}

// chosenRootFile, in the .dexter directory, records that the user chose to
// index a directory that does not look like a project (`dexter init -y`).
const chosenRootFile = "chosen-root"

// ChosenRoot reports whether the user chose dir as a workspace although it
// does not look like a project. See MarkChosenRoot.
func ChosenRoot(dir string) bool {
	return regularFile(filepath.Join(DBDir(dir), chosenRootFile))
}

// MarkChosenRoot records that the user chose dir as a workspace (-y/--yes),
// so that later sessions there, and in directories below it, use it without
// -y.
func MarkChosenRoot(dir string) error {
	if err := os.MkdirAll(DBDir(dir), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(DBDir(dir), chosenRootFile), []byte("This directory was chosen as a Dexter workspace with -y/--yes.\n"), 0o644)
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

// NonProjectRootError explains why dir is not a workspace to index, or returns
// nil when it is one. Every frontend refuses such a directory unless the user
// insists, because indexing it (the home directory, a mistyped path) would
// read every file under it.
func NonProjectRootError(dir string) error {
	if IsHomeDir(dir) {
		if HasIndex(dir) && ChosenRoot(dir) {
			return nil
		}
		return fmt.Errorf("refusing to use %s as a workspace: it is your home directory, not a project", dir)
	}
	if LooksLikeProject(dir) {
		return nil
	}
	return fmt.Errorf("refusing to use %s as a workspace: no mix.exs or .git found, so it does not look like an Elixir project", dir)
}
