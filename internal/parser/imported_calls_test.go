package parser

import (
	"fmt"
	"testing"
)

// TestParseText_ImportedCalls checks which unqualified calls are recorded as
// calls of an imported module: they obey only: and except:, a call of a
// name and arity that the module defines is its own, and calls on a def
// line, in a script, and the keys of an only: list are recorded.
func TestParseText_ImportedCalls(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    []string // "Module.function:line:kind"
		notWant []string
	}{
		{
			name: "only list",
			src: `defmodule A do
  import Q, only: [from: 2]
  def a, do: from(x, y)
  def b(x) do
    where(x, y)
  end
end
`,
			want:    []string{"Q.from:2:import_only", "Q.from:3:call"},
			notWant: []string{"Q.where:5:call"},
		},
		{
			name: "only list over several lines",
			src: `defmodule A do
  import Q,
    only: [
      other: 1,
      from: 2
    ]

  def a, do: from(1, 2)
end
`,
			want: []string{"Q.other:4:import_only", "Q.from:5:import_only", "Q.from:8:call"},
		},
		{
			name: "except list",
			src: `defmodule A do
  import Q, except: [from: 2]
  def a(x) do
    from(x, 1)
    where(x, 1)
  end
end
`,
			want:    []string{"Q.where:5:call"},
			notWant: []string{"Q.from:2:import_only", "Q.from:4:call"},
		},
		{
			name: "only atom brings in every name",
			src: `defmodule A do
  import Q, only: :functions
  def a(x) do
    from(x, 1)
  end
end
`,
			want: []string{"Q.from:4:call"},
		},
		{
			name: "a local function of the same arity is the module's own",
			src: `defmodule A do
  import Q
  def cast(attrs), do: attrs
  def a(attrs) do
    cast(attrs)
    attrs |> cast(:x, :y)
    cast(attrs, :x, :y)
    cast(attrs, a: 1, b: 2)
  end
end
`,
			// Line 8 is cast/2: the keyword pairs are one argument.
			want:    []string{"Q.cast:6:call", "Q.cast:7:call", "Q.cast:8:call"},
			notWant: []string{"Q.cast:5:call"},
		},
		{
			name: "an import in a quote reaches the quote only",
			src: `defmodule MyAppWeb do
  def controller do
    quote do
      import Plug.Conn
      def act(conn), do: put_status(conn, 1)
    end
  end

  def helper(x), do: put_status(x, 1)
end
`,
			want:    []string{"Plug.Conn.put_status:5:call"},
			notWant: []string{"Plug.Conn.put_status:9:call"},
		},
		{
			name: "a callback or a type of the name is not a local function",
			src: `defmodule A do
  import Q
  @callback foo(term) :: term
  @type bar :: term
  def run(x), do: {foo(x), bar()}
end
`,
			want: []string{"Q.foo:5:call", "Q.bar:5:call"},
		},
		{
			name: "a local function in another module of the file does not count",
			src: `defmodule A do
  import Q
  def a(x), do: cast(x)
end

defmodule B do
  def cast(x), do: x
end
`,
			want: []string{"Q.cast:3:call"},
		},
		{
			name: "def line: one-line body, guard, and default argument",
			src: `defmodule A do
  import Q
  def a(x) when is_ok(x), do: cast(x, 1)
  def b(x \\ fallback()), do: x
end
`,
			want:    []string{"Q.cast:3:call", "Q.is_ok:3:call", "Q.fallback:4:call"},
			notWant: []string{"Q.a:3:call", "Q.b:4:call"},
		},
		{
			name: "a call after an attribute read on the same line",
			src: `defmodule A do
  import Q
  def a(x) do
    check(@handler, from(x))
    Enum.reduce(@days, cast(x, 1), fn d, acc -> acc end)
  end
end
`,
			want: []string{"Q.check:4:call", "Q.from:4:call", "Q.cast:5:call"},
		},
		{
			name: "captures",
			src: `defmodule A do
  import Q
  def cast(x), do: x
  def a, do: {&cast/3, &cast/1, &Q.other/1}
end
`,
			// cast/1 is the module's own; Q.other is a qualified call.
			want:    []string{"Q.cast:4:call"},
			notWant: []string{"Q.cast/1"},
		},
		{
			name: "__MODULE__ calls",
			src: `defmodule A do
  def a(x), do: {__MODULE__.b(x), &__MODULE__.b/1, __MODULE__.Sub.c(x)}
  def b(x), do: x
end
`,
			want: []string{"A.b:2:call", "A.Sub.c:2:call"},
		},
		{
			name: "script without a module",
			src: `import Q

query = from(x in "t", select: x)
query |> where(true)
`,
			want: []string{"Q.from:3:call", "Q.where:4:call"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, refs, err := ParseText("test.ex", tt.src)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, r := range refs {
				if r.Function != "" {
					got[fmt.Sprintf("%s.%s:%d:%s", r.Module, r.Function, r.Line, r.Kind)] = true
				}
			}
			for _, w := range tt.want {
				if !got[w] {
					t.Errorf("missing %s; got %v", w, got)
				}
			}
			for _, w := range tt.notWant {
				if got[w] {
					t.Errorf("unexpected %s", w)
				}
			}
		})
	}
}

func TestCallArity(t *testing.T) {
	tests := []struct {
		src  string
		want int
	}{
		{"f()", 0},
		{"f(a)", 1},
		{"f(a, b)", 2},
		{"f(a, b: 1, c: 2)", 2},
		{"f(a, [b, c], %{d: 1})", 3},
		{"x |> f(a)", 2},
		{"x |> f", 1},
		{"f(a) do\n :ok\nend", 2},
		{"case f(a) do\n _ -> :ok\nend", 1},
		{"x = Mod.f(a) do\n :ok\nend", 2},
		{"g(f(a)) do\n :ok\nend", 1},
		{"f(fn x -> x end, b)", 2},
		{"f(\n  a,\n  b\n)", 2},
		{"f a", -1},
		{"&f/3", 3},
	}
	for _, tt := range tests {
		src := []byte(tt.src)
		tokens := TokenizeFull(src).Tokens
		pos := -1
		for i, tok := range tokens {
			if tok.Kind == TokIdent && TokenText(src, tok) == "f" {
				pos = i
				break
			}
		}
		if got := callArity(src, tokens, len(tokens), pos); got != tt.want {
			t.Errorf("callArity(%q) = %d, want %d", tt.src, got, tt.want)
		}
	}
}
