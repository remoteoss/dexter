package treesitter

import (
	"strings"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_elixir "github.com/tree-sitter/tree-sitter-elixir/bindings/go"
)

// parseElixir creates a parser, parses src, and returns the root node plus a
// cleanup function that closes both tree and parser. Used by the standalone
// (non-cached) entry points. Returns (nil, nil) on failure.
func parseElixir(src []byte) (root *tree_sitter.Node, cleanup func()) {
	p := tree_sitter.NewParser()
	if err := p.SetLanguage(tree_sitter.NewLanguage(tree_sitter_elixir.Language())); err != nil {
		p.Close()
		return nil, nil
	}
	tree := p.Parse(src, nil)
	return tree.RootNode(), func() {
		tree.Close()
		p.Close()
	}
}

// VariableOccurrence is a position where a variable name appears.
type VariableOccurrence struct {
	Line     uint // 0-based
	StartCol uint // 0-based
	EndCol   uint // 0-based, exclusive
}

// FindVariableOccurrences parses src with tree-sitter and returns all
// occurrences of the variable at the given cursor position within the
// enclosing function scope. Returns nil if the cursor is not on a variable.
func FindVariableOccurrences(src []byte, line, col uint) []VariableOccurrence {
	root, cleanup := parseElixir(src)
	if root == nil {
		return nil
	}
	defer cleanup()
	return FindVariableOccurrencesWithTree(root, src, line, col)
}

// FindVariableOccurrencesWithTree is like FindVariableOccurrences but uses a
// pre-parsed tree root, avoiding redundant parsing when a cached tree exists.
func FindVariableOccurrencesWithTree(root *tree_sitter.Node, src []byte, line, col uint) []VariableOccurrence {
	cursorNode := nodeAtPosition(root, line, col)
	if cursorNode == nil {
		return nil
	}

	if cursorNode.Kind() != "identifier" {
		return nil
	}

	varName := cursorNode.Utf8Text(src)

	if isDefinitionKeyword(varName) {
		return nil
	}

	// Module attribute (@foo or @foo value): scope is the enclosing defmodule.
	if isModuleAttributeIdent(cursorNode, src) {
		scope := findEnclosingModule(cursorNode, src)
		if scope == nil {
			return nil
		}
		var occurrences []VariableOccurrence
		collectModuleAttributeOccurrences(scope, src, varName, &occurrences)
		return occurrences
	}

	// Check it's actually a variable — not a function name in a call or def keyword
	if isFunctionNameInCall(cursorNode, src) {
		return nil
	}

	// Find the enclosing scope: a stab_clause that binds this variable, or
	// the enclosing def/defp/defmacro/test call.
	scope := findEnclosingScope(cursorNode, src, varName)
	if scope == nil {
		return nil
	}

	// Collect all identifier nodes with the same name in the scope.
	// Skip the scope check for the root when the scope itself is a
	// stab_clause or a call-with-do_block that rebinds this variable
	// (we already determined it's our scope boundary).
	var occurrences []VariableOccurrence
	skipRoot := scope.Kind() == "stab_clause" ||
		(scope.Kind() == "call" && callHasDoBlock(scope) && callArgumentPatternsBindVariable(scope, src, varName))
	collectVariableOccurrences(scope, src, varName, &occurrences, skipRoot)
	return occurrences
}

// nodeAtPosition finds the deepest (most specific) node at the given position.
func nodeAtPosition(node *tree_sitter.Node, line, col uint) *tree_sitter.Node {
	if node == nil {
		return nil
	}
	start := node.StartPosition()
	end := node.EndPosition()

	// Check if position is within this node
	if line < uint(start.Row) || line > uint(end.Row) {
		return nil
	}
	if line == uint(start.Row) && col < uint(start.Column) {
		return nil
	}
	if line == uint(end.Row) && col >= uint(end.Column) {
		return nil
	}

	// Try to find a more specific child
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		child := node.Child(i)
		if found := nodeAtPosition(child, line, col); found != nil {
			return found
		}
	}

	return node
}

