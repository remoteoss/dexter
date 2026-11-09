package lsp

import (
	"fmt"
	"strings"

	"github.com/remoteoss/dexter/internal/parser"
)

// The kinds of a function site. A rename edits every site; references list
// the sites that are not declarations (see siteIsDeclaration), and the
// declarations only when the request includes them.
const (
	siteDefinition     = "definition"     // the def, defp, defmacro, or defdelegate line of the function
	siteSpec           = "spec"           // a @spec or @callback line of the function
	siteDefoverridable = "defoverridable" // a key in defoverridable
	siteCallback       = "callback"       // a @callback or @optional_callbacks entry of a behaviour
	siteImportOnly     = "import"         // a key in the only: list of an import
	siteImportExcept   = "except"         // a key in the except: list of an import
	siteCall           = "call"           // a call or a capture
	siteTypespec       = "typespec"       // the name in a typespec
	siteDelegate       = "defdelegate"    // a defdelegate that forwards to the function (`as:`)
)

// siteIsDeclaration reports whether a site of this kind declares the
// function and does not use it. An except: key is neither: the compiler does
// not link it to the function, so it is not a reference.
func siteIsDeclaration(kind string) bool {
	switch kind {
	case siteDefinition, siteSpec, siteDefoverridable, siteCallback, siteImportExcept:
		return true
	}
	return false
}

type siteKey struct {
	filePath string
	line     int
}

// siteCollector collects the sites of a function: the lines that a rename
// edits and that references list. One collector serves one request; each
// file is read and tokenized at most once.
type siteCollector struct {
	s       *Server
	sites   []renameSite
	seen    map[siteKey]int
	parsed  map[string]*parsedSource // nil: the file cannot be read
	skipped map[string]bool          // read, and cannot hold a site (sourceWith)
	files   map[string]*importScanFile
}

func newSiteCollector(s *Server) *siteCollector {
	return &siteCollector{s: s, seen: make(map[siteKey]int), parsed: make(map[string]*parsedSource),
		skipped: make(map[string]bool), files: make(map[string]*importScanFile)}
}

// isLibraryFile reports whether path is in the stdlib or in deps.
func (c *siteCollector) isLibraryFile(path string) bool {
	return c.s.isStdlibPath(path) || c.s.isDepsFile(path)
}

// add adds a site. A line can be a site for more than one reason; the flags
// are joined, and a use wins over a declaration for the kind.
func (c *siteCollector) add(site renameSite) {
	if c.isLibraryFile(site.filePath) {
		return
	}
	k := siteKey{site.filePath, site.line}
	if i, ok := c.seen[k]; ok {
		have := &c.sites[i]
		have.includeKeyword = have.includeKeyword || site.includeKeyword
		have.fromIndex = have.fromIndex || site.fromIndex
		have.bareOK = have.bareOK || site.bareOK
		have.dynamicOK = have.dynamicOK || site.dynamicOK
		if have.localArities == nil {
			// The scan of the line knows the module's own arities; an index
			// row of the line does not.
			have.localArities = site.localArities
		}
		if have.ownArities == nil {
			have.ownArities = site.ownArities
		}
		if siteIsDeclaration(have.kind) && !siteIsDeclaration(site.kind) {
			have.kind = site.kind
		}
		return
	}
	c.seen[k] = len(c.sites)
	c.sites = append(c.sites, site)
}

// source returns the text and tokens of path, or nil when it cannot be read.
func (c *siteCollector) source(path string) *parsedSource {
	if p, ok := c.parsed[path]; ok {
		return p
	}
	var p *parsedSource
	if text, _, ok := c.s.readFileText(path); ok {
		src := []byte(text)
		res := parser.TokenizeFull(src)
		p = &parsedSource{text: text, src: src, tokens: res.Tokens, interp: res.Interp, lineStarts: res.LineStarts}
	}
	c.parsed[path] = p
	return p
}

