package lsp

import (
	"fmt"
	"sort"
	"strings"
)

// This file computes renames as pure plans (the full new content of every
// affected file) without touching disk. The LSP Rename handler and the MCP
// dexter_rename_symbol tool share the same site collection and edit
// application; only the final step differs (LSP writes/returns TextEdits, MCP
// renders a diff). TestFunctionRenamePlan_MatchesApply and
// TestModuleRenamePlan_MatchesApply keep the two paths in lockstep.

// FileChange is one file's part of a RenamePlan.
type FileChange struct {
	Path    string // current path
	NewPath string // non-empty when the file moves (conventional module rename)
	OldText string
	NewText string
	Open    bool // file is open in the editor (attached mode only)
}

// RenamePlan is the complete effect of a rename, sorted by path.
type RenamePlan struct {
	Changes []FileChange
}

// FunctionRenamePlan computes the plan for renaming module.functionName to
// newName across the workspace. It performs the same validation and collects
// the same edit sites as the LSP rename, but writes nothing.
func (s *Server) FunctionRenamePlan(module, functionName, newName string) (*RenamePlan, error) {
	if !isValidFunctionName(newName) {
		return nil, fmt.Errorf("invalid function name %q: must match [a-z_][a-z0-9_?!]*", newName)
	}
	defs, err := s.store.LookupFunction(module, functionName)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("function %s.%s not found in the index", module, functionName)
	}
	if existing, err := s.store.LookupFunction(module, newName); err == nil && len(existing) > 0 {
		return nil, fmt.Errorf("function %s.%s already exists", module, newName)
	}

	sites := s.collectFunctionRenameSites(module, functionName)

	sitesByFile := make(map[string][]renameSite, len(sites))
	for _, site := range sites {
		sitesByFile[site.filePath] = append(sitesByFile[site.filePath], site)
	}

	changes := make(map[string]*FileChange, len(sitesByFile))
	for fp, fileSites := range sitesByFile {
		text, open, ok := s.ReadFileText(fp)
		if !ok {
			continue
		}
		lines := strings.Split(text, "\n")
		newText := strings.Join(applyTokenEditsToLines(lines, fileSites, functionName, newName), "\n")
		if newText == text {
			continue
		}
		changes[fp] = &FileChange{Path: fp, OldText: text, NewText: newText, Open: open}
	}

	// Delegate `as:` updates apply on top of the token edits, mirroring the
	// LSP path where they run after buildTextEdits has written files.
	readLines := func(path string) ([]string, bool, bool) {
		if fc, ok := changes[path]; ok {
			return strings.Split(fc.NewText, "\n"), fc.Open, true
		}
		text, open, ok := s.ReadFileText(path)
		if !ok {
			return nil, false, false
		}
		return strings.Split(text, "\n"), open, true
	}
	for _, de := range s.delegateSpanEdits(module, functionName, newName, readLines) {
		newText := strings.Join(spliceLines(de.fileLines, de.spanStart, de.spanEnd, de.updated), "\n")
		if fc, ok := changes[de.filePath]; ok {
			fc.NewText = newText
			continue
		}
		oldText, _, ok := s.ReadFileText(de.filePath)
		if !ok {
			continue
		}
		changes[de.filePath] = &FileChange{Path: de.filePath, OldText: oldText, NewText: newText, Open: de.open}
	}

	return sortedPlan(changes), nil
}

// ModuleRenamePlan computes the plan for renaming oldModule (and its
// submodules) to newModule, including conventional file moves. It performs the
// same collision checks as the LSP rename, but writes nothing.
func (s *Server) ModuleRenamePlan(oldModule, newModule string) (*RenamePlan, error) {
	if !isValidModuleName(newModule) {
		return nil, fmt.Errorf("invalid module name %q: must be CamelCase segments separated by dots", newModule)
	}
	if defs, err := s.store.LookupModule(oldModule); err != nil || len(defs) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("module %s not found in the index", oldModule)
	}

	mr := s.buildModuleRename(oldModule, newModule)
	if err := mr.checkCollisions(); err != nil {
		return nil, err
	}
	mr.collectSites()
	fileCache := mr.readFiles()
	return mr.plan(fileCache), nil
}

