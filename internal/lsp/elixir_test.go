package lsp

import (
	"strings"
	"testing"
)

func TestExtractExpression(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		col      int
		expected string
	}{
		// Cursor on middle segment → truncate at that segment's end
		{
			name:     "cursor on middle module segment",
			line:     "    Foo.Bar.baz(123)",
			col:      9,
			expected: "Foo.Bar",
		},
		// Cursor on dot → include next segment
		{
			name:     "cursor on dot between segments",
			line:     "    Foo.Bar.Baz",
			col:      7,
			expected: "Foo.Bar",
		},
		{
			name:     "bare function",
			line:     "    do_something(x)",
			col:      7,
			expected: "do_something",
		},
		// Cursor on first segment → return only that segment
		{
			name:     "cursor at start of expr",
			line:     "    Foo.bar()",
			col:      4,
			expected: "Foo",
		},
		// Cursor on last segment → return full expression
		{
			name:     "cursor at end of expr",
			line:     "    Foo.bar()",
			col:      10,
			expected: "Foo.bar",
		},
		{
			name:     "function with question mark",
			line:     "    valid?(x)",
			col:      6,
			expected: "valid?",
		},
		{
			name:     "function with bang",
			line:     "    process!(x)",
			col:      6,
			expected: "process!",
		},
		// Cursor on first segment of underscore module
		{
			name:     "cursor on first segment of underscore module",
			line:     "    MyApp_Web.Router",
			col:      8,
			expected: "MyApp_Web",
		},
		// Cursor on last segment → full expr
		{
			name:     "cursor on last segment",
			line:     "    MyApp_Web.Router",
			col:      16,
			expected: "MyApp_Web.Router",
		},
		{
			name:     "empty line",
			line:     "",
			col:      0,
			expected: "",
		},
		{
			name:     "cursor on paren",
			line:     "    Foo.bar()",
			col:      11,
			expected: "",
		},
		// Three-part expression: cursor on each segment
		{
			name:     "three-part: cursor on first",
			line:     "MyApp.Repo.all",
			col:      2,
			expected: "MyApp",
		},
		{
			name:     "three-part: cursor on middle",
			line:     "MyApp.Repo.all",
			col:      7,
			expected: "MyApp.Repo",
		},
		{
			name:     "three-part: cursor on last",
			line:     "MyApp.Repo.all",
			col:      11,
			expected: "MyApp.Repo.all",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractExpression(tt.line, tt.col)
			if got != tt.expected {
				t.Errorf("ExtractExpression(%q, %d) = %q, want %q", tt.line, tt.col, got, tt.expected)
			}
		})
	}
}

func TestExtractModuleAndFunction(t *testing.T) {
	tests := []struct {
		name         string
		expr         string
		expectedMod  string
		expectedFunc string
	}{
		{
			name:         "module with function",
			expr:         "Foo.Bar.baz",
			expectedMod:  "Foo.Bar",
			expectedFunc: "baz",
		},
		{
			name:         "module without function",
			expr:         "Foo.Bar.Baz",
			expectedMod:  "Foo.Bar.Baz",
			expectedFunc: "",
		},
		{
			name:         "single module",
			expr:         "Repo",
			expectedMod:  "Repo",
			expectedFunc: "",
		},
		{
			name:         "bare function name",
			expr:         "do_something",
			expectedMod:  "",
			expectedFunc: "do_something",
		},
		{
			name:         "function with underscores",
			expr:         "Foo.Bar.my_function_name",
			expectedMod:  "Foo.Bar",
			expectedFunc: "my_function_name",
		},
		{
			name:         "deeply nested module",
			expr:         "MyApp.Handlers.Webhooks.V2.process_event",
			expectedMod:  "MyApp.Handlers.Webhooks.V2",
			expectedFunc: "process_event",
		},
		{
			name:         "empty string",
			expr:         "",
			expectedMod:  "",
			expectedFunc: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mod, fn := ExtractModuleAndFunction(tt.expr)
			if mod != tt.expectedMod {
				t.Errorf("module: got %q, want %q", mod, tt.expectedMod)
			}
			if fn != tt.expectedFunc {
				t.Errorf("function: got %q, want %q", fn, tt.expectedFunc)
			}
		})
	}
}

func TestExtractAliasBlockParent(t *testing.T) {
	t.Run("cursor inside multi-line block", func(t *testing.T) {
		text := `defmodule MyApp.Web do
  alias MyApp.Services.{
    Accounts,

  }
end`
		parent, ok := ExtractAliasBlockParent(strings.Split(text, "\n"), 3)
		if !ok {
			t.Fatal("expected to be inside alias block")
		}
		if parent != "MyApp.Services" {
			t.Errorf("got %q, want MyApp.Services", parent)
		}
	})

	t.Run("cursor on line with children", func(t *testing.T) {
		text := `defmodule MyApp.Web do
  alias MyApp.Services.{
    Accounts,
  }
end`
		parent, ok := ExtractAliasBlockParent(strings.Split(text, "\n"), 2)
		if !ok {
			t.Fatal("expected to be inside alias block")
		}
		if parent != "MyApp.Services" {
			t.Errorf("got %q, want MyApp.Services", parent)
		}
	})

	t.Run("cursor after closing brace", func(t *testing.T) {
		text := `defmodule MyApp.Web do
  alias MyApp.Services.{
    Accounts
  }

end`
		_, ok := ExtractAliasBlockParent(strings.Split(text, "\n"), 4)
		if ok {
			t.Error("should not be inside alias block after closing brace")
		}
	})

	t.Run("cursor on normal alias line", func(t *testing.T) {
		text := `defmodule MyApp.Web do
  alias MyApp.Repo

end`
		_, ok := ExtractAliasBlockParent(strings.Split(text, "\n"), 2)
		if ok {
			t.Error("should not be inside alias block on a normal line")
		}
	})

	t.Run("cursor on same line as opening brace", func(t *testing.T) {
		text := `defmodule MyApp.Web do
  alias MyApp.Handlers.{
end`
		parent, ok := ExtractAliasBlockParent(strings.Split(text, "\n"), 1)
		if !ok {
			t.Fatal("expected to be inside alias block")
		}
		if parent != "MyApp.Handlers" {
			t.Errorf("got %q, want MyApp.Handlers", parent)
		}
	})

	t.Run("resolves __MODULE__ in parent", func(t *testing.T) {
		text := `defmodule MyApp.HRIS do
  alias __MODULE__.{
    Services,

  }
end`
		parent, ok := ExtractAliasBlockParent(strings.Split(text, "\n"), 3)
		if !ok {
			t.Fatal("expected to be inside alias block")
		}
		if parent != "MyApp.HRIS" {
			t.Errorf("got %q, want MyApp.HRIS", parent)
		}
	})

	t.Run("single-line block with closing brace", func(t *testing.T) {
		text := `defmodule MyApp.Web do
  alias MyApp.{Accounts, Users}

end`
		_, ok := ExtractAliasBlockParent(strings.Split(text, "\n"), 1)
		if ok {
			t.Error("should not be inside alias block when braces close on same line")
		}
	})

	t.Run("trailing brace on content line", func(t *testing.T) {
		text := `defmodule MyApp.Web do
  alias MyApp.Billing.{
    Services.MakePayment }
end`
		parent, ok := ExtractAliasBlockParent(strings.Split(text, "\n"), 2)
		if !ok {
			t.Fatal("expected to be inside alias block when } follows module content")
		}
		if parent != "MyApp.Billing" {
			t.Errorf("got %q, want MyApp.Billing", parent)
		}
	})

	t.Run("blank lines between alias and cursor", func(t *testing.T) {
		text := `defmodule MyApp.Web do
  alias MyApp.Services.{
    Accounts,


  }
end`
		parent, ok := ExtractAliasBlockParent(strings.Split(text, "\n"), 4)
		if !ok {
			t.Fatal("expected to be inside alias block")
		}
		if parent != "MyApp.Services" {
			t.Errorf("got %q, want MyApp.Services", parent)
		}
	})
}

