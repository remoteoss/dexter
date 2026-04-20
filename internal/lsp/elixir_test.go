package lsp

import (
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
)

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

func tokenize(code string) ([]parser.Token, []byte, []int) {
	source := []byte(code)
	result := parser.TokenizeFull(source)
	return result.Tokens, source, result.LineStarts
}

func TestExpressionAtCursor(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		line     int
		col      int
		wantMod  string
		wantFunc string
	}{
		{
			name:     "cursor on middle module segment",
			code:     "    Foo.Bar.baz(123)",
			line:     0,
			col:      9, // 'a' in Bar
			wantMod:  "Foo.Bar",
			wantFunc: "",
		},
		{
			name:     "cursor on function name",
			code:     "    Foo.Bar.baz(123)",
			line:     0,
			col:      12, // 'b' in baz
			wantMod:  "Foo.Bar",
			wantFunc: "baz",
		},
		{
			name:     "cursor on first module segment",
			code:     "    Foo.bar()",
			line:     0,
			col:      4, // 'F' in Foo
			wantMod:  "Foo",
			wantFunc: "",
		},
		{
			name:     "bare function call",
			code:     "    do_something(x)",
			line:     0,
			col:      7,
			wantMod:  "",
			wantFunc: "do_something",
		},
		{
			name:     "cursor on dot includes next segment",
			code:     "    Foo.Bar.Baz",
			line:     0,
			col:      7, // the dot between Foo and Bar
			wantMod:  "Foo.Bar",
			wantFunc: "",
		},
		{
			name:     "three-part cursor on last",
			code:     "MyApp.Repo.all",
			line:     0,
			col:      11, // 'a' in all
			wantMod:  "MyApp.Repo",
			wantFunc: "all",
		},
		{
			name:     "three-part cursor on middle",
			code:     "MyApp.Repo.all",
			line:     0,
			col:      7, // 'e' in Repo
			wantMod:  "MyApp.Repo",
			wantFunc: "",
		},
		{
			name:     "three-part cursor on first",
			code:     "MyApp.Repo.all",
			line:     0,
			col:      2, // 'A' in MyApp
			wantMod:  "MyApp",
			wantFunc: "",
		},
		{
			name:     "function with question mark",
			code:     "    valid?(x)",
			line:     0,
			col:      6,
			wantMod:  "",
			wantFunc: "valid?",
		},
		{
			name:     "function with bang",
			code:     "    process!(x)",
			line:     0,
			col:      6,
			wantMod:  "",
			wantFunc: "process!",
		},
		{
			name:     "empty line",
			code:     "",
			line:     0,
			col:      0,
			wantMod:  "",
			wantFunc: "",
		},
		{
			name:     "cursor on paren",
			code:     "    Foo.bar()",
			line:     0,
			col:      11, // the open paren
			wantMod:  "",
			wantFunc: "",
		},
		// --- Token-aware improvements over char-based version ---
		{
			name:     "expression inside string is ignored",
			code:     `x = "Foo.bar"`,
			line:     0,
			col:      7, // 'o' in Foo inside the string
			wantMod:  "",
			wantFunc: "",
		},
		{
			name:     "expression inside comment is ignored",
			code:     "  # Foo.bar is great",
			line:     0,
			col:      6, // 'o' in Foo inside comment
			wantMod:  "",
			wantFunc: "",
		},
		{
			name:     "expression inside heredoc is ignored",
			code:     "  \"\"\"\n  Foo.bar\n  \"\"\"",
			line:     1,
			col:      4, // 'o' in Foo inside heredoc
			wantMod:  "",
			wantFunc: "",
		},
		{
			name:     "multiline: cursor on second line",
			code:     "defmodule MyApp do\n  Foo.Bar.baz()\nend",
			line:     1,
			col:      6, // 'B' in Bar
			wantMod:  "Foo.Bar",
			wantFunc: "",
		},
		{
			name:     "module-only expression",
			code:     "  Foo.Bar.Baz",
			line:     0,
			col:      10, // 'B' in Baz
			wantMod:  "Foo.Bar.Baz",
			wantFunc: "",
		},
		{
			name:     "pipe into qualified call",
			code:     "    |> Foo.Bar.transform()",
			line:     0,
			col:      15, // 't' in transform
			wantMod:  "Foo.Bar",
			wantFunc: "transform",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens, source, lineStarts := tokenize(tt.code)
			ctx := ExpressionAtCursor(tokens, source, lineStarts, tt.line, tt.col)
			if ctx.ModuleRef != tt.wantMod {
				t.Errorf("ModuleRef = %q, want %q", ctx.ModuleRef, tt.wantMod)
			}
			if ctx.FunctionName != tt.wantFunc {
				t.Errorf("FunctionName = %q, want %q", ctx.FunctionName, tt.wantFunc)
			}
		})
	}
}

func TestFullExpressionAtCursor(t *testing.T) {
	code := "    Foo.Bar.baz(123)"
	tokens, source, lineStarts := tokenize(code)

	// Cursor on Foo — full returns entire chain
	ctx := FullExpressionAtCursor(tokens, source, lineStarts, 0, 5)
	if ctx.ModuleRef != "Foo.Bar" {
		t.Errorf("ModuleRef = %q, want %q", ctx.ModuleRef, "Foo.Bar")
	}
	if ctx.FunctionName != "baz" {
		t.Errorf("FunctionName = %q, want %q", ctx.FunctionName, "baz")
	}

	// Truncated version should only return Foo
	ctx2 := ExpressionAtCursor(tokens, source, lineStarts, 0, 5)
	if ctx2.ModuleRef != "Foo" {
		t.Errorf("truncated ModuleRef = %q, want %q", ctx2.ModuleRef, "Foo")
	}
	if ctx2.FunctionName != "" {
		t.Errorf("truncated FunctionName = %q, want %q", ctx2.FunctionName, "")
	}
}

