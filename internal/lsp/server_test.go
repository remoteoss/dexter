package lsp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/stdlib"
	"github.com/remoteoss/dexter/internal/store"
)

func setupTestServer(t *testing.T) (*Server, func()) {
	t.Helper()
	dir := t.TempDir()

	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	server := NewServer(s, dir)
	server.snippetSupport = true

	// Resolve the mix binary so formatting tests work
	if p, err := exec.LookPath("mix"); err == nil {
		server.mixBin = p
	}

	return server, func() {
		server.backgroundWork.Wait()
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
		if item.Label == label || item.FilterText == label {
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

	var foundCreate, foundAll bool
	for _, item := range items {
		if item.Label == "create/2" {
			foundCreate = true
			if item.InsertText != "create(${1:name}, ${2:email})$0" {
				t.Errorf("create/2: expected snippet insert text, got %q", item.InsertText)
			}
			if item.InsertTextFormat != protocol.InsertTextFormatSnippet {
				t.Errorf("create/2: expected snippet format, got %v", item.InsertTextFormat)
			}
		}
		if item.Label == "all/0" {
			foundAll = true
			if item.InsertText != "all()" {
				t.Errorf("all/0: expected plain call for zero-arity, got %q", item.InsertText)
			}
			if item.InsertTextFormat == protocol.InsertTextFormatSnippet {
				t.Error("all/0: should not have snippet format for zero-arity")
			}
		}
	}
	if !foundCreate {
		t.Error("expected to find completion item create/2")
	}
	if !foundAll {
		t.Error("expected to find completion item all/0")
	}
}

func TestCompletion_PipeSnippet(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def transform(data, format, opts) do
    :ok
  end

  def validate(data) do
    :ok
  end
end
`)

	uri := "file:///test.ex"

	// Pipe context: first arg should be omitted from snippet
	server.docs.Set(uri, "  |> MyApp.Accounts.trans")
	items := completionAt(t, server, uri, 0, 25)
	for _, item := range items {
		if item.Label == "transform/3" {
			if item.InsertText != "transform(${1:format}, ${2:opts})$0" {
				t.Errorf("pipe transform/3: expected pipe snippet, got %q", item.InsertText)
			}
			break
		}
	}

	// Single-arity in pipe: should become zero-arg call
	server.docs.Set(uri, "  |> MyApp.Accounts.vali")
	items = completionAt(t, server, uri, 0, 24)
	for _, item := range items {
		if item.Label == "validate/1" {
			if item.InsertText != "validate()" {
				t.Errorf("pipe validate/1: expected empty parens, got %q", item.InsertText)
			}
			break
		}
	}

	// Non-pipe context: all args should be present
	server.docs.Set(uri, "  MyApp.Accounts.trans")
	items = completionAt(t, server, uri, 0, 21)
	for _, item := range items {
		if item.Label == "transform/3" {
			if item.InsertText != "transform(${1:data}, ${2:format}, ${3:opts})$0" {
				t.Errorf("non-pipe transform/3: expected full snippet, got %q", item.InsertText)
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
		if item.FilterText == "fetch" {
			fetchCount++
		}
	}
	if fetchCount != 2 {
		t.Errorf("expected 2 fetch completions (arity 1 and 2), got %d", fetchCount)
	}
	if !hasCompletionItem(items, "fetch/1") {
		t.Error("expected 'fetch/1' label in completions")
	}
	if !hasCompletionItem(items, "fetch/2") {
		t.Error("expected 'fetch/2' label in completions")
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

func TestCompletion_TypesAfterDot(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  @type status :: :active | :inactive
  @opaque token :: binary()
  @typep internal_id :: integer()

  def create(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Accounts.")

	items := completionAt(t, server, uri, 0, 17)
	if !hasCompletionItem(items, "status") {
		t.Error("expected public @type 'status' in completions")
	}
	if !hasCompletionItem(items, "token") {
		t.Error("expected @opaque 'token' in completions")
	}
	if hasCompletionItem(items, "internal_id") {
		t.Error("should not include private @typep 'internal_id' from another module")
	}

	var statusItem *protocol.CompletionItem
	for _, item := range items {
		if item.FilterText == "status" || item.Label == "status" {
			item := item
			statusItem = &item
			break
		}
	}
	if statusItem == nil {
		t.Fatal("status item not found")
	}
	if statusItem.Kind != protocol.CompletionItemKindTypeParameter {
		t.Errorf("expected TypeParameter kind for @type, got %v", statusItem.Kind)
	}
}

func TestCompletion_BufferLocalTypes(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  @type status :: :active | :inactive
  @typep internal_id :: integer()

  sta
end`)

	// "sta" prefix should surface @type status
	items := completionAt(t, server, uri, 4, 5)
	if !hasCompletionItem(items, "status") {
		t.Error("expected @type 'status' from buffer")
	}

	// "inter" prefix should surface @typep internal_id (private types visible in same file)
	server.docs.Set(uri, `defmodule MyModule do
  @type status :: :active | :inactive
  @typep internal_id :: integer()

  inter
end`)
	items = completionAt(t, server, uri, 4, 7)
	if !hasCompletionItem(items, "internal_id") {
		t.Error("expected @typep 'internal_id' from same buffer")
	}
}

func TestCompletion_Variables(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  def process(user, account) do
    name = user.name
    email = user.email
    na
  end
end`)

	items := completionAt(t, server, uri, 4, 6)
	if !hasCompletionItem(items, "name") {
		t.Error("expected variable 'name' in completions")
	}
	if hasCompletionItem(items, "email") {
		t.Error("should not include 'email' — doesn't match prefix 'na'")
	}

	// Check it returns the right kind
	for _, item := range items {
		if item.Label == "name" {
			if item.Kind != protocol.CompletionItemKindVariable {
				t.Errorf("expected Variable kind, got %v", item.Kind)
			}
			break
		}
	}
}

func TestCompletion_VariablesIncludesParams(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  def process(user_data, opts) do
    us
  end
end`)

	items := completionAt(t, server, uri, 2, 6)
	if !hasCompletionItem(items, "user_data") {
		t.Error("expected function param 'user_data' in completions")
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
		if item.FilterText == "create" {
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
	path := filepath.Join(server.projectRoot, "lib", "payment.ex")
	indexFile(t, server.store, server.projectRoot, "lib/payment.ex", src)
	fileURI := "file://" + path
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
	path := filepath.Join(server.projectRoot, "lib", "order.ex")
	indexFile(t, server.store, server.projectRoot, "lib/order.ex", src)
	fileURI := "file://" + path
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

func TestDefinition_Variable(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  def process(user) do
    name = user.name
    IO.puts(name)
  end
end`)

	// Cursor on "name" at line 3 col 12 (the reference in IO.puts)
	locs := definitionAt(t, server, uri, 3, 12)
	if len(locs) == 0 {
		t.Fatal("expected go-to-definition for variable 'name'")
	}
	// Should jump to line 2 where name is first assigned
	if locs[0].Range.Start.Line != 2 {
		t.Errorf("expected definition on line 2, got line %d", locs[0].Range.Start.Line)
	}
}

func TestDefinition_VariableParam(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  def process(user) do
    IO.puts(user)
  end
end`)

	// Cursor on "user" at line 2 col 12 (the reference in IO.puts)
	locs := definitionAt(t, server, uri, 2, 12)
	if len(locs) == 0 {
		t.Fatal("expected go-to-definition for param 'user'")
	}
	// Should jump to line 1 where user is the function param
	if locs[0].Range.Start.Line != 1 {
		t.Errorf("expected definition on line 1, got line %d", locs[0].Range.Start.Line)
	}
}

func TestDefinition_ImportedFunctionViaUseChain(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Regression: `import MyApp.Factory` where MyApp.Factory uses ExMachina
	// (which injects `insert` via __using__). go-to-definition on `insert`
	// was returning nil because the import lookup only checked direct
	// definitions, not the imported module's use chain.
	exMachinaSrc := `defmodule MyApp.ExMachina do
  defmacro __using__(_opts) do
    quote do
      def insert(factory_name, attrs \\ []), do: :ok
    end
  end
end`
	factorySrc := `defmodule MyApp.Factory do
  use MyApp.ExMachina
end`
	callerSrc := `defmodule MyApp.SomeTest do
  import MyApp.Factory

  def run do
    insert(:user)
  end
end`

	indexFile(t, server.store, server.projectRoot, "lib/ex_machina.ex", exMachinaSrc)
	exMachinaURI := "file://" + filepath.Join(server.projectRoot, "lib/ex_machina.ex")
	server.docs.Set(exMachinaURI, exMachinaSrc)

	indexFile(t, server.store, server.projectRoot, "lib/factory.ex", factorySrc)
	factoryURI := "file://" + filepath.Join(server.projectRoot, "lib/factory.ex")
	server.docs.Set(factoryURI, factorySrc)

	callerURI := "file://" + filepath.Join(server.projectRoot, "test/some_test.exs")
	server.docs.Set(callerURI, callerSrc)

	// col=4 is on `insert` (line 4, 0-indexed)
	locs := definitionAt(t, server, callerURI, 4, 4)
	if len(locs) == 0 {
		t.Fatal("expected go-to-definition for `insert` imported from a module that uses ExMachina")
	}
}

func TestReferences_UseWithOptOverride(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Remote.Mox: imports the `mod` opt (default Mox) via unquote
	indexFile(t, server.store, server.projectRoot, "lib/mox.ex", `defmodule Remote.Mox do
  defmacro __using__(opts \\ []) do
    mod = Keyword.get(opts, :mod, Mox)
    quote do
      import unquote(mod)
    end
  end
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/mox_lib.ex", `defmodule Mox do
  def expect(mock, name, fun), do: :ok
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/hammox_lib.ex", `defmodule Hammox do
  def expect(mock, name, fun), do: :ok
end
`)

	// Two consumers: one with default Mox, one with mod: Hammox override
	defaultCallerSrc := `defmodule DefaultTest do
  use Remote.Mox

  def run do
    expect(MyMock, :foo, fn -> :ok end)
  end
end
`
	overrideCallerSrc := `defmodule OverrideTest do
  use Remote.Mox, mod: Hammox

  def run do
    expect(MyMock, :foo, fn -> :ok end)
  end
end
`
	overridePath := filepath.Join(server.projectRoot, "test", "override_test.ex")
	indexFile(t, server.store, server.projectRoot, "test/default_test.ex", defaultCallerSrc)
	indexFile(t, server.store, server.projectRoot, "test/override_test.ex", overrideCallerSrc)

	// Go-to-references on `expect` from the override file (mod: Hammox)
	overrideURI := "file://" + overridePath
	server.docs.Set(overrideURI, overrideCallerSrc)

	locs := referencesAt(t, server, overrideURI, 4, 4)
	if len(locs) == 0 {
		t.Fatal("expected references for expect with mod: Hammox override")
	}

	// Both files should be in the results (both use Remote.Mox to get expect)
	foundDefault, foundOverride := false, false
	for _, loc := range locs {
		locStr := string(loc.URI)
		if strings.Contains(locStr, "default_test.ex") {
			foundDefault = true
		}
		if strings.Contains(locStr, "override_test.ex") {
			foundOverride = true
		}
	}
	if !foundDefault {
		t.Error("expected reference in default_test.ex")
	}
	if !foundOverride {
		t.Error("expected reference in override_test.ex")
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

func TestReferences_UsingMacroShowsUseSites(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	macroSrc := `defmodule MyApp.Schema do
  defmacro __using__(_opts) do
    quote do
      import Ecto.Schema
    end
  end
end`
	callerASrc := `defmodule MyApp.User do
  use MyApp.Schema
end`
	callerBSrc := `defmodule MyApp.Post do
  use MyApp.Schema
end`

	indexFile(t, server.store, server.projectRoot, "lib/schema.ex", macroSrc)
	indexFile(t, server.store, server.projectRoot, "lib/user.ex", callerASrc)
	indexFile(t, server.store, server.projectRoot, "lib/post.ex", callerBSrc)

	schemaURI := "file://" + filepath.Join(server.projectRoot, "lib/schema.ex")
	server.docs.Set(schemaURI, macroSrc)

	// col=13 is on `__using__` in "  defmacro __using__(_opts) do"
	locs := referencesAt(t, server, schemaURI, 1, 13)
	if len(locs) != 2 {
		t.Fatalf("expected 2 use sites, got %d: %v", len(locs), locs)
	}

	paths := make(map[string]bool)
	for _, loc := range locs {
		paths[string(loc.URI)] = true
	}
	userURI := "file://" + filepath.Join(server.projectRoot, "lib/user.ex")
	postURI := "file://" + filepath.Join(server.projectRoot, "lib/post.ex")
	if !paths[userURI] {
		t.Errorf("expected user.ex in use sites, got: %v", locs)
	}
	if !paths[postURI] {
		t.Errorf("expected post.ex in use sites, got: %v", locs)
	}
}

func TestReferences_BareFunctionCalls(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	src := `defmodule MyApp.Service do
  def run(args) do
    result = do_work(args)
    result |> validate
  end

  defp do_work(args) do
    {:ok, args}
  end

  defp validate(result) do
    result
  end
end`

	indexFile(t, server.store, server.projectRoot, "lib/service.ex", src)
	serviceURI := "file://" + filepath.Join(server.projectRoot, "lib/service.ex")
	server.docs.Set(serviceURI, src)

	// References for do_work (private function) — cursor on the defp line
	locs := referencesAt(t, server, serviceURI, 6, 8)
	found := false
	for _, loc := range locs {
		if loc.Range.Start.Line == 2 { // call on line 3 (0-indexed = 2)
			found = true
		}
	}
	if !found {
		t.Errorf("expected bare call to do_work on line 3, got: %v", locs)
	}

	// References for validate (pipe call) — cursor on the defp line
	locs = referencesAt(t, server, serviceURI, 10, 8)
	found = false
	for _, loc := range locs {
		if loc.Range.Start.Line == 3 { // pipe call on line 4 (0-indexed = 3)
			found = true
		}
	}
	if !found {
		t.Errorf("expected pipe call to validate on line 4, got: %v", locs)
	}
}

func TestReferences_PublicBarePipeCall(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Mimics: def get_company_by_slug, then |> get_company_by_slug() in same module,
	// plus a qualified call from another file.
	defSrc := `defmodule MyApp.Companies.CRUD do
  def get_company_by_slug(slug), do: {:ok, slug}
  def get_company_by_slug!(slug), do: slug

  def fetch_company_by_slug(slug) do
    slug
    |> get_company_by_slug()
  end
end`

	callerSrc := `defmodule MyApp.Web do
  def show(slug) do
    MyApp.Companies.CRUD.get_company_by_slug(slug)
  end
end`

	indexFile(t, server.store, server.projectRoot, "lib/crud.ex", defSrc)
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerSrc)
	defURI := "file://" + filepath.Join(server.projectRoot, "lib/crud.ex")
	server.docs.Set(defURI, defSrc)

	// Trigger refs from the def line (line 1, col 6) — cursor on "get_company_by_slug"
	locs := referencesAt(t, server, defURI, 1, 6)

	// Should find:
	// - line 6: |> get_company_by_slug() (bare pipe call in same module)
	// - line 2 in web.ex: qualified call
	foundPipe := false
	foundQualified := false
	for _, loc := range locs {
		if loc.Range.Start.Line == 6 { // pipe call (0-indexed)
			foundPipe = true
		}
		if strings.Contains(string(loc.URI), "web.ex") {
			foundQualified = true
		}
	}
	if !foundPipe {
		lines := make([]string, len(locs))
		for i, loc := range locs {
			lines[i] = fmt.Sprintf("  %s:%d", loc.URI, loc.Range.Start.Line)
		}
		t.Errorf("expected bare pipe call on line 6, got:\n%s", strings.Join(lines, "\n"))
	}
	if !foundQualified {
		t.Error("expected qualified call from web.ex")
	}

	// Also trigger refs from the pipe call site (line 6, col 7) — cursor on "get_company_by_slug" in the pipe
	locs2 := referencesAt(t, server, defURI, 6, 7)
	foundDef := false
	foundQualified2 := false
	for _, loc := range locs2 {
		if loc.Range.Start.Line == 1 || loc.Range.Start.Line == 2 { // def lines
			foundDef = true
		}
		if strings.Contains(string(loc.URI), "web.ex") {
			foundQualified2 = true
		}
	}
	if !foundDef {
		lines := make([]string, len(locs2))
		for i, loc := range locs2 {
			lines[i] = fmt.Sprintf("  %s:%d", loc.URI, loc.Range.Start.Line)
		}
		t.Errorf("refs from pipe call site: expected def lines, got:\n%s", strings.Join(lines, "\n"))
	}
	if !foundQualified2 {
		lines := make([]string, len(locs2))
		for i, loc := range locs2 {
			lines[i] = fmt.Sprintf("  %s:%d", loc.URI, loc.Range.Start.Line)
		}
		t.Errorf("refs from pipe call site: expected qualified call from web.ex, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestReferences_FollowDelegateReverse(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// MyApp.Accounts.CRUD defines the real function
	crudSrc := `defmodule MyApp.Accounts.CRUD do
  def list_users, do: []

  def other do
    list_users()
  end
end`

	// MyApp.Accounts delegates to CRUD
	facadeSrc := `defmodule MyApp.Accounts do
  defdelegate list_users(), to: MyApp.Accounts.CRUD
end`

	// Callers use the facade module
	callerSrc := `defmodule MyApp.Web do
  alias MyApp.Accounts

  def index do
    Accounts.list_users()
  end
end`

	indexFile(t, server.store, server.projectRoot, "lib/crud.ex", crudSrc)
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", facadeSrc)
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerSrc)

	crudURI := "file://" + filepath.Join(server.projectRoot, "lib/crud.ex")
	server.docs.Set(crudURI, crudSrc)

	// Go-to-refs on the real definition in CRUD (line 1: "def list_users")
	locs := referencesAt(t, server, crudURI, 1, 6)

	// Should find the call through the delegate facade in web.ex
	foundDelegateRef := false
	for _, loc := range locs {
		if strings.Contains(string(loc.URI), "web.ex") {
			foundDelegateRef = true
		}
	}
	if !foundDelegateRef {
		lines := make([]string, len(locs))
		for i, loc := range locs {
			lines[i] = fmt.Sprintf("  %s:%d", loc.URI, loc.Range.Start.Line)
		}
		t.Errorf("expected ref from web.ex (via defdelegate), got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestReferences_NestedModuleDefinition(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Nested module: defmodule MoneyResponse inside Money creates
	// MyApp.Money.MoneyResponse, but the defmodule line says just "MoneyResponse"
	defSrc := `defmodule MyApp.Money do
  defmodule MoneyResponse do
    def schema, do: %{}
  end
end
`
	callerSrc := `defmodule MyApp.Cards do
  alias MyApp.Money.MoneyResponse

  def show do
    MoneyResponse.schema()
  end
end
`
	defPath := filepath.Join(server.projectRoot, "lib", "money.ex")
	indexFile(t, server.store, server.projectRoot, "lib/money.ex", defSrc)
	indexFile(t, server.store, server.projectRoot, "lib/cards.ex", callerSrc)

	defURI := "file://" + defPath
	server.docs.Set(defURI, defSrc)

	// Go-to-references on "MoneyResponse" in the defmodule line (line 1, col 13)
	locs := referencesAt(t, server, defURI, 1, 13)
	if len(locs) == 0 {
		t.Fatal("expected references for nested module MoneyResponse")
	}
	// Should find the alias and/or usage in cards.ex
	foundCallerRef := false
	for _, loc := range locs {
		if strings.Contains(string(loc.URI), "cards.ex") {
			foundCallerRef = true
			break
		}
	}
	if !foundCallerRef {
		t.Error("expected reference in cards.ex for nested module MoneyResponse")
	}
}

func TestReferences_NestedDefmoduleWithConflictingAlias(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// The nested defmodule TransactionRecord creates MyApp.Payments.TransactionRecord,
	// but inside it there's `alias MyApp.Billing.TransactionRecord` (a different module).
	// Go-to-references on the defmodule line should find references to the API module,
	// NOT the billing module.
	defSrc := `defmodule MyApp.Payments do
  defmodule TransactionRecord do
    alias MyApp.Billing.TransactionRecord

    def schema, do: %{}
  end
end
`
	callerSrc := `defmodule MyApp.Web do
  alias MyApp.Payments.TransactionRecord

  def show do
    TransactionRecord.schema()
  end
end
`
	// Also index the billing module so the alias has a target
	billingSrc := `defmodule MyApp.Billing.TransactionRecord do
  def get(id), do: id
end
`
	defPath := filepath.Join(server.projectRoot, "lib", "payments.ex")
	indexFile(t, server.store, server.projectRoot, "lib/payments.ex", defSrc)
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerSrc)
	indexFile(t, server.store, server.projectRoot, "lib/billing.ex", billingSrc)

	defURI := "file://" + defPath
	server.docs.Set(defURI, defSrc)

	// Go-to-references on "TransactionRecord" in the defmodule line (line 1, col 13)
	locs := referencesAt(t, server, defURI, 1, 13)

	// Should find references to MyApp.Payments.TransactionRecord (the API module),
	// NOT MyApp.Billing.TransactionRecord
	foundWebRef := false
	foundBillingRef := false
	for _, loc := range locs {
		locStr := string(loc.URI)
		if strings.Contains(locStr, "web.ex") {
			foundWebRef = true
		}
		if strings.Contains(locStr, "billing.ex") {
			foundBillingRef = true
		}
	}
	if !foundWebRef {
		t.Error("expected reference in web.ex for the API TransactionRecord module")
	}
	if foundBillingRef {
		t.Error("should NOT return references to MyApp.Billing.TransactionRecord")
	}
}

func TestDefinition_QualifiedCallOnNestedModule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Inner.helper() called from parent Outer — "Inner" is an implicit alias
	// for MyApp.Outer.Inner created by the nested defmodule.
	content := `defmodule MyApp.Outer do
  defmodule Inner do
    def helper, do: :ok
  end

  def run do
    Inner.helper()
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "outer.ex")
	indexFile(t, server.store, server.projectRoot, "lib/outer.ex", content)
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	// Go-to-definition on "helper" in Inner.helper() (line 6, col 10)
	locs := definitionAt(t, server, docURI, 6, 10)
	if len(locs) == 0 {
		t.Fatal("expected go-to-definition for Inner.helper() via implicit alias")
	}
	// Should jump to def helper on line 2
	if locs[0].Range.Start.Line != 2 {
		t.Errorf("expected jump to line 2 (def helper), got line %d", locs[0].Range.Start.Line)
	}
}

func TestDefinition_TripleNestedModule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.A do
  defmodule B do
    defmodule C do
      def deep, do: :ok
    end
  end

  def run do
    B.C.deep()
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "a.ex")
	indexFile(t, server.store, server.projectRoot, "lib/a.ex", content)
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	// Go-to-definition on "deep" in B.C.deep() (line 8, col 8)
	locs := definitionAt(t, server, docURI, 8, 8)
	if len(locs) == 0 {
		t.Fatal("expected go-to-definition for triple-nested B.C.deep()")
	}
	if locs[0].Range.Start.Line != 3 {
		t.Errorf("expected jump to line 3 (def deep), got line %d", locs[0].Range.Start.Line)
	}
}

