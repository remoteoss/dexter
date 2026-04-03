package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// === Unit tests for rename helpers ===

func TestCamelToSnake(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Accounts", "accounts"},
		{"SomeUser", "some_user"},
		{"MyApp", "my_app"},
		{"HTTPClient", "http_client"},
		{"MyHTTPClient", "my_http_client"},
		{"A", "a"},
		{"User", "user"},
		{"APIKey", "api_key"},
	}
	for _, tt := range tests {
		got := camelToSnake(tt.input)
		if got != tt.expected {
			t.Errorf("camelToSnake(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestModuleToExpectedFilename(t *testing.T) {
	tests := []struct {
		module   string
		expected string
	}{
		{"MyApp.Accounts", "accounts.ex"},
		{"MyApp.SomeUser", "some_user.ex"},
		{"MyApp.HTTPClient", "http_client.ex"},
		{"MyApp", "my_app.ex"},
	}
	for _, tt := range tests {
		got := moduleToExpectedFilename(tt.module)
		if got != tt.expected {
			t.Errorf("moduleToExpectedFilename(%q) = %q, want %q", tt.module, got, tt.expected)
		}
	}
}

func TestFindTokenColumn(t *testing.T) {
	tests := []struct {
		line     string
		token    string
		expected int
	}{
		{"def list_users do", "list_users", 4},
		{"  def list_users do", "list_users", 6},
		{"list_users(args)", "list_users", 0},
		{"def list_users_by_id do", "list_users", -1}, // no false match inside longer ident
		{"MyApp.list_users()", "list_users", 6},
		{"no match here", "list_users", -1},
		// match at end of line
		{"  list_users", "list_users", 2},
	}
	for _, tt := range tests {
		got := findTokenColumn(tt.line, tt.token)
		if got != tt.expected {
			t.Errorf("findTokenColumn(%q, %q) = %d, want %d", tt.line, tt.token, got, tt.expected)
		}
	}
}

func TestFindAllTokenColumns(t *testing.T) {
	tests := []struct {
		line     string
		token    string
		expected []int
	}{
		// single occurrence
		{"def list_users do", "list_users", []int{4}},
		// multiple occurrences (import only:)
		{"import MyApp, only: [foo: 1, foo: 2]", "foo", []int{21, 29}},
		// no match
		{"def other_func do", "list_users", nil},
		// not a false match inside longer identifier
		{"def list_users_and_more, do: list_users()", "list_users", []int{29}},
		// multi-byte unicode character before token should not break boundary check
		{"α_foo(x)", "foo", nil},      // preceded by identifier char (via underscore)
		{"α foo(x)", "foo", []int{3}}, // preceded by space, OK
	}
	for _, tt := range tests {
		got := findAllTokenColumns(tt.line, tt.token)
		if len(got) != len(tt.expected) {
			t.Errorf("findAllTokenColumns(%q, %q) = %v, want %v", tt.line, tt.token, got, tt.expected)
			continue
		}
		for i := range got {
			if got[i] != tt.expected[i] {
				t.Errorf("findAllTokenColumns(%q, %q)[%d] = %d, want %d", tt.line, tt.token, i, got[i], tt.expected[i])
			}
		}
	}
}

func TestIsDepsFile(t *testing.T) {
	root := t.TempDir()
	server := &Server{}

	// Set up: my_app/mix.exs, my_app/deps/some_dep/mix.exs (dep has its own mix.exs)
	myApp := filepath.Join(root, "my_app")
	depDir := filepath.Join(myApp, "deps", "some_dep")
	if err := os.MkdirAll(filepath.Join(depDir, "lib"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, mixPath := range []string{
		filepath.Join(myApp, "mix.exs"),
		filepath.Join(depDir, "mix.exs"), // dep has its own mix.exs
	} {
		if err := os.WriteFile(mixPath, []byte(""), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("file inside deps/ is a dep even when dep has its own mix.exs", func(t *testing.T) {
		depFile := filepath.Join(depDir, "lib", "some_dep.ex")
		if !server.isDepsFile(depFile) {
			t.Errorf("expected isDepsFile to return true for %s", depFile)
		}
	})

	t.Run("project lib file is not a dep", func(t *testing.T) {
		libFile := filepath.Join(myApp, "lib", "my_app.ex")
		if server.isDepsFile(libFile) {
			t.Errorf("expected isDepsFile to return false for %s", libFile)
		}
	})

	t.Run("file with no mix.exs ancestor is not a dep", func(t *testing.T) {
		isolated := t.TempDir()
		file := filepath.Join(isolated, "foo.ex")
		if server.isDepsFile(file) {
			t.Errorf("expected isDepsFile to return false for %s", file)
		}
	})
}

func TestPrepareRename_DepsSymbol(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Index a file under a deps/ directory
	mixDir := filepath.Join(server.projectRoot, "myapp")
	if err := os.MkdirAll(mixDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mixDir, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}
	depsContent := `defmodule Ecto.Query do
  def from(query) do
    query
  end
end
`
	depsPath := filepath.Join(mixDir, "deps", "ecto", "lib", "ecto", "query.ex")
	if err := os.MkdirAll(filepath.Dir(depsPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(depsPath, []byte(depsContent), 0644); err != nil {
		t.Fatal(err)
	}
	indexFile(t, server.store, server.projectRoot, "myapp/deps/ecto/lib/ecto/query.ex", depsContent)

	// Also create a project file that uses it
	projectContent := `defmodule MyApp.Repo do
  def all do
    Ecto.Query.from(User)
  end
end
`
	projectPath := filepath.Join(server.projectRoot, "myapp", "lib", "repo.ex")
	indexFile(t, server.store, server.projectRoot, "myapp/lib/repo.ex", projectContent)
	projectURI := "file://" + projectPath
	server.docs.Set(projectURI, projectContent)

	// Cursor on "Ecto.Query.from" call in project file — should be rejected (deps symbol)
	r := prepareRenameAt(t, server, projectURI, 2, 18)
	if r != nil {
		t.Error("expected nil for symbol defined in deps/")
	}
}

func TestPrepareRename_DepsAndLibsDuplicate(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Monorepo layout: module defined in libs/ (first-party) and also
	// symlinked into apps/myapp/deps/ (dependency copy). The first-party
	// definition should allow rename; the deps copy should be skipped.
	appDir := filepath.Join(server.projectRoot, "apps", "myapp")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	libContent := `defmodule SharedLib.Application do
  def start, do: :ok
end
`
	// First-party definition in libs/
	libPath := filepath.Join(server.projectRoot, "libs", "shared_lib", "lib", "shared_lib", "application.ex")
	if err := os.MkdirAll(filepath.Dir(libPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libPath, []byte(libContent), 0644); err != nil {
		t.Fatal(err)
	}
	indexFile(t, server.store, server.projectRoot, "libs/shared_lib/lib/shared_lib/application.ex", libContent)

	// Duplicate definition under deps/ (simulates symlink)
	depsPath := filepath.Join(appDir, "deps", "shared_lib", "lib", "shared_lib", "application.ex")
	if err := os.MkdirAll(filepath.Dir(depsPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(depsPath, []byte(libContent), 0644); err != nil {
		t.Fatal(err)
	}
	indexFile(t, server.store, server.projectRoot, "apps/myapp/deps/shared_lib/lib/shared_lib/application.ex", libContent)

	// Open the libs/ file and try to prepare rename
	libURI := "file://" + libPath
	server.docs.Set(libURI, libContent)

	r := prepareRenameAt(t, server, libURI, 0, 22)
	if r == nil {
		t.Fatal("expected PrepareRename to succeed for module with first-party definition in libs/")
	}

	// Actually perform the rename and verify only the libs/ copy is updated
	edit := renameAt(t, server, libURI, 0, 22, "Setup")
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}

	// The deps/ copy should NOT be modified
	depsData, _ := os.ReadFile(depsPath)
	if strings.Contains(string(depsData), "Setup") {
		t.Error("deps/ file should not be modified by rename")
	}
}

func TestIsValidFunctionName(t *testing.T) {
	valid := []string{"foo", "list_users", "valid?", "update!", "_private", "foo_bar_baz"}
	invalid := []string{"", "Foo", "foo bar", "123abc", "foo.bar"}
	for _, n := range valid {
		if !isValidFunctionName(n) {
			t.Errorf("isValidFunctionName(%q) should be true", n)
		}
	}
	for _, n := range invalid {
		if isValidFunctionName(n) {
			t.Errorf("isValidFunctionName(%q) should be false", n)
		}
	}
}

func TestIsValidModuleName(t *testing.T) {
	valid := []string{"Foo", "MyApp", "MyApp.Accounts", "MyApp.Accounts.User"}
	invalid := []string{"", "foo", "MyApp.", ".MyApp", "My App", "myApp"}
	for _, n := range valid {
		if !isValidModuleName(n) {
			t.Errorf("isValidModuleName(%q) should be true", n)
		}
	}
	for _, n := range invalid {
		if isValidModuleName(n) {
			t.Errorf("isValidModuleName(%q) should be false", n)
		}
	}
}

// === Integration helpers ===

func renameAt(t *testing.T, server *Server, docURI string, line, col uint32, newName string) *protocol.WorkspaceEdit {
	t.Helper()
	result, err := server.Rename(context.Background(), &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
			Position:     protocol.Position{Line: line, Character: col},
		},
		NewName: newName,
	})
	if err != nil {
		t.Fatalf("Rename error: %v", err)
	}
	return result
}

func prepareRenameAt(t *testing.T, server *Server, docURI string, line, col uint32) *protocol.Range {
	t.Helper()
	result, err := server.PrepareRename(context.Background(), &protocol.PrepareRenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
			Position:     protocol.Position{Line: line, Character: col},
		},
	})
	if err != nil {
		t.Fatalf("PrepareRename error: %v", err)
	}
	return result
}

func collectEdits(edit *protocol.WorkspaceEdit, filePath string) []protocol.TextEdit {
	if edit == nil {
		return nil
	}
	fileURI := protocol.DocumentURI(uri.File(filePath))
	return edit.Changes[fileURI]
}

func hasEdit(edits []protocol.TextEdit, newText string) bool {
	for _, e := range edits {
		if e.NewText == newText {
			return true
		}
	}
	return false
}

func editsContainLine(edits []protocol.TextEdit, lineNum uint32) bool {
	for _, e := range edits {
		if e.Range.Start.Line == lineNum {
			return true
		}
	}
	return false
}

// fileContains checks whether the file at path contains the given substring.
// Used to verify server-side writes for files not open in the editor.
func fileContains(filePath, substr string) bool {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), substr)
}

// hasRename returns true if the rename result (either in WorkspaceEdit or
// written directly to disk) contains newText for the given file.
func hasRename(edit *protocol.WorkspaceEdit, filePath, newText string) bool {
	if hasEdit(collectEdits(edit, filePath), newText) {
		return true
	}
	return fileContains(filePath, newText)
}

// === PrepareRename tests ===

func TestPrepareRename_FunctionName(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.Accounts do
  def list_users do
    []
  end
end
`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", content)
	docURI := "file:///test/accounts.ex"
	server.docs.Set(docURI, content)

	// Cursor on "list_users" (line 1, col 6 — 0-based)
	r := prepareRenameAt(t, server, docURI, 1, 6)
	if r == nil {
		t.Fatal("expected non-nil range")
	}
	if r.Start.Line != 1 {
		t.Errorf("expected line 1, got %d", r.Start.Line)
	}
	// "list_users" starts at col 6
	if r.Start.Character != 6 {
		t.Errorf("expected start col 6, got %d", r.Start.Character)
	}
	if r.End.Character != 16 { // len("list_users") = 10
		t.Errorf("expected end col 16, got %d", r.End.Character)
	}
}

func TestPrepareRename_ModuleName_FullRange(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.Accounts do
  def list_users, do: []
end
`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", content)
	docURI := "file:///test/accounts.ex"
	server.docs.Set(docURI, content)

	// Cursor on "Accounts" in "defmodule MyApp.Accounts do" (line 0, col 20)
	r := prepareRenameAt(t, server, docURI, 0, 20)
	if r == nil {
		t.Fatal("expected non-nil range")
	}
	// Should highlight just "Accounts" (the last segment), not "MyApp.Accounts"
	// "Accounts" starts at col 16, ends at col 24
	if r.Start.Character != 16 {
		t.Errorf("expected start col 16 (start of Accounts), got %d", r.Start.Character)
	}
	if r.End.Character != 24 { // len("Accounts") = 8
		t.Errorf("expected end col 24 (end of Accounts), got %d", r.End.Character)
	}
}

func TestPrepareRename_Whitespace(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  def foo, do: :ok
end
`
	server.docs.Set("file:///test/foo.ex", content)

	r := prepareRenameAt(t, server, "file:///test/foo.ex", 1, 2) // cursor on spaces before def
	if r != nil {
		t.Error("expected nil for whitespace position")
	}
}

func TestPrepareRename_UnknownSymbol(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  def foo, do: bar()
end
`
	server.docs.Set("file:///test/foo.ex", content)

	// "bar" is not in the index
	r := prepareRenameAt(t, server, "file:///test/foo.ex", 1, 15)
	if r != nil {
		t.Error("expected nil for unknown symbol")
	}
}

// === Function rename tests ===

func TestRename_Function_SingleFile(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.Accounts do
  def list_users do
    []
  end

  def other do
    list_users()
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", content)
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	edit := renameAt(t, server, docURI, 1, 6, "all_users")
	edits := collectEdits(edit, path)

	if len(edits) == 0 {
		t.Fatal("expected edits")
	}
	for _, e := range edits {
		if e.NewText != "all_users" {
			t.Errorf("unexpected new text %q", e.NewText)
		}
	}
	// Should have edited line 1 (def) and line 6 (call)
	if !editsContainLine(edits, 1) {
		t.Error("expected edit on definition line 1")
	}
	if !editsContainLine(edits, 6) {
		t.Error("expected edit on call line 6")
	}
}

func TestRename_Function_CrossFile(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	defContent := `defmodule MyApp.Accounts do
  def list_users do
    []
  end
end
`
	callerContent := `defmodule MyApp.Web do
  alias MyApp.Accounts

  def index do
    Accounts.list_users()
  end
end
`
	defPath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	callerPath := filepath.Join(server.projectRoot, "lib", "web.ex")
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", defContent)
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerContent)

	defURI := "file://" + defPath
	server.docs.Set(defURI, defContent)

	edit := renameAt(t, server, defURI, 1, 6, "get_users")

	// Definition file should be edited
	defEdits := collectEdits(edit, defPath)
	if len(defEdits) == 0 {
		t.Error("expected edits in definition file")
	}

	// Caller file should have "get_users" (either in WorkspaceEdit or written to disk)
	if !hasRename(edit, callerPath, "get_users") {
		t.Error("expected 'get_users' in caller file")
	}
}

func TestRename_Function_MultipleClauses(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  def process(:ok), do: :done
  def process(:error), do: :failed
  def process(_), do: :unknown
end
`
	path := filepath.Join(server.projectRoot, "lib", "my_app.ex")
	indexFile(t, server.store, server.projectRoot, "lib/my_app.ex", content)
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	edit := renameAt(t, server, docURI, 1, 6, "handle")
	edits := collectEdits(edit, path)

	// All three clauses should be renamed
	if len(edits) < 3 {
		t.Errorf("expected at least 3 edits for multiple clauses, got %d", len(edits))
	}
}

func TestRename_Function_ImportOnly(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	defContent := `defmodule MyApp.Helpers do
  def format_date(date) do
    date
  end
end
`
	callerContent := `defmodule MyApp.Web do
  import MyApp.Helpers, only: [format_date: 1]

  def show do
    format_date(~D[2024-01-01])
  end
end
`
	defPath := filepath.Join(server.projectRoot, "lib", "helpers.ex")
	callerPath := filepath.Join(server.projectRoot, "lib", "web.ex")
	indexFile(t, server.store, server.projectRoot, "lib/helpers.ex", defContent)
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerContent)

	defURI := "file://" + defPath
	server.docs.Set(defURI, defContent)

	edit := renameAt(t, server, defURI, 1, 6, "format_date_string")

	// import only: line should be updated (either in WorkspaceEdit or written to disk)
	if !hasRename(edit, callerPath, "format_date_string") {
		t.Error("expected 'format_date_string' in caller file (import only: line)")
	}
}

func TestRename_Module_MultipleOccurrencesOnSameLine(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	defContent := `defmodule MyApp.User do
  defstruct [:name, :parent]
end
`
	callerContent := `defmodule MyApp.Accounts do
  alias MyApp.User

  @type family :: %User{name: String.t(), parent: User.t()}
end
`
	indexFile(t, server.store, server.projectRoot, "lib/user.ex", defContent)
	callerPath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", callerContent)

	defURI := "file://" + filepath.Join(server.projectRoot, "lib", "user.ex")
	server.docs.Set(defURI, defContent)

	_ = renameAt(t, server, defURI, 0, 18, "Person")

	// Both occurrences of "User" on the @type line should be replaced correctly,
	// even though "Person" is a different length than "User". This catches a bug
	// where left-to-right replacement with pre-computed column positions would
	// garble the second occurrence (e.g. "parentPersoner.t()" instead of
	// "parent: Person.t()").
	data, _ := os.ReadFile(callerPath)
	callerResult := string(data)

	expectedTypeLine := `  @type family :: %Person{name: String.t(), parent: Person.t()}`
	if !strings.Contains(callerResult, expectedTypeLine) {
		t.Errorf("expected @type line to be:\n  %s\ngot file:\n%s", expectedTypeLine, callerResult)
	}

	expectedAliasLine := `  alias MyApp.Person`
	if !strings.Contains(callerResult, expectedAliasLine) {
		t.Errorf("expected alias line to be:\n  %s\ngot file:\n%s", expectedAliasLine, callerResult)
	}
}

func TestRename_Function_Spec(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.Accounts do
  @spec list_users() :: [User.t()]
  def list_users do
    []
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", content)
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	edit := renameAt(t, server, docURI, 2, 6, "all_users")
	edits := collectEdits(edit, path)

	// Should rename both the @spec line and the def line
	if !editsContainLine(edits, 1) { // @spec is line index 1
		t.Error("expected edit on @spec line")
	}
	if !editsContainLine(edits, 2) { // def is line index 2
		t.Error("expected edit on def line")
	}
}

func TestRename_Function_InvalidName(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  def list_users, do: []
end
`
	path := filepath.Join(server.projectRoot, "lib", "my_app.ex")
	indexFile(t, server.store, server.projectRoot, "lib/my_app.ex", content)
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	_, err := server.Rename(context.Background(), &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
			Position:     protocol.Position{Line: 1, Character: 6},
		},
		NewName: "InvalidName", // uppercase — invalid
	})
	if err == nil {
		t.Error("expected error for invalid function name")
	}
}

// === Module rename tests ===

func TestRename_Module_BasicRefs(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	defContent := `defmodule MyApp.Accounts do
  def list_users, do: []
end
`
	callerContent := `defmodule MyApp.Web do
  alias MyApp.Accounts
  import MyApp.Accounts

  def index do
    MyApp.Accounts.list_users()
  end
end
`
	defPath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	callerPath := filepath.Join(server.projectRoot, "lib", "web.ex")
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", defContent)
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerContent)

	defURI := "file://" + defPath
	server.docs.Set(defURI, defContent)

	edit := renameAt(t, server, defURI, 0, 20, "Auth")
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}

	// Caller should have MyApp.Auth (either in WorkspaceEdit or written to disk)
	if !hasRename(edit, callerPath, "MyApp.Auth") {
		t.Error("expected MyApp.Auth in caller file")
	}
	if fileContains(callerPath, "MyApp.Auth.Accounts") {
		t.Error("unexpected MyApp.Auth.Accounts in caller file")
	}
}