func TestExtractAliases(t *testing.T) {
	t.Run("simple alias", func(t *testing.T) {
		aliases := ExtractAliases("  alias MyApp.Repo")
		if aliases["Repo"] != "MyApp.Repo" {
			t.Errorf("got %q, want MyApp.Repo", aliases["Repo"])
		}
	})

	t.Run("alias with as:", func(t *testing.T) {
		aliases := ExtractAliases("  alias MyApp.Handlers.Foo, as: MyFoo")
		if aliases["MyFoo"] != "MyApp.Handlers.Foo" {
			t.Errorf("got %q, want MyApp.Handlers.Foo", aliases["MyFoo"])
		}
	})

	t.Run("multi-alias", func(t *testing.T) {
		aliases := ExtractAliases("  alias MyApp.Handlers.{Foo, Bar, Baz}")
		if aliases["Foo"] != "MyApp.Handlers.Foo" {
			t.Errorf("Foo: got %q", aliases["Foo"])
		}
		if aliases["Bar"] != "MyApp.Handlers.Bar" {
			t.Errorf("Bar: got %q", aliases["Bar"])
		}
		if aliases["Baz"] != "MyApp.Handlers.Baz" {
			t.Errorf("Baz: got %q", aliases["Baz"])
		}
	})

	t.Run("multiple alias lines", func(t *testing.T) {
		text := "  alias MyApp.Repo\n  alias MyApp.Accounts.User\n  alias MyApp.Handlers.{Foo, Bar}"
		aliases := ExtractAliases(text)
		if aliases["Repo"] != "MyApp.Repo" {
			t.Errorf("Repo: got %q", aliases["Repo"])
		}
		if aliases["User"] != "MyApp.Accounts.User" {
			t.Errorf("User: got %q", aliases["User"])
		}
		if aliases["Foo"] != "MyApp.Handlers.Foo" {
			t.Errorf("Foo: got %q", aliases["Foo"])
		}
		if aliases["Bar"] != "MyApp.Handlers.Bar" {
			t.Errorf("Bar: got %q", aliases["Bar"])
		}
	})

	t.Run("ignores non-alias lines", func(t *testing.T) {
		text := "defmodule Foo do\n  use GenServer\n  alias MyApp.Repo\n  def foo, do: :ok"
		aliases := ExtractAliases(text)
		if len(aliases) != 1 {
			t.Errorf("expected 1 alias, got %d", len(aliases))
		}
		if aliases["Repo"] != "MyApp.Repo" {
			t.Errorf("Repo: got %q", aliases["Repo"])
		}
	})

	t.Run("resolves __MODULE__ using defmodule name", func(t *testing.T) {
		text := "defmodule MyApp.HRIS do\n  alias __MODULE__.Schemas.UserRelationship\n  alias __MODULE__.Services\nend"
		aliases := ExtractAliases(text)
		if aliases["UserRelationship"] != "MyApp.HRIS.Schemas.UserRelationship" {
			t.Errorf("UserRelationship: got %q, want MyApp.HRIS.Schemas.UserRelationship", aliases["UserRelationship"])
		}
		if aliases["Services"] != "MyApp.HRIS.Services" {
			t.Errorf("Services: got %q, want MyApp.HRIS.Services", aliases["Services"])
		}
	})

	t.Run("resolves __MODULE__ with as: alias", func(t *testing.T) {
		text := "defmodule MyApp.MyPayProvider do\n  alias __MODULE__, as: MyPayProvider\nend"
		aliases := ExtractAliases(text)
		if aliases["MyPayProvider"] != "MyApp.MyPayProvider" {
			t.Errorf("MyPayProvider: got %q, want MyApp.MyPayProvider", aliases["MyPayProvider"])
		}
	})

	t.Run("multi-line alias with as on next line", func(t *testing.T) {
		text := "defmodule MyApp.Web do\n  alias MyApp.Helpers.Paginator,\n    as: Pages\nend"
		aliases := ExtractAliases(text)
		if aliases["Pages"] != "MyApp.Helpers.Paginator" {
			t.Errorf("Pages: got %q, want MyApp.Helpers.Paginator", aliases["Pages"])
		}
		// Should NOT also register as a simple alias under the last segment
		if _, ok := aliases["Paginator"]; ok {
			t.Error("should not register simple alias Paginator when as: is on next line")
		}
	})

	t.Run("multi-line alias with as and extra whitespace before comma", func(t *testing.T) {
		text := "defmodule MyApp.Web do\n  alias MyApp.Billing.Services.MakePayment        ,\n  as: MakePaymentNow\nend"
		aliases := ExtractAliases(text)
		if aliases["MakePaymentNow"] != "MyApp.Billing.Services.MakePayment" {
			t.Errorf("MakePaymentNow: got %q, want MyApp.Billing.Services.MakePayment", aliases["MakePaymentNow"])
		}
		if _, ok := aliases["MakePayment"]; ok {
			t.Error("should not register simple alias MakePayment when as: is on next line")
		}
	})

	t.Run("multi-line multi-alias with braces spanning lines", func(t *testing.T) {
		text := "defmodule MyApp.Web do\n  alias MyApp.Handlers.{\n    Accounts,\n    Users,\n    Profiles\n  }\nend"
		aliases := ExtractAliases(text)
		if aliases["Accounts"] != "MyApp.Handlers.Accounts" {
			t.Errorf("Accounts: got %q, want MyApp.Handlers.Accounts", aliases["Accounts"])
		}
		if aliases["Users"] != "MyApp.Handlers.Users" {
			t.Errorf("Users: got %q, want MyApp.Handlers.Users", aliases["Users"])
		}
		if aliases["Profiles"] != "MyApp.Handlers.Profiles" {
			t.Errorf("Profiles: got %q, want MyApp.Handlers.Profiles", aliases["Profiles"])
		}
	})

	t.Run("multi-line multi-alias with comments inside", func(t *testing.T) {
		text := "defmodule MyApp.Web do\n  alias MyApp.Services.{\n    Accounts,\n    # Users is deprecated\n    Profiles\n  }\nend"
		aliases := ExtractAliases(text)
		if aliases["Accounts"] != "MyApp.Services.Accounts" {
			t.Errorf("Accounts: got %q, want MyApp.Services.Accounts", aliases["Accounts"])
		}
		if aliases["Profiles"] != "MyApp.Services.Profiles" {
			t.Errorf("Profiles: got %q, want MyApp.Services.Profiles", aliases["Profiles"])
		}
		if len(aliases) != 2 {
			t.Errorf("expected 2 aliases, got %d: %v", len(aliases), aliases)
		}
	})

	t.Run("multi-line multi-alias with multiple children per line", func(t *testing.T) {
		text := "defmodule MyApp.Web do\n  alias MyApp.Handlers.{\n    Accounts, Users,\n    Profiles\n  }\nend"
		aliases := ExtractAliases(text)
		if aliases["Accounts"] != "MyApp.Handlers.Accounts" {
			t.Errorf("Accounts: got %q, want MyApp.Handlers.Accounts", aliases["Accounts"])
		}
		if aliases["Users"] != "MyApp.Handlers.Users" {
			t.Errorf("Users: got %q, want MyApp.Handlers.Users", aliases["Users"])
		}
		if aliases["Profiles"] != "MyApp.Handlers.Profiles" {
			t.Errorf("Profiles: got %q, want MyApp.Handlers.Profiles", aliases["Profiles"])
		}
	})

	t.Run("multi-line multi-alias with trailing comma", func(t *testing.T) {
		text := "defmodule MyApp.Web do\n  alias MyApp.Handlers.{\n    Accounts,\n    Users,\n  }\nend"
		aliases := ExtractAliases(text)
		if aliases["Accounts"] != "MyApp.Handlers.Accounts" {
			t.Errorf("Accounts: got %q, want MyApp.Handlers.Accounts", aliases["Accounts"])
		}
		if aliases["Users"] != "MyApp.Handlers.Users" {
			t.Errorf("Users: got %q, want MyApp.Handlers.Users", aliases["Users"])
		}
		if len(aliases) != 2 {
			t.Errorf("expected 2 aliases, got %d: %v", len(aliases), aliases)
		}
	})

	t.Run("multi-line alias bail-out on new statement", func(t *testing.T) {
		text := "defmodule MyApp.Web do\n  alias MyApp.Handlers.{\n    Accounts,\n  def foo, do: :ok\nend"
		aliases := ExtractAliases(text)
		// Key assertion: no alias for "foo" or anything weird — the def line must not be swallowed
		if _, ok := aliases["foo"]; ok {
			t.Error("should not register 'foo' as an alias")
		}
	})

	t.Run("partial __MODULE__ alias resolves in lookup", func(t *testing.T) {
		// Simulates: alias __MODULE__.Services -> Services = MyApp.HRIS.Services
		// Then a lookup for "Services.AssociateWithTeamV2" should resolve
		// to "MyApp.HRIS.Services.AssociateWithTeamV2"
		text := "defmodule MyApp.HRIS do\n  alias __MODULE__.Services\nend"
		aliases := ExtractAliases(text)
		// The LSP definition handler does this partial lookup:
		moduleRef := "Services"
		suffix := "AssociateWithTeamV2"
		resolved, ok := aliases[moduleRef]
		if !ok {
			t.Fatal("Services alias not found")
		}
		full := resolved + "." + suffix
		if full != "MyApp.HRIS.Services.AssociateWithTeamV2" {
			t.Errorf("got %q, want MyApp.HRIS.Services.AssociateWithTeamV2", full)
		}
	})
}