func TestResolveBareFunctionModule_ParentScopeFunction(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Function defined in Outer, bare-called from inside Inner.
	// The enclosing module at the call site is Inner (which doesn't define it),
	// so the file-scan fallback should find it in Outer.
	content := `defmodule MyApp.Outer do
  def shared_helper, do: :ok

  defmodule Inner do
    def run do
      shared_helper()
    end
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "outer.ex")
	indexFile(t, server.store, server.projectRoot, "lib/outer.ex", content)
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	// Go-to-definition on shared_helper() inside Inner (line 5, col 6)
	locs := definitionAt(t, server, docURI, 5, 6)
	if len(locs) == 0 {
		t.Fatal("expected go-to-definition for parent-scope function called from nested module")
	}
	if locs[0].Range.Start.Line != 1 {
		t.Errorf("expected jump to line 1 (def shared_helper in Outer), got line %d", locs[0].Range.Start.Line)
	}
}

func TestReferences_ModuleOnlyExcludesFunctionCalls(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	guardsSrc := `defmodule MyApp.Guards do
  defguard is_positive(x) when x > 0
  defguard is_negative(x) when x < 0
end`

	consumerSrc := `defmodule MyApp.Consumer do
  import MyApp.Guards

  def check(x) when is_positive(x), do: :pos
  def check(x) when is_negative(x), do: :neg
end`

	indexFile(t, server.store, server.projectRoot, "lib/guards.ex", guardsSrc)
	indexFile(t, server.store, server.projectRoot, "lib/consumer.ex", consumerSrc)

	consumerURI := "file://" + filepath.Join(server.projectRoot, "lib/consumer.ex")
	server.docs.Set(consumerURI, consumerSrc)

	// References on the module name "MyApp.Guards" at the import line
	// should return only the import, not the bare guard function calls.
	locs := referencesAt(t, server, consumerURI, 1, 10)

	for _, loc := range locs {
		locURI := string(loc.URI)
		line := loc.Range.Start.Line
		// Lines 3 and 4 are bare guard calls — they should NOT appear.
		if strings.Contains(locURI, "consumer.ex") && (line == 3 || line == 4) {
			t.Errorf("module-only references should not include function call at line %d", line)
		}
	}
}

func TestReferences_Variable(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp do
  def process(data) do
    result = transform(data)
    log(result)
    result
  end
end`)

	// Cursor on "result" at line 2 (0-based), col 4
	locs := referencesAt(t, server, uri, 2, 4)
	if len(locs) != 3 {
		t.Fatalf("expected 3 occurrences of 'result', got %d", len(locs))
	}
	for _, loc := range locs {
		if string(loc.URI) != uri {
			t.Errorf("expected URI %q, got %q", uri, loc.URI)
		}
	}
	lines := []uint32{locs[0].Range.Start.Line, locs[1].Range.Start.Line, locs[2].Range.Start.Line}
	expected := []uint32{2, 3, 4}
	for i, got := range lines {
		if got != expected[i] {
			t.Errorf("occurrence %d: expected line %d, got %d", i, expected[i], got)
		}
	}
}