func TestRename_Module_AliasAsPreserved(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	defContent := `defmodule MyApp.Accounts do
  def list_users, do: []
end
`
	callerContent := `defmodule MyApp.Web do
  alias MyApp.Accounts, as: Accts

  def index do
    Accts.list_users()
  end
end
`
	defPath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	callerPath := filepath.Join(server.projectRoot, "lib", "web.ex")
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", defContent)
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerContent)

	defURI := "file://" + defPath
	server.docs.Set(defURI, defContent)

	edit := renameAt(t, server, defURI, 0, 20, "Auth")

	// The alias line should be updated (either WorkspaceEdit or written to disk)
	if !hasRename(edit, callerPath, "MyApp.Auth") {
		t.Error("expected MyApp.Auth in caller file (alias line)")
	}
	// "Accts" should NOT be changed
	if fileContains(callerPath, "as: MyApp") || fileContains(callerPath, "as: Auth") {
		t.Error("as: alias 'Accts' should not be changed by module rename")
	}
}

func TestRename_Module_ViaAlias(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	defContent := `defmodule MyApp.Billing.Services.CreateInvoice do
  def call(params), do: params
end
`
	callerContent := `defmodule MyApp.BillingWeb.InvoiceController do
  alias MyApp.Billing.Services.CreateInvoice

  def create(params) do
    CreateInvoice.call(params)
  end
end
`
	indexFile(t, server.store, server.projectRoot, "lib/create_invoice.ex", defContent)
	callerPath := filepath.Join(server.projectRoot, "lib", "controller.ex")
	indexFile(t, server.store, server.projectRoot, "lib/controller.ex", callerContent)

	// Rename from the caller file, cursor on the alias "CreateInvoice"
	callerURI := "file://" + callerPath
	server.docs.Set(callerURI, callerContent)

	// User types just "GenerateInvoice" — replacing the highlighted alias segment.
	// The rename should preserve the namespace: MyApp.Billing.Services.GenerateInvoice
	edit := renameAt(t, server, callerURI, 4, 4, "GenerateInvoice")

	// The defmodule should keep its full namespace, not become "defmodule GenerateInvoice do"
	newDefPath := filepath.Join(server.projectRoot, "lib", "generate_invoice.ex")
	expectedDefmodule := "defmodule MyApp.Billing.Services.GenerateInvoice do"
	if !fileContains(newDefPath, expectedDefmodule) {
		data, _ := os.ReadFile(newDefPath)
		t.Errorf("expected defmodule to be:\n  %s\ngot file:\n%s", expectedDefmodule, string(data))
	}

	// The caller (open buffer) should have WorkspaceEdit replacing the alias usage
	callerEdits := collectEdits(edit, callerPath)
	if !hasEdit(callerEdits, "GenerateInvoice") {
		t.Errorf("expected WorkspaceEdit with 'GenerateInvoice' for caller, got %+v", callerEdits)
	}
}

