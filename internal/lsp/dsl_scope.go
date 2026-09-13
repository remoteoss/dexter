package lsp

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const maxEbinIndexCacheEntries = 128

// ebinModuleIndex lists the modules compiled into one ebin directory, sorted so
// that finding everything under a prefix is a binary search plus a short scan.
//
// This is deliberately narrower than a whole-build-root manifest. Enumerating one
// application's ebin costs a single readdir (1319 entries for Ash, under a
// millisecond) and only happens for applications that actually provide DSL
// macros, so the memory is bounded by a handful of directories rather than by the
// number of modules in the project.
type ebinModuleIndex struct {
	dir     string
	stamp   fileStamp
	modules []string // sorted, without the "Elixir." prefix or ".beam" suffix
}

type ebinIndexCache struct {
	mu   sync.RWMutex
	dirs map[string]*ebinModuleIndex
}

func newEbinIndexCache() *ebinIndexCache {
	return &ebinIndexCache{dirs: make(map[string]*ebinModuleIndex)}
}

func (c *ebinIndexCache) get(dir string) *ebinModuleIndex {
	c.mu.RLock()
	idx := c.dirs[dir]
	c.mu.RUnlock()
	return idx
}

func (c *ebinIndexCache) put(dir string, idx *ebinModuleIndex) {
	c.mu.Lock()
	if _, exists := c.dirs[dir]; !exists && len(c.dirs) >= maxEbinIndexCacheEntries {
		// Keep the hot read path on an RWMutex instead of turning every lookup
		// into an LRU write. Reaching 128 distinct DSL-provider ebin directories
		// is already exceptional, so arbitrary one-at-a-time eviction gives a
		// hard memory bound without taxing the normal case.
		for victim := range c.dirs {
			delete(c.dirs, victim)
			break
		}
	}
	c.dirs[dir] = idx
	c.mu.Unlock()
}

// dslProvider is a module whose generated macros are in scope, with the BEAM it
// was found in. The path travels with the name because these modules are
// generated at compile time: they have no source file, so the index cannot
// resolve them and the caller must not try.
type dslProvider struct {
	module   string
	beamPath string
}

// modulesUnder returns the modules in dir whose name begins with prefix and has
// exactly one further dot-separated segment.
//
// One segment is what puts a module in scope at a given DSL depth. Inside
// `code_interface do`, Ash.Resource.Dsl.CodeInterface.Define is available but
// ...CodeInterface.Define.CustomInput is not — that belongs one level deeper,
// inside `define do`.
func (s *Server) modulesUnder(dir, prefix string) []dslProvider {
	idx := s.ebinIndex(dir)
	if idx == nil {
		return nil
	}
	start := sort.SearchStrings(idx.modules, prefix)
	var out []dslProvider
	for i := start; i < len(idx.modules) && strings.HasPrefix(idx.modules[i], prefix); i++ {
		name := idx.modules[i]
		if strings.Contains(name[len(prefix):], ".") {
			continue
		}
		out = append(out, dslProvider{
			module:   name,
			beamPath: filepath.Join(dir, "Elixir."+name+".beam"),
		})
	}
	return out
}

func (s *Server) ebinIndex(dir string) *ebinModuleIndex {
	idx := s.ebinIndexes.get(dir)
	if idx != nil && idx.valid() {
		return idx
	}

	fresh := buildEbinModuleIndex(dir)
	s.ebinIndexes.put(dir, fresh)
	return fresh
}

// valid reports whether the listing still describes dir. A directory's mtime
// moves when a module is added or removed, which is exactly when the list could
// be wrong; recompiling an existing module does not change which modules exist.
func (i *ebinModuleIndex) valid() bool {
	return i.modules != nil && statFileStamp(i.dir) == i.stamp
}

func buildEbinModuleIndex(dir string) *ebinModuleIndex {
	idx := &ebinModuleIndex{dir: dir, stamp: statFileStamp(dir)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return idx
	}
	modules := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "Elixir.") || !strings.HasSuffix(name, ".beam") {
			continue
		}
		module := name[len("Elixir.") : len(name)-len(".beam")]
		if module != "" {
			modules = append(modules, module)
		}
	}
	sort.Strings(modules)
	idx.modules = modules
	return idx
}

