package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/remoteoss/dexter/internal/parser"
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

// containerScanDepth and containerScanDirs bound IsProjectContainer. A Mix
// project of the root itself is rarely deeper than apps/<name>/mix.exs or
// services/<group>/<name>/mix.exs; a scan that reaches the directory limit
// gives up and takes dir for a project, as before.
const (
	containerScanDepth = 4
	containerScanDirs  = 20000
)

// IsProjectContainer reports whether dir only holds other projects: it has no
// Mix project of its own (no mix.exs outside the repositories below it), and
// at least one repository below it (a clone, or a linked worktree) holds a
// Mix project. A directory that holds the clones and worktrees of several
// repositories is such a container. Each file below it belongs to the
// repository that holds it, so a frontend serves each file from that
// project, and does not index the container itself: its index would be a
// copy of the projects in it.
//
// A monorepo with Mix projects in apps/ and libs/ is not a container, also
// when worktrees are checked out inside it.
func IsProjectContainer(dir string) bool {
	projects, ok := ContainerProjects(dir)
	return ok && len(projects) > 0
}

// ContainerProjects returns the repositories below dir that hold a Mix
// project, in walk order, when dir has no Mix project of its own; ok is false
// when it has one, or when dir is too large to scan (see containerScanDirs).
func ContainerProjects(dir string) (projects []string, ok bool) {
	if regularFile(filepath.Join(dir, "mix.exs")) {
		return nil, false
	}
	visited := 0
	var walk func(d string, depth int) (own bool)
	walk = func(d string, depth int) bool {
		if visited++; visited > containerScanDirs {
			return true // too large to tell: a project, as before
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			return false
		}
		if d != dir && hasEntry(entries, ".git") {
			if holdsMixProject(d, containerScanDepth-1) {
				projects = append(projects, d)
			}
			return false // another repository: its files are not dir's
		}
		if d != dir && hasEntry(entries, "mix.exs") {
			return true
		}
		if depth >= containerScanDepth {
			return false
		}
		for _, e := range entries {
			if e.IsDir() && !skipContainerDir(e.Name()) && walk(filepath.Join(d, e.Name()), depth+1) {
				return true
			}
		}
		return false
	}
	if walk(dir, 0) {
		return nil, false
	}
	return projects, true
}

// holdsMixProject reports whether a mix.exs is in d or below it, to depth.
func holdsMixProject(d string, depth int) bool {
	entries, err := os.ReadDir(d)
	if err != nil {
		return false
	}
	if hasEntry(entries, "mix.exs") {
		return true
	}
	if depth == 0 {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && !skipContainerDir(e.Name()) && holdsMixProject(filepath.Join(d, e.Name()), depth-1) {
			return true
		}
	}
	return false
}

func hasEntry(entries []os.DirEntry, name string) bool {
	for _, e := range entries {
		if e.Name() == name {
			return true
		}
	}
	return false
}

// skipContainerDir is a directory that holds no project of the container:
// build output, dependencies, tool state, and hidden directories.
func skipContainerDir(name string) bool {
	return name == "_build" || name == "deps" || name == "node_modules" || strings.HasPrefix(name, ".")
}

// NestedProject returns the project below base that owns the files of dir, or
// "" when base owns them (or dir is not below base). Below base, a file
// belongs to another project only when a nested clone (a .git directory) or a
// linked worktree is between the file and base; the nearest one is the
// project. An index alone does not make a project (a Dexter index left in
// apps/foo of a monorepo is not one), and a submodule (a .git file that is
// not a linked worktree) stays a part of the project that holds it. A file of
// a dependency or of build output (deps, _build, node_modules) belongs to the
// project that holds that directory: a git dependency is a clone, but not a
// project of its own.
//
// The LSP proxy and the MCP frontend use it to send each file to its
// project's daemon.
func NestedProject(dir, base string) string {
	dir = filepath.Clean(dir)
	base = filepath.Clean(base)
	if !within(dir, base) {
		return ""
	}
	dir = aboveDependencies(dir, base)
	for d := dir; d != base && within(d, base); d = filepath.Dir(d) {
		if isNestedProject(d) {
			return d
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return ""
}

// isNestedProject reports whether dir is a clone (it holds a .git directory)
// or a linked worktree.
func isNestedProject(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	if info.IsDir() {
		return true
	}
	return info.Mode().IsRegular() && parser.IsLinkedWorktree(dir)
}

// aboveDependencies returns the parent of the first deps, _build, or
// node_modules directory of dir below base, or dir when it has none.
func aboveDependencies(dir, base string) string {
	rel, err := filepath.Rel(base, dir)
	if err != nil || rel == "." {
		return dir
	}
	at := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "deps" || part == "_build" || part == "node_modules" {
			return at
		}
		at = filepath.Join(at, part)
	}
	return dir
}

// within reports whether path is base or below it.
func within(path, base string) bool {
	if path == base {
		return true
	}
	if !strings.HasPrefix(path, base) {
		return false
	}
	return strings.HasSuffix(base, string(filepath.Separator)) || path[len(base)] == filepath.Separator
}