func TestExtractAliasesInScope(t *testing.T) {
	src := `defmodule MyApp.Outer do
  alias MyApp.Repo
  alias MyApp.Config

  defmodule Inner do
    alias MyApp.Billing.Invoice

    def run do
      Invoice.get()
    end
  end

  def call do
    Repo.all()
  end
end
`
	t.Run("outer scope sees outer aliases only", func(t *testing.T) {
		// Line 13 = "def call do" inside Outer
		aliases := ExtractAliasesInScope(src, 13)
		if aliases["Repo"] != "MyApp.Repo" {
			t.Errorf("expected Repo alias in outer scope, got %q", aliases["Repo"])
		}
		if _, ok := aliases["Invoice"]; ok {
			t.Error("Invoice alias should NOT be visible in outer scope")
		}
	})

	t.Run("inner scope sees inner aliases only", func(t *testing.T) {
		// Line 8 = "Invoice.get()" inside Inner
		aliases := ExtractAliasesInScope(src, 8)
		if aliases["Invoice"] != "MyApp.Billing.Invoice" {
			t.Errorf("expected Invoice alias in inner scope, got %q", aliases["Invoice"])
		}
		if _, ok := aliases["Repo"]; ok {
			t.Error("Repo alias should NOT be visible in inner scope")
		}
	})

	t.Run("nested module with conflicting alias", func(t *testing.T) {
		conflictSrc := `defmodule MyApp.Payments do
  defmodule TransactionRecord do
    alias MyApp.Billing.TransactionRecord
    def schema, do: %{}
  end
end
`
		// Line 3 = "def schema" inside the nested TransactionRecord
		aliases := ExtractAliasesInScope(conflictSrc, 3)
		if aliases["TransactionRecord"] != "MyApp.Billing.TransactionRecord" {
			t.Errorf("expected Billing alias inside nested module, got %q", aliases["TransactionRecord"])
		}

		// Line 0 = "defmodule MyApp.Payments do" — outer scope has no aliases
		aliases = ExtractAliasesInScope(conflictSrc, 0)
		if _, ok := aliases["TransactionRecord"]; ok {
			t.Error("TransactionRecord alias should NOT be visible in outer scope")
		}
	})

	t.Run("fn...end block does not break scope tracking", func(t *testing.T) {
		// Regression: fn...end has an "end" without a corresponding "do",
		// which caused the depth counter to go out of sync and pop the
		// module scope prematurely.
		fnSrc := `defmodule MyApp.Aggregator do
  alias MyApp.Filters

  defp build_filter(:active, items) do
    codes =
      Filters.get_codes(items) ++
        Filters.get_extra_codes(items)

    fn item ->
      item.code in codes
    end
  end

  def run(items) do
    Filters.all(items)
  end
end
`
		// Line 14 = "def run" — should still see aliases from the module scope
		aliases := ExtractAliasesInScope(fnSrc, 14)
		if aliases["Filters"] != "MyApp.Filters" {
			t.Errorf("expected Filters alias after fn...end block, got %q", aliases["Filters"])
		}
	})

	t.Run("fn with end in comment does not confuse depth", func(t *testing.T) {
		commentSrc := `defmodule MyApp.Worker do
  alias MyApp.Processor

  defp make_handler(items) do
    fn -> # this is something in the end
      Processor.run(items)
    end
  end

  def execute(items) do
    Processor.start(items)
  end
end
`
		// Line 10 = "def execute" — should still see aliases
		aliases := ExtractAliasesInScope(commentSrc, 10)
		if aliases["Processor"] != "MyApp.Processor" {
			t.Errorf("expected Processor alias after fn with end-in-comment, got %q", aliases["Processor"])
		}
	})

	t.Run("heredoc containing end does not break scope", func(t *testing.T) {
		heredocSrc := `defmodule MyApp.Docs do
  alias MyApp.Formatter

  @moduledoc """
  end
  some text
  end
  """

  def render(text) do
    Formatter.run(text)
  end
end
`
		// Line 10 = "def render" — should still see aliases despite "end" lines in heredoc
		aliases := ExtractAliasesInScope(heredocSrc, 10)
		if aliases["Formatter"] != "MyApp.Formatter" {
			t.Errorf("expected Formatter alias after heredoc with end lines, got %q", aliases["Formatter"])
		}
	})

	t.Run("string containing do or end does not affect depth", func(t *testing.T) {
		stringSrc := `defmodule MyApp.Config do
  alias MyApp.Settings

  def label do
    x = "something do"
    y = "end"
    Settings.get(x, y)
  end
end
`
		// Line 7 = "Settings.get(x, y)" — aliases should still resolve
		aliases := ExtractAliasesInScope(stringSrc, 7)
		if aliases["Settings"] != "MyApp.Settings" {
			t.Errorf("expected Settings alias with do/end in strings, got %q", aliases["Settings"])
		}
	})

	t.Run("trailing fn with no args does not break scope", func(t *testing.T) {
		// Regression: "handler = fn" at end of line was not detected by ContainsFn
		// because all patterns required a space after "fn".
		trailingFnSrc := `defmodule MyApp.Builder do
  alias MyApp.Validator

  def build do
    handler = fn
      :ok -> true
      :error -> false
    end

    Validator.run(handler)
  end
end
`
		// Line 10 = "Validator.run(handler)" — should still see aliases
		aliases := ExtractAliasesInScope(trailingFnSrc, 10)
		if aliases["Validator"] != "MyApp.Validator" {
			t.Errorf("expected Validator alias after trailing fn, got %q", aliases["Validator"])
		}
	})
}

func TestExtractImports(t *testing.T) {
	t.Run("parses imports", func(t *testing.T) {
		text := "  import MyApp.Helpers.Formatting\n  import Ecto.Query"
		imports := ExtractImports(text)
		if len(imports) != 2 {
			t.Fatalf("expected 2 imports, got %d", len(imports))
		}
		if imports[0] != "MyApp.Helpers.Formatting" {
			t.Errorf("imports[0]: got %q", imports[0])
		}
		if imports[1] != "Ecto.Query" {
			t.Errorf("imports[1]: got %q", imports[1])
		}
	})

	t.Run("ignores non-import lines", func(t *testing.T) {
		text := "defmodule Foo do\n  import Ecto.Query\n  alias MyApp.Repo"
		imports := ExtractImports(text)
		if len(imports) != 1 {
			t.Errorf("expected 1 import, got %d", len(imports))
		}
	})
}

func TestFindFunctionDefinition(t *testing.T) {
	text := `defmodule Foo do
  def public_func(a, b) do
    a + b
  end

  defp private_func(x) do
    x * 2
  end

  defmacro my_macro(expr) do
    quote do: unquote(expr)
  end

  defmacrop private_macro(expr) do
    quote do: unquote(expr)
  end
end`

	tests := []struct {
		name          string
		functionName  string
		expectedLine  int
		expectedFound bool
	}{
		{"public function", "public_func", 2, true},
		{"private function", "private_func", 6, true},
		{"macro", "my_macro", 10, true},
		{"private macro", "private_macro", 14, true},
		{"missing function", "nonexistent", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, found := FindFunctionDefinition(text, tt.functionName)
			if found != tt.expectedFound {
				t.Errorf("found: got %v, want %v", found, tt.expectedFound)
			}
			if line != tt.expectedLine {
				t.Errorf("line: got %d, want %d", line, tt.expectedLine)
			}
		})
	}
}

