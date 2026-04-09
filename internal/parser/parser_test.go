package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.ex")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseFile_SingleModule(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Handlers.Foo do
  def bar(arg) do
    :ok
  end

  defp baz do
    :secret
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(defs) != 3 {
		t.Fatalf("expected 3 definitions, got %d", len(defs))
	}

	// Module
	if defs[0].Module != "MyApp.Handlers.Foo" || defs[0].Kind != "module" || defs[0].Line != 1 {
		t.Errorf("unexpected module def: %+v", defs[0])
	}

	// Public function
	if defs[1].Module != "MyApp.Handlers.Foo" || defs[1].Function != "bar" || defs[1].Kind != "def" || defs[1].Line != 2 {
		t.Errorf("unexpected def: %+v", defs[1])
	}

	// Private function
	if defs[2].Module != "MyApp.Handlers.Foo" || defs[2].Function != "baz" || defs[2].Kind != "defp" || defs[2].Line != 6 {
		t.Errorf("unexpected defp: %+v", defs[2])
	}
}

func TestParseFile_MultipleFunctionHeads(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Webhooks do
  def process_event("completed", payload) do
    :ok
  end

  def process_event("declined", payload) do
    :declined
  end

  def process_event(_, _) do
    :unknown
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcDefs := 0
	for _, d := range defs {
		if d.Function == "process_event" {
			funcDefs++
			if d.Module != "MyApp.Webhooks" || d.Kind != "def" {
				t.Errorf("unexpected process_event def: %+v", d)
			}
		}
	}
	if funcDefs != 3 {
		t.Errorf("expected 3 process_event heads, got %d", funcDefs)
	}
}

func TestParseFile_NestedModules(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Outer do
  def outer_func do
    :ok
  end

  defmodule MyApp.Outer.Inner do
    def inner_func do
      :ok
    end
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	modules := map[string]bool{}
	for _, d := range defs {
		if d.Kind == "module" {
			modules[d.Module] = true
		}
	}

	if !modules["MyApp.Outer"] {
		t.Error("missing MyApp.Outer module")
	}
	if !modules["MyApp.Outer.Inner"] {
		t.Error("missing MyApp.Outer.Inner module")
	}

	// inner_func should belong to MyApp.Outer.Inner
	for _, d := range defs {
		if d.Function == "inner_func" && d.Module != "MyApp.Outer.Inner" {
			t.Errorf("inner_func should belong to MyApp.Outer.Inner, got %s", d.Module)
		}
	}
}

func TestParseFile_Macros(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Macros do
  defmacro my_macro(arg) do
    quote do: unquote(arg)
  end

  defmacrop private_macro do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[string]string{}
	for _, d := range defs {
		if d.Function != "" {
			kinds[d.Function] = d.Kind
		}
	}

	if kinds["my_macro"] != "defmacro" {
		t.Errorf("expected defmacro for my_macro, got %s", kinds["my_macro"])
	}
	if kinds["private_macro"] != "defmacrop" {
		t.Errorf("expected defmacrop for private_macro, got %s", kinds["private_macro"])
	}
}

func TestParseFile_FunctionWithQuestionMark(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Guards do
  def valid?(thing) do
    true
  end

  def process!(thing) do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]bool{}
	for _, d := range defs {
		if d.Function != "" {
			funcs[d.Function] = true
		}
	}

	if !funcs["valid?"] {
		t.Error("missing valid? function")
	}
	if !funcs["process!"] {
		t.Error("missing process! function")
	}
}

func TestParseFile_HeredocDefmoduleIgnored(t *testing.T) {
	path := writeTempFile(t, `defmodule Tesla do
  @moduledoc """
  Example:

      defmodule MyApi do
        def new(opts) do
          Tesla.client(middleware, adapter)
        end
      end
  """

  def client(middleware, adapter \\ nil), do: build(middleware, adapter)

  defp build(middleware, adapter) do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Should only have Tesla module, not MyApi
	modules := map[string]bool{}
	for _, d := range defs {
		if d.Kind == "module" {
			modules[d.Module] = true
		}
	}
	if !modules["Tesla"] {
		t.Error("missing Tesla module")
	}
	if modules["MyApi"] {
		t.Error("MyApi from heredoc should not be indexed")
	}

	// client should belong to Tesla
	found := false
	for _, d := range defs {
		if d.Function == "client" {
			found = true
			if d.Module != "Tesla" {
				t.Errorf("client should belong to Tesla, got %s", d.Module)
			}
		}
	}
	if !found {
		t.Error("missing client function")
	}

	// build should belong to Tesla too
	for _, d := range defs {
		if d.Function == "build" && d.Module != "Tesla" {
			t.Errorf("build should belong to Tesla, got %s", d.Module)
		}
	}
}

func TestParseFile_SigillHeredocIgnored(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Docs do
  @doc ~S"""
  Usage:

      defmodule Example do
        def example_func do
          :ok
        end
      end
  """
  def real_func do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	modules := map[string]bool{}
	funcs := map[string]string{}
	for _, d := range defs {
		if d.Kind == "module" {
			modules[d.Module] = true
		}
		if d.Function != "" {
			funcs[d.Function] = d.Module
		}
	}

	if modules["Example"] {
		t.Error("Example from sigil heredoc should not be indexed")
	}
	if funcs["example_func"] != "" {
		t.Error("example_func from sigil heredoc should not be indexed")
	}
	if funcs["real_func"] != "MyApp.Docs" {
		t.Errorf("real_func should belong to MyApp.Docs, got %s", funcs["real_func"])
	}
}

func TestParseFile_ModuleNestingRestoresAfterEnd(t *testing.T) {
	// After an inner module's `end`, functions should belong to the outer module
	path := writeTempFile(t, `defmodule MyApp.Outer do
  defmodule MyApp.Outer.Inner do
    def inner_func do
      :ok
    end
  end

  def outer_func do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]string{}
	for _, d := range defs {
		if d.Function != "" {
			funcs[d.Function] = d.Module
		}
	}

	if funcs["inner_func"] != "MyApp.Outer.Inner" {
		t.Errorf("inner_func should belong to MyApp.Outer.Inner, got %s", funcs["inner_func"])
	}
	if funcs["outer_func"] != "MyApp.Outer" {
		t.Errorf("outer_func should belong to MyApp.Outer, got %s", funcs["outer_func"])
	}
}