func TestReferences_VariableScopedToFunction(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp do
  def first(x) do
    x + 1
  end

  def second(x) do
    x * 2
  end
end`)

	// Cursor on "x" in first/1 at line 1, col 12
	locs := referencesAt(t, server, uri, 1, 12)
	if len(locs) != 2 {
		t.Fatalf("expected 2 occurrences of 'x' in first/1, got %d", len(locs))
	}
	for _, loc := range locs {
		line := loc.Range.Start.Line
		if line != 1 && line != 2 {
			t.Errorf("unexpected line %d — should only see lines 1 and 2 (first/1 scope)", line)
		}
	}
}

func TestReferences_BareFunctionCallInsideWithBlock(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	src := `defmodule MyApp.Service do
  def run(args) do
    with {:ok, result} <- do_work(args) do
      format(result)
    end
  end

  defp do_work(args) do
    {:ok, args}
  end

  defp format(result) do
    result
  end
end`

	indexFile(t, server.store, server.projectRoot, "lib/service.ex", src)
	serviceURI := "file://" + filepath.Join(server.projectRoot, "lib/service.ex")
	server.docs.Set(serviceURI, src)

	// References for "do_work" from the call site inside with (line 2, col 30)
	// "    with {:ok, result} <- do_work(args) do"
	// do_work starts at col 27
	locs := referencesAt(t, server, serviceURI, 2, 27)
	if len(locs) == 0 {
		t.Fatal("expected function references for 'do_work' inside with block, got none")
	}

	// References for "format" from inside with do block (line 3, col 6)
	locs = referencesAt(t, server, serviceURI, 3, 6)
	if len(locs) == 0 {
		t.Fatal("expected function references for 'format' inside with do block, got none")
	}
}

func TestReferences_BareFunctionCallInsideCaseBlock(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	src := `defmodule MyApp.Service do
  def run(args) do
    case do_work(args) do
      {:ok, result} ->
        format(result)
      _ ->
        :error
    end
  end

  defp do_work(args) do
    {:ok, args}
  end

  defp format(result) do
    result
  end
end`

	indexFile(t, server.store, server.projectRoot, "lib/service.ex", src)
	serviceURI := "file://" + filepath.Join(server.projectRoot, "lib/service.ex")
	server.docs.Set(serviceURI, src)

	// References for "do_work" from the call site inside case (line 2)
	locs := referencesAt(t, server, serviceURI, 2, 9)
	if len(locs) == 0 {
		t.Fatal("expected function references for 'do_work' inside case block, got none")
	}

	// References for "format" from inside case arm (line 4, col 8)
	locs = referencesAt(t, server, serviceURI, 4, 8)
	if len(locs) == 0 {
		t.Fatal("expected function references for 'format' inside case arm, got none")
	}
}

func TestServer_Formatting(t *testing.T) {
	_, err := exec.LookPath("mix")
	if err != nil {
		t.Skip("mix not available in PATH")
	}

	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Create mix.exs so findMixRoot succeeds
	if err := os.WriteFile(filepath.Join(server.projectRoot, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	unformatted := `defmodule   MyApp.Fmt   do
def   hello(   ), do:    :world
end
`
	filePath := filepath.Join(server.projectRoot, "lib", "fmt.ex")
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		t.Fatal(err)
	}
	docURI := string(uri.File(filePath))
	server.docs.Set(docURI, unformatted)

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits == nil {
		t.Fatal("expected formatting edits, got nil")
	}
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(edits))
	}
	// Minimal edit should NOT start at line 0 — the "end\n" suffix is shared
	if edits[0].Range.Start.Character != 0 {
		t.Error("expected edit to start at character 0")
	}
	if !strings.Contains(edits[0].NewText, "defmodule MyApp.Fmt do") {
		t.Errorf("expected formatted output, got: %s", edits[0].NewText)
	}
	// The edit range should be smaller than the whole document
	if edits[0].Range.End.Line > 3 {
		t.Errorf("expected minimal edit range, but end line is %d", edits[0].Range.End.Line)
	}
}

func TestServer_Formatting_OutsideProjectRoot(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///etc/shadow.ex"
	server.docs.Set(uri, "defmodule Evil do\nend\n")

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits != nil {
		t.Error("expected nil edits for file outside project root")
	}
}

func TestServer_Formatting_NonElixirFile(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.go"
	server.docs.Set(uri, "package main")

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits != nil {
		t.Error("expected nil edits for non-Elixir file")
	}
}

func TestServer_Formatting_AlreadyFormatted(t *testing.T) {
	_, err := exec.LookPath("mix")
	if err != nil {
		t.Skip("mix not available in PATH")
	}

	server, cleanup := setupTestServer(t)
	defer cleanup()

	if err := os.WriteFile(filepath.Join(server.projectRoot, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	formatted := "defmodule MyApp.Fmt do\n  def hello, do: :world\nend\n"
	filePath := filepath.Join(server.projectRoot, "lib", "fmt.ex")
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		t.Fatal(err)
	}
	docURI := string(uri.File(filePath))
	server.docs.Set(docURI, formatted)

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits != nil {
		t.Error("expected nil edits for already-formatted file")
	}
}

func TestDetectElixirStdlibRoot(t *testing.T) {
	root, ok := stdlib.DetectElixirLibRoot("")
	if !ok {
		t.Skip("elixir not available in PATH")
	}

	enumPath := filepath.Join(root, "elixir", "lib", "enum.ex")
	if _, err := os.Stat(enumPath); os.IsNotExist(err) {
		t.Errorf("expected stdlib enum.ex at %s", enumPath)
	}
}

func TestFindMixRoot(t *testing.T) {
	root := t.TempDir()

	// Create a monorepo structure (no root mix.exs):
	//   root/my_app/mix.exs
	//   root/my_app/lib/
	//   root/other_project/mix.exs
	//   root/other_project/lib/
	myApp := filepath.Join(root, "my_app")
	if err := os.MkdirAll(filepath.Join(myApp, "lib"), 0755); err != nil {
		t.Fatal(err)
	}
	myAppMix := filepath.Join(myApp, "mix.exs")
	if err := os.WriteFile(myAppMix, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	otherProject := filepath.Join(root, "other_project")
	if err := os.MkdirAll(filepath.Join(otherProject, "lib"), 0755); err != nil {
		t.Fatal(err)
	}
	otherMix := filepath.Join(otherProject, "mix.exs")
	if err := os.WriteFile(otherMix, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("finds nearest mix.exs from project lib dir", func(t *testing.T) {
		got := findMixRoot(filepath.Join(myApp, "lib"))
		if got != myApp {
			t.Errorf("expected %s, got %s", myApp, got)
		}
	})

	t.Run("finds mix.exs in same directory", func(t *testing.T) {
		got := findMixRoot(myApp)
		if got != myApp {
			t.Errorf("expected %s, got %s", myApp, got)
		}
	})

	t.Run("each project resolves to its own mix root", func(t *testing.T) {
		got := findMixRoot(filepath.Join(otherProject, "lib"))
		if got != otherProject {
			t.Errorf("expected %s, got %s", otherProject, got)
		}
	})

	t.Run("returns empty when no mix.exs exists", func(t *testing.T) {
		empty := t.TempDir()
		got := findMixRoot(empty)
		if got != "" {
			t.Errorf("expected empty string, got %s", got)
		}
	})
}

func TestFormatting_NoDocument(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: "file:///nonexistent.ex",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits != nil {
		t.Error("expected nil edits for unknown document")
	}
}

func TestFormatting_NoMixProject(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	docURI := "file:///tmp/nomixroot/lib/foo.ex"
	server.docs.Set(docURI, "defmodule Foo do\nend\n")

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{
			URI: protocol.DocumentURI(docURI),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits != nil {
		t.Error("expected nil edits when no mix.exs exists")
	}
}

func TestFormatter_PersistentProcessReuse(t *testing.T) {
	_, err := exec.LookPath("mix")
	if err != nil {
		t.Skip("mix not available in PATH")
	}

	server, cleanup := setupTestServer(t)
	defer cleanup()

	if err := os.WriteFile(filepath.Join(server.projectRoot, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(server.projectRoot, "lib", "test.ex")
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		t.Fatal(err)
	}
	docURI := string(uri.File(filePath))
	server.docs.Set(docURI, "defmodule   Test   do\nend\n")

	// First format — starts the persistent process
	edits1, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits1 == nil {
		t.Fatal("expected edits from first format")
	}

	// Second format — should reuse the same process (much faster)
	server.docs.Set(docURI, "defmodule   Test2   do\nend\n")
	start := time.Now()
	edits2, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits2 == nil {
		t.Fatal("expected edits from second format")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("second format took %s, expected reuse of persistent process", elapsed)
	}
}

func TestFormatter_RestartAfterCrash(t *testing.T) {
	_, err := exec.LookPath("mix")
	if err != nil {
		t.Skip("mix not available in PATH")
	}

	server, cleanup := setupTestServer(t)
	defer cleanup()

	if err := os.WriteFile(filepath.Join(server.projectRoot, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(server.projectRoot, "lib", "test.ex")
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		t.Fatal(err)
	}
	docURI := string(uri.File(filePath))
	server.docs.Set(docURI, "defmodule   Test   do\nend\n")

	// Start the persistent process
	_, err = server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Kill the persistent process
	mixRoot := findMixRoot(filepath.Dir(filePath))
	formatterExs := findFormatterConfig(filePath, mixRoot)
	server.formattersMu.Lock()
	if fp, ok := server.formatters[formatterExs]; ok {
		fp.Close()
	}
	server.formattersMu.Unlock()

	// Next format should recover (restart or fall back)
	server.docs.Set(docURI, "defmodule   Test2   do\nend\n")
	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits == nil {
		t.Fatal("expected formatting to recover after process crash")
	}
	if !strings.Contains(edits[0].NewText, "defmodule Test2 do") {
		t.Errorf("unexpected format result: %s", edits[0].NewText)
	}
}

func TestFormatter_WillSaveWaitUntil(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	if err := os.WriteFile(filepath.Join(server.projectRoot, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(server.projectRoot, "lib", "test.ex")
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		t.Fatal(err)
	}
	docURI := string(uri.File(filePath))
	server.docs.Set(docURI, "defmodule   Test   do\nend\n")

	edits, err := server.WillSaveWaitUntil(context.Background(), &protocol.WillSaveTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits != nil {
		t.Fatal("expected WillSaveWaitUntil to return no edits, formatting should only happen via textDocument/formatting")
	}
}

func TestFormatter_DidOpen_SkipsDepsFiles(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Create project root mix.exs so isDepsFile can detect deps/ relative to it
	if err := os.WriteFile(filepath.Join(server.projectRoot, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a deps structure: projectRoot/deps/some_dep/lib/foo.ex
	depsDir := filepath.Join(server.projectRoot, "deps", "some_dep", "lib")
	if err := os.MkdirAll(depsDir, 0755); err != nil {
		t.Fatal(err)
	}

	depFile := filepath.Join(depsDir, "foo.ex")
	depURI := string(uri.File(depFile))

	// Open a dep file — should NOT start a formatter process.
	// We verify by checking that no goroutine was launched: isDepsFile
	// returns true, so the eager-start path is skipped entirely.
	_ = server.DidOpen(context.Background(), &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:  protocol.DocumentURI(depURI),
			Text: "defmodule SomeDep.Foo do\nend\n",
		},
	})

	server.formattersMu.Lock()
	count := len(server.formatters)
	server.formattersMu.Unlock()

	if count != 0 {
		t.Errorf("expected no formatter processes for dep file, got %d", count)
	}
}

func TestMixCommand_SetsDir(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	dir := t.TempDir()
	cmd := server.mixCommand(context.Background(), dir, "format", "-")
	if cmd.Dir != dir {
		t.Errorf("expected Dir=%s, got %s", dir, cmd.Dir)
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
	if caps.DocumentHighlightProvider != true {
		t.Error("DocumentHighlightProvider should be true")
	}
	if caps.TypeDefinitionProvider != true {
		t.Error("TypeDefinitionProvider should be true")
	}
	if caps.SignatureHelpProvider == nil {
		t.Error("SignatureHelpProvider should not be nil")
	}
}

// === DocumentHighlight ===

func TestDocumentHighlight_Variable(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp do
  def process(data) do
    result = transform(data)
    result
  end
end`)

	result, err := server.DocumentHighlight(context.Background(), &protocol.DocumentHighlightParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: 2, Character: 4}, // cursor on "result"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 highlights for 'result', got %d", len(result))
	}
}

