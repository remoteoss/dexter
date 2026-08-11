package treesitter

import (
	"slices"
	"testing"
)

// These tests cover the HEEX-aware variable scoping added on top of the
// pure-Elixir scope model (see heex_scope.go). HEEX binding constructs —
// EEx loops, case/cond clause arms, and :for/:let special attributes — parse
// as flat sibling directives rather than nested blocks, so their bindings must
// be modeled as byte ranges and occurrences filtered to the cursor's range.

// TestHEEX_LoopScope: a variable bound by `<%= for item <- @items do %>` is
// active only within the loop body; a same-named identifier after `<% end %>`
// belongs to a separate scope and must not be renamed together.
func TestHEEX_LoopScope(t *testing.T) {
	src := []byte(`defmodule Foo do
  def render(assigns) do
    ~H"""
    <%= for item <- @items do %>
      {item}
    <% end %>
    {item}
    """
  end
end
`)

	// Cursor on the loop binding `item` (line 3, col 12): binding site + body use.
	got := FindVariableOccurrences(src, 3, 12)
	want := []VariableOccurrence{
		{Line: 3, StartCol: 12, EndCol: 16}, // for item <- ...
		{Line: 4, StartCol: 7, EndCol: 11},  // {item} in body
	}
	if !slices.Equal(got, want) {
		t.Fatalf("loop binding: got %+v\nwant %+v", got, want)
	}

	// Cursor inside the body (line 4) resolves to the same loop scope.
	if got := FindVariableOccurrences(src, 4, 7); !slices.Equal(got, want) {
		t.Fatalf("loop body: got %+v\nwant %+v", got, want)
	}

	// Cursor on `item` after `<% end %>` (line 6): a separate scope, one occ only.
	gotAfter := FindVariableOccurrences(src, 6, 5)
	wantAfter := []VariableOccurrence{{Line: 6, StartCol: 5, EndCol: 9}}
	if !slices.Equal(gotAfter, wantAfter) {
		t.Fatalf("after loop: got %+v\nwant %+v", gotAfter, wantAfter)
	}
}

// TestHEEX_ClosingDirectiveTrailingCode: a closing directive may legally carry
// code after `end` (e.g. `<% end\n  log(item) %>`). That trailing code is a
// sibling AFTER the block, so a reference in it belongs to the outer scope, not
// the loop — the loop body range must end at the START of the `<% end %>`
// directive, not its end.
func TestHEEX_ClosingDirectiveTrailingCode(t *testing.T) {
	src := []byte(`defmodule Foo do
  def render(assigns) do
    ~H"""
    <%= for item <- @items do %>
      {item}
    <% end
      log(item) %>
    {item}
    """
  end
end
`)

	// Loop binding `item` (line 3): body use only, NOT the trailing log(item).
	got := FindVariableOccurrences(src, 3, 12)
	want := []VariableOccurrence{
		{Line: 3, StartCol: 12, EndCol: 16}, // for item <- ...
		{Line: 4, StartCol: 7, EndCol: 11},  // {item} in body
	}
	if !slices.Equal(got, want) {
		t.Fatalf("loop binding with trailing code: got %+v\nwant %+v", got, want)
	}

	// `item` in the trailing code after `end` (line 6) is outer-scope: it groups
	// with the post-template {item} (line 7), never the loop body.
	gotTrailing := FindVariableOccurrences(src, 6, 10)
	wantTrailing := []VariableOccurrence{
		{Line: 6, StartCol: 10, EndCol: 14}, // log(item) after end
		{Line: 7, StartCol: 5, EndCol: 9},   // {item} after template
	}
	if !slices.Equal(gotTrailing, wantTrailing) {
		t.Fatalf("trailing item: got %+v\nwant %+v", gotTrailing, wantTrailing)
	}
}