// plan renders the moduleRename as a RenamePlan: for every file with edit
// sites, the fully-updated content, plus the conventional new path for files
// that move. It reuses the exact edit application (applyEditsToLines) and path
// computation (conventionalNewPath) the apply path uses.
func (mr *moduleRename) plan(fileCache map[string]moduleFileInfo) *RenamePlan {
	newPaths := make(map[string]string)
	for _, r := range mr.allModuleDefs {
		if _, ok := mr.moduleRenames[r.Module]; !ok {
			continue
		}
		if newPath, follows := mr.conventionalNewPath(r); follows {
			newPaths[r.FilePath] = newPath
		}
	}

	changes := make(map[string]*FileChange, len(mr.sitesByFile))
	for fp, sites := range mr.sitesByFile {
		fi, ok := fileCache[fp]
		if !ok {
			continue
		}
		oldText := strings.Join(fi.lines, "\n")
		newText := strings.Join(mr.applyEditsToLines(fi.lines, sites), "\n")
		newPath := newPaths[fp]
		if newText == oldText && newPath == "" {
			continue
		}
		changes[fp] = &FileChange{Path: fp, NewPath: newPath, OldText: oldText, NewText: newText, Open: fi.open}
	}
	return sortedPlan(changes)
}

func sortedPlan(changes map[string]*FileChange) *RenamePlan {
	plan := &RenamePlan{Changes: make([]FileChange, 0, len(changes))}
	for _, fc := range changes {
		plan.Changes = append(plan.Changes, *fc)
	}
	sort.Slice(plan.Changes, func(i, j int) bool { return plan.Changes[i].Path < plan.Changes[j].Path })
	return plan
}

// collectFunctionRenameSites gathers every (file, line) that mentions
// module.functionName and must be edited by a rename: definitions, indexed
// references, transitive use-chain references, @spec/@callback lines, bare
// intra-module calls, and `import Module, only: [...]` lines. Shared by the
// LSP rename (which then applies edits) and FunctionRenamePlan.
func (s *Server) collectFunctionRenameSites(module, functionName string) []renameSite {
	type siteKey struct {
		filePath string
		line     int
	}
	seen := make(map[siteKey]bool)
	var sites []renameSite

	addSiteOpts := func(filePath string, line int, includeKeyword bool) {
		if s.stdlibRoot != "" && strings.HasPrefix(filePath, s.stdlibRoot) {
			return
		}
		if s.isDepsFile(filePath) {
			return
		}
		k := siteKey{filePath, line}
		if !seen[k] {
			seen[k] = true
			sites = append(sites, renameSite{filePath, line, includeKeyword})
		}
	}
	addSite := func(filePath string, line int) {
		addSiteOpts(filePath, line, false)
	}

	// Definition sites
	defResults, err := s.store.LookupFunction(module, functionName)
	if err != nil {
		return nil
	}
	for _, r := range defResults {
		addSite(r.FilePath, r.Line)
	}

	// Direct reference sites (calls, imports; skip alias/use which are module-level)
	refResults, err := s.store.LookupReferences(module, functionName)
	if err != nil {
		return nil
	}
	for _, r := range refResults {
		if r.Kind == "alias" || r.Kind == "use" {
			continue
		}
		addSite(r.FilePath, r.Line)
	}

	// Transitive refs via __using__ chains
	for _, mod := range s.findModulesWhoseUsingImports(module) {
		transitive, err := s.store.LookupReferences(mod, functionName)
		if err == nil {
			for _, r := range transitive {
				if r.Kind == "alias" || r.Kind == "use" {
					continue
				}
				addSite(r.FilePath, r.Line)
			}
		}
	}

	// Collect definition file paths for file-scanning passes below
	defFilePaths := make(map[string]bool)
	for _, r := range defResults {
		defFilePaths[r.FilePath] = true
	}

	// Scan definition files for @spec/@callback lines and bare intra-module calls
	// (none of these are indexed in the store).
	specPrefix := "@spec " + functionName
	callbackPrefix := "@callback " + functionName
	for filePath := range defFilePaths {
		fileText, _, ok := s.ReadFileText(filePath)
		if !ok {
			continue
		}
		// @spec and @callback lines
		for i, line := range strings.Split(fileText, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, specPrefix) {
				rest := trimmed[len(specPrefix):]
				if len(rest) == 0 || rest[0] == '(' || rest[0] == ' ' || rest[0] == '\t' {
					addSite(filePath, i+1)
				}
			}
			if strings.HasPrefix(trimmed, callbackPrefix) {
				rest := trimmed[len(callbackPrefix):]
				if len(rest) == 0 || rest[0] == '(' || rest[0] == ' ' || rest[0] == '\t' {
					addSite(filePath, i+1)
				}
			}
		}
		// Bare calls: functionName(...) and |> functionName
		for _, lineNum := range FindBareFunctionCalls(fileText, functionName) {
			addSite(filePath, lineNum)
		}
	}

	// Scan all files that import the module for `import Module, only: [functionName: N]` lines,
	// then also scan those files for bare calls (which aren't indexed as references).
	importRefs, _ := s.store.LookupReferences(module, "")
	importFilePaths := make(map[string]bool)
	for _, r := range importRefs {
		if r.Kind != "import" {
			continue
		}
		lineText, ok := s.FileLine(r.FilePath, r.Line)
		if !ok {
			continue
		}
		if findTokenColumn(lineText, functionName) >= 0 {
			addSiteOpts(r.FilePath, r.Line, true)
			importFilePaths[r.FilePath] = true
		}
	}
	for filePath := range importFilePaths {
		fileText, _, ok := s.ReadFileText(filePath)
		if !ok {
			continue
		}
		for _, lineNum := range FindBareFunctionCalls(fileText, functionName) {
			addSite(filePath, lineNum)
		}
	}

	return sites
}