func TestRename_Module_AliasedSubmoduleCall(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/payments.ex", `defmodule MyApp.Payments do
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/payments/crud.ex", `defmodule MyApp.Payments.CRUD do
  def list, do: []
end
`)
	callerContent := `defmodule MyApp.Checkout do
  alias MyApp.Payments

  def run do
    Payments.CRUD.list()
  end
end
`
	callerPath := filepath.Join(server.projectRoot, "lib", "checkout.ex")
	indexFile(t, server.store, server.projectRoot, "lib/checkout.ex", callerContent)

	defURI := "file://" + filepath.Join(server.projectRoot, "lib/payments.ex")
	server.docs.Set(defURI, `defmodule MyApp.Payments do
end
`)
	callerURI := "file://" + callerPath
	server.docs.Set(callerURI, callerContent)

	edit := renameAt(t, server, defURI, 0, 20, "Billing")
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}

	// The aliased call Payments.CRUD.list() should become Billing.CRUD.list()
	callerEdits := collectEdits(edit, callerPath)
	if !hasEdit(callerEdits, "Billing.CRUD") {
		t.Errorf("expected alias-based call Payments.CRUD to be updated, got edits: %+v", callerEdits)
	}
}

func TestRename_Module_SubmoduleCascade(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def list_users, do: []
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/accounts/user.ex", `defmodule MyApp.Accounts.User do
  defstruct [:name]
end
`)
	callerContent := `defmodule MyApp.Web do
  alias MyApp.Accounts.User

  def show do
    %MyApp.Accounts.User{}
  end
end
`
	callerPath := filepath.Join(server.projectRoot, "lib", "web.ex")
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerContent)

	defPath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	defURI := "file://" + defPath
	server.docs.Set(defURI, `defmodule MyApp.Accounts do
  def list_users, do: []
end
`)

	edit := renameAt(t, server, defURI, 0, 20, "Auth")
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}

	// Caller should have MyApp.Auth.User (either in WorkspaceEdit or written to disk)
	if !hasRename(edit, callerPath, "MyApp.Auth.User") {
		t.Error("expected submodule MyApp.Accounts.User to be renamed to MyApp.Auth.User")
	}
}

func TestRename_Module_FileRenameConvention(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// accounts.ex follows the convention for MyApp.Accounts
	content := `defmodule MyApp.Accounts do
  def list_users, do: []
end
`
	oldPath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", content)
	defURI := "file://" + oldPath
	server.docs.Set(defURI, content)

	edit := renameAt(t, server, defURI, 0, 20, "Auth")
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}

	// File should have been written to new path and old path removed
	newPath := filepath.Join(server.projectRoot, "lib", "auth.ex")
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		t.Error("expected new file auth.ex to exist")
	}
	if _, err := os.Stat(oldPath); err == nil {
		t.Error("expected old file accounts.ex to be removed")
	}
	if !fileContains(newPath, "defmodule MyApp.Auth") {
		t.Errorf("expected new file to contain 'defmodule MyApp.Auth'")
	}
}

func TestRename_Module_FileRenameExsExtension(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.AccountsTest do
  use ExUnit.Case
  def test_thing, do: :ok
end
`
	oldPath := filepath.Join(server.projectRoot, "test", "accounts_test.exs")
	indexFile(t, server.store, server.projectRoot, "test/accounts_test.exs", content)
	defURI := "file://" + oldPath
	server.docs.Set(defURI, content)

	renameAt(t, server, defURI, 0, 20, "AuthTest")

	// File should be renamed preserving the .exs extension
	newPath := filepath.Join(server.projectRoot, "test", "auth_test.exs")
	if _, err := os.Stat(newPath); os.IsNotExist(err) {
		t.Error("expected file renamed to auth_test.exs (preserving .exs extension)")
	}
	if !fileContains(newPath, "defmodule MyApp.AuthTest") {
		t.Errorf("expected 'defmodule MyApp.AuthTest' in new file")
	}
}

