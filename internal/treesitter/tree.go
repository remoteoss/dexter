package treesitter

import (
	"slices"
	"unsafe"

	tree_sitter_heex "github.com/phoenixframework/tree-sitter-heex/bindings/go"
	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_elixir "github.com/tree-sitter/tree-sitter-elixir/bindings/go"
)

// Tree contains a document trunk tree and a map of any branch sub-trees.
// For Elixir trunks, Branches is a map of `quoted_content` node IDs within sigils
// in the document tree to their corresponding HEEX sub-tree. For HEEX trunks,
// Branches is a map of `expression_value` node IDs within interpolated expressions
// in the document tree to their corresponding Elixir sub-tree. Sub-trees may
// be nested arbitrarily deep, though in practice it will typically be 1-3 levels.
//
// For nested sub-trees, Root points back to the parent tree that contains the
// sub-tree. Navigation is possible both up (using Parent()) and down (using Child(i)).
//
// Elixir->HEEX: (sigil (sigil_name) node: (quoted_content))
// HEEX->Elixir: (expression node: (expression_value))
type Tree struct {
	Root     *TreeNode
	Trunk    *tree_sitter.Tree
	Branches map[uintptr]*Tree
	Language Language
}

// TrunkNode returns a TreeNode pointing to the root node of the trunk.
func (t *Tree) TrunkNode() *TreeNode {
	return &TreeNode{Tree: t, Node: t.Trunk.RootNode()}
}

// Close recursively closes the trunk tree and any branch sub-trees.
func (t *Tree) Close() {
	for _, b := range t.Branches {
		b.Close()
	}
	t.Trunk.Close()
}

// TreeNode represents a node within a tree or sub-tree.
// This facilitates traversal between trunk trees and branch sub-trees.
type TreeNode struct {
	Tree *Tree
	Node *tree_sitter.Node
}

// See tree_sitter.Node.Kind().
func (tn *TreeNode) Kind() string {
	return tn.Node.Kind()
}

// See tree_sitter.Node.IsNamed().
func (tn *TreeNode) IsNamed() bool {
	return tn.Node.IsNamed()
}

// See tree_sitter.Node.ToSexp().
func (tn *TreeNode) ToSexp() string {
	return tn.Node.ToSexp()
}

// See tree_sitter.Node.StartByte().
func (tn *TreeNode) StartByte() uint {
	if tn.Tree.Root == nil {
		return tn.Node.StartByte()
	}
	return tn.Tree.Root.StartByte() + tn.Node.StartByte()
}

// See tree_sitter.Node.EndByte().
func (tn *TreeNode) EndByte() uint {
	if tn.Tree.Root == nil {
		return tn.Node.EndByte()
	}
	return tn.Tree.Root.StartByte() + tn.Node.EndByte()
}

// Parent returns the node containing the given node in the tree, or the node
// in the root tree that contains the node if the node is the root of a branch
// sub-tree. If the node is the top-most root, returns nil.
func (tn *TreeNode) Parent() *TreeNode {
	if parent := tn.Node.Parent(); parent != nil {
		return &TreeNode{Tree: tn.Tree, Node: parent}
	}
	return tn.Tree.Root
}

// ChildCount returns the number of children for the given node, returning
// 1 for nodes that link to a branch sub-tree.
func (tn *TreeNode) ChildCount() uint {
	if branch := tn.Tree.Branches[tn.Node.Id()]; branch != nil {
		return 1
	}
	return tn.Node.ChildCount()
}

// Child returns the tree/child of the given node, moving into a sub-tree if
// the node links to a branch sub-tree.
func (tn *TreeNode) Child(i uint) *TreeNode {
	if branch := tn.Tree.Branches[tn.Node.Id()]; branch != nil {
		return branch.TrunkNode()
	}
	return &TreeNode{Tree: tn.Tree, Node: tn.Node.Child(i)}
}

// RawChild returns the i-th child within the node's own tree, without crossing
// into a branch sub-tree (unlike Child, which transparently descends into
// Branches). Use it to walk one language's native structure — e.g. the HEEX
// tree's own markup nodes — without being pulled into the nested Elixir
// expression trees hanging off those nodes.
func (tn *TreeNode) RawChild(i uint) *TreeNode {
	return &TreeNode{Tree: tn.Tree, Node: tn.Node.Child(i)}
}

// RawChildCount returns the native tree-sitter child count, ignoring branch
// sub-trees (unlike ChildCount, which reports 1 at a node linking to a branch).
// It is the counterpart to RawChild: a `for i < RawChildCount()` loop bound to
// RawChild(i) walks one language's own structure without crossing into nested
// sub-trees.
func (tn *TreeNode) RawChildCount() uint {
	return tn.Node.ChildCount()
}

