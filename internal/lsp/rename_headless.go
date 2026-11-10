package lsp

import (
	"context"
	"fmt"
	"strings"
)

// RenameFunctionHeadless renames module.function to newName for a caller that
// has no editor to apply a WorkspaceEdit, such as an MCP tool or a CLI
// command. Every change goes onto the disk, through the same planner as an
// editor's rename, with the headless rule of renameFileAction: a file that an
// editor has open without unsaved changes is written (the editor reloads it),
// and a file with unsaved changes in an editor stops the whole rename before
// any change, with an error that names the file. The index follows the disk:
// the returned channel closes when the index has the rename, so a caller that
// answers from the index next waits for it. It is nil when err is not.
func (s *Server) RenameFunctionHeadless(ctx context.Context, module, function, newName string) (indexed <-chan struct{}, err error) {
	return s.renameFunctionHeadless(ctx, module, function, newName, nil)
}

func (s *Server) renameFunctionHeadless(ctx context.Context, module, function, newName string, report *RenameSummary) (indexed <-chan struct{}, err error) {
	if !isValidFunctionName(newName) {
		return nil, fmt.Errorf("invalid function name %q: must match [a-z_][a-z0-9_?!]*", newName)
	}
	defs, err := s.store.LookupFunction(module, function)
	if err != nil {
		return nil, fmt.Errorf("cannot rename %s.%s: the index query failed (%v); nothing was renamed", module, function, err)
	}
	if len(defs) == 0 {
		if cbs, _ := s.store.LookupCallbackDef(module, function); len(cbs) == 0 {
			return nil, fmt.Errorf("function %s.%s not found in the index", module, function)
		}
	}
	edit, err := s.renameFunctionEdits(ctx, module, function, newName, requesterHeadless, report)
	return headlessResult(edit, err)
}

// RenameModuleHeadless renames oldModule, and its submodules, to newModule
// for a caller that has no editor. See RenameFunctionHeadless for the rules
// and the returned channel. Files that follow the naming convention are moved
// on disk.
func (s *Server) RenameModuleHeadless(ctx context.Context, oldModule, newModule string) (indexed <-chan struct{}, err error) {
	return s.renameModuleHeadless(ctx, oldModule, newModule, nil)
}

func (s *Server) renameModuleHeadless(ctx context.Context, oldModule, newModule string, report *RenameSummary) (indexed <-chan struct{}, err error) {
	if !isValidModuleName(newModule) {
		return nil, fmt.Errorf("invalid module name %q: must be CamelCase segments separated by dots", newModule)
	}
	defs, err := s.store.LookupModule(oldModule)
	if err != nil {
		return nil, fmt.Errorf("cannot rename %s: the index query failed (%v); nothing was renamed", oldModule, err)
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("module %s not found in the index", oldModule)
	}
	edit, err := s.renameModuleEdits(ctx, oldModule, newModule, requesterHeadless, report)
	return headlessResult(edit, err)
}

// headlessResult checks that a headless rename left nothing for an editor to
// apply. The headless rule writes every file it does not refuse, so a
// non-empty edit is a planner bug, and it is reported instead of dropped.
func headlessResult(edit *WorkspaceEdit, err error) (<-chan struct{}, error) {
	if err != nil {
		return nil, err
	}
	if edit != nil && (len(edit.Changes) > 0 || len(edit.DocumentChanges) > 0) {
		var files []string
		for u := range edit.Changes {
			files = append(files, string(u))
		}
		return nil, fmt.Errorf("internal error: a headless rename left edits for an editor (%s)", strings.Join(files, ", "))
	}
	if edit == nil || edit.indexed == nil {
		closed := make(chan struct{})
		close(closed)
		return closed, nil
	}
	return edit.indexed, nil
}