func TestDocumentHighlight_Function(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp do
  def process(data) do
    process(data)
    # process comment
    "process string"
  end
end`)

	result, err := server.DocumentHighlight(context.Background(), &protocol.DocumentHighlightParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: 2, Character: 4}, // cursor on process() call
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Should find def process and process() call, NOT comment or string
	if len(result) != 2 {
		t.Fatalf("expected 2 highlights for 'process', got %d", len(result))
	}
}

// === TypeDefinition ===

func TestTypeDefinition(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  @type status :: :active | :inactive
  @opaque token :: String.t()

  def create(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Accounts.status")

	result, err := server.TypeDefinition(context.Background(), &protocol.TypeDefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: 0, Character: 18}, // cursor on "status"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) == 0 {
		t.Fatal("expected type definition result for 'status'")
	}
}

func TestTypeDefinition_SkipsNonTypes(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Accounts.create")

	result, err := server.TypeDefinition(context.Background(), &protocol.TypeDefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: 0, Character: 18}, // cursor on "create"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Errorf("expected no type definition for regular function 'create', got %d", len(result))
	}
}

// === SignatureHelp ===

func TestSignatureHelp_QualifiedCall(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  @doc "Creates an account"
  @spec create(map(), keyword()) :: {:ok, term()}
  def create(attrs, opts) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.Test do
  alias MyApp.Accounts
  def run do
    Accounts.create(attrs, )
  end
end`)

	// cursor on the space after the comma in "Accounts.create(attrs, )"
	result, err := server.SignatureHelp(context.Background(), &protocol.SignatureHelpParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: 3, Character: 27},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected signature help result")
	}
	if len(result.Signatures) != 1 {
		t.Fatalf("expected 1 signature, got %d", len(result.Signatures))
	}
	sig := result.Signatures[0]
	if sig.Label != "create(attrs, opts)" {
		t.Errorf("unexpected label: %s", sig.Label)
	}
	if len(sig.Parameters) != 2 {
		t.Errorf("expected 2 parameters, got %d", len(sig.Parameters))
	}
	if result.ActiveParameter != 1 {
		t.Errorf("expected active parameter 1, got %d", result.ActiveParameter)
	}
}