// NamedChild returns the i-th named child of the node (0-based over named
// children only), or nil if there are fewer than i+1. Like Child, it descends
// transparently into a branch sub-tree, so a named child of a node linking to a
// branch is that sub-tree's root.
func (tn *TreeNode) NamedChild(i uint) *TreeNode {
	var seen uint
	for j := uint(0); j < tn.ChildCount(); j++ {
		c := tn.Child(j)
		if !c.IsNamed() {
			continue
		}
		if seen == i {
			return c
		}
		seen++
	}
	return nil
}

// StartPosition returns the (row, col) start position of the given node
// within the top-most root tree.
func (tn *TreeNode) StartPosition() tree_sitter.Point {
	if tn.Tree.Root == nil {
		return tn.Node.StartPosition()
	}
	p := tn.Tree.Root.StartPosition()
	sp := tn.Node.StartPosition()
	p.Row += sp.Row
	if sp.Row == 0 {
		p.Column += sp.Column
	} else {
		p.Column = sp.Column
	}
	return p
}

// EndPosition returns the (row, col) end position of the given node
// within the top-most root tree.
func (tn *TreeNode) EndPosition() tree_sitter.Point {
	if tn.Tree.Root == nil {
		return tn.Node.EndPosition()
	}
	p := tn.Tree.Root.StartPosition()
	ep := tn.Node.EndPosition()
	p.Row += ep.Row
	if ep.Row == 0 {
		p.Column += ep.Column
	} else {
		p.Column = ep.Column
	}
	return p
}

// Utf8Text returns the UTF-8 encoded string representation of the given node
// within the top-most root tree.
func (tn *TreeNode) Utf8Text(src []byte) string {
	return string(src[tn.StartByte():tn.EndByte()])
}

// ContainsPosition returns true if the node contains the given position
// in the top-most root tree. Tree-sitter end positions are exclusive,
// consistent with nodeAtPosition.
func (tn *TreeNode) ContainsPosition(line, col uint) bool {
	start := tn.StartPosition()
	end := tn.EndPosition()
	if line < uint(start.Row) || line > uint(end.Row) {
		return false
	}
	if line == uint(start.Row) && col < uint(start.Column) {
		return false
	}
	if line == uint(end.Row) && col >= uint(end.Column) {
		return false
	}
	return true
}

// ChildAtPosition find the deepest (most specific) child node at the given position
// within the top-most root tree.
func (tn *TreeNode) ChildAtPosition(line, col uint) *TreeNode {
	// Check if position is within this node
	if tn == nil || !tn.ContainsPosition(line, col) {
		return nil
	}

	// Try to find a more specific child
	for i := uint(0); i < tn.ChildCount(); i++ {
		if found := tn.Child(i).ChildAtPosition(line, col); found != nil {
			return found
		}
	}

	return tn
}

// NewTree creates parsers, parses src, parses nested HEEX templates, and returns the created trees.
// Used by the standalone (non-cached) entry points. Returns nil on failure.
func NewTree(src []byte) *Tree {
	parsers := AllParsers()
	if parsers == nil {
		return nil
	}
	for _, p := range parsers {
		defer p.Close()
	}
	return NewTreeWithParsers(src, parsers)
}

// NewTreeWithParsers parses src, parses nested HEEX templates, and returns the created trees.
// Used by cached entry points . Returns nil on failure.
func NewTreeWithParsers(src []byte, parsers map[Language]*tree_sitter.Parser) *Tree {
	return newTree(LangElixir, src, parsers)
}

func newTree(lang Language, src []byte, parsers map[Language]*tree_sitter.Parser) *Tree {
	trunk := parsers[lang].Parse(src, nil)
	if trunk == nil {
		return nil
	}

	t := &Tree{
		Language: lang,
		Trunk:    trunk,
		Branches: make(map[uintptr]*Tree),
	}

	visitTree(trunk.RootNode(), &parseVisitor{parsers: parsers, src: src, tree: t})

	return t
}

type parseVisitor struct {
	parsers map[Language]*tree_sitter.Parser
	src     []byte
	tree    *Tree
	tags    []parseVisitorTag
}

// See `heexTag` / `heexTagStack` in `tokenizer.go`; this follows the same approach.
type parseVisitorTag struct {
	name        []byte
	interpolate bool
}

func (v *parseVisitor) currentTag() *parseVisitorTag {
	if len(v.tags) == 0 {
		return nil
	}
	return &v.tags[len(v.tags)-1]
}

func (v *parseVisitor) curlyInterpolate() bool {
	if cur := v.currentTag(); cur != nil {
		return cur.interpolate
	}
	return true
}

