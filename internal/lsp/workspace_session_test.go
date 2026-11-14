package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/store"
)

type recordingWorkspaceEvents struct {
	reconciled chan string
	removed    chan string
}

func (e *recordingWorkspaceEvents) ReconcileFile(path string)                   { e.reconciled <- path }
func (e *recordingWorkspaceEvents) ReconcileSavedFile(path string)              { e.reconciled <- path }
func (e *recordingWorkspaceEvents) RemoveFile(path string)                      { e.removed <- path }
func (e *recordingWorkspaceEvents) SetStdlibRoot(context.Context, string) error { return nil }

func TestDaemonSessionRoutesDiskEventsToWorkspaceOwner(t *testing.T) {
	root := t.TempDir()
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	events := &recordingWorkspaceEvents{reconciled: make(chan string, 1), removed: make(chan string, 1)}
	server := NewServerWithOptions(s, root, ServerOptions{
		Index:           NewIndexCoordinator(),
		Events:          events,
		ManageWorkspace: false,
	})

	path := filepath.Join(root, "lib", "worker.ex")
	docURI := protocol.DocumentURI(uri.File(path))
	if err := server.DidSave(context.Background(), &protocol.DidSaveTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
	}); err != nil {
		t.Fatal(err)
	}
	if got := <-events.reconciled; got != path {
		t.Fatalf("reconciled %q, want %q", got, path)
	}

	if err := server.DidChangeWatchedFiles(context.Background(), &protocol.DidChangeWatchedFilesParams{
		Changes: []*protocol.FileEvent{{URI: docURI, Type: protocol.FileChangeTypeDeleted}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := <-events.removed; got != path {
		t.Fatalf("removed %q, want %q", got, path)
	}
}

func TestDaemonSessionDoesNotOwnWorkspaceLifecycle(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	server := NewServerWithOptions(s, root, ServerOptions{
		Index:           NewIndexCoordinator(),
		ManageWorkspace: false,
	})
	if _, err := server.Initialize(context.Background(), &protocol.InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	server.index.backgroundWork.Wait()
	if !s.IsEmpty() {
		t.Fatal("daemon-backed LSP session unexpectedly started workspace indexing")
	}
	if err := server.Exit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonSessionRejectsSymlinkedEditorRoot(t *testing.T) {
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	server := NewServerWithOptions(s, root, ServerOptions{
		Index:           NewIndexCoordinator(),
		Events:          &recordingWorkspaceEvents{reconciled: make(chan string, 1), removed: make(chan string, 1)},
		ManageWorkspace: false,
	})
	_, err = server.Initialize(context.Background(), &protocol.InitializeParams{
		WorkspaceFolders: []protocol.WorkspaceFolder{{URI: string(uri.File(alias))}},
	})
	if err == nil {
		t.Fatal("daemon session accepted a different workspace root spelling")
	}
}

func TestDaemonSessionRejectsNestedSymlinkedEditorRoot(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "apps", "real")
	alias := filepath.Join(root, "alias")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	server := NewServerWithOptions(s, root, ServerOptions{
		Index:           NewIndexCoordinator(),
		Events:          &recordingWorkspaceEvents{reconciled: make(chan string, 1), removed: make(chan string, 1)},
		ManageWorkspace: false,
	})
	_, err = server.Initialize(context.Background(), &protocol.InitializeParams{
		WorkspaceFolders: []protocol.WorkspaceFolder{{URI: string(uri.File(alias))}},
	})
	if err == nil {
		t.Fatal("daemon session accepted a nested symlink workspace spelling")
	}
}

// An editor whose workspace folder is above the project (an editor root
// marker that found a parent directory) gets the project; a folder outside
// the project is refused with a message that says what to do.
func TestDaemonSessionEditorRootAboveTheProject(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "work", "proj")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	session := func() *Server {
		return NewServerWithOptions(s, root, ServerOptions{
			Index:  NewIndexCoordinator(),
			Events: &recordingWorkspaceEvents{reconciled: make(chan string, 8), removed: make(chan string, 8)},
		})
	}
	initialize := func(server *Server, folder string) error {
		_, err := server.Initialize(context.Background(), &protocol.InitializeParams{
			WorkspaceFolders: []protocol.WorkspaceFolder{{URI: string(uri.File(folder)), Name: filepath.Base(folder)}},
		})
		return err
	}
	server := session()
	if err := initialize(server, parent); err != nil {
		t.Fatalf("an editor folder above the project: %v", err)
	}
	if server.projectRoot != root {
		t.Errorf("project root = %s, want %s", server.projectRoot, root)
	}
	other := filepath.Join(parent, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	err = initialize(session(), other)
	if err == nil || !strings.Contains(err.Error(), "is not inside the project") || !strings.Contains(err.Error(), "Set the editor's root for Dexter to "+root) {
		t.Errorf("an editor folder outside the project: %v", err)
	}
}
