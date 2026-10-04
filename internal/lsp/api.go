package lsp

import (
	"fmt"
	"sort"
	"sync"
)

// This file is the exported, name-based surface of the LSP server for callers
// outside an editor session, such as the MCP tools that the workspace daemon
// runs. Everything here delegates to the same internals the LSP handlers use,
// so the results are the same for each frontend.

// ReadFileText returns a file's current text, preferring an editor-owned buffer.
func (s *Server) ReadFileText(filePath string) (text string, open bool, ok bool) {
	return s.readFileText(filePath)
}

// FileLine returns one 1-based line, preferring an editor-owned buffer.
func (s *Server) FileLine(filePath string, lineNum int) (string, bool) {
	return s.getFileLine(filePath, lineNum)
}

// RenameSummary reports what a rename changed on disk.
type RenameSummary struct {
	FilesChanged []string
	FilesMoved   map[string]string // old path → new path (conventional module renames)
	// FilesFailed lists the files the rename could not change. They still use
	// the old name. FailureReason is the first error.
	FilesFailed   []string
	FailureReason string

	mu      sync.Mutex
	indexed []<-chan struct{}
}

// changed records that the rename changed path. A nil summary ignores it, so
// the editor rename passes nil.
func (r *RenameSummary) changed(path string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.FilesChanged = append(r.FilesChanged, path)
	r.mu.Unlock()
}

// failed records that the rename could not change path.
func (r *RenameSummary) failed(path string, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if len(r.FilesFailed) == 0 {
		r.FailureReason = err.Error()
	}
	r.FilesFailed = append(r.FilesFailed, path)
	r.mu.Unlock()
}

// recordFailures copies the failures of one rename pass into the summary.
func (r *RenameSummary) recordFailures(f *renameFailures) {
	if r == nil {
		return
	}
	f.mu.Lock()
	paths := append([]string(nil), f.paths...)
	first := f.first
	f.mu.Unlock()
	if len(paths) == 0 {
		return
	}
	r.mu.Lock()
	if len(r.FilesFailed) == 0 {
		r.FailureReason = first
	}
	r.FilesFailed = append(r.FilesFailed, paths...)
	r.mu.Unlock()
}

// recordModuleRename records the files and moves of a module rename.
func (r *RenameSummary) recordModuleRename(sitesByFile map[string][]moduleEditSite, movedFiles, clientRenames map[string]string, failures *renameFailures) {
	if r == nil {
		return
	}
	r.mu.Lock()
	for path := range sitesByFile {
		r.FilesChanged = append(r.FilesChanged, path)
	}
	if len(movedFiles)+len(clientRenames) > 0 && r.FilesMoved == nil {
		r.FilesMoved = make(map[string]string, len(movedFiles)+len(clientRenames))
	}
	for from, to := range movedFiles {
		r.FilesMoved[from] = to
	}
	for from, to := range clientRenames {
		r.FilesMoved[from] = to
	}
	r.mu.Unlock()
	r.recordFailures(failures)
}

// waitFor records an index update that the rename started. finish waits for
// it, so the caller sees an index that shows the rename.
func (r *RenameSummary) waitFor(indexed <-chan struct{}) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.indexed = append(r.indexed, indexed)
	r.mu.Unlock()
}

// finish waits for the index updates, sorts the lists, removes duplicates,
// and removes the failed files from the changed files.
func (r *RenameSummary) finish() {
	r.mu.Lock()
	indexed := r.indexed
	r.indexed = nil
	r.mu.Unlock()
	for _, ch := range indexed {
		<-ch
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.FilesFailed = sortedUnique(r.FilesFailed)
	failed := make(map[string]struct{}, len(r.FilesFailed))
	for _, path := range r.FilesFailed {
		failed[path] = struct{}{}
	}
	all := sortedUnique(r.FilesChanged)
	changed := make([]string, 0, len(all))
	for _, path := range all {
		if _, ok := failed[path]; !ok {
			changed = append(changed, path)
		}
	}
	r.FilesChanged = changed
}

func sortedUnique(paths []string) []string {
	if len(paths) == 0 {
		return paths
	}
	sort.Strings(paths)
	out := paths[:1]
	for _, path := range paths[1:] {
		if path != out[len(out)-1] {
			out = append(out, path)
		}
	}
	return out
}

// RenameFunction renames module.functionName to newName across the workspace
// with the same validation and the same machinery as the editor rename. It
// returns when the index shows the rename.
//
// The caller has no editor, so every file that this server does not hold open
// is written on disk. The workspace daemon calls it on its headless language
// service, which holds no editor buffers.
func (s *Server) RenameFunction(module, functionName, newName string) (*RenameSummary, error) {
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

	summary := &RenameSummary{}
	edit, err := s.renameFunctionEdits(module, functionName, newName, summary)
	summary.finish()
	if err != nil {
		return nil, err
	}
	if err := requireNoBufferEdits(edit); err != nil {
		return summary, err
	}
	return summary, nil
}

// RenameModule renames oldModule (and its submodules) to newModule across the
// workspace, with the same validation and machinery as the editor rename,
// including the moves of files that follow the naming convention. It returns
// when the index shows the rename.
func (s *Server) RenameModule(oldModule, newModule string) (*RenameSummary, error) {
	if !isValidModuleName(newModule) {
		return nil, fmt.Errorf("invalid module name %q: must be CamelCase segments separated by dots", newModule)
	}
	defs, err := s.store.LookupModule(oldModule)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("module %s not found in the index", oldModule)
	}

	summary := &RenameSummary{}
	edit, err := s.renameModuleEdits(oldModule, newModule, summary)
	summary.finish()
	if err != nil {
		return nil, err
	}
	if err := requireNoBufferEdits(edit); err != nil {
		return summary, err
	}
	return summary, nil
}

// requireNoBufferEdits reports an error when the rename machinery left edits
// for buffers that the server holds open: a caller without an editor cannot
// apply them. The headless language service holds no buffers, so this is a
// guard, not an expected path.
func requireNoBufferEdits(edit *WorkspaceEdit) error {
	if edit == nil || (len(edit.Changes) == 0 && len(edit.DocumentChanges) == 0) {
		return nil
	}
	return fmt.Errorf("the rename has edits for files that are open in this language service; only an editor can apply them")
}
