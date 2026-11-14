package lsp

import (
	"context"
	"sort"
	"sync"
	"time"

	"go.lsp.dev/uri"
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

// UnsavedBuffer returns the text of filePath when an editor holds it open in
// this session with changes that it has not saved, and the time of the last
// change. seq orders buffers across sessions: the higher one changed later.
func (s *Server) UnsavedBuffer(filePath string) (text string, seq uint64, changedAt time.Time, ok bool) {
	return s.docs.UnsavedBuffer(string(uri.File(filePath)))
}

// RenameSummary reports what a rename changed on disk.
type RenameSummary struct {
	FilesChanged []string
	FilesMoved   map[string]string // old path → new path (conventional module renames)
	// FilesFailed lists the files the rename could not change. They still use
	// the old name. FailureReason is the first error.
	FilesFailed   []string
	FailureReason string
	// IndexPending is true when the files are written but the index update
	// did not end within renameSummaryWait (it waits behind a long index pass,
	// such as the one after a branch switch) or before the request ended.
	IndexPending bool
	// Warnings are the rename's messages for the user, such as calls that it
	// could not change (through a variable, or with the function's atom).
	// An editor gets them as messages; a caller without one shows them.
	Warnings []string

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

// moved records that the rename moved a file to a new path.
func (r *RenameSummary) moved(from, to string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.FilesMoved == nil {
		r.FilesMoved = make(map[string]string)
	}
	r.FilesMoved[from] = to
	r.FilesChanged = append(r.FilesChanged, to)
	r.mu.Unlock()
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

// renameSummaryWait bounds the wait of a headless rename for its index update.
var renameSummaryWait = 5 * time.Second

// finish waits for the index updates (see IndexPending), sorts the lists,
// removes duplicates, and removes the failed files from the changed files.
func (r *RenameSummary) finish(ctx context.Context) {
	r.mu.Lock()
	indexed := r.indexed
	r.indexed = nil
	r.mu.Unlock()
	timer := time.NewTimer(renameSummaryWait)
	defer timer.Stop()
wait:
	for _, ch := range indexed {
		select {
		case <-ch:
		case <-timer.C:
			r.IndexPending = true
			break wait
		case <-ctx.Done():
			r.IndexPending = true
			break wait
		}
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
// for a caller without an editor (the MCP tools), with the same rules and the
// same machinery as the editor rename; see RenameFunctionHeadless. It returns
// when the index shows the rename.
func (s *Server) RenameFunction(module, functionName, newName string) (*RenameSummary, error) {
	return s.RenameFunctionContext(context.Background(), module, functionName, newName)
}

// RenameFunctionContext is RenameFunction for a caller that can cancel. A
// rename canceled before it starts to write changes nothing; one canceled
// during its writes completes.
func (s *Server) RenameFunctionContext(ctx context.Context, module, functionName, newName string) (*RenameSummary, error) {
	summary := &RenameSummary{}
	indexed, err := s.renameFunctionHeadless(ctx, module, functionName, newName, summary)
	if indexed != nil {
		summary.waitFor(indexed)
	}
	summary.finish(ctx)
	if err != nil {
		return nil, err
	}
	return summary, nil
}

// RenameModule renames oldModule (and its submodules) to newModule across the
// workspace for a caller without an editor, including the moves of files that
// follow the naming convention. See RenameFunction.
func (s *Server) RenameModule(oldModule, newModule string) (*RenameSummary, error) {
	return s.RenameModuleContext(context.Background(), oldModule, newModule)
}

// RenameModuleContext is RenameModule for a caller that can cancel, with the
// same rule as RenameFunctionContext.
func (s *Server) RenameModuleContext(ctx context.Context, oldModule, newModule string) (*RenameSummary, error) {
	summary := &RenameSummary{}
	indexed, err := s.renameModuleHeadless(ctx, oldModule, newModule, summary)
	if indexed != nil {
		summary.waitFor(indexed)
	}
	summary.finish(ctx)
	if err != nil {
		return nil, err
	}
	return summary, nil
}
