package lsp

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/remoteoss/dexter/internal/beam"
	"github.com/remoteoss/dexter/internal/store"
)

// generatedDefinitionSources is what a compiled module records about where its
// functions were defined.
type generatedDefinitionSources struct {
	// debugInfo holds the definition lines from the Dbgi chunk. It is empty for
	// a module compiled without debug info, and for Erlang modules.
	debugInfo beam.DebugInfo

	// compileSource is the source path from the CInf chunk: the file the module
	// was compiled from, as it was spelled on the machine that compiled it.
	compileSource string

	// beamMtime is when the BEAM was written, to tell whether its lines can
	// have drifted from the source.
	beamMtime int64
}

// generatedDefinitionSourcesFor returns a compiled module's definition sources.
// They are read once per BEAM, and a failed read is memoized as empty: a module
// compiled without debug info will not grow it until rebuilt.
func (s *Server) generatedDefinitionSourcesFor(module, beamPath string) (generatedDefinitionSources, bool) {
	// Resolving first validates the entry and yields the BEAM path.
	_ = s.generatedFunctionsFor(module, beamPath)

	entry, ok := s.generatedCache.get(module)
	if !ok || entry.beamPath == "" {
		return generatedDefinitionSources{}, false
	}
	if !entry.definitionSourcesResolved {
		started := s.debugNow()
		info, err := beam.ReadDefinitionLines(entry.beamPath)
		source, _ := beam.ReadSourcePath(entry.beamPath)
		if !started.IsZero() {
			s.debugf("Generated BEAM definition sources: module=%s definitions=%d error=%v source=%q total=%s", module, len(info.Lines), err, source, time.Since(started).Round(time.Microsecond))
		}
		entry.definitionSources = generatedDefinitionSources{debugInfo: info, compileSource: source, beamMtime: entry.beamStamp.mtime}
		entry.definitionSourcesResolved = true
		// Another request may have loaded a newer BEAM while this one read the
		// chunks. Its entry must not be replaced with this older one.
		if current, ok := s.generatedCache.get(module); ok && current.beamPath == entry.beamPath && current.beamStamp == entry.beamStamp {
			s.generatedCache.put(module, entry)
		}
	}
	return entry.definitionSources, true
}

// generatedDefinitionResultsFor is where navigation sends a generated function.
// It starts from generatedDefinitionResults, the closest source-backed module,
// and moves to the function's own line when the compiled module records one in
// that module's source: for a function a DSL declared, that is the line that
// declared it. precise reports whether it did.
//
// A module with no source row of its own, such as a Spark entity module, may
// still record a line in the file it was compiled from, which is usually a
// framework file under deps. That file is tried after the indexed ones.
//
// A recorded line is used only when it belongs to the file being opened and
// falls after the module's own line, since a line at or before it says nothing
// the module result does not. The BEAM may be older than the source. Dexter
// cannot compile the project, and a line from the last compile is usually
// right and at worst a few lines off, which is still closer than the module
// line. When the source has changed since the compile, correctLineDrift moves
// each line to follow the edit where it can. Anything else keeps the module
// result.
func (s *Server) generatedDefinitionResultsFor(module, beamPath string, functions []beam.Function) (results []store.LookupResult, precise bool) {
	results = s.generatedDefinitionResults(module)
	if len(functions) == 0 {
		return results, false
	}
	sources, ok := s.generatedDefinitionSourcesFor(module, beamPath)
	if !ok {
		return results, false
	}
	for _, result := range results {
		if !sources.describe(result.FilePath, s.projectRoot) {
			continue
		}
		if lines := s.recordedLinesIn(module, result.FilePath, result.Line, sources, functions); len(lines) > 0 {
			return lines, true
		}
		return results, false
	}
	if own, err := s.store.LookupModule(module); err == nil && len(own) == 0 {
		if source := s.compiledSourceFile(sources.compileSource); source != "" {
			if lines := s.recordedLinesIn(module, source, 1, sources, functions); len(lines) > 0 {
				return lines, true
			}
		}
	}
	return results, false
}

// recordedLinesIn returns a result for each distinct line after afterLine that
// the compiled module records for functions in file, corrected for edits made
// since the compile. afterLine is the module's line in the current text, so it
// is compared after the correction: lines added above the module move both.
func (s *Server) recordedLinesIn(module, file string, afterLine int, sources generatedDefinitionSources, functions []beam.Function) []store.LookupResult {
	lines := s.correctLineDrift(file, sources.beamMtime, generatedFunctionLines(module, file, sources, functions), functions)
	return slices.DeleteFunc(lines, func(r store.LookupResult) bool { return r.Line <= afterLine })
}

