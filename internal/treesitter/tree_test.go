package treesitter

import (
	"maps"
	"slices"
	"testing"
)

func TestNewTree(t *testing.T) {
	src := `def render(assigns) do
  ~H"""
  <div class={foo()}>
    <%= bar() %>
  </div>
  """
end`
	tree := NewTree([]byte(src))
	if tree.Language != LangElixir {
		t.Errorf("expected Elixir root tree, got %#v", tree.Language)
	}

	heexNodeIds := slices.Collect(maps.Keys(tree.Branches))
	if len(heexNodeIds) != 1 {
		t.Errorf("expected 1 Heex branch, got %d", len(heexNodeIds))
	}
	heexTree := tree.Branches[heexNodeIds[0]]
	if heexTree.Language != LangHeex {
		t.Errorf("expected Heex branch sub-tree, got %#v", heexTree.Language)
	}
	if rootId := heexTree.Root.Node.Id(); rootId != heexNodeIds[0] {
		t.Errorf("expected Heex root to match branch node ID %d, got %d", heexNodeIds[0], rootId)
	}
	wantHeex := "<div class={foo()}>\n    <%= bar() %>\n  </div>\n  "
	if heexText := heexTree.TrunkNode().Utf8Text([]byte(src)); heexText != wantHeex {
		t.Errorf("unexpected Heex text  (-want, +got)\n- %#v\n+ %#v", wantHeex, heexText)
	}

	exNodeIds := slices.Collect(maps.Keys(heexTree.Branches))
	if len(exNodeIds) != 2 {
		t.Errorf("expected 2 Elixir branch, got %d", len(exNodeIds))
	}
	for _, branch := range heexTree.Branches {
		if exText := branch.TrunkNode().Utf8Text([]byte(src)); !slices.Contains([]string{"foo()", "bar()"}, exText) {
			t.Errorf("unexpected nested Elixir text, got %#v", exText)
		}
	}
}

func TestTreeNode_ByteAndPosition(t *testing.T) {
	src := `def render(assigns) do
  ~H"""
  <div class={foo()}>
    <%= bar() %>
  </div>
  """
end`

	tree := NewTree([]byte(src))
	// bar() on line 4 col 8
	node := tree.TrunkNode().ChildAtPosition(3, 8)
	text := node.Utf8Text([]byte(src))
	if node.StartByte() != 61 {
		t.Errorf("expected %#v to start at byte %d, got %d", text, 61, node.StartByte())
	}
	if node.EndByte() != 64 {
		t.Errorf("expected %#v to end at byte %d, got %d", text, 64, node.EndByte())
	}

	if sp := node.StartPosition(); sp.Row != 3 || sp.Column != 8 {
		t.Errorf("expected %#v to start at position (Row: %d, Col: %d), got (Row: %d, Col: %d)", text, 0, 0, sp.Row, sp.Column)
	}
	if ep := node.EndPosition(); ep.Row != 3 || ep.Column != 11 {
		t.Errorf("expected %#v to end at position (Row: %d, Col: %d), got (Row: %d, Col: %d)", text, 0, 0, ep.Row, ep.Column)
	}
}

func TestTree_NestingRules(t *testing.T) {
	src := []byte(`def render(assigns) do
  ~H"""
  <%= cond do %>
    <% one() -> %>
      bar
    <% true -> %>
      oops
  <% end %>

  <%= if two() do %>
    hello!
  <% end
    three() %>

  <script><%= four() %>{five()}</script>
  <style><%= six() %>{seven()}</style>
  <div phx-no-curly-interpolation><%= eight() %>{nine()}</div>
  <.foo phx-no-curly-interpolation><% ten() %>{eleven()}</.foo>

  <div>{twelve()}</div>
  """
end`)

	tree := NewTree(src)
	branches := slices.Collect(maps.Values(tree.Branches))
	if len(branches) > 1 {
		t.Fatalf("expected root tree to have a single branch but got %d", len(branches))
	}

	// move into HEEX tree
	tree = branches[0]
	exprs := map[string]bool{}
	for _, branch := range tree.Branches {
		exprs[branch.TrunkNode().Utf8Text(src)] = true
	}

	want := map[string]bool{
		"cond do":          true,
		"one() ->":         true,
		"true ->":          true,
		"end":              true,
		"if two() do":      true,
		"end\n    three()": true,
		"four()":           true,
		"five()":           false,
		"six()":            true,
		"seven()":          false,
		"eight()":          true,
		"nine()":           false,
		"ten()":            true,
		"eleven()":         false,
		// A normal tag following the interpolation-suppressing tags above must
		// have its curly interpolation parsed: the suppressing tags must not leak
		// their `interpolate: false` state to later siblings.
		"twelve()": true,
	}
	for text, shouldParse := range want {
		if shouldParse && !exprs[text] {
			t.Errorf("expected %+v to be parsed but it was not", exprs)
		} else if !shouldParse && exprs[text] {
			t.Errorf("expected %+v not to be parsed but it was", exprs)
		}
		delete(exprs, text)
	}

	for text := range exprs {
		t.Errorf("unexpected expression parsed: %+v", text)
	}
}
