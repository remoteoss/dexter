package treesitter

import (
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
)

// EnclosingBlockPath parses src and returns the calls whose do blocks enclose the
// cursor. See EnclosingBlockPathWithTree.
func EnclosingBlockPath(src []byte, line, col uint) []string {
	root, cleanup := parseElixir(src)
	if root == nil {
		return nil
	}
	defer cleanup()
	return EnclosingBlockPathWithTree(root, src, line, col)
}

// EnclosingBlockPathWithTree returns the names of the bare calls whose do blocks
// enclose the given position, outermost first. For a cursor inside
//
//	defmodule M do
//	  code_interface do
//	    defin█
//	  end
//	end
//
// it returns ["defmodule", "code_interface"].
//
// Every enclosing do block is reported, including language forms like defmodule,
// def and if, because deciding which of them are meaningful is the caller's job
// and depends on information the tree does not have. Callers resolve the longest
// trailing suffix that means something, which drops the language forms without a
// hardcoded skip list that would have to be kept in step with the grammar.
//
// Only bare calls are reported. A qualified call such as Foo.bar do ... end names
// its target explicitly, so there is nothing to infer from the enclosing scope.
func EnclosingBlockPathWithTree(root *tree_sitter.Node, src []byte, line, col uint) []string {
	node := deepestNodeAtInclusive(root, line, col)
	if node == nil {
		return nil
	}

	var innermost []string
	for current := node; current != nil; current = current.Parent() {
		if current.Kind() != "call" {
			continue
		}
		if !pointInsideDoBlock(current, line, col) {
			continue
		}
		if name := bareCallName(current, src); name != "" {
			innermost = append(innermost, name)
		}
	}
	if len(innermost) == 0 {
		return nil
	}

	path := make([]string, len(innermost))
	for i, name := range innermost {
		path[len(innermost)-1-i] = name
	}
	return path
}

// bareCallName returns the function name of a call written without a module
// target, or "" for qualified and parenthesised-dot forms.
func bareCallName(call *tree_sitter.Node, src []byte) string {
	if call.ChildCount() == 0 {
		return ""
	}
	first := call.Child(0)
	if first.Kind() != "identifier" {
		return ""
	}
	return string(src[first.StartByte():first.EndByte()])
}

// pointInsideDoBlock reports whether the cursor sits within one of call's do
// block bodies.
//
// The body is the span between the `do` and `end` keywords, and the end is
// exclusive. Testing the cursor point rather than the byte range of whatever node
// the descent landed on matters at both edges: a cursor resting on or just after
// `end` is outside the block for scoping purposes even though the `end` token
// falls within the do_block node, while a cursor at column zero of a line inside
// a nested block lands on the do_block node itself and would otherwise be
// excluded by its own start byte.
func pointInsideDoBlock(call *tree_sitter.Node, line, col uint) bool {
	point := tree_sitter.Point{Row: line, Column: col}
	for i := uint(0); i < call.ChildCount(); i++ {
		block := call.Child(i)
		if block.Kind() != "do_block" {
			continue
		}
		start, end := doBlockBody(block)
		if !pointBefore(point, start) && pointBefore(point, end) {
			return true
		}
	}
	return false
}

// doBlockBody returns the span between a do_block's `do` and `end` keywords,
// falling back to the whole node when either is absent (malformed code).
func doBlockBody(block *tree_sitter.Node) (start, end tree_sitter.Point) {
	start, end = block.StartPosition(), block.EndPosition()
	for i := uint(0); i < block.ChildCount(); i++ {
		child := block.Child(i)
		switch child.Kind() {
		case "do":
			start = child.EndPosition()
		case "end":
			end = child.StartPosition()
		}
	}
	return start, end
}

func pointBefore(a, b tree_sitter.Point) bool {
	if a.Row != b.Row {
		return a.Row < b.Row
	}
	return a.Column < b.Column
}

// deepestNodeAtInclusive is nodeAtPosition with the end position treated as
// inclusive. A completion cursor sits immediately after the text just typed, so
// at that column the identifier being completed has already ended and the strict
// variant would climb out to an ancestor — or return nothing at end of line.
func deepestNodeAtInclusive(node *tree_sitter.Node, line, col uint) *tree_sitter.Node {
	if node == nil {
		return nil
	}
	start, end := node.StartPosition(), node.EndPosition()
	if line < uint(start.Row) || line > uint(end.Row) {
		return nil
	}
	if line == uint(start.Row) && col < uint(start.Column) {
		return nil
	}
	if line == uint(end.Row) && col > uint(end.Column) {
		return nil
	}
	for i := uint(0); i < node.ChildCount(); i++ {
		if found := deepestNodeAtInclusive(node.Child(i), line, col); found != nil {
			return found
		}
	}
	return node
}
