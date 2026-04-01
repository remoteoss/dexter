package lsp

import (
	"context"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
)

func TestExtractDocAbove_Heredoc(t *testing.T) {
	src := `defmodule MyApp.Users do
  @doc """
  Creates a new user with the given attributes.

  Returns {:ok, user} on success.
  """
  def create(attrs) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc, spec := extractDocAbove(lines, 6)

	if doc == "" {
		t.Fatal("expected doc, got empty")
	}
	if !strings.Contains(doc, "Creates a new user") {
		t.Errorf("expected doc to contain 'Creates a new user', got %q", doc)
	}
	if !strings.Contains(doc, "Returns {:ok, user}") {
		t.Errorf("expected doc to contain return info, got %q", doc)
	}
	if spec != "" {
		t.Errorf("expected no spec, got %q", spec)
	}
}

func TestExtractDocAbove_SingleLine(t *testing.T) {
	src := `defmodule MyApp.Users do
  @doc "Creates a new user."
  def create(attrs) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc, _ := extractDocAbove(lines, 2)

	if doc != "Creates a new user." {
		t.Errorf("expected 'Creates a new user.', got %q", doc)
	}
}

func TestExtractDocAbove_WithSpec(t *testing.T) {
	src := `defmodule MyApp.Users do
  @doc """
  Creates a new user.
  """
  @spec create(map()) :: {:ok, User.t()} | {:error, Ecto.Changeset.t()}
  def create(attrs) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc, spec := extractDocAbove(lines, 5)

	if !strings.Contains(doc, "Creates a new user") {
		t.Errorf("expected doc content, got %q", doc)
	}
	if !strings.Contains(spec, "@spec create(map())") {
		t.Errorf("expected spec content, got %q", spec)
	}
}

func TestExtractDocAbove_MultiLineSpec(t *testing.T) {
	src := `defmodule MyApp.Users do
  @spec create(attrs :: map()) ::
          {:ok, User.t()} | {:error, Ecto.Changeset.t()}
  def create(attrs) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	_, spec := extractDocAbove(lines, 3)

	if !strings.Contains(spec, "@spec create") {
		t.Errorf("expected spec to contain '@spec create', got %q", spec)
	}
	if !strings.Contains(spec, "{:ok, User.t()}") {
		t.Errorf("expected spec to contain return type, got %q", spec)
	}
}

func TestExtractDocAbove_DocFalse(t *testing.T) {
	src := `defmodule MyApp.Users do
  @doc false
  def internal_helper(x) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc, _ := extractDocAbove(lines, 2)

	if doc != "" {
		t.Errorf("expected empty doc for @doc false, got %q", doc)
	}
}

func TestExtractDocAbove_NoDoc(t *testing.T) {
	src := `defmodule MyApp.Users do
  def create(attrs) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc, spec := extractDocAbove(lines, 1)

	if doc != "" {
		t.Errorf("expected no doc, got %q", doc)
	}
	if spec != "" {
		t.Errorf("expected no spec, got %q", spec)
	}
}

func TestExtractDocAbove_DoesNotLeakFromPreviousFunction(t *testing.T) {
	src := `defmodule MyApp.Users do
  @doc """
  First function doc.
  """
  def first(x) do
    :ok
  end

  def second(y) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc, _ := extractDocAbove(lines, 8)

	if doc != "" {
		t.Errorf("expected no doc for second function, got %q", doc)
	}
}

func TestExtractDocAbove_SpecBeforeDoc(t *testing.T) {
	src := `defmodule MyApp.Users do
  @spec create(map()) :: :ok
  @doc "Creates a user."
  def create(attrs) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc, spec := extractDocAbove(lines, 3)

	if doc != "Creates a user." {
		t.Errorf("expected doc, got %q", doc)
	}
	if !strings.Contains(spec, "@spec create") {
		t.Errorf("expected spec, got %q", spec)
	}
}

func TestExtractModuledoc_Heredoc(t *testing.T) {
	src := `defmodule MyApp.Users do
  @moduledoc """
  Manages user accounts.

  Provides CRUD operations.
  """

  def create(attrs) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc := extractModuledoc(lines, 0)

	if !strings.Contains(doc, "Manages user accounts") {
		t.Errorf("expected moduledoc content, got %q", doc)
	}
	if !strings.Contains(doc, "Provides CRUD operations") {
		t.Errorf("expected second paragraph, got %q", doc)
	}
}

func TestExtractModuledoc_SingleLine(t *testing.T) {
	src := `defmodule MyApp.Users do
  @moduledoc "Manages user accounts."

  def create(attrs) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc := extractModuledoc(lines, 0)

	if doc != "Manages user accounts." {
		t.Errorf("expected 'Manages user accounts.', got %q", doc)
	}
}

