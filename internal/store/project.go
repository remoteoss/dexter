package store

import (
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/pathcase"
)

// The project root rule. Every frontend (the CLI, the LSP proxy and server,
// MCP) finds the workspace of a path with ResolveRoot, so one directory
// always gets one daemon and one index:
//
//  1. The repository of a path is the nearest directory above it that is a
//     clone (it holds a .git directory) or a linked worktree. A submodule (a
//     .git file that is not a linked worktree) belongs to the repository
//     that holds it, and a file in deps, _build, or node_modules belongs to
//     the project that holds that directory. The home directory is never a
//     repository: a dotfiles repository there is not a project.
//  2. A repository with a Mix project of its own (a mix.exs at its root, or
//     below it but not in a nested repository) is one project: a normal
//     project, an umbrella, or a monorepo. A repository that only holds
//     other repositories with Mix projects is a container: each repository
//     in it is a project, and the container has no index of its own.
//  3. With no repository, the outermost mix.exs below the home directory is
//     the project (an umbrella that is not in git).
//
// Before step 1, at each level, an index next to a mix.exs is a project: a
// user made it there on purpose (`dexter init` in apps/my_app of a
// monorepo), and the nearest one wins. An index alone does not make a
// project: one that a frontend made in the wrong directory (a parent of many
// projects) would otherwise make that directory the workspace of every later
// session below it. An index in a directory that the rule does not resolve
// to itself is never used (see StrayIndexesAbove).

// RootKind is what ResolveRoot found.
type RootKind int

const (
	// NotAProject: no repository and no mix.exs. Root.Dir is the start
	// directory; a frontend uses it only when the user insists (-y).
	NotAProject RootKind = iota
	// OneProject: Root.Dir is a project with one index.
	OneProject
	// Container: Root.Dir only holds other projects (see ContainerProjects).
	Container
)

// Root is the workspace of a path.
type Root struct {
	Dir  string
	Kind RootKind
}

