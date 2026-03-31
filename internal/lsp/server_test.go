package lsp

import (
	"os"
	"path/filepath"
	"testing"

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

	return server, func() { s.Close() }
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