// delegateSpanEdit is a computed update to a defdelegate facade that forwards
// to a renamed function: the `as:` option is added or updated so the facade
// keeps working. fileLines is the file content the span indices refer to.
type delegateSpanEdit struct {
	filePath  string
	spanStart int // 0-based first line of the defdelegate span
	spanEnd   int // exclusive
	updated   []string
	open      bool
	fileLines []string
}

// delegateSpanEdits computes the defdelegate `as:` updates for a rename of
// module.functionName to newName. readLines supplies the current lines of a
// file (from disk on the apply path, from the in-progress plan on the plan
// path) so both paths see the file state after token edits.
func (s *Server) delegateSpanEdits(module, functionName, newName string, readLines func(string) (lines []string, open, ok bool)) []delegateSpanEdit {
	if !s.followDelegates {
		return nil
	}
	delegates, err := s.store.LookupDelegatesTo(module, functionName)
	if err != nil {
		return nil
	}
	var edits []delegateSpanEdit
	for _, del := range delegates {
		if s.stdlibRoot != "" && strings.HasPrefix(del.FilePath, s.stdlibRoot) {
			continue
		}
		if s.isDepsFile(del.FilePath) {
			continue
		}
		fileLines, open, ok := readLines(del.FilePath)
		if !ok {
			continue
		}
		startLine := del.Line - 1
		if startLine >= len(fileLines) {
			continue
		}

		updatedSpan, spanStart, spanEnd := updateDelegateAs(fileLines, startLine, del.Function, newName)
		// Check if anything actually changed
		changed := len(updatedSpan) != spanEnd-spanStart
		if !changed {
			for i, line := range updatedSpan {
				if line != fileLines[spanStart+i] {
					changed = true
					break
				}
			}
		}
		if !changed {
			continue
		}
		edits = append(edits, delegateSpanEdit{del.FilePath, spanStart, spanEnd, updatedSpan, open, fileLines})
	}
	return edits
}

// applyTokenEditsToLines applies right-to-left whole-token replacements for the
// given sites, returning the updated lines. Shared by the LSP apply path
// (buildTextEdits) and the plan path.
func applyTokenEditsToLines(origLines []string, fileSites []renameSite, oldToken, newToken string) []string {
	lines := make([]string, len(origLines))
	copy(lines, origLines)

	for _, site := range fileSites {
		if site.line-1 >= len(lines) {
			continue
		}
		lineText := lines[site.line-1]
		var cols []int
		if site.includeKeyword {
			cols = findAllTokenColumns(lineText, oldToken)
		} else {
			cols = findFunctionTokenColumns(lineText, oldToken)
		}
		for i := len(cols) - 1; i >= 0; i-- {
			lineText = lineText[:cols[i]] + newToken + lineText[cols[i]+len(oldToken):]
		}
		lines[site.line-1] = lineText
	}
	return lines
}

// spliceLines replaces lines[start:end] with replacement.
func spliceLines(lines []string, start, end int, replacement []string) []string {
	out := make([]string, 0, len(lines)-(end-start)+len(replacement))
	out = append(out, lines[:start]...)
	out = append(out, replacement...)
	out = append(out, lines[end:]...)
	return out
}