func TestSignatureHelp_LocalFunction(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.Test do
  def process(data, opts) do
    :ok
  end

  def run do
    process(x, )
  end
end`)

	// cursor after comma in process(x, )
	result, err := server.SignatureHelp(context.Background(), &protocol.SignatureHelpParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: 6, Character: 15},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected signature help for local function")
	}
	if result.Signatures[0].Label != "process(data, opts)" {
		t.Errorf("unexpected label: %s", result.Signatures[0].Label)
	}
}

func TestSignatureHelp_Nested(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/string_helper.ex", `defmodule MyApp.StringHelper do
  def upcase(text) do
    String.upcase(text)
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.Test do
  alias MyApp.StringHelper
  def run do
    Enum.map(list, fn x -> StringHelper.upcase() end)
  end
end`)

	// cursor between the parens of StringHelper.upcase()
	// col 47 is the ) — cursor just before it, inside the empty args
	result, err := server.SignatureHelp(context.Background(), &protocol.SignatureHelpParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: 3, Character: 47},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected signature help for nested call")
	}
	if result.Signatures[0].Label != "upcase(text)" {
		t.Errorf("expected upcase(text), got: %s", result.Signatures[0].Label)
	}
	if result.ActiveParameter != 0 {
		t.Errorf("expected active parameter 0, got %d", result.ActiveParameter)
	}
}

// === OutgoingCalls ===

func TestOutgoingCalls(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create(attrs) do
    MyApp.Repo.insert(attrs)
    MyApp.Mailer.send(attrs)
  end

  def list do
    MyApp.Repo.all()
  end
end
`)

	indexFile(t, server.store, server.projectRoot, "lib/repo.ex", `defmodule MyApp.Repo do
  def insert(attrs) do
    :ok
  end
  def all do
    :ok
  end
end
`)

	indexFile(t, server.store, server.projectRoot, "lib/mailer.ex", `defmodule MyApp.Mailer do
  def send(attrs) do
    :ok
  end
end
`)

	// Open the accounts file in the doc store so PrepareCallHierarchy can read it
	accountsPath := filepath.Join(server.projectRoot, "lib/accounts.ex")
	accountsContent, _ := os.ReadFile(accountsPath)
	accountsURI := string(uri.File(accountsPath))
	server.docs.Set(accountsURI, string(accountsContent))

	// Prepare call hierarchy for create/1
	items, err := server.PrepareCallHierarchy(context.Background(), &protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{
				URI: protocol.DocumentURI(accountsURI),
			},
			Position: protocol.Position{Line: 1, Character: 6}, // on "create"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("expected PrepareCallHierarchy to return an item")
	}

	calls, err := server.OutgoingCalls(context.Background(), &protocol.CallHierarchyOutgoingCallsParams{
		Item: items[0],
	})
	if err != nil {
		t.Fatal(err)
	}

	// create() calls Repo.insert and Mailer.send
	if len(calls) < 2 {
		t.Fatalf("expected at least 2 outgoing calls from create, got %d", len(calls))
	}

	names := make(map[string]bool)
	for _, c := range calls {
		names[c.To.Name] = true
	}
	if !names["MyApp.Repo.insert"] {
		t.Error("expected outgoing call to MyApp.Repo.insert")
	}
	if !names["MyApp.Mailer.send"] {
		t.Error("expected outgoing call to MyApp.Mailer.send")
	}
}