func (v *parseVisitor) onNode(node *tree_sitter.Node) {
	// When visiting Elixir trees, parse nested ~H sigils as HEEX sub-trees.
	if v.tree.Language == LangElixir &&
		node.Kind() == "quoted_content" &&
		node.Parent() != nil && node.Parent().Kind() == "sigil" &&
		/* sigil_name */ node.PrevNamedSibling() != nil && node.PrevNamedSibling().Utf8Text(v.src) == "H" {
		if tree := newTree(LangHeex, v.src[node.StartByte():node.EndByte()], v.parsers); tree != nil {
			tree.Root = &TreeNode{Tree: v.tree, Node: node}
			v.tree.Branches[node.Id()] = tree
		}
	}

	// When visiting HEEX trees, parse nested expressions as Elixir sub-trees;
	// EEx directives (<% and <%=) are always parsed, while {..} curly interpolation
	// is only parsed when enabled by the surrounding context. It's enabled by default
	// and disabled within <script>/<style> tags and within any tags with the special
	// `phx-no-curly-interpolation` attribute. Curly interpolation is always enabled
	// on the tag itself e.g. "<div phx-no-curly-interpolation id={component_id()} />".
	if v.tree.Language == LangHeex &&
		node.Kind() == "partial_expression_value" || node.Kind() == "ending_expression_value" ||
		(node.Kind() == "expression_value" && (node.Parent().Kind() == "directive" || v.curlyInterpolate())) {
		if tree := newTree(LangElixir, v.src[node.StartByte():node.EndByte()], v.parsers); tree != nil {
			tree.Root = &TreeNode{Tree: v.tree, Node: node}
			v.tree.Branches[node.Id()] = tree
		}
	}
}

func (v *parseVisitor) onEnter(node *tree_sitter.Node) {
	if node.Kind() == "tag" {
		if startTag := node.NamedChild(0); startTag != nil && startTag.Kind() == "start_tag" {
			if tagNameNode := startTag.NamedChild(0); tagNameNode != nil {
				tagName := v.src[tagNameNode.StartByte():tagNameNode.EndByte()]
				ignoreContents := slices.Equal(tagName, []byte("script")) || slices.Equal(tagName, []byte("style"))
				hasNoInterpolateAttr := hasAttr(startTag, v.src, []byte("phx-no-curly-interpolation"))
				interpolate := v.curlyInterpolate() && !ignoreContents && !hasNoInterpolateAttr
				v.tags = append(v.tags, parseVisitorTag{name: tagName, interpolate: interpolate})
			}
		}
	} else if node.Kind() == "component" {
		if startComp := node.NamedChild(0); startComp != nil && startComp.Kind() == "start_component" {
			if compNameNode := startComp.NamedChild(0); compNameNode != nil {
				compName := v.src[compNameNode.StartByte():compNameNode.EndByte()]
				hasNoInterpolateAttr := hasAttr(startComp, v.src, []byte("phx-no-curly-interpolation"))
				interpolate := v.curlyInterpolate() && !hasNoInterpolateAttr
				v.tags = append(v.tags, parseVisitorTag{name: compName, interpolate: interpolate})
			}
		}
	}
}

func (v *parseVisitor) onLeave(node *tree_sitter.Node) {
	if len(v.tags) > 0 && (node.Kind() == "tag" || node.Kind() == "component") {
		v.tags = v.tags[:len(v.tags)-1]
	}
}

// Returns true if the given start_tag/start_component node has the given attribute.
func hasAttr(node *tree_sitter.Node, src, attr []byte) bool {
	for i := range node.ChildCount() {
		if c := node.Child(i); c.Kind() == "attribute" {
			if an := c.NamedChild(0); an != nil && an.Kind() == "attribute_name" {
				if slices.Equal(src[an.StartByte():an.EndByte()], attr) {
					return true
				}
			}
		}
	}
	return false
}

type Language byte

const (
	LangElixir Language = iota
	LangHeex
)

func NewParser(lang Language) *tree_sitter.Parser {
	var language unsafe.Pointer
	switch lang {
	case LangElixir:
		language = tree_sitter_elixir.Language()
	case LangHeex:
		language = tree_sitter_heex.Language()
	}

	p := tree_sitter.NewParser()
	if err := p.SetLanguage(tree_sitter.NewLanguage(language)); err != nil {
		return nil
	}

	return p
}

func AllParsers() map[Language]*tree_sitter.Parser {
	parsers := make(map[Language]*tree_sitter.Parser)

	for _, l := range []Language{LangElixir, LangHeex} {
		p := NewParser(l)
		if p == nil {
			// if a parser fails to initialize, close any already-opened parsers
			for _, pp := range parsers {
				pp.Close()
			}
			return nil
		}
		parsers[l] = p
	}

	return parsers
}

type visitor interface {
	onNode(node *tree_sitter.Node)
	onEnter(node *tree_sitter.Node)
	onLeave(node *tree_sitter.Node)
}

func visitTree(root *tree_sitter.Node, v visitor) {
	cursor := root.Walk()
	defer cursor.Close()

	for {
		// visit current node
		v.onEnter(cursor.Node())
		v.onNode(cursor.Node())

		// traverse down one level, if possible
		if cursor.GotoFirstChild() {
			continue
		}

		// traverse via siblings, if possible
		for !cursor.GotoNextSibling() {
			// move back up and recurse, returning once we're back to the root
			v.onLeave(cursor.Node())
			if !cursor.GotoParent() {
				return
			}
		}
	}
}
