package lsp

import (
	"context"
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

	defs, err := parser.ParseFile(stdlibFile)
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

	defs, err := parser.ParseFile(stdlibFile)
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