func TestExtractModuledoc_False(t *testing.T) {
	src := `defmodule MyApp.Internal do
  @moduledoc false

  def helper(x), do: x
end`
	lines := strings.Split(src, "\n")
	doc := extractModuledoc(lines, 0)

	if doc != "" {
		t.Errorf("expected empty doc for @moduledoc false, got %q", doc)
	}
}

func TestExtractModuledoc_AfterUseAndAlias(t *testing.T) {
	src := `defmodule MyApp.Users do
  use Ecto.Schema
  alias MyApp.Repo

  @moduledoc """
  Users context module.
  """

  def list do
    Repo.all(User)
  end
end`
	lines := strings.Split(src, "\n")
	doc := extractModuledoc(lines, 0)

	if !strings.Contains(doc, "Users context module") {
		t.Errorf("expected moduledoc after use/alias, got %q", doc)
	}
}

func TestExtractModuledoc_None(t *testing.T) {
	src := `defmodule MyApp.Users do
  def create(attrs) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc := extractModuledoc(lines, 0)

	if doc != "" {
		t.Errorf("expected no moduledoc, got %q", doc)
	}
}

func TestExtractSignature(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{"  def create(attrs) do", "def create(attrs)"},
		{"  defp validate(attrs), do: :ok", "defp validate(attrs), do: :ok"},
		{"  defmacro my_macro(arg) do", "defmacro my_macro(arg)"},
		{"  defmodule MyApp.Users do", "defmodule MyApp.Users"},
	}

	for _, tt := range tests {
		lines := []string{tt.line}
		got := extractSignature(lines, 0)
		if got != tt.want {
			t.Errorf("extractSignature(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestFormatHoverContent(t *testing.T) {
	content := formatHoverContent("Creates a user.", "@spec create(map()) :: :ok", "def create(attrs)")

	if !strings.Contains(content, "```elixir") {
		t.Error("expected elixir code block")
	}
	if !strings.Contains(content, "def create(attrs)") {
		t.Error("expected signature in code block")
	}
	if !strings.Contains(content, "@spec create") {
		t.Error("expected spec in code block")
	}
	if !strings.Contains(content, "Creates a user.") {
		t.Error("expected doc text")
	}
}

func TestFormatHoverContent_NoDoc(t *testing.T) {
	content := formatHoverContent("", "", "def create(attrs)")

	if !strings.Contains(content, "def create(attrs)") {
		t.Error("expected signature even without doc")
	}
}

func TestFormatHoverContent_Empty(t *testing.T) {
	content := formatHoverContent("", "", "")

	if content != "" {
		t.Errorf("expected empty content, got %q", content)
	}
}

func TestDedentBlock(t *testing.T) {
	lines := []string{
		"    First line.",
		"    Second line.",
		"",
		"    Third line.",
	}
	got := dedentBlock(lines)
	want := "First line.\nSecond line.\n\nThird line."
	if got != want {
		t.Errorf("dedentBlock = %q, want %q", got, want)
	}
}