// sourceWith returns the source of path when its text can hold a site of
// name in a file that imports the module (mayHoldImportSite), and nil
// otherwise, without tokenizing a file that cannot. readable is false when
// the file cannot be read.
func (c *siteCollector) sourceWith(path, name string) (p *parsedSource, readable bool) {
	if p, ok := c.parsed[path]; ok {
		if p == nil {
			return nil, false
		}
		if !mayHoldImportSite(p.text, name) {
			return nil, true
		}
		return p, true
	}
	if c.skipped[path] {
		return nil, true
	}
	text, _, ok := c.s.readFileText(path)
	if !ok {
		c.parsed[path] = nil
		return nil, false
	}
	if !mayHoldImportSite(text, name) {
		return nil, true
	}
	return c.source(path), true
}

// prefetch reads the files in parallel and tokenizes the ones that can hold
// a site of name (see sourceWith), so that the scan of many importing files
// does not read and tokenize them one at a time.
func (c *siteCollector) prefetch(paths []string, name string) {
	var todo []string
	for _, p := range paths {
		if _, done := c.parsed[p]; !done && !c.skipped[p] {
			todo = append(todo, p)
		}
	}
	if len(todo) < 2 {
		return
	}
	results := make([]*parsedSource, len(todo))
	readable := make([]bool, len(todo))
	failed := parallel(len(todo), func(i int) error {
		text, _, ok := c.s.readFileText(todo[i])
		if !ok {
			return nil
		}
		if !mayHoldImportSite(text, name) {
			readable[i] = true
			return nil
		}
		src := []byte(text)
		res := parser.TokenizeFull(src)
		results[i] = &parsedSource{text: text, src: src, tokens: res.Tokens, interp: res.Interp, lineStarts: res.LineStarts}
		readable[i] = true
		return nil
	})
	for i, err := range failed {
		if err != nil {
			// A file whose read or tokens panicked counts as unread: the
			// rename reports it instead of leaving its sites out.
			readable[i], results[i] = false, nil
		}
	}
	for i, path := range todo {
		switch {
		case !readable[i]:
			c.parsed[path] = nil
		case results[i] == nil:
			c.skipped[path] = true
		default:
			c.parsed[path] = results[i]
		}
	}
}

// mayHoldImportSite reports whether text can hold a site of name in a file
// that imports the module: a call or a capture (`name(`, `name arg`,
// `&name/1`, `|> name`, with comment lines between `|>` and the name), or a keyword
// key (`name:`, in an only: or except: list). It is a text check that
// can say yes for a match in a string or a comment; the tokens decide then.
func mayHoldImportSite(text, name string) bool {
	for from := 0; from < len(text); {
		j := strings.Index(text[from:], name)
		if j < 0 {
			return false
		}
		start := from + j
		end := start + len(name)
		from = start + 1
		if start > 0 && (isIdentByte(text[start-1]) || text[start-1] == '.') {
			continue
		}
		if end < len(text) && (isIdentByte(text[end]) || text[end] == '?' || text[end] == '!') {
			continue
		}
		e := end
		for e < len(text) && (text[e] == ' ' || text[e] == '\t') {
			e++
		}
		if e < len(text) && (text[e] == '(' || text[e] == ':') {
			return true
		}
		// A call without parentheses: `name arg`, with an argument on the
		// same line after a space (see parser.NoParenArgCount).
		if e > end && e < len(text) && mayStartNoParenArg(text[e]) {
			return true
		}
		if b := prevCodeByte(text, start); b >= 0 && (text[b] == '&' || (text[b] == '>' && b > 0 && text[b-1] == '|')) {
			return true
		}
	}
	return false
}