// TestHEEX_ShadowedOuterVariable: an outer Elixir `item` and a HEEX loop `item`
// that shadows it are scoped independently in both directions.
func TestHEEX_ShadowedOuterVariable(t *testing.T) {
	src := []byte(`defmodule Foo do
  def render(assigns) do
    item = 1
    ~H"""
    <%= for item <- @items do %>
      {item}
    <% end %>
    """
    item
  end
end
`)

	// Cursor on the outer binding (line 2): binding + the trailing use on line 8,
	// never the loop body.
	gotOuter := FindVariableOccurrences(src, 2, 4)
	wantOuter := []VariableOccurrence{
		{Line: 2, StartCol: 4, EndCol: 8}, // item = 1
		{Line: 8, StartCol: 4, EndCol: 8}, // item after the template
	}
	if !slices.Equal(gotOuter, wantOuter) {
		t.Fatalf("outer item: got %+v\nwant %+v", gotOuter, wantOuter)
	}

	// Cursor on the shadowing loop binding (line 4): confined to the loop body.
	gotInner := FindVariableOccurrences(src, 4, 12)
	wantInner := []VariableOccurrence{
		{Line: 4, StartCol: 12, EndCol: 16}, // for item <- ...
		{Line: 5, StartCol: 7, EndCol: 11},  // {item} in body
	}
	if !slices.Equal(gotInner, wantInner) {
		t.Fatalf("inner item: got %+v\nwant %+v", gotInner, wantInner)
	}
}

// TestHEEX_AdjacentCaseClauses: same-named bindings in adjacent case clause arms
// are isolated — renaming `x` in clause A must not touch `x` in clause B.
func TestHEEX_AdjacentCaseClauses(t *testing.T) {
	src := []byte(`defmodule Foo do
  def render(assigns) do
    ~H"""
    <%= case @v do %>
    <% {:a, x} -> %>
      {x}
    <% {:b, x} -> %>
      {x}
    <% end %>
    """
  end
end
`)

	// Clause A binding `x` (line 4): its arm only.
	gotA := FindVariableOccurrences(src, 4, 12)
	wantA := []VariableOccurrence{
		{Line: 4, StartCol: 12, EndCol: 13}, // {:a, x} ->
		{Line: 5, StartCol: 7, EndCol: 8},   // {x}
	}
	if !slices.Equal(gotA, wantA) {
		t.Fatalf("clause A: got %+v\nwant %+v", gotA, wantA)
	}

	// Clause B binding `x` (line 6): its arm only.
	gotB := FindVariableOccurrences(src, 6, 12)
	wantB := []VariableOccurrence{
		{Line: 6, StartCol: 12, EndCol: 13}, // {:b, x} ->
		{Line: 7, StartCol: 7, EndCol: 8},   // {x}
	}
	if !slices.Equal(gotB, wantB) {
		t.Fatalf("clause B: got %+v\nwant %+v", gotB, wantB)
	}
}

// TestHEEX_SpecialAttributeBindings: :for and :let bindings are confined to
// their enclosing element and do not leak to sibling elements.
func TestHEEX_SpecialAttributeBindings(t *testing.T) {
	src := []byte(`defmodule Foo do
  def render(assigns) do
    ~H"""
    <span :for={y <- @ys} :let={z}>{y}{z}</span>
    <div>{y}</div>
    """
  end
end
`)

	// :for binding `y` (line 3): the :for site + the {y} inside <span>, NOT the
	// {y} in the sibling <div>.
	gotY := FindVariableOccurrences(src, 3, 16)
	wantY := []VariableOccurrence{
		{Line: 3, StartCol: 16, EndCol: 17}, // :for={y <- ...}
		{Line: 3, StartCol: 36, EndCol: 37}, // {y} inside <span>
	}
	if !slices.Equal(gotY, wantY) {
		t.Fatalf(":for y: got %+v\nwant %+v", gotY, wantY)
	}

	// :let binding `z` (line 3): the :let site + the {z} inside <span> only.
	gotZ := FindVariableOccurrences(src, 3, 32)
	wantZ := []VariableOccurrence{
		{Line: 3, StartCol: 32, EndCol: 33}, // :let={z}
		{Line: 3, StartCol: 39, EndCol: 40}, // {z} inside <span>
	}
	if !slices.Equal(gotZ, wantZ) {
		t.Fatalf(":let z: got %+v\nwant %+v", gotZ, wantZ)
	}

	// The sibling <div>'s {y} (line 4) is a separate reference, one occ only.
	gotDiv := FindVariableOccurrences(src, 4, 10)
	wantDiv := []VariableOccurrence{{Line: 4, StartCol: 10, EndCol: 11}}
	if !slices.Equal(gotDiv, wantDiv) {
		t.Fatalf("sibling div y: got %+v\nwant %+v", gotDiv, wantDiv)
	}
}

