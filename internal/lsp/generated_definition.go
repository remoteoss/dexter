package lsp

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/remoteoss/dexter/internal/beam"
	"github.com/remoteoss/dexter/internal/parser"
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
	results, owner := s.generatedDefinitionResults(module)
	if len(functions) == 0 {
		return results, false
	}
	sources, ok := s.generatedDefinitionSourcesFor(module, beamPath)
	if !ok {
		return results, false
	}
	if result, found, ambiguous := sources.bestDescribedResult(results); found {
		src := s.currentSourceOf(result.FilePath, sources.beamMtime)
		if lines := recordedLinesIn(module, owner, result, src, sources, functions); len(lines) > 0 {
			return lines, true
		}
		return src.currentModuleResults(results, owner), false
	} else if ambiguous {
		return results, false
	}
	if owner != module {
		// The generator's file has no module that owns the declaration, so
		// its lines are not followed through edits there.
		if source := s.compiledSourceFile(sources.compileSource); source != "" {
			src := s.currentSourceOf(source, sources.beamMtime)
			if lines := recordedLinesIn(module, "", store.LookupResult{FilePath: source, Line: 1}, src, sources, functions); len(lines) > 0 {
				return lines, true
			}
		}
	}
	return results, false
}

// generatedModuleLocation is where a module that exists only as a BEAM is
// defined: the file it was compiled from, at the line the compiler recorded
// for the module, such as a Module.create call. It is empty when the BEAM does
// not record both, or the line is not in the file.
func (s *Server) generatedModuleLocation(module string) []store.LookupResult {
	if len(s.generatedFunctionsForModule(module)) == 0 {
		return nil
	}
	sources, ok := s.generatedDefinitionSourcesFor(module, "")
	if !ok || sources.debugInfo.ModuleLine < 1 {
		return nil
	}
	source := s.compiledSourceFile(sources.compileSource)
	if source == "" {
		return nil
	}
	if src := s.currentSourceOf(source, sources.beamMtime); src == nil || sources.debugInfo.ModuleLine > len(src.lines) {
		return nil
	}
	return []store.LookupResult{{Module: module, FilePath: source, Line: sources.debugInfo.ModuleLine, Kind: "module"}}
}

// recordedLinesIn returns a result for each distinct line in result's file
// that the compiled module records for functions, corrected for edits made
// since the compile, and after the module's own line.
//
// The module's line is taken from the current text when it has changed, so
// that it is compared after the correction: lines added above the module move
// both. A generator can give a def any line (`quote line: 99`), so a line past
// the end of the current text is dropped too.
//
// When nothing is left, which is the case for a def a @before_compile hook
// made at the module line, the module's body is searched for the call that
// declared the function: the one call whose first argument is its name as an
// atom (`later :deferred`). Only a single match counts.
func recordedLinesIn(module, owner string, result store.LookupResult, src *currentSource, sources generatedDefinitionSources, functions []beam.Function) []store.LookupResult {
	if src == nil {
		return nil
	}
	moduleLine := result.Line
	if line, ok := src.moduleLine(owner); ok && src.changed {
		moduleLine = line
	}
	// A line at or before the module's line as compiled says nothing the
	// module result does not, so it is dropped before any correction can move
	// it somewhere that only looks meaningful.
	compiledModuleLine := result.Line
	if owner == module && sources.debugInfo.ModuleLine > 0 {
		compiledModuleLine = sources.debugInfo.ModuleLine
	}
	lines := slices.DeleteFunc(generatedFunctionLines(module, result.FilePath, sources, functions), func(r store.LookupResult) bool { return r.Line <= compiledModuleLine })
	if src.changed {
		lines = src.correctDrift(module, owner, lines, sources, functions)
	}
	lines = slices.DeleteFunc(lines, func(r store.LookupResult) bool { return r.Line <= moduleLine || r.Line > len(src.lines) })
	if len(lines) > 0 || owner == "" {
		return lines
	}
	if line, ok := src.uniqueDeclaration(owner, functions); ok && line > moduleLine {
		return []store.LookupResult{{Module: module, FilePath: result.FilePath, Line: line, Kind: functions[0].Kind, Arity: functions[0].Arity}}
	}
	return nil
}

