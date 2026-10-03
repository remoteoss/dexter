package parser

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// IsElixirKeyword returns true if the name is an Elixir language keyword
// (control flow or definition keyword) rather than a user-defined macro.
func IsElixirKeyword(name string) bool {
	return elixirKeyword[name]
}

// elixirKeyword is the set of Elixir language constructs that take do blocks
// but are NOT user-defined macros — excluded from bare macro call tracking.
var elixirKeyword = map[string]bool{
	// Control flow
	"if": true, "unless": true, "cond": true, "case": true,
	"try": true, "receive": true, "for": true, "with": true,
	"fn": true, "do": true, "end": true, "else": true,
	"after": true, "catch": true, "rescue": true,
	"quote": true, "unquote": true, "when": true,
	"and": true, "or": true, "not": true, "in": true,
	// Definition keywords — def lines end with " do" but are definitions, not calls
	"def": true, "defp": true, "defmacro": true, "defmacrop": true,
	"defguard": true, "defguardp": true, "defdelegate": true,
	"defmodule": true, "defprotocol": true, "defimpl": true,
	"defstruct": true, "defexception": true,
}

type Definition struct {
	Module     string
	Function   string
	Arity      int
	Line       int
	FilePath   string
	Kind       string
	DelegateTo string
	DelegateAs string // for defdelegate with as: — the function name in the target module
	Params     string // comma-separated parameter names for this arity
}

type Reference struct {
	Module   string // fully-resolved module name
	Function string // function name (empty for module-only refs like alias/import/use)
	Line     int
	FilePath string
	Kind     string // "call", "alias", "import", "use"
}

func ParseFile(path string) ([]Definition, []Reference, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return ParseText(path, string(data))
}

// ParseText parses Elixir source text and returns definitions and references.
// The path is used to populate FilePath fields but the text is not read from disk.
func ParseText(path, text string) ([]Definition, []Reference, error) {
	source := []byte(text)
	result := TokenizeFull(source)
	return parseTextFromTokens(path, source, result.Tokens, result.Interp)
}

// ScanFuncName reads a function/type name ([a-z_][a-z0-9_?!]*) from the start of s.
func ScanFuncName(s string) string {
	if len(s) == 0 {
		return ""
	}
	c := s[0]
	if (c < 'a' || c > 'z') && c != '_' {
		return ""
	}
	i := 1
	for i < len(s) {
		c = s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '?' || c == '!' {
			i++
		} else {
			break
		}
	}
	return s[:i]
}

// JoinParams returns a comma-separated string of the first `arity` parameter
// names extracted from a function definition. Returns "" when names is nil or
// shorter than arity.
func JoinParams(names []string, arity int) string {
	if names == nil || arity > len(names) {
		return ""
	}
	return strings.Join(names[:arity], ",")
}

func resolveModule(s, currentModule string) string {
	if currentModule != "" {
		return strings.ReplaceAll(s, "__MODULE__", currentModule)
	}
	return s
}

// ExpandAliasPrefix rewrites the leading segment of a module reference using
// the aliases in scope: after `alias SharedLib.Accounts`, the reference
// `Accounts.Users` becomes `SharedLib.Accounts.Users`. It leaves __MODULE__
// alone — see resolveModule for that.
func ExpandAliasPrefix(modRef string, aliases map[string]string) string {
	if len(aliases) == 0 {
		return modRef
	}
	if full, ok := aliases[modRef]; ok {
		return full
	}
	if dot := strings.IndexByte(modRef, '.'); dot > 0 {
		if full, ok := aliases[modRef[:dot]]; ok {
			return full + modRef[dot:]
		}
	}
	return modRef
}

// ResolveModuleRef resolves a module reference through aliases and __MODULE__.
// Returns "" if the reference contains unresolvable __MODULE__.
func ResolveModuleRef(modRef string, aliases map[string]string, currentModule string) string {
	resolved := resolveModule(ExpandAliasPrefix(modRef, aliases), currentModule)
	if strings.Contains(resolved, "__MODULE__") {
		return ""
	}
	return resolved
}

