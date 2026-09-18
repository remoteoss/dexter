package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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