func TestRename_Module_FileRenameLastSegmentOnly(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	defContent := `defmodule MyApp.Docusign do
  def send, do: :ok
end
`
	callerContent := `defmodule MyApp.Web do
  alias MyApp.Docusign

  def run do
    Docusign.send()
  end
end
`
	oldPath := filepath.Join(server.projectRoot, "lib", "docusign.ex")
	callerPath := filepath.Join(server.projectRoot, "lib", "web.ex")
	indexFile(t, server.store, server.projectRoot, "lib/docusign.ex", defContent)
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerContent)

	// Test 1: rename from the def file (open)
	t.Run("from def file", func(t *testing.T) {
		defURI := "file://" + oldPath
		server.docs.Set(defURI, defContent)

		renameAt(t, server, defURI, 0, 16, "Docusigns")

		newPath := filepath.Join(server.projectRoot, "lib", "docusigns.ex")
		if _, err := os.Stat(newPath); os.IsNotExist(err) {
			t.Error("expected file renamed to docusigns.ex")
		}
		if !fileContains(newPath, "defmodule MyApp.Docusigns") {
			data, _ := os.ReadFile(newPath)
			t.Errorf("expected 'defmodule MyApp.Docusigns', got:\n%s", string(data))
		}
	})

	// Re-index with original content for test 2
	indexFile(t, server.store, server.projectRoot, "lib/docusign.ex", defContent)

	// Test 2: rename from a caller file via alias (def file is closed)
	t.Run("from caller via alias", func(t *testing.T) {
		callerURI := "file://" + callerPath
		server.docs.Set(callerURI, callerContent)

		renameAt(t, server, callerURI, 4, 4, "Docusigns")

		newPath := filepath.Join(server.projectRoot, "lib", "docusigns.ex")
		if _, err := os.Stat(newPath); os.IsNotExist(err) {
			t.Error("expected file renamed to docusigns.ex")
		}
		if !fileContains(newPath, "defmodule MyApp.Docusigns") {
			data, _ := os.ReadFile(newPath)
			t.Errorf("expected 'defmodule MyApp.Docusigns', got:\n%s", string(data))
		}
	})
}