// prevCodeByte returns the index of the last byte before pos that is not
// white space and not on a comment line, or -1.
func prevCodeByte(text string, pos int) int {
	b := pos - 1
	for {
		for b >= 0 && (text[b] == ' ' || text[b] == '\t' || text[b] == '\n' || text[b] == '\r') {
			b--
		}
		if b < 0 {
			return -1
		}
		lineStart := strings.LastIndexByte(text[:b+1], '\n') + 1
		if strings.HasPrefix(strings.TrimLeft(text[lineStart:b+1], " \t"), "#") {
			b = lineStart - 1
			continue
		}
		return b
	}
}

// mayStartNoParenArg reports whether c can start the first argument of a
// call without parentheses: a name, a module, a number, a string, a
// charlist, a sigil, a character, an atom, a list, a tuple, a map or
// struct, or an attribute.
func mayStartNoParenArg(c byte) bool {
	switch c {
	case '"', '\'', '~', '?', ':', '[', '{', '%', '@':
		return true
	}
	return isIdentByte(c)
}

func isIdentByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// kindAt is siteCall, or siteTypespec when the line of p is in a typespec.
func kindAt(p *parsedSource, line int) string {
	if p != nil && p.inTypespec(line) {
		return siteTypespec
	}
	return siteCall
}