func TestFindFunctionDefinition_Guards(t *testing.T) {
	text := `defmodule Foo do
  defguard is_admin(user) when user.role == :admin
  defguardp is_active(user) when user.status == :active
end`

	line, found := FindFunctionDefinition(text, "is_admin")
	if !found || line != 2 {
		t.Errorf("is_admin: got line %d found %v", line, found)
	}

	line, found = FindFunctionDefinition(text, "is_active")
	if !found || line != 3 {
		t.Errorf("is_active: got line %d found %v", line, found)
	}
}

func TestFindFunctionDefinition_Delegate(t *testing.T) {
	text := `defmodule Foo do
  defdelegate fetch(id), to: MyApp.Repo
end`

	line, found := FindFunctionDefinition(text, "fetch")
	if !found || line != 2 {
		t.Errorf("fetch: got line %d found %v", line, found)
	}
}

func TestFindFunctionDefinition_InlineDo(t *testing.T) {
	text := `defmodule Foo do
  def add(a, b), do: a + b
  defp secret(x), do: x * 2
end`

	line, found := FindFunctionDefinition(text, "add")
	if !found || line != 2 {
		t.Errorf("add: got line %d found %v", line, found)
	}
	line, found = FindFunctionDefinition(text, "secret")
	if !found || line != 3 {
		t.Errorf("secret: got line %d found %v", line, found)
	}
}

func TestExtractExpression_PipeOperator(t *testing.T) {
	line := "    |> Foo.Bar.transform()"
	// col=12 is on 'a' of Bar → returns up to and including Bar
	if got := ExtractExpression(line, 12); got != "Foo.Bar" {
		t.Errorf("cursor on Bar: got %q, want %q", got, "Foo.Bar")
	}
	// col=15 is on 't' of transform → returns full expression
	if got := ExtractExpression(line, 15); got != "Foo.Bar.transform" {
		t.Errorf("cursor on transform: got %q, want %q", got, "Foo.Bar.transform")
	}
}

func TestExtractAliases_DoesNotMatchAliasInStrings(t *testing.T) {
	// Lines that happen to contain "alias" but aren't real alias declarations
	text := `  some_var = "alias MyApp.Fake"
  alias MyApp.Real`
	aliases := ExtractAliases(text)
	if _, ok := aliases["Fake"]; ok {
		t.Error("should not match alias inside a string")
	}
	if aliases["Real"] != "MyApp.Real" {
		t.Errorf("Real: got %q", aliases["Real"])
	}
}

func TestExtractModuleAndFunction_QuestionMarkBang(t *testing.T) {
	mod, fn := ExtractModuleAndFunction("Foo.valid?")
	if mod != "Foo" || fn != "valid?" {
		t.Errorf("got mod=%q fn=%q", mod, fn)
	}

	mod, fn = ExtractModuleAndFunction("Foo.process!")
	if mod != "Foo" || fn != "process!" {
		t.Errorf("got mod=%q fn=%q", mod, fn)
	}
}

func TestExtractModuleAttribute(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		col      int
		expected string
	}{
		{"cursor on attr name", "      tags: @open_api_shared_tags,", 18, "open_api_shared_tags"},
		{"cursor on @", "      tags: @open_api_shared_tags,", 12, "open_api_shared_tags"},
		{"cursor at end of attr", "      tags: @open_api_shared_tags,", 31, "open_api_shared_tags"},
		{"not on attr", "      tags: :something,", 10, ""},
		{"standalone attr", "  @endpoint_scopes %{", 4, "endpoint_scopes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractModuleAttribute(tt.line, tt.col)
			if got != tt.expected {
				t.Errorf("ExtractModuleAttribute(%q, %d) = %q, want %q", tt.line, tt.col, got, tt.expected)
			}
		})
	}
}

func TestFindModuleAttributeDefinition(t *testing.T) {
	text := `defmodule MyAppWeb.V1.PayslipController do
  @open_api_shared_tags ["Payroll", "Payslips"]

  @endpoint_scopes %{
    index: %{scopes: [:read]}
  }

  def show(conn, _params) do
    tags = @open_api_shared_tags
    :ok
  end
end`

	t.Run("finds user-defined attribute", func(t *testing.T) {
		line, found := FindModuleAttributeDefinition(text, "open_api_shared_tags")
		if !found || line != 2 {
			t.Errorf("expected line 2, got line=%d found=%v", line, found)
		}
	})

	t.Run("finds multi-line attribute", func(t *testing.T) {
		line, found := FindModuleAttributeDefinition(text, "endpoint_scopes")
		if !found || line != 4 {
			t.Errorf("expected line 4, got line=%d found=%v", line, found)
		}
	})

	t.Run("ignores reserved attributes", func(t *testing.T) {
		for _, reserved := range []string{"doc", "moduledoc", "spec", "behaviour", "callback", "impl", "derive"} {
			_, found := FindModuleAttributeDefinition(text, reserved)
			if found {
				t.Errorf("reserved attr @%s should not be found", reserved)
			}
		}
	})

	t.Run("returns false for missing attribute", func(t *testing.T) {
		_, found := FindModuleAttributeDefinition(text, "nonexistent")
		if found {
			t.Error("expected not found for nonexistent attribute")
		}
	})
}

func TestExtractCompletionContext(t *testing.T) {
	tests := []struct {
		name         string
		line         string
		col          int
		wantPrefix   string
		wantAfterDot bool
	}{
		{
			name:         "module prefix",
			line:         "  MyApp.Han",
			col:          11,
			wantPrefix:   "MyApp.Han",
			wantAfterDot: false,
		},
		{
			name:         "after dot — function listing",
			line:         "  Foo.",
			col:          6,
			wantPrefix:   "Foo",
			wantAfterDot: true,
		},
		{
			name:         "function prefix after dot",
			line:         "  Foo.ba",
			col:          8,
			wantPrefix:   "Foo.ba",
			wantAfterDot: false,
		},
		{
			name:         "bare function prefix",
			line:         "  some_func",
			col:          11,
			wantPrefix:   "some_func",
			wantAfterDot: false,
		},
		{
			name:         "cursor at start — no completion",
			line:         "  Foo.bar",
			col:          0,
			wantPrefix:   "",
			wantAfterDot: false,
		},
		{
			name:         "empty line",
			line:         "",
			col:          0,
			wantPrefix:   "",
			wantAfterDot: false,
		},
		{
			name:         "cursor on whitespace",
			line:         "  Foo.bar  ",
			col:          10,
			wantPrefix:   "",
			wantAfterDot: false,
		},
		{
			name:         "deeply nested module dot",
			line:         "  MyApp.Handlers.Webhooks.V2.",
			col:          29,
			wantPrefix:   "MyApp.Handlers.Webhooks.V2",
			wantAfterDot: true,
		},
		{
			name:         "question mark function",
			line:         "  Foo.valid?",
			col:          12,
			wantPrefix:   "Foo.valid?",
			wantAfterDot: false,
		},
		{
			name:         "bang function",
			line:         "  Foo.process!",
			col:          14,
			wantPrefix:   "Foo.process!",
			wantAfterDot: false,
		},
		{
			name:         "mid-word cursor",
			line:         "  Enum.map_reduce",
			col:          10,
			wantPrefix:   "Enum.map",
			wantAfterDot: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, afterDot, _ := ExtractCompletionContext(tt.line, tt.col)
			if prefix != tt.wantPrefix {
				t.Errorf("prefix: got %q, want %q", prefix, tt.wantPrefix)
			}
			if afterDot != tt.wantAfterDot {
				t.Errorf("afterDot: got %v, want %v", afterDot, tt.wantAfterDot)
			}
		})
	}
}

func TestExtractUses(t *testing.T) {
	t.Run("extracts use declarations", func(t *testing.T) {
		text := "defmodule Foo do\n  use Ecto.Schema\n  use Remote.Ecto.Schema\n  use GenServer\nend"
		uses := ExtractUses(text)
		if len(uses) != 3 {
			t.Fatalf("expected 3 uses, got %d: %v", len(uses), uses)
		}
		if uses[0] != "Ecto.Schema" {
			t.Errorf("uses[0]: got %q, want Ecto.Schema", uses[0])
		}
		if uses[1] != "Remote.Ecto.Schema" {
			t.Errorf("uses[1]: got %q, want Remote.Ecto.Schema", uses[1])
		}
		if uses[2] != "GenServer" {
			t.Errorf("uses[2]: got %q, want GenServer", uses[2])
		}
	})

	t.Run("ignores non-use lines", func(t *testing.T) {
		text := "defmodule Foo do\n  alias MyApp.Repo\n  import Ecto.Query\nend"
		uses := ExtractUses(text)
		if len(uses) != 0 {
			t.Errorf("expected 0 uses, got %d: %v", len(uses), uses)
		}
	})

	t.Run("empty text", func(t *testing.T) {
		uses := ExtractUses("")
		if len(uses) != 0 {
			t.Errorf("expected 0 uses, got %d", len(uses))
		}
	})
}