func TestExtractQuotedString(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`"hello world"`, "hello world"},
		{`"with \"escape\""`, `with \"escape\"`},
		{`""`, ""},
		{`"unterminated`, ""},
	}

	for _, tt := range tests {
		got := extractQuotedString(tt.input)
		if got != tt.want {
			t.Errorf("extractQuotedString(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func hoverAt(t *testing.T, server *Server, uri string, line, col uint32) *protocol.Hover {
	t.Helper()
	result, err := server.Hover(context.Background(), &protocol.HoverParams{
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

func TestHover_FunctionWithDoc(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  @doc """
  Creates a new account.
  """
  @spec create(map()) :: {:ok, term()}
  def create(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  alias MyApp.Accounts

  Accounts.create(attrs)
end`)

	hover := hoverAt(t, server, uri, 3, 13)
	if hover == nil {
		t.Fatal("expected hover result")
	}

	content := hover.Contents.Value
	if !strings.Contains(content, "Creates a new account") {
		t.Errorf("expected doc in hover, got %q", content)
	}
	if !strings.Contains(content, "@spec create") {
		t.Errorf("expected spec in hover, got %q", content)
	}
}

func TestHover_ModuleWithModuledoc(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  @moduledoc """
  The Accounts context.
  """

  def create(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Accounts")

	hover := hoverAt(t, server, uri, 0, 8)
	if hover == nil {
		t.Fatal("expected hover result for module")
	}

	content := hover.Contents.Value
	if !strings.Contains(content, "The Accounts context") {
		t.Errorf("expected moduledoc in hover, got %q", content)
	}
}

func TestHover_NoDocStillShowsSignature(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create(attrs) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  MyApp.Accounts.create(attrs)")

	hover := hoverAt(t, server, uri, 0, 19)
	if hover == nil {
		t.Fatal("expected hover result even without doc")
	}

	content := hover.Contents.Value
	if !strings.Contains(content, "def create(attrs)") {
		t.Errorf("expected signature in hover, got %q", content)
	}
}

func TestHover_LocalFunction(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyModule do
  @doc "Helper function."
  @spec helper(integer()) :: :ok
  def helper(x) do
    :ok
  end

  def caller do
    helper(1)
  end
end`)

	hover := hoverAt(t, server, uri, 8, 6)
	if hover == nil {
		t.Fatal("expected hover for local function")
	}

	content := hover.Contents.Value
	if !strings.Contains(content, "Helper function.") {
		t.Errorf("expected doc for local function, got %q", content)
	}
	if !strings.Contains(content, "@spec helper") {
		t.Errorf("expected spec for local function, got %q", content)
	}
}

func TestHover_ExternalFile(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	externalDir := t.TempDir()
	indexFile(t, server.store, externalDir, "lib/enum.ex", `defmodule Enum do
  @moduledoc """
  Functions for working with collections.
  """

  @doc """
  Maps the given function over the enumerable.
  """
  @spec map(t(), (element() -> any())) :: list()
  def map(enumerable, fun) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, "  Enum.map(list, fn x -> x end)")

	hover := hoverAt(t, server, uri, 0, 7)
	if hover == nil {
		t.Fatal("expected hover for external module function")
	}

	content := hover.Contents.Value
	if !strings.Contains(content, "Maps the given function") {
		t.Errorf("expected doc from external file, got %q", content)
	}
}

func TestHover_NoResult(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	uri := "file:///test.ex"
	server.docs.Set(uri, "  :ok")

	hover := hoverAt(t, server, uri, 0, 3)
	if hover != nil {
		t.Errorf("expected nil hover for atom, got %+v", hover)
	}
}

func TestHover_TypeWithTypedoc(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/user.ex", `defmodule MyApp.User do
  @typedoc """
  The primary user struct.
  """
  @type t() :: %__MODULE__{}

  @typedoc "An opaque session token."
  @opaque token :: String.t()

  @typep internal :: map()
end
`)

	uri := "file:///test.ex"

	// Hover on t — heredoc typedoc (col=11 points to 't' in "MyApp.User.t()")
	server.docs.Set(uri, "MyApp.User.t()")
	hover := hoverAt(t, server, uri, 0, 11)
	if hover == nil {
		t.Fatal("expected hover for @type t")
	}
	if !strings.Contains(hover.Contents.Value, "primary user struct") {
		t.Errorf("expected typedoc in hover, got %q", hover.Contents.Value)
	}
	if !strings.Contains(hover.Contents.Value, "@type t()") {
		t.Errorf("expected type signature in hover, got %q", hover.Contents.Value)
	}

	// Hover on token — single-line typedoc (col=11 points to 't' in "MyApp.User.token()")
	server.docs.Set(uri, "MyApp.User.token()")
	hover = hoverAt(t, server, uri, 0, 11)
	if hover == nil {
		t.Fatal("expected hover for @opaque token")
	}
	if !strings.Contains(hover.Contents.Value, "opaque session token") {
		t.Errorf("expected typedoc in hover, got %q", hover.Contents.Value)
	}
}

func TestHover_TypeDocNotLeakingAcrossTypes(t *testing.T) {
	// @typedoc should only apply to the immediately following @type,
	// not to subsequent ones.
	src := `defmodule MyApp.User do
  @typedoc "First type."
  @type first :: integer()
  @type second :: string()
end`
	lines := strings.Split(src, "\n")

	// extractDocAbove for second (line index 3) should find no doc —
	// the @typedoc belongs to first, not second.
	doc, _ := extractDocAbove(lines, 3)
	if doc != "" {
		t.Errorf("expected no doc for second type, got %q", doc)
	}
}

func TestHover_SigilHeredoc(t *testing.T) {
	src := `defmodule MyApp.Users do
  @doc ~S"""
  Creates a user.

  Use #{interpolation} safely.
  """
  def create(attrs) do
    :ok
  end
end`
	lines := strings.Split(src, "\n")
	doc, _ := extractDocAbove(lines, 6)

	if !strings.Contains(doc, "Creates a user") {
		t.Errorf("expected doc from sigil heredoc, got %q", doc)
	}
	if !strings.Contains(doc, "#{interpolation}") {
		t.Errorf("expected raw interpolation preserved, got %q", doc)
	}
}