// collect adds the sites of module.functionName: its definitions, the
// references the index has, the calls through a module whose __using__
// imports the module, the __MODULE__ calls, @spec, @callback, and
// defoverridable lines, the bare calls in the module, and the import keys and
// bare calls in the files that import it. injectors, when not nil, are the
// modules whose __using__ imports the module; nil looks them up.
func (c *siteCollector) collect(module, functionName string, injectors []string) error {
	s := c.s
	defResults, err := s.store.LookupFunction(module, functionName)
	if err != nil {
		return fmt.Errorf("cannot rename %s.%s: the index query failed (%v); nothing was renamed", module, functionName, err)
	}
	defFilePaths := make(map[string]bool)
	ownArities := make(map[int]bool)
	for _, r := range defResults {
		switch r.Kind {
		case "type", "typep", "opaque":
			// A type of the same name (`@type run`) is not the function.
			continue
		}
		defFilePaths[r.FilePath] = true
		ownArities[r.Arity] = true
		c.add(renameSite{module: module, filePath: r.FilePath, line: r.Line, fromIndex: true, bareOK: true, kind: siteDefinition})
	}
	// An unqualified call on a line of a file that imports the module is the
	// module's function, except in a module that defines a function of the
	// same name itself (the importing module, or another module of the
	// file). The module at the line comes from the file's tokens, with
	// do/end matching; the modules that define the name come from one
	// query (see the import scan below).
	// inModule reports whether a line of a definition file is inside the
	// module itself. A nested or a sibling module does not see the module's
	// functions unqualified, whatever it defines. A file that cannot be
	// tokenized keeps the line.
	inModule := func(path string, line int) bool {
		if !defFilePaths[path] {
			return false
		}
		p := c.source(path)
		if p == nil {
			return true
		}
		enclosing := p.moduleAt(line)
		return enclosing == "" || enclosing == module
	}

	// Files that import the module: an unqualified call there can be the
	// imported function. A failed query leaves the set empty, which only
	// keeps unqualified calls on lines with another call of the same name.
	importRefs, _ := s.store.LookupReferences(module, "")
	importFiles := make(map[string]bool)
	for _, r := range importRefs {
		if r.Kind == "import" {
			importFiles[r.FilePath] = true
		}
	}

	// Direct reference sites (calls, imports — skip alias/use which are module-level)
	refResults, err := s.store.LookupReferences(module, functionName)
	if err != nil {
		return fmt.Errorf("cannot rename %s.%s: the index query failed (%v); nothing was renamed", module, functionName, err)
	}
	for _, r := range refResults {
		if r.Kind == "alias" || r.Kind == "use" {
			continue
		}
		c.add(renameSite{module: module, filePath: r.FilePath, line: r.Line, fromIndex: true,
			bareOK: importFiles[r.FilePath] || inModule(r.FilePath, r.Line), kind: r.Kind})
	}

	// Transitive refs via __using__ chains: the __using__ block imports the
	// module, so unqualified calls there are its function.
	// The files that `use` such a module have the import too; the index
	// does not list their unqualified calls, so they are scanned below.
	if injectors == nil {
		injectors = s.findModulesWhoseUsingImports(module)
	}
	usingImporters := make(map[string]bool)
	for _, mod := range injectors {
		transitive, err := s.store.LookupReferences(mod, functionName)
		if err == nil {
			for _, r := range transitive {
				if r.Kind == "alias" || r.Kind == "use" {
					continue
				}
				c.add(renameSite{module: module, filePath: r.FilePath, line: r.Line, fromIndex: true, bareOK: true, kind: r.Kind})
			}
		}
		if users, err := s.store.LookupReferences(mod, ""); err == nil {
			for _, r := range users {
				if r.Kind == "use" {
					usingImporters[r.FilePath] = true
				}
			}
		}
	}

	// Scan definition files for @spec/@callback lines and bare intra-module calls
	// (none of these are indexed in the store). A definition file that cannot
	// be read is caught by planTextEdits, because its definitions are sites.
	specPrefix := "@spec " + functionName
	callbackPrefix := "@callback " + functionName
	for filePath := range defFilePaths {
		p := c.source(filePath)
		if p == nil {
			continue
		}
		// __MODULE__.function calls, which the index does not list; only in
		// code (also in #{}), never in a string or a comment. siteColumns
		// resolves __MODULE__.
		for _, line := range selfCallLines(p, functionName) {
			c.add(renameSite{module: module, filePath: filePath, line: line, kind: siteCall})
		}
		// `defoverridable name: 2` keeps the name of the function.
		for _, line := range keywordListLines(p, "defoverridable", functionName) {
			c.add(renameSite{module: module, filePath: filePath, line: line, includeKeyword: true, kind: siteDefoverridable})
		}
		// @spec and @callback lines
		for i, line := range strings.Split(p.text, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, specPrefix) {
				rest := trimmed[len(specPrefix):]
				if len(rest) == 0 || rest[0] == '(' || rest[0] == ' ' || rest[0] == '\t' {
					c.add(renameSite{module: module, filePath: filePath, line: i + 1, bareOK: inModule(filePath, i+1), kind: siteSpec})
				}
			}
			if strings.HasPrefix(trimmed, callbackPrefix) {
				rest := trimmed[len(callbackPrefix):]
				if len(rest) == 0 || rest[0] == '(' || rest[0] == ' ' || rest[0] == '\t' {
					c.add(renameSite{module: module, filePath: filePath, line: i + 1, bareOK: inModule(filePath, i+1), kind: siteSpec})
				}
			}
		}
		// Bare calls and captures: functionName(...), |> functionName,
		// &functionName/1. Only the ones of the module: a nested module in
		// the same file can have its own function of the same name.
		for _, lineNum := range findBareCallLines(p.src, p.tokens, p.interp, functionName) {
			if inModule(filePath, lineNum) {
				c.add(renameSite{module: module, filePath: filePath, line: lineNum, bareOK: true, kind: kindAt(p, lineNum), ownArities: ownArities})
			}
		}
	}

	// Scan the files that import the module: `import Module, only:
	// [functionName: N]` lines, and the unqualified calls and captures,
	// which the index does not list. An import whose only: list leaves the
	// function out, or whose except: list names it, does not bring it in. A
	// file whose text does not spell the name holds neither, and is not
	// tokenized. Files in the stdlib and in deps are not scanned: a rename
	// cannot edit them, and references list only their indexed sites.
	isMacro := false
	for _, r := range defResults {
		if r.Kind == "defmacro" || r.Kind == "defmacrop" {
			isMacro = true
		}
	}
	for path := range importFiles {
		if c.isLibraryFile(path) {
			delete(importFiles, path)
		}
	}
	for path := range usingImporters {
		if c.isLibraryFile(path) {
			delete(usingImporters, path)
		}
	}
	scanPaths := make([]string, 0, len(importFiles)+len(usingImporters))
	for filePath := range importFiles {
		scanPaths = append(scanPaths, filePath)
	}
	for filePath := range usingImporters {
		scanPaths = append(scanPaths, filePath)
	}
	c.prefetch(scanPaths, functionName)
	importScan := make(map[string]*importScanFile)
	for _, r := range importRefs {
		if r.Kind != "import" || !importFiles[r.FilePath] {
			continue
		}
		f, readable := c.importFile(r.FilePath, functionName)
		if !readable {
			// The index lists the file and it cannot be read: planTextEdits
			// reports the index as behind instead of skipping the file.
			c.add(renameSite{module: module, filePath: r.FilePath, line: r.Line, includeKeyword: true, fromIndex: true, kind: "import"})
			continue
		}
		if f == nil {
			continue
		}
		opts := f.importAt(r.Line)
		// The function's key in an only: or except: list is renamed with
		// it, on whatever line of the list it stands.
		kind := siteImportOnly
		if opts.kind == "except" {
			kind = siteImportExcept
		}
		for _, line := range opts.names[functionName] {
			c.add(renameSite{module: module, filePath: r.FilePath, line: line, includeKeyword: true, kind: kind})
		}
		if opts.imports(functionName, isMacro) {
			importScan[r.FilePath] = f
		}
	}
	for filePath := range usingImporters {
		if _, ok := importScan[filePath]; ok {
			continue
		}
		if f, _ := c.importFile(filePath, functionName); f != nil {
			importScan[filePath] = f
		}
	}
	type bareHit struct {
		path string
		bareCallHit
	}
	var hits []bareHit
	enclosingSet := map[string]bool{}
	for filePath, f := range importScan {
		for _, h := range f.callsOf(functionName) {
			hits = append(hits, bareHit{filePath, h})
			if h.module != "" && h.module != module {
				enclosingSet[h.module] = true
			}
		}
	}
	// A module that defines a function of the name itself calls its own
	// function with those arities: on its lines, only the calls of another
	// arity are the imported function (renameSite.localArities).
	var local map[string]map[int]bool
	if len(enclosingSet) > 0 {
		mods := make([]string, 0, len(enclosingSet))
		for m := range enclosingSet {
			mods = append(mods, m)
		}
		// A failed query keeps every call, as an unreadable file does.
		local, _ = s.store.ModuleArities(functionName, mods)
	}
	for _, h := range hits {
		kind := siteCall
		if h.typespec {
			kind = siteTypespec
		}
		site := renameSite{module: module, filePath: h.path, line: h.line, bareOK: true, kind: kind}
		if h.module != "" && h.module != module {
			site.localArities = local[h.module]
		}
		c.add(site)
	}
	return nil
}

// ownBareCallLines is findBareCallLines for the file of a module that
// defines name with the arities own: a line whose calls of name all have a
// known arity that is not one of them calls a function that the module
// imports, and is left out. A call whose arity cannot be read stays.
func ownBareCallLines(p *parsedSource, name string, own map[int]bool) []int {
	lines := findBareCallLines(p.src, p.tokens, p.interp, name)
	if len(own) == 0 || len(lines) == 0 {
		return lines
	}
	foreign := make(map[int]bool)
	mine := make(map[int]bool)
	for i, t := range p.tokens {
		if t.Kind != parser.TokIdent || parser.TokenText(p.src, t) != name {
			continue
		}
		if i > 0 && p.tokens[i-1].Kind == parser.TokDot {
			continue
		}
		if lo, hi, known := tokenArity(p.src, p.tokens, i); known && !anyArityIn(own, lo, hi) {
			foreign[t.Line] = true
		} else {
			mine[t.Line] = true
		}
	}
	out := lines[:0:0]
	for _, l := range lines {
		if mine[l] || !foreign[l] {
			out = append(out, l)
		}
	}
	return out
}