func TestExtractUsingImports(t *testing.T) {
	t.Run("extracts and resolves alias", func(t *testing.T) {
		// Mirrors Remote.Ecto.Schema's __using__ structure
		text := `defmodule Remote.Ecto.Schema do
  alias Remote.Ecto.Schema

  defmacro __using__(args \\ []) do
    quote do
      import Ecto.Schema, except: [schema: 2]
      import Schema
      alias Remote.Ecto.Schema.Fields
    end
  end

  defmacro schema(source, do: block) do
    :ok
  end
end`
		imports, _, _, _, _ := parseUsingBody(text)
		if len(imports) != 2 {
			t.Fatalf("expected 2 imports, got %d: %v", len(imports), imports)
		}
		if imports[0] != "Ecto.Schema" {
			t.Errorf("imports[0]: got %q, want Ecto.Schema", imports[0])
		}
		// "import Schema" resolves via "alias Remote.Ecto.Schema" → Schema
		if imports[1] != "Remote.Ecto.Schema" {
			t.Errorf("imports[1]: got %q, want Remote.Ecto.Schema", imports[1])
		}
	})

	t.Run("stops at next def at same indent", func(t *testing.T) {
		text := `defmodule Lib do
  defmacro __using__(_) do
    quote do
      import Foo
    end
  end

  def other_func, do: :ok
end`
		imports, _, _, _, _ := parseUsingBody(text)
		if len(imports) != 1 || imports[0] != "Foo" {
			t.Errorf("expected [Foo], got %v", imports)
		}
	})

	t.Run("no __using__ returns nil", func(t *testing.T) {
		text := "defmodule Lib do\n  def foo, do: :ok\nend"
		imports, _, _, _, _ := parseUsingBody(text)
		if len(imports) != 0 {
			t.Errorf("expected no imports, got %v", imports)
		}
	})
}

func TestExtractUsingInlineDefs(t *testing.T) {
	text := `defmodule MyLib do
  defmacro __using__(_opts) do
    quote do
      def helper(x), do: x * 2
      def other(y), do: y
    end
  end

  def module_level, do: :ok
end`

	inlineDefsOf := func(name string) []int {
		_, defs, _, _, _ := parseUsingBody(text)
		var lines []int
		for _, d := range defs[name] {
			lines = append(lines, d.line)
		}
		return lines
	}

	t.Run("finds inline def", func(t *testing.T) {
		lineNums := inlineDefsOf("helper")
		if len(lineNums) != 1 || lineNums[0] != 4 {
			t.Errorf("expected [4], got %v", lineNums)
		}
	})

	t.Run("does not find module-level def", func(t *testing.T) {
		lineNums := inlineDefsOf("module_level")
		if len(lineNums) != 0 {
			t.Errorf("expected empty, got %v", lineNums)
		}
	})

	t.Run("returns empty for missing function", func(t *testing.T) {
		lineNums := inlineDefsOf("nonexistent")
		if len(lineNums) != 0 {
			t.Errorf("expected empty, got %v", lineNums)
		}
	})
}

func TestParseUsingBody_InlineDefArity(t *testing.T) {
	text := `defmodule MyLib do
  defmacro __using__(_opts) do
    quote do
      def zero_arity, do: :ok
      def one_arity(x), do: x
      def two_arity(x, y), do: x + y
      def bitstring_param(<<header::binary-size(4), rest::binary>>), do: {header, rest}
      defmacro my_macro(ast), do: ast
    end
  end
end`
	_, inlineDefs, _, _, _ := parseUsingBody(text)

	check := func(name string, wantArity int, wantKind string) {
		t.Helper()
		defs, ok := inlineDefs[name]
		if !ok || len(defs) == 0 {
			t.Errorf("%s: not found in inline defs", name)
			return
		}
		if defs[0].arity != wantArity {
			t.Errorf("%s: arity=%d, want %d", name, defs[0].arity, wantArity)
		}
		if defs[0].kind != wantKind {
			t.Errorf("%s: kind=%q, want %q", name, defs[0].kind, wantKind)
		}
	}

	check("zero_arity", 0, "def")
	check("one_arity", 1, "def")
	check("two_arity", 2, "def")
	check("bitstring_param", 1, "def")
	check("my_macro", 1, "defmacro")
}

func TestParseUsingBody_SkipsUnquoteUse(t *testing.T) {
	text := `defmodule Remote.Oban.Worker do
  defmacro __using__(opts) do
    {oban_module, opts} = Keyword.pop(opts, :oban_module, Oban.Worker)

    quote do
      use unquote(oban_module), unquote(opts)
    end
  end
end`
	_, _, transUses, _, _ := parseUsingBody(text)
	for _, u := range transUses {
		if u == "unquote" {
			t.Error("transUses should not contain 'unquote'")
		}
	}
}

func TestParseUsingBody_KeywordModuleHints(t *testing.T) {
	t.Run("Keyword.put_new adds module as transitive use", func(t *testing.T) {
		text := `defmodule Remote.Oban.Pro.Worker do
  defmacro __using__(opts) do
    opts = Keyword.put_new(opts, :oban_module, Oban.Pro.Worker)

    quote do
      use Remote.Oban.Worker, unquote(opts)
    end
  end
end`
		_, _, transUses, _, _ := parseUsingBody(text)
		found := false
		for _, u := range transUses {
			if u == "Oban.Pro.Worker" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected Oban.Pro.Worker in transUses, got %v", transUses)
		}
	})

	t.Run("Keyword.pop default adds module as opt binding", func(t *testing.T) {
		text := `defmodule MyLib do
  defmacro __using__(opts) do
    {mod, opts} = Keyword.pop(opts, :base_module, MyLib.DefaultBase)

    quote do
      use unquote(mod), unquote(opts)
    end
  end
end`
		_, _, _, optBindings, _ := parseUsingBody(text)
		found := false
		for _, b := range optBindings {
			if b.optKey == "base_module" && b.defaultMod == "MyLib.DefaultBase" && b.kind == "use" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected base_module opt binding with MyLib.DefaultBase, got %v", optBindings)
		}
	})

	t.Run("ignores non-module Keyword defaults", func(t *testing.T) {
		text := `defmodule MyLib do
  defmacro __using__(opts) do
    {flag, opts} = Keyword.pop(opts, :debug, false)

    quote do
      use MyLib.Base, unquote(opts)
    end
  end
end`
		_, _, transUses, _, _ := parseUsingBody(text)
		for _, u := range transUses {
			if u == "false" {
				t.Error("transUses should not contain 'false'")
			}
		}
	})
}