func copyMap(m map[string]string) map[string]string {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func copyBoolMap(m map[string]bool) map[string]bool {
	cp := make(map[string]bool, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func IsElixirFile(path string) bool {
	extension := filepath.Ext(path)
	return extension == ".ex" || extension == ".exs"
}

// WalkElixirFiles walks root, skipping _build/.git/node_modules directories and
// linked git worktrees nested below root, and calls fn for each .ex/.exs file
// found. A nested worktree is a directory with a linked-worktree .git file, or
// one that git still records as a worktree of root's repository (see
// NestedWorktreeTops).
//
// A root that is itself a symlink to a directory is followed; symlinks below it
// are not. This has to match CollectElixirFilesParallel exactly, because the
// two describe the same set of files to different phases of the same index: the
// cold build enumerates with Collect, and the incremental sweep prunes every
// stored path this walk does not yield. Opening the root follows a symlink, and
// the entries below it report a symlinked directory as a non-directory, as in
// the collector. Paths keep the caller's prefix, which the editor's URIs depend
// on, instead of the resolved one filepath.EvalSymlinks would give.
func WalkElixirFiles(root string, fn func(path string, d fs.DirEntry) error) error {
	info, err := os.Stat(root)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		if !IsElixirFile(root) {
			return nil
		}
		return fn(root, fs.FileInfoToDirEntry(info))
	}

	recorded := NestedWorktreeSet(root)
	var walk func(dir string, isRoot bool) error
	walk = func(dir string, isRoot bool) error {
		if _, ok := recorded[dir]; ok && !isRoot {
			return nil
		}
		entries, err := ReadDirUnsorted(dir)
		if err != nil {
			return nil
		}
		if !isRoot && HasLinkedWorktreeGitFile(dir, entries) {
			return nil
		}
		for _, e := range entries {
			name := e.Name()
			path := filepath.Join(dir, name)
			if e.IsDir() {
				if skipDir(name) {
					continue
				}
				if err := walk(path, false); err != nil {
					return err
				}
				continue
			}
			if IsElixirFile(name) {
				if err := fn(path, e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(root, true)
}

// skipDir reports whether a directory name is excluded from indexing.
func skipDir(name string) bool {
	return name == "_build" || name == ".git" || name == "node_modules"
}

// HasLinkedWorktreeGitFile reports whether dir, whose entries are given, is the
// top of a linked git worktree. Such a checkout nested inside the project (e.g.
// Claude Code's .claude/worktrees/) is a full copy of the repository, and
// indexing it would duplicate every definition. Scanning the entries already
// read costs no syscall; only a directory that has a .git file pays one read.
func HasLinkedWorktreeGitFile(dir string, entries []fs.DirEntry) bool {
	for _, e := range entries {
		if e.Name() == ".git" {
			return !e.IsDir() && isLinkedWorktreeGitFile(filepath.Join(dir, ".git"))
		}
	}
	return false
}

// isLinkedWorktreeGitFile reports whether the .git file at path belongs to a
// linked worktree. Submodules also have a .git file, and they stay indexed like
// any other directory. Only a directory that has a .git file pays for this check.
func isLinkedWorktreeGitFile(path string) bool {
	gitdir, ok := gitdirFromFile(path)
	if !ok {
		return false
	}
	// Git gives each linked worktree an admin directory with a commondir file,
	// and a submodule's has none. This also covers worktrees of bare
	// repositories. After git prunes the admin directory, only its place under
	// <common dir>/worktrees/ is left to go by.
	if _, err := os.Stat(filepath.Join(gitdir, "commondir")); err == nil {
		return true
	}
	// git worktree add writes the admin directory's gitdir file, then the .git
	// file, then commondir, so a watcher can read the .git file before commondir
	// exists. The gitdir file naming this .git file back identifies the worktree
	// then; a submodule's git directory has no gitdir file.
	if b, err := os.ReadFile(filepath.Join(gitdir, "gitdir")); err == nil {
		back, backErr := os.Stat(resolveFrom(gitdir, strings.TrimSpace(string(b))))
		self, selfErr := os.Stat(path)
		if backErr == nil && selfErr == nil && os.SameFile(back, self) {
			return true
		}
	}
	if _, err := os.Stat(gitdir); err == nil {
		return false
	}
	// A submodule checked out at worktrees/<name> has its git directory at
	// .git/modules/worktrees/<name>, which is not a worktree admin directory.
	parent := filepath.Dir(gitdir)
	return filepath.Base(parent) == "worktrees" && filepath.Base(filepath.Dir(parent)) != "modules"
}

// gitdirFromFile returns the git directory that the .git file at path names,
// resolved against the file's directory. It reports false when path is not such
// a file, which includes a .git directory.
func gitdirFromFile(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	var buf [4096]byte
	n, _ := f.Read(buf[:])
	_ = f.Close()
	line, _, _ := strings.Cut(string(buf[:n]), "\n")
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	gitdir = strings.TrimSpace(gitdir)
	if !ok || gitdir == "" {
		return "", false
	}
	return resolveFrom(filepath.Dir(path), gitdir), true
}

// resolveFrom resolves path, as a git record writes it, against dir.
func resolveFrom(dir, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	return filepath.Clean(path)
}

// GitDir returns the git directory of the checkout at dir: dir/.git when that
// is a directory, or the directory a .git file names in a linked worktree or a
// submodule.
func GitDir(dir string) (string, bool) {
	path := filepath.Join(dir, ".git")
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		return path, true
	}
	return gitdirFromFile(path)
}

// IsLinkedWorktree reports whether dir is the top of a linked git worktree.
func IsLinkedWorktree(dir string) bool {
	return isLinkedWorktreeGitFile(filepath.Join(dir, ".git"))
}

// InLinkedWorktree reports whether path lies inside a linked git worktree nested
// below root, which the walkers skip. Single-file updates from watchers and
// editors check it so they do not index what a full walk leaves out. root
// itself may be a linked worktree; only directories strictly below it count.
func InLinkedWorktree(root, path string) bool {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	dir := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		if isLinkedWorktreeGitFile(filepath.Join(dir, ".git")) {
			return true
		}
	}
	return false
}

// NestedWorktreeTops lists the linked worktrees of root's repository that are
// checked out strictly below root. It reads git's own records: each admin
// directory in <common dir>/worktrees/ has a gitdir file that names the
// worktree's .git file. Paths come back in the spelling of root; aliases are
// other spellings of root that git may have recorded instead, and the
// symlink-resolved one is always tried.
//
// A worktree stays listed while git records it, even after its .git file is
// gone: git worktree remove, like rm -r, can delete the .git file first and the
// rest of the checkout after it, and a walk in between must not index what is
// left. git worktree prune drops the record of a checkout without a .git file.
//
// A root without a .git entry costs one stat, so walks of subtrees and of
// directories outside a repository pay almost nothing.
func NestedWorktreeTops(root string, aliases ...string) []string {
	gitDir, ok := GitDir(root)
	if !ok {
		return nil
	}
	common := gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common = resolveFrom(gitDir, strings.TrimSpace(string(b)))
	}
	admins, err := ReadDirUnsorted(filepath.Join(common, "worktrees"))
	if err != nil || len(admins) == 0 {
		return nil
	}
	spellings := append([]string{root}, aliases...)
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		spellings = append(spellings, resolved)
	}
	var tops []string
	for _, admin := range admins {
		adminDir := filepath.Join(common, "worktrees", admin.Name())
		b, err := os.ReadFile(filepath.Join(adminDir, "gitdir"))
		if err != nil {
			continue
		}
		top := filepath.Dir(resolveFrom(adminDir, strings.TrimSpace(string(b))))
		for _, spelling := range spellings {
			rel, err := filepath.Rel(spelling, top)
			if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				tops = append(tops, filepath.Join(root, rel))
				break
			}
		}
	}
	return tops
}

// NestedWorktreeSet is NestedWorktreeTops as a set, nil when
// there are none, so that the common case is a lookup in a nil map.
func NestedWorktreeSet(root string) map[string]struct{} {
	tops := NestedWorktreeTops(root)
	if len(tops) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(tops))
	for _, top := range tops {
		set[top] = struct{}{}
	}
	return set
}