// === FoldingRanges ===

func TestFoldingRanges(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.Accounts do
  def create(attrs) do
    :ok
  end

  def list do
    :ok
  end
end`)

	result, err := server.FoldingRanges(context.Background(), &protocol.FoldingRangeParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should have 3 fold ranges: defmodule (0-8), create (1-3), list (5-7)
	if len(result) != 3 {
		t.Fatalf("expected 3 folding ranges, got %d: %+v", len(result), result)
	}

	// Verify the module-level fold
	foundModule := false
	for _, r := range result {
		if r.StartLine == 0 && r.EndLine == 8 {
			foundModule = true
		}
	}
	if !foundModule {
		t.Error("expected folding range for defmodule (lines 0-8)")
	}
}

// === CodeAction ===

func TestCodeAction_AddAlias(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.Test do
  def run do
    Accounts.create(attrs)
  end
end`)

	actions, err := server.CodeAction(context.Background(), &protocol.CodeActionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
		Range: protocol.Range{
			Start: protocol.Position{Line: 2, Character: 4}, // on "Accounts"
			End:   protocol.Position{Line: 2, Character: 12},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(actions) == 0 {
		t.Fatal("expected at least one code action for unaliased Accounts")
	}

	found := false
	for _, a := range actions {
		if a.Title == "Add alias MyApp.Accounts" {
			found = true
			break
		}
	}
	if !found {
		var titles []string
		for _, a := range actions {
			titles = append(titles, a.Title)
		}
		t.Errorf("expected 'Add alias MyApp.Accounts' action, got: %v", titles)
	}
}

func TestCodeAction_NoActionWhenAliased(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.Test do
  alias MyApp.Accounts
  def run do
    Accounts.create(attrs)
  end
end`)

	actions, err := server.CodeAction(context.Background(), &protocol.CodeActionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
		Range: protocol.Range{
			Start: protocol.Position{Line: 3, Character: 4},
			End:   protocol.Position{Line: 3, Character: 12},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(actions) != 0 {
		t.Errorf("expected no code actions when module is already aliased, got %d", len(actions))
	}
}

func TestCodeAction_DottedModuleRef(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/random_api/client.ex", `defmodule MyApp.RandomAPI.Client do
  def request(url) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.Test do
  def run do
    RandomAPI.Client.request("/api")
  end
end`)

	actions, err := server.CodeAction(context.Background(), &protocol.CodeActionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
		Range: protocol.Range{
			Start: protocol.Position{Line: 2, Character: 4}, // on "RandomAPI"
			End:   protocol.Position{Line: 2, Character: 12},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(actions) == 0 {
		t.Fatal("expected code action for unaliased RandomAPI.Client")
	}

	found := false
	for _, a := range actions {
		if a.Title == "Add alias MyApp.RandomAPI" {
			found = true
			break
		}
	}
	if !found {
		var titles []string
		for _, a := range actions {
			titles = append(titles, a.Title)
		}
		t.Errorf("expected 'Add alias MyApp.RandomAPI' action, got: %v", titles)
	}
}

func TestDeclaration_ImplTrueFallback(t *testing.T) {
	// When the behaviour chain can't be statically resolved (e.g. dynamic
	// `use unquote(mod)`), Declaration should fall back to a global @callback
	// search when @impl true is present above the function definition.
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/my_behaviour.ex", `defmodule MyApp.MyBehaviour do
  @callback process(job :: term()) :: :ok | {:error, term()}
end
`)

	uri := "file:///test_worker.ex"
	server.docs.Set(uri, `defmodule MyApp.Worker do
  use MyApp.SomeWrapper

  @impl true
  def process(job) do
    :ok
  end
end`)

	// Cursor on "process" (line 4, 0-based — the def process line)
	actions, err := server.Declaration(context.Background(), &protocol.DeclarationParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: 4, Character: 6},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) == 0 {
		t.Fatal("expected at least one declaration location via @impl true fallback, got none")
	}
	found := false
	for _, loc := range actions {
		if strings.Contains(string(loc.URI), "my_behaviour.ex") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a location in my_behaviour.ex, got: %v", actions)
	}
}

func TestDeclaration_ImplModule(t *testing.T) {
	// When @impl SomeModule is explicit, Declaration should use it directly
	// without needing to resolve the behaviour chain.
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/my_behaviour.ex", `defmodule MyApp.MyBehaviour do
  @callback process(job :: term()) :: :ok | {:error, term()}
end
`)

	uri := "file:///test_worker2.ex"
	server.docs.Set(uri, `defmodule MyApp.Worker2 do
  @impl MyApp.MyBehaviour
  def process(job) do
    :ok
  end
end`)

	actions, err := server.Declaration(context.Background(), &protocol.DeclarationParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
			Position:     protocol.Position{Line: 2, Character: 6},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) == 0 {
		t.Fatal("expected a declaration location via @impl MyApp.MyBehaviour, got none")
	}
	if !strings.Contains(string(actions[0].URI), "my_behaviour.ex") {
		t.Errorf("expected location in my_behaviour.ex, got: %s", actions[0].URI)
	}
}

func TestCodeAction_FullyQualifiedModule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/random_api/client.ex", `defmodule MyApp.RandomAPI.Client do
  def new(opts) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.Test do
  def run do
    MyApp.RandomAPI.Client.new(opts)
  end
end`)

	// Cursor on "Client" within the fully qualified module name
	actions, err := server.CodeAction(context.Background(), &protocol.CodeActionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri)},
		Range: protocol.Range{
			Start: protocol.Position{Line: 2, Character: 19}, // on "Client"
			End:   protocol.Position{Line: 2, Character: 25},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(actions) == 0 {
		t.Fatal("expected code action for fully qualified MyApp.RandomAPI.Client")
	}

	// Check the action title
	if actions[0].Title != "Add alias MyApp.RandomAPI.Client" {
		t.Errorf("unexpected title: %s", actions[0].Title)
	}

	// Check that the edit includes both the alias insertion and the module replacement
	edits := actions[0].Edit.Changes[protocol.DocumentURI(uri)]
	if len(edits) != 2 {
		t.Fatalf("expected 2 edits (alias + replacement), got %d", len(edits))
	}

	// One edit should insert the alias, the other should replace the module ref
	hasAlias := false
	hasReplacement := false
	for _, e := range edits {
		if strings.Contains(e.NewText, "alias MyApp.RandomAPI.Client") {
			hasAlias = true
		}
		if e.NewText == "Client" {
			hasReplacement = true
		}
	}
	if !hasAlias {
		t.Error("expected an edit inserting 'alias MyApp.RandomAPI.Client'")
	}
	if !hasReplacement {
		t.Error("expected an edit replacing the module ref with 'Client'")
	}
}