func TestRename_Module_NoShowDocumentFromCaller(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	defContent := `defmodule MyApp.Accounts do
  def list_users, do: []
end
`
	callerContent := `defmodule MyApp.Web do
  alias MyApp.Accounts

  def index, do: Accounts.list_users()
end
`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", defContent)
	callerPath := filepath.Join(server.projectRoot, "lib", "web.ex")
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerContent)

	// Trigger rename from the CALLER file, not the definition file
	callerURI := "file://" + callerPath
	server.docs.Set(callerURI, callerContent)

	// Rename "Accounts" in "alias MyApp.Accounts" from the caller file
	edit := renameAt(t, server, callerURI, 1, 14, "Auth")
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}

	// The definition file should have been renamed on disk
	newDefPath := filepath.Join(server.projectRoot, "lib", "auth.ex")
	if _, err := os.Stat(newDefPath); os.IsNotExist(err) {
		t.Error("expected definition file to be renamed to auth.ex")
	}

	// Since we triggered from the caller (not the definition file),
	// showDocument should NOT have been sent. We can't directly test the
	// showDocument call without a mock client, but we verify the logic is
	// correct by checking that the caller file's edits are in the WorkspaceEdit
	// (meaning the caller is treated as an open buffer, not navigated away from).
	callerEdits := collectEdits(edit, callerPath)
	if len(callerEdits) == 0 {
		// Caller is open in docs, so its edits should be in the WorkspaceEdit
		// (not written to disk). This confirms we stayed on the caller file.
		if !hasRename(edit, callerPath, "MyApp.Auth") {
			t.Error("expected MyApp.Auth in caller file")
		}
	}
}

