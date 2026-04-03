package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/stdlib"
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
	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFileWithRefs(path, defs, refs); err != nil {
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

func definitionAt(t *testing.T, server *Server, uri string, line, col uint32) []protocol.Location {
	t.Helper()
	result, err := server.Definition(context.Background(), &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: line, Character: col},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func referencesAt(t *testing.T, server *Server, uri string, line, col uint32) []protocol.Location {
	t.Helper()
	result, err := server.References(context.Background(), &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: line, Character: col},
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func completionAt(t *testing.T, server *Server, uri string, line, col uint32) []protocol.CompletionItem {
	t.Helper()
	result, err := server.Completion(context.Background(), &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: line, Character: col},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		return nil
	}
	return result.Items
}

func hasCompletionItem(items []protocol.CompletionItem, label string) bool {
	for _, item := range items {
		if item.Label == label {
			return true
		}
	}
	return false
}

func TestCompletion_FunctionAfterDot(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create(attrs) do
    :ok
  end

  def list(opts) do
    :ok
  end

  defp validate(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Accounts.")

	items := completionAt(t, server, uri, 0, 17)
	if !hasCompletionItem(items, "create") {
		t.Error("expected 'create' in completions")
	}
	if !hasCompletionItem(items, "list") {
		t.Error("expected 'list' in completions")
	}
	if hasCompletionItem(items, "validate") {
		t.Error("should not include private function 'validate'")
	}
}

func TestCompletion_SubModuleAfterDot(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/ecto/query.ex", `defmodule Ecto.Query do
  def from(expr, kw \\ []) do
    :ok
  end
end

defmodule Ecto.Query.API do
end

defmodule Ecto.Schema do
end
`)

	uri := "file:///test.ex"

	// Typing "Ecto." should offer only immediate sub-module segments
	server.docs.Set(uri, "  Ecto.")
	items := completionAt(t, server, uri, 0, 7)
	if !hasCompletionItem(items, "Query") {
		t.Error("expected 'Query' sub-module after 'Ecto.'")
	}
	if !hasCompletionItem(items, "Schema") {
		t.Error("expected 'Schema' sub-module after 'Ecto.'")
	}
	// Ecto.Query.API should appear as "Query" (immediate segment), not "Query.API"
	if hasCompletionItem(items, "Query.API") {
		t.Error("should not show deep nested 'Query.API' after 'Ecto.' — only immediate children")
	}

	// Query appears exactly once even though Ecto.Query and Ecto.Query.API both exist
	queryCount := 0
	for _, item := range items {
		if item.Label == "Query" {
			queryCount++
		}
	}
	if queryCount != 1 {
		t.Errorf("expected 'Query' exactly once, got %d", queryCount)
	}

	// Typing "Ecto.Q" (prefix search) should still show full names
	server.docs.Set(uri, "  Ecto.Q")
	items = completionAt(t, server, uri, 0, 8)
	if !hasCompletionItem(items, "Ecto.Query") {
		t.Error("expected 'Ecto.Query' when typing 'Ecto.Q'")
	}
	if hasCompletionItem(items, "Ecto.Schema") {
		t.Error("should not include 'Ecto.Schema' — doesn't match prefix 'Ecto.Q'")
	}
}

func TestCompletion_FunctionSnippet(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create(name, email) do
    :ok
  end

  def all do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Accounts.")

	items := completionAt(t, server, uri, 0, 17)

	for _, item := range items {
		if item.Label == "create" {
			if item.InsertText != "create(${1:arg1}, ${2:arg2})" {
				t.Errorf("create: expected snippet insert text, got %q", item.InsertText)
			}
			if item.InsertTextFormat != protocol.InsertTextFormatSnippet {
				t.Errorf("create: expected snippet format, got %v", item.InsertTextFormat)
			}
			break
		}
	}

	for _, item := range items {
		if item.Label == "all" {
			if item.InsertText != "" {
				t.Errorf("all: expected no insert text for zero-arity, got %q", item.InsertText)
			}
			if item.InsertTextFormat == protocol.InsertTextFormatSnippet {
				t.Error("all: should not have snippet format for zero-arity")
			}
			break
		}
	}
}

func TestCompletion_MultiArity(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def fetch(id) do
    :ok
  end

  def fetch(id, opts) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Accounts.")

	items := completionAt(t, server, uri, 0, 17)
	fetchCount := 0
	for _, item := range items {
		if item.Label == "fetch" {
			fetchCount++
		}
	}
	if fetchCount != 2 {
		t.Errorf("expected 2 fetch completions (arity 1 and 2), got %d", fetchCount)
	}
}

func TestCompletion_FunctionPrefix(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create(attrs) do
    :ok
  end

  def cancel(id) do
    :ok
  end

  def list(opts) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Accounts.cr")

	items := completionAt(t, server, uri, 0, 19)
	if !hasCompletionItem(items, "create") {
		t.Error("expected 'create' in completions")
	}
	if hasCompletionItem(items, "list") {
		t.Error("should not include 'list' — doesn't match prefix 'cr'")
	}
}

func TestCompletion_ModulePrefix(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/handlers.ex", `defmodule MyApp.Handlers do
end

defmodule MyApp.Handlers.Webhooks do
end

defmodule MyApp.Repo do
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Hand")

	items := completionAt(t, server, uri, 0, 12)
	if !hasCompletionItem(items, "MyApp.Handlers") {
		t.Error("expected 'MyApp.Handlers' in completions")
	}
	if !hasCompletionItem(items, "MyApp.Handlers.Webhooks") {
		t.Error("expected 'MyApp.Handlers.Webhooks' in completions")
	}
	if hasCompletionItem(items, "MyApp.Repo") {
		t.Error("should not include 'MyApp.Repo' — doesn't match prefix")
	}
}

func TestCompletion_AliasedModule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  alias MyApp.Accounts

  Accounts.
end`)

	items := completionAt(t, server, uri, 3, 12)
	if !hasCompletionItem(items, "create") {
		t.Error("expected 'create' via aliased module")
	}
}

func TestCompletion_AliasedModulePrefix(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/feature_flag.ex", `defmodule MyApp.Services.IsFeatureFlagEnabled do
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  alias MyApp.Services.IsFeatureFlagEnabled

  IsFeatureFl
end`)

	items := completionAt(t, server, uri, 3, 13)
	if !hasCompletionItem(items, "IsFeatureFlagEnabled") {
		t.Error("expected 'IsFeatureFlagEnabled' via alias prefix")
	}
}

func TestCompletion_AliasedModulePrefix_WithAs(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/handlers.ex", `defmodule MyApp.Handlers.Foo do
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  alias MyApp.Handlers.Foo, as: MyFoo

  MyF
end`)

	items := completionAt(t, server, uri, 3, 5)
	if !hasCompletionItem(items, "MyFoo") {
		t.Error("expected 'MyFoo' via alias as: prefix")
	}
}

func TestCompletion_AliasedModulePrefix_MultiAlias(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/handlers.ex", `defmodule MyApp.Handlers.Foo do
end

defmodule MyApp.Handlers.Bar do
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  alias MyApp.Handlers.{Foo, Bar}

  Fo
end`)

	items := completionAt(t, server, uri, 3, 4)
	if !hasCompletionItem(items, "Foo") {
		t.Error("expected 'Foo' via multi-alias prefix")
	}
	if hasCompletionItem(items, "Bar") {
		t.Error("should not include 'Bar' — doesn't match prefix 'Fo'")
	}
}

func TestCompletion_AliasedModulePrefix_ModuleReference(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/feature_flag.ex", `defmodule MyApp.HRIS.Services.IsFeatureFlagEnabled do
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.HRIS do
  alias __MODULE__.Services.IsFeatureFlagEnabled

  IsFeature
end`)

	items := completionAt(t, server, uri, 3, 11)
	if !hasCompletionItem(items, "IsFeatureFlagEnabled") {
		t.Error("expected 'IsFeatureFlagEnabled' via __MODULE__ alias prefix")
	}
}

func TestCompletion_AliasedModulePrefix_PartialPath(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/feature_flag.ex", `defmodule MyApp.HRIS.Services.IsFeatureFlagEnabled do
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.HRIS do
  alias __MODULE__.Services

  Services.IsFeature
end`)

	items := completionAt(t, server, uri, 3, 20)
	if !hasCompletionItem(items, "Services.IsFeatureFlagEnabled") {
		t.Error("expected 'Services.IsFeatureFlagEnabled' via partial alias path")
	}
}

func TestCompletion_AliasedModulePrefix_NoFalsePositives(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  alias MyApp.Accounts

  NonExist
end`)

	items := completionAt(t, server, uri, 3, 10)
	if len(items) != 0 {
		t.Errorf("expected no completions for 'NonExist', got %d", len(items))
	}
}

func TestCompletion_ImportedFunctions(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/helpers.ex", `defmodule MyApp.Helpers do
  def format_date(d) do
    :ok
  end

  def format_currency(amount) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  import MyApp.Helpers

  format_d
end`)

	items := completionAt(t, server, uri, 3, 10)
	if !hasCompletionItem(items, "format_date") {
		t.Error("expected 'format_date' via import")
	}
	if hasCompletionItem(items, "format_currency") {
		t.Error("should not include 'format_currency' — doesn't match prefix 'format_d'")
	}
}

func TestCompletion_LocalBufferFunctions(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  def process(data) do
    :ok
  end

  defp private_helper(data) do
    :ok
  end

  pr
end`)

	items := completionAt(t, server, uri, 9, 4)
	if !hasCompletionItem(items, "process") {
		t.Error("expected 'process' from buffer")
	}
	if !hasCompletionItem(items, "private_helper") {
		t.Error("expected 'private_helper' from buffer")
	}
}