// ResolveRoot returns the workspace of path, a file or a directory. See the
// project root rule above.
func ResolveRoot(path string) Root {
	dir := filepath.Clean(path)
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		dir = filepath.Dir(dir)
	}
	dir = ownerOfDependencies(dir)
	home := homeDir()
	// One Lstat of .git at each level. Only a walk that finds no repository
	// (rare) looks for a mix.exs.
	var gitFile string
	for d := dir; !home.is(d); {
		// An index that a user made in a Mix project (`dexter init` in
		// apps/my_app of a monorepo) is that project's: the nearest one wins.
		// An index without a mix.exs next to it makes no project.
		if regularFile(DBPath(d)) && regularFile(filepath.Join(d, "mix.exs")) {
			return Root{Dir: d, Kind: OneProject}
		}
		if info, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			if info.IsDir() || (info.Mode().IsRegular() && parser.IsLinkedWorktree(d)) {
				return classifyRepo(d)
			}
			if gitFile == "" && info.Mode().IsRegular() {
				gitFile = d // a submodule outside a clone, or a worktree whose admin directory is gone
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if gitFile != "" {
		// A .git file that is not a linked worktree: a submodule outside its
		// clone, or the folder of a bare repository and its worktrees
		// (`.git` holds `gitdir: ./.bare`), which can be a container.
		return classifyRepo(gitFile)
	}
	var mixDir string
	for d := dir; !home.is(d); {
		if regularFile(filepath.Join(d, "mix.exs")) {
			mixDir = d
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	if mixDir != "" {
		return Root{Dir: mixDir, Kind: OneProject}
	}
	// A folder that is not in git but holds clones (~/code): a container,
	// as the proxy sees it. The home directory is never one.
	if !home.is(dir) && IsProjectContainer(dir) {
		return Root{Dir: dir, Kind: Container}
	}
	return Root{Dir: dir, Kind: NotAProject}
}

// classifyRepo is step 2 of the project root rule for the repository dir.
func classifyRepo(dir string) Root {
	if regularFile(filepath.Join(dir, "mix.exs")) {
		return Root{Dir: dir, Kind: OneProject}
	}
	if scan := scanRepo(dir); scan.container {
		return Root{Dir: dir, Kind: Container}
	}
	return Root{Dir: dir, Kind: OneProject} // a monorepo, or a repository without Elixir
}

// FindProjectRoot returns the directory of ResolveRoot: the project root of
// path, the container that holds it, or path itself when it is in no
// project.
func FindProjectRoot(path string) string {
	return ResolveRoot(path).Dir
}

// homeDirs is the home directory as written and with its symlinks resolved.
type homeDirs []string

// homeCache holds homeDir for one value of the home directory, so that a
// walk does not resolve its symlinks each time.
var homeCache struct {
	sync.Mutex
	home string
	dirs homeDirs
}

func homeDir() homeDirs {
	home := userHome()
	if home == "" {
		return nil
	}
	homeCache.Lock()
	defer homeCache.Unlock()
	if homeCache.home == home && homeCache.dirs != nil {
		return homeCache.dirs
	}
	h := homeDirs{filepath.Clean(home)}
	if resolved, err := filepath.EvalSymlinks(home); err == nil && resolved != h[0] {
		h = append(h, resolved)
	}
	homeCache.home, homeCache.dirs = home, h
	return h
}

// is reports whether d is the home directory. A string compare without case
// finds the candidates, so a walk pays a stat only at a directory that can be
// the home directory in another spelling.
func (h homeDirs) is(d string) bool {
	for _, home := range h {
		if d == home {
			return true
		}
		if strings.EqualFold(d, home) && SameDir(d, home) {
			return true
		}
	}
	return false
}

// ownerOfDependencies returns the project directory that holds dir when dir is
// in a dependency or build directory of it (deps, _build, or node_modules next
// to a mix.exs, a package.json, or a .git), or dir. The outermost one wins: a
// dependency's own deps belong to the same project.
func ownerOfDependencies(dir string) string {
	owner := dir
	for d := dir; ; {
		parent := filepath.Dir(d)
		if parent == d {
			return owner
		}
		if isDependencyDir(d) {
			owner = parent
		}
		d = parent
	}
}

func isDependencyDir(d string) bool {
	switch filepath.Base(d) {
	case "deps", "_build", "node_modules":
		parent := filepath.Dir(d)
		return regularFile(filepath.Join(parent, "mix.exs")) ||
			regularFile(filepath.Join(parent, "package.json")) ||
			gitMarker(filepath.Join(parent, ".git"))
	}
	return false
}

// looksLikeProject reports whether dir shows the cheap signs of a workspace
// root: a mix.exs or a .git. A Dexter database alone is not a sign (see the
// project root rule).
func looksLikeProject(dir string) bool {
	return regularFile(filepath.Join(dir, "mix.exs")) || gitMarker(filepath.Join(dir, ".git"))
}

// HasIndex reports whether dir holds a Dexter database.
func HasIndex(dir string) bool {
	return regularFile(DBPath(dir)) || regularFile(LegacyDBPath(dir))
}

// StrayIndex is a Dexter index that the project root rule does not use.
type StrayIndex struct {
	Dir   string // the directory that holds the .dexter directory
	Bytes int64  // the size of the database and its journal files
}

// StrayIndexesAbove lists the Dexter indexes in the directories above root,
// up to and including the home directory, that no session uses: the rule
// does not resolve their directory to itself (see servesItself). Earlier
// versions of Dexter made such indexes in parents of several projects, where
// they can be very large.
// Only the .dexter layout counts: Open moved the old .dexter.db layout long
// ago.
func StrayIndexesAbove(root string) []StrayIndex {
	home := homeDir()
	var stray []StrayIndex
	for d := filepath.Clean(root); ; {
		if home.is(d) {
			return stray
		}
		parent := filepath.Dir(d)
		if parent == d {
			return stray
		}
		d = parent
		if s, ok := StrayIndexAt(d); ok && !servesItself(d) {
			stray = append(stray, s)
		}
	}
}

// servesItself reports whether a session that starts in dir uses an index
// in dir: the rule resolves dir to itself, as one project. The index of a
// monorepo above one of its apps, or of a monorepo above a clone in it, is
// in use; the index of a container, or of a folder that is not a project,
// is not.
func servesItself(dir string) bool {
	r := ResolveRoot(dir)
	return r.Kind == OneProject && r.Dir == dir
}

// StrayIndexAt returns the index of dir as a StrayIndex, when it has one. A
// container's own index is one: the rule gives a container no index.
func StrayIndexAt(dir string) (StrayIndex, bool) {
	if !regularFile(DBPath(dir)) {
		return StrayIndex{}, false
	}
	return StrayIndex{Dir: dir, Bytes: indexBytes(dir)}, true
}

// indexFiles are the files that Dexter keeps in its .dexter directory.
var indexFiles = []string{"dexter.db", "dexter.db-wal", "dexter.db-shm", ".gitignore", "chosen-root"}

func indexBytes(dir string) int64 {
	var n int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(DBPath(dir) + suffix); err == nil {
			n += info.Size()
		}
	}
	return n
}

// RemoveIndex deletes the Dexter index of dir: each file that Dexter keeps in
// .dexter, one at a time, and then the directory when it is empty. It never
// deletes a file that it does not know, so a .dexter that holds other files
// stays, and the error names them. The caller holds the workspace lock of
// dir, so that no daemon opens the index while it goes.
func RemoveIndex(dir string) error {
	dbDir := DBDir(dir)
	for _, name := range indexFiles {
		if err := os.Remove(filepath.Join(dbDir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(dbDir); err != nil && !os.IsNotExist(err) {
		entries, readErr := os.ReadDir(dbDir)
		if readErr != nil || len(entries) == 0 {
			return err
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return &OtherFilesError{Dir: dbDir, Names: names}
	}
	return nil
}

// OtherFilesError is RemoveIndex's answer when it deleted the index, but the
// .dexter directory also holds files that Dexter did not make.
type OtherFilesError struct {
	Dir   string
	Names []string
}

func (e *OtherFilesError) Error() string {
	return fmt.Sprintf("deleted the index, but %s also holds %s, which Dexter did not make, so the directory stays", e.Dir, strings.Join(e.Names, ", "))
}

// StrayIndexMessage explains the stray indexes above a workspace and how to
// delete them, or is "" when there are none.
func StrayIndexMessage(stray []StrayIndex) string {
	if len(stray) == 0 {
		return ""
	}
	var where []string
	for _, s := range stray {
		where = append(where, fmt.Sprintf("%s (%s)", DBDir(s.Dir), formatBytes(s.Bytes)))
	}
	if len(stray) == 1 {
		return fmt.Sprintf("Dexter: an earlier version of Dexter made an index at %s. No session uses it now: Dexter makes an index only at a project root. To free the space, run `dexter prune --delete` in this project.", where[0])
	}
	return fmt.Sprintf("Dexter: an earlier version of Dexter made indexes at %s. No session uses them now: Dexter makes an index only at a project root. To free the space, run `dexter prune --delete` in this project.", strings.Join(where, ", "))
}

// FormatBytes writes a size for a person: KB, MB, or GB.
func FormatBytes(n int64) string { return formatBytes(n) }

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MB", n>>20)
	default:
		return fmt.Sprintf("%d KB", n>>10)
	}
}

// IsHomeDir reports whether dir is the user's home directory.
func IsHomeDir(dir string) bool {
	for _, home := range homeDir() {
		if SameDir(dir, home) {
			return true
		}
	}
	return false
}

// userHome is the home directory: $HOME, or the user database when HOME is
// unset or not absolute (a service, a sandbox), so that the walk still stops
// there.
func userHome() string {
	if home, err := os.UserHomeDir(); err == nil && filepath.IsAbs(home) {
		return home
	}
	if u, err := user.Current(); err == nil && filepath.IsAbs(u.HomeDir) {
		return u.HomeDir
	}
	return ""
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
// insists (-y, which applies to that command only), because indexing it (the
// home directory, a mistyped path) would read every file under it. The error
// is a *NotProjectError.
func NonProjectRootError(dir string) error {
	if IsHomeDir(dir) {
		return &NotProjectError{Dir: dir, Home: true}
	}
	if looksLikeProject(dir) || IsProjectContainer(dir) {
		// A container is a valid start: the CLI refuses it with the list of
		// its projects (ContainerRootError), MCP and the proxy serve each
		// project in it.
		return nil
	}
	return &NotProjectError{Dir: dir}
}

// NotProjectError is NonProjectRootError's answer: dir is the home directory,
// or has no mix.exs or .git.
type NotProjectError struct {
	Dir  string
	Home bool
}

func (e *NotProjectError) Error() string {
	if e.Home {
		return fmt.Sprintf("refusing to use %s as a workspace: it is your home directory, not a project", e.Dir)
	}
	return fmt.Sprintf("refusing to use %s as a workspace: no mix.exs or .git found, so it does not look like an Elixir project", e.Dir)
}

// ContainerRootError explains that dir only holds other projects, or returns
// nil. A command that indexes one workspace refuses a container, because its
// index would be a copy of every project in it.
func ContainerRootError(dir string) error {
	projects, ok := ContainerProjects(dir)
	if !ok || len(projects) == 0 {
		return nil
	}
	example := projects[0]
	if rel, err := filepath.Rel(dir, example); err == nil {
		example = rel
	}
	return fmt.Errorf("refusing to use %s as a workspace: it holds %d projects (clones and worktrees of other repositories), not one. Run Dexter in the project you work in, for example %s", dir, len(projects), example)
}

// containerScanDepth and containerScanDirs bound the scan of a repository
// that has no mix.exs at its root. The Mix projects of a monorepo itself are
// rarely deeper than apps/<name>/mix.exs or services/<group>/<name>/mix.exs.
// A scan that reaches the directory limit takes the repository for one
// project, as before the container rule.
const (
	containerScanDepth = 4
	containerScanDirs  = 20000
)

// repoScan is what scanRepo found in a repository.
type repoScan struct {
	own       bool     // a Mix project of its own, or too large to tell
	nested    []string // the nested repositories, in breadth-first order
	container bool     // !own, and a nested repository holds a Mix project
}

// scanCacheTTL bounds how long a scan result is used again. A repository
// seldom changes between a monorepo and a container, but a long-lived daemon
// must see it when it does.
const scanCacheTTL = 30 * time.Second

var scanCache = struct {
	sync.Mutex
	entries map[string]scanCacheEntry
}{entries: make(map[string]scanCacheEntry)}

type scanCacheEntry struct {
	scan repoScan
	at   time.Time
}

// scanRepo reads the directories of dir breadth first, to containerScanDepth.
// It stops at the first mix.exs that is not in a nested repository (a
// monorepo), and does not go into a nested repository (a clone or a linked
// worktree), dependency or build output, or hidden directory. The cost for a
// container is then the count of its own directories, not the size of the
// repositories in it.
func scanRepo(dir string) repoScan {
	scanCache.Lock()
	if e, ok := scanCache.entries[dir]; ok && time.Since(e.at) < scanCacheTTL {
		scanCache.Unlock()
		return e.scan
	}
	scanCache.Unlock()

	scan := scanRepoUncached(dir)
	if !scan.own {
		for _, p := range scan.nested {
			if holdsMixProject(p, containerScanDepth-1) {
				scan.container = true
				break
			}
		}
	}

	scanCache.Lock()
	scanCache.entries[dir] = scanCacheEntry{scan: scan, at: time.Now()}
	scanCache.Unlock()
	return scan
}

func scanRepoUncached(dir string) repoScan {
	type item struct {
		dir   string
		depth int
	}
	var scan repoScan
	queue := []item{{dir, 0}}
	visited := 0
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if visited++; visited > containerScanDirs {
			log.Printf("Project root: %s has more than %d directories to scan; Dexter takes it for one project", dir, containerScanDirs)
			scan.own = true
			return scan
		}
		if it.dir != dir && isNestedProject(it.dir) {
			scan.nested = append(scan.nested, it.dir)
			continue
		}
		entries, err := os.ReadDir(it.dir)
		if err != nil {
			continue
		}
		if it.dir != dir && hasEntry(entries, "mix.exs") {
			scan.own = true
			return scan
		}
		if it.depth >= containerScanDepth {
			continue
		}
		for _, e := range entries {
			if skipContainerDir(e.Name()) {
				continue
			}
			path := filepath.Join(it.dir, e.Name())
			switch {
			case e.Type()&os.ModeSymlink != 0:
				// A clone that is a symlink is a repository of the container,
				// unless it points back into the container. The scan does not
				// go into other symlinks, so a link loop cannot hold it. This
				// case is first: on Windows a directory symlink is also a
				// directory.
				if isNestedProject(path) && !linksInto(path, dir) {
					scan.nested = append(scan.nested, path)
				}
			case e.IsDir():
				queue = append(queue, item{path, it.depth + 1})
			}
		}
	}
	return scan
}

// linksInto reports whether the symlink path points to dir or into it.
func linksInto(path, dir string) bool {
	target := canonicalDir(path)
	if target == "" {
		return true
	}
	if resolved := canonicalDir(dir); resolved != "" {
		dir = resolved
	}
	return within(target, dir)
}

// CanonicalDir is dir with its symlinks resolved and in the letter case that
// the disk stores, or "" when it does not exist. Two spellings of one
// directory (a symlink, /tmp and /private/tmp, other letter case) give the
// same result.
func CanonicalDir(dir string) string { return canonicalDir(dir) }

func canonicalDir(dir string) string {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	return pathcase.DiskSpelling(resolved)
}

// IsProjectContainer reports whether dir only holds other projects: it has no
// Mix project of its own, and at least one repository below it (a clone, or a
// linked worktree) holds a Mix project. A directory that holds the clones and
// worktrees of several repositories is such a container. Each file below it
// belongs to the repository that holds it, so a frontend serves each file
// from that project, and does not index the container itself: its index
// would be a copy of the projects in it.
//
// A monorepo with Mix projects in apps/ and libs/ is not a container, also
// when worktrees are checked out inside it.
func IsProjectContainer(dir string) bool {
	if regularFile(filepath.Join(dir, "mix.exs")) {
		return false
	}
	return scanRepo(dir).container
}

// LinkedRepositories returns the nested repositories of the container dir
// that are symlinks, with the directory that each one points to. A file in
// such a repository can come with either path: the link's, below dir, or
// the resolved one, outside it. It reads the cached scan.
func LinkedRepositories(dir string) map[string]string {
	var links map[string]string
	for _, p := range scanRepo(dir).nested {
		info, err := os.Lstat(p)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target := canonicalDir(p)
		if target == "" {
			continue
		}
		if links == nil {
			links = make(map[string]string)
		}
		links[target] = p
	}
	return links
}

// ContainerProjects returns the repositories below dir that hold a Mix
// project, in breadth-first order, when dir has no Mix project of its own;
// ok is false when it has one, or when dir is too large to scan (see
// containerScanDirs). It checks each repository, so it costs more than
// IsProjectContainer: it is for the messages that list the projects.
func ContainerProjects(dir string) (projects []string, ok bool) {
	if regularFile(filepath.Join(dir, "mix.exs")) {
		return nil, false
	}
	scan := scanRepo(dir)
	if scan.own {
		return nil, false
	}
	for _, p := range scan.nested {
		if holdsMixProject(p, containerScanDepth-1) {
			projects = append(projects, p)
		}
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
// "" when base owns them (or dir is not below base). container tells whether
// base is a container (IsProjectContainer).
//
// A file goes to another project only when base's own index does not hold
// it: the walker of base skips linked worktrees, so the nearest linked
// worktree between dir and base owns its files. Below a container, which has
// no index, the repository just below base (a clone or a worktree) owns its
// files, unless a nearer linked worktree is in it. A nested clone inside a
// project, a submodule, and a dependency in deps, _build, or node_modules
// stay with the project that holds them, as the walker indexes them there.
//
// The LSP proxy and the MCP frontend use it to send each file to its
// project's daemon.
func NestedProject(dir, base string, container bool) string {
	dir = filepath.Clean(dir)
	base = filepath.Clean(base)
	if !within(dir, base) {
		return ""
	}
	dir = ownerOfDependencies(dir)
	outermost := ""
	for d := dir; d != base && within(d, base); d = filepath.Dir(d) {
		info, err := os.Lstat(filepath.Join(d, ".git"))
		if err == nil {
			if info.Mode().IsRegular() && parser.IsLinkedWorktree(d) {
				return d // the nearest linked worktree
			}
			if container && info.IsDir() {
				outermost = d
			}
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return outermost
}

// isNestedProject reports whether dir is a clone (it holds a .git directory)
// or a linked worktree: a repository boundary of the project root rule.
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