func TestParseFile_SingleLineDefWithDefaultArg(t *testing.T) {
	path := writeTempFile(t, `defmodule Tesla do
  def client(middleware, adapter \\ nil), do: build(middleware, adapter)
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, d := range defs {
		if d.Function == "client" {
			found = true
			if d.Module != "Tesla" {
				t.Errorf("client should belong to Tesla, got %s", d.Module)
			}
			if d.Line != 2 {
				t.Errorf("client should be on line 2, got %d", d.Line)
			}
		}
	}
	if !found {
		t.Error("missing client function")
	}
}

func TestParseFile_InlineModuledoc(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Simple do
  @moduledoc "A simple module"

  def hello do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, d := range defs {
		if d.Function == "hello" && d.Module == "MyApp.Simple" {
			found = true
		}
	}
	if !found {
		t.Error("missing hello function in MyApp.Simple")
	}
}

func TestParseFile_Defdelegate(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Accounts do
  defdelegate fetch(id), to: MyApp.Repo
  defdelegate create(attrs), to: MyApp.Accounts.Create
  defdelegate update(user, attrs), to: MyApp.Accounts.Update
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]bool{}
	for _, d := range defs {
		if d.Kind == "defdelegate" {
			funcs[d.Function] = true
			if d.Module != "MyApp.Accounts" {
				t.Errorf("%s should belong to MyApp.Accounts, got %s", d.Function, d.Module)
			}
		}
	}

	for _, name := range []string{"fetch", "create", "update"} {
		if !funcs[name] {
			t.Errorf("missing defdelegate %s", name)
		}
	}
}

func TestParseFile_DefdelegateTo(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Accounts do
  defdelegate fetch(id), to: MyApp.Repo
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range defs {
		if d.Function == "fetch" {
			if d.DelegateTo != "MyApp.Repo" {
				t.Errorf("expected DelegateTo MyApp.Repo, got %q", d.DelegateTo)
			}
			return
		}
	}
	t.Error("missing defdelegate fetch")
}

func TestParseFile_DefdelegateMultiLine(t *testing.T) {
	path := writeTempFile(t, `defmodule BusinessDomain.ThirdPartyProvider do
  alias BusinessDomain.ThirdPartyProvider.Finders.ListMatches

  defdelegate list_matches(slug, opts),
    to: ListMatches,
    as: :call

  defdelegate create_match(
                open_items,
                slug,
                user_slug
              ),
              to: ListMatches,
              as: :call
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range defs {
		if d.Function == "list_matches" {
			if d.DelegateTo != "BusinessDomain.ThirdPartyProvider.Finders.ListMatches" {
				t.Errorf("list_matches: expected DelegateTo BusinessDomain.ThirdPartyProvider.Finders.ListMatches, got %q", d.DelegateTo)
			}
		}
		if d.Function == "create_match" {
			if d.DelegateTo != "BusinessDomain.ThirdPartyProvider.Finders.ListMatches" {
				t.Errorf("create_match: expected DelegateTo BusinessDomain.ThirdPartyProvider.Finders.ListMatches, got %q", d.DelegateTo)
			}
		}
	}
}

func TestParseFile_DefdelegateAliasAsResolution(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Values.Timesheet do
  alias MyApp.Serializer.Date, as: DateSerializer

  defdelegate format(date), to: DateSerializer
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range defs {
		if d.Function == "format" {
			if d.DelegateTo != "MyApp.Serializer.Date" {
				t.Errorf("expected DelegateTo MyApp.Serializer.Date, got %q", d.DelegateTo)
			}
			return
		}
	}
	t.Error("missing defdelegate format")
}

func TestParseFile_DefdelegateModuleAlias(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.HRIS do
  alias __MODULE__.Services

  defdelegate link_via_team_membership(user_id, company_id),
    to: Services.AssociateWithTeam,
    as: :call
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range defs {
		if d.Function == "link_via_team_membership" {
			if d.DelegateTo != "MyApp.HRIS.Services.AssociateWithTeam" {
				t.Errorf("expected MyApp.HRIS.Services.AssociateWithTeam, got %q", d.DelegateTo)
			}
			return
		}
	}
	t.Error("missing defdelegate link_via_team_membership")
}

func TestParseFile_DefdelegateTo__MODULE__Directly(t *testing.T) {
	path := writeTempFile(t, `defmodule DataUtils.Banks do
  defdelegate account_number, to: __MODULE__, as: :scramble_alphanumeric
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range defs {
		if d.Function == "account_number" {
			if d.DelegateTo != "DataUtils.Banks" {
				t.Errorf("expected DataUtils.Banks, got %q", d.DelegateTo)
			}
			if d.DelegateAs != "scramble_alphanumeric" {
				t.Errorf("expected scramble_alphanumeric, got %q", d.DelegateAs)
			}
			return
		}
	}
	t.Error("missing defdelegate account_number")
}

func TestParseFile_AliasModuleAs(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.MyPayProvider do
  alias __MODULE__, as: MyPayProvider

  defdelegate process(payload), to: MyPayProvider.Processor, as: :call
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range defs {
		if d.Function == "process" {
			if d.DelegateTo != "MyApp.MyPayProvider.Processor" {
				t.Errorf("expected MyApp.MyPayProvider.Processor, got %q", d.DelegateTo)
			}
			return
		}
	}
	t.Error("missing defdelegate process")
}

func TestParseFile_FunctionsAfterNestedModules(t *testing.T) {
	// Functions defined after nested modules (which close with `end`) should
	// still belong to the outer module, not get mis-attributed due to over-popping.
	path := writeTempFile(t, `defmodule Outer.Module do
  defmodule Inner do
    defstruct [:x]
  end

  defmodule OtherInner do
    def helper do
      :ok
    end
  end

  def public_func(x) do
    x + 1
  end

  defp private_func do
    :hidden
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]string{}
	for _, d := range defs {
		if d.Function != "" {
			funcs[d.Function] = d.Module
		}
	}

	if funcs["public_func"] != "Outer.Module" {
		t.Errorf("public_func should belong to Outer.Module, got %q", funcs["public_func"])
	}
	if funcs["private_func"] != "Outer.Module" {
		t.Errorf("private_func should belong to Outer.Module, got %q", funcs["private_func"])
	}
	if funcs["helper"] != "Outer.Module.OtherInner" {
		t.Errorf("helper should belong to Outer.Module.OtherInner, got %q", funcs["helper"])
	}
}

func TestParseFile_MacroAfterManyFunctions(t *testing.T) {
	// Simulates the Ecto.Query pattern: a macro defined after many function bodies
	// whose `end`s would over-pop a naive module stack.
	path := writeTempFile(t, `defmodule EctoLike.Query do
  defmodule SubQuery do
    defstruct [:query]
  end

  def first(query) do
    query
  end

  def last(query) do
    query
  end

  defp build(query) do
    if query do
      :ok
    else
      :error
    end
  end

  defmacro from(expr, kw \\ []) do
    quote do
      unquote(expr)
    end
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]string{}
	for _, d := range defs {
		if d.Function != "" {
			funcs[d.Function] = d.Module
		}
	}

	if funcs["from"] != "EctoLike.Query" {
		t.Errorf("from macro should belong to EctoLike.Query, got %q", funcs["from"])
	}
	if funcs["first"] != "EctoLike.Query" {
		t.Errorf("first should belong to EctoLike.Query, got %q", funcs["first"])
	}
}

func TestParseFile_RelativeNestedModule(t *testing.T) {
	path := writeTempFile(t, `defmodule MyAppWeb.ApiDocs.Payslips do
  defmodule PayslipDownloadResponse do
    def schema do
      :ok
    end
  end

  def index do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	modules := map[string]int{}
	funcs := map[string]string{}
	for _, d := range defs {
		if d.Kind == "module" {
			modules[d.Module] = d.Line
		}
		if d.Function != "" {
			funcs[d.Function] = d.Module
		}
	}

	if _, ok := modules["MyAppWeb.ApiDocs.Payslips"]; !ok {
		t.Error("missing MyAppWeb.ApiDocs.Payslips")
	}
	if _, ok := modules["MyAppWeb.ApiDocs.Payslips.PayslipDownloadResponse"]; !ok {
		t.Error("missing MyAppWeb.ApiDocs.Payslips.PayslipDownloadResponse (relative nested module)")
	}
	if funcs["schema"] != "MyAppWeb.ApiDocs.Payslips.PayslipDownloadResponse" {
		t.Errorf("schema should belong to MyAppWeb.ApiDocs.Payslips.PayslipDownloadResponse, got %q", funcs["schema"])
	}
	if funcs["index"] != "MyAppWeb.ApiDocs.Payslips" {
		t.Errorf("index should belong to MyAppWeb.ApiDocs.Payslips, got %q", funcs["index"])
	}
}

func TestParseFile_Defguard(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Guards do
  defguard is_admin(user) when user.role == :admin
  defguardp is_active(user) when user.status == :active
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[string]string{}
	for _, d := range defs {
		if d.Function != "" {
			kinds[d.Function] = d.Kind
		}
	}

	if kinds["is_admin"] != "defguard" {
		t.Errorf("expected defguard for is_admin, got %q", kinds["is_admin"])
	}
	if kinds["is_active"] != "defguardp" {
		t.Errorf("expected defguardp for is_active, got %q", kinds["is_active"])
	}
}

func TestParseFile_Defprotocol(t *testing.T) {
	path := writeTempFile(t, `defprotocol MyApp.Formatter do
  @doc "Formats a value"
  def format(value)
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	foundProtocol := false
	foundFunc := false
	for _, d := range defs {
		if d.Module == "MyApp.Formatter" && d.Kind == "defprotocol" {
			foundProtocol = true
		}
		if d.Module == "MyApp.Formatter" && d.Function == "format" {
			foundFunc = true
		}
	}

	if !foundProtocol {
		t.Error("missing defprotocol MyApp.Formatter")
	}
	if !foundFunc {
		t.Error("missing def format in MyApp.Formatter")
	}
}

func TestParseFile_Defimpl(t *testing.T) {
	path := writeTempFile(t, `defimpl MyApp.Formatter, for: MyApp.User do
  def format(user) do
    user.name
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	foundImpl := false
	foundFunc := false
	for _, d := range defs {
		if d.Kind == "defimpl" && d.Module == "MyApp.Formatter" {
			foundImpl = true
		}
		if d.Function == "format" {
			foundFunc = true
		}
	}

	if !foundImpl {
		t.Error("missing defimpl MyApp.Formatter")
	}
	if !foundFunc {
		t.Error("missing def format in defimpl")
	}
}

func TestParseFile_Defstruct(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.User do
  defstruct [:name, :email, :role]

  def new(attrs) do
    struct!(__MODULE__, attrs)
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	foundStruct := false
	for _, d := range defs {
		if d.Kind == "defstruct" && d.Module == "MyApp.User" {
			foundStruct = true
		}
	}

	if !foundStruct {
		t.Error("missing defstruct in MyApp.User")
	}
}

func TestParseFile_Defexception(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.NotFoundError do
  defexception message: "not found"
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	foundException := false
	for _, d := range defs {
		if d.Kind == "defexception" && d.Module == "MyApp.NotFoundError" {
			foundException = true
		}
	}

	if !foundException {
		t.Error("missing defexception in MyApp.NotFoundError")
	}
}

func TestParseFile_WhenGuards(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Validators do
  def validate(x) when is_integer(x) and x > 0 do
    :ok
  end

  def validate(x) when is_binary(x) do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	count := 0
	for _, d := range defs {
		if d.Function == "validate" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 validate heads, got %d", count)
	}
}

func TestParseFile_ProtocolFollowedByModule(t *testing.T) {
	path := writeTempFile(t, `defprotocol Enumerable do
  @doc """
  Reduces the enumerable.
  """
  def reduce(enumerable, acc, fun)

  @doc """
  Counts the enumerable.
  """
  def count(enumerable)
end

defmodule Enum do
  def map(enumerable, fun) do
    :ok
  end

  def filter(enumerable, fun) do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	modules := map[string]string{}
	funcs := map[string]string{}
	for _, d := range defs {
		if d.Kind == "defprotocol" || d.Kind == "module" {
			modules[d.Module] = d.Kind
		}
		if d.Function != "" {
			funcs[d.Function] = d.Module
		}
	}

	if modules["Enumerable"] != "defprotocol" {
		t.Error("missing defprotocol Enumerable")
	}
	if modules["Enum"] != "module" {
		t.Errorf("expected Enum as module, got kind %q (modules: %v)", modules["Enum"], modules)
	}
	if funcs["reduce"] != "Enumerable" {
		t.Errorf("reduce should belong to Enumerable, got %q", funcs["reduce"])
	}
	if funcs["map"] != "Enum" {
		t.Errorf("map should belong to Enum, got %q", funcs["map"])
	}
	if funcs["filter"] != "Enum" {
		t.Errorf("filter should belong to Enum, got %q", funcs["filter"])
	}
}

func TestParseFile_StdlibStyleWithManyDocBlocks(t *testing.T) {
	content := "defmodule Enum do\n" +
		"  @moduledoc \"\"\"\n" +
		"  Functions for working with collections (known as enumerables).\n" +
		"\n" +
		"  All the functions in this module are eager.\n" +
		"  \"\"\"\n" +
		"\n" +
		"  @doc \"\"\"\n" +
		"  Returns `true` if all elements in `enumerable` are truthy.\n" +
		"\n" +
		"  ## Examples\n" +
		"\n" +
		"      iex> Enum.all?([1, 2, 3])\n" +
		"      true\n" +
		"\n" +
		"  \"\"\"\n" +
		"  @spec all?(t()) :: boolean()\n" +
		"  def all?(enumerable) when is_list(enumerable) do\n" +
		"    all_list(enumerable)\n" +
		"  end\n" +
		"\n" +
		"  def all?(enumerable) do\n" +
		"    Enumerable.reduce(enumerable, {:cont, true}, fn _, _ -> {:halt, false} end)\n" +
		"  end\n" +
		"\n" +
		"  @doc \"\"\"\n" +
		"  Returns the count of elements.\n" +
		"\n" +
		"  ## Examples\n" +
		"\n" +
		"      iex> Enum.count([1, 2, 3])\n" +
		"      3\n" +
		"\n" +
		"  \"\"\"\n" +
		"  @spec count(t()) :: non_neg_integer()\n" +
		"  def count(enumerable) when is_list(enumerable) do\n" +
		"    length(enumerable)\n" +
		"  end\n" +
		"\n" +
		"  def count(enumerable) do\n" +
		"    case Enumerable.count(enumerable) do\n" +
		"      {:ok, value} -> value\n" +
		"      {:error, _} -> 0\n" +
		"    end\n" +
		"  end\n" +
		"\n" +
		"  @doc \"\"\"\n" +
		"  Filters the enumerable.\n" +
		"\n" +
		"  ## Examples\n" +
		"\n" +
		"      iex> Enum.filter([1, 2, 3], fn x -> rem(x, 2) == 0 end)\n" +
		"      [2]\n" +
		"\n" +
		"  \"\"\"\n" +
		"  @spec filter(t(), (element() -> as_boolean(term()))) :: list()\n" +
		"  def filter(enumerable, fun) when is_list(enumerable) and is_function(fun, 1) do\n" +
		"    filter_list(enumerable, fun)\n" +
		"  end\n" +
		"\n" +
		"  def filter(enumerable, fun) when is_function(fun, 1) do\n" +
		"    :ok\n" +
		"  end\n" +
		"\n" +
		"  @doc \"\"\"\n" +
		"  Maps the given function over enumerable.\n" +
		"\n" +
		"  ## Examples\n" +
		"\n" +
		"      iex> Enum.map([1, 2, 3], fn x -> x * 2 end)\n" +
		"      [2, 4, 6]\n" +
		"\n" +
		"  \"\"\"\n" +
		"  @spec map(t(), (element() -> any())) :: list()\n" +
		"  def map(enumerable, fun) when is_list(enumerable) and is_function(fun, 1) do\n" +
		"    :lists.map(fun, enumerable)\n" +
		"  end\n" +
		"\n" +
		"  def map(enumerable, fun) when is_function(fun, 1) do\n" +
		"    :ok\n" +
		"  end\n" +
		"\n" +
		"  @doc \"\"\"\n" +
		"  Sorts the enumerable according to Erlang's term ordering.\n" +
		"\n" +
		"  ## Examples\n" +
		"\n" +
		"      iex> Enum.sort([3, 2, 1])\n" +
		"      [1, 2, 3]\n" +
		"\n" +
		"  \"\"\"\n" +
		"  @spec sort(t()) :: list()\n" +
		"  def sort(enumerable) when is_list(enumerable) do\n" +
		"    :lists.sort(enumerable)\n" +
		"  end\n" +
		"\n" +
		"  def sort(enumerable) do\n" +
		"    sort(enumerable, &(&1 <= &2))\n" +
		"  end\n" +
		"\n" +
		"  @doc \"\"\"\n" +
		"  Sorts the enumerable by the given function.\n" +
		"  \"\"\"\n" +
		"  @spec sort(t(), (element(), element() -> boolean())) :: list()\n" +
		"  def sort(enumerable, sorter)\n" +
		"\n" +
		"  def sort(enumerable, sorter) when is_list(enumerable) and is_function(sorter, 2) do\n" +
		"    :lists.sort(sorter, enumerable)\n" +
		"  end\n" +
		"\n" +
		"  def sort(enumerable, :asc) when is_list(enumerable), do: :lists.sort(enumerable)\n" +
		"  def sort(enumerable, :desc) when is_list(enumerable), do: :lists.reverse(:lists.sort(enumerable))\n" +
		"\n" +
		"  @doc \"\"\"\n" +
		"  Returns a subset list of the given enumerable by index_range.\n" +
		"\n" +
		"  ## Examples\n" +
		"\n" +
		"      iex> Enum.slice(1..100, 5..10)\n" +
		"      [6, 7, 8, 9, 10, 11]\n" +
		"\n" +
		"  \"\"\"\n" +
		"  @spec slice(t(), Range.t()) :: list()\n" +
		"  def slice(enumerable, first..last//step = index_range) when is_integer(first) do\n" +
		"    slice_range(enumerable, index_range)\n" +
		"  end\n" +
		"\n" +
		"  @doc \"\"\"\n" +
		"  Reduces the enumerable.\n" +
		"  \"\"\"\n" +
		"  @spec reduce(t(), any(), (element(), any() -> any())) :: any()\n" +
		"  def reduce(enumerable, acc, fun) when is_list(enumerable) and is_function(fun, 2) do\n" +
		"    :lists.foldl(fun, acc, enumerable)\n" +
		"  end\n" +
		"\n" +
		"  def reduce(enumerable, acc, fun) when is_function(fun, 2) do\n" +
		"    :ok\n" +
		"  end\n" +
		"\n" +
		"  defp all_list([h | t]) do\n" +
		"    if h, do: all_list(t), else: false\n" +
		"  end\n" +
		"\n" +
		"  defp all_list([]), do: true\n" +
		"\n" +
		"  defp filter_list([h | t], fun) do\n" +
		"    if fun.(h), do: [h | filter_list(t, fun)], else: filter_list(t, fun)\n" +
		"  end\n" +
		"\n" +
		"  defp filter_list([], _fun), do: []\n" +
		"\n" +
		"  defp slice_range(enumerable, range) do\n" +
		"    Enum.to_list(enumerable) |> Enum.slice(range)\n" +
		"  end\n" +
		"end\n"
	path := writeTempFile(t, content)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]bool{}
	privateFuncs := map[string]bool{}
	for _, d := range defs {
		if d.Module == "Enum" && d.Function != "" {
			switch d.Kind {
			case "def":
				funcs[d.Function] = true
			case "defp":
				privateFuncs[d.Function] = true
			}
		}
	}

	for _, name := range []string{"all?", "count", "filter", "map", "sort", "slice", "reduce"} {
		if !funcs[name] {
			t.Errorf("missing public function %q in Enum (found: %v)", name, funcs)
		}
	}

	for _, name := range []string{"all_list", "filter_list", "slice_range"} {
		if !privateFuncs[name] {
			t.Errorf("missing private function %q in Enum", name)
		}
	}

	if funcs["all_list"] || funcs["filter_list"] || funcs["slice_range"] {
		t.Error("private functions should not be in public funcs map")
	}
}

func TestParseFile_InlineDoSyntax(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Math do
  def add(a, b), do: a + b
  defp secret(x), do: x * 2
  def identity(x), do: x
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]string{}
	for _, d := range defs {
		if d.Function != "" {
			funcs[d.Function] = d.Kind
		}
	}

	if funcs["add"] != "def" {
		t.Errorf("expected def for add, got %q", funcs["add"])
	}
	if funcs["secret"] != "defp" {
		t.Errorf("expected defp for secret, got %q", funcs["secret"])
	}
	if funcs["identity"] != "def" {
		t.Errorf("expected def for identity, got %q", funcs["identity"])
	}
}

func TestParseFile_FnEndBlockDoesNotPopModule(t *testing.T) {
	path := writeTempFile(t, `defmodule Enum do
  def min_max(enumerable, empty_fallback) do
    reduce_fun = fn entry, [min | max] = acc ->
      cond do
        entry < min -> [entry | max]
        entry > max -> [min | entry]
        true -> acc
      end
    end

    first_fun = fn entry ->
      [entry | entry]
    end

    result = reduce(enumerable, :first, reduce_fun, first_fun)
    result
  end

  def sort(enumerable) when is_list(enumerable) do
    :lists.sort(enumerable)
  end

  def sort(enumerable) do
    sort(enumerable, &(&1 <= &2))
  end

  def zip(enumerables) do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]string{}
	for _, d := range defs {
		if d.Function != "" {
			funcs[d.Function] = d.Module
		}
	}

	for _, name := range []string{"min_max", "sort", "zip"} {
		if funcs[name] != "Enum" {
			t.Errorf("%s should belong to Enum, got %q (all funcs: %v)", name, funcs[name], funcs)
		}
	}
}

func TestParseFile_FnEndMisindentedDoesNotPopModule(t *testing.T) {
	// Regression: when fn...end is misformatted so the end aligns with defmodule,
	// indent-matching would incorrectly pop the module. Depth-counting handles this.
	path := writeTempFile(t, `defmodule MyModule do
  def build(items) do
    handler = fn item ->
      item
end
  end

  def next(x) do
    x + 1
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]string{}
	for _, d := range defs {
		if d.Function != "" {
			funcs[d.Function] = d.Module
		}
	}

	for _, name := range []string{"build", "next"} {
		if funcs[name] != "MyModule" {
			t.Errorf("%s should belong to MyModule, got %q (all funcs: %v)", name, funcs[name], funcs)
		}
	}
}

func TestParseFile_TrailingFnDoesNotPopModule(t *testing.T) {
	// Regression: "handler = fn" at end of line was not detected as a block
	// opener because ContainsFn required a space after "fn".
	path := writeTempFile(t, `defmodule MyModule do
  def build do
    handler = fn
      :ok -> true
      :error -> false
    end

    handler
  end

  def next(x) do
    x + 1
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]string{}
	for _, d := range defs {
		if d.Function != "" {
			funcs[d.Function] = d.Module
		}
	}

	for _, name := range []string{"build", "next"} {
		if funcs[name] != "MyModule" {
			t.Errorf("%s should belong to MyModule, got %q (all funcs: %v)", name, funcs[name], funcs)
		}
	}
}

func TestParseFile_FnEndWithTrailingParen(t *testing.T) {
	path := writeTempFile(t, `defmodule MyModule do
  def process(enumerable) do
    Enumerable.reduce(enumerable, {:cont, []}, fn entry, acc ->
      {:cont, [entry | acc]}
    end)
  end

  def next_func(x) do
    x + 1
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	funcs := map[string]string{}
	for _, d := range defs {
		if d.Function != "" {
			funcs[d.Function] = d.Module
		}
	}

	if funcs["process"] != "MyModule" {
		t.Errorf("process should belong to MyModule, got %q", funcs["process"])
	}
	if funcs["next_func"] != "MyModule" {
		t.Errorf("next_func should belong to MyModule, got %q", funcs["next_func"])
	}
}

func TestExtractArity(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		funcName string
		expected int
	}{
		{"no parens", "  def foo, do: :ok", "foo", 0},
		{"empty parens", "  def foo(), do: :ok", "foo", 0},
		{"one arg", "  def foo(a), do: :ok", "foo", 1},
		{"two args", "  def foo(a, b) do", "foo", 2},
		{"three args", "  def create(name, email, role) do", "create", 3},
		{"nested map", "  def foo(%{name: name}, opts) do", "foo", 2},
		{"nested list", "  def foo([head | tail], acc) do", "foo", 2},
		{"default arg", "  defmacro from(expr, kw \\\\ []) do", "from", 2},
		{"defguard", "  defguard is_admin(user) when user.role == :admin", "is_admin", 1},
		{"defdelegate", "  defdelegate create(attrs), to: Create", "create", 1},
		{"tuple arg", "  def foo({a, b}, c) do", "foo", 2},
		{"keyword list", "  def foo(a, opts \\\\ [key: :val]) do", "foo", 2},
		{"pattern match", "  def handle_call(:get, _from, state) do", "handle_call", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractArity(tt.line, tt.funcName)
			if got != tt.expected {
				t.Errorf("ExtractArity(%q, %q) = %d, want %d", tt.line, tt.funcName, got, tt.expected)
			}
		})
	}
}

func TestCountDefaultParams(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		funcName string
		expected int
	}{
		{"no defaults", "  def foo(a, b) do", "foo", 0},
		{"one default", `  def foo(a, b \\ []) do`, "foo", 1},
		{"two defaults", `  def foo(a, b \\ nil, c \\ []) do`, "foo", 2},
		{"no parens", "  def foo, do: :ok", "foo", 0},
		{"empty parens", "  def foo() do", "foo", 0},
		{"default with keyword list", `  def foo(a, opts \\ [key: :val]) do`, "foo", 1},
		{"default in nested not counted", `  def foo(%{a: b \\ c}, d) do`, "foo", 0},
		{"defmacro default", `  defmacro from(expr, kw \\ []) do`, "from", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CountDefaultParams(tt.line, tt.funcName)
			if got != tt.expected {
				t.Errorf("CountDefaultParams(%q, %q) = %d, want %d", tt.line, tt.funcName, got, tt.expected)
			}
		})
	}
}

func TestExtractParamNames(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		funcName string
		expected []string
	}{
		{"simple params", "  def create(name, email) do", "create", []string{"name", "email"}},
		{"default param", `  def fetch(slug, opts \\ []) do`, "fetch", []string{"slug", "opts"}},
		{"pattern match map", "  def process(%{name: name}, data) do", "process", []string{"arg1", "data"}},
		{"no params", "  def run do", "run", nil},
		{"empty parens", "  def run() do", "run", nil},
		{"single param", "  def get(id) do", "get", []string{"id"}},
		{"underscore param", "  def handle(_ignored, state) do", "handle", []string{"_ignored", "state"}},
		{"struct = var", "  def update(%User{} = user, attrs) do", "update", []string{"user", "attrs"}},
		{"var = struct", "  def update(user = %User{}, attrs) do", "update", []string{"user", "attrs"}},
		{"tuple pattern", "  def handle_info({:DOWN, _ref, :process, pid, _reason}, state) do", "handle_info", []string{"arg1", "state"}},
		{"bare underscore", "  def foo(_, b) do", "foo", []string{"arg1", "b"}},
		{"atom literal", "  def handle_call(:get, _from, state) do", "handle_call", []string{"arg1", "_from", "state"}},
		{"list pattern = var", "  def process([_ | _] = list, opts) do", "process", []string{"list", "opts"}},
		{"binary pattern", "  def parse(<<header::16, rest::binary>>) do", "parse", []string{"arg1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractParamNames(tt.line, tt.funcName)
			if len(got) != len(tt.expected) {
				t.Fatalf("ExtractParamNames(%q, %q) = %v, want %v", tt.line, tt.funcName, got, tt.expected)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("param %d: got %q, want %q", i, got[i], tt.expected[i])
				}
			}
		})
	}
}

func TestParseFile_DefaultParamExpansion(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Companies do
  def fetch_company_by_slug(slug, opts \\ []) do
    :ok
  end

  def no_defaults(a, b) do
    :ok
  end

  def multi_default(a, b \\ nil, c \\ []) do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Collect all (function, arity) pairs
	type funcArity struct {
		name   string
		arity  int
		params string
	}
	var funcArities []funcArity
	for _, d := range defs {
		if d.Function != "" {
			funcArities = append(funcArities, funcArity{d.Function, d.Arity, d.Params})
		}
	}

	// fetch_company_by_slug should have arity 1 and 2
	found := map[string]bool{}
	paramsByKey := map[string]string{}
	for _, fa := range funcArities {
		key := fa.name + "/" + fmt.Sprintf("%d", fa.arity)
		found[key] = true
		paramsByKey[key] = fa.params
	}

	for _, expected := range []string{
		"fetch_company_by_slug/1",
		"fetch_company_by_slug/2",
		"no_defaults/2",
		"multi_default/1",
		"multi_default/2",
		"multi_default/3",
	} {
		if !found[expected] {
			t.Errorf("expected definition %s not found; got %v", expected, funcArities)
		}
	}

	// no_defaults should NOT have arity 1
	if found["no_defaults/1"] {
		t.Errorf("no_defaults/1 should not exist")
	}

	// Check params are correctly extracted
	if params := paramsByKey["fetch_company_by_slug/1"]; params != "slug" {
		t.Errorf("fetch_company_by_slug/1 params: got %q, want %q", params, "slug")
	}
	if params := paramsByKey["fetch_company_by_slug/2"]; params != "slug,opts" {
		t.Errorf("fetch_company_by_slug/2 params: got %q, want %q", params, "slug,opts")
	}
	if params := paramsByKey["multi_default/1"]; params != "a" {
		t.Errorf("multi_default/1 params: got %q, want %q", params, "a")
	}
	if params := paramsByKey["multi_default/3"]; params != "a,b,c" {
		t.Errorf("multi_default/3 params: got %q, want %q", params, "a,b,c")
	}
}

func TestParseFile_Arity(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Accounts do
  def create(attrs) do
    :ok
  end

  def list(opts, page) do
    :ok
  end

  def all do
    :ok
  end

  defp validate(changeset, rules) do
    :ok
  end

  defdelegate fetch(id), to: MyApp.Repo
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	arities := map[string]int{}
	for _, d := range defs {
		if d.Function != "" {
			arities[d.Function] = d.Arity
		}
	}

	if arities["create"] != 1 {
		t.Errorf("create: expected arity 1, got %d", arities["create"])
	}
	if arities["list"] != 2 {
		t.Errorf("list: expected arity 2, got %d", arities["list"])
	}
	if arities["all"] != 0 {
		t.Errorf("all: expected arity 0, got %d", arities["all"])
	}
	if arities["validate"] != 2 {
		t.Errorf("validate: expected arity 2, got %d", arities["validate"])
	}
	if arities["fetch"] != 1 {
		t.Errorf("fetch: expected arity 1, got %d", arities["fetch"])
	}
}

func TestParseFile_TypeDefinitions(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.User do
  @type t() :: %__MODULE__{}
  @type id :: pos_integer()
  @typep internal(a, b) :: {a, b}
  @opaque token :: String.t()

  def create(attrs) do
    :ok
  end
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	byFunc := map[string]Definition{}
	for _, d := range defs {
		if d.Function != "" {
			byFunc[d.Function] = d
		}
	}

	cases := []struct {
		name string
		kind string
		line int
	}{
		{"t", "type", 2},
		{"id", "type", 3},
		{"token", "opaque", 5},
	}

	for _, tc := range cases {
		d, ok := byFunc[tc.name]
		if !ok {
			t.Errorf("expected %q to be indexed", tc.name)
			continue
		}
		if d.Kind != tc.kind {
			t.Errorf("%q: got kind %q, want %q", tc.name, d.Kind, tc.kind)
		}
		if d.Line != tc.line {
			t.Errorf("%q: got line %d, want %d", tc.name, d.Line, tc.line)
		}
		if d.Module != "MyApp.User" {
			t.Errorf("%q: got module %q, want MyApp.User", tc.name, d.Module)
		}
	}

	// @typep is not indexed
	if _, ok := byFunc["internal"]; ok {
		t.Error("@typep 'internal' should not be indexed in the DB")
	}
	// Non-parameterized type arity
	if byFunc["t"].Arity != 0 {
		t.Errorf("t: expected arity 0, got %d", byFunc["t"].Arity)
	}
}

func TestParseFile_TrailingWhitespaceOnEnd(t *testing.T) {
	// "end" with trailing whitespace must still pop the module stack,
	// otherwise the next module's definitions get lost.
	path := writeTempFile(t, "defmodule MyApp.Foxtrot do\n  def alpha do\n    :ok\n  end\nend   \n\ndefmodule MyApp.Sierra do\n  def beta do\n    :ok\n  end\nend\n")
	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var modules []string
	for _, d := range defs {
		if d.Kind == "module" {
			modules = append(modules, d.Module)
		}
	}
	if len(modules) != 2 || modules[0] != "MyApp.Foxtrot" || modules[1] != "MyApp.Sierra" {
		t.Errorf("expected [MyApp.Foxtrot, MyApp.Sierra], got %v", modules)
	}

	found := false
	for _, d := range defs {
		if d.Module == "MyApp.Sierra" && d.Function == "beta" {
			found = true
		}
	}
	if !found {
		t.Error("expected to find MyApp.Sierra.beta")
	}
}

func TestParseFile_ExtraWhitespaceBeforeDo(t *testing.T) {
	path := writeTempFile(t, "defmodule  MyApp.Echo  do\n  def run do\n    :ok\n  end\nend\n")
	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(defs))
	}
	if defs[0].Module != "MyApp.Echo" || defs[0].Kind != "module" {
		t.Errorf("unexpected module def: %+v", defs[0])
	}
}

func TestParseFile_TabSeparatedKeywords(t *testing.T) {
	path := writeTempFile(t, "defmodule\tMyApp.Tango\tdo\n\tdef\tmeow(arg) do\n\t\t:ok\n\tend\n\t@type\tname :: String.t()\nend\n")
	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var moduleFound, funcFound, typeFound bool
	for _, d := range defs {
		switch {
		case d.Kind == "module" && d.Module == "MyApp.Tango":
			moduleFound = true
		case d.Function == "meow" && d.Kind == "def":
			funcFound = true
		case d.Function == "name" && d.Kind == "type":
			typeFound = true
		}
	}
	if !moduleFound {
		t.Error("expected to find module MyApp.Tango")
	}
	if !funcFound {
		t.Error("expected to find def meow")
	}
	if !typeFound {
		t.Error("expected to find @type name")
	}
}

func TestParseFile_TypesNotIndexedOutsideModule(t *testing.T) {
	// @type at the top level (outside a defmodule) should not be indexed.
	path := writeTempFile(t, `@type orphan :: integer()
`)
	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		if d.Function == "orphan" {
			t.Error("should not index @type outside of a defmodule")
		}
	}
}

func TestParseFileReferences_ModuleFunctionCalls(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Accounts do
  alias MyApp.Repo
  import Ecto.Query

  def list do
    Repo.all(MyApp.User)
  end

  def get(id) do
    Repo.get(MyApp.User, id)
  end
end
`)

	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Expect: alias MyApp.Repo, import Ecto.Query, Repo.all, MyApp.User (x2), Repo.get
	callRefs := filterRefs(refs, "call")
	aliasRefs := filterRefs(refs, "alias")
	importRefs := filterRefs(refs, "import")

	if len(aliasRefs) != 1 {
		t.Errorf("expected 1 alias ref, got %d", len(aliasRefs))
	}
	if len(importRefs) != 1 {
		t.Errorf("expected 1 import ref, got %d", len(importRefs))
	}

	// Repo.all and Repo.get should resolve via alias to MyApp.Repo
	var repoCallCount int
	for _, r := range callRefs {
		if r.Module == "MyApp.Repo" {
			repoCallCount++
		}
	}
	if repoCallCount != 2 {
		t.Errorf("expected 2 MyApp.Repo calls (via alias), got %d", repoCallCount)
	}
}

func TestParseFileReferences_AliasResolution(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Controller do
  alias MyApp.Accounts, as: Acc

  def index do
    Acc.list_users()
  end
end
`)

	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(refs, "call")
	found := false
	for _, r := range callRefs {
		if r.Module == "MyApp.Accounts" && r.Function == "list_users" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Acc.list_users to resolve to MyApp.Accounts.list_users; refs: %+v", callRefs)
	}
}

func TestParseFileReferences_SkipsHeredocs(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Foo do
  @doc """
  Calls Other.Module.thing() in documentation
  """
  def bar do
    Real.Module.call()
  end
end
`)

	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(refs, "call")
	for _, r := range callRefs {
		if r.Module == "Other.Module" {
			t.Error("should not extract references from inside heredocs")
		}
	}

	found := false
	for _, r := range callRefs {
		if r.Module == "Real.Module" && r.Function == "call" {
			found = true
		}
	}
	if !found {
		t.Error("expected Real.Module.call reference")
	}
}

func TestParseFileReferences_SkipsCommentsAndStrings(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Foo do
  def bar do
    # This is a comment: Fake.Module.call()
    x = "a string with Fake.Module.call() inside"
    Real.Module.call()
  end
end
`)

	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	callRefs := filterRefs(refs, "call")
	for _, r := range callRefs {
		if r.Module == "Fake.Module" {
			t.Errorf("should not extract references from comments or strings, got %+v", r)
		}
	}

	found := false
	for _, r := range callRefs {
		if r.Module == "Real.Module" && r.Function == "call" {
			found = true
		}
	}
	if !found {
		t.Error("expected Real.Module.call reference")
	}
}

func TestParseFileReferences_UseDeclaration(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Worker do
  use Oban.Worker

  def perform(job) do
    :ok
  end
end
`)

	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	useRefs := filterRefs(refs, "use")
	if len(useRefs) != 1 {
		t.Fatalf("expected 1 use ref, got %d", len(useRefs))
	}
	if useRefs[0].Module != "Oban.Worker" {
		t.Errorf("expected Oban.Worker, got %q", useRefs[0].Module)
	}
}

func TestParseFileReferences_BareMacroCalls(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Schema do
  use MyApp.EctoSchema

  embedded_schema do
    field :name, :string
  end
end

defmodule MyApp.OtherSchema do
  use MyApp.EctoSchema

  schema "other_things" do
    field :value, :integer
  end
end
`)

	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// embedded_schema and schema should be attributed to MyApp.EctoSchema (the use'd module)
	var foundEmbedded, foundSchema bool
	for _, r := range refs {
		if r.Kind == "call" && r.Module == "MyApp.EctoSchema" {
			if r.Function == "embedded_schema" {
				foundEmbedded = true
			}
			if r.Function == "schema" {
				foundSchema = true
			}
		}
	}
	if !foundEmbedded {
		t.Error("expected embedded_schema attributed to MyApp.EctoSchema")
	}
	if !foundSchema {
		t.Error("expected schema attributed to MyApp.EctoSchema")
	}

	// Elixir keywords must not be captured as bare macro calls
	for _, r := range refs {
		if r.Kind == "call" && r.Module != "" && elixirKeyword[r.Function] {
			t.Errorf("should not capture Elixir keyword %q as bare macro call", r.Function)
		}
	}
}

func TestParseFileReferences_BareMacroNotWithoutInjector(t *testing.T) {
	// Bare calls without a preceding use/import should not be captured
	path := writeTempFile(t, `defmodule MyApp.NoUse do
  embedded_schema do
    field :name, :string
  end
end
`)

	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, r := range refs {
		if r.Kind == "call" && r.Function == "embedded_schema" {
			t.Errorf("should not capture bare call without a use/import injector, got %+v", r)
		}
	}
}

func filterRefs(refs []Reference, kind string) []Reference {
	var out []Reference
	for _, r := range refs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func TestParseFile_CallbackDefinitions(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Worker do
  @callback init(args :: term) :: {:ok, state :: term} | {:error, reason :: term}
  @callback handle_call(request, from, state) :: {:reply, term, term}
  @callback name :: String.t()
  @macrocallback before_compile(env :: Macro.Env.t) :: Macro.t
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	type callbackExpectation struct {
		function string
		kind     string
		arity    int
	}

	expected := []callbackExpectation{
		{"init", "callback", 1},
		{"handle_call", "callback", 3},
		{"name", "callback", 0},
		{"before_compile", "macrocallback", 1},
	}

	var callbacks []Definition
	for _, d := range defs {
		if d.Kind == "callback" || d.Kind == "macrocallback" {
			callbacks = append(callbacks, d)
		}
	}

	if len(callbacks) != len(expected) {
		t.Fatalf("expected %d callbacks, got %d: %+v", len(expected), len(callbacks), callbacks)
	}

	for i, exp := range expected {
		cb := callbacks[i]
		if cb.Function != exp.function || cb.Kind != exp.kind || cb.Arity != exp.arity {
			t.Errorf("callback[%d]: expected {%s, %s, arity=%d}, got {%s, %s, arity=%d}",
				i, exp.function, exp.kind, exp.arity, cb.Function, cb.Kind, cb.Arity)
		}
		if cb.Module != "MyApp.Worker" {
			t.Errorf("callback[%d]: expected module MyApp.Worker, got %s", i, cb.Module)
		}
	}
}

func TestParseFile_CallbackOutsideModuleIgnored(t *testing.T) {
	path := writeTempFile(t, `@callback orphan(arg) :: term
defmodule MyApp.Worker do
  @callback valid(arg) :: term
end
`)

	defs, _, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var callbacks []Definition
	for _, d := range defs {
		if d.Kind == "callback" || d.Kind == "macrocallback" {
			callbacks = append(callbacks, d)
		}
	}

	if len(callbacks) != 1 {
		t.Fatalf("expected 1 callback (only the one inside the module), got %d: %+v", len(callbacks), callbacks)
	}
	if callbacks[0].Function != "valid" {
		t.Errorf("expected callback 'valid', got %q", callbacks[0].Function)
	}
}

func TestParseFile_CallbackModuleRefsExtracted(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Behaviour do
  @callback fetch(id :: integer) :: {:ok, User.t()} | {:error, String.t()}
end
`)

	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Module refs in the callback type annotation should be extracted as call refs.
	refModules := map[string]bool{}
	for _, r := range refs {
		if r.Kind == "call" {
			refModules[r.Module+"."+r.Function] = true
		}
	}

	if !refModules["User.t"] {
		t.Errorf("expected User.t ref from callback type annotation, refs: %v", refs)
	}
	if !refModules["String.t"] {
		t.Errorf("expected String.t ref from callback type annotation, refs: %v", refs)
	}
}
