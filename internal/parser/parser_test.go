package parser

import (
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

	defs, err := ParseFile(path)
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

func TestParseFile_InlineDoSyntax(t *testing.T) {
	path := writeTempFile(t, `defmodule MyApp.Math do
  def add(a, b), do: a + b
  defp secret(x), do: x * 2
  def identity(x), do: x
end
`)

	defs, err := ParseFile(path)
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