func TestCompletion_NoResults(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, "  ")

	items := completionAt(t, server, uri, 0, 2)
	if len(items) != 0 {
		t.Errorf("expected no completions on whitespace, got %d", len(items))
	}
}

func TestCompletionResolve_WithDoc(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  @doc """
  Creates a new account with the given attributes.
  """
  @spec create(map()) :: {:ok, term()} | {:error, term()}
  def create(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Accounts.")

	items := completionAt(t, server, uri, 0, 17)
	if !hasCompletionItem(items, "create") {
		t.Fatal("expected 'create' in completions")
	}

	var createItem protocol.CompletionItem
	for _, item := range items {
		if item.Label == "create" {
			createItem = item
			break
		}
	}

	resolved, err := server.CompletionResolve(context.Background(), &createItem)
	if err != nil {
		t.Fatal(err)
	}

	doc, ok := resolved.Documentation.(protocol.MarkupContent)
	if !ok {
		t.Fatalf("expected MarkupContent documentation, got %T", resolved.Documentation)
	}
	if !strings.Contains(doc.Value, "Creates a new account") {
		t.Errorf("expected doc to contain 'Creates a new account', got: %s", doc.Value)
	}
	if !strings.Contains(doc.Value, "@spec create") {
		t.Errorf("expected doc to contain '@spec create', got: %s", doc.Value)
	}
}

func TestCompletionResolve_NoData(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	item := &protocol.CompletionItem{
		Label: "something",
	}
	resolved, err := server.CompletionResolve(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Documentation != nil {
		t.Error("expected nil documentation when no data is set")
	}
}

func TestCompletionResolve_PathTraversal(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	item := &protocol.CompletionItem{
		Label: "create",
		Data: map[string]interface{}{
			"filePath": "/etc/passwd",
			"line":     1,
		},
	}
	resolved, err := server.CompletionResolve(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Documentation != nil {
		t.Error("expected nil documentation for path outside project root")
	}
}

func TestCompletionResolve_StdlibPath(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	stdlibDir := t.TempDir()
	server.stdlibRoot = stdlibDir

	stdlibFile := filepath.Join(stdlibDir, "lib", "enum.ex")
	if err := os.MkdirAll(filepath.Dir(stdlibFile), 0755); err != nil {
		t.Fatal(err)
	}
	content := `defmodule Enum do
  @doc """
  Returns the count of elements in the enumerable.
  """
  @spec count(t()) :: non_neg_integer()
  def count(enumerable) do
    :erlang.length(enumerable)
  end
end
`
	if err := os.WriteFile(stdlibFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	item := &protocol.CompletionItem{
		Label: "count",
		Data: map[string]interface{}{
			"filePath": stdlibFile,
			"line":     float64(6),
		},
	}

	resolved, err := server.CompletionResolve(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}

	doc, ok := resolved.Documentation.(protocol.MarkupContent)
	if !ok {
		t.Fatalf("expected MarkupContent documentation, got %T", resolved.Documentation)
	}
	if !strings.Contains(doc.Value, "Returns the count") {
		t.Errorf("expected doc to contain 'Returns the count', got: %s", doc.Value)
	}
}

func TestCompletion_StdlibModule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	stdlibDir := t.TempDir()
	server.stdlibRoot = stdlibDir

	stdlibFile := filepath.Join(stdlibDir, "elixir", "lib", "enum.ex")
	if err := os.MkdirAll(filepath.Dir(stdlibFile), 0755); err != nil {
		t.Fatal(err)
	}
	content := `defmodule Enum do
  @doc """
  Returns the count.
  """
  def count(enumerable) do
    :erlang.length(enumerable)
  end

  def map(enumerable, fun) do
    :lists.map(fun, enumerable)
  end

  def filter(enumerable, fun) do
    :lists.filter(fun, enumerable)
  end

  defp reduce_list([], acc, _fun), do: acc
end
`
	if err := os.WriteFile(stdlibFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	defs, _, err := parser.ParseFile(stdlibFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.IndexFile(stdlibFile, defs); err != nil {
		t.Fatal(err)
	}

	uri := "file:///test.ex"
	server.docs.Set(uri, "  Enum.")

	items := completionAt(t, server, uri, 0, 7)
	if !hasCompletionItem(items, "count") {
		t.Error("expected 'count' in Enum completions")
	}
	if !hasCompletionItem(items, "map") {
		t.Error("expected 'map' in Enum completions")
	}
	if !hasCompletionItem(items, "filter") {
		t.Error("expected 'filter' in Enum completions")
	}
	if hasCompletionItem(items, "reduce_list") {
		t.Error("should not include private function 'reduce_list'")
	}
}

func TestCompletion_StdlibModulePrefix(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	stdlibDir := t.TempDir()
	server.stdlibRoot = stdlibDir

	stdlibFile := filepath.Join(stdlibDir, "elixir", "lib", "enum.ex")
	if err := os.MkdirAll(filepath.Dir(stdlibFile), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stdlibFile, []byte(`defmodule Enum do
end
`), 0644); err != nil {
		t.Fatal(err)
	}

	defs, _, err := parser.ParseFile(stdlibFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.IndexFile(stdlibFile, defs); err != nil {
		t.Fatal(err)
	}

	uri := "file:///test.ex"
	server.docs.Set(uri, "  Enu")

	items := completionAt(t, server, uri, 0, 5)
	if !hasCompletionItem(items, "Enum") {
		t.Error("expected 'Enum' in module prefix completions")
	}
}

func TestCompletion_UseInjectedImport(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Index the used module — __using__ imports itself (alias Schema → MyApp.Schema)
	indexFile(t, server.store, server.projectRoot, "lib/schema.ex", `defmodule MyApp.Schema do
  alias MyApp.Schema

  defmacro __using__(_opts) do
    quote do
      import Schema
    end
  end

  @doc "Defines a schema."
  defmacro schema(source, do: block) do
    :ok
  end

  defmacro embedded_schema(do: block) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.User do
  use MyApp.Schema

  sch
end`)

	// col=5 — cursor after "sch" (prefix "sch")
	items := completionAt(t, server, uri, 3, 5)
	if !hasCompletionItem(items, "schema") {
		t.Errorf("expected 'schema' in completions from use-injected import, got %v",
			func() []string {
				var labels []string
				for _, item := range items {
					labels = append(labels, item.Label)
				}
				return labels
			}())
	}
}

func TestCompletion_UseInjectedInlineDef(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/helpers.ex", `defmodule MyApp.Helpers do
  defmacro __using__(_opts) do
    quote do
      def double(x), do: x * 2
      def triple(x), do: x * 3
    end
  end
end
`)

	uri := "file:///test.ex"

	// "do" prefix — should match double
	server.docs.Set(uri, `defmodule MyApp.User do
  use MyApp.Helpers

  do
end`)
	items := completionAt(t, server, uri, 3, 4)
	if !hasCompletionItem(items, "double") {
		t.Error("expected 'double' in completions from use-injected inline def")
	}

	// "tr" prefix — should match triple
	server.docs.Set(uri, `defmodule MyApp.User do
  use MyApp.Helpers

  tr
end`)
	items = completionAt(t, server, uri, 3, 4)
	if !hasCompletionItem(items, "triple") {
		t.Error("expected 'triple' in completions from use-injected inline def")
	}
}

func TestDefinition_ModuleKeyword(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	src := `defmodule MyApp.Accounts do
  @moduledoc "Manages accounts."

  alias __MODULE__.User

  def get_user(id), do: id
end`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", src)
	fileURI := "file://" + filepath.Join(server.projectRoot, "lib/accounts.ex")
	server.docs.Set(fileURI, src)

	// col=9 is on '__MODULE__' in the alias line (line 3)
	locs := definitionAt(t, server, fileURI, 3, 9)
	if len(locs) == 0 {
		t.Fatal("expected definition for __MODULE__")
	}
	if locs[0].Range.Start.Line != 0 {
		t.Errorf("expected jump to defmodule on line 0, got line %d", locs[0].Range.Start.Line)
	}
}

func TestDefinition_ModuleKeywordSubmodule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts/user.ex", `defmodule MyApp.Accounts.User do
  def new, do: %{}
end`)
	src := `defmodule MyApp.Accounts do
  alias __MODULE__.User

  def get_user(id), do: User.new()
end`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", src)
	fileURI := "file://" + filepath.Join(server.projectRoot, "lib/accounts.ex")
	server.docs.Set(fileURI, src)

	// col=20 is on 'User' in alias __MODULE__.User (line 1)
	locs := definitionAt(t, server, fileURI, 1, 20)
	if len(locs) == 0 {
		t.Fatal("expected definition for __MODULE__.User")
	}
	if locs[0].Range.Start.Line != 0 {
		t.Errorf("expected jump to MyApp.Accounts.User defmodule on line 0, got line %d", locs[0].Range.Start.Line)
	}
}

func TestDefinition_KernelAutoImport(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/kernel.ex", `defmodule Kernel do
  def to_timeout(duration), do: duration
end`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.Worker do
  def run do
    to_timeout({:second, 5})
  end
end`)

	// col=5 is on 'to_timeout' (line 2)
	locs := definitionAt(t, server, uri, 2, 5)
	if len(locs) == 0 {
		t.Fatal("expected definition for Kernel auto-imported to_timeout")
	}
}

func TestDefinition_UsingInDocstring(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Regression: parseUsingBody was matching `defmacro __using__` inside @moduledoc
	// example code (heredoc) before finding the real implementation, causing
	// cachedUsing to return stale/wrong data and go-to-definition to fail.
	macroProviderSrc := `defmodule MyApp.MacroProvider do
  def embedded_schema(block), do: block
end`
	schemaBaseSrc := `defmodule MyApp.SchemaBase do
  @moduledoc """
  Example usage:

      defmodule MyApp.Schema do
        defmacro __using__(_) do
          quote do
            use MyApp.SchemaBase
          end
        end
      end

  """

  defmacro __using__(_) do
    quote do
      import MyApp.MacroProvider
    end
  end
end`
	callerSrc := `defmodule MyApp.Record do
  use MyApp.SchemaBase

  embedded_schema do
    :ok
  end
end`

	indexFile(t, server.store, server.projectRoot, "lib/macro_provider.ex", macroProviderSrc)
	indexFile(t, server.store, server.projectRoot, "lib/schema_base.ex", schemaBaseSrc)
	schemaBaseURI := "file://" + filepath.Join(server.projectRoot, "lib/schema_base.ex")
	server.docs.Set(schemaBaseURI, schemaBaseSrc)

	callerURI := "file://" + filepath.Join(server.projectRoot, "lib/record.ex")
	server.docs.Set(callerURI, callerSrc)

	// col=2 is on 'embedded_schema' (line 3 in callerSrc, 0-indexed)
	locs := definitionAt(t, server, callerURI, 3, 2)
	if len(locs) == 0 {
		t.Fatal("expected go-to-definition for bare macro call; __using__ in docstring should not shadow real __using__")
	}
}

func TestDefinition_BareTypeReference(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Regression: bare type references (e.g. charge_type()) inside the same module
	// were not resolved because FindFunctionDefinition only checked FuncDefRe, not TypeDefRe.
	src := `defmodule MyApp.Payment do
  @type charge_type :: :OUR | :BEN | :SHA

  @type params :: %{
    required(:charge) => charge_type()
  }
end`
	fileURI := "file:///test.ex"
	server.docs.Set(fileURI, src)

	// col=26 is on 'charge_type' in the @type params line (line 4)
	locs := definitionAt(t, server, fileURI, 4, 26)
	if len(locs) == 0 {
		t.Fatal("expected go-to-definition for bare type reference charge_type()")
	}
	// Should jump to the @type charge_type definition on line 1 (0-indexed)
	if locs[0].Range.Start.Line != 1 {
		t.Errorf("expected jump to @type charge_type on line 1, got line %d", locs[0].Range.Start.Line)
	}
}

func TestDefinition_BareTypeReferenceInStructType(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Regression: bare type reference inside a struct type definition.
	src := `defmodule MyApp.Order do
  @type status :: :pending | :complete

  @type t :: %__MODULE__{
    status: status()
  }
end`
	fileURI := "file:///order.ex"
	server.docs.Set(fileURI, src)

	// col=13 is on 'status' in the @type t definition (line 4)
	locs := definitionAt(t, server, fileURI, 4, 13)
	if len(locs) == 0 {
		t.Fatal("expected go-to-definition for bare type reference status()")
	}
	if locs[0].Range.Start.Line != 1 {
		t.Errorf("expected jump to @type status on line 1, got line %d", locs[0].Range.Start.Line)
	}
}

func TestReferences_TransitiveUseChain(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// The macro provider defines `special_field` as a macro.
	macroProviderSrc := `defmodule MyApp.MacroProvider do
  defmacro special_field(name, type) do
    quote do: :ok
  end
end`

	// The base worker injects the macro provider via its __using__.
	baseWorkerSrc := `defmodule MyApp.BaseWorker do
  defmacro __using__(_opts) do
    quote do
      import MyApp.MacroProvider
    end
  end
end`

	// The concrete worker uses BaseWorker, making special_field available.
	workerSrc := `defmodule MyApp.ConcreteWorker do
  use MyApp.BaseWorker

  special_field :name, :string
end`

	indexFile(t, server.store, server.projectRoot, "lib/macro_provider.ex", macroProviderSrc)
	indexFile(t, server.store, server.projectRoot, "lib/base_worker.ex", baseWorkerSrc)
	workerPath := filepath.Join(server.projectRoot, "lib/worker.ex")
	indexFile(t, server.store, server.projectRoot, "lib/worker.ex", workerSrc)

	macroProviderURI := "file://" + filepath.Join(server.projectRoot, "lib/macro_provider.ex")
	server.docs.Set(macroProviderURI, macroProviderSrc)
	baseWorkerURI := "file://" + filepath.Join(server.projectRoot, "lib/base_worker.ex")
	server.docs.Set(baseWorkerURI, baseWorkerSrc)
	workerURI := "file://" + filepath.Join(server.projectRoot, "lib/worker.ex")
	server.docs.Set(workerURI, workerSrc)

	// Hovering on `special_field` at line 1 of macro_provider.ex (col on the name)
	locs := referencesAt(t, server, macroProviderURI, 1, 13)
	if len(locs) == 0 {
		t.Fatal("expected references for special_field via transitive use chain")
	}
	found := false
	for _, loc := range locs {
		if strings.Contains(string(loc.URI), "worker.ex") && loc.Range.Start.Line == 3 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected worker.ex:3 in references, got: %v", locs)
	}
	_ = workerPath
}

func TestReferences_DeepTransitiveUseChain(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Regression: `use A` → A.__using__ calls `use B` → B.__using__ calls `use C`
	// → C defines a macro. go-to-references on C.macro should find callers even
	// when the use chain is 3 hops deep.
	definerSrc := `defmodule MyApp.MacroDefs do
  defmacro schema_field(name) do
    quote do: :ok
  end
end`
	levelCSrc := `defmodule MyApp.Level.C do
  defmacro __using__(_) do
    quote do
      import MyApp.MacroDefs
    end
  end
end`
	levelBSrc := `defmodule MyApp.Level.B do
  defmacro __using__(_) do
    quote do
      use MyApp.Level.C
    end
  end
end`
	levelASrc := `defmodule MyApp.Level.A do
  defmacro __using__(_) do
    quote do
      use MyApp.Level.B
    end
  end
end`
	callerSrc := `defmodule MyApp.Caller do
  use MyApp.Level.A

  schema_field :name
end`

	indexFile(t, server.store, server.projectRoot, "lib/macro_defs.ex", definerSrc)
	indexFile(t, server.store, server.projectRoot, "lib/level_c.ex", levelCSrc)
	indexFile(t, server.store, server.projectRoot, "lib/level_b.ex", levelBSrc)
	indexFile(t, server.store, server.projectRoot, "lib/level_a.ex", levelASrc)
	indexFile(t, server.store, server.projectRoot, "lib/caller.ex", callerSrc)

	definerURI := "file://" + filepath.Join(server.projectRoot, "lib/macro_defs.ex")
	server.docs.Set(definerURI, definerSrc)
	callerURI := "file://" + filepath.Join(server.projectRoot, "lib/caller.ex")
	server.docs.Set(callerURI, callerSrc)
	for _, f := range []struct{ uri, src string }{
		{"file://" + filepath.Join(server.projectRoot, "lib/level_c.ex"), levelCSrc},
		{"file://" + filepath.Join(server.projectRoot, "lib/level_b.ex"), levelBSrc},
		{"file://" + filepath.Join(server.projectRoot, "lib/level_a.ex"), levelASrc},
	} {
		server.docs.Set(f.uri, f.src)
	}

	// col=13 is on `schema_field` definition in macro_defs.ex (line 1)
	locs := referencesAt(t, server, definerURI, 1, 13)
	if len(locs) == 0 {
		t.Fatal("expected references for schema_field via 3-hop transitive use chain")
	}
	found := false
	for _, loc := range locs {
		if strings.Contains(string(loc.URI), "caller.ex") && loc.Range.Start.Line == 3 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected caller.ex:3 in references for deep transitive use chain, got: %v", locs)
	}
}

func TestDetectElixirStdlibRoot(t *testing.T) {
	root, ok := stdlib.DetectElixirLibRoot()
	if !ok {
		t.Skip("elixir not available in PATH")
	}

	enumPath := filepath.Join(root, "elixir", "lib", "enum.ex")
	if _, err := os.Stat(enumPath); os.IsNotExist(err) {
		t.Errorf("expected stdlib enum.ex at %s", enumPath)
	}
}

// === DocumentSymbol tests ===

func documentSymbols(t *testing.T, server *Server, docURI string) []protocol.DocumentSymbol {
	t.Helper()
	result, err := server.DocumentSymbol(context.Background(), &protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var symbols []protocol.DocumentSymbol
	for _, item := range result {
		if sym, ok := item.(protocol.DocumentSymbol); ok {
			symbols = append(symbols, sym)
		}
	}
	return symbols
}

func findSymbol(symbols []protocol.DocumentSymbol, name string) *protocol.DocumentSymbol {
	for i := range symbols {
		if symbols[i].Name == name {
			return &symbols[i]
		}
		if found := findSymbol(symbols[i].Children, name); found != nil {
			return found
		}
	}
	return nil
}

func collectNames(symbols []protocol.DocumentSymbol) []string {
	var names []string
	for _, s := range symbols {
		names = append(names, s.Name)
	}
	return names
}

func TestDocumentSymbol_BasicModule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.Accounts do
  @type status :: :active | :inactive

  defstruct [:name, :email]

  def list_users do
    []
  end

  defp format_user(user) do
    user
  end

  defmacro is_admin(user) do
    quote do: unquote(user).role == :admin
  end

  @opaque internal_state :: map()

  defdelegate create(params), to: MyApp.Creator
end
`
	docURI := "file:///test/accounts.ex"
	server.docs.Set(docURI, content)

	symbols := documentSymbols(t, server, docURI)

	if len(symbols) != 1 {
		t.Fatalf("expected 1 top-level symbol, got %d", len(symbols))
	}

	mod := symbols[0]
	if mod.Name != "MyApp.Accounts" {
		t.Errorf("expected module name MyApp.Accounts, got %q", mod.Name)
	}
	if mod.Kind != protocol.SymbolKindModule {
		t.Errorf("expected Module kind, got %v", mod.Kind)
	}
	if mod.Detail != "defmodule" {
		t.Errorf("expected defmodule detail, got %q", mod.Detail)
	}

	childNames := collectNames(mod.Children)
	expectedChildren := []string{"status/0", "defstruct", "list_users/0", "format_user/1", "is_admin/1", "internal_state/0", "create/1"}
	if len(childNames) != len(expectedChildren) {
		t.Fatalf("expected %d children, got %d: %v", len(expectedChildren), len(childNames), childNames)
	}
	for i, name := range expectedChildren {
		if childNames[i] != name {
			t.Errorf("child %d: expected %q, got %q", i, name, childNames[i])
		}
	}

	// Verify kinds
	if s := findSymbol(symbols, "status/0"); s != nil && s.Kind != protocol.SymbolKindTypeParameter {
		t.Errorf("expected TypeParameter for @type, got %v", s.Kind)
	}
	if s := findSymbol(symbols, "defstruct"); s != nil && s.Kind != protocol.SymbolKindStruct {
		t.Errorf("expected Struct for defstruct, got %v", s.Kind)
	}
	if s := findSymbol(symbols, "list_users/0"); s != nil && s.Kind != protocol.SymbolKindFunction {
		t.Errorf("expected Function for def, got %v", s.Kind)
	}
	if s := findSymbol(symbols, "list_users/0"); s != nil && s.Detail != "def" {
		t.Errorf("expected def detail, got %q", s.Detail)
	}
	if s := findSymbol(symbols, "format_user/1"); s != nil && s.Detail != "defp" {
		t.Errorf("expected defp detail, got %q", s.Detail)
	}
	if s := findSymbol(symbols, "is_admin/1"); s != nil && s.Detail != "defmacro" {
		t.Errorf("expected defmacro detail, got %q", s.Detail)
	}
}

func TestDocumentSymbol_NestedModules(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.Parent do
  def parent_func, do: :ok

  defmodule Child do
    def child_func, do: :ok

    defmodule GrandChild do
      def grandchild_func, do: :ok
    end
  end
end
`
	docURI := "file:///test/nested.ex"
	server.docs.Set(docURI, content)

	symbols := documentSymbols(t, server, docURI)
	if len(symbols) != 1 {
		t.Fatalf("expected 1 top-level, got %d", len(symbols))
	}

	parent := symbols[0]
	if parent.Name != "MyApp.Parent" {
		t.Errorf("expected MyApp.Parent, got %q", parent.Name)
	}

	// Parent should have parent_func and Child as children
	childNames := collectNames(parent.Children)
	if len(childNames) != 2 {
		t.Fatalf("expected 2 children of Parent, got %d: %v", len(childNames), childNames)
	}
	if childNames[0] != "parent_func/0" {
		t.Errorf("expected parent_func/0, got %q", childNames[0])
	}
	if childNames[1] != "Child" {
		t.Errorf("expected Child, got %q", childNames[1])
	}

	// Child should have child_func and GrandChild
	child := findSymbol(parent.Children, "Child")
	if child == nil {
		t.Fatal("Child not found")
	}
	grandChild := findSymbol(child.Children, "GrandChild")
	if grandChild == nil {
		t.Fatal("GrandChild not found")
	}
	if findSymbol(grandChild.Children, "grandchild_func/0") == nil {
		t.Error("grandchild_func/0 not found in GrandChild")
	}
}

func TestDocumentSymbol_FunctionBodyRanges(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  def multi_line(x) do
    x + 1
  end

  def inline(x), do: x
end
`
	docURI := "file:///test/ranges.ex"
	server.docs.Set(docURI, content)

	symbols := documentSymbols(t, server, docURI)
	mod := symbols[0]

	multiLine := findSymbol(mod.Children, "multi_line/1")
	if multiLine == nil {
		t.Fatal("multi_line/1 not found")
	}
	// multi_line should span from def line to end line (lines 1-3, 0-based)
	if multiLine.Range.Start.Line != 1 {
		t.Errorf("multi_line start line: expected 1, got %d", multiLine.Range.Start.Line)
	}
	if multiLine.Range.End.Line != 3 {
		t.Errorf("multi_line end line: expected 3, got %d", multiLine.Range.End.Line)
	}

	inline := findSymbol(mod.Children, "inline/1")
	if inline == nil {
		t.Fatal("inline/1 not found")
	}
	// inline should be single-line (line 5, 0-based)
	if inline.Range.Start.Line != 5 {
		t.Errorf("inline start line: expected 5, got %d", inline.Range.Start.Line)
	}
	if inline.Range.End.Line != 5 {
		t.Errorf("inline end line: expected 5, got %d", inline.Range.End.Line)
	}
}

func TestDocumentSymbol_SelectionRangeContainedInRange(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.Accounts do
  def list_users do
    []
  end

  defmodule Permissions do
    def can_edit?(user) do
      true
    end
  end
end
`
	docURI := "file:///test/contained.ex"
	server.docs.Set(docURI, content)

	symbols := documentSymbols(t, server, docURI)

	var checkContainment func(syms []protocol.DocumentSymbol, path string)
	checkContainment = func(syms []protocol.DocumentSymbol, path string) {
		for _, s := range syms {
			fullPath := path + "/" + s.Name
			r := s.Range
			sr := s.SelectionRange

			if sr.Start.Line < r.Start.Line || sr.End.Line > r.End.Line {
				t.Errorf("%s: selectionRange lines [%d-%d] not within range lines [%d-%d]",
					fullPath, sr.Start.Line, sr.End.Line, r.Start.Line, r.End.Line)
			}
			if sr.Start.Line == r.Start.Line && sr.Start.Character < r.Start.Character {
				t.Errorf("%s: selectionRange start char %d before range start char %d",
					fullPath, sr.Start.Character, r.Start.Character)
			}
			if sr.End.Line == r.End.Line && sr.End.Character > r.End.Character {
				t.Errorf("%s: selectionRange end char %d after range end char %d",
					fullPath, sr.End.Character, r.End.Character)
			}
			checkContainment(s.Children, fullPath)
		}
	}
	checkContainment(symbols, "")
}

func TestDocumentSymbol_DescribeTestBlocks(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyAppTest do
  use ExUnit.Case

  describe "user creation" do
    setup do
      {:ok, user: build(:user)}
    end

    test "creates a valid user" do
      assert true
    end

    test "fails with invalid data" do
      assert true
    end
  end

  defp build_user(attrs) do
    Map.merge(%{name: "test"}, attrs)
  end

  describe "user deletion" do
    test "deletes user" do
      assert true
    end
  end
end
`
	docURI := "file:///test/my_test.exs"
	server.docs.Set(docURI, content)

	symbols := documentSymbols(t, server, docURI)
	mod := symbols[0]

	// Should have 2 describe blocks + 1 defp as direct children of the module
	describes := []protocol.DocumentSymbol{}
	var privateFn *protocol.DocumentSymbol
	for i, c := range mod.Children {
		if c.Detail == "describe" {
			describes = append(describes, c)
		}
		if c.Detail == "defp" {
			privateFn = &mod.Children[i]
		}
	}
	if len(describes) != 2 {
		t.Fatalf("expected 2 describe blocks, got %d", len(describes))
	}
	if privateFn == nil {
		t.Fatal("expected defp build_user/1 as direct child of module")
	}
	if privateFn.Name != "build_user/1" {
		t.Errorf("expected build_user/1, got %q", privateFn.Name)
	}

	// Verify ordering: first describe, then defp, then second describe
	childNames := collectNames(mod.Children)
	if len(childNames) != 3 {
		t.Fatalf("expected 3 direct children of module, got %d: %v", len(childNames), childNames)
	}
	if childNames[0] != "describe user creation" {
		t.Errorf("expected first child 'describe user creation', got %q", childNames[0])
	}
	if childNames[1] != "build_user/1" {
		t.Errorf("expected second child 'build_user/1', got %q", childNames[1])
	}
	if childNames[2] != "describe user deletion" {
		t.Errorf("expected third child 'describe user deletion', got %q", childNames[2])
	}

	// First describe should have setup + 2 tests as children
	desc1 := describes[0]
	if desc1.Name != "describe user creation" {
		t.Errorf("expected 'describe user creation', got %q", desc1.Name)
	}
	if len(desc1.Children) != 3 {
		t.Fatalf("expected 3 children in first describe, got %d: %v", len(desc1.Children), collectNames(desc1.Children))
	}

	// Verify setup is a child
	if desc1.Children[0].Detail != "setup" {
		t.Errorf("expected setup as first child, got %q", desc1.Children[0].Detail)
	}
	// Verify tests are children
	if desc1.Children[1].Detail != "test" {
		t.Errorf("expected test as second child, got detail=%q", desc1.Children[1].Detail)
	}

	// Second describe should have 1 test
	desc2 := describes[1]
	if len(desc2.Children) != 1 {
		t.Fatalf("expected 1 child in second describe, got %d", len(desc2.Children))
	}
}

func TestDocumentSymbol_BrokenCode(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.InProgress do
  def completed_func(x) do
    x + 1
  end

  def half_written_func(

  defp another_complete(y) do
    y * 2
  end
`
	docURI := "file:///test/broken.ex"
	server.docs.Set(docURI, content)

	symbols := documentSymbols(t, server, docURI)
	if len(symbols) != 1 {
		t.Fatalf("expected 1 top-level symbol even with broken code, got %d", len(symbols))
	}

	mod := symbols[0]
	names := collectNames(mod.Children)
	// Should still find all three functions despite the broken one
	if len(names) < 2 {
		t.Errorf("expected at least 2 children from broken file, got %d: %v", len(names), names)
	}
	if findSymbol(mod.Children, "completed_func/1") == nil {
		t.Error("completed_func/1 should still be found in broken code")
	}
	if findSymbol(mod.Children, "another_complete/1") == nil {
		t.Error("another_complete/1 should still be found in broken code")
	}
}

func TestDocumentSymbol_Protocol(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defprotocol Printable do
  @spec to_string(t) :: String.t()
  def to_string(data)
end
`
	docURI := "file:///test/protocol.ex"
	server.docs.Set(docURI, content)

	symbols := documentSymbols(t, server, docURI)
	if len(symbols) != 1 {
		t.Fatalf("expected 1 symbol, got %d", len(symbols))
	}
	if symbols[0].Kind != protocol.SymbolKindInterface {
		t.Errorf("expected Interface kind for defprotocol, got %v", symbols[0].Kind)
	}
}

func TestDocumentSymbol_EmptyFile(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	server.docs.Set("file:///test/empty.ex", "")
	symbols := documentSymbols(t, server, "file:///test/empty.ex")
	if len(symbols) != 0 {
		t.Errorf("expected 0 symbols for empty file, got %d", len(symbols))
	}
}

func TestDocumentSymbol_UnopenedFile(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	symbols := documentSymbols(t, server, "file:///test/nonexistent.ex")
	if len(symbols) != 0 {
		t.Errorf("expected 0 symbols for unopened file, got %d", len(symbols))
	}
}

// === Workspace Symbol tests ===

func workspaceSymbols(t *testing.T, server *Server, query string) []protocol.SymbolInformation {
	t.Helper()
	result, err := server.Symbols(context.Background(), &protocol.WorkspaceSymbolParams{
		Query: query,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWorkspaceSymbol_SearchModules(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def list_users, do: []
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/users.ex", `defmodule MyApp.Users do
  def get(id), do: nil
end
`)

	results := workspaceSymbols(t, server, "Accounts")
	found := false
	for _, r := range results {
		if r.Name == "MyApp.Accounts" {
			found = true
			if r.Kind != protocol.SymbolKindModule {
				t.Errorf("expected Module kind, got %v", r.Kind)
			}
			break
		}
	}
	if !found {
		t.Error("expected to find MyApp.Accounts in results")
	}

	// Should not find Users when searching for Accounts
	for _, r := range results {
		if strings.Contains(r.Name, "Users") && !strings.Contains(r.Name, "Accounts") {
			t.Errorf("unexpected result %q when searching for Accounts", r.Name)
		}
	}
}

func TestWorkspaceSymbol_SearchFunctions(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def list_users, do: []
  def create_user(attrs), do: attrs
end
`)

	results := workspaceSymbols(t, server, "list_users")
	found := false
	for _, r := range results {
		if strings.Contains(r.Name, "list_users") {
			found = true
			if r.Kind != protocol.SymbolKindFunction {
				t.Errorf("expected Function kind, got %v", r.Kind)
			}
			if r.ContainerName != "MyApp.Accounts" {
				t.Errorf("expected container MyApp.Accounts, got %q", r.ContainerName)
			}
			break
		}
	}
	if !found {
		t.Error("expected to find list_users in results")
	}
}

func TestWorkspaceSymbol_EmptyQuery(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/foo.ex", `defmodule MyApp.Foo do
  def bar, do: :ok
end
`)

	results := workspaceSymbols(t, server, "")
	if len(results) != 0 {
		t.Errorf("expected 0 results for empty query, got %d", len(results))
	}
}

func TestWorkspaceSymbol_ExcludesStdlib(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Simulate stdlib by setting stdlibRoot and indexing a file under it
	stdlibDir := filepath.Join(server.projectRoot, "stdlib")
	server.stdlibRoot = stdlibDir

	indexFile(t, server.store, server.projectRoot, "stdlib/elixir/lib/enum.ex", `defmodule Enum do
  def map(list, fun), do: list
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/my_enum.ex", `defmodule MyEnum do
  def my_map(list), do: list
end
`)

	results := workspaceSymbols(t, server, "Enum")
	for _, r := range results {
		if r.Name == "Enum" {
			t.Error("stdlib module Enum should be excluded from workspace symbols")
		}
	}

	// But MyEnum should be found
	found := false
	for _, r := range results {
		if r.Name == "MyEnum" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected to find MyEnum in results")
	}
}

func TestWorkspaceSymbol_LocationIsCorrect(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def list_users, do: []
end
`)

	results := workspaceSymbols(t, server, "list_users")
	for _, r := range results {
		if strings.Contains(r.Name, "list_users") {
			expectedPath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
			gotPath := uri.URI(r.Location.URI).Filename()
			if gotPath != expectedPath {
				t.Errorf("expected path %q, got %q", expectedPath, gotPath)
			}
			// list_users is on line 2 (1-indexed) = line 1 (0-indexed)
			if r.Location.Range.Start.Line != 1 {
				t.Errorf("expected line 1, got %d", r.Location.Range.Start.Line)
			}
			return
		}
	}
	t.Error("list_users not found in results")
}

func TestWorkspaceSymbol_KindMapping(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/kinds.ex", `defmodule MyApp.Kinds do
  @type my_type :: atom()
  defstruct [:field]
  def my_func, do: :ok
  defmacro my_macro, do: :ok
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/my_protocol.ex", `defprotocol MyApp.MyProtocol do
  def to_string(data)
end
`)

	tests := []struct {
		query    string
		expected protocol.SymbolKind
	}{
		{"MyApp.Kinds", protocol.SymbolKindModule},
		{"my_type", protocol.SymbolKindTypeParameter},
		{"__struct__", protocol.SymbolKindStruct},
		{"my_func", protocol.SymbolKindFunction},
		{"my_macro", protocol.SymbolKindFunction},
		{"MyApp.MyProtocol", protocol.SymbolKindInterface},
	}

	for _, tt := range tests {
		results := workspaceSymbols(t, server, tt.query)
		found := false
		for _, r := range results {
			if strings.Contains(r.Name, tt.query) {
				found = true
				if r.Kind != tt.expected {
					t.Errorf("query %q: expected kind %v, got %v", tt.query, tt.expected, r.Kind)
				}
				break
			}
		}
		if !found {
			names := []string{}
			for _, r := range results {
				names = append(names, r.Name)
			}
			t.Errorf("query %q: not found in results: %v", tt.query, names)
		}
	}
}

func TestDocumentSymbol_NameWithArity(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  def zero_arity, do: :ok
  def one_arity(a), do: a
  def two_arity(a, b), do: {a, b}
  def default_args(a, b \\ nil), do: {a, b}
end
`
	docURI := "file:///test/arity.ex"
	server.docs.Set(docURI, content)

	symbols := documentSymbols(t, server, docURI)
	mod := symbols[0]

	expected := map[string]bool{
		"zero_arity/0":   true,
		"one_arity/1":    true,
		"two_arity/2":    true,
		"default_args/2": true,
	}

	for _, c := range mod.Children {
		if !expected[c.Name] {
			t.Errorf("unexpected symbol %q", c.Name)
		}
		delete(expected, c.Name)
	}
	for name := range expected {
		t.Errorf("missing expected symbol %q", name)
	}
}

// Verify capabilities are advertised
func TestServer_Capabilities_DocumentSymbolAndWorkspaceSymbol(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	result, err := server.Initialize(context.Background(), &protocol.InitializeParams{
		RootURI: protocol.DocumentURI(fmt.Sprintf("file://%s", server.projectRoot)),
	})
	if err != nil {
		t.Fatal(err)
	}

	caps := result.Capabilities
	if caps.DocumentSymbolProvider != true {
		t.Error("DocumentSymbolProvider should be true")
	}
	if caps.WorkspaceSymbolProvider != true {
		t.Error("WorkspaceSymbolProvider should be true")
	}
}