// isFunctionNameInCall returns true if the identifier is the function name
// in a call expression (e.g., `foo` in `foo(args)`) or a function name being
// defined (e.g., `foo` in `def foo(args) do`).
func isFunctionNameInCall(node *tree_sitter.Node, src []byte) bool {
	parent := node.Parent()
	if parent == nil {
		return false
	}

	// Direct function call: identifier is the first child of a `call`
	if parent.Kind() == "call" {
		if parent.ChildCount() > 0 {
			first := parent.Child(0)
			if first.StartPosition().Row == node.StartPosition().Row &&
				first.StartPosition().Column == node.StartPosition().Column {
				return true
			}
		}
	}

	// Function definition: identifier is inside the `arguments` of a def/defp/etc call.
	// e.g., `def list_users do` → call("def", arguments(identifier("list_users")), do_block)
	// or `def list_users(x) do` → call("def", arguments(call("list_users", ...)), do_block)
	if parent.Kind() == "arguments" {
		grandparent := parent.Parent()
		if grandparent != nil && grandparent.Kind() == "call" && grandparent.ChildCount() > 0 {
			defName := grandparent.Child(0)
			if defName.Kind() == "identifier" && functionKeywords[defName.Utf8Text(src)] {
				return true
			}
		}
	}

	return false
}

var defKeywords = map[string]bool{
	"def": true, "defp": true, "defmacro": true, "defmacrop": true,
	"defguard": true, "defguardp": true, "defdelegate": true,
	"defmodule": true, "defprotocol": true, "defimpl": true,
	"defstruct": true, "defexception": true,
	"describe": true, "test": true, "setup": true,
	"import": true, "alias": true, "use": true, "require": true,
}

// functionKeywords are the def-family keywords that define function scopes.
var functionKeywords = map[string]bool{
	"def": true, "defp": true, "defmacro": true, "defmacrop": true,
	"defguard": true, "defguardp": true,
	"test": true,
}

func isDefinitionKeyword(name string) bool {
	return defKeywords[name]
}

// findEnclosingScope walks up from node to find the nearest scope boundary
// for varName. A stab_clause (fn/case arm) that binds varName in its
// arguments is a scope boundary. A call with do_block (with/for/etc.) whose
// argument patterns rebind varName is also a scope boundary. Otherwise, the
// enclosing def/defp/defmacro/test call is the scope.
func findEnclosingScope(node *tree_sitter.Node, src []byte, varName string) *tree_sitter.Node {
	current := node.Parent()
	for current != nil {
		if current.Kind() == "stab_clause" && stabBindsVariable(current, src, varName) {
			return current
		}
		if current.Kind() == "call" && current.ChildCount() > 0 {
			firstChild := current.Child(0)
			if firstChild.Kind() == "identifier" && functionKeywords[firstChild.Utf8Text(src)] {
				return current
			}
			// with/for/etc. that rebind this variable in argument patterns
			if callHasDoBlock(current) && callArgumentPatternsBindVariable(current, src, varName) {
				return current
			}
		}
		current = current.Parent()
	}
	return nil
}

// collectVariableOccurrences recursively collects identifier nodes matching
// varName within the given subtree, skipping function names in calls.
// skipScopeCheck should be true when node is the scope root itself (so we
// don't immediately bail out of the scope we chose).
func collectVariableOccurrences(node *tree_sitter.Node, src []byte, varName string, out *[]VariableOccurrence, skipScopeCheck bool) {
	if node == nil {
		return
	}

	if node.Kind() == "identifier" {
		if node.Utf8Text(src) == varName && !isFunctionNameInCall(node, src) && !isDefinitionKeyword(varName) {
			*out = append(*out, VariableOccurrence{
				Line:     uint(node.StartPosition().Row),
				StartCol: uint(node.StartPosition().Column),
				EndCol:   uint(node.EndPosition().Column),
			})
		}
	}

	if !skipScopeCheck {
		// Skip nested stab_clauses that rebind this variable — they create a
		// new scope for it. If the variable is NOT rebound, descend — it's a
		// captured reference from the outer scope and must be renamed together.
		if node.Kind() == "stab_clause" && stabBindsVariable(node, src, varName) {
			return
		}

		// Call nodes with do_block (with/for/etc.) that rebind this variable in
		// their argument patterns: the do_block and pattern sides use a new
		// binding, but the expression sides (right of =/←) still reference
		// the outer variable and must be traversed.
		if node.Kind() == "call" && callHasDoBlock(node) && callArgumentPatternsBindVariable(node, src, varName) {
			collectPatternExpressionOccurrences(node, src, varName, out)
			return
		}
	}

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		collectVariableOccurrences(node.Child(i), src, varName, out, false)
	}
}

