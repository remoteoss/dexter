package lsp

import (
	"context"
	"encoding/json"
	"fmt"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// WorkspaceEdit is our own workspace edit type. go.lsp.dev/protocol's
// WorkspaceEdit types documentChanges as []TextDocumentEdit, so it cannot
// express resource operations (create/rename/delete file). We need rename
// operations: when a module rename moves a file that is open in the editor,
// the editor itself must move the buffer, otherwise it is left holding a
// modified buffer pointing at a path the server deleted — saving it recreates
// the old file with the new module name and the project no longer compiles.
//
// Per the LSP spec a client that supports documentChanges must ignore
// changes entirely when documentChanges is present, so the two fields are
// mutually exclusive: emit everything through documentChanges as soon as one
// resource operation is needed.
type WorkspaceEdit struct {
	Changes         map[protocol.DocumentURI][]protocol.TextEdit `json:"changes,omitempty"`
	DocumentChanges []interface{}                                `json:"documentChanges,omitempty"`
}

// TextDocumentEdit is a documentChanges entry holding text edits for one
// document. Version is always null: we never track buffer versions, and the
// spec allows a null version to mean "apply without a version check".
type TextDocumentEdit struct {
	TextDocument versionedTextDocumentIdentifier `json:"textDocument"`
	Edits        []protocol.TextEdit             `json:"edits"`
}

type versionedTextDocumentIdentifier struct {
	URI     protocol.DocumentURI `json:"uri"`
	Version *int                 `json:"version"`
}

// RenameFile is a documentChanges entry that moves a file. Clients apply
// documentChanges in order, so a TextDocumentEdit for OldURI placed before
// this operation is applied to the buffer first and then travels with it.
type RenameFile struct {
	Kind    string               `json:"kind"` // always "rename"
	OldURI  protocol.DocumentURI `json:"oldUri"`
	NewURI  protocol.DocumentURI `json:"newUri"`
	Options *RenameFileOptions   `json:"options,omitempty"`
}

type RenameFileOptions struct {
	Overwrite      bool `json:"overwrite,omitempty"`
	IgnoreIfExists bool `json:"ignoreIfExists,omitempty"`
}

// pathToURI converts a filesystem path to a document URI.
func pathToURI(path string) protocol.DocumentURI {
	return protocol.DocumentURI(uri.File(path))
}

// newRenameFile builds a rename operation for the given paths.
func newRenameFile(oldPath, newPath string) RenameFile {
	return RenameFile{
		Kind:    "rename",
		OldURI:  pathToURI(oldPath),
		NewURI:  pathToURI(newPath),
		Options: &RenameFileOptions{Overwrite: true},
	}
}

// textDocumentEdit builds a documentChanges entry for a single document.
func textDocumentEdit(fileURI protocol.DocumentURI, edits []protocol.TextEdit) TextDocumentEdit {
	return TextDocumentEdit{
		TextDocument: versionedTextDocumentIdentifier{URI: fileURI},
		Edits:        edits,
	}
}

// toProtocol degrades a WorkspaceEdit to the protocol type, dropping resource
// operations. Only used by the protocol.Server interface shim; the production
// path replies with the full type through the handler in Serve.
func (e *WorkspaceEdit) toProtocol() *protocol.WorkspaceEdit {
	if e == nil {
		return nil
	}
	return &protocol.WorkspaceEdit{Changes: e.Changes}
}

// renameHandler intercepts textDocument/rename so the reply can carry
// resource operations. protocol.ServerHandler marshals whatever
// Server.Rename returns, and protocol.WorkspaceEdit has no field for them,
// so the request has to be answered before it reaches that dispatcher.
func (s *Server) renameHandler(next jsonrpc2.Handler) jsonrpc2.Handler {
	return func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		if req.Method() != protocol.MethodTextDocumentRename {
			return next(ctx, reply, req)
		}
		var params protocol.RenameParams
		if err := json.Unmarshal(req.Params(), &params); err != nil {
			return reply(ctx, nil, fmt.Errorf("%w: %v", jsonrpc2.ErrParse, err))
		}
		edit, err := s.RenameEdit(ctx, &params)
		if err != nil {
			return reply(ctx, nil, err)
		}
		if edit == nil {
			return reply(ctx, nil, nil)
		}
		return reply(ctx, edit, nil)
	}
}
