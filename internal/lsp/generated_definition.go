package lsp

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/remoteoss/dexter/internal/beam"
	"github.com/remoteoss/dexter/internal/store"
)

// generatedSymbolLocations builds definition targets for callables that exist
// only in compiled form.
//
// generatedDefinitionResults names the module that owns such a callable; this
// names a position inside it, which is what an editor actually needs. The
// position is the compiler's own annotation for each callable — the Docs chunk
// entry's anno, carried on beam.Function.Line — paired with the owning module's
// source file.
//
// Callables annotated with the module line are deliberately skipped. That
// annotation means a transformer or a `@before_compile` hook produced the code
// away from any single source position, and returning line 1 is both
// indistinguishable from a real hit in the editor and no more useful than the
// module row the caller would otherwise fall back to. Only annotations that
// point somewhere specific are worth surfacing.
func (s *Server) generatedSymbolLocations(module, beamPath string, functions []beam.Function) []store.LookupResult {
	source := s.sourceFileForModule(module, beamPath)
	if source == "" {
		return nil
	}

	out := make([]store.LookupResult, 0, len(functions))
	seenLines := make(map[int]struct{}, len(functions))
	for _, f := range functions {
		if f.Line <= 1 {
			continue
		}
		if _, dup := seenLines[f.Line]; dup {
			continue
		}
		seenLines[f.Line] = struct{}{}
		out = append(out, store.LookupResult{
			Module:   module,
			FilePath: source,
			Line:     f.Line,
			Kind:     f.Kind,
			Arity:    f.Arity,
		})
	}
	return out
}

// sourceFileForModule returns the on-disk source file for a module, preferring
// the index and falling back to the module's own compile info.
//
// The index is authoritative: a module with a source row is a module someone
// wrote, whatever else the BEAM claims. Generated modules have no row, and for
// those the compile info records the file the generating macro lived in — for
// Spark's entity modules that is the framework file, not the module's own name.
func (s *Server) sourceFileForModule(module, beamPath string) string {
	if results, err := s.store.LookupModule(module); err == nil {
		for _, r := range results {
			if r.FilePath != "" && regularFileExists(r.FilePath) {
				return r.FilePath
			}
		}
	}
	if beamPath == "" {
		return ""
	}
	recorded, ok := beam.ReadSourcePath(beamPath)
	if !ok {
		return ""
	}
	if regularFileExists(recorded) {
		return recorded
	}
	return s.rebaseRecordedSource(recorded)
}

// rebaseRecordedSource maps a compile-time source path onto this checkout.
//
// The path was recorded wherever the artifact was built, which for a dependency
// is often a different directory — a `_build` copied between checkouts, a Docker
// build, a vendored artifact — so the absolute prefix carries no meaning here.
// Only the tail below the application's own lib directory is stable.
//
// The application is read from the recorded path, not from the BEAM path that
// led here. A generated module can live in one application while its source lives
// in another: Spark creates Ash's entity modules, so the artifact sits in ash's
// ebin while the file that generated it is under spark. Trusting the BEAM's
// application would look for the tail in the wrong dependency.
func (s *Server) rebaseRecordedSource(recorded string) string {
	if s.projectRoot == "" {
		return ""
	}
	for _, candidate := range sourceRebaseCandidates(recorded, s.projectRoot) {
		if regularFileExists(candidate) {
			return candidate
		}
	}
	return ""
}

// sourceRebaseCandidates lists the plausible on-disk locations for a recorded
// source path. Every Mix layout is offered, because the artifact does not record
// which one it was: `lib/<app>/...` is the application's own repository,
// `deps/<app>/lib/<app>/...` is a dependency, and `deps/<app>/...` is the older
// vendored shape.
func sourceRebaseCandidates(recorded, projectRoot string) []string {
	slash := filepath.ToSlash(recorded)
	idx := strings.LastIndex(slash, "/lib/")
	if idx < 0 {
		return nil
	}
	parts := strings.SplitN(slash[idx+len("/lib/"):], "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	app, tail := parts[0], filepath.FromSlash(parts[1])
	return []string{
		filepath.Join(projectRoot, "lib", app, tail),
		filepath.Join(projectRoot, "deps", app, "lib", app, tail),
		filepath.Join(projectRoot, "deps", app, tail),
	}
}

// regularFileExists reports whether path names a readable regular file. Anything
// else — a directory, a symlink to nowhere, a permission error — is treated as
// absent so a caller never returns an unopenable location to an editor.
func regularFileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
