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
	tokens := Tokenize(source)
	return parseTextFromTokens(path, source, tokens)
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

// ResolveModuleRef resolves a module reference through aliases and __MODULE__.
// Returns "" if the reference contains unresolvable __MODULE__.
func ResolveModuleRef(modRef string, aliases map[string]string, currentModule string) string {
	resolved := modRef
	if full, ok := aliases[modRef]; ok {
		resolved = full
	} else if parts := strings.SplitN(modRef, ".", 2); len(parts) == 2 {
		if full, ok := aliases[parts[0]]; ok {
			resolved = full + "." + parts[1]
		}
	}
	resolved = resolveModule(resolved, currentModule)
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

// WalkElixirFiles walks root, skipping _build/.git/node_modules directories,
// and calls fn for each .ex/.exs file found.
//
// A root that is itself a symlink to a directory is followed; symlinks below it
// are not. This has to match CollectElixirFilesParallel exactly, because the
// two describe the same set of files to different phases of the same index: the
// cold build enumerates with Collect, and the incremental sweep prunes every
// stored path this walk does not yield. filepath.WalkDir alone stats the root
// with Lstat, so it treats a symlinked root as a non-directory and yields
// nothing — which on a symlinked project or stdlib root made the sweep prune
// the entire index the cold build had just written. Walking the root's children
// rather than the root keeps the caller's path prefix, which the editor's URIs
// depend on, instead of the resolved one filepath.EvalSymlinks would give.
func WalkElixirFiles(root string, fn func(path string, d fs.DirEntry) error) error {
	walk := func(target string) error {
		return filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if skipDir(filepath.Base(path)) {
					return filepath.SkipDir
				}
				return nil
			}
			if !IsElixirFile(path) {
				return nil
			}
			return fn(path, d)
		})
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		return walk(root)
	}

	entries, err := readDirUnsorted(root)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() && skipDir(e.Name()) {
			continue
		}
		if err := walk(filepath.Join(root, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// skipDir reports whether a directory name is excluded from indexing.
func skipDir(name string) bool {
	return name == "_build" || name == ".git" || name == "node_modules"
}

// readDirUnsorted lists a directory without sorting the entries. os.ReadDir and
// filepath.WalkDir both sort every directory they read; the indexer keys rows by
// path and does not care about order, so the sort is pure cost.
func readDirUnsorted(dir string) ([]fs.DirEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	return entries, err
}

// CollectElixirFilesParallel returns the paths of every .ex/.exs file below root,
// skipping the same directories as WalkElixirFiles. It fans the traversal out
// across all cores: on a large monorepo the single-threaded walk is a real share
// of a cold index, and each directory read is an independent syscall.
//
// The returned order is unspecified. Callers key rows by path, so traversal
// order does not affect any query result.
func CollectElixirFilesParallel(root string) []string {
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

		entries, err := readDirUnsorted(dir)
		if err != nil {
			return
		}

		var local []string
		for _, e := range entries {
			name := e.Name()
			path := filepath.Join(dir, name)

			// IsDir() is false for a symlink to a directory, so symlinked trees
			// are not descended into — the same behaviour as filepath.WalkDir.
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
