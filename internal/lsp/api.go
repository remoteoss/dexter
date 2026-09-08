package lsp

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

// This file is the exported, name-based surface of the LSP server used by
// callers outside the LSP session (the MCP server and the CLI). Everything
// here delegates to the same internals the LSP handlers use, so results are
// identical regardless of which front end asked.

// Serve runs the Server over the given reader/writer (typically
// stdin/stdout). It blocks until the connection closes.
func Serve(server *Server, in io.Reader, out io.Writer) error {
	logger, _ := zap.NewProduction()
	stream := jsonrpc2.NewStream(stdinoutCloser{in, out})
	conn := jsonrpc2.NewConn(stream)
	server.client = protocol.ClientDispatcher(conn, logger)
	server.conn = conn

	handler := server.renameHandler(protocol.ServerHandler(server, nil))
	ctx := context.Background()

	conn.Go(ctx, handler)
	<-conn.Done()
	return conn.Err()
}

// Ready is closed after the LSP initialize request has been handled. Attached
// services must wait for it before accepting requests so client capabilities,
// stdlib discovery, and the live connection are all available.
func (s *Server) Ready() <-chan struct{} {
	return s.ready
}

// SetStdlibRoot records the Elixir stdlib directory so lookups can classify
// stdlib symbols. The LSP session sets this during Initialize; headless
// callers (MCP) set it explicitly after resolving the stdlib themselves.
func (s *Server) SetStdlibRoot(root string) {
	s.stdlibRoot = root
}

// StdlibRoot returns the Elixir stdlib directory, or "" if not detected. In
// attached MCP mode this is set by Initialize after the Handler is built, so
// callers must read it per request rather than caching it.
func (s *Server) StdlibRoot() string {
	return s.stdlibRoot
}