// generatedFunctionLines returns a result for each distinct line that the
// compiled module records for functions in file: one per clause when the
// clauses were made at different lines, as a DSL that adds a clause per call
// does.
//
// The Dbgi line is preferred because it also honors `@file`. The Docs chunk
// annotation is the fallback, because a module compiled without debug info
// still has it. It is often the line the def was expanded at, but not always:
// a generator can give the def one line and its docs another.
func generatedFunctionLines(module, file string, sources generatedDefinitionSources, functions []beam.Function) []store.LookupResult {
	useDebugInfo := len(sources.debugInfo.Lines) > 0
	var lines []store.LookupResult
	for _, function := range functions {
		key := beam.FunctionKey{Name: function.Name, Arity: function.Arity}
		recorded := []int{function.Line}
		if useDebugInfo {
			recorded = []int{sources.debugInfo.Lines[key]}
			if clauses := sources.debugInfo.Clauses[key]; len(clauses) > 0 {
				recorded = clauses
			}
		}
		for _, line := range recorded {
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
	}
	return lines
}

// currentSource is a file's text as it is now, in the open buffer if there is
// one and otherwise on disk, with what navigation derives from it computed on
// first use.
type currentSource struct {
	path string
	text string
	// lines is text split into lines, with the lines inside heredocs blanked.
	lines []string
	// changed reports that the text may differ from the text the BEAM was
	// compiled from: the buffer has unsaved changes, or the file was written
	// after the BEAM.
	changed bool

	owners []string
	defs   []parser.Definition
	parsed bool

	// compiled returns the debug info of another module's BEAM, so that the
	// other modules in this file can serve as anchors. It may be nil.
	compiled func(module string) (beam.DebugInfo, bool)
}

// currentSourceOf returns path's current text, or nil if it cannot be read.
// Only a buffer open in the editor counts: a file that another request cached
// is not unsaved edits.
func (s *Server) currentSourceOf(path string, beamMtime int64) *currentSource {
	open, isOpen := s.docs.GetIfOpen(string(pathToURI(path)))
	disk, err := os.ReadFile(path)
	src := &currentSource{path: path, compiled: func(module string) (beam.DebugInfo, bool) {
		sources, ok := s.generatedDefinitionSourcesFor(module, "")
		return sources.debugInfo, ok && len(sources.debugInfo.Anchors) > 0
	}}
	switch {
	case isOpen:
		src.text = open
		src.changed = err != nil || open != string(disk)
	case err == nil:
		src.text = string(disk)
	default:
		return nil
	}
	if stamp := statFileStamp(path); stamp.exists && newerBySecond(stamp.mtime, beamMtime) {
		src.changed = true
	}
	src.lines = blankHeredocs(strings.Split(src.text, "\n"))
	return src
}

// newerBySecond reports whether mtime is in a later second than beamMtime. A
// compiled BEAM's mtime has whole seconds while a source file's does not, so a
// file saved and compiled within one second would otherwise look newer than
// its BEAM. An edit in the same second as the compile counts as compiled, which
// only means its lines are not corrected.
func newerBySecond(mtime, beamMtime int64) bool {
	return mtime/int64(time.Second) > beamMtime/int64(time.Second)
}

// ownerAt returns the innermost module whose body holds the 1-based line.
func (src *currentSource) ownerAt(line int) string {
	if src.owners == nil {
		src.owners = moduleOwnersByLine([]byte(src.text), len(src.lines))
	}
	if line < 1 || line >= len(src.owners) {
		return ""
	}
	return src.owners[line]
}

// definitions returns the definitions the parser finds in the current text.
func (src *currentSource) definitions() []parser.Definition {
	if !src.parsed {
		src.parsed = true
		src.defs, _, _ = parser.ParseText(src.path, src.text)
	}
	return src.defs
}

// moduleLine returns the line of module's defmodule in the current text.
func (src *currentSource) moduleLine(module string) (int, bool) {
	if module == "" {
		return 0, false
	}
	for _, def := range src.definitions() {
		if def.Module == module && def.Function == "" {
			return def.Line, true
		}
	}
	return 0, false
}

// currentModuleResults moves the module results in this file to owner's line
// in the current text, which differs from the index while a buffer has unsaved
// changes.
func (src *currentSource) currentModuleResults(results []store.LookupResult, owner string) []store.LookupResult {
	if src == nil || !src.changed {
		return results
	}
	line, ok := src.moduleLine(owner)
	if !ok {
		return results
	}
	out := slices.Clone(results)
	for i := range out {
		if out[i].FilePath == src.path {
			out[i].Line = line
		}
	}
	return out
}

// correctDrift moves recorded lines to follow edits made since the compile.
//
// Dexter cannot compile the project, so it reads the current text instead,
// and only the body of owner, the module whose source holds the declarations:
// a sibling module in the same file never matches, and without an owner no
// line is moved. The signals, most exact first (see anchoredLine):
//
//   - The definitions the source still has on either side. The compiled
//     module records their old lines and the current text has their new ones;
//     when both moved by the same amount, so did this line.
//   - A call whose first argument is the function's name as an atom, which is
//     how a macro call names what it declares (`define :list_rooms`,
//     `field :email`). The nearest wins; the recorded line is kept when it
//     still declares the function, and two equally near matches count as none.
//     It is not used for a function whose clauses several calls made.
//   - A one-to-one match of generated defs to `def unquote(...)` lines.
//
// A line with neither signal is kept as recorded.
func (src *currentSource) correctDrift(module, owner string, results []store.LookupResult, sources generatedDefinitionSources, functions []beam.Function) []store.LookupResult {
	if owner == "" {
		return results
	}
	names := make(map[int]string, len(functions))
	for _, function := range functions {
		names[function.Arity] = function.Name
	}
	inOwner := func(line int) bool { return src.ownerAt(line) == owner }
	clauses := make(map[int]int, len(results))
	for _, result := range results {
		clauses[result.Arity]++
	}
	out := results[:0]
	for _, result := range results {
		byName := func() (int, bool) {
			// Clauses made by several calls come from calls that do not spell
			// the function's name (`get "/a"` makes a `match/2` clause), so a
			// call that does, such as `plug :match`, is not one of them.
			if clauses[result.Arity] > 1 {
				return 0, false
			}
			return nearestDeclaringLine(src.lines, result.Line, names[result.Arity], inOwner)
		}
		line := result.Line
		if moved, ok := src.anchoredLine(module, owner, result.Line, sources.debugInfo, byName); ok {
			line = moved
		}
		if slices.ContainsFunc(out, func(r store.LookupResult) bool { return r.Line == line }) {
			continue
		}
		result.Line = line
		out = append(out, result)
	}
	return out
}

// anchoredLine places recorded in the current text by the nearest
// definitions above and below it that a compiled module and the current text
// both have: the defs and the module lines of every module in the file whose
// BEAM was compiled from it.
//
// When both anchors moved by the same amount, the line moved with them.
// Otherwise byName, if given, may place it by a call that declares the
// function. Failing that, the edit was between the anchors, and the line is
// placed only when every generated def between them can be matched to a line
// in owner that defines a function with an unquoted name
// (`def unquote(name)()`), one to one and in order: the shape a comprehension
// leaves. Otherwise it is not placed.
func (src *currentSource) anchoredLine(module, owner string, recorded int, info beam.DebugInfo, byName func() (int, bool)) (int, bool) {
	// Lines move for the whole file, so every module defined in it can serve
	// as an anchor, not only this one: a nested module's line counts as much
	// as a def. Another module's BEAM is used only when it was compiled from
	// this file.
	type moduleKey struct {
		module string
		key    beam.FunctionKey
	}
	current := make(map[moduleKey]int)
	moduleLines := make(map[string]int)
	for _, def := range src.definitions() {
		if def.Function == "" {
			if _, seen := moduleLines[def.Module]; !seen {
				moduleLines[def.Module] = def.Line
			}
			continue
		}
		key := moduleKey{def.Module, beam.FunctionKey{Name: def.Function, Arity: def.Arity}}
		if line, seen := current[key]; !seen || def.Line < line {
			current[key] = def.Line
		}
	}
	compiled := map[string]beam.DebugInfo{module: info}
	if src.compiled != nil {
		for other := range moduleLines {
			if other == module {
				continue
			}
			if otherInfo, ok := src.compiled(other); ok && recordedPathMatch(otherInfo.File, otherInfo.RelativeFile, src.path) > 0 {
				compiled[other] = otherInfo
			}
		}
	}

	type anchor struct{ old, now int }
	var above, below *anchor
	consider := func(old, now int) {
		switch {
		case old < recorded && (above == nil || old > above.old):
			above = &anchor{old, now}
		case old > recorded && (below == nil || old < below.old):
			below = &anchor{old, now}
		}
	}
	for name, moduleInfo := range compiled {
		for key, old := range moduleInfo.Anchors {
			if now, ok := current[moduleKey{name, key}]; ok {
				consider(old, now)
			}
		}
		if now, ok := moduleLines[name]; ok && moduleInfo.ModuleLine > 0 {
			consider(moduleInfo.ModuleLine, now)
		}
	}
	if above != nil && below != nil && above.now-above.old == below.now-below.old {
		return recorded + above.now - above.old, true
	}
	if byName != nil {
		if line, ok := byName(); ok {
			return line, true
		}
	}

	// The edit was between the anchors. A def a comprehension generates has
	// no name to search for, but its line is a `def unquote(...)`: if the
	// compiled module's generated defs between the anchors have as many
	// distinct lines as the current text has such defs there, they are the
	// same defs in the same order.
	oldFrom, oldTo, newFrom, newTo := 1, int(^uint(0)>>1), 1, len(src.lines)
	if above != nil {
		oldFrom, newFrom = above.old+1, above.now+1
	}
	if below != nil {
		oldTo, newTo = below.old-1, below.now-1
	}
	var generated []int
	for key, old := range info.Anchors {
		if _, inSource := current[moduleKey{module, key}]; !inSource && old >= oldFrom && old <= oldTo && !slices.Contains(generated, old) {
			generated = append(generated, old)
		}
	}
	var unquoted []int
	for line := newFrom; line <= newTo; line++ {
		if lineDefinesUnquotedName(src.lines[line-1]) && src.ownerAt(line) == owner {
			unquoted = append(unquoted, line)
		}
	}
	if len(generated) == 0 || len(generated) != len(unquoted) {
		return 0, false
	}
	slices.Sort(generated)
	index := slices.Index(generated, recorded)
	if index < 0 {
		return 0, false
	}
	return unquoted[index], true
}

// lineDefinesUnquotedName reports whether text starts a def, defp, defmacro,
// or defmacrop whose name is unquoted.
func lineDefinesUnquotedName(text string) bool {
	rest := strings.TrimLeft(text, " \t")
	for _, keyword := range []string{"def ", "defp ", "defmacro ", "defmacrop "} {
		if after, ok := strings.CutPrefix(rest, keyword); ok {
			return strings.HasPrefix(strings.TrimLeft(after, " \t"), "unquote(")
		}
	}
	return false
}

// uniqueDeclaration returns the one line in owner's body that declares one of
// functions by name, if exactly one does.
func (src *currentSource) uniqueDeclaration(owner string, functions []beam.Function) (int, bool) {
	names := declarationNames(functions[0].Name)
	found := 0
	for line := 1; line <= len(src.lines); line++ {
		if !lineDeclaresAtom(src.lines[line-1], names) || src.ownerAt(line) != owner {
			continue
		}
		if found != 0 {
			return 0, false
		}
		found = line
	}
	return found, found != 0
}

// declarationNames is the names a declaring call can spell for function: the
// name itself, and without a trailing `!` or `?`, as `define :get` makes
// `get!` and `flag :active` makes `active?`.
func declarationNames(function string) []string {
	names := []string{function}
	if base := strings.TrimRight(function, "!?"); base != function && base != "" {
		names = append(names, base)
	}
	return names
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
// declares function by name and that accept allows. found is false, and the
// line is recorded, when no line does or two are equally near. A nil accept
// allows every line.
func nearestDeclaringLine(lines []string, recorded int, function string, accept func(line int) bool) (line int, found bool) {
	if function == "" || recorded < 1 {
		return recorded, false
	}
	names := declarationNames(function)
	declares := func(line int) bool {
		return line >= 1 && line <= len(lines) && (accept == nil || accept(line)) && lineDeclaresAtom(lines[line-1], names)
	}
	if declares(recorded) {
		return recorded, true
	}
	for distance := 1; recorded-distance >= 1 || recorded+distance <= len(lines); distance++ {
		above, below := declares(recorded-distance), declares(recorded+distance)
		switch {
		case above && below:
			return recorded, false
		case above:
			return recorded - distance, true
		case below:
			return recorded + distance, true
		}
	}
	return recorded, false
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

// bestDescribedResult returns the result whose file the recorded lines belong
// to. Usually there is one row, but a module name defined in two files (two
// umbrella apps, say) has two, and a moved project matches neither exactly.
// The row that matches the recorded path most specifically wins. If two match
// equally, ambiguous is set, because a line from one file means nothing in the
// other.
func (sources generatedDefinitionSources) bestDescribedResult(results []store.LookupResult) (best store.LookupResult, found, ambiguous bool) {
	bestScore := 0
	for _, result := range results {
		score := sources.describe(result.FilePath)
		switch {
		case score == 0:
		case score > bestScore:
			best, bestScore, ambiguous = result, score, false
		case score == bestScore && result.FilePath != best.FilePath:
			ambiguous = true
		}
	}
	if ambiguous {
		return store.LookupResult{}, false, true
	}
	return best, bestScore > 0, false
}

// describe scores how specifically the recorded source names path: zero when
// it does not, highest for the same absolute path. When the module has debug
// info, its file names are the test; otherwise the compile info's.
func (sources generatedDefinitionSources) describe(path string) int {
	if len(sources.debugInfo.Lines) > 0 {
		return recordedPathMatch(sources.debugInfo.File, sources.debugInfo.RelativeFile, path)
	}
	return recordedPathMatch(sources.compileSource, "", path)
}

// exactPathMatch is the score for a recorded absolute path equal to the path.
const exactPathMatch = 1 << 20

// recordedPathMatch scores how specifically a recorded source path names path.
// The same absolute path is best. Otherwise the two are compared from their
// ends, which covers a project that has moved, was copied, or is reached
// through a symlink since it was compiled: each shared trailing component
// counts, and at least a directory and the file name must be shared. relative,
// the path relative to the compiler's working directory, counts its own
// components when it is a suffix of path.
func recordedPathMatch(recorded, relative, path string) int {
	if recorded != "" && recorded == path {
		return exactPathMatch
	}
	score := 0
	if recorded != "" {
		if shared := sharedTrailingComponents(recorded, path); shared >= 2 {
			score = shared
		}
	}
	if relative != "" && !filepath.IsAbs(relative) {
		slash := filepath.ToSlash(relative)
		if strings.HasSuffix(filepath.ToSlash(path), "/"+slash) {
			score = max(score, strings.Count(slash, "/")+1)
		}
	}
	return score
}

// sharedTrailingComponents counts the path components a and b share at their
// ends.
func sharedTrailingComponents(a, b string) int {
	as := strings.Split(filepath.ToSlash(a), "/")
	bs := strings.Split(filepath.ToSlash(b), "/")
	shared := 0
	for shared < len(as) && shared < len(bs) {
		component := as[len(as)-1-shared]
		if component == "" || component != bs[len(bs)-1-shared] {
			break
		}
		shared++
	}
	return shared
}

// moduleOwnersByLine returns, for each 1-based line of source, the innermost
// module whose body holds it, found with the tokenizer so that strings,
// heredocs and comments do not count. Index 0 is unused.
func moduleOwnersByLine(source []byte, lineCount int) []string {
	owners := make([]string, lineCount+1)
	tokens := parser.Tokenize(source)
	type frame struct {
		name  string
		depth int
	}
	var stack []frame
	depth := 0
	current := func() string {
		if len(stack) == 0 {
			return ""
		}
		return stack[len(stack)-1].name
	}
	filled := 0
	fill := func(through int) {
		for filled < through && filled < lineCount {
			filled++
			owners[filled] = current()
		}
	}
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		// A line belongs to the module that is open at its first token.
		fill(token.Line)
		switch token.Kind {
		case parser.TokDo, parser.TokFn:
			parser.TrackBlockDepth(token.Kind, &depth)
		case parser.TokEnd:
			before := depth
			parser.TrackBlockDepth(token.Kind, &depth)
			if len(stack) > 0 && stack[len(stack)-1].depth == before {
				stack = stack[:len(stack)-1]
			}
		case parser.TokDefmodule, parser.TokDefprotocol, parser.TokDefimpl:
			name, next, hasDo := tokParseModuleDef(source, tokens, i+1, current())
			if name == "" {
				continue
			}
			if hasDo {
				depth++
				stack = append(stack, frame{name: name, depth: depth})
			}
			i = next - 1
		}
	}
	fill(lineCount)
	return owners
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