func TestParseUsingBody_CaseTemplateUsing(t *testing.T) {
	t.Run("using do form with inline imports", func(t *testing.T) {
		text := `defmodule MyApp.ConnCase do
  use ExUnit.CaseTemplate

  using do
    quote do
      import Phoenix.ConnTest
      import MyApp.Helpers
    end
  end
end
`
		imported, _, _, _, _ := parseUsingBody(text)
		foundConn, foundHelpers := false, false
		for _, imp := range imported {
			if imp == "Phoenix.ConnTest" {
				foundConn = true
			}
			if imp == "MyApp.Helpers" {
				foundHelpers = true
			}
		}
		if !foundConn {
			t.Error("expected Phoenix.ConnTest in imports")
		}
		if !foundHelpers {
			t.Error("expected MyApp.Helpers in imports")
		}
	})

	t.Run("using opts do form delegating to helper function", func(t *testing.T) {
		// Mirrors MyAppWeb.ConnCase: using opts do / using_block(opts) / end
		// with using_block defined as a separate def that returns a quote do block
		text := `defmodule MyAppWeb.ConnCase do
  use ExUnit.CaseTemplate

  def using_block(_opts) do
    quote do
      import Phoenix.ConnTest
      import Plug.Conn
      use MyAppWeb.VerifiedRoutes
    end
  end

  using opts do
    using_block(opts)
  end
end`
		imported, _, transUses, _, _ := parseUsingBody(text)

		foundConn, foundPlug := false, false
		for _, imp := range imported {
			if imp == "Phoenix.ConnTest" {
				foundConn = true
			}
			if imp == "Plug.Conn" {
				foundPlug = true
			}
		}
		if !foundConn {
			t.Errorf("expected Phoenix.ConnTest in imports (via helper), got %v", imported)
		}
		if !foundPlug {
			t.Errorf("expected Plug.Conn in imports (via helper), got %v", imported)
		}

		foundRoutes := false
		for _, u := range transUses {
			if u == "MyAppWeb.VerifiedRoutes" {
				foundRoutes = true
			}
		}
		if !foundRoutes {
			t.Errorf("expected MyAppWeb.VerifiedRoutes in transUses (via helper), got %v", transUses)
		}
	})

	t.Run("using without ExUnit.CaseTemplate does not trigger", func(t *testing.T) {
		// `using` is a common Elixir keyword/macro — should not be treated as
		// __using__ unless the module explicitly uses ExUnit.CaseTemplate
		text := `defmodule MyApp.Schema do
  using MyField do
    :ok
  end
end`
		imported, _, _, _, _ := parseUsingBody(text)
		if len(imported) != 0 {
			t.Errorf("expected no imports for non-CaseTemplate using, got %v", imported)
		}
	})
}

func TestParseUsingBody_UnquoteImport(t *testing.T) {
	t.Run("import unquote(mod) with Keyword.get default", func(t *testing.T) {
		// Remote.Mox pattern: `mod = Keyword.get(opts, :mod, Mox)` + `import unquote(mod)`
		text := `defmodule Remote.Mox do
  defmacro __using__(opts \\ []) do
    mod = Keyword.get(opts, :mod, Mox)
    quote do
      import unquote(mod)
    end
  end
end`
		imported, _, _, optBindings, _ := parseUsingBody(text)
		// Dynamic unquote imports should NOT be in static imports
		for _, imp := range imported {
			if imp == "Mox" {
				t.Errorf("Mox should not be in static imports (it's a dynamic opt binding)")
			}
		}
		_ = imported
		// Should have an opt binding for override
		if len(optBindings) == 0 {
			t.Fatal("expected at least one opt binding")
		}
		b := optBindings[0]
		if b.optKey != "mod" {
			t.Errorf("optKey: want 'mod', got %q", b.optKey)
		}
		if b.defaultMod != "Mox" {
			t.Errorf("defaultMod: want 'Mox', got %q", b.defaultMod)
		}
		if b.kind != "import" {
			t.Errorf("kind: want 'import', got %q", b.kind)
		}
	})

	t.Run("consumer opts override used in lookup", func(t *testing.T) {
		// When consumer passes `use Remote.Mox, mod: Hammox`, the import should be Hammox
		text := `defmodule Remote.Mox do
  defmacro __using__(opts \\ []) do
    mod = Keyword.get(opts, :mod, Mox)
    quote do
      import unquote(mod)
    end
  end
end`
		_, _, _, optBindings, _ := parseUsingBody(text)
		if len(optBindings) == 0 {
			t.Fatal("expected opt binding")
		}
		// With consumer opts {mod: Hammox}, the effective import should be Hammox
		consumerOpts := map[string]string{"mod": "Hammox"}
		effectiveMod := consumerOpts[optBindings[0].optKey]
		if effectiveMod != "Hammox" {
			t.Errorf("consumer override: want 'Hammox', got %q", effectiveMod)
		}
		// Without consumer opts, should fall back to default
		if optBindings[0].defaultMod != "Mox" {
			t.Errorf("default: want 'Mox', got %q", optBindings[0].defaultMod)
		}
	})

	t.Run("use unquote(mod) with Keyword.get default", func(t *testing.T) {
		text := `defmodule MyLib do
  defmacro __using__(opts \\ []) do
    base = Keyword.get(opts, :base, MyLib.Base)
    quote do
      use unquote(base)
    end
  end
end`
		_, _, transUses, optBindings, _ := parseUsingBody(text)
		// Dynamic unquote uses should NOT be in static transUses
		for _, u := range transUses {
			if u == "MyLib.Base" {
				t.Errorf("MyLib.Base should not be in static transUses (it's a dynamic opt binding)")
			}
		}
		_ = transUses
		if len(optBindings) == 0 || optBindings[0].kind != "use" {
			t.Errorf("expected a 'use' opt binding, got %v", optBindings)
		}
	})
}

func TestParseUsingBody_Aliases(t *testing.T) {
	t.Run("simple alias", func(t *testing.T) {
		text := `defmodule MyApp.Schema do
  defmacro __using__(_opts) do
    quote do
      alias MyApp.Repo
      alias MyApp.Accounts.User
      import Ecto.Query
    end
  end
end`
		_, _, _, _, aliases := parseUsingBody(text)
		if aliases == nil {
			t.Fatal("expected aliases, got nil")
		}
		if aliases["Repo"] != "MyApp.Repo" {
			t.Errorf("Repo: got %q, want MyApp.Repo", aliases["Repo"])
		}
		if aliases["User"] != "MyApp.Accounts.User" {
			t.Errorf("User: got %q, want MyApp.Accounts.User", aliases["User"])
		}
	})

	t.Run("alias with as:", func(t *testing.T) {
		text := `defmodule MyApp.Schema do
  defmacro __using__(_opts) do
    quote do
      alias MyApp.Accounts.UserProfile, as: Profile
    end
  end
end`
		_, _, _, _, aliases := parseUsingBody(text)
		if aliases == nil {
			t.Fatal("expected aliases, got nil")
		}
		if aliases["Profile"] != "MyApp.Accounts.UserProfile" {
			t.Errorf("Profile: got %q, want MyApp.Accounts.UserProfile", aliases["Profile"])
		}
	})

	t.Run("multi alias", func(t *testing.T) {
		text := `defmodule MyApp.Schema do
  defmacro __using__(_opts) do
    quote do
      alias MyApp.{Repo, Config, Helper}
    end
  end
end`
		_, _, _, _, aliases := parseUsingBody(text)
		if aliases == nil {
			t.Fatal("expected aliases, got nil")
		}
		if aliases["Repo"] != "MyApp.Repo" {
			t.Errorf("Repo: got %q, want MyApp.Repo", aliases["Repo"])
		}
		if aliases["Config"] != "MyApp.Config" {
			t.Errorf("Config: got %q, want MyApp.Config", aliases["Config"])
		}
		if aliases["Helper"] != "MyApp.Helper" {
			t.Errorf("Helper: got %q, want MyApp.Helper", aliases["Helper"])
		}
	})

	t.Run("alias resolved through file-level alias", func(t *testing.T) {
		text := `defmodule MyApp.Schema do
  alias Remote.Ecto.Schema, as: EctoSchema

  defmacro __using__(_opts) do
    quote do
      alias EctoSchema.Fields
    end
  end
end`
		_, _, _, _, aliases := parseUsingBody(text)
		if aliases == nil {
			t.Fatal("expected aliases, got nil")
		}
		if aliases["Fields"] != "Remote.Ecto.Schema.Fields" {
			t.Errorf("Fields: got %q, want Remote.Ecto.Schema.Fields", aliases["Fields"])
		}
	})

	t.Run("no __using__ returns nil aliases", func(t *testing.T) {
		text := "defmodule Lib do\n  def foo, do: :ok\nend"
		_, _, _, _, aliases := parseUsingBody(text)
		if aliases != nil {
			t.Errorf("expected nil aliases, got %v", aliases)
		}
	})
}