// generatedFunctionLines returns a result for each distinct line that the
// compiled module records for functions in file.
//
// The Dbgi line is preferred because it also honors `@file`. The Docs chunk
// annotation is the fallback, because a module compiled without debug info
// still has it. It is often the line the def was expanded at, but not always:
// a generator can give the def one line and its docs another.
func generatedFunctionLines(module, file string, sources generatedDefinitionSources, functions []beam.Function) []store.LookupResult {
	useDebugInfo := len(sources.debugInfo.Lines) > 0
	var lines []store.LookupResult
	for _, function := range functions {
		line := function.Line
		if useDebugInfo {
			line = sources.debugInfo.Lines[beam.FunctionKey{Name: function.Name, Arity: function.Arity}]
		}
		if line <= 0 || slices.ContainsFunc(lines, func(r store.LookupResult) bool { return r.Line == line }) {
			continue
		}
		lines = append(lines, store.LookupResult{
			Module:   module,
			FilePath: file,
			Line:     line,
			Kind:     function.Kind,
			Arity:    function.Arity,
		})
	}
	return lines
}

// correctLineDrift moves recorded lines to follow edits made since the compile.
//
// A BEAM older than its source, or a buffer with unsaved changes, can describe
// lines that have moved. Dexter cannot compile the project, so it looks for
// the declaration in the current text instead: a call whose first argument is
// the function's name as an atom, which is how a macro call names what it
// declares (`define :list_rooms`, `field :email`). The nearest match wins.
// The recorded line is kept when it still declares the function, when no line
// does, or when two matches are equally near. A line past the end of the
// current text, with no declaration to move to, is dropped. A BEAM that is
// current is never corrected, because its lines are exact even where the
// declaration does not spell the name.
func (s *Server) correctLineDrift(path string, beamMtime int64, results []store.LookupResult, functions []beam.Function) []store.LookupResult {
	text, ok := s.driftedSourceText(path, beamMtime)
	if !ok {
		return results
	}
	lines := blankHeredocs(strings.Split(text, "\n"))
	names := make(map[int]string, len(functions))
	for _, function := range functions {
		names[function.Arity] = function.Name
	}
	out := results[:0]
	for _, result := range results {
		line := nearestDeclaringLine(lines, result.Line, names[result.Arity])
		if line > len(lines) || slices.ContainsFunc(out, func(r store.LookupResult) bool { return r.Line == line }) {
			continue
		}
		result.Line = line
		out = append(out, result)
	}
	return out
}

// driftedSourceText returns the current text of path when it may differ from
// the text the BEAM was compiled from: the open buffer if it has unsaved
// changes, or the file if it was written after the BEAM.
func (s *Server) driftedSourceText(path string, beamMtime int64) (string, bool) {
	open, isOpen := s.docs.GetIfOpen(string(pathToURI(path)))
	source := statFileStamp(path)
	stale := source.exists && source.mtime > beamMtime
	if !isOpen && !stale {
		return "", false
	}
	disk, err := os.ReadFile(path)
	switch {
	case isOpen && (err != nil || open != string(disk)):
		return open, true
	case err == nil && stale:
		return string(disk), true
	}
	return "", false
}

// blankHeredocs replaces the lines inside heredocs with empty lines, so that
// an example in a @moduledoc or @doc, such as `define :foo`, is not taken for
// a declaration. Line numbers do not change.
func blankHeredocs(lines []string) []string {
	out := make([]string, len(lines))
	var open string
	for i, line := range lines {
		inside := open != ""
		for _, delimiter := range []string{`"""`, "'''"} {
			if open != "" && open != delimiter {
				continue
			}
			if strings.Count(line, delimiter)%2 == 1 {
				if open == "" {
					open = delimiter
				} else {
					open = ""
				}
			}
		}
		if !inside && open == "" {
			out[i] = line
		}
	}
	return out
}

// nearestDeclaringLine returns the 1-based line nearest to recorded that
// declares function by name, or recorded when that is ambiguous or not found.
func nearestDeclaringLine(lines []string, recorded int, function string) int {
	if function == "" || recorded < 1 {
		return recorded
	}
	names := []string{function}
	if base := strings.TrimRight(function, "!?"); base != function && base != "" {
		names = append(names, base)
	}
	declares := func(line int) bool {
		return line >= 1 && line <= len(lines) && lineDeclaresAtom(lines[line-1], names)
	}
	if declares(recorded) {
		return recorded
	}
	for distance := 1; recorded-distance >= 1 || recorded+distance <= len(lines); distance++ {
		above, below := declares(recorded-distance), declares(recorded+distance)
		switch {
		case above && below:
			return recorded
		case above:
			return recorded - distance
		case below:
			return recorded + distance
		}
	}
	return recorded
}

