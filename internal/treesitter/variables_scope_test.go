package treesitter

import (
	"fmt"
	"strings"
	"testing"
)

// markedSource strips the markers from src: § is the cursor, and each
// [[name]] is an occurrence that the lookup must return.
func markedSource(t *testing.T, src string) (clean []byte, line, col uint, want []VariableOccurrence) {
	t.Helper()
	var b strings.Builder
	var row, c uint
	cursor := false
	var start uint
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "§"):
			line, col, cursor = row, c, true
			i += len("§")
		case strings.HasPrefix(src[i:], "[["):
			start = c
			i += 2
		case strings.HasPrefix(src[i:], "]]"):
			want = append(want, VariableOccurrence{Line: row, StartCol: start, EndCol: c})
			i += 2
		default:
			b.WriteByte(src[i])
			if src[i] == '\n' {
				row, c = row+1, 0
			} else {
				c++
			}
			i++
		}
	}
	if !cursor {
		t.Fatal("no cursor marker")
	}
	return []byte(b.String()), line, col, want
}

func formatOccurrences(occs []VariableOccurrence) string {
	parts := make([]string, len(occs))
	for i, o := range occs {
		parts[i] = fmt.Sprintf("%d:%d-%d", o.Line, o.StartCol, o.EndCol)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func TestFindVariableOccurrences_Scoping(t *testing.T) {
	tests := []struct {
		name string
		src  string
	}{
		{"module attribute with the same name as a default argument", `defmodule M do
  @timeout 5000
  def f([[§timeout]] \\ @timeout) do
    Process.sleep([[timeout]])
  end
end`},
		{"module attribute with the same name, cursor in the body", `defmodule M do
  @x 1
  def f([[x]]) do
    @x + [[§x]]
  end
end`},
		{"field access with the same name", `defmodule M do
  def f([[§name]], user) do
    user.name <> [[name]]
  end
end`},
		{"remote call with the same name", `defmodule M do
  def f([[§name]], user) do
    user.name() <> Mod.name([[name]])
  end
end`},
		{"cond conditions read the outer variable", `defmodule M do
  def f([[§x]]) do
    cond do
      [[x]] > 1 -> [[x]]
      true -> :small
    end
  end
end`},
		{"cond condition, cursor on the arm", `defmodule M do
  def f([[x]]) do
    cond do
      [[§x]] > 1 -> [[x]]
      true -> :small
    end
  end
end`},
		{"receive after takes an expression", `defmodule M do
  def f([[§t]]) do
    receive do
      :a -> :ok
    after
      [[t]] -> [[t]]
    end
  end
end`},
		{"bare test context parameter, cursor in the body", `defmodule MT do
  test "works", [[ctx]] do
    assert [[§ctx]].conn
    [[ctx]]
  end
end`},
		{"bare test context parameter, cursor on the parameter", `defmodule MT do
  test "works", [[§ctx]] do
    assert [[ctx]].conn
  end
end`},
		{"for filter with a do block, cursor in the body", `defmodule M do
  def f(xs) do
    for [[x]] <- xs, [[x]] > 1 do
      [[§x]]
    end
  end
end`},
		{"for filter with a do block, cursor on the filter", `defmodule M do
  def f(xs) do
    for [[x]] <- xs, [[§x]] > 1 do
      [[x]]
    end
  end
end`},
		{"for filters and into with a do block", `defmodule M do
  def f(xs, m) do
    for [[x]] <- xs, [[x]] > 1, rem([[x]], 2) == 0, into: m do
      {[[§x]], [[x]]}
    end
  end
end`},
		{"with else reads the outer variable", `defmodule M do
  def f([[§x]]) do
    with {:ok, x} <- g([[x]]) do
      x
    else
      _ -> [[x]]
    end
  end
end`},
		{"with else, cursor in the else clause", `defmodule M do
  def f([[x]]) do
    with {:ok, x} <- g([[x]]) do
      x
    else
      _ -> [[§x]]
    end
  end
end`},
		{"with binding does not reach the else clause", `defmodule M do
  def f(x) do
    with {:ok, [[§x]]} <- g(x) do
      [[x]]
    else
      _ -> x
    end
  end
end`},
		{"a rebind in a clause body reads the outer variable on its right", `defmodule M do
  def f([[§x]], y) do
    case y do
      :a ->
        x = [[x]] + 1
        x
      _ -> [[x]]
    end
  end
end`},
		{"a rebind in a clause body, cursor on the right side", `defmodule M do
  def f([[x]], y) do
    case y do
      :a ->
        x = [[§x]] + 1
        x
      _ -> [[x]]
    end
  end
end`},
		{"a rebind in a clause body, cursor on the new binding", `defmodule M do
  def f(x, y) do
    case y do
      :a ->
        [[x]] = x + 1
        [[§x]]
      _ -> x
    end
  end
end`},
		{"a guarded case clause binds its pattern", `defmodule M do
  def f(x) do
    case g() do
      [[§x]] when [[x]] > 0 -> [[x]]
      _ -> x
    end
  end
end`},
		{"a guarded case clause hides the outer variable", `defmodule M do
  def f([[§x]]) do
    case g() do
      x when x > 0 -> x
      _ -> [[x]]
    end
  end
end`},
		{"a guarded fn clause hides the outer variable", `defmodule M do
  def f([[§x]]) do
    Enum.map([1], fn x when x > 0 -> x end)
    [[x]]
  end
end`},
		{"a guard reads the outer variable when the pattern does not bind it", `defmodule M do
  def f([[§x]], y) do
    case y do
      z when z > [[x]] -> z
      _ -> [[x]]
    end
  end
end`},
		{"a local capture is not the variable", `defmodule M do
  def f([[§x]], l) do
    Enum.map(l, &x/1) ++ Enum.map(l, &([[x]] + &1))
  end
end`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, line, col, want := markedSource(t, tt.src)
			got := FindVariableOccurrences(src, line, col)
			if formatOccurrences(got) != formatOccurrences(want) {
				t.Errorf("occurrences = %s, want %s", formatOccurrences(got), formatOccurrences(want))
			}
		})
	}
}
