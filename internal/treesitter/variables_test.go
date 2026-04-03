package treesitter

import (
	"testing"
)

func TestFindVariableOccurrences_BasicVariable(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    result = transform(data)
    log(result)
    result
  end
end`)

	// Cursor on "result" at line 2, col 4
	occs := FindVariableOccurrences(src, 2, 4)
	if len(occs) != 3 {
		t.Fatalf("expected 3 occurrences of 'result', got %d", len(occs))
	}
	// Line 2: result = transform(data)
	if occs[0].Line != 2 {
		t.Errorf("occ[0] line: expected 2, got %d", occs[0].Line)
	}
	// Line 3: log(result)
	if occs[1].Line != 3 {
		t.Errorf("occ[1] line: expected 3, got %d", occs[1].Line)
	}
	// Line 4: result
	if occs[2].Line != 4 {
		t.Errorf("occ[2] line: expected 4, got %d", occs[2].Line)
	}
}

func TestFindVariableOccurrences_FunctionParam(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    transform(data)
  end
end`)

	// Cursor on "data" parameter at line 1, col 14
	occs := FindVariableOccurrences(src, 1, 14)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of 'data', got %d", len(occs))
	}
}

func TestFindVariableOccurrences_NotOnVariable(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    transform(data)
  end
end`)

	// Cursor on "transform" (function call, not a variable)
	occs := FindVariableOccurrences(src, 2, 4)
	if occs != nil {
		t.Errorf("expected nil for function call, got %d occurrences", len(occs))
	}
}

func TestFindVariableOccurrences_NotOnKeyword(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    data
  end
end`)

	// Cursor on "def" keyword
	occs := FindVariableOccurrences(src, 1, 2)
	if occs != nil {
		t.Errorf("expected nil for keyword, got %d occurrences", len(occs))
	}
}

func TestFindVariableOccurrences_ScopedToFunction(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def first(x) do
    x + 1
  end

  def second(x) do
    x + 2
  end
end`)

	// Cursor on "x" in first function (line 1, col 12)
	occs := FindVariableOccurrences(src, 1, 12)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of 'x' in first/1, got %d", len(occs))
	}
	// Should only find x in first, not second
	for _, occ := range occs {
		if occ.Line >= 5 {
			t.Errorf("found occurrence in second function at line %d", occ.Line)
		}
	}
}

func TestFindVariableOccurrences_ModuleNameNotVariable(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    MyApp.transform(data)
  end
end`)

	// Cursor on "MyApp" (alias, not a variable)
	occs := FindVariableOccurrences(src, 2, 4)
	if occs != nil {
		t.Errorf("expected nil for module alias, got %d occurrences", len(occs))
	}
}

func TestFindVariableOccurrences_ModuleAttribute_BasicRename(t *testing.T) {
	src := []byte(`defmodule MyApp do
  @timeout 5000

  def run do
    Process.sleep(@timeout)
  end
end`)

	// Cursor on "timeout" in the definition "@timeout 5000" (line 1, col 3)
	occs := FindVariableOccurrences(src, 1, 3)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of @timeout (definition + reference), got %d", len(occs))
	}
	if occs[0].Line != 1 {
		t.Errorf("occ[0] line: expected 1 (@timeout 5000), got %d", occs[0].Line)
	}
	if occs[1].Line != 4 {
		t.Errorf("occ[1] line: expected 4 (@timeout reference), got %d", occs[1].Line)
	}
}

func TestFindVariableOccurrences_ModuleAttribute_CursorOnReference(t *testing.T) {
	src := []byte(`defmodule MyApp do
  @timeout 5000

  def run do
    Process.sleep(@timeout)
  end
end`)

	// Cursor on "timeout" in the reference "@timeout" inside the def (line 4, col 20)
	occs := FindVariableOccurrences(src, 4, 20)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences from reference cursor, got %d", len(occs))
	}
}

func TestFindVariableOccurrences_ModuleAttribute_DoesNotMatchPlainVariable(t *testing.T) {
	src := []byte(`defmodule MyApp do
  @timeout 5000

  def run do
    timeout = 1000
    Process.sleep(timeout)
  end
end`)

	// Cursor on "timeout" in "@timeout 5000" — should NOT find the plain variable
	occs := FindVariableOccurrences(src, 1, 3)
	if len(occs) != 1 {
		t.Fatalf("expected 1 occurrence (@timeout def only, no plain variable), got %d: %+v", len(occs), occs)
	}
}

