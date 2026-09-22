package lsp

import (
	"context"
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/store"
)

type recordingWorkspaceEvents struct {
	reconciled chan string
	removed    chan string
}

func (e *recordingWorkspaceEvents) ReconcileFile(path string) { e.reconciled <- path }
func (e *recordingWorkspaceEvents) RemoveFile(path string)    { e.removed <- path }

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
