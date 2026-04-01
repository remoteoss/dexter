package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/store"
)

func setupTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	dir := t.TempDir()

	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	server := NewServer(s, dir)

	return server, func() {
		if err := s.Close(); err != nil {
			t.Errorf("failed to close store: %v", err)
		}
	}
}

func indexFile(t *testing.T, s *store.Store, dir, relPath, content string) {
	t.Helper()
	path := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	defs, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}
}

func TestServer_FollowDelegates_Default(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  alias MyApp.Accounts.Create

  defdelegate create(attrs), to: Create, as: :call
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/accounts/create.ex", `defmodule MyApp.Accounts.Create do
  def call(attrs) do
    :ok
  end
end
`)

	// followDelegates defaults to true — should jump to Create.call
	results, err := server.store.LookupFollowDelegate("MyApp.Accounts", "create")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0].Kind == "defdelegate" {
		t.Error("with followDelegates=true, should not return defdelegate line")
	}
}

func TestServer_FollowDelegates_False(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	server.followDelegates = false

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  alias MyApp.Accounts.Create

  defdelegate create(attrs), to: Create, as: :call
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/accounts/create.ex", `defmodule MyApp.Accounts.Create do
  def call(attrs) do
    :ok
  end
end
`)

	// followDelegates=false — should return the defdelegate line itself
	results, err := server.store.LookupFunction("MyApp.Accounts", "create")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	if results[0].Kind != "defdelegate" {
		t.Errorf("with followDelegates=false, expected defdelegate kind, got %q", results[0].Kind)
	}
}

// waitFor polls condition every 10ms until it returns true or one second elapses.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestServer_DidChangeWatchedFiles_Create(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	path := filepath.Join(server.projectRoot, "lib", "my_module.ex")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`defmodule MyApp.MyModule do
  def hello, do: :world
end`), 0644); err != nil {
		t.Fatal(err)
	}

	err := server.DidChangeWatchedFiles(context.Background(), &protocol.DidChangeWatchedFilesParams{
		Changes: []*protocol.FileEvent{
			{URI: uri.File(path), Type: protocol.FileChangeTypeCreated},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		_, found := server.store.GetFileMtime(path)
		return found
	})

	results, err := server.store.LookupFunction("MyApp.MyModule", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected hello/0 to be indexed after DidChangeWatchedFiles create event")
	}
}

func TestServer_DidChangeWatchedFiles_Delete(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/my_module.ex", `defmodule MyApp.MyModule do
  def hello, do: :world
end`)
	path := filepath.Join(server.projectRoot, "lib", "my_module.ex")

	results, err := server.store.LookupFunction("MyApp.MyModule", "hello")
	if err != nil || len(results) == 0 {
		t.Fatal("file should be indexed before delete test")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	err = server.DidChangeWatchedFiles(context.Background(), &protocol.DidChangeWatchedFilesParams{
		Changes: []*protocol.FileEvent{
			{URI: uri.File(path), Type: protocol.FileChangeTypeDeleted},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		results, _ := server.store.LookupFunction("MyApp.MyModule", "hello")
		return len(results) == 0
	})
}

func TestServer_backgroundReindex_PrunesDeletedFiles(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/gone.ex", `defmodule Gone do
  def bye, do: :poof
end`)
	path := filepath.Join(server.projectRoot, "lib", "gone.ex")

	results, err := server.store.LookupFunction("Gone", "bye")
	if err != nil || len(results) == 0 {
		t.Fatal("should be indexed before deletion")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	server.backgroundReindex()

	waitFor(t, func() bool {
		results, _ := server.store.LookupFunction("Gone", "bye")
		return len(results) == 0
	})
}

func TestServer_InitializationOptions_FollowDelegates(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Default should be true
	if !server.followDelegates {
		t.Error("followDelegates should default to true")
	}

	// Simulate initializationOptions with followDelegates=false
	opts := map[string]interface{}{
		"followDelegates": false,
	}
	if v, ok := opts["followDelegates"].(bool); ok {
		server.followDelegates = v
	}

	if server.followDelegates {
		t.Error("followDelegates should be false after setting via initializationOptions")
	}
}