// lineDeclaresAtom reports whether text is a call whose first argument is one
// of names as an atom: `define :list_rooms`, `field :email, :string`, or
// `Lib.define(:name)`. That is the shape of a macro call that declares a
// name. An atom anywhere else, such as a keyword value or a typespec, is not.
func lineDeclaresAtom(text string, names []string) bool {
	rest := strings.TrimLeft(text, " \t")
	callee := 0
	for callee < len(rest) && (isIdentifierByte(rest[callee]) || rest[callee] == '.') {
		callee++
	}
	// A module attribute such as `@tag :slow` sets a value; it declares nothing.
	if callee == 0 || rest[0] == ':' || rest[0] == '@' || (rest[0] >= '0' && rest[0] <= '9') {
		return false
	}
	args := strings.TrimLeft(rest[callee:], " \t")
	if len(args) == len(rest[callee:]) {
		// No space after the callee: only a parenthesized call qualifies.
		if !strings.HasPrefix(args, "(") {
			return false
		}
	}
	args = strings.TrimLeft(strings.TrimPrefix(args, "("), " \t")
	for _, name := range names {
		if strings.HasPrefix(args, ":"+name) {
			end := 1 + len(name)
			if end == len(args) || !isIdentifierByte(args[end]) {
				return true
			}
		}
	}
	return false
}

func isIdentifierByte(b byte) bool {
	return b == '_' || b == '!' || b == '?' || b == '@' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// describe reports whether the recorded lines belong to path. When the module
// has debug info, its file names are the test; otherwise the compile info's.
func (sources generatedDefinitionSources) describe(path, projectRoot string) bool {
	if len(sources.debugInfo.Lines) > 0 {
		return debugInfoDescribesFile(sources.debugInfo, path)
	}
	if sources.compileSource == "" {
		return false
	}
	return sources.compileSource == path || slices.Contains(sourceRebaseCandidates(sources.compileSource, projectRoot), path)
}

// debugInfoDescribesFile reports whether debug info was compiled from path.
// The absolute path is compared first; the relative one covers a project that
// has moved or is reached through a symlink since it was compiled.
func debugInfoDescribesFile(info beam.DebugInfo, path string) bool {
	if info.File != "" && info.File == path {
		return true
	}
	relative := filepath.ToSlash(info.RelativeFile)
	if relative == "" || filepath.IsAbs(info.RelativeFile) {
		return false
	}
	return strings.HasSuffix(filepath.ToSlash(path), "/"+relative)
}

// compiledSourceFile maps a source path from compile info onto this checkout.
//
// The path was recorded wherever the artifact was built, which for a dependency
// is often a different directory: a `_build` copied between checkouts, a Docker
// build, a vendored artifact. When it does not exist here, only the tail below
// the application's own lib directory is stable.
//
// The application is read from the recorded path, not from the BEAM path. A
// generated module can live in one application while its source lives in
// another: Spark creates Ash's entity modules, so the artifact sits in ash's
// ebin while the file that generated it is under spark.
//
// A recorded path outside the project is tried last. It is right for a `path:`
// dependency, but a `_build` copied from another checkout or worktree records
// that checkout, which usually still exists: returning it would open the file
// in the wrong tree.
func (s *Server) compiledSourceFile(recorded string) string {
	if recorded == "" {
		return ""
	}
	if pathWithin(recorded, s.projectRoot) && regularFileExists(recorded) {
		return recorded
	}
	for _, candidate := range sourceRebaseCandidates(recorded, s.projectRoot) {
		if regularFileExists(candidate) {
			return candidate
		}
	}
	if regularFileExists(recorded) {
		return recorded
	}
	return ""
}

// pathWithin reports whether path is root or below it.
func pathWithin(path, root string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sourceRebaseCandidates lists the plausible on-disk locations for a recorded
// source path, most specific first.
//
// First come the recorded path's own tails under the project root, longest
// first: a checkout that moved or was copied keeps its layout below the root,
// whether that is `lib/`, an umbrella's `apps/<app>/lib/`, or `deps/`. A tail
// needs at least one directory, so a bare file name never matches a file at
// the root.
//
// Then every Mix layout for the application named below the last `lib/`,
// because an artifact built elsewhere does not record which one applies here:
// `lib/<app>/...` is the application's own repository, `deps/<app>/lib/<app>/...`
// is a dependency, and `deps/<app>/...` is the older vendored shape.
func sourceRebaseCandidates(recorded, projectRoot string) []string {
	if projectRoot == "" {
		return nil
	}
	slash := filepath.ToSlash(recorded)
	var candidates []string
	parts := strings.Split(strings.TrimPrefix(slash, "/"), "/")
	for i := 0; i+2 <= len(parts); i++ {
		candidates = append(candidates, filepath.Join(projectRoot, filepath.FromSlash(strings.Join(parts[i:], "/"))))
	}

	if idx := strings.LastIndex(slash, "/lib/"); idx >= 0 {
		appTail := strings.SplitN(slash[idx+len("/lib/"):], "/", 2)
		if len(appTail) == 2 && appTail[0] != "" && appTail[1] != "" {
			app, tail := appTail[0], filepath.FromSlash(appTail[1])
			candidates = append(candidates,
				filepath.Join(projectRoot, "lib", app, tail),
				filepath.Join(projectRoot, "deps", app, "lib", app, tail),
				filepath.Join(projectRoot, "deps", app, tail),
			)
		}
	}
	return slices.Compact(candidates)
}

// regularFileExists reports whether path names a readable regular file.
// Anything else is treated as absent, so an editor is never sent to a location
// it cannot open.
func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