func TestExpressionAtCursor_ExprBounds(t *testing.T) {
	code := "    Foo.Bar.baz(123)"
	tokens, source, lineStarts := tokenize(code)

	// Cursor on baz: exprStart should be at Foo (col 4), exprEnd after baz
	ctx := ExpressionAtCursor(tokens, source, lineStarts, 0, 12)
	if ctx.ExprStart != 4 {
		t.Errorf("ExprStart = %d, want 4", ctx.ExprStart)
	}
	if ctx.ExprEnd != 15 {
		t.Errorf("ExprEnd = %d, want 15", ctx.ExprEnd)
	}

	// Cursor on Bar: exprStart at Foo (col 4), exprEnd after Bar
	ctx2 := ExpressionAtCursor(tokens, source, lineStarts, 0, 9)
	if ctx2.ExprStart != 4 {
		t.Errorf("ExprStart = %d, want 4", ctx2.ExprStart)
	}
	if ctx2.ExprEnd != 11 {
		t.Errorf("ExprEnd = %d, want 11", ctx2.ExprEnd)
	}
}

func TestCursorContext_Expr(t *testing.T) {
	tests := []struct {
		mod, fn, want string
	}{
		{"Foo.Bar", "baz", "Foo.Bar.baz"},
		{"Foo.Bar", "", "Foo.Bar"},
		{"", "baz", "baz"},
		{"", "", ""},
	}
	for _, tt := range tests {
		ctx := CursorContext{ModuleRef: tt.mod, FunctionName: tt.fn}
		if got := ctx.Expr(); got != tt.want {
			t.Errorf("CursorContext{%q, %q}.Expr() = %q, want %q", tt.mod, tt.fn, got, tt.want)
		}
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
	t.Run("missing close brace", func(t *testing.T) {
		text := `defmodule MyApp.Web do
  alias MyApp.Services.{
    Accounts,
  
  def foo do
    # missing close brace
  end
end`
		lines := strings.Split(text, "\n")
		// Unclosed `{`: user is still typing. We still want the parent for completion/hover
		// on lines inside the block, and the forward scan must not walk the whole file
		// looking for a `}` on the same line as `{` (regression guard for the line-bound
		// scan in ExtractAliasBlockParent).
		for _, line := range []int{2, 3} {
			parent, ok := ExtractAliasBlockParent(lines, line)
			if !ok || parent != "MyApp.Services" {
				t.Errorf("line %d: expected in block parent MyApp.Services, got %q, ok=%v", line, parent, ok)
			}
		}
		parent, ok := ExtractAliasBlockParent(lines, 0)
		if ok {
			t.Errorf("line 0 (defmodule): expected not in alias block, got parent %q", parent)
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

	t.Run("alias on same line as defmodule do is not skipped", func(t *testing.T) {
		// Regression: the for-loop post-increment skipped the first token after
		// processModuleDef returned. On a single-line defmodule + alias, the
		// alias token was missed.
		text := "defmodule MyApp.Web do alias MyApp.Accounts\nend"
		aliases := ExtractAliases(text)
		if aliases["Accounts"] != "MyApp.Accounts" {
			t.Errorf("Accounts: got %q, want MyApp.Accounts", aliases["Accounts"])
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

	t.Run("defmodule with do on next line keeps alias in inner scope", func(t *testing.T) {
		src := `defmodule MyApp.Outer do
  defmodule Inner
  do
    alias MyApp.InnerOnly
    def run, do: InnerOnly.call()
  end

  def outer_run do
    :ok
  end
end
`
		// Line 4 = inside Inner module body.
		innerAliases := ExtractAliasesInScope(src, 4)
		if innerAliases["InnerOnly"] != "MyApp.InnerOnly" {
			t.Errorf("expected InnerOnly alias in inner scope, got %q", innerAliases["InnerOnly"])
		}

		// Line 7 = inside Outer after Inner ends.
		outerAliases := ExtractAliasesInScope(src, 7)
		if _, ok := outerAliases["InnerOnly"]; ok {
			t.Error("InnerOnly alias should NOT leak to outer scope")
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
	t.Run("alias and require as on same line with semicolon", func(t *testing.T) {
		// Regression: after `alias Mod, as: Name` / `require Mod, as: Name`, the token
		// walker must resume at the value token (ScanKeywordOptionValue's nextPos) so the
		// for-loop post-increment does not skip the next statement on the same line.
		text := `defmodule MyApp.Outer do
  alias MyApp.Foo, as: MyFoo; alias MyApp.Bar, as: MyBar
  require MyApp.Baz, as: MyBaz; require MyApp.Qux, as: MyQux

  def call do
    MyFoo.run()
    MyBar.run()
    MyBaz.ok()
    MyQux.ok()
  end
end`
		// Line 4 is `def call do` — still inside Outer; aliases from lines 1–2 must be visible.
		aliases := ExtractAliasesInScope(text, 4)
		if aliases["MyFoo"] != "MyApp.Foo" {
			t.Errorf("MyFoo: got %q, want MyApp.Foo", aliases["MyFoo"])
		}
		if aliases["MyBar"] != "MyApp.Bar" {
			t.Errorf("MyBar: got %q, want MyApp.Bar", aliases["MyBar"])
		}
		if aliases["MyBaz"] != "MyApp.Baz" {
			t.Errorf("MyBaz: got %q, want MyApp.Baz", aliases["MyBaz"])
		}
		if aliases["MyQux"] != "MyApp.Qux" {
			t.Errorf("MyQux: got %q, want MyApp.Qux", aliases["MyQux"])
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

	tf := NewTokenizedFile(text)
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
			line, found := tf.FindFunctionDefinition(tt.functionName)
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

	tf := NewTokenizedFile(text)
	line, found := tf.FindFunctionDefinition("is_admin")
	if !found || line != 2 {
		t.Errorf("is_admin: got line %d found %v", line, found)
	}

	line, found = tf.FindFunctionDefinition("is_active")
	if !found || line != 3 {
		t.Errorf("is_active: got line %d found %v", line, found)
	}
}

func TestFindFunctionDefinition_Delegate(t *testing.T) {
	text := `defmodule Foo do
  defdelegate fetch(id), to: MyApp.Repo
end`

	tf := NewTokenizedFile(text)
	line, found := tf.FindFunctionDefinition("fetch")
	if !found || line != 2 {
		t.Errorf("fetch: got line %d found %v", line, found)
	}
}

func TestFindFunctionDefinition_InlineDo(t *testing.T) {
	text := `defmodule Foo do
  def add(a, b), do: a + b
  defp secret(x), do: x * 2
end`

	tf := NewTokenizedFile(text)
	line, found := tf.FindFunctionDefinition("add")
	if !found || line != 2 {
		t.Errorf("add: got line %d found %v", line, found)
	}
	line, found = tf.FindFunctionDefinition("secret")
	if !found || line != 3 {
		t.Errorf("secret: got line %d found %v", line, found)
	}
}

func TestExtractAliases_MultiAliasBraceUnexpectedTokenForwardProgress(t *testing.T) {
	text := `defmodule MyApp.Web do
  alias MyApp.{:unexpected, Accounts, 42, Users}
end`
	aliases := ExtractAliases(text)
	if aliases["Accounts"] != "MyApp.Accounts" {
		t.Errorf("Accounts: got %q, want MyApp.Accounts", aliases["Accounts"])
	}
	if aliases["Users"] != "MyApp.Users" {
		t.Errorf("Users: got %q, want MyApp.Users", aliases["Users"])
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

func TestModuleAttributeAtCursor(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		line     int
		col      int
		expected string
	}{
		{"cursor on attr name", "      tags: @open_api_shared_tags,", 0, 18, "open_api_shared_tags"},
		{"cursor on @", "      tags: @open_api_shared_tags,", 0, 12, "open_api_shared_tags"},
		{"cursor at end of attr", "      tags: @open_api_shared_tags,", 0, 31, "open_api_shared_tags"},
		{"not on attr", "      tags: :something,", 0, 10, ""},
		{"standalone attr", "  @endpoint_scopes %{", 0, 4, "endpoint_scopes"},
		{"inside string ignored", `  x = "has @fake_attr inside"`, 0, 14, ""},
		{"inside comment ignored", "  # @fake_attr comment", 0, 5, ""},
		{"multiline second line", "first_line\n  @my_attr value", 1, 5, "my_attr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tf := NewTokenizedFile(tt.text)
			got := tf.ModuleAttributeAtCursor(tt.line, tt.col)
			if got != tt.expected {
				t.Errorf("ModuleAttributeAtCursor(%d, %d) = %q, want %q", tt.line, tt.col, got, tt.expected)
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

	t.Run("does not treat attribute reference as definition", func(t *testing.T) {
		refText := `defmodule MyApp.Worker do
  def run(job) do
    process(@my_attr)
    @my_attr
    :ok
  end
end`

		_, found := FindModuleAttributeDefinition(refText, "my_attr")
		if found {
			t.Error("expected reference-only @my_attr to not be treated as a definition")
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

	t.Run("Keyword.fetch! and Keyword.pop! bindings", func(t *testing.T) {
		text := `defmodule MyLib do
  defmacro __using__(opts) do
    fetched = Keyword.fetch!(opts, :fetched_mod)
    {popped, opts} = Keyword.pop!(opts, :popped_mod, DefaultMod)

    quote do
      import unquote(fetched)
      use unquote(popped)
    end
  end
end`
		_, _, _, optBindings, _ := parseUsingBody(text)
		foundFetch := false
		foundPop := false
		for _, b := range optBindings {
			if b.optKey == "fetched_mod" && b.kind == "import" {
				foundFetch = true
				if b.defaultMod != "" {
					t.Errorf("fetch! should have no default, got %q", b.defaultMod)
				}
			}
			if b.optKey == "popped_mod" && b.kind == "use" {
				foundPop = true
				if b.defaultMod != "DefaultMod" {
					t.Errorf("pop! default: want DefaultMod, got %q", b.defaultMod)
				}
			}
		}
		if !foundFetch {
			t.Errorf("expected opt binding for fetched_mod (via fetch!), got %v", optBindings)
		}
		if !foundPop {
			t.Errorf("expected opt binding for popped_mod (via pop!), got %v", optBindings)
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

	t.Run("two alias as on one line in quote", func(t *testing.T) {
		// Same regression as ExtractAliasesInScope semicolon case, but through parseUsingBody
		// (use-chain / __using__ extraction uses a separate loop with the same nextPos rule).
		text := `defmodule MyApp.Schema do
  defmacro __using__(_opts) do
    quote do
      alias MyApp.Foo, as: MyFoo; alias MyApp.Bar, as: MyBar
    end
  end
end`
		_, _, _, _, aliases := parseUsingBody(text)
		if aliases == nil {
			t.Fatal("expected aliases, got nil")
		}
		if aliases["MyFoo"] != "MyApp.Foo" {
			t.Errorf("MyFoo: got %q, want MyApp.Foo", aliases["MyFoo"])
		}
		if aliases["MyBar"] != "MyApp.Bar" {
			t.Errorf("MyBar: got %q, want MyApp.Bar", aliases["MyBar"])
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

	t.Run("multi alias with unexpected tokens does not hang", func(t *testing.T) {
		text := `defmodule MyApp.Schema do
  defmacro __using__(_opts) do
    quote do
      alias MyApp.{:unexpected, Repo, 42}
    end
  end
end`
		_, _, _, _, aliases := parseUsingBody(text)
		if aliases == nil || aliases["Repo"] != "MyApp.Repo" {
			t.Errorf("Repo: got %q, want MyApp.Repo", aliases["Repo"])
		}
	})
}

func TestParseUsingBody_IgnoresHelperCallsInsideInlineDefBodies(t *testing.T) {
	text := `defmodule MyLib do
  def helper_name(_opts) do
    quote do
      import SharedLib.Hidden
    end
  end

  defmacro __using__(opts) do
    quote do
      def run(value) do
        helper_name(opts)
        value
      end
    end
  end
end`

	imported, _, _, _, _ := parseUsingBody(text)
	if len(imported) != 0 {
		t.Fatalf("expected no imports from helper call inside inline def body, got %v", imported)
	}
}

func TestParseHelperQuoteBlock_IgnoresInlineDefBodies(t *testing.T) {
	text := `defmodule MyLib do
  def helper_name(_opts) do
    quote do
      def run(value) do
        import SharedLib.Hidden
        value
      end
    end
  end
end`

	lines := strings.Split(text, "\n")
	imported, _, _, _, _ := parseHelperQuoteBlock(lines, "helper_name", nil)
	if len(imported) != 0 {
		t.Fatalf("expected no imports from inside inline def body, got %v", imported)
	}
}

func TestParseHelperQuoteBlock_MultiAliasUnexpectedTokenForwardProgress(t *testing.T) {
	text := `defmodule MyLib do
  def build_aliases(_opts) do
    quote do
      alias MyApp.{:unexpected, Accounts, 42}
    end
  end
end`
	lines := strings.Split(text, "\n")
	_, _, _, _, aliases := parseHelperQuoteBlock(lines, "build_aliases", nil)
	if aliases == nil || aliases["Accounts"] != "MyApp.Accounts" {
		t.Errorf("Accounts: got %q, want MyApp.Accounts", aliases["Accounts"])
	}
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

func TestCallContextAtCursor(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		line, col  int
		wantExpr   string
		wantArgIdx int
		wantOK     bool
	}{
		// Parenthesized calls
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
			name:       "multi-line paren call",
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
			col:        4,
			wantExpr:   "",
			wantArgIdx: 0,
			wantOK:     false,
		},
		// Paren-less calls
		{
			name:       "no-paren qualified call first arg",
			text:       `IO.puts "hello"`,
			line:       0,
			col:        10,
			wantExpr:   "IO.puts",
			wantArgIdx: 0,
			wantOK:     true,
		},
		{
			name:       "no-paren bare call first arg",
			text:       `import MyApp.Repo`,
			line:       0,
			col:        10,
			wantExpr:   "import",
			wantArgIdx: 0,
			wantOK:     true,
		},
		{
			name:       "no-paren keyword if is not a call",
			text:       "if true do\n  :ok\nend",
			line:       0,
			col:        5,
			wantExpr:   "",
			wantArgIdx: 0,
			wantOK:     false,
		},
		{
			name:       "no-paren two args second",
			text:       `Enum.each list, fun`,
			line:       0,
			col:        18,
			wantExpr:   "Enum.each",
			wantArgIdx: 1,
			wantOK:     true,
		},
		{
			name:       "no-paren inside string not matched",
			text:       `x = "foo bar"`,
			line:       0,
			col:        8,
			wantExpr:   "",
			wantArgIdx: 0,
			wantOK:     false,
		},
		// Edge cases: maps, nested calls, keyword lists, tuples
		{
			name:       "no-paren map param cursor on key",
			text:       `IO.inspect %{a: 1, b: 2}`,
			line:       0,
			col:        15,
			wantExpr:   "IO.inspect",
			wantArgIdx: 0,
			wantOK:     true,
		},
		{
			name:       "no-paren map param cursor inside map after comma",
			text:       `IO.inspect %{a: 1, b: 2}`,
			line:       0,
			col:        20,
			wantExpr:   "IO.inspect",
			wantArgIdx: 0,
			wantOK:     true,
		},
		{
			name:       "no-paren with nested paren call as arg",
			text:       `IO.puts String.upcase("hi")`,
			line:       0,
			col:        23,
			wantExpr:   "String.upcase",
			wantArgIdx: 0,
			wantOK:     true,
		},
		{
			name:       "no-paren second arg is paren call",
			text:       `Enum.each list, Enum.count(other)`,
			line:       0,
			col:        30,
			wantExpr:   "Enum.count",
			wantArgIdx: 0,
			wantOK:     true,
		},
		{
			name:       "no-paren keyword list arg",
			text:       `plug :auth, only: [:index]`,
			line:       0,
			col:        20,
			wantExpr:   "plug",
			wantArgIdx: 1,
			wantOK:     true,
		},
		{
			name:       "no-paren tuple arg",
			text:       `send self(), {:ok, result}`,
			line:       0,
			col:        20,
			wantExpr:   "send",
			wantArgIdx: 1,
			wantOK:     true,
		},
		{
			name:       "no-paren with paren call as sole arg",
			text:       `IO.puts inspect(x)`,
			line:       0,
			col:        17,
			wantExpr:   "inspect",
			wantArgIdx: 0,
			wantOK:     true,
		},
		{
			name:       "cursor on func name of no-paren call",
			text:       `IO.puts "hello"`,
			line:       0,
			col:        5,
			wantExpr:   "IO.puts",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// Pipe operator — cursor inside paren call on RHS
		{
			name:       "pipe into paren call",
			text:       `list |> Enum.map(fn x -> x end)`,
			line:       0,
			col:        20,
			wantExpr:   "Enum.map",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// Struct as argument
		{
			name:       "no-paren struct arg",
			text:       `Repo.insert %User{name: "joe"}`,
			line:       0,
			col:        20,
			wantExpr:   "Repo.insert",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// Multi-line no-paren (comma at end of prev line)
		{
			name:       "multi-line no-paren call",
			text:       "use MyApp.Web,\n  controllers: true",
			line:       1,
			col:        15,
			wantExpr:   "use",
			wantArgIdx: 1,
			wantOK:     true,
		},
		// Nested paren call inside paren call — cursor on outer's second arg
		{
			name:       "nested paren call second arg of outer",
			text:       `Enum.reduce(list, %{}, fn x, acc -> acc end)`,
			line:       0,
			col:        18,
			wantExpr:   "Enum.reduce",
			wantArgIdx: 1,
			wantOK:     true,
		},
		// Sigil as argument to no-paren call
		{
			name:       "no-paren sigil arg",
			text:       `Regex.compile ~r/foo/`,
			line:       0,
			col:        18,
			wantExpr:   "Regex.compile",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// Guard clause — cursor inside is_integer(x) call
		{
			name:       "inside guard call",
			text:       `def foo(x) when is_integer(x) do`,
			line:       0,
			col:        27,
			wantExpr:   "is_integer",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// Capture operator — not a call
		{
			name:       "capture operator not a call",
			text:       `&Enum.map/2`,
			line:       0,
			col:        5,
			wantExpr:   "",
			wantArgIdx: 0,
			wantOK:     false,
		},
		// Bare assignment — not a call
		{
			name:       "assignment not a call",
			text:       `result = Enum.map(list, fun)`,
			line:       0,
			col:        3,
			wantExpr:   "",
			wantArgIdx: 0,
			wantOK:     false,
		},
		// Nested map inside paren call
		{
			name:       "map inside paren call",
			text:       `Repo.insert(%{name: "joe", age: 30})`,
			line:       0,
			col:        25,
			wantExpr:   "Repo.insert",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// Keyword list as last arg in paren call
		{
			name:       "keyword list in paren call",
			text:       `GenServer.call(pid, :msg, timeout: 5000)`,
			line:       0,
			col:        35,
			wantExpr:   "GenServer.call",
			wantArgIdx: 2,
			wantOK:     true,
		},
		// Empty args — cursor right after open paren
		{
			name:       "cursor right after open paren",
			text:       `foo()`,
			line:       0,
			col:        4,
			wantExpr:   "foo",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// Cross-line: unrelated expression on previous line should not match
		{
			name:       "unrelated line above not matched",
			text:       "result\n\"hello\"",
			line:       1,
			col:        3,
			wantExpr:   "",
			wantArgIdx: 0,
			wantOK:     false,
		},
		// def/defp — keyword not treated as call
		{
			name:       "def keyword not a call",
			text:       `def foo(x) do`,
			line:       0,
			col:        8,
			wantExpr:   "foo",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// Binary <<>> as argument
		{
			name:       "no-paren binary arg",
			text:       `send pid, <<1, 2, 3>>`,
			line:       0,
			col:        15,
			wantExpr:   "send",
			wantArgIdx: 1,
			wantOK:     true,
		},
		// fn/end block as argument to paren call
		{
			name:       "fn end block inside paren call",
			text:       "Enum.map(list, fn x -> x * 2 end)",
			line:       0,
			col:        25,
			wantExpr:   "Enum.map",
			wantArgIdx: 1,
			wantOK:     true,
		},
		// Cursor inside fn block body — enclosing call is Task.async
		{
			name:       "cursor inside fn block of paren call",
			text:       "Task.async(fn ->\n  heavy_work()\nend)",
			line:       1,
			col:        5,
			wantExpr:   "Task.async",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// Pipe chain — cursor on rightmost call
		{
			name:       "pipe chain paren call",
			text:       `list |> Enum.filter(fn x -> x > 0 end) |> Enum.map(fn x -> x * 2 end)`,
			line:       0,
			col:        55,
			wantExpr:   "Enum.map",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// Anonymous function call var.(arg)
		{
			name:       "anonymous function call",
			text:       `callback.(arg1, arg2)`,
			line:       0,
			col:        15,
			wantExpr:   "callback",
			wantArgIdx: 1,
			wantOK:     true,
		},
		// Nested keyword default — def foo(opts \\ [key: :val])
		{
			name:       "cursor inside default keyword list",
			text:       `def foo(opts \\ [key: :val]) do`,
			line:       0,
			col:        22,
			wantExpr:   "foo",
			wantArgIdx: 0,
			wantOK:     true,
		},
		// String interpolation as argument
		{
			name:       "string interpolation arg",
			text:       `Logger.info("User #{name} logged in")`,
			line:       0,
			col:        20,
			wantExpr:   "Logger.info",
			wantArgIdx: 0,
			wantOK:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tf := NewTokenizedFile(tt.text)
			expr, argIdx, ok := tf.CallContextAtCursor(tt.line, tt.col)
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

func TestExtractEnclosingModuleFromTokens_NestedModules(t *testing.T) {
	text := `defmodule MyApp.Outer do
  defmodule Inner do
    def run do
      __MODULE__
    end
  end

  def call do
    __MODULE__
  end
end`

	tokens := parser.Tokenize([]byte(text))

	inner := extractEnclosingModuleFromTokens([]byte(text), tokens, 3)
	if inner != "MyApp.Outer.Inner" {
		t.Errorf("inner: got %q, want MyApp.Outer.Inner", inner)
	}

	outer := extractEnclosingModuleFromTokens([]byte(text), tokens, 7)
	if outer != "MyApp.Outer" {
		t.Errorf("outer: got %q, want MyApp.Outer", outer)
	}
}

func TestExtractEnclosingModuleFromTokens_DoesNotStealLaterDoFromInlineModule(t *testing.T) {
	text := `defmodule MyApp.Outer do
  defmodule Inline, do: nil

  def run do
    __MODULE__
  end
end`

	tokens := parser.Tokenize([]byte(text))

	enclosing := extractEnclosingModuleFromTokens([]byte(text), tokens, 4)
	if enclosing != "MyApp.Outer" {
		t.Errorf("got %q, want MyApp.Outer", enclosing)
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

func TestSkipToEndOfStatement_NegativeDepthClamp(t *testing.T) {
	// Regression: skipToEndOfStatement would go negative on unmatched closing
	// brackets, causing premature termination on the next TokEOL.
	source := []byte("x = ) + y\nz = 1")
	tokens := parser.Tokenize(source)
	n := len(tokens)

	// Start at index 0; the ) at index 2 is unmatched.
	// Without clamping, depth goes -1, and the function returns at the first EOL.
	// With clamping, we should reach the EOL at the end of the first line normally.
	endIdx := skipToEndOfStatement(tokens, n, 0)

	// We expect it to stop at the EOL after "y" (end of first statement)
	if endIdx >= n {
		t.Fatalf("expected endIdx < n, got %d", endIdx)
	}
	if tokens[endIdx].Kind != parser.TokEOL && tokens[endIdx].Kind != parser.TokEOF {
		t.Errorf("expected TokEOL or TokEOF at endIdx, got %v", tokens[endIdx].Kind)
	}
}

func TestExtractEnclosingModule_DefprotocolAndDefimpl(t *testing.T) {
	// Regression: extractEnclosingModuleFromTokens only handled TokDefmodule,
	// missing TokDefprotocol and TokDefimpl.
	t.Run("defprotocol", func(t *testing.T) {
		text := `defprotocol MyApp.Printable do
  def print(data)
end`
		tokens := parser.Tokenize([]byte(text))
		enclosing := extractEnclosingModuleFromTokens([]byte(text), tokens, 1)
		if enclosing != "MyApp.Printable" {
			t.Errorf("got %q, want MyApp.Printable", enclosing)
		}
	})

	t.Run("defimpl", func(t *testing.T) {
		text := `defimpl MyApp.Printable, for: MyApp.User do
  def print(user), do: user.name
end`
		tokens := parser.Tokenize([]byte(text))
		enclosing := extractEnclosingModuleFromTokens([]byte(text), tokens, 1)
		if enclosing != "MyApp.Printable" {
			t.Errorf("got %q, want MyApp.Printable", enclosing)
		}
	})
}

func TestExtractAliasesInScope_DefmoduleDoOnNextLine(t *testing.T) {
	// Regression: when `do` appears on the next line after defmodule,
	// the module frame was not properly pushed, causing aliases to leak.
	text := `defmodule MyApp.Outer
do
  alias MyApp.OuterOnly

  defmodule Inner do
    alias MyApp.InnerOnly
    def run, do: InnerOnly.call()
  end

  def outer_run, do: OuterOnly.call()
end`

	// Line 6 is inside Inner — should see InnerOnly but not OuterOnly
	innerAliases := ExtractAliasesInScope(text, 6)
	if innerAliases["InnerOnly"] != "MyApp.InnerOnly" {
		t.Errorf("InnerOnly: got %q, want MyApp.InnerOnly", innerAliases["InnerOnly"])
	}
	if _, ok := innerAliases["OuterOnly"]; ok {
		t.Error("OuterOnly should NOT be visible inside Inner")
	}

	// Line 9 is inside Outer after Inner ends — should see OuterOnly but not InnerOnly
	outerAliases := ExtractAliasesInScope(text, 9)
	if outerAliases["OuterOnly"] != "MyApp.OuterOnly" {
		t.Errorf("OuterOnly: got %q, want MyApp.OuterOnly", outerAliases["OuterOnly"])
	}
	if _, ok := outerAliases["InnerOnly"]; ok {
		t.Error("InnerOnly should NOT leak to outer scope")
	}
}

func TestExtractAliases_MultiAliasUnexpectedTokensForwardProgress(t *testing.T) {
	// Regression: collectModuleName returning ("", k) without advancing k
	// caused infinite loops in multi-alias brace scanning.
	// Note: we test atoms and numbers as unexpected tokens. Maps with braces
	// are a separate edge case that may confuse brace depth tracking.
	text := `defmodule MyApp.Web do
  alias MyApp.{
    :unexpected_atom,
    Accounts,
    123,
    Users
  }
end`
	aliases := ExtractAliases(text)

	// Should extract valid module names despite unexpected tokens
	if aliases["Accounts"] != "MyApp.Accounts" {
		t.Errorf("Accounts: got %q, want MyApp.Accounts", aliases["Accounts"])
	}
	if aliases["Users"] != "MyApp.Users" {
		t.Errorf("Users: got %q, want MyApp.Users", aliases["Users"])
	}
}

func TestParseUsingBody_KeywordFetchAndPopBang(t *testing.T) {
	// Regression: Keyword.fetch! and Keyword.pop! were not handled because
	// the switch cases only checked "fetch" and "pop", not "fetch!" and "pop!".
	text := `defmodule MyLib do
  defmacro __using__(opts) do
    required_mod = Keyword.fetch!(opts, :required_mod)
    {optional_mod, opts} = Keyword.pop!(opts, :optional_mod, DefaultMod)

    quote do
      import unquote(required_mod)
      use unquote(optional_mod)
    end
  end
end`

	_, _, _, optBindings, _ := parseUsingBody(text)

	foundFetch := false
	foundPop := false
	for _, b := range optBindings {
		if b.optKey == "required_mod" && b.kind == "import" {
			foundFetch = true
			if b.defaultMod != "" {
				t.Errorf("fetch! should have no default, got %q", b.defaultMod)
			}
		}
		if b.optKey == "optional_mod" && b.kind == "use" {
			foundPop = true
			if b.defaultMod != "DefaultMod" {
				t.Errorf("pop! default: want DefaultMod, got %q", b.defaultMod)
			}
		}
	}
	if !foundFetch {
		t.Errorf("expected opt binding for required_mod via fetch!, got %v", optBindings)
	}
	if !foundPop {
		t.Errorf("expected opt binding for optional_mod via pop!, got %v", optBindings)
	}
}

func TestFindModuleAttributeDefinition_StatementStartCheck(t *testing.T) {
	// Regression: FindModuleAttributeDefinitionTokenized matched @attr used
	// as a value reference (not at statement start), jumping to wrong locations.
	text := `defmodule MyApp.Worker do
  @config_value %{timeout: 5000}

  def run(job) do
    process(@config_value)
    @config_value
    :ok
  end
end`

	line, found := FindModuleAttributeDefinition(text, "config_value")
	if !found {
		t.Fatal("expected to find @config_value definition")
	}
	// Should find line 2 (the actual definition), not lines 5 or 6 (references)
	if line != 2 {
		t.Errorf("expected definition at line 2, got line %d", line)
	}
}

func TestCallContextNoParen_KeywordFilter(t *testing.T) {
	// Regression: callContextNoParen didn't filter Elixir keywords like `if`,
	// `case`, `cond`, `with`, causing them to be detected as function calls.
	tests := []struct {
		name   string
		text   string
		line   int
		col    int
		wantOK bool
	}{
		{"if is not a call", "if true do\n  :ok\nend", 0, 5, false},
		{"case is not a call", "case x do\n  _ -> :ok\nend", 0, 6, false},
		{"cond is not a call", "cond do\n  true -> :ok\nend", 0, 3, false},
		{"with is not a call", "with {:ok, x} <- foo() do\n  x\nend", 0, 10, false},
		{"unless is not a call", "unless false do\n  :ok\nend", 0, 8, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tf := NewTokenizedFile(tt.text)
			_, _, ok := tf.CallContextAtCursor(tt.line, tt.col)
			if ok != tt.wantOK {
				t.Errorf("got ok=%v, want ok=%v", ok, tt.wantOK)
			}
		})
	}
}

// =============================================================================
// Consistency tests - ensure similar functions handle edge cases the same way
// =============================================================================

// TestModuleScopeConsistency verifies that functions handling module scope
// produce consistent results for tricky edge cases.
func TestModuleScopeConsistency(t *testing.T) {
	// These test cases have historically caused divergence between similar functions
	testCases := []struct {
		name           string
		text           string
		innerLine      int // line inside inner module
		outerLine      int // line inside outer module (after inner ends)
		wantInnerMod   string
		wantOuterMod   string
		wantInnerAlias string // alias only visible in inner
		wantOuterAlias string // alias only visible in outer
	}{
		{
			name: "nested modules basic",
			text: `defmodule MyApp.Outer do
  alias MyApp.OuterOnly

  defmodule Inner do
    alias MyApp.InnerOnly
    def run, do: :ok
  end

  def call, do: :ok
end`,
			innerLine:      5,
			outerLine:      8,
			wantInnerMod:   "MyApp.Outer.Inner",
			wantOuterMod:   "MyApp.Outer",
			wantInnerAlias: "InnerOnly",
			wantOuterAlias: "OuterOnly",
		},
		{
			name: "do on next line",
			text: `defmodule MyApp.Outer
do
  alias MyApp.OuterOnly

  defmodule Inner
  do
    alias MyApp.InnerOnly
    def run, do: :ok
  end

  def call, do: :ok
end`,
			innerLine:      7,
			outerLine:      10,
			wantInnerMod:   "MyApp.Outer.Inner",
			wantOuterMod:   "MyApp.Outer",
			wantInnerAlias: "InnerOnly",
			wantOuterAlias: "OuterOnly",
		},
		{
			name: "defprotocol and defimpl",
			text: `defprotocol MyApp.Printable do
  alias MyApp.ProtoOnly
  def print(data)
end

defimpl MyApp.Printable, for: MyApp.User do
  alias MyApp.ImplOnly
  def print(user), do: user.name
end`,
			innerLine:      2, // inside protocol
			outerLine:      7, // inside impl
			wantInnerMod:   "MyApp.Printable",
			wantOuterMod:   "MyApp.Printable",
			wantInnerAlias: "ProtoOnly",
			wantOuterAlias: "ImplOnly",
			// Note: protocol and impl are separate top-level constructs,
			// so their aliases don't leak to each other (both are "" for the other)
		},
		{
			name: "fn...end does not break scope",
			text: `defmodule MyApp.Worker do
  alias MyApp.Helper

  def run do
    handler = fn x ->
      x * 2
    end
    handler.(1)
  end

  def other, do: Helper.call()
end`,
			innerLine:      5,  // inside fn
			outerLine:      10, // after fn ends
			wantInnerMod:   "MyApp.Worker",
			wantOuterMod:   "MyApp.Worker",
			wantInnerAlias: "Helper",
			wantOuterAlias: "Helper",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test extractEnclosingModuleFromTokens
			source := []byte(tc.text)
			tokens := parser.Tokenize(source)

			innerMod := extractEnclosingModuleFromTokens(source, tokens, tc.innerLine)
			if innerMod != tc.wantInnerMod {
				t.Errorf("enclosing module at inner line %d: got %q, want %q",
					tc.innerLine, innerMod, tc.wantInnerMod)
			}

			outerMod := extractEnclosingModuleFromTokens(source, tokens, tc.outerLine)
			if outerMod != tc.wantOuterMod {
				t.Errorf("enclosing module at outer line %d: got %q, want %q",
					tc.outerLine, outerMod, tc.wantOuterMod)
			}

			// Test ExtractAliasesInScope
			innerAliases := ExtractAliasesInScope(tc.text, tc.innerLine)
			if tc.wantInnerAlias != "" {
				if _, ok := innerAliases[tc.wantInnerAlias]; !ok {
					t.Errorf("inner line %d: expected alias %q not found, got %v",
						tc.innerLine, tc.wantInnerAlias, innerAliases)
				}
			}
			// Only check alias leakage if inner and outer are different scopes
			// (same module name means they're in the same scope or separate top-level modules)
			if tc.wantOuterAlias != "" && tc.wantOuterAlias != tc.wantInnerAlias && tc.wantInnerMod != tc.wantOuterMod {
				if _, ok := innerAliases[tc.wantOuterAlias]; ok {
					t.Errorf("inner line %d: outer alias %q should not be visible",
						tc.innerLine, tc.wantOuterAlias)
				}
			}

			outerAliases := ExtractAliasesInScope(tc.text, tc.outerLine)
			if tc.wantOuterAlias != "" {
				if _, ok := outerAliases[tc.wantOuterAlias]; !ok {
					t.Errorf("outer line %d: expected alias %q not found, got %v",
						tc.outerLine, tc.wantOuterAlias, outerAliases)
				}
			}
			// Only check alias leakage if inner and outer are different scopes
			if tc.wantInnerAlias != "" && tc.wantInnerAlias != tc.wantOuterAlias && tc.wantInnerMod != tc.wantOuterMod {
				if _, ok := outerAliases[tc.wantInnerAlias]; ok {
					t.Errorf("outer line %d: inner alias %q should not be visible",
						tc.outerLine, tc.wantInnerAlias)
				}
			}
		})
	}
}

// TestDepthTrackingConsistency verifies that all depth-tracking code handles
// edge cases consistently (especially negative depth clamping).
func TestDepthTrackingConsistency(t *testing.T) {
	// Code with unmatched brackets at start (simulates cursor mid-expression)
	testCases := []struct {
		name   string
		text   string
		line   int
		wantOK bool // should not crash or return garbage
	}{
		{
			name:   "unmatched close paren",
			text:   ") + foo(x)\nbar()",
			line:   1,
			wantOK: true,
		},
		{
			name:   "unmatched close bracket",
			text:   "] ++ list\nother()",
			line:   1,
			wantOK: true,
		},
		{
			name:   "unmatched end",
			text:   "end\ndef foo, do: :ok",
			line:   1,
			wantOK: true,
		},
		{
			name:   "deeply nested then unmatched",
			text:   "foo(bar([{x}]))\n))]}\nvalid()",
			line:   2,
			wantOK: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// These should not panic
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on %q: %v", tc.name, r)
				}
			}()

			// Test various functions that track depth
			source := []byte(tc.text)
			tokens := parser.Tokenize(source)

			// extractEnclosingModuleFromTokens
			_ = extractEnclosingModuleFromTokens(source, tokens, tc.line)

			// ExtractAliasesInScope
			_ = ExtractAliasesInScope(tc.text, tc.line)

			// skipToEndOfStatement
			if len(tokens) > 0 {
				_ = skipToEndOfStatement(tokens, len(tokens), 0)
			}

			// TokenWalker
			w := parser.NewTokenWalker(source, tokens)
			for w.More() {
				w.Advance()
			}
			if w.Depth() < 0 || w.BlockDepth() < 0 {
				t.Errorf("TokenWalker depth went negative: depth=%d blockDepth=%d",
					w.Depth(), w.BlockDepth())
			}
		})
	}
}