func TestFindVariableOccurrences_CapturedByAnonymousFunction(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(items) do
    prefix = "hello"
    Enum.map(items, fn item -> prefix <> item end)
  end
end`)

	// Cursor on "prefix" at line 2 col 4
	occs := FindVariableOccurrences(src, 2, 4)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of 'prefix' (binding + captured ref), got %d", len(occs))
	}
	if occs[0].Line != 2 {
		t.Errorf("occ[0] line: expected 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 3 {
		t.Errorf("occ[1] line: expected 3, got %d", occs[1].Line)
	}
}

func TestFindVariableOccurrences_ShadowedInAnonymousFunction(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    x = 1
    fn x -> x + 1 end
    x
  end
end`)

	// Cursor on outer "x" at line 2 col 4
	occs := FindVariableOccurrences(src, 2, 4)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of outer 'x' (binding + final ref), got %d", len(occs))
	}
	// Should be line 2 (x = 1) and line 4 (bare x), NOT inside the fn
	if occs[0].Line != 2 {
		t.Errorf("occ[0] line: expected 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 4 {
		t.Errorf("occ[1] line: expected 4, got %d", occs[1].Line)
	}
}

func TestFindVariableOccurrences_InsideAnonymousFunction(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    x = 1
    fn x -> x + 1 end
    x
  end
end`)

	// Cursor on inner "x" (fn parameter) at line 3
	// "    fn x -> x + 1 end" — "x" parameter is at col 7
	occs := FindVariableOccurrences(src, 3, 7)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of inner 'x' (param + body ref), got %d", len(occs))
	}
	// Both should be on line 3
	for _, occ := range occs {
		if occ.Line != 3 {
			t.Errorf("expected occurrence on line 3, got line %d", occ.Line)
		}
	}
}

func TestFindVariableOccurrences_WithBlockScope(t *testing.T) {
	src := []byte(`defmodule M do
  def f() do
    thing = nil
    with {:ok, thing} <- fetch("something") do
      {:ok, thing}
    end
    thing = :something
  end
end`)

	// Cursor on outer "thing" (line 2: "thing = nil")
	occs := FindVariableOccurrences(src, 2, 4)
	// Should find: line 2 (thing = nil) and line 6 (thing = :something)
	// Should NOT find: anything inside the with block
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of outer 'thing', got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 2 {
		t.Errorf("occ[0]: expected line 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 6 {
		t.Errorf("occ[1]: expected line 6, got %d", occs[1].Line)
	}

	// Cursor on inner "thing" (line 4: "{:ok, thing}" in do block)
	innerOccs := FindVariableOccurrences(src, 4, 12)
	// Should find occurrences within the with scope only:
	// line 3 ({:ok, thing} pattern) and line 4 ({:ok, thing} body)
	if len(innerOccs) != 2 {
		t.Fatalf("expected 2 occurrences of inner 'thing', got %d: %+v", len(innerOccs), innerOccs)
	}
	for _, occ := range innerOccs {
		if occ.Line != 3 && occ.Line != 4 {
			t.Errorf("inner occ: expected line 3 or 4, got %d", occ.Line)
		}
	}
}

func TestFindVariableOccurrences_WithBlockExpressionSide(t *testing.T) {
	src := []byte(`defmodule M do
  def f() do
    thing = nil
    with {:ok, thing} <- fetch(thing) do
      thing
    end
    thing = :something
  end
end`)

	// Cursor on outer "thing" (line 2: "thing = nil")
	occs := FindVariableOccurrences(src, 2, 4)
	// Should find: line 2 (thing = nil), line 3 fetch(thing), line 6 (thing = :something)
	// Should NOT find: {:ok, thing} pattern or thing in do block
	if len(occs) != 3 {
		t.Fatalf("expected 3 occurrences of outer 'thing', got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 2 {
		t.Errorf("occ[0]: expected line 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 3 || occs[1].StartCol != 31 {
		t.Errorf("occ[1]: expected line 3 col 31 (fetch(thing)), got line %d col %d", occs[1].Line, occs[1].StartCol)
	}
	if occs[2].Line != 6 {
		t.Errorf("occ[2]: expected line 6, got %d", occs[2].Line)
	}
}

func TestFindVariableOccurrences_WithMultiClauseSequential(t *testing.T) {
	src := []byte(`defmodule M do
  def f() do
    thing = nil
    with {:ok, thing} <- fetch(thing),
         {:ok, other} <- bar(thing) do
      thing
    end
    thing = :something
  end
end`)

	// Cursor on outer "thing" (line 2)
	occs := FindVariableOccurrences(src, 2, 4)
	// Should find: line 2 (thing = nil), line 3 fetch(thing), line 7 (thing = :something)
	// Should NOT find: bar(thing) on line 4 — that refs the rebound thing
	// Should NOT find: thing in pattern or do block
	if len(occs) != 3 {
		t.Fatalf("expected 3 occurrences of outer 'thing', got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 2 {
		t.Errorf("occ[0]: expected line 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 3 {
		t.Errorf("occ[1]: expected line 3 (fetch(thing)), got line %d col %d", occs[1].Line, occs[1].StartCol)
	}
	if occs[2].Line != 7 {
		t.Errorf("occ[2]: expected line 7, got %d", occs[2].Line)
	}
}

func TestFindVariableOccurrences_ForBlockScope(t *testing.T) {
	src := []byte(`defmodule M do
  def f(items) do
    item = "default"
    for item <- items do
      process(item)
    end
    use(item)
  end
end`)

	// Cursor on outer "item" (line 2)
	occs := FindVariableOccurrences(src, 2, 4)
	// Should find: line 2 (item = "default") and line 6 (use(item))
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of outer 'item', got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 2 {
		t.Errorf("occ[0]: expected line 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 6 {
		t.Errorf("occ[1]: expected line 6, got %d", occs[1].Line)
	}
}

func TestFindVariablesInScope(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(user, account) do
    name = user.name
    email = user.email
    na
  end

  def other(x) do
    y = x + 1
  end
end`)

	// Cursor on "na" at line 4, col 5
	vars := FindVariablesInScope(src, 4, 5)
	if vars == nil {
		t.Fatal("expected variables, got nil")
	}

	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	// Should include function params and local variables
	for _, expected := range []string{"user", "account", "name", "email"} {
		if !varSet[expected] {
			t.Errorf("expected variable %q in scope", expected)
		}
	}

	// Should not include variables from other functions
	if varSet["x"] || varSet["y"] {
		t.Error("should not include variables from other function scopes")
	}

	// Should not include function names
	if varSet["process"] || varSet["other"] {
		t.Error("should not include function names")
	}
}