// TestHEEX_CollisionRespectsRange: rename collision reach must equal rename
// reach. A newName that exists only outside the cursor variable's HEEX range is
// not a collision; one that exists inside it is.
func TestHEEX_CollisionRespectsRange(t *testing.T) {
	src := []byte(`defmodule Foo do
  def render(assigns) do
    outer = 1
    ~H"""
    <%= for item <- @items do %>
      {item}
    <% end %>
    {outer}
    """
  end
end
`)

	// Renaming the loop `item` (line 4, col 12) to `outer`: `outer` lives only in
	// the outer scope, invisible from inside the loop — NOT a collision.
	if tree := NewTree(src); tree != nil {
		defer tree.Close()
		if tree.NameExistsInScopeOf(src, 4, 12, "outer") {
			t.Error("false-positive collision: 'outer' is outside the loop's range")
		}
	}

	// Renaming the outer `outer` (line 2) to `item`: `item` lives only inside the
	// loop, invisible from the outer scope — NOT a collision.
	if tree := NewTree(src); tree != nil {
		defer tree.Close()
		if tree.NameExistsInScopeOf(src, 2, 4, "item") {
			t.Error("false-positive collision: 'item' is confined to the loop range")
		}
	}

	// A newName that IS reachable from the cursor variable's range is a real
	// collision: renaming the loop `item` to `taken`, which is bound in the body.
	collSrc := []byte(`defmodule Foo do
  def render(assigns) do
    ~H"""
    <%= for item <- @items do %>
      {taken = compute(item)}
      {taken}
    <% end %>
    """
  end
end
`)
	if tree := NewTree(collSrc); tree != nil {
		defer tree.Close()
		if !tree.NameExistsInScopeOf(collSrc, 3, 12, "taken") {
			t.Error("missed collision: 'taken' is bound inside the loop body")
		}
	}
}

// TestHEEX_AutocompleteScope: FindVariablesInScope surfaces in-scope loop/:for/
// :let/clause bindings and hides out-of-scope ones.
func TestHEEX_AutocompleteScope(t *testing.T) {
	loop := []byte(`defmodule Foo do
  def render(assigns) do
    ~H"""
    <%= for item <- @items do %>
      {item}
    <% end %>
    <p>done</p>
    """
  end
end
`)

	// Inside the loop body: `item` is visible.
	if got := FindVariablesInScope(loop, 4, 7); !slices.Contains(got, "item") {
		t.Errorf("inside loop: expected 'item' visible, got %+v", got)
	}
	// After `<% end %>`: `item` is out of scope and must not be offered.
	if got := FindVariablesInScope(loop, 6, 7); slices.Contains(got, "item") {
		t.Errorf("after loop: 'item' leaked into scope, got %+v", got)
	}

	attr := []byte(`defmodule Foo do
  def render(assigns) do
    ~H"""
    <span :for={y <- @ys} :let={z}>{y}{z}</span>
    <div>{y}</div>
    """
  end
end
`)

	// Inside <span>: both :for `y` and :let `z` are visible.
	inSpan := FindVariablesInScope(attr, 3, 33)
	if !slices.Contains(inSpan, "y") || !slices.Contains(inSpan, "z") {
		t.Errorf("inside span: expected 'y' and 'z' visible, got %+v", inSpan)
	}
	// In the sibling <div>: the element-scoped `z` must not leak.
	if got := FindVariablesInScope(attr, 4, 10); slices.Contains(got, "z") {
		t.Errorf("sibling div: ':let z' leaked into scope, got %+v", got)
	}
}
