package lsp

import (
	"sync"
)

// sharedCaches are the caches whose inputs are only the files on disk and the
// shared index. They belong to the IndexCoordinator, so every editor session
// and the daemon's headless core of one workspace share them: the warm-up at
// the end of a reindex, and the work of one editor, serve every other one.
// Each cache has its own lock and validates its entries against the disk, so
// sharing them is safe for concurrent use.
//
// Caches that depend on a session stay on Server: the BEAM processes and what
// is read through them depend on the mix binary that the session found.
type sharedCaches struct {
	using       *usingCache
	generated   *generatedFunctionCache
	beamLibs    *beamLibIndexCache
	buildRoots  *buildRootCache
	mixApps     *mixAppCache
	ebinIndexes *ebinIndexCache
	deps        *depsCache
}

func newSharedCaches() sharedCaches {
	return sharedCaches{
		using:       &usingCache{entries: make(map[string]*usingCacheEntry)},
		generated:   newGeneratedFunctionCache(),
		beamLibs:    newBeamLibIndexCache(),
		buildRoots:  &buildRootCache{},
		mixApps:     &mixAppCache{apps: make(map[string]mixAppEntry)},
		ebinIndexes: newEbinIndexCache(),
		deps:        &depsCache{dirs: make(map[string]bool)},
	}
}

// usingCache maps a module name to the parsed __using__ body of the file that
// defines it. See cachedUsingWithPath.
type usingCache struct {
	mu      sync.RWMutex
	entries map[string]*usingCacheEntry
}

func (c *usingCache) get(module string) (*usingCacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[module]
	return entry, ok
}

func (c *usingCache) put(module string, entry *usingCacheEntry) {
	c.mu.Lock()
	c.entries[module] = entry
	c.mu.Unlock()
}

// drop removes the entry of module if it is still old. A concurrent request
// that already put a newer entry keeps it.
func (c *usingCache) drop(module string, old *usingCacheEntry) {
	c.mu.Lock()
	if c.entries[module] == old {
		delete(c.entries, module)
	}
	c.mu.Unlock()
}

func (c *usingCache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// depsCache maps a directory to whether the files in it are dependencies: they
// are below the deps directory of a Mix project. The answer depends only on
// where mix.exs files are, so it is cleared when a mix.exs or mix.lock
// changes, and on each full reconciliation, which follows lost events and
// branch switches. Clearing is one map allocation; the next requests fill it
// again with a few stats for each directory.
type depsCache struct {
	mu   sync.RWMutex
	dirs map[string]bool
}

// A nil depsCache, as in a Server built by a test without a coordinator,
// caches nothing.
func (c *depsCache) get(dir string) (bool, bool) {
	if c == nil {
		return false, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	result, ok := c.dirs[dir]
	return result, ok
}

func (c *depsCache) put(dir string, result bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.dirs[dir] = result
	c.mu.Unlock()
}

func (c *depsCache) clear() {
	c.mu.Lock()
	if len(c.dirs) > 0 {
		c.dirs = make(map[string]bool)
	}
	c.mu.Unlock()
}

// ManifestChanged tells the workspace that a mix.exs or mix.lock changed: the
// caches that depend on where the Mix projects are are cleared, and BEAMs that
// failed to start are tried again, because new dependencies can fix them. The
// runtime calls it for the events of its native watcher; a session calls it
// for the events of its editor.
func (s *Server) ManifestChanged() {
	s.index.manifestChanged()
	s.index.beamFailures.forget()
}

// manifestChanged clears the caches that depend on where the Mix projects of
// the workspace are.
func (c *IndexCoordinator) manifestChanged() {
	c.caches.deps.clear()
}