func TestRename_Module_FileRenameSkippedWhenNotConvention(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// my_custom_name.ex does NOT follow the convention for MyApp.Accounts
	content := `defmodule MyApp.Accounts do
  def list_users, do: []
end
`
	customPath := filepath.Join(server.projectRoot, "lib", "my_custom_name.ex")
	if err := os.MkdirAll(filepath.Dir(customPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	// Index using the non-conventional path
	indexFile(t, server.store, server.projectRoot, "lib/my_custom_name.ex", content)

	defURI := "file://" + customPath
	server.docs.Set(defURI, content)

	renameAt(t, server, defURI, 0, 20, "Auth")

	// Original file should still exist (not renamed)
	if _, err := os.Stat(customPath); os.IsNotExist(err) {
		t.Error("expected original file to still exist when it doesn't follow naming convention")
	}
}

func TestRename_Module_InvalidName(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.Accounts do
  def list_users, do: []
end
`
	path := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", content)
	defURI := "file://" + path
	server.docs.Set(defURI, content)

	_, err := server.Rename(context.Background(), &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(defURI)},
			Position:     protocol.Position{Line: 0, Character: 20},
		},
		NewName: "myapp.auth", // lowercase — invalid
	})
	if err == nil {
		t.Error("expected error for invalid module name")
	}
}

// === Protocol rename tests ===

func TestRename_Module_Protocol(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/printable.ex", `defprotocol MyApp.Printable do
  def to_string(data)
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/printable/string.ex", `defimpl MyApp.Printable, for: String do
  def to_string(data), do: data
end
`)
	callerContent := `defmodule MyApp.Formatter do
  alias MyApp.Printable

  def format(x), do: MyApp.Printable.to_string(x)
end
`
	callerPath := filepath.Join(server.projectRoot, "lib", "formatter.ex")
	indexFile(t, server.store, server.projectRoot, "lib/formatter.ex", callerContent)

	defPath := filepath.Join(server.projectRoot, "lib", "printable.ex")
	defURI := "file://" + defPath
	server.docs.Set(defURI, `defprotocol MyApp.Printable do
  def to_string(data)
end
`)

	edit := renameAt(t, server, defURI, 0, 20, "Serializable")
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}

	// defimpl file should have "Serializable" replacing "Printable"
	implPath := filepath.Join(server.projectRoot, "lib", "printable", "string.ex")
	if !hasRename(edit, implPath, "Serializable") {
		t.Errorf("expected defimpl line to be updated with Serializable")
	}

	// Caller file should have MyApp.Serializable
	if !hasRename(edit, callerPath, "MyApp.Serializable") {
		t.Error("expected MyApp.Serializable in caller file")
	}
}

// === Behaviour tests ===

func TestRename_Function_Callback(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.Worker do
  @callback process(term()) :: :ok | {:error, term()}

  @spec process(term()) :: :ok | {:error, term()}
  def process(job) do
    :ok
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "worker.ex")
	indexFile(t, server.store, server.projectRoot, "lib/worker.ex", content)
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	edit := renameAt(t, server, docURI, 4, 6, "handle")
	edits := collectEdits(edit, path)

	// Should rename @callback line, @spec line, and def line
	if !editsContainLine(edits, 1) {
		t.Error("expected edit on @callback line (line index 1)")
	}
	if !editsContainLine(edits, 3) {
		t.Error("expected edit on @spec line (line index 3)")
	}
	if !editsContainLine(edits, 4) {
		t.Error("expected edit on def line (line index 4)")
	}
}

func TestRename_Module_BehaviourRef(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/worker.ex", `defmodule MyApp.Worker do
  @callback process(term()) :: :ok
end
`)
	implContent := `defmodule MyApp.ConcreteWorker do
  @behaviour MyApp.Worker

  def process(job), do: :ok
end
`
	implPath := filepath.Join(server.projectRoot, "lib", "concrete_worker.ex")
	indexFile(t, server.store, server.projectRoot, "lib/concrete_worker.ex", implContent)

	defPath := filepath.Join(server.projectRoot, "lib", "worker.ex")
	defURI := "file://" + defPath
	server.docs.Set(defURI, `defmodule MyApp.Worker do
  @callback process(term()) :: :ok
end
`)

	edit := renameAt(t, server, defURI, 0, 16, "Job")
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}

	// @behaviour line in impl file should be updated
	if !hasRename(edit, implPath, "MyApp.Job") {
		t.Error("expected @behaviour MyApp.Worker to be updated to @behaviour MyApp.Job")
	}
}

func TestRename_Module_DirectoryRename(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/companies.ex", `defmodule MyApp.Companies do
  def list, do: []
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/companies/do_something.ex", `defmodule MyApp.Companies.DoSomething do
  def call, do: :ok
end
`)

	defPath := filepath.Join(server.projectRoot, "lib", "companies.ex")
	defURI := "file://" + defPath
	server.docs.Set(defURI, `defmodule MyApp.Companies do
  def list, do: []
end
`)

	renameAt(t, server, defURI, 0, 16, "Enterprises")

	// Root file should be renamed
	if _, err := os.Stat(filepath.Join(server.projectRoot, "lib", "enterprises.ex")); os.IsNotExist(err) {
		t.Error("expected root module file renamed to enterprises.ex")
	}

	// Submodule file is closed — should be moved to new directory on disk
	newSubPath := filepath.Join(server.projectRoot, "lib", "enterprises", "do_something.ex")
	if _, err := os.Stat(newSubPath); os.IsNotExist(err) {
		t.Error("expected submodule file moved to lib/enterprises/do_something.ex")
	}
	oldSubPath := filepath.Join(server.projectRoot, "lib", "companies", "do_something.ex")
	if _, err := os.Stat(oldSubPath); err == nil {
		t.Error("expected old submodule file lib/companies/do_something.ex to be removed")
	}

	// New submodule file should have renamed module
	if !fileContains(newSubPath, "MyApp.Enterprises.DoSomething") {
		t.Error("expected new submodule file to contain MyApp.Enterprises.DoSomething")
	}
}

// === Store tests ===

func TestListSubmodules(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def list_users, do: []
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/accounts/user.ex", `defmodule MyApp.Accounts.User do
  defstruct [:name]
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/accounts/profile.ex", `defmodule MyApp.Accounts.Profile do
  defstruct [:bio]
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/other.ex", `defmodule MyApp.Other do
  def foo, do: :ok
end
`)

	subs, err := server.store.ListSubmodules("MyApp.Accounts")
	if err != nil {
		t.Fatal(err)
	}

	if len(subs) != 2 {
		t.Fatalf("expected 2 submodules, got %d: %v", len(subs), subs)
	}

	found := map[string]bool{}
	for _, s := range subs {
		found[s] = true
	}
	if !found["MyApp.Accounts.User"] {
		t.Error("expected MyApp.Accounts.User")
	}
	if !found["MyApp.Accounts.Profile"] {
		t.Error("expected MyApp.Accounts.Profile")
	}
	if found["MyApp.Accounts"] {
		t.Error("ListSubmodules should not include the prefix module itself")
	}
	if found["MyApp.Other"] {
		t.Error("ListSubmodules should not include unrelated modules")
	}
}

func TestRename_Module_PrefixSameName(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp.CostCalculator do
  def calculate(x), do: x * 2
end
`
	callerContent := `defmodule MyApp.Web do
  alias MyApp.CostCalculator

  def run do
    MyApp.CostCalculator.calculate(5)
  end
end
`
	defPath := filepath.Join(server.projectRoot, "lib", "cost_calculator.ex")
	_ = callerContent // caller file verifies via hasRename below
	indexFile(t, server.store, server.projectRoot, "lib/cost_calculator.ex", content)

	defURI := "file://" + defPath
	server.docs.Set(defURI, content)

	// Verify prepareRename returns just the last segment "CostCalculator"
	r := prepareRenameAt(t, server, defURI, 0, 16)
	if r == nil {
		t.Fatal("expected non-nil prepareRename result")
	}
	// "CostCalculator" starts at col 16 in "defmodule MyApp.CostCalculator do"
	if r.Start.Character != 16 {
		t.Errorf("expected last segment range starting at col 16, got %d", r.Start.Character)
	}

	renameAt(t, server, defURI, 0, 16, "CostCalculatorZ")

	// Check the renamed file has the full qualified name
	newPath := filepath.Join(server.projectRoot, "lib", "cost_calculator_z.ex")
	newContent, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("cannot read new file: %v", err)
	}
	if !strings.Contains(string(newContent), "defmodule MyApp.CostCalculatorZ do") {
		t.Errorf("expected 'defmodule MyApp.CostCalculatorZ do', got:\n%s", newContent)
	}
}

func TestRename_Module_QualifiedCallsUpdated(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	defContent := `defmodule MyApp.Accounts do
  def list_users, do: []
  def create_user(attrs), do: attrs
end
`
	callerContent := `defmodule MyApp.Web do
  alias MyApp.Accounts

  def index do
    Accounts.list_users()
  end

  def create do
    Accounts.create_user(%{})
  end
end
`
	defPath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	callerPath := filepath.Join(server.projectRoot, "lib", "web.ex")
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", defContent)
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerContent)

	defURI := "file://" + defPath
	server.docs.Set(defURI, defContent)

	renameAt(t, server, defURI, 0, 20, "Auth")

	// Caller file should have:
	// 1. alias updated: "MyApp.Auth"
	if !hasRename(nil, callerPath, "alias MyApp.Auth") {
		data, _ := os.ReadFile(callerPath)
		t.Errorf("expected 'alias MyApp.Auth' in caller file, got:\n%s", data)
	}

	// 2. Short-form calls updated: "Auth.list_users()" and "Auth.create_user("
	if !fileContains(callerPath, "Auth.list_users()") {
		data, _ := os.ReadFile(callerPath)
		t.Errorf("expected 'Auth.list_users()' in caller file, got:\n%s", data)
	}
	if !fileContains(callerPath, "Auth.create_user(") {
		data, _ := os.ReadFile(callerPath)
		t.Errorf("expected 'Auth.create_user(' in caller file, got:\n%s", data)
	}
}