// CollectReferences gathers references to module (or module.function) across
// the workspace, name-based. It mirrors the collection performed by the LSP
// References handler: direct refs, transitive refs through static __using__
// import chains, bare intra-module calls in definition files, and refs to
// defdelegate facades that target the function. Results are deduplicated by
// file+line, stdlib-filtered, and sorted by file then line.
func (s *Server) CollectReferences(module, function string) []store.ReferenceResult {
	refResults, err := s.store.LookupReferences(module, function)
	if err != nil {
		return nil
	}

	if function != "" {
		// Transitive refs via static __using__ import chains. Call sites of
		// use-injected functions are attributed to the injecting module in the
		// store, so we look up refs under each injector too.
		for _, mod := range s.findModulesWhoseUsingImports(module) {
			if transitive, err := s.store.LookupReferences(mod, function); err == nil {
				refResults = append(refResults, transitive...)
			}
		}

		// Bare intra-module calls in definition files are not indexed.
		refResults = append(refResults, s.findBareCallRefs(module, function)...)

		// Follow defdelegate in reverse: calls to facades that delegate here.
		if s.followDelegates {
			if delegates, err := s.store.LookupDelegatesTo(module, function); err == nil {
				for _, del := range delegates {
					if delegateRefs, err := s.store.LookupReferences(del.Module, del.Function); err == nil {
						refResults = append(refResults, delegateRefs...)
					}
					refResults = append(refResults, s.findBareCallRefs(del.Module, del.Function)...)
				}
			}
		}
	}

	type refKey struct {
		filePath string
		line     int
	}
	seen := make(map[refKey]struct{}, len(refResults))
	var out []store.ReferenceResult
	for _, r := range refResults {
		if s.stdlibRoot != "" && strings.HasPrefix(r.FilePath, s.stdlibRoot) {
			continue
		}
		k := refKey{r.FilePath, r.Line}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// StopGitHeadWatch ends the WatchGitHead goroutine and waits for it, joining
// any reindex it is mid-way through, so the store can be closed safely. The
// MCP server calls it when tearing down a workspace; an LSP session never
// does, its git-head watch runs for the life of the process.
func (s *Server) StopGitHeadWatch() {
	s.gitHeadStopOnce.Do(func() { close(s.gitHeadStop) })
	s.gitHeadWG.Wait()
}

// WithReindexLock runs fn while holding the reindex lock, serializing it with
// ReindexWorkspace and the background reindexes. The MCP file watcher wraps
// its index writes in it so they cannot interleave with a concurrent
// workspace reindex's walk-and-prune.
func (s *Server) WithReindexLock(fn func()) {
	s.reindexing.Lock()
	defer s.reindexing.Unlock()
	s.indexWrites.RLock()
	defer s.indexWrites.RUnlock()
	if s.indexUnavailable {
		return
	}
	fn()
}

// RenameSummary reports what a rename changed on disk.
type RenameSummary struct {
	FilesChanged []string
	FilesMoved   map[string]string // old path → new path (conventional module renames)
}

// RenameFunction renames module.functionName to newName across the workspace
// with the same validation and on-disk semantics as the LSP rename. It returns
// once the index reflects the rename. Open-buffer edits are delivered per
// deliverEdits.
func (s *Server) RenameFunction(module, functionName, newName string) (RenameSummary, error) {
	if !isValidFunctionName(newName) {
		return RenameSummary{}, fmt.Errorf("invalid function name %q: must match [a-z_][a-z0-9_?!]*", newName)
	}
	defs, err := s.store.LookupFunction(module, functionName)
	if err != nil {
		return RenameSummary{}, err
	}
	if len(defs) == 0 {
		return RenameSummary{}, fmt.Errorf("function %s.%s not found in the index", module, functionName)
	}
	if existing, err := s.store.LookupFunction(module, newName); err == nil && len(existing) > 0 {
		return RenameSummary{}, fmt.Errorf("function %s.%s already exists", module, newName)
	}

	edit, files, err := s.renameFunctionEdits(module, functionName, newName, true)
	if err != nil {
		return RenameSummary{}, err
	}
	if err := s.deliverEdits(edit); err != nil {
		return RenameSummary{}, err
	}
	// The machinery reindexes what it wrote in the background; callers are
	// promised an up-to-date index.
	s.backgroundWork.Wait()
	return RenameSummary{FilesChanged: files}, nil
}

// RenameModule renames oldModule (and its submodules) to newModule across the
// workspace, writing changes and conventional file moves to disk.
func (s *Server) RenameModule(oldModule, newModule string) (RenameSummary, error) {
	if !isValidModuleName(newModule) {
		return RenameSummary{}, fmt.Errorf("invalid module name %q: must be CamelCase segments separated by dots", newModule)
	}
	if defs, err := s.store.LookupModule(oldModule); err != nil || len(defs) == 0 {
		if err != nil {
			return RenameSummary{}, err
		}
		return RenameSummary{}, fmt.Errorf("module %s not found in the index", oldModule)
	}

	edit, moved, files, err := s.renameModuleEdits(oldModule, newModule, true)
	if err != nil {
		return RenameSummary{}, err
	}
	if err := s.deliverEdits(edit); err != nil {
		return RenameSummary{}, err
	}
	s.backgroundWork.Wait()
	return RenameSummary{FilesChanged: files, FilesMoved: moved}, nil
}

// deliverEdits carries out a WorkspaceEdit on behalf of a caller that is not
// an editor. With a live LSP client (attached mode) the whole edit is
// forwarded as a workspace/applyEdit request: the editor owns the open
// buffers, and for a file it has open it owns the move too, so it applies
// everything and syncs back via didChange exactly as an editor-initiated
// rename would. Without a client there is no editor to ask, so the edit is
// carried out on disk; headless servers have no open buffers and never move a
// file through the client, so that path is a defensive no-op in practice.
func (s *Server) deliverEdits(edit *WorkspaceEdit) error {
	if edit.empty() {
		return nil
	}
	if s.conn != nil {
		prepared, err := s.prepareDeliveredEdit(edit)
		if err != nil {
			return err
		}
		applied, err := s.applyEdit(context.Background(), "dexter rename", edit)
		if err != nil {
			return err
		}
		if !applied {
			return fmt.Errorf("editor did not apply the rename edits for open files")
		}
		s.recordDeliveredEdit(prepared)
		return nil
	}

	edits := edit.textEditsByPath()
	renames := edit.fileRenames()
	for path, fileEdits := range edits {
		text, _, ok := s.ReadFileText(path)
		if !ok {
			return fmt.Errorf("reading %s to apply rename edits", path)
		}
		if err := os.WriteFile(path, []byte(applyTextEdits(text, fileEdits)), 0644); err != nil {
			return err
		}
	}
	// Renames come after the edits, so an edited file is moved with its new
	// contents — the same order the client applies documentChanges in.
	for from, to := range renames {
		if err := os.MkdirAll(filepath.Dir(to), 0755); err != nil {
			return err
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
		_ = s.store.RemoveFile(from)
	}

	paths := make([]string, 0, len(edits)+len(renames))
	for path := range edits {
		if to, moved := renames[path]; moved {
			paths = append(paths, to)
		} else {
			paths = append(paths, path)
		}
	}
	for from, to := range renames {
		if _, edited := edits[from]; !edited {
			paths = append(paths, to)
		}
	}
	if len(paths) > 0 {
		s.reindexPaths(paths)
	}
	return nil
}

type deliveredFile struct {
	oldPath string
	newPath string
	text    string
	open    bool
}

// prepareDeliveredEdit snapshots the source text without mutating disk or the
// document store. That keeps an editor-rejected workspace edit fully atomic.
func (s *Server) prepareDeliveredEdit(edit *WorkspaceEdit) ([]deliveredFile, error) {
	edits := edit.textEditsByPath()
	renames := edit.fileRenames()
	paths := make(map[string]struct{}, len(edits)+len(renames))
	for path := range edits {
		paths[path] = struct{}{}
	}
	for path := range renames {
		paths[path] = struct{}{}
	}

	prepared := make([]deliveredFile, 0, len(paths))
	for path := range paths {
		text, open, ok := s.ReadFileText(path)
		if !ok {
			return nil, fmt.Errorf("reading %s to prepare rename edits", path)
		}
		if fileEdits := edits[path]; len(fileEdits) > 0 {
			text = applyTextEdits(text, fileEdits)
		}
		newPath := path
		if renamed, ok := renames[path]; ok {
			newPath = renamed
		}
		prepared = append(prepared, deliveredFile{oldPath: path, newPath: newPath, text: text, open: open})
	}
	return prepared, nil
}

// recordDeliveredEdit makes MCP reads and index queries reflect an accepted
// editor edit immediately, without waiting for subsequent didChange events.
func (s *Server) recordDeliveredEdit(files []deliveredFile) {
	s.indexWrites.RLock()
	defer s.indexWrites.RUnlock()
	if s.indexUnavailable {
		return
	}
	for _, file := range files {
		if file.oldPath != file.newPath {
			_ = s.store.RemoveFile(file.oldPath)
		}
		defs, refs, err := parser.ParseText(file.newPath, file.text)
		if err == nil {
			_ = s.store.IndexFileWithRefs(file.newPath, defs, refs)
		}
		if file.open {
			if file.oldPath != file.newPath {
				s.docs.Close(string(uri.File(file.oldPath)))
			}
			s.docs.Set(string(uri.File(file.newPath)), file.text)
		}
	}
}

// applyTextEdits applies non-overlapping TextEdits to text. Positions use the
// same line/byte-column convention the rename machinery produces them in.
func applyTextEdits(text string, edits []protocol.TextEdit) string {
	sorted := make([]protocol.TextEdit, len(edits))
	copy(sorted, edits)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i].Range.Start, sorted[j].Range.Start
		if a.Line != b.Line {
			return a.Line > b.Line
		}
		return a.Character > b.Character
	})

	lines := strings.Split(text, "\n")
	for _, e := range sorted {
		start, end := e.Range.Start, e.Range.End
		if int(start.Line) >= len(lines) || int(end.Line) >= len(lines) {
			continue
		}
		prefix := lines[start.Line][:start.Character]
		suffix := lines[end.Line][end.Character:]
		replacement := strings.Split(prefix+e.NewText+suffix, "\n")
		lines = append(lines[:start.Line], append(replacement, lines[end.Line+1:]...)...)
	}
	return strings.Join(lines, "\n")
}

func (s *Server) reindexPaths(paths []string) {
	for _, path := range paths {
		s.indexOneFile(path)
	}
}
