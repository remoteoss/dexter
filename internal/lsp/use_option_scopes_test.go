package lsp

import (
	"fmt"
	"strings"
	"testing"
)

// TestRegression_UseChainOptionScopes: each case has the module that Elixir
// gives (clauses and branches start from the bindings before the block; a
// match inside a quote is the caller's code; helpers, guards, validate!),
// or the use line where the answer depends on which branch runs: Dexter
// does not evaluate the condition, and the two clauses use the wrapper with
// different options.
func TestRegression_UseChainOptionScopes(t *testing.T) {
	inject := func(tag string) string {
		return "  defmacro __using__(_opts) do\n    quote do\n      def new(args, opts \\\\ []), do: {:" + tag + ", args, opts}\n    end\n  end"
	}
	lib := []bbFile{
		optModule("PL.Base", inject("base")),
		optModule("PL.Second", inject("second")),
		optModule("PL.Third", inject("third")),
		optModule("PL.Wrapper", optUsing("    {mod, opts} = Keyword.pop(opts, :base, PL.Base)", "      use unquote(mod), unquote(opts)")),
		optModule("PL.IfElse", "  defmacro __using__(opts) do\n    if opts[:flag] do\n      opts = Keyword.put(opts, :base, PL.Second)\n      quote do\n        use PL.Wrapper, unquote(opts)\n      end\n    else\n      quote do\n        use PL.Wrapper, unquote(opts)\n      end\n    end\n  end"),
		optModule("PL.CaseArms", "  defmacro __using__(opts) do\n    case opts[:kind] do\n      :a ->\n        opts = Keyword.put(opts, :base, PL.Second)\n        quote do\n          use PL.Wrapper, unquote(opts)\n        end\n\n      _ ->\n        quote do\n          use PL.Wrapper, unquote(opts)\n        end\n    end\n  end"),
		optModule("PL.HelperParam", "  defmacro __using__(opts) do\n    build(Keyword.put_new(opts, :base, PL.Second))\n  end\n\n  defp build(o) do\n    quote do\n      use PL.Wrapper, unquote(o)\n    end\n  end"),
		optModule("PL.FnShadow", "  defmacro __using__(opts) do\n    opts = Keyword.put(opts, :base, PL.Second)\n    _ = Enum.map([1], fn opts -> opts end)\n    quote do\n      use PL.Wrapper, unquote(opts)\n    end\n  end"),
		optModule("PL.Guarded", "  defmacro __using__(opts) when is_list(opts) do\n    opts = Keyword.put_new(opts, :base, PL.Second)\n    quote do\n      use PL.Wrapper, unquote(opts)\n    end\n  end"),
		optModule("PL.MultiLineGet", "  defmacro __using__(opts) do\n    mod =\n      Keyword.get(opts, :base, PL.Second)\n    quote do\n      use unquote(mod)\n    end\n  end"),
		optModule("PL.DelThenGet", optUsing("    opts = Keyword.delete(opts, :base)\n    mod = Keyword.get(opts, :base, PL.Second)", "      use unquote(mod)")),
		optModule("PL.Nil", optUsing("    mod = Keyword.get(opts, :base, nil) || PL.Second", "      use unquote(mod)")),
		optModule("PL.TwoClauses", "  defmacro __using__(opts) when is_list(opts) do\n    opts = Keyword.put(opts, :base, PL.Third)\n    quote do\n      use PL.Wrapper, unquote(opts)\n    end\n  end\n\n  defmacro __using__(which) when is_atom(which) do\n    quote do\n      use PL.Wrapper, base: PL.Second\n    end\n  end"),
		optModule("PL.Validate2", optUsing("    opts = Keyword.validate!(opts, [:queue, base: PL.Second])\n    mod = opts[:base]", "      use unquote(mod)")),
		optModule("PL.InnerQuote", "  defmacro __using__(opts) do\n    mod = Keyword.get(opts, :base, PL.Second)\n    quote do\n      use unquote(mod)\n    end\n  end"),
		optModule("PL.DoKw", "  defmacro __using__(opts) do\n    _ = if true, do: :ok\n    opts = Keyword.put(opts, :base, PL.Second)\n    quote do\n      use PL.Wrapper, unquote(opts)\n    end\n  end"),
		optModule("PL.HelperExtraArg", "  defmacro __using__(opts) do\n    opts = Keyword.put_new(opts, :base, PL.Second)\n    build(opts, :x)\n  end\n\n  defp build(o, _env) do\n    quote do\n      use PL.Wrapper, unquote(o)\n    end\n  end"),
		optModule("PL.InQuoteTuple", "  defmacro __using__(opts) do\n    quote do\n      {mod, opts} = Keyword.pop(unquote(opts), :base, PL.Third)\n      use PL.Wrapper, unquote(opts)\n    end\n  end"),
		optModule("PL.NestedAtom", "  defmacro __using__(_opts) do\n    quote do\n      use PL.Wrapper, base: PL.Second\n    end\n  end\n\n  defmodule Inner do\n    defmacro __using__(which) when is_atom(which) do\n      quote do\n        use PL.Wrapper, base: PL.Third\n      end\n    end\n  end"),
		optModule("PL.OneLineNested", "  defmacro __using__(opts) when is_list(opts) do\n    quote do\n      use PL.Wrapper, base: PL.Third\n    end\n  end\n\n  defmodule Tiny, do: def(x, do: x)\n\n  defmacro __using__(which) when is_atom(which) do\n    quote do\n      use PL.Wrapper, base: PL.Second\n    end\n  end"),
		{"lib/pl_siblings.ex", "defmodule PL.SibA do\n  defmacro __using__(_opts) do\n    quote do\n      use PL.Wrapper, base: PL.Second\n    end\n  end\nend\n\ndefmodule PL.SibB do\n  defmacro __using__(which) when is_atom(which) do\n    quote do\n      use PL.Wrapper, base: PL.Third\n    end\n  end\nend\n"},
		optModule("PL.InQuoteRebind", "  defmacro __using__(opts) do\n    quote do\n      opts = [base: PL.Third]\n      use PL.Wrapper, unquote(opts)\n    end\n  end"),
	}
	workers := []struct{ name, use, want string }{
		{"W.IfElseOff", "use PL.IfElse, base: PL.Third", "use"},         // Elixir: third; the branch depends on a value
		{"W.IfElseOn", "use PL.IfElse, flag: true", "use"},              // Elixir: second
		{"W.CaseB", "use PL.CaseArms, kind: :b, base: PL.Third", "use"}, // Elixir: third
		{"W.CaseA", "use PL.CaseArms, kind: :a", "use"},                 // Elixir: second
		{"W.HelperParam", "use PL.HelperParam", "second"},
		{"W.FnShadow", "use PL.FnShadow", "second"},
		{"W.Guarded", "use PL.Guarded", "second"},
		{"W.MultiLineGet", "use PL.MultiLineGet", "second"},
		{"W.DelThenGet", "use PL.DelThenGet, base: PL.Third", "second"},
		{"W.Nil", "use PL.Nil", "second"},
		{"W.TwoClausesList", "use PL.TwoClauses, base: PL.Base", "third"},
		{"W.TwoClausesAtom", "use PL.TwoClauses, :x", "second"},
		{"W.Validate2", "use PL.Validate2", "second"},
		{"W.Validate2Own", "use PL.Validate2, base: PL.Third", "third"},
		{"W.DoKw", "use PL.DoKw", "second"},
		{"W.InQuoteRebind", "use PL.InQuoteRebind, base: PL.Second", "second"},
		{"W.HelperExtraArg", "use PL.HelperExtraArg", "second"},
		{"W.InQuoteTuple", "use PL.InQuoteTuple, base: PL.Second", "second"},
		{"W.NestedAtom", "use PL.NestedAtom, :x", "second"},
		{"W.OneLineNested", "use PL.OneLineNested, :x", "second"},
		{"W.Sibling", "use PL.SibA, :x", "second"},
	}
	files := append([]bbFile(nil), lib...)
	var caller strings.Builder
	caller.WriteString("defmodule PL.Caller do\n")
	for i, w := range workers {
		files = append(files, optModule(w.name, "  "+w.use+"\n\n  def run(a), do: new(a)"))
		fmt.Fprintf(&caller, "  def f%d(a), do: %s.new(a)\n", i, w.name)
	}
	caller.WriteString("end\n")
	callerFile := bbFile{"lib/pl_caller.ex", caller.String()}
	files = append(files, callerFile)
	server, cleanup := bbServer(t, files...)
	defer cleanup()
	uri := bbOpen(t, server, callerFile)
	for _, w := range workers {
		wf := optModule(w.name, "  "+w.use+"\n\n  def run(a), do: new(a)")
		wantLine := "lib/pl_" + w.want + ".ex:4"
		if w.want == "use" {
			wantLine = wf.rel + ":2"
		}
		l, c := bbPos(t, callerFile.src, w.name+".new", "new")
		got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot)
		if len(got) != 1 || got[0] != wantLine {
			t.Errorf("%s: definition = %v, want %s", w.name, got, wantLine)
		}
	}
}
