package lsp

// bareCallHit is one line with an unqualified call or capture of a name.
type bareCallHit struct {
	line     int
	module   string // the module at the line (extractEnclosingModuleFromTokens)
	typespec bool   // the line is in a typespec
}

// importScanFile is what the site collector needs from a file that imports
// a module: the options of its import lines, and its bare calls by name.
type importScanFile struct {
	p       *parsedSource
	imports map[int]importOptions
	calls   map[string][]bareCallHit
}

func (f *importScanFile) importAt(line int) importOptions {
	if o, ok := f.imports[line]; ok {
		return o
	}
	if f.p == nil {
		return importOptions{names: map[string][]int{}}
	}
	o := parseImportOptions(f.p, line)
	f.imports[line] = o
	return o
}

func (f *importScanFile) callsOf(name string) []bareCallHit {
	if h, ok := f.calls[name]; ok || f.p == nil {
		return h
	}
	var hits []bareCallHit
	for _, line := range findBareCallLines(f.p.src, f.p.tokens, f.p.interp, name) {
		hits = append(hits, bareCallHit{line: line, module: f.p.moduleAt(line), typespec: f.p.inTypespec(line)})
	}
	f.calls[name] = hits
	return hits
}

// importFile returns the importScanFile of path. f is nil when the file
// cannot hold a site of name, or when it cannot be read (readable is false
// then).
func (c *siteCollector) importFile(path, name string) (f *importScanFile, readable bool) {
	p, readable := c.sourceWith(path, name)
	if p == nil {
		return nil, readable
	}
	if f, ok := c.files[path]; ok && f != nil {
		return f, true
	}
	f = &importScanFile{p: p, imports: make(map[int]importOptions), calls: make(map[string][]bareCallHit)}
	c.files[path] = f
	return f, true
}