// isModuleAttributeIdent returns true if the identifier is the name part of a
// module attribute expression. Tree-sitter represents these as:
//
//	@foo       → unary_operator("@") → identifier("foo")
//	@foo value → unary_operator("@") → call → identifier("foo") …
func isModuleAttributeIdent(node *tree_sitter.Node, src []byte) bool {
	parent := node.Parent()
	if parent == nil {
		return false
	}
	if isAtUnaryOp(parent, src) {
		return true
	}
	// @attr value: identifier is the first child of a call whose parent is @
	if parent.Kind() == "call" {
		if grandparent := parent.Parent(); grandparent != nil && isAtUnaryOp(grandparent, src) {
			if parent.ChildCount() > 0 && parent.Child(0).StartByte() == node.StartByte() {
				return true
			}
		}
	}
	return false
}

// isAtUnaryOp returns true if node is a unary_operator with the @ operator.
func isAtUnaryOp(node *tree_sitter.Node, src []byte) bool {
	if node.Kind() != "unary_operator" {
		return false
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		child := node.Child(i)
		if !child.IsNamed() && child.EndByte() > child.StartByte() && src[child.StartByte()] == '@' {
			return true
		}
	}
	return false
}

// findEnclosingModule walks up from node to find the nearest defmodule call.
func findEnclosingModule(node *tree_sitter.Node, src []byte) *tree_sitter.Node {
	current := node.Parent()
	for current != nil {
		if current.Kind() == "call" && current.ChildCount() > 0 {
			first := current.Child(0)
			if first.Kind() == "identifier" && first.Utf8Text(src) == "defmodule" {
				return current
			}
		}
		current = current.Parent()
	}
	return nil
}

// collectModuleAttributeOccurrences collects all @attrName occurrences within
// the subtree — that is, identifier nodes named attrName that are part of a
// module attribute expression (@attrName or @attrName value).
func collectModuleAttributeOccurrences(node *tree_sitter.Node, src []byte, attrName string, out *[]VariableOccurrence) {
	if node == nil {
		return
	}
	if node.Kind() == "identifier" && node.Utf8Text(src) == attrName && isModuleAttributeIdent(node, src) {
		*out = append(*out, VariableOccurrence{
			Line:     uint(node.StartPosition().Row),
			StartCol: uint(node.StartPosition().Column),
			EndCol:   uint(node.EndPosition().Column),
		})
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		collectModuleAttributeOccurrences(node.Child(i), src, attrName, out)
	}
}

// FindTokenOccurrences parses src with tree-sitter and returns positions of
// all identifier or alias nodes whose text matches token. Unlike a plain
// string search, this naturally skips strings, comments, atoms, and other
// non-code contexts.
func FindTokenOccurrences(src []byte, token string) []VariableOccurrence {
	root, cleanup := parseElixir(src)
	if root == nil {
		return nil
	}
	defer cleanup()
	return FindTokenOccurrencesWithTree(root, src, token)
}

// FindTokenOccurrencesWithTree is like FindTokenOccurrences but uses a
// pre-parsed tree root.
func FindTokenOccurrencesWithTree(root *tree_sitter.Node, src []byte, token string) []VariableOccurrence {
	var occurrences []VariableOccurrence
	collectTokenOccurrences(root, src, token, &occurrences)
	return occurrences
}

