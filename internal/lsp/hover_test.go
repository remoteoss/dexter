package lsp

import (
	"context"
	"path/filepath"
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

	src := `defmodule MyModule do
  @doc "Helper function."
  @spec helper(integer()) :: :ok
  def helper(x) do
    :ok
  end

  def caller do
    helper(1)
  end
end`
	path := filepath.Join(server.projectRoot, "lib", "my_module.ex")
	indexFile(t, server.store, server.projectRoot, "lib/my_module.ex", src)
	uri := "file://" + path
	server.docs.Set(uri, src)

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

func TestHover_UseInjectedImport(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Index the module that will be `use`d — it imports itself via an alias
	indexFile(t, server.store, server.projectRoot, "lib/my_schema.ex", `defmodule MyApp.Schema do
  alias MyApp.Schema

  defmacro __using__(_opts) do
    quote do
      import Ecto.Schema
      import Schema
    end
  end

  @doc """
  Defines a schema with extended options.
  """
  defmacro schema(source, do: block) do
    :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.User do
  use MyApp.Schema

  schema "users" do
  end
end`)

	// col=2 is on 's' of "schema" (bare call)
	hover := hoverAt(t, server, uri, 3, 2)
	if hover == nil {
		t.Fatal("expected hover for use-injected macro")
	}
	if !strings.Contains(hover.Contents.Value, "Defines a schema with extended options") {
		t.Errorf("expected doc for schema macro, got %q", hover.Contents.Value)
	}
}

func TestHover_UseInjectedInlineDef(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Index the module with a function defined inline in the quote do block
	indexFile(t, server.store, server.projectRoot, "lib/my_helpers.ex", `defmodule MyApp.Helpers do
  defmacro __using__(_opts) do
    quote do
      @doc "Doubles the value."
      def double(x), do: x * 2
    end
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.User do
  use MyApp.Helpers

  def call do
    double(5)
  end
end`)

	// col=4 is on 'd' of "double" (bare call)
	hover := hoverAt(t, server, uri, 4, 4)
	if hover == nil {
		t.Fatal("expected hover for use-injected inline def")
	}
	if !strings.Contains(hover.Contents.Value, "double") {
		t.Errorf("expected signature for inline def, got %q", hover.Contents.Value)
	}
}

func TestHover_DoubleUseChain(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Middleware layer: its __using__ delegates to the base layer via use
	indexFile(t, server.store, server.projectRoot, "lib/base_worker.ex", `defmodule MyApp.BaseWorker do
  defmacro __using__(_opts) do
    quote do
      import MyApp.BaseWorker, only: [args_schema: 1]
    end
  end

  @doc "Defines the argument schema."
  defmacro args_schema(do: _block) do
    quote do: :ok
  end
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/workflow.ex", `defmodule MyApp.Workflow do
  defmacro __using__(opts) do
    quote do
      use MyApp.BaseWorker, unquote(opts)
    end
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.FileWorker do
  use MyApp.Workflow

  args_schema do
    :ok
  end
end`)

	// col=2 is on 'a' of "args_schema"
	hover := hoverAt(t, server, uri, 3, 2)
	if hover == nil {
		t.Fatal("expected hover for double-use-injected macro")
	}
	if !strings.Contains(hover.Contents.Value, "args_schema") {
		t.Errorf("expected args_schema in hover, got %q", hover.Contents.Value)
	}
}

func TestHover_TripleUseChain_DynamicModule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Base layer: defines args_schema macro and imports it in __using__
	indexFile(t, server.store, server.projectRoot, "lib/pro_worker.ex", `defmodule Oban.Pro.Worker do
  defmacro __using__(_opts) do
    quote do
      import Oban.Pro.Worker, only: [args_schema: 1]
    end
  end

  @doc "Defines the argument schema."
  defmacro args_schema(do: _block) do
    quote do: :ok
  end
end
`)
	// Middle layer: uses a dynamic module via unquote(oban_module)
	indexFile(t, server.store, server.projectRoot, "lib/oban_worker.ex", `defmodule Remote.Oban.Worker do
  defmacro __using__(opts) do
    {oban_module, opts} = Keyword.pop(opts, :oban_module, Oban.Worker)

    quote do
      use unquote(oban_module), unquote(opts)
    end
  end
end
`)
	// Top layer: sets oban_module to Oban.Pro.Worker via Keyword.put_new
	indexFile(t, server.store, server.projectRoot, "lib/pro_wrapper.ex", `defmodule Remote.Oban.Pro.Worker do
  defmacro __using__(opts) do
    opts = Keyword.put_new(opts, :oban_module, Oban.Pro.Worker)

    quote do
      use Remote.Oban.Worker, unquote(opts)
    end
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.FileWorker do
  use Remote.Oban.Pro.Worker, owner: :my_team, queue: :default

  args_schema do
    field :employer_id, :id, required: true
  end
end`)

	// col=2 is on 'a' of "args_schema"
	hover := hoverAt(t, server, uri, 3, 2)
	if hover == nil {
		t.Fatal("expected hover for triple-use-chain with dynamic module")
	}
	if !strings.Contains(hover.Contents.Value, "args_schema") {
		t.Errorf("expected args_schema in hover, got %q", hover.Contents.Value)
	}
	if !strings.Contains(hover.Contents.Value, "Defines the argument schema") {
		t.Errorf("expected doc content in hover, got %q", hover.Contents.Value)
	}
}

func TestHover_CaseTemplateUsingWithHelperFunction(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Phoenix.ConnTest defines build_conn
	indexFile(t, server.store, server.projectRoot, "lib/conn_test.ex", `defmodule Phoenix.ConnTest do
  @doc "Builds a test connection."
  def build_conn, do: %Plug.Conn{}
end
`)
	// ConnCase uses ExUnit.CaseTemplate, delegates to using_block/1
	indexFile(t, server.store, server.projectRoot, "lib/conn_case.ex", `defmodule MyAppWeb.ConnCase do
  use ExUnit.CaseTemplate

  def using_block(_opts) do
    quote do
      import Phoenix.ConnTest
    end
  end

  using opts do
    using_block(opts)
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyTest do
  use MyAppWeb.ConnCase

  def test do
    build_conn()
  end
end`)

	// col=4 is on 'b' of "build_conn"
	hover := hoverAt(t, server, uri, 4, 4)
	if hover == nil {
		t.Fatal("expected hover for build_conn injected via CaseTemplate using_block delegation")
	}
	if !strings.Contains(hover.Contents.Value, "build_conn") {
		t.Errorf("expected build_conn in hover, got %q", hover.Contents.Value)
	}
}

func TestHover_UseWithOptOverride(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Remote.Mox pattern: `import unquote(mod)` where mod comes from opts
	indexFile(t, server.store, server.projectRoot, "lib/mox.ex", `defmodule Remote.Mox do
  defmacro __using__(opts \\ []) do
    mod = Keyword.get(opts, :mod, Mox)
    quote do
      import unquote(mod)
    end
  end
end
`)
	// Mox defines `expect`
	indexFile(t, server.store, server.projectRoot, "lib/mox_lib.ex", `defmodule Mox do
  @doc "Sets up an expectation on a mock."
  def expect(mock, name, n \\ 1, fun), do: :ok
end
`)
	// Hammox also defines `expect` (with different doc)
	indexFile(t, server.store, server.projectRoot, "lib/hammox_lib.ex", `defmodule Hammox do
  @doc "Sets up a type-checked expectation."
  def expect(mock, name, n \\ 1, fun), do: :ok
end
`)

	// Without opts: uses default Mox
	uri1 := "file:///test1.ex"
	server.docs.Set(uri1, `defmodule MyTest do
  use Remote.Mox

  def run do
    expect(MyMock, :foo, fn -> :ok end)
  end
end`)
	hover1 := hoverAt(t, server, uri1, 4, 4)
	if hover1 == nil {
		t.Fatal("expected hover for expect (default Mox)")
	}
	if !strings.Contains(hover1.Contents.Value, "Sets up an expectation") {
		t.Errorf("expected Mox doc, got %q", hover1.Contents.Value)
	}

	// With `mod: Hammox`: should use Hammox instead
	uri2 := "file:///test2.ex"
	server.docs.Set(uri2, `defmodule MyTest do
  use Remote.Mox, mod: Hammox

  def run do
    expect(MyMock, :foo, fn -> :ok end)
  end
end`)
	hover2 := hoverAt(t, server, uri2, 4, 4)
	if hover2 == nil {
		t.Fatal("expected hover for expect (Hammox override)")
	}
	if !strings.Contains(hover2.Contents.Value, "type-checked") {
		t.Errorf("expected Hammox doc, got %q", hover2.Contents.Value)
	}
}

func TestHover_DocSinceBeforeDef(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// Mirrors Oban.Pro.Worker's exact structure: __using__ body with inline defs
	// and @doc false, then @doc heredoc + @doc since: before the target defmacro
	indexFile(t, server.store, server.projectRoot, "lib/pro_worker.ex", `defmodule Oban.Pro.Worker do
  defmacro __using__(_opts) do
    quote do
      import Oban.Pro.Worker, only: [args_schema: 1]

      @doc false
      def __verify_stages__(module), do: module.__stages__()

      @doc false
      def __stages__ do
        :ok
      end

      def __opts__, do: []

      def new(args, opts \\ []), do: :ok

      def backoff(job), do: :ok

      def timeout(job), do: :ok

      def perform(job), do: :ok

      def fetch_recorded(job), do: :ok

      defoverridable backoff: 1, new: 2, perform: 1, timeout: 1
    end
  end

  @doc """
  Define an args schema struct with field definitions.

  ## Example

      defmodule MyApp.Worker do
        use Oban.Pro.Worker

        args_schema do
          field :id, :id, required: true
        end
      end
  """
  @doc since: "0.14.0"
  defmacro args_schema(do: _block) do
    quote do: :ok
  end
end
`)

	uri := "file:///test.ex"
	server.docs.Set(uri, `defmodule MyApp.Worker do
  use Oban.Pro.Worker

  args_schema do
    :ok
  end
end`)

	hover := hoverAt(t, server, uri, 3, 2)
	if hover == nil {
		t.Fatal("expected hover for args_schema with @doc since:")
	}
	if !strings.Contains(hover.Contents.Value, "Define an args schema struct") {
		t.Errorf("expected doc content before @doc since:, got %q", hover.Contents.Value)
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

func TestHover_ModuleKeyword(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	src := `defmodule MyApp.Accounts do
  @moduledoc "Manages user accounts."

  alias __MODULE__.User

  def get_user(id), do: {:ok, id}
end`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", src)
	uri := "file:///test.ex"
	server.docs.Set(uri, src)

	// col=9 is on '__MODULE__' in the alias line (line 3)
	hover := hoverAt(t, server, uri, 3, 9)
	if hover == nil {
		t.Fatal("expected hover for __MODULE__")
	}
	if !strings.Contains(hover.Contents.Value, "MyApp.Accounts") {
		t.Errorf("expected module name in hover, got %q", hover.Contents.Value)
	}
	if !strings.Contains(hover.Contents.Value, "Manages user accounts") {
		t.Errorf("expected moduledoc in hover, got %q", hover.Contents.Value)
	}
}

func TestHover_ModuleKeywordSubmodule(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts/user.ex", `defmodule MyApp.Accounts.User do
  @moduledoc "Represents a user."
  def new, do: %{}
end`)

	src := `defmodule MyApp.Accounts do
  alias __MODULE__.User

  def get_user(id), do: User.new()
end`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", src)
	uri := "file:///test.ex"
	server.docs.Set(uri, src)

	// col=9 is on 'User' in alias __MODULE__.User (line 1)
	hover := hoverAt(t, server, uri, 1, 20)
	if hover == nil {
		t.Fatal("expected hover for __MODULE__.User")
	}
	if !strings.Contains(hover.Contents.Value, "MyApp.Accounts.User") {
		t.Errorf("expected submodule in hover, got %q", hover.Contents.Value)
	}
}

func TestHover_QualifiedCallViaUseChain(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// SharedLib.Storage defines __using__ that imports SharedLib.Queryable
	storageSrc := `defmodule SharedLib.Storage do
  defmacro __using__(_opts) do
    quote do
      import SharedLib.Queryable
    end
  end
end`
	queryableSrc := `defmodule SharedLib.Queryable do
  @doc "Fetches all records matching the query."
  def all(queryable), do: queryable
end`

	// MyApp.Repo uses SharedLib.Storage, so all/1 is injected
	repoSrc := `defmodule MyApp.Repo do
  use SharedLib.Storage
end`

	callerSrc := `defmodule MyApp.Accounts do
  alias MyApp.Repo

  def list do
    Repo.all(User)
  end
end`

	indexFile(t, server.store, server.projectRoot, "lib/storage.ex", storageSrc)
	storageURI := "file://" + filepath.Join(server.projectRoot, "lib/storage.ex")
	server.docs.Set(storageURI, storageSrc)

	indexFile(t, server.store, server.projectRoot, "lib/queryable.ex", queryableSrc)

	indexFile(t, server.store, server.projectRoot, "lib/repo.ex", repoSrc)
	repoURI := "file://" + filepath.Join(server.projectRoot, "lib/repo.ex")
	server.docs.Set(repoURI, repoSrc)

	callerURI := "file://" + filepath.Join(server.projectRoot, "lib/accounts.ex")
	server.docs.Set(callerURI, callerSrc)

	// line 4 (0-indexed): `    Repo.all(User)` — col 9 is on `all`
	hover := hoverAt(t, server, callerURI, 4, 9)
	if hover == nil {
		t.Fatal("expected hover result for Repo.all resolved via use-chain, got nil")
	}
	if !strings.Contains(hover.Contents.Value, "Fetches all records") {
		t.Errorf("expected doc from use-chain source, got %q", hover.Contents.Value)
	}
}

func TestHover_QualifiedCallViaUseChain_CallbackDoc(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	// SharedLib.Storage defines @callback with @doc and injects bare def via __using__.
	// The @callback for get spans multiple lines to test multi-line extraction.
	storageSrc := `defmodule SharedLib.Storage do
  @doc """
  Fetches all records from the data store.
  """
  @callback all(queryable :: term) :: [term]

  @doc """
  Fetches a single record by its ID.
  """
  @callback get(queryable :: term, id :: term) ::
              term | nil

  defmacro __using__(_opts) do
    quote do
      @behaviour SharedLib.Storage

      def all(queryable), do: queryable
      def get(queryable, id), do: nil
    end
  end
end`

	repoSrc := `defmodule MyApp.Repo do
  use SharedLib.Storage
end`

	callerSrc := `defmodule MyApp.Accounts do
  alias MyApp.Repo

  def list do
    Repo.all(User)
  end

  def find(id) do
    Repo.get(User, id)
  end
end`

	indexFile(t, server.store, server.projectRoot, "lib/storage.ex", storageSrc)
	storageURI := "file://" + filepath.Join(server.projectRoot, "lib/storage.ex")
	server.docs.Set(storageURI, storageSrc)

	indexFile(t, server.store, server.projectRoot, "lib/repo.ex", repoSrc)
	repoURI := "file://" + filepath.Join(server.projectRoot, "lib/repo.ex")
	server.docs.Set(repoURI, repoSrc)

	callerURI := "file://" + filepath.Join(server.projectRoot, "lib/accounts.ex")
	server.docs.Set(callerURI, callerSrc)

	// line 4 (0-indexed): `    Repo.all(User)` — col 9 is on `all`
	hover := hoverAt(t, server, callerURI, 4, 9)
	if hover == nil {
		t.Fatal("expected hover result for Repo.all via callback doc, got nil")
	}
	if !strings.Contains(hover.Contents.Value, "Fetches all records") {
		t.Errorf("expected callback doc for all, got %q", hover.Contents.Value)
	}

	// line 8 (0-indexed): `    Repo.get(User, id)` — col 9 is on `get`
	hover = hoverAt(t, server, callerURI, 8, 9)
	if hover == nil {
		t.Fatal("expected hover result for Repo.get via callback doc, got nil")
	}
	if !strings.Contains(hover.Contents.Value, "Fetches a single record") {
		t.Errorf("expected callback doc for get, got %q", hover.Contents.Value)
	}
	if !strings.Contains(hover.Contents.Value, "term | nil") {
		t.Errorf("expected full multi-line callback spec for get, got %q", hover.Contents.Value)
	}
}