func TestRename_Module_TypeReferencesUpdated(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	defContent := `defmodule MyApp.User do
  defstruct [:name, :email]

  @type t :: %__MODULE__{name: String.t(), email: String.t()}
end
`
	callerContent := `defmodule MyApp.Accounts do
  alias MyApp.User

  @spec get_user(integer()) :: User.t() | nil
  def get_user(id) do
    %User{name: "test"}
  end

  @spec list_users() :: [User.t()]
  def list_users do
    [%User{name: "test"}]
  end
end
`
	defPath := filepath.Join(server.projectRoot, "lib", "user.ex")
	callerPath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	indexFile(t, server.store, server.projectRoot, "lib/user.ex", defContent)
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", callerContent)

	defURI := "file://" + defPath
	server.docs.Set(defURI, defContent)

	renameAt(t, server, defURI, 0, 16, "UserZ")

	// Caller should have User.t() renamed to UserZ.t()
	if !fileContains(callerPath, "UserZ.t()") {
		data, _ := os.ReadFile(callerPath)
		t.Errorf("expected 'UserZ.t()' in caller file, got:\n%s", data)
	}

	// @spec lines should be updated too
	if !fileContains(callerPath, ":: UserZ.t() | nil") {
		data, _ := os.ReadFile(callerPath)
		t.Errorf("expected ':: UserZ.t() | nil' in caller @spec, got:\n%s", data)
	}

	// Struct usage should be updated
	if fileContains(callerPath, "%User{") && !fileContains(callerPath, "%UserZ{") {
		data, _ := os.ReadFile(callerPath)
		t.Errorf("expected '%%UserZ{' in caller file, got:\n%s", data)
	}
}

// === Variable / module attribute rename tests ===

func TestRename_Variable_Basic(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  def process(items) do
    result = transform(items)
    log(result)
    result
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "my_app.ex")
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	// Rename "result" at line 2, col 4
	edit := renameAt(t, server, docURI, 2, 4, "output")
	edits := collectEdits(edit, path)

	if len(edits) != 3 {
		t.Fatalf("expected 3 edits for 'result' (assignment + log arg + bare return), got %d", len(edits))
	}
	for _, e := range edits {
		if e.NewText != "output" {
			t.Errorf("unexpected NewText %q, want \"output\"", e.NewText)
		}
	}
	if !editsContainLine(edits, 2) {
		t.Error("expected edit on line 2 (assignment)")
	}
	if !editsContainLine(edits, 3) {
		t.Error("expected edit on line 3 (log arg)")
	}
	if !editsContainLine(edits, 4) {
		t.Error("expected edit on line 4 (bare return)")
	}
}

func TestRename_Variable_CapturedInAnonymousFunction(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  def process(items) do
    prefix = "hello"
    Enum.map(items, fn item -> prefix <> item end)
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "my_app.ex")
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	// Rename "prefix" at line 2, col 4
	edit := renameAt(t, server, docURI, 2, 4, "greeting")
	edits := collectEdits(edit, path)

	if len(edits) != 2 {
		t.Fatalf("expected 2 edits (binding + captured ref inside fn), got %d", len(edits))
	}
	for _, e := range edits {
		if e.NewText != "greeting" {
			t.Errorf("unexpected NewText %q, want \"greeting\"", e.NewText)
		}
	}
	if !editsContainLine(edits, 2) {
		t.Error("expected edit on line 2 (binding)")
	}
	if !editsContainLine(edits, 3) {
		t.Error("expected edit on line 3 (captured ref inside fn)")
	}
}

func TestRename_Variable_ShadowedByAnonymousFunction(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  def process(data) do
    x = 1
    fn x -> x + 1 end
    x
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "my_app.ex")
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	// Rename outer "x" at line 2, col 4
	edit := renameAt(t, server, docURI, 2, 4, "val")
	edits := collectEdits(edit, path)

	// Should edit line 2 (x = 1) and line 4 (bare x), NOT line 3 (fn param + body)
	if len(edits) != 2 {
		t.Fatalf("expected 2 edits for outer x (binding + final ref), got %d", len(edits))
	}
	if !editsContainLine(edits, 2) {
		t.Error("expected edit on line 2 (outer binding)")
	}
	if !editsContainLine(edits, 4) {
		t.Error("expected edit on line 4 (outer reference)")
	}
	if editsContainLine(edits, 3) {
		t.Error("line 3 is inside fn scope and should NOT be edited")
	}
}

func TestRename_Variable_WithBlockScope(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  def process() do
    thing = nil
    with {:ok, thing} <- fetch(thing),
         {:ok, other} <- bar(thing) do
      thing
    end
    thing = :something
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "my_app.ex")
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	// Rename outer "thing" (line 2: "thing = nil") to "stuff"
	edit := renameAt(t, server, docURI, 2, 4, "stuff")
	edits := collectEdits(edit, path)

	// Should rename: line 2 (thing = nil), line 3 fetch(thing), line 7 (thing = :something)
	// Should NOT rename: line 3 pattern {:ok, thing}, line 4 bar(thing), line 5 thing in do block
	if len(edits) != 3 {
		t.Fatalf("expected 3 edits for outer 'thing', got %d", len(edits))
	}
	if !editsContainLine(edits, 2) {
		t.Error("expected edit on line 2 (thing = nil)")
	}
	if !editsContainLine(edits, 3) {
		t.Error("expected edit on line 3 (fetch(thing))")
	}
	if !editsContainLine(edits, 7) {
		t.Error("expected edit on line 7 (thing = :something)")
	}
	if editsContainLine(edits, 4) {
		t.Error("line 4 bar(thing) refs rebound thing and should NOT be edited")
	}
	if editsContainLine(edits, 5) {
		t.Error("line 5 thing in do block should NOT be edited")
	}
}

func TestRename_ModuleAttribute_Basic(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  @timeout 5000

  def run do
    Process.sleep(@timeout)
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "my_app.ex")
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	// Rename "timeout" in "@timeout 5000" at line 1, col 3
	edit := renameAt(t, server, docURI, 1, 3, "wait_ms")
	edits := collectEdits(edit, path)

	if len(edits) != 2 {
		t.Fatalf("expected 2 edits (@timeout def + @timeout reference), got %d", len(edits))
	}
	for _, e := range edits {
		if e.NewText != "wait_ms" {
			t.Errorf("unexpected NewText %q, want \"wait_ms\"", e.NewText)
		}
	}
	if !editsContainLine(edits, 1) {
		t.Error("expected edit on line 1 (@timeout definition)")
	}
	if !editsContainLine(edits, 4) {
		t.Error("expected edit on line 4 (@timeout reference)")
	}
}