func TestFindVariablesInScope_CaseClauseBoundary(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(user) do
    claims = user.claims

    case fetch("test") do
      {:ok, company} ->
        company

      _ -> :error
    end
  end
end`)

	// Cursor in the second case clause (line 9: "_ -> :error")
	vars := FindVariablesInScope(src, 9, 14)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	// Should include function params and top-level variables
	if !varSet["user"] {
		t.Error("expected function param 'user'")
	}
	if !varSet["claims"] {
		t.Error("expected top-level variable 'claims'")
	}
	// Should NOT include variables from the other case clause
	if varSet["company"] {
		t.Error("should not include 'company' from a different case clause")
	}
}

func TestFindVariablesInScope_WithVariablesVisible(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(user) do
    with {:ok, thing} = fetch("something") do
      thi
    end
  end
end`)

	// Cursor inside the with's do block (line 3: "thi")
	vars := FindVariablesInScope(src, 3, 9)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	// Should include variables from the with pattern when cursor is inside
	if !varSet["thing"] {
		t.Error("expected 'thing' from with pattern to be visible in do block")
	}
	if !varSet["user"] {
		t.Error("expected function param 'user'")
	}
}

func TestFindVariablesInScope_WithVariablesDontLeak(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(user) do
    with {:ok, thing} <- fetch("something") do
      thing
    end

    us
  end
end`)

	// Cursor AFTER the with block (line 6: "us")
	vars := FindVariablesInScope(src, 6, 6)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	if !varSet["user"] {
		t.Error("expected function param 'user'")
	}
	if varSet["thing"] {
		t.Error("should not include 'thing' — with bindings don't leak to outer scope")
	}
}

func TestFindVariablesInScope_ForVariablesDontLeak(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(list) do
    for item <- list do
      item
    end

    li
  end
end`)

	// Cursor AFTER the for block (line 6: "li")
	vars := FindVariablesInScope(src, 6, 6)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	if !varSet["list"] {
		t.Error("expected function param 'list'")
	}
	if varSet["item"] {
		t.Error("should not include 'item' — for bindings don't leak to outer scope")
	}
}

func TestFindVariablesInScope_IfVariablesDontLeak(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(flag) do
    if flag do
      result = compute()
      result
    end

    fl
  end
end`)

	// Cursor AFTER the if block (line 7: "fl")
	vars := FindVariablesInScope(src, 7, 6)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	if !varSet["flag"] {
		t.Error("expected function param 'flag'")
	}
	if varSet["result"] {
		t.Error("should not include 'result' — if bindings don't leak to outer scope")
	}
}

func TestFindVariablesInScope_OnlyAboveCursor(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(user) do
    name = user.name
    na
    email = user.email
  end
end`)

	// Cursor on line 3 ("na") — should see name and user, but NOT email
	vars := FindVariablesInScope(src, 3, 6)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	if !varSet["user"] {
		t.Error("expected function param 'user'")
	}
	if !varSet["name"] {
		t.Error("expected 'name' defined above cursor")
	}
	if varSet["email"] {
		t.Error("should not include 'email' defined below cursor")
	}
}

func TestFindVariablesInScope_OutsideFunction(t *testing.T) {
	src := []byte(`defmodule MyModule do
  @attr "hello"
  at
end`)

	// Cursor at module level, not inside a function
	vars := FindVariablesInScope(src, 2, 4)
	if vars != nil {
		t.Errorf("expected nil for cursor outside function, got %v", vars)
	}
}