func collectTokenOccurrences(node *tree_sitter.Node, src []byte, token string, out *[]VariableOccurrence) {
	if node == nil {
		return
	}

	kind := node.Kind()

	// Skip subtrees that can't contain meaningful identifier references
	if kind == "string" || kind == "comment" || kind == "sigil" || kind == "charlist" {
		return
	}

	if kind == "identifier" && node.Utf8Text(src) == token {
		*out = append(*out, VariableOccurrence{
			Line:     uint(node.StartPosition().Row),
			StartCol: uint(node.StartPosition().Column),
			EndCol:   uint(node.EndPosition().Column),
		})
	}

	// Alias nodes may contain dotted names like "MyApp.Repo". Match if the
	// full text equals token, or if a dot-separated segment matches. When a
	// segment matches, report only that segment's column range.
	if kind == "alias" {
		text := node.Utf8Text(src)
		if text == token {
			*out = append(*out, VariableOccurrence{
				Line:     uint(node.StartPosition().Row),
				StartCol: uint(node.StartPosition().Column),
				EndCol:   uint(node.EndPosition().Column),
			})
		} else {
			startCol := uint(node.StartPosition().Column)
			offset := uint(0)
			for _, segment := range strings.Split(text, ".") {
				if segment == token {
					*out = append(*out, VariableOccurrence{
						Line:     uint(node.StartPosition().Row),
						StartCol: startCol + offset,
						EndCol:   startCol + offset + uint(len(token)),
					})
				}
				offset += uint(len(segment)) + 1 // +1 for the dot
			}
		}
	}

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		collectTokenOccurrences(node.Child(i), src, token, out)
	}
}

// FindVariablesInScope parses src with tree-sitter and returns all unique
// variable names visible at the given cursor position within the enclosing
// function scope. Respects clause boundaries: variables from other case/fn
// clauses are excluded. Returns nil if the cursor is not inside a function.
func FindVariablesInScope(src []byte, line, col uint) []string {
	root, cleanup := parseElixir(src)
	if root == nil {
		return nil
	}
	defer cleanup()
	return FindVariablesInScopeWithTree(root, src, line, col)
}

// FindVariablesInScopeWithTree is like FindVariablesInScope but uses a
// pre-parsed tree root.
func FindVariablesInScopeWithTree(root *tree_sitter.Node, src []byte, line, col uint) []string {
	cursorNode := nodeAtPosition(root, line, col)
	if cursorNode == nil && col > 0 {
		cursorNode = nodeAtPosition(root, line, col-1)
	}
	if cursorNode == nil {
		return nil
	}

	scope := findEnclosingFunction(cursorNode, src)
	if scope == nil {
		return nil
	}

	seen := make(map[string]bool)
	var variables []string
	collectVariableNames(scope, src, seen, &variables, line, col)
	return variables
}

// findEnclosingFunction walks up from node to find the nearest def/defp/etc scope.
func findEnclosingFunction(node *tree_sitter.Node, src []byte) *tree_sitter.Node {
	current := node.Parent()
	for current != nil {
		if current.Kind() == "call" && current.ChildCount() > 0 {
			firstChild := current.Child(0)
			if firstChild.Kind() == "identifier" && functionKeywords[firstChild.Utf8Text(src)] {
				return current
			}
		}
		current = current.Parent()
	}
	return nil
}

// collectVariableNames collects unique variable identifier names within a subtree,
// excluding function names, definition keywords, and module attributes.
// Skips stab_clauses and do..end calls that don't contain the cursor,
// since variables don't leak out of those scopes in Elixir.
func collectVariableNames(node *tree_sitter.Node, src []byte, seen map[string]bool, out *[]string, cursorLine, cursorCol uint) {
	if node == nil {
		return
	}

	if !nodeContainsPosition(node, cursorLine, cursorCol) {
		// Variables in other case/fn clauses are not in scope.
		if node.Kind() == "stab_clause" {
			return
		}
		// Variables inside any do..end block don't leak to the outer scope.
		if node.Kind() == "call" && callHasDoBlock(node) {
			return
		}
	}

	if node.Kind() == "identifier" {
		// Only collect variables that appear before the cursor position.
		pos := node.StartPosition()
		beforeCursor := uint(pos.Row) < cursorLine || (uint(pos.Row) == cursorLine && uint(pos.Column) < cursorCol)
		if beforeCursor {
			name := node.Utf8Text(src)
			if !seen[name] && !isFunctionNameInCall(node, src) && !isDefinitionKeyword(name) && !isModuleAttributeIdent(node, src) {
				seen[name] = true
				*out = append(*out, name)
			}
		}
	}

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		collectVariableNames(node.Child(i), src, seen, out, cursorLine, cursorCol)
	}
}