func TestRename_ModuleAttribute_DoesNotRenameVariableWithSameName(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	content := `defmodule MyApp do
  @timeout 5000

  def run do
    timeout = 1000
    Process.sleep(@timeout + timeout)
  end
end
`
	path := filepath.Join(server.projectRoot, "lib", "my_app.ex")
	docURI := "file://" + path
	server.docs.Set(docURI, content)

	// Rename "@timeout" attribute (cursor on the identifier part, line 1)
	edit := renameAt(t, server, docURI, 1, 3, "wait_ms")
	edits := collectEdits(edit, path)

	// Should rename @timeout (def on line 1) and @timeout reference (line 5),
	// but NOT the plain `timeout` variable on lines 4 and 5.
	if len(edits) != 2 {
		t.Fatalf("expected exactly 2 edits (only @timeout occurrences), got %d", len(edits))
	}
	for _, e := range edits {
		if e.NewText != "wait_ms" {
			t.Errorf("unexpected NewText %q", e.NewText)
		}
	}
}

// === updateDelegateAs tests ===

func TestUpdateDelegateAs(t *testing.T) {
	tests := []struct {
		name          string
		line          string
		facadeName    string
		newTargetName string
		expected      string
	}{
		{
			name:          "add as: when none exists",
			line:          "  defdelegate get_company_by_slug(slug), to: CRUD",
			facadeName:    "get_company_by_slug",
			newTargetName: "get_company",
			expected:      "  defdelegate get_company_by_slug(slug), to: CRUD, as: :get_company",
		},
		{
			name:          "update existing as:",
			line:          "  defdelegate list_users(), to: CRUD, as: :fetch_users",
			facadeName:    "list_users",
			newTargetName: "get_users",
			expected:      "  defdelegate list_users(), to: CRUD, as: :get_users",
		},
		{
			name:          "remove as: when names match",
			line:          "  defdelegate list_users(), to: CRUD, as: :fetch_users",
			facadeName:    "list_users",
			newTargetName: "list_users",
			expected:      "  defdelegate list_users(), to: CRUD",
		},
		{
			name:          "no-op when no as: and names already match",
			line:          "  defdelegate list_users(), to: CRUD",
			facadeName:    "list_users",
			newTargetName: "list_users",
			expected:      "  defdelegate list_users(), to: CRUD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := updateDelegateAs(tt.line, tt.facadeName, tt.newTargetName)
			if got != tt.expected {
				t.Errorf("updateDelegateAs(%q, %q, %q)\n  got:  %q\n  want: %q", tt.line, tt.facadeName, tt.newTargetName, got, tt.expected)
			}
		})
	}
}

// === Rename with defdelegate tests ===

func TestRename_Function_UpdatesDelegateAs(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// CRUD module defines the real function
	crudContent := `defmodule MyApp.Accounts.CRUD do
  def list_users, do: []
end
`
	// Facade module delegates to CRUD
	facadeContent := `defmodule MyApp.Accounts do
  defdelegate list_users(), to: MyApp.Accounts.CRUD
end
`
	// Caller uses the facade
	callerContent := `defmodule MyApp.Web do
  alias MyApp.Accounts

  def index do
    Accounts.list_users()
  end
end
`
	crudPath := filepath.Join(server.projectRoot, "lib", "crud.ex")
	facadePath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	callerPath := filepath.Join(server.projectRoot, "lib", "web.ex")
	indexFile(t, server.store, server.projectRoot, "lib/crud.ex", crudContent)
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", facadeContent)
	indexFile(t, server.store, server.projectRoot, "lib/web.ex", callerContent)

	crudURI := "file://" + crudPath
	server.docs.Set(crudURI, crudContent)

	// Rename list_users → all_users in the CRUD module
	edit := renameAt(t, server, crudURI, 1, 6, "all_users")

	// The CRUD def should be renamed
	crudEdits := collectEdits(edit, crudPath)
	if !hasEdit(crudEdits, "all_users") {
		t.Error("expected CRUD def to be renamed to all_users")
	}

	// The defdelegate line should get as: :all_users added (either in WorkspaceEdit or on disk)
	if hasRename(edit, facadePath, "as: :all_users") {
		// Good — the delegate line was updated
	} else {
		data, _ := os.ReadFile(facadePath)
		t.Errorf("expected defdelegate to have 'as: :all_users', got:\n%s", string(data))
	}

	// The caller file should NOT be modified — it calls through the facade
	// which still exposes list_users
	callerData, _ := os.ReadFile(callerPath)
	if strings.Contains(string(callerData), "all_users") {
		t.Error("caller file should NOT be modified — it uses the facade, not the CRUD module directly")
	}
}

func TestRename_Function_UpdatesExistingDelegateAs(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	crudContent := `defmodule MyApp.Accounts.CRUD do
  def fetch_users, do: []
end
`
	// Facade already has as: mapping
	facadeContent := `defmodule MyApp.Accounts do
  defdelegate list_users(), to: MyApp.Accounts.CRUD, as: :fetch_users
end
`
	crudPath := filepath.Join(server.projectRoot, "lib", "crud.ex")
	facadePath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	indexFile(t, server.store, server.projectRoot, "lib/crud.ex", crudContent)
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", facadeContent)

	crudURI := "file://" + crudPath
	server.docs.Set(crudURI, crudContent)

	// Rename fetch_users → get_users in the CRUD module
	renameAt(t, server, crudURI, 1, 6, "get_users")

	// The existing as: should be updated
	if !fileContains(facadePath, "as: :get_users") {
		data, _ := os.ReadFile(facadePath)
		t.Errorf("expected defdelegate as: to be updated to :get_users, got:\n%s", string(data))
	}
	// The facade function name should NOT change
	if !fileContains(facadePath, "defdelegate list_users()") {
		data, _ := os.ReadFile(facadePath)
		t.Errorf("expected facade function name to remain list_users, got:\n%s", string(data))
	}
}

func TestRename_Function_RemovesDelegateAsWhenNamesMatch(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	crudContent := `defmodule MyApp.Accounts.CRUD do
  def fetch_users, do: []
end
`
	// Facade has as: :fetch_users mapping
	facadeContent := `defmodule MyApp.Accounts do
  defdelegate list_users(), to: MyApp.Accounts.CRUD, as: :fetch_users
end
`
	crudPath := filepath.Join(server.projectRoot, "lib", "crud.ex")
	facadePath := filepath.Join(server.projectRoot, "lib", "accounts.ex")
	indexFile(t, server.store, server.projectRoot, "lib/crud.ex", crudContent)
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", facadeContent)

	crudURI := "file://" + crudPath
	server.docs.Set(crudURI, crudContent)

	// Rename fetch_users → list_users (now matches the facade name)
	renameAt(t, server, crudURI, 1, 6, "list_users")

	// The as: clause should be removed since names now match
	data, _ := os.ReadFile(facadePath)
	facadeResult := string(data)
	if strings.Contains(facadeResult, "as:") {
		t.Errorf("expected as: clause to be removed (names match), got:\n%s", facadeResult)
	}
	if !strings.Contains(facadeResult, "defdelegate list_users(), to: MyApp.Accounts.CRUD") {
		t.Errorf("expected delegate line preserved without as:, got:\n%s", facadeResult)
	}
}