// dslScopeModules resolves the modules whose macros are in scope inside a DSL
// block, given the extension modules a compiled module reports and the path of
// do blocks enclosing the cursor.
//
// The mapping from a block path to a module name is the convention Spark's
// section_mod_name/entity_mod_name implement: the extension module followed by
// each path segment camelized. Dexter does not trust that derivation — every
// candidate is confirmed against the compiled artifacts, so a framework that
// names things differently simply yields nothing rather than wrong suggestions.
//
// Suffixes are tried longest first so that the innermost enclosing DSL block
// wins, and so that language forms (defmodule, def, if) fall away on their own
// instead of needing a skip list kept in step with the grammar.
func (s *Server) dslScopeModules(extensions []string, blockPath []string) []dslProvider {
	if len(extensions) == 0 || len(blockPath) == 0 {
		return nil
	}
	for start := 0; start < len(blockPath); start++ {
		suffix := blockPath[start:]
		camelized := make([]string, len(suffix))
		for i, segment := range suffix {
			camelized[i] = camelize(segment)
		}
		joined := strings.Join(camelized, ".")

		var found []dslProvider
		for _, extension := range extensions {
			dir := s.ebinDirForExtension(extension)
			if dir == "" {
				continue
			}
			found = append(found, s.modulesUnder(dir, extension+"."+joined+".")...)
		}
		if len(found) > 0 {
			sort.Slice(found, func(i, j int) bool { return found[i].module < found[j].module })
			return found
		}
	}
	return nil
}

// dslProvidersInScope returns the modules whose generated macros are in scope at
// a cursor inside module. Nested in a DSL block, that is the block's own entity
// and option modules; at the top of the module it is the extensions themselves,
// which supply the section macros.
//
// blockPath is a thunk because walking the syntax tree costs more than the whole
// rest of this resolution, and the overwhelming majority of modules have no DSL
// extensions at all. Asking for the extensions first means an ordinary module
// never pays for the walk.
func (s *Server) dslProvidersInScope(module string, blockPath func() []string) []dslProvider {
	extensions := s.macroProvidersForModule(module)
	if len(extensions) == 0 {
		return nil
	}
	if scoped := s.dslScopeModules(extensions, blockPath()); len(scoped) > 0 {
		return scoped
	}
	// The extensions are ordinary indexed modules, so their BEAMs resolve through
	// the normal path and need no hint.
	providers := make([]dslProvider, len(extensions))
	for i, extension := range extensions {
		providers[i] = dslProvider{module: extension}
	}
	return providers
}

// ebinDirForExtension resolves where an extension module's own application was
// compiled. That is generally a different application from the consumer's: an
// Ash resource lives in the project's ebin while Ash.Resource.Dsl lives in
// deps/ash's, and the section and entity modules are generated alongside it.
//
// Resolving through the generated-function cache means the extension's own macros
// are parsed and cached here too, so offering them later costs nothing extra.
func (s *Server) ebinDirForExtension(extension string) string {
	_ = s.generatedFunctionsForModule(extension)
	entry, ok := s.generatedCache.get(extension)
	if !ok || entry.beamPath == "" {
		return ""
	}
	return filepath.Dir(entry.beamPath)
}

// camelize mirrors Macro.camelize for the ASCII identifiers Elixir allows in DSL
// names: split on underscores, drop empty segments, upper-case the first letter
// of each. It need not be exhaustive — a mismatch only means the derived module
// is not found, and nothing is suggested.
func camelize(name string) string {
	parts := strings.Split(name, "_")
	var out strings.Builder
	out.Grow(len(name))
	for _, part := range parts {
		if part == "" {
			continue
		}
		out.WriteString(strings.ToUpper(part[:1]))
		out.WriteString(part[1:])
	}
	return out.String()
}
