package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// beamLibIndex is an immutable snapshot of one build root's _build/<profile>/lib.
//
// It exists because both obvious ways of finding a module's BEAM are too slow
// for a hot path:
//
//   - filepath.Glob("_build/*/lib/*/ebin/Elixir.Mod.beam") readdir's every
//     dependency's ebin directory and compares every filename in it: measured
//     1.9ms and 2.5k allocations on a 17-dep fixture holding ~2k beam files,
//     paid per module per completion request.
//   - A manifest of every compiled module avoids the readdir but holds roughly
//     one map entry per module in the project *and* its dependencies — on a
//     100k-file monorepo that is ~175k entries and ~18MB resident, and
//     rebuilding it costs ~100ms during which every completion blocks.
//
// Neither is necessary. The application a module belongs to is derivable from
// the source path the index already gives us (deps/<app>, apps/<app>, or the
// project's own app), so resolving a BEAM is a single stat. Only the small part
// that is not derivable is cached here: the application directory names.
//
// Snapshots are published immutably and replaced rather than mutated, so reads
// need no lock after the pointer is obtained.
type beamLibIndex struct {
	buildRoot string
	libDir    string
	libStamp  fileStamp
	apps      []string // every application directory under libDir
	rootApp   string   // the project's own app, when unambiguous
	loaded    bool
}

type beamLibIndexCache struct {
	mu    sync.RWMutex
	roots map[string]*beamLibIndex
}

func newBeamLibIndexCache() *beamLibIndexCache {
	return &beamLibIndexCache{roots: make(map[string]*beamLibIndex)}
}

// beamLocation is where a module's compiled BEAM is, plus the signal that says
// whether a "not compiled" answer is still true.
type beamLocation struct {
	// beamPath is empty when the module has no compiled BEAM. beamStamp is the
	// stat already performed while resolving it, so callers do not repeat it.
	beamPath  string
	beamStamp fileStamp
	// watchDir is the directory whose mtime moves when beamPath would appear.
	// Stamping it lets a negative result be invalidated by the compile that
	// creates the module rather than by a timer, so a developer who adds a
	// macro call and recompiles sees the new functions on the next keystroke.
	watchDir   string
	watchStamp fileStamp
}

// libIndex returns a current snapshot for buildRoot, rebuilding it only when
// the lib directory itself changed. The rebuild readdir's a directory holding
// one entry per application, so it stays cheap however many modules exist.
func (s *Server) libIndex(buildRoot string) *beamLibIndex {
	if buildRoot == "" {
		return nil
	}
	s.beamLibs.mu.RLock()
	idx := s.beamLibs.roots[buildRoot]
	s.beamLibs.mu.RUnlock()
	if idx != nil && idx.valid() {
		return idx
	}

	fresh := buildBeamLibIndex(buildRoot)
	s.debugf("Generated BEAM applications: build_root=%s lib=%s apps=%d root_app=%s loaded=%t", buildRoot, fresh.libDir, len(fresh.apps), fresh.rootApp, fresh.loaded)
	s.beamLibs.mu.Lock()
	s.beamLibs.roots[buildRoot] = fresh
	s.beamLibs.mu.Unlock()
	return fresh
}

// valid reports whether the snapshot still describes libDir. A directory's mtime
// moves when an application is added or removed, which is exactly when the
// cached names could be wrong.
func (i *beamLibIndex) valid() bool {
	if !i.loaded {
		return false
	}
	return statFileStamp(i.libDir) == i.libStamp
}

func buildBeamLibIndex(buildRoot string) *beamLibIndex {
	profile := os.Getenv("MIX_ENV")
	if profile == "" {
		profile = "dev"
	}
	idx := &beamLibIndex{
		buildRoot: buildRoot,
		libDir:    filepath.Join(buildRoot, "_build", profile, "lib"),
	}
	idx.libStamp = statFileStamp(idx.libDir)

	entries, err := os.ReadDir(idx.libDir)
	if err != nil {
		// Nothing compiled yet. loaded stays false so the next call retries
		// instead of trusting an empty snapshot.
		return idx
	}

	// Dependency directories share their name with the application they build,
	// so subtracting them from libDir leaves the project's own application.
	depNames := make(map[string]bool, 64)
	if deps, err := os.ReadDir(filepath.Join(buildRoot, "deps")); err == nil {
		for _, dep := range deps {
			if dep.IsDir() {
				depNames[dep.Name()] = true
			}
		}
	}

	var roots []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		idx.apps = append(idx.apps, name)
		if !depNames[name] {
			roots = append(roots, name)
		}
	}
	if len(roots) == 1 {
		idx.rootApp = roots[0]
	}
	idx.loaded = true
	return idx
}