// ReadDirUnsorted lists a directory without sorting the entries. os.ReadDir and
// filepath.WalkDir both sort every directory they read; the indexer keys rows by
// path and does not care about order, so the sort is pure cost.
func ReadDirUnsorted(dir string) ([]fs.DirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	return entries, err
}

// CollectElixirFilesParallel returns the paths of every .ex/.exs file below root,
// skipping the same directories and nested worktrees as WalkElixirFiles. It fans the traversal out
// across all cores: on a large monorepo the single-threaded walk is a real share
// of a cold index, and each directory read is an independent syscall.
//
// The returned order is unspecified. Callers key rows by path, so traversal
// order does not affect any query result.
func CollectElixirFilesParallel(root string) []string {
	// A root that is a file is yielded alone, as WalkElixirFiles does.
	if info, err := os.Stat(root); err == nil && !info.IsDir() {
		if IsElixirFile(root) {
			return []string{root}
		}
		return nil
	}
	recorded := NestedWorktreeSet(root)
	workers := runtime.NumCPU()
	sem := make(chan struct{}, workers)

	var (
		mu    sync.Mutex
		files []string
		wg    sync.WaitGroup
	)

	var walk func(dir string)
	walk = func(dir string) {
		defer wg.Done()

		if _, ok := recorded[dir]; ok && dir != root {
			return
		}
		entries, err := ReadDirUnsorted(dir)
		if err != nil {
			return
		}
		if dir != root && HasLinkedWorktreeGitFile(dir, entries) {
			return
		}

		var local []string
		for _, e := range entries {
			name := e.Name()
			path := filepath.Join(dir, name)

			// IsDir() is false for a symlink to a directory, so symlinked trees
			// are not descended into, as in WalkElixirFiles.
			if e.IsDir() {
				if skipDir(name) {
					continue
				}
				wg.Add(1)
				select {
				case sem <- struct{}{}:
					go func(d string) {
						defer func() { <-sem }()
						walk(d)
					}(path)
				default:
					// Pool is saturated; recurse inline rather than queue
					// unbounded goroutines.
					walk(path)
				}
				continue
			}
			if IsElixirFile(path) {
				local = append(local, path)
			}
		}

		if len(local) > 0 {
			mu.Lock()
			files = append(files, local...)
			mu.Unlock()
		}
	}

	wg.Add(1)
	walk(root)
	wg.Wait()
	return files
}