// collectPatternExpressionOccurrences traverses the expression (right) sides
// of =/← binary operators in a call's arguments, processing clauses
// sequentially. Once a clause's pattern (left side) rebinds varName,
// subsequent clauses and the do_block use the new binding — so we stop.
func collectPatternExpressionOccurrences(callNode *tree_sitter.Node, src []byte, varName string, out *[]VariableOccurrence) {
	for i := uint(0); i < uint(callNode.ChildCount()); i++ {
		child := callNode.Child(i)
		if child.Kind() != "arguments" {
			continue
		}
		for j := uint(0); j < uint(child.ChildCount()); j++ {
			arg := child.Child(j)
			if arg.Kind() == "binary_operator" && arg.ChildCount() >= 3 {
				op := arg.Child(1).Utf8Text(src)
				if op == "=" || op == "<-" {
					// Right side is evaluated before the pattern match,
					// so it still references the outer variable.
					collectVariableOccurrences(arg.Child(2), src, varName, out, false)
					// If the left (pattern) side rebinds our variable,
					// all subsequent clauses use the new binding — stop.
					if subtreeContainsIdentifier(arg.Child(0), src, varName) {
						return
					}
					continue
				}
			}
			// Not a pattern operator (e.g. filter in for) — traverse normally
			collectVariableOccurrences(arg, src, varName, out, false)
		}
	}
}

// callArgumentPatternsBindVariable checks whether a call's argument patterns
// (left side of = or <- operators) contain a binding of varName.
func callArgumentPatternsBindVariable(node *tree_sitter.Node, src []byte, varName string) bool {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Kind() != "arguments" {
			continue
		}
		for j := uint(0); j < uint(child.ChildCount()); j++ {
			arg := child.Child(j)
			if arg.Kind() == "binary_operator" && arg.ChildCount() >= 3 {
				op := arg.Child(1).Utf8Text(src)
				if op == "=" || op == "<-" {
					if subtreeContainsIdentifier(arg.Child(0), src, varName) {
						return true
					}
				}
			}
		}
	}
	return false
}

func callHasDoBlock(node *tree_sitter.Node) bool {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if node.Child(i).Kind() == "do_block" {
			return true
		}
	}
	return false
}

// nodeContainsPosition returns true if the node's range includes the given position.
// Tree-sitter end positions are exclusive, consistent with nodeAtPosition.
func nodeContainsPosition(node *tree_sitter.Node, line, col uint) bool {
	start := node.StartPosition()
	end := node.EndPosition()
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

// stabBindsVariable returns true if the stab_clause's arguments (pattern)
// contain an identifier matching varName, meaning it creates a new binding.
func stabBindsVariable(stabClause *tree_sitter.Node, src []byte, varName string) bool {
	for i := uint(0); i < uint(stabClause.ChildCount()); i++ {
		child := stabClause.Child(i)
		if child.Kind() == "arguments" {
			return subtreeContainsIdentifier(child, src, varName)
		}
	}
	return false
}

// subtreeContainsIdentifier returns true if any identifier node in the subtree
// has the given name.
func subtreeContainsIdentifier(node *tree_sitter.Node, src []byte, name string) bool {
	if node == nil {
		return false
	}
	if node.Kind() == "identifier" && node.Utf8Text(src) == name {
		return true
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if subtreeContainsIdentifier(node.Child(i), src, name) {
			return true
		}
	}
	return false
}