// locateModuleBEAM resolves the compiled BEAM for a module whose source lives at
// sourcePath, without enumerating compiled modules. Returns a zero beamPath when
// the module is not compiled, together with the directory to watch for it.
func (s *Server) locateModuleBEAM(buildRoot, module, sourcePath string) beamLocation {
	idx := s.libIndex(buildRoot)
	if idx == nil || module == "" {
		return beamLocation{}
	}
	fileName := "Elixir." + module + ".beam"

	// The derived application is authoritative when it has an ebin directory, so
	// a module missing from it is simply not compiled — do not go probing every
	// dependency to double-check.
	if app := appForSource(idx.buildRoot, sourcePath); app != "" {
		if loc, ok := beamInApp(idx.libDir, app, fileName); ok {
			s.debugf("Generated BEAM locate: module=%s strategy=source-app app=%s beam=%s", module, app, loc.beamPath)
			return loc
		}
	} else if idx.rootApp != "" {
		if loc, ok := beamInApp(idx.libDir, idx.rootApp, fileName); ok {
			s.debugf("Generated BEAM locate: module=%s strategy=root-app app=%s beam=%s", module, idx.rootApp, loc.beamPath)
			return loc
		}
	}

	// Either the application could not be derived, or its ebin is missing
	// because the mix :app name differs from its directory. Probe the
	// applications once; the per-module cache means this is not repeated.
	for _, app := range idx.apps {
		ebin := filepath.Join(idx.libDir, app, "ebin")
		beam := filepath.Join(ebin, fileName)
		if stamp := statFileStamp(beam); stamp.exists {
			s.debugf("Generated BEAM locate: module=%s strategy=application-scan app=%s beam=%s", module, app, beam)
			return beamLocation{
				beamPath:   beam,
				beamStamp:  stamp,
				watchDir:   ebin,
				watchStamp: statFileStamp(ebin),
			}
		}
	}

	// Nothing is compiled for this module. Watch the lib directory itself when
	// there is no ebin to watch, so the answer is revisited once a build exists.
	if idx.loaded && idx.libStamp.exists {
		s.debugf("Generated BEAM locate: module=%s strategy=application-scan result=missing watch=%s", module, idx.libDir)
		return beamLocation{watchDir: idx.libDir, watchStamp: idx.libStamp}
	}
	s.debugf("Generated BEAM locate: module=%s strategy=application-scan result=unavailable watch=%s", module, idx.libDir)
	return beamLocation{watchDir: idx.libDir}
}

// beamInApp looks for fileName inside one application's ebin. The second result
// is false only when that application has no ebin directory, meaning the caller
// should keep looking elsewhere.
func beamInApp(libDir, app, fileName string) (beamLocation, bool) {
	ebin := filepath.Join(libDir, app, "ebin")
	ebinStamp := statFileStamp(ebin)
	if !ebinStamp.exists {
		return beamLocation{}, false
	}
	loc := beamLocation{watchDir: ebin, watchStamp: ebinStamp}
	beam := filepath.Join(ebin, fileName)
	if stamp := statFileStamp(beam); stamp.exists {
		loc.beamPath = beam
		loc.beamStamp = stamp
	}
	return loc, true
}

// appForSource derives the mix application a source file belongs to. Mix checks
// dependencies out to <root>/deps/<app> and umbrella children live in
// <root>/apps/<app>, and both map directly onto _build/<profile>/lib/<app>.
// Anchoring on buildRoot keeps an unrelated directory that happens to be named
// "deps" from being mistaken for the dependency root.
func appForSource(buildRoot, sourcePath string) string {
	if buildRoot == "" || sourcePath == "" {
		return ""
	}
	rel := strings.TrimPrefix(filepath.ToSlash(sourcePath), filepath.ToSlash(buildRoot)+"/")
	if rel == filepath.ToSlash(sourcePath) {
		return "" // not inside this build root
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return ""
	}
	if parts[0] == "deps" || parts[0] == "apps" {
		return parts[1]
	}
	return ""
}

// locateCompiledModule finds a module's BEAM. hint is the source file that
// places the module: its own, or for a generated module the nearest lexical
// parent's.
//
// The build found from hint is usually the only one, and then this is
// locateModuleBEAM. A monorepo can compile a library from another project
// instead: tiger builds libs/* as path dependencies inside apps/tiger, so a
// library's own tree has no _build at all. When the hinted build has no BEAM, or
// the workspace holds several builds, the other builds are asked for the
// library's application too, and the most recently compiled BEAM wins. A stale
// choice only costs generated functions that a newer compile added, and a
// library compiled in more than one place is rare.
func (s *Server) locateCompiledModule(buildRoot, module, hint string) beamLocation {
	loc := s.locateModuleBEAM(buildRoot, module, hint)
	builds := s.workspaceBuildRoots()
	if loc.beamPath != "" && len(builds) <= 1 {
		return loc
	}
	app := s.mixAppFor(hint)
	if app == "" {
		return loc
	}
	fileName := "Elixir." + module + ".beam"
	best := loc
	var watch beamLocation
	var compared []string
	for _, root := range builds {
		if root == buildRoot {
			continue // locateModuleBEAM already asked it
		}
		idx := s.libIndex(root)
		if idx == nil || !idx.loaded {
			continue
		}
		candidate, ok := beamInApp(idx.libDir, app, fileName)
		if !ok {
			continue
		}
		if candidate.beamPath == "" {
			if watch.watchDir == "" {
				watch = candidate
			}
			continue
		}
		compared = append(compared, candidate.beamPath)
		if best.beamPath == "" || candidate.beamStamp.mtime > best.beamStamp.mtime {
			best = candidate
		}
	}
	switch {
	case best.beamPath != "":
		if best.beamPath != loc.beamPath || len(compared) > 0 {
			s.debugf("Generated BEAM locate: module=%s strategy=workspace-builds app=%s beam=%s compared=%v", module, app, best.beamPath, compared)
		}
		return best
	case watch.watchDir != "" && !loc.watchStamp.exists:
		// Not compiled anywhere yet, and the hinted build has no ebin to
		// watch: the application's ebin in a build that compiles it moves
		// when the module appears.
		s.debugf("Generated BEAM locate: module=%s strategy=workspace-builds app=%s result=missing watch=%s", module, app, watch.watchDir)
		return watch
	default:
		return loc
	}
}