func TestParseUsingBody_HeredocModuledoc(t *testing.T) {
	// Regression: moduledocs with code examples containing brackets that span
	// multiple lines (e.g. multi-line keyword lists, markdown links) must not
	// confuse the parser. Line-based joinBracketLines treats heredoc content as
	// code, causing unmatched [ or ( on one line to join with all subsequent
	// lines until the bracket closes — potentially swallowing defmacro __using__.
	t.Run("import inside __using__ survives moduledoc with brackets", func(t *testing.T) {
		text := `defmodule SharedLib.Pro.Workers.Chunk do
  @moduledoc """
  Chunk workers execute jobs in groups based on a size or timeout option.

  ## Usage

      defmodule MyApp.ChunkWorker do
        use SharedLib.Pro.Workers.Chunk, queue: :messages, size: 100
      end

  ## Options

  Options are passed as a keyword list:

      [
        by: :worker,
        size: 100,
        timeout: 1000
      ]

  The [return values](#t:result/0) are different from standard workers.

  See [the documentation](#module-options) for more details.
  """

  @type options :: [
          by: atom(),
          size: pos_integer(),
          timeout: pos_integer()
        ]

  @doc false
  defmacro __using__(opts) do
    {chunk_opts, other_opts} = Keyword.split(opts, [:by, :size, :timeout])

    quote do
      use SharedLib.Pro.Worker, unquote(other_opts)

      alias SharedLib.Pro.Workers.Chunk

      @impl SharedLib.Worker
      def new(args, opts) when is_map(args) and is_list(opts) do
        super(args, opts)
      end

      @impl SharedLib.Worker
      def perform(%Job{} = job) do
        :ok
      end
    end
  end
end`
		imports, inlineDefs, transUses, _, _ := parseUsingBody(text)
		// The __using__ body has "use SharedLib.Pro.Worker" — should appear in transUses
		found := false
		for _, u := range transUses {
			if u == "SharedLib.Pro.Worker" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected SharedLib.Pro.Worker in transUses, got %v", transUses)
		}
		// Inline defs: new/2, perform/1
		if _, ok := inlineDefs["new"]; !ok {
			t.Errorf("expected 'new' in inlineDefs, got keys: %v", mapKeys(inlineDefs))
		}
		if _, ok := inlineDefs["perform"]; !ok {
			t.Errorf("expected 'perform' in inlineDefs, got keys: %v", mapKeys(inlineDefs))
		}
		_ = imports
	})

	t.Run("full chain: import through __using__ with long moduledoc", func(t *testing.T) {
		text := `defmodule SharedLib.Pro.Worker do
  @moduledoc """
  The SharedLib.Pro.Worker is a replacement for SharedLib.Worker with expanded
  capabilities such as encryption and output recording.

  ## Usage

      def MyApp.Worker do
        use SharedLib.Pro.Worker

        @impl SharedLib.Pro.Worker
        def process(%Job{} = job) do
          :ok
        end
      end

  ## Encryption

  Workers can be encrypted by passing the ` + "`:encrypted`" + ` option:

      use SharedLib.Pro.Worker,
        encrypted: [key: {MyApp.Config, :secret_key}]

  ## Hooks

  Lifecycle hooks are declared with the ` + "`:hooks`" + ` option:

      use SharedLib.Pro.Worker,
        hooks: [
          on_start: &MyApp.Telemetry.worker_started/1,
          on_complete: &MyApp.Telemetry.worker_completed/1
        ]
  """

  defmacro __using__(opts) do
    {_hook_opts, other_opts} = Keyword.split(opts, [:hooks, :encrypted])

    quote do
      @behaviour SharedLib.Worker
      @behaviour SharedLib.Pro.Worker

      import SharedLib.Pro.Worker,
        only: [
          args_schema: 1,
          field: 2,
          field: 3,
          embeds_one: 2,
          embeds_one: 3
        ]

      alias SharedLib.{Job, Worker}

      def __opts__, do: unquote(other_opts)
    end
  end

  defmacro args_schema(do: block) do
    quote do
      Module.register_attribute(__MODULE__, :args_fields, accumulate: true)
      unquote(block)
    end
  end

  defmacro field(name, type, opts \\ []) do
    quote do
      @args_fields {unquote(name), unquote(type), unquote(opts)}
    end
  end
end`
		imports, inlineDefs, _, _, aliases := parseUsingBody(text)
		// Should find the import
		found := false
		for _, imp := range imports {
			if imp == "SharedLib.Pro.Worker" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected SharedLib.Pro.Worker in imports, got %v", imports)
		}
		// Should find inline def __opts__
		if _, ok := inlineDefs["__opts__"]; !ok {
			t.Errorf("expected '__opts__' in inlineDefs, got keys: %v", mapKeys(inlineDefs))
		}
		// Should find aliases
		if aliases == nil || aliases["Job"] != "SharedLib.Job" {
			t.Errorf("expected alias Job -> SharedLib.Job, got %v", aliases)
		}
	})
}

func mapKeys[V any](m map[string][]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestExtractUsesWithOpts(t *testing.T) {
	t.Run("no opts", func(t *testing.T) {
		text := "defmodule Foo do\n  use Remote.Mox\nend"
		calls := ExtractUsesWithOpts(text, nil)
		if len(calls) != 1 || calls[0].Module != "Remote.Mox" {
			t.Errorf("expected [Remote.Mox], got %v", calls)
		}
		if len(calls[0].Opts) != 0 {
			t.Errorf("expected no opts, got %v", calls[0].Opts)
		}
	})

	t.Run("with module opt", func(t *testing.T) {
		text := "defmodule Foo do\n  use Remote.Mox, mod: Hammox\nend"
		calls := ExtractUsesWithOpts(text, nil)
		if len(calls) != 1 {
			t.Fatalf("expected 1 use call, got %d", len(calls))
		}
		if calls[0].Opts["mod"] != "Hammox" {
			t.Errorf("mod opt: want 'Hammox', got %q", calls[0].Opts["mod"])
		}
	})

	t.Run("multiple opts", func(t *testing.T) {
		text := "defmodule Foo do\n  use MyLib, mod: Hammox, repo: MyRepo\nend"
		calls := ExtractUsesWithOpts(text, nil)
		if len(calls) != 1 {
			t.Fatalf("expected 1 use call, got %d", len(calls))
		}
		if calls[0].Opts["mod"] != "Hammox" {
			t.Errorf("mod: want Hammox, got %q", calls[0].Opts["mod"])
		}
		if calls[0].Opts["repo"] != "MyRepo" {
			t.Errorf("repo: want MyRepo, got %q", calls[0].Opts["repo"])
		}
	})

	t.Run("aliases resolved for module opts", func(t *testing.T) {
		aliases := map[string]string{"Hammox": "MyApp.Hammox"}
		text := "defmodule Foo do\n  use Remote.Mox, mod: Hammox\nend"
		calls := ExtractUsesWithOpts(text, aliases)
		if calls[0].Opts["mod"] != "MyApp.Hammox" {
			t.Errorf("alias not resolved: got %q", calls[0].Opts["mod"])
		}
	})

	t.Run("multiline opts", func(t *testing.T) {
		text := "defmodule Foo do\n  use Tool,\n    name: \"mock\",\n    controller: CompanyController,\n    action: :show\nend"
		calls := ExtractUsesWithOpts(text, nil)
		if len(calls) != 1 {
			t.Fatalf("expected 1 use call, got %d", len(calls))
		}
		if calls[0].Module != "Tool" {
			t.Errorf("module: want Tool, got %q", calls[0].Module)
		}
		if calls[0].Opts["controller"] != "CompanyController" {
			t.Errorf("controller: want CompanyController, got %q", calls[0].Opts["controller"])
		}
	})

	t.Run("multiline opts with module values", func(t *testing.T) {
		text := "defmodule Foo do\n  use Remote.Mox,\n    mod: Hammox,\n    repo: MyRepo\nend"
		calls := ExtractUsesWithOpts(text, nil)
		if len(calls) != 1 {
			t.Fatalf("expected 1 use call, got %d", len(calls))
		}
		if calls[0].Opts["mod"] != "Hammox" {
			t.Errorf("mod: want Hammox, got %q", calls[0].Opts["mod"])
		}
		if calls[0].Opts["repo"] != "MyRepo" {
			t.Errorf("repo: want MyRepo, got %q", calls[0].Opts["repo"])
		}
	})
}

func TestFindBufferFunctions(t *testing.T) {
	text := `defmodule Foo do
  def public_one(a) do
    :ok
  end

  def public_two(b) do
    :ok
  end

  defp private_func(x) do
    x
  end

  defmacro my_macro(expr) do
    quote do: unquote(expr)
  end

  defguard is_admin(user) when user.role == :admin

  defdelegate fetch(id), to: MyApp.Repo

  def public_one(a, b) do
    :ok
  end
end`

	results := FindBufferFunctions(text)

	t.Run("deduplicates same name and arity", func(t *testing.T) {
		// public_one/1 and public_one/2 are different, so both should appear
		count := 0
		for _, r := range results {
			if r.Name == "public_one" {
				count++
			}
		}
		if count != 2 {
			t.Errorf("expected public_one twice (arity 1 and 2), got %d times", count)
		}
	})

	t.Run("finds all unique functions", func(t *testing.T) {
		if len(results) != 7 {
			t.Fatalf("expected 7 unique function/arity combos, got %d", len(results))
		}
	})

	t.Run("preserves kind", func(t *testing.T) {
		for _, r := range results {
			if r.Name == "my_macro" && r.Kind != "defmacro" {
				t.Errorf("expected defmacro kind for my_macro, got %q", r.Kind)
			}
			if r.Name == "private_func" && r.Kind != "defp" {
				t.Errorf("expected defp kind for private_func, got %q", r.Kind)
			}
		}
	})

	t.Run("empty buffer", func(t *testing.T) {
		results := FindBufferFunctions("")
		if len(results) != 0 {
			t.Errorf("expected 0 results for empty buffer, got %d", len(results))
		}
	})
}

func TestExtractCallContext(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		line, col  int
		wantExpr   string
		wantArgIdx int
		wantOK     bool
	}{
		{
			name:       "simple call first arg",
			text:       "foo(x, y)",
			line:       0,
			col:        4,
			wantExpr:   "foo",
			wantArgIdx: 0,
			wantOK:     true,
		},
		{
			name:       "simple call second arg",
			text:       "foo(x, y)",
			line:       0,
			col:        7,
			wantExpr:   "foo",
			wantArgIdx: 1,
			wantOK:     true,
		},
		{
			name:       "qualified call",
			text:       "Enum.map(list, fun)",
			line:       0,
			col:        15,
			wantExpr:   "Enum.map",
			wantArgIdx: 1,
			wantOK:     true,
		},
		{
			name:       "nested call finds inner",
			text:       "Enum.map(list, fn x -> String.upcase(x) end)",
			line:       0,
			col:        37,
			wantExpr:   "String.upcase",
			wantArgIdx: 0,
			wantOK:     true,
		},
		{
			name:       "multi-line",
			text:       "defmodule MyApp do\n  def run do\n    foo(x,\n      y)\n  end\nend",
			line:       3,
			col:        6,
			wantExpr:   "foo",
			wantArgIdx: 1,
			wantOK:     true,
		},
		{
			name:       "not in call",
			text:       "x = 1",
			line:       0,
			col:        0,
			wantExpr:   "",
			wantArgIdx: 0,
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, argIdx, ok := ExtractCallContext(tt.text, tt.line, tt.col)
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if expr != tt.wantExpr {
				t.Errorf("expr = %q, want %q", expr, tt.wantExpr)
			}
			if argIdx != tt.wantArgIdx {
				t.Errorf("argIdx = %d, want %d", argIdx, tt.wantArgIdx)
			}
		})
	}
}

