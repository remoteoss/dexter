package lsp

import (
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
		imports, _, _, _ := parseUsingBody(text)
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
		imports, _, _, _ := parseUsingBody(text)
		if len(imports) != 1 || imports[0] != "Foo" {
			t.Errorf("expected [Foo], got %v", imports)
		}
	})

	t.Run("no __using__ returns nil", func(t *testing.T) {
		text := "defmodule Lib do\n  def foo, do: :ok\nend"
		imports, _, _, _ := parseUsingBody(text)
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
		_, defs, _, _ := parseUsingBody(text)
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
      defmacro my_macro(ast), do: ast
    end
  end
end`
	_, inlineDefs, _, _ := parseUsingBody(text)

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
	_, _, transUses, _ := parseUsingBody(text)
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
		_, _, transUses, _ := parseUsingBody(text)
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
		_, _, _, optBindings := parseUsingBody(text)
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
		_, _, transUses, _ := parseUsingBody(text)
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
		imported, _, _, _ := parseUsingBody(text)
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
		imported, _, transUses, _ := parseUsingBody(text)

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
		imported, _, _, _ := parseUsingBody(text)
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
		imported, _, _, optBindings := parseUsingBody(text)
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
		_, _, _, optBindings := parseUsingBody(text)
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
		_, _, transUses, optBindings := parseUsingBody(text)
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

func TestParseKeywordModuleOpts(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		aliases  map[string]string
		expected map[string]string
	}{
		{
			name:     "single module opt",
			input:    "mod: Hammox",
			expected: map[string]string{"mod": "Hammox"},
		},
		{
			name:     "multiple module opts",
			input:    "mod: Hammox, repo: MyRepo",
			expected: map[string]string{"mod": "Hammox", "repo": "MyRepo"},
		},
		{
			name:     "alias resolved",
			input:    "mod: Hammox",
			aliases:  map[string]string{"Hammox": "MyApp.Hammox"},
			expected: map[string]string{"mod": "MyApp.Hammox"},
		},
		{
			name:     "non-module values ignored",
			input:    "mod: Hammox, async: true, queue: :default",
			expected: map[string]string{"mod": "Hammox"},
		},
		{
			name:     "empty string",
			input:    "",
			expected: map[string]string{},
		},
		{
			name:     "dotted module name",
			input:    "repo: MyApp.Oban.Repo",
			expected: map[string]string{"repo": "MyApp.Oban.Repo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseKeywordModuleOpts(tt.input, tt.aliases)
			if len(got) != len(tt.expected) {
				t.Fatalf("ParseKeywordModuleOpts(%q) = %v, want %v", tt.input, got, tt.expected)
			}
			for k, v := range tt.expected {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
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