// workspaceBuildRootTTL bounds how long a discovered set of builds is trusted.
// A first compile in another project creates a _build the scan has not seen.
const workspaceBuildRootTTL = 30 * time.Second

// workspaceBuildRootDepth is how deep below the workspace root Mix projects are
// looked for: apps/<name> and libs/<name> are depth 2, and one more level
// covers grouped layouts such as packages/<group>/<name>.
const workspaceBuildRootDepth = 3

type buildRootCache struct {
	mu        sync.Mutex
	roots     []string
	scannedAt time.Time
}

// workspaceBuildRoots lists the Mix projects in the workspace that have been
// compiled: directories holding both mix.exs and _build. Dependencies, build
// output, node_modules, and hidden directories are skipped, so the walk reads
// one directory per project-level folder rather than the source tree.
func (s *Server) workspaceBuildRoots() []string {
	c := s.buildRoots
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.scannedAt.IsZero() && time.Since(c.scannedAt) < workspaceBuildRootTTL {
		return c.roots
	}
	var roots []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if isRegularFile(filepath.Join(dir, "mix.exs")) && isDir(filepath.Join(dir, "_build")) {
			roots = append(roots, dir)
		}
		if depth == workspaceBuildRootDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || strings.HasPrefix(name, ".") || name == "deps" || name == "_build" || name == "node_modules" {
				continue
			}
			walk(filepath.Join(dir, name), depth+1)
		}
	}
	if s.projectRoot != "" {
		walk(s.projectRoot, 0)
	}
	c.roots, c.scannedAt = roots, time.Now()
	s.debugf("Generated BEAM builds: workspace=%s builds=%v", s.projectRoot, roots)
	return roots
}

type mixAppEntry struct {
	stamp fileStamp
	app   string
}

type mixAppCache struct {
	mu   sync.Mutex
	apps map[string]mixAppEntry // mix.exs path → application name
}

var (
	mixAppPattern       = regexp.MustCompile(`\bapp:\s*:([a-z_][A-Za-z0-9_]*)`)
	mixAppAttrPattern   = regexp.MustCompile(`\bapp:\s*@([a-z_][A-Za-z0-9_]*)`)
	mixModuleAttrFormat = `(?m)^\s*@%s\s+:([a-z_][A-Za-z0-9_]*)`
)

// mixAppFor returns the application of the Mix project that owns path: the
// nearest mix.exs at or above it, within the workspace. The application name is
// what a build calls the project's lib directory, and it need not match the
// project's directory: libs/remote-library builds as `remote`.
func (s *Server) mixAppFor(path string) string {
	if path == "" || s.projectRoot == "" {
		return ""
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		rel, err := filepath.Rel(s.projectRoot, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return ""
		}
		mixPath := filepath.Join(dir, "mix.exs")
		if stamp := statFileStamp(mixPath); stamp.exists {
			return s.mixAppFromFile(mixPath, stamp)
		}
		if rel == "." {
			return ""
		}
	}
}

func (s *Server) mixAppFromFile(mixPath string, stamp fileStamp) string {
	c := s.mixApps
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.apps[mixPath]; ok && entry.stamp == stamp {
		return entry.app
	}
	app := ""
	if data, err := os.ReadFile(mixPath); err == nil {
		app = parseMixApp(data)
	}
	c.apps[mixPath] = mixAppEntry{stamp: stamp, app: app}
	return app
}

// parseMixApp reads the application name from a mix.exs project definition:
// `app: :name`, or `app: @attr` with the attribute set to an atom. Anything
// else is left unresolved, which only skips the cross-build lookup.
func parseMixApp(data []byte) string {
	if match := mixAppPattern.FindSubmatch(data); match != nil {
		return string(match[1])
	}
	if match := mixAppAttrPattern.FindSubmatch(data); match != nil {
		attr := regexp.MustCompile(fmt.Sprintf(mixModuleAttrFormat, regexp.QuoteMeta(string(match[1]))))
		if value := attr.FindSubmatch(data); value != nil {
			return string(value[1])
		}
	}
	return ""
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