func TestFindBareFunctionCalls(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		funcName string
		want     []int
	}{
		{
			name:     "simple call",
			text:     "def foo do\n  bar(x)\nend",
			funcName: "bar",
			want:     []int{2},
		},
		{
			name:     "keyword key shadows call on same line",
			text:     "def foo do\n  %{resource_type: resource_type(x)}\nend",
			funcName: "resource_type",
			want:     []int{2},
		},
		{
			name:     "keyword key only, no call",
			text:     "def foo do\n  %{resource_type: :payroll}\nend",
			funcName: "resource_type",
			want:     nil,
		},
		{
			name:     "pipe call",
			text:     "def foo(x) do\n  x |> bar()\nend",
			funcName: "bar",
			want:     []int{2},
		},
		{
			name:     "definition line excluded",
			text:     "defp resource_type(%Foo{}), do: \"foo\"\ndefp resource_type(%Bar{}), do: \"bar\"",
			funcName: "resource_type",
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindBareFunctionCalls(tt.text, tt.funcName)
			if len(got) != len(tt.want) {
				t.Fatalf("FindBareFunctionCalls(%q) = %v, want %v", tt.funcName, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("FindBareFunctionCalls(%q)[%d] = %d, want %d", tt.funcName, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestExtractParamNames(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		expected []string
	}{
		{
			name:     "simple params",
			line:     "  def create(attrs, opts) do",
			expected: []string{"attrs", "opts"},
		},
		{
			name:     "default param",
			line:     `  def fetch(slug, opts \\ []) do`,
			expected: []string{"slug", "opts"},
		},
		{
			name:     "pattern match param",
			line:     "  def process(%{name: name}, data) do",
			expected: []string{"arg1", "data"},
		},
		{
			name:     "no params",
			line:     "  def run do",
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := []string{tt.line}
			got := extractParamNames(lines, 0)
			if len(got) != len(tt.expected) {
				t.Fatalf("got %v, want %v", got, tt.expected)
			}
			for i := range got {
				if got[i] != tt.expected[i] {
					t.Errorf("param %d: got %q, want %q", i, got[i], tt.expected[i])
				}
			}
		})
	}
}

func TestExtractAliasesInScope_AliasInString(t *testing.T) {
	text := `defmodule MyApp.Foo do
  def bar do
    x = "alias MyApp.Helpers, as: H"
    H.help()
  end
end`
	aliases := ExtractAliasesInScope(text, 3)
	if _, ok := aliases["H"]; ok {
		t.Error("should not extract alias from string content")
	}
}

func TestExtractAliasesInScope_AliasInHeredoc(t *testing.T) {
	text := `defmodule MyApp.Foo do
  @doc """
  alias MyApp.Helpers, as: H
  """
  def bar do
    H.help()
  end
end`
	aliases := ExtractAliasesInScope(text, 5)
	if _, ok := aliases["H"]; ok {
		t.Error("should not extract alias from heredoc content")
	}
}

func TestExtractAliasesInScope_MultilineAliasWithComment(t *testing.T) {
	text := `defmodule MyApp.Foo do
  alias MyApp.Helpers.Paginator,
    # Short name for convenience
    as: Pages

  def bar, do: Pages.paginate()
end`
	aliases := ExtractAliasesInScope(text, 5)
	if aliases["Pages"] != "MyApp.Helpers.Paginator" {
		t.Errorf("expected Pages -> MyApp.Helpers.Paginator, got %q", aliases["Pages"])
	}
}

func TestExtractAliasesInScope_NestedModuleScope(t *testing.T) {
	text := `defmodule MyApp.Outer do
  alias MyApp.Helpers

  defmodule Inner do
    def bar, do: Helpers.help()
  end
end`
	outerAliases := ExtractAliasesInScope(text, 1)
	innerAliases := ExtractAliasesInScope(text, 4)

	if outerAliases["Helpers"] != "MyApp.Helpers" {
		t.Error("outer module should have the alias")
	}
	if _, ok := innerAliases["Helpers"]; ok {
		t.Error("inner module should NOT inherit outer alias")
	}
}

func TestExtractAliasesInScope_MultilineBlockTrailingComma(t *testing.T) {
	text := `defmodule MyApp.Web do
  alias MyApp.{
    Accounts,
    Users,
  }

  def foo, do: Accounts.list()
end`
	aliases := ExtractAliasesInScope(text, 6)
	if aliases["Accounts"] != "MyApp.Accounts" {
		t.Errorf("Accounts: got %q, want MyApp.Accounts", aliases["Accounts"])
	}
	if aliases["Users"] != "MyApp.Users" {
		t.Errorf("Users: got %q, want MyApp.Users", aliases["Users"])
	}
}

func TestExtractUsesWithOpts_StringContent(t *testing.T) {
	text := `defmodule MyApp.Foo do
  def bar do
    x = "use Tool,"
    y = "name: mock"
  end
end`
	calls := ExtractUsesWithOpts(text, nil)
	for _, c := range calls {
		if c.Module == "Tool" {
			t.Error("should not extract use from string content")
		}
	}
}

func TestExtractAliasBlockParent_NotConfusedByMapBraces(t *testing.T) {
	lines := strings.Split(`defmodule MyApp.Foo do
  def bar do
    map = %{
      key: "value"
    }
  end
end`, "\n")
	_, inBlock := ExtractAliasBlockParent(lines, 3)
	if inBlock {
		t.Error("map literal brace should not be detected as alias block")
	}
}
