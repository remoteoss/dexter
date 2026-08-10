package treesitter

import (
	"strings"
)

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
	tree := NewTree(src)
	if tree == nil {
		return nil
	}
	defer tree.Close()
	return tree.FindVariableOccurrences(src, line, col)
}

// FindVariableOccurrencesWithTree is like FindVariableOccurrences but uses a
// pre-parsed tree root, avoiding redundant parsing when a cached tree exists.
func (t *Tree) FindVariableOccurrences(src []byte, line, col uint) []VariableOccurrence {
	resolved := t.resolveVariableScope(src, line, col)
	if resolved == nil {
		return nil
	}

	if resolved.moduleAttribute {
		var occurrences []VariableOccurrence
		collectModuleAttributeOccurrences(resolved.scope, src, resolved.varName, &occurrences)
		return occurrences
	}

	var occurrences []VariableOccurrence

	// When the scope is a stab_clause because the body rebinds the variable
	// (not the args), only collect from the body — the args hold the outer
	// variable's pin reference, which belongs to a different scope.
	if resolved.scope.Kind() == "stab_clause" && stabBodyRebindsVariable(resolved.scope, src, resolved.varName) && !stabBindsVariable(resolved.scope, src, resolved.varName) {
		for i := uint(0); i < uint(resolved.scope.ChildCount()); i++ {
			child := resolved.scope.Child(i)
			if child.Kind() != "arguments" {
				collectVariableOccurrences(child, src, resolved.varName, &occurrences, false)
			}
		}
		return occurrences
	}

	// with/for call scope: use cursor-aware collection to correctly handle
	// multi-clause patterns where different clauses bind/reference the variable.
	if resolved.scope.Kind() == "call" && callHasDoBlock(resolved.scope) && callArgumentPatternsBindVariable(resolved.scope, src, resolved.varName) {
		collectWithOccurrences(resolved.scope, resolved.cursorNode, src, resolved.varName, &occurrences)
		return occurrences
	}

	// A def-family call (scope.Kind() == "call" here, since with/for calls are
	// handled above) is the scope root, so skip the def-boundary check on it —
	// otherwise collection would bail immediately on the very scope it chose.
	skipRoot := resolved.scope.Kind() == "stab_clause" || resolved.scope.Kind() == "call"
	collectVariableOccurrences(resolved.scope, src, resolved.varName, &occurrences, skipRoot)
	return occurrences
}

// NameExistsInScopeOf checks whether newName already exists as a variable or
// module attribute in the same scope as the variable at (line, col). Uses the
// same scope-finding rules as FindVariableOccurrencesWithTree so collision
// detection matches the rename's reach.
//
// Bare identifiers that are zero-arity function calls (not bound as variables)
// are NOT considered collisions — in Elixir, a variable simply shadows them.
func (t *Tree) NameExistsInScopeOf(src []byte, line, col uint, newName string) bool {
	resolved := t.resolveVariableScope(src, line, col)
	if resolved == nil {
		return false
	}

	if resolved.moduleAttribute {
		return moduleAttributeExists(resolved.scope, src, newName)
	}

	// Find the first non-call identifier matching newName in the scope.
	target := findFirstNonCallIdentifier(resolved.scope, src, newName)
	if target == nil {
		return false
	}

	// Check if that identifier is actually a variable (bound in its scope)
	// rather than a bare zero-arity function call. Reuses the full variable
	// resolution logic so the same scoping rules apply.
	pos := target.StartPosition()
	return len(t.FindVariableOccurrences(src, uint(pos.Row), uint(pos.Column))) > 0
}

// findFirstNonCallIdentifier returns the first identifier node in the subtree
// matching name that is not a function name in a call expression. Nested
// function definitions (def/defp/etc.) are independent scopes, so they are not
// descended into — a same-named binding inside one is not a collision in the
// scope rooted at node. (The root itself may be such a def call when renaming a
// function-local; that is the chosen scope and is always searched.)
func findFirstNonCallIdentifier(node *TreeNode, src []byte, name string) *TreeNode {
	return findFirstNonCallIdentifierInScope(node, src, name, true)
}

func findFirstNonCallIdentifierInScope(node *TreeNode, src []byte, name string, isRoot bool) *TreeNode {
	if node == nil {
		return nil
	}
	if !isRoot && definesNestedScope(node, src) {
		return nil
	}
	if node.Kind() == "identifier" && node.Utf8Text(src) == name && !isFunctionNameInCall(node, src) {
		return node
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if found := findFirstNonCallIdentifierInScope(node.Child(i), src, name, false); found != nil {
			return found
		}
	}
	return nil
}

// resolvedScope holds the result of locating a variable's scope.
type resolvedScope struct {
	cursorNode      *TreeNode
	scope           *TreeNode
	varName         string
	moduleAttribute bool // true when the identifier is a module attribute (@foo)
}

// resolveVariableScope locates the cursor node at (line, col), validates it as
// a variable or module attribute, and returns the enclosing scope. Returns nil
// if the position is not on a renameable variable.
func (t *Tree) resolveVariableScope(src []byte, line, col uint) *resolvedScope {
	cursorNode := t.TrunkNode().ChildAtPosition(line, col)

	if cursorNode == nil || cursorNode.Kind() != "identifier" {
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
		return &resolvedScope{cursorNode: cursorNode, scope: scope, varName: varName, moduleAttribute: true}
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

	// A bare identifier could be a variable or a zero-arity function call.
	// Only treat it as a variable if the name is actually defined (bound)
	// earlier in the scope — e.g. as a function parameter or via assignment.
	// This ensures bare function calls fall through to function reference lookup.
	// Exception: if the cursor is on an assignment target (LHS of =), it is
	// unambiguously a variable binding regardless of other occurrences.
	if !isAssignmentTarget(cursorNode, src) && !variableDefinedInScope(scope, src, varName, line, col) {
		return nil
	}

	return &resolvedScope{cursorNode: cursorNode, scope: scope, varName: varName}
}

// moduleAttributeExists returns true if @name appears in the subtree.
func moduleAttributeExists(node *TreeNode, src []byte, name string) bool {
	if node == nil {
		return false
	}
	if node.Kind() == "identifier" && node.Utf8Text(src) == name && isModuleAttributeIdent(node, src) {
		return true
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if moduleAttributeExists(node.Child(i), src, name) {
			return true
		}
	}
	return false
}

// isFunctionNameInCall returns true if the identifier is the function name
// in a call expression (e.g., `foo` in `foo(args)`) or a function name being
// defined (e.g., `foo` in `def foo(args) do`).
func isFunctionNameInCall(node *TreeNode, src []byte) bool {
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

// moduleKeywords are the keywords that open a module body — an independent
// variable scope. Variables bound directly in a module body belong only to
// that module, not to sibling modules or the surrounding script.
var moduleKeywords = map[string]bool{
	"defmodule": true, "defprotocol": true, "defimpl": true,
}

// isFunctionDefinitionCall reports whether node is a def/defp/defmacro/etc.
// call — the boundary of an independent variable scope. Variables inside a
// function definition do not leak to (and cannot reference) an enclosing
// module/script scope, so traversals rooted at an outer scope must not descend
// into these.
func isFunctionDefinitionCall(node *TreeNode, src []byte) bool {
	if node.Kind() != "call" || node.ChildCount() == 0 {
		return false
	}
	first := node.Child(0)
	return first.Kind() == "identifier" && functionKeywords[first.Utf8Text(src)]
}

// isModuleDefinitionCall reports whether node is a defmodule/defprotocol/defimpl
// call, which opens a module-body scope.
func isModuleDefinitionCall(node *TreeNode, src []byte) bool {
	if node.Kind() != "call" || node.ChildCount() == 0 {
		return false
	}
	first := node.Child(0)
	return first.Kind() == "identifier" && moduleKeywords[first.Utf8Text(src)]
}

// definesNestedScope reports whether node is a call that introduces its own
// variable scope — a function or module definition. A traversal rooted at an
// outer scope (a module body, or the whole file) must not descend into these,
// or a rename/collision check would wrongly reach into an unrelated scope.
func definesNestedScope(node *TreeNode, src []byte) bool {
	return isFunctionDefinitionCall(node, src) || isModuleDefinitionCall(node, src)
}

// isAssignmentTarget returns true if node is on the left-hand side of a `=`
// binary operator, meaning it is unambiguously a variable binding.
func isAssignmentTarget(node *TreeNode, src []byte) bool {
	parent := node.Parent()
	if parent == nil || parent.Kind() != "binary_operator" || parent.ChildCount() < 3 {
		return false
	}
	if parent.Child(1).Utf8Text(src) != "=" {
		return false
	}
	left := parent.Child(0)
	return node.StartByte() >= left.StartByte() && node.EndByte() <= left.EndByte()
}

// variableDefinedInScope returns true if varName is bound (defined) in the
// scope — either as a function parameter or via assignment/pattern match —
// at a position other than the cursor. A bare identifier that only appears
// at the cursor position is ambiguous (could be a zero-arity function call)
// and should not be treated as a variable.
func variableDefinedInScope(scope *TreeNode, src []byte, varName string, cursorLine, cursorCol uint) bool {
	return identifierExistsElsewhere(scope, src, varName, cursorLine, cursorCol, true)
}

// identifierExistsElsewhere returns true if an identifier matching name
// exists anywhere in the subtree at a position different from (line, col).
// It skips function names in calls and definition keywords. Nested function
// definitions are independent scopes and are not descended into (isRoot guards
// the chosen scope itself, which may be such a def call) — otherwise a bare
// top-level call sharing a name with a function-local would be misread as a
// variable.
func identifierExistsElsewhere(node *TreeNode, src []byte, name string, line, col uint, isRoot bool) bool {
	if node == nil {
		return false
	}
	if !isRoot && definesNestedScope(node, src) {
		return false
	}
	if node.Kind() == "identifier" && node.Utf8Text(src) == name && !isFunctionNameInCall(node, src) {
		pos := node.StartPosition()
		if uint(pos.Row) != line || uint(pos.Column) != col {
			return true
		}
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if identifierExistsElsewhere(node.Child(i), src, name, line, col, false) {
			return true
		}
	}
	return false
}

// findEnclosingScope walks up from node to find the nearest scope boundary
// for varName. A stab_clause (fn/case arm) that binds varName in its
// arguments is a scope boundary. A stab_clause whose body rebinds varName
// is also a scope boundary (the cursor is on an inner binding). A call with
// do_block (with/for/etc.) whose argument patterns rebind varName is a scope
// boundary ONLY when the cursor is inside the do_block — not when it's on the
// right side of a <- clause, which is evaluated in the outer scope.
// Otherwise, the enclosing def/defp/defmacro/test call is the scope.
func findEnclosingScope(node *TreeNode, src []byte, varName string) *TreeNode {
	prev := node
	current := node.Parent()
	for current != nil {
		if current.Kind() == "stab_clause" {
			if stabBindsVariable(current, src, varName) {
				return current
			}
			// Body rebinds the variable (e.g. `fn ^x -> x = nil end`): the
			// stab_clause is the scope boundary for the inner binding.
			// Note: if the cursor is on a closure reference BEFORE the rebind
			// in the same body, it will be scoped to the fn rather than the
			// outer function. This is an acceptable limitation for a rare pattern.
			if stabBodyRebindsVariable(current, src, varName) {
				return current
			}
		}
		if current.Kind() == "call" && current.ChildCount() > 0 {
			firstChild := current.Child(0)
			if firstChild.Kind() == "identifier" && functionKeywords[firstChild.Utf8Text(src)] {
				return current
			}
			// A module body (defmodule/defprotocol/defimpl) is its own scope:
			// module-level bindings belong to this module, not to sibling
			// modules or the surrounding script.
			if firstChild.Kind() == "identifier" && moduleKeywords[firstChild.Utf8Text(src)] {
				return current
			}
			// with/for/etc.: scope boundary unless cursor is on clause 0's rhs (outer scope).
			if callHasDoBlock(current) && callArgumentPatternsBindVariable(current, src, varName) {
				if cursorNeedsWithScope(current, prev, node, src, varName) {
					return current
				}
			}
		}
		// Reached the file root without an inner scope: top-level script
		// bindings (e.g. config/runtime.exs) are scoped to the whole file.
		if current.Kind() == "source" && current.Parent() == nil {
			return current
		}
		prev = current
		current = current.Parent()
	}
	return nil
}

// nodeIsInsideDoBlock returns true if child is inside the do_block of callNode.
func nodeIsInsideDoBlock(callNode, child *TreeNode) bool {
	for i := uint(0); i < uint(callNode.ChildCount()); i++ {
		block := callNode.Child(i)
		if block.Kind() == "do_block" &&
			block.StartByte() <= child.StartByte() &&
			child.EndByte() <= block.EndByte() {
			return true
		}
	}
	return false
}

// cursorNeedsWithScope returns true if the cursor is in a position where the
// given with/for call should act as a scope boundary: inside the do_block,
// on a lvalue of <-/=, or on the rhs of clause N>0 (which references clause
// N-1's binding, not the outer scope).
func cursorNeedsWithScope(callNode, prev, cursor *TreeNode, src []byte, varName string) bool {
	if nodeIsInsideDoBlock(callNode, prev) {
		return true
	}
	clauses := extractArrowClauses(callNode, src)
	bound := false
	for _, clause := range clauses {
		lhs := clause.Child(0)
		rhs := clause.Child(2)
		// Cursor on lhs = new binding → with is the scope
		if lhs.StartByte() <= cursor.StartByte() && cursor.EndByte() <= lhs.EndByte() {
			return true
		}
		// Cursor on rhs of a clause where a previous lhs bound varName → with is the scope
		if bound && rhs.StartByte() <= cursor.StartByte() && cursor.EndByte() <= rhs.EndByte() {
			return true
		}
		if subtreeContainsUnpinnedIdentifier(lhs, src, varName) {
			bound = true
		}
	}
	return false
}

// collectVariableOccurrences recursively collects identifier nodes matching
// varName within the given subtree, skipping function names in calls.
// skipScopeCheck should be true when node is the scope root itself (so we
// don't immediately bail out of the scope we chose).
func collectVariableOccurrences(node *TreeNode, src []byte, varName string, out *[]VariableOccurrence, skipScopeCheck bool) {
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
		// Skip nested stab_clauses that rebind this variable — either via an
		// unpinned param binding OR a body-level assignment. In both cases the
		// stab_clause introduces a new scope for this variable.
		// Exception: the args are still collected so pinned references (^var)
		// in the params are included in the rename.
		if node.Kind() == "stab_clause" {
			if stabBindsVariable(node, src, varName) {
				// Unpinned param binding — skip entire clause.
				return
			}
			if stabBodyRebindsVariable(node, src, varName) {
				// Body rebind (e.g. `fn ^x -> x = nil end`) — collect only
				// the args (for pin references), skip the body.
				collectStabArgs(node, src, varName, out)
				return
			}
		}

		// Call nodes with do_block (with/for/etc.) that rebind this variable in
		// their argument patterns: the do_block and pattern sides use a new
		// binding, but the expression sides (right of =/←) still reference
		// the outer variable and must be traversed.
		if node.Kind() == "call" && callHasDoBlock(node) && callArgumentPatternsBindVariable(node, src, varName) {
			collectPatternExpressionOccurrences(node, src, varName, out)
			return
		}

		// Function and module definitions introduce their own variable scope.
		// When collecting from an outer scope — e.g. the whole file for a
		// top-level script binding, or a module body — do not descend into a
		// nested definition, or a rename would wrongly touch same-named bindings
		// that live in that separate scope.
		if definesNestedScope(node, src) {
			return
		}
	}

	for i := uint(0); i < uint(node.ChildCount()); i++ {
		collectVariableOccurrences(node.Child(i), src, varName, out, false)
	}
}

// stabBodyRebindsVariable returns true if the body of the stab_clause contains
// an assignment (=) whose left-hand side unpinnedly binds varName.
func stabBodyRebindsVariable(stabClause *TreeNode, src []byte, varName string) bool {
	for i := uint(0); i < uint(stabClause.ChildCount()); i++ {
		child := stabClause.Child(i)
		if child.Kind() == "arguments" {
			continue // args are handled separately by stabBindsVariable
		}
		if subtreeContainsAssignmentOf(child, src, varName) {
			return true
		}
	}
	return false
}

// subtreeContainsAssignmentOf returns true if the subtree has a binary "="
// whose lvalue unpinnedly binds varName.
func subtreeContainsAssignmentOf(node *TreeNode, src []byte, varName string) bool {
	if node == nil {
		return false
	}
	if node.Kind() == "binary_operator" && node.ChildCount() >= 3 {
		if node.Child(1).Utf8Text(src) == "=" {
			if subtreeContainsUnpinnedIdentifier(node.Child(0), src, varName) {
				return true
			}
		}
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if subtreeContainsAssignmentOf(node.Child(i), src, varName) {
			return true
		}
	}
	return false
}

// collectStabArgs collects variable occurrences from the args of a stab_clause
// only (not the body). Used when the body rebinds the variable.
func collectStabArgs(stabClause *TreeNode, src []byte, varName string, out *[]VariableOccurrence) {
	for i := uint(0); i < uint(stabClause.ChildCount()); i++ {
		child := stabClause.Child(i)
		if child.Kind() == "arguments" {
			collectVariableOccurrences(child, src, varName, out, false)
		}
	}
}

// isModuleAttributeIdent returns true if the identifier is the name part of a
// module attribute expression. Tree-sitter represents these as:
//
//	@foo       → unary_operator("@") → identifier("foo")
//	@foo value → unary_operator("@") → call → identifier("foo") …
//
// Within HEEX trees (regardless of nesting depth), the `@` symbol is instead a
// macro that expands `@x` to `assigns.x`. Within HEEX trees, this returns `false`.
func isModuleAttributeIdent(node *TreeNode, src []byte) bool {
	parent := node.Parent()
	if parent == nil {
		return false
	}

	// walk to the document root and make sure we aren't nested within any HEEX trees
	for t := node.Tree.Root; t != nil; t = t.Tree.Root {
		if t.Tree.Language == LangHeex {
			return false
		}
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
func isAtUnaryOp(node *TreeNode, src []byte) bool {
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
func findEnclosingModule(node *TreeNode, src []byte) *TreeNode {
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
func collectModuleAttributeOccurrences(node *TreeNode, src []byte, attrName string, out *[]VariableOccurrence) {
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
	tree := NewTree(src)
	if tree == nil {
		return nil
	}
	defer tree.Close()
	return tree.FindTokenOccurrences(src, token)
}

// FindTokenOccurrencesWithTree is like FindTokenOccurrences but uses a
// pre-parsed tree root.
func (t *Tree) FindTokenOccurrences(src []byte, token string) []VariableOccurrence {
	var occurrences []VariableOccurrence
	collectTokenOccurrences(t.TrunkNode(), src, token, &occurrences)
	return occurrences
}

func collectTokenOccurrences(node *TreeNode, src []byte, token string, out *[]VariableOccurrence) {
	if node == nil {
		return
	}

	kind := node.Kind()

	// Skip subtrees that can't contain meaningful identifier references
	if kind == "comment" || kind == "charlist" {
		return
	}

	if (kind == "identifier" /* Elixir */ || kind == "function" /* HEEX */) &&
		node.Utf8Text(src) == token {
		*out = append(*out, VariableOccurrence{
			Line:     uint(node.StartPosition().Row),
			StartCol: uint(node.StartPosition().Column),
			EndCol:   uint(node.EndPosition().Column),
		})
	}

	// Alias nodes may contain dotted names like "MyApp.Repo". Match if the
	// full text equals token, or if a dot-separated segment matches. When a
	// segment matches, report only that segment's column range.
	if kind == "alias" /* Elixir */ || kind == "module" /* HEEX */ {
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
	tree := NewTree(src)
	if tree == nil {
		return nil
	}
	defer tree.Close()
	return tree.FindVariablesInScope(src, line, col)
}

// FindVariablesInScopeWithTree is like FindVariablesInScope but uses a
// pre-parsed tree root.
func (t *Tree) FindVariablesInScope(src []byte, line, col uint) []string {
	cursorNode := t.TrunkNode().ChildAtPosition(line, col)
	if cursorNode == nil && col > 0 {
		cursorNode = t.TrunkNode().ChildAtPosition(line, col-1)
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
func findEnclosingFunction(node *TreeNode, src []byte) *TreeNode {
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
func collectVariableNames(node *TreeNode, src []byte, seen map[string]bool, out *[]string, cursorLine, cursorCol uint) {
	if node == nil {
		return
	}

	if !node.ContainsPosition(cursorLine, cursorCol) {
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

// extractArrowClauses returns the binary_operator nodes for <- and = in the
// call's arguments, in source order.
func extractArrowClauses(callNode *TreeNode, src []byte) []*TreeNode {
	var clauses []*TreeNode
	for i := uint(0); i < uint(callNode.ChildCount()); i++ {
		child := callNode.Child(i)
		if child.Kind() != "arguments" {
			continue
		}
		for j := uint(0); j < uint(child.ChildCount()); j++ {
			arg := child.Child(j)
			if arg.Kind() == "binary_operator" && arg.ChildCount() >= 3 {
				op := arg.Child(1).Utf8Text(src)
				if op == "<-" || op == "=" {
					clauses = append(clauses, arg)
				}
			}
		}
	}
	return clauses
}

// collectWithOccurrences handles variable collection for a with/for call scope.
// It determines the cursor's position (which clause, lhs vs rhs, or do_block)
// and collects exactly the right occurrences for the binding at the cursor.
//
// The rules for `with {:ok, x} <- rhs0, {:ok, x} <- rhs1 do body end`:
//   - Cursor on rhs0: outer scope — not handled here (findEnclosingScope won't stop here)
//   - Cursor on lhs0: collect lhs0 + rhs1 (until next rebind) + body (if no rebind)
//   - Cursor on rhs1: uses lhs0's binding — collect lhs0 + rhs1 (+ further rhs until rebind) + body
//   - Cursor on lhs1: collect lhs1 + body
//   - Cursor in body: uses last clause's binding — collect last lhs + body
func collectWithOccurrences(callNode, cursor *TreeNode, src []byte, varName string, out *[]VariableOccurrence) {
	clauses := extractArrowClauses(callNode, src)

	// Find which clause and side the cursor is on
	cursorIdx := -1
	cursorOnLhs := false

	for i, clause := range clauses {
		lhs := clause.Child(0)
		rhs := clause.Child(2)
		if lhs.StartByte() <= cursor.StartByte() && cursor.EndByte() <= lhs.EndByte() {
			cursorIdx = i
			cursorOnLhs = true
			break
		}
		if rhs.StartByte() <= cursor.StartByte() && cursor.EndByte() <= rhs.EndByte() {
			cursorIdx = i
			cursorOnLhs = false
			break
		}
	}

	// Find the do_block
	var doBlock *TreeNode
	for i := uint(0); i < uint(callNode.ChildCount()); i++ {
		child := callNode.Child(i)
		if child.Kind() == "do_block" {
			doBlock = child
			if doBlock.StartByte() <= cursor.StartByte() && cursor.EndByte() <= doBlock.EndByte() {
				cursorIdx = len(clauses) // cursor in do_block — treat as "after all clauses"
			}
			break
		}
	}

	// Cursor on operator/comma/whitespace between clauses — not on any lhs or rhs
	if cursorIdx < 0 {
		return
	}

	// Cursor in do_block (cursorIdx == len(clauses)): uses the last clause's binding
	if cursorIdx >= len(clauses) {
		lastBindingIdx := -1
		for i, clause := range clauses {
			if subtreeContainsUnpinnedIdentifier(clause.Child(0), src, varName) {
				lastBindingIdx = i
			}
		}
		if lastBindingIdx >= 0 {
			collectVariableOccurrences(clauses[lastBindingIdx].Child(0), src, varName, out, false)
			for i := lastBindingIdx + 1; i < len(clauses); i++ {
				collectVariableOccurrences(clauses[i].Child(2), src, varName, out, false)
				if subtreeContainsUnpinnedIdentifier(clauses[i].Child(0), src, varName) {
					return
				}
			}
		}
		if doBlock != nil {
			collectVariableOccurrences(doBlock, src, varName, out, false)
		}
		return
	}

	if cursorOnLhs {
		// Cursor on lhs of clause N: collect lhs N, then rhs of N+1..., until rebind
		collectVariableOccurrences(clauses[cursorIdx].Child(0), src, varName, out, false)
		for i := cursorIdx + 1; i < len(clauses); i++ {
			collectVariableOccurrences(clauses[i].Child(2), src, varName, out, false)
			if subtreeContainsUnpinnedIdentifier(clauses[i].Child(0), src, varName) {
				return
			}
		}
		if doBlock != nil {
			collectVariableOccurrences(doBlock, src, varName, out, false)
		}
		return
	}

	// Cursor on rhs of clause N>0: references lhs of clause N-1
	collectVariableOccurrences(clauses[cursorIdx-1].Child(0), src, varName, out, false) // lhs N-1
	collectVariableOccurrences(clauses[cursorIdx].Child(2), src, varName, out, false)   // rhs N
	for i := cursorIdx + 1; i < len(clauses); i++ {
		collectVariableOccurrences(clauses[i].Child(2), src, varName, out, false)
		if subtreeContainsUnpinnedIdentifier(clauses[i].Child(0), src, varName) {
			return
		}
	}
	if doBlock != nil {
		collectVariableOccurrences(doBlock, src, varName, out, false)
	}
}

// collectPatternExpressionOccurrences traverses the expression (right) sides
// of =/← binary operators in a call's arguments, processing clauses
// sequentially. Once a clause's pattern (left side) rebinds varName,
// subsequent clauses and the do_block use the new binding — so we stop.
func collectPatternExpressionOccurrences(callNode *TreeNode, src []byte, varName string, out *[]VariableOccurrence) {
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
					if subtreeContainsUnpinnedIdentifier(arg.Child(0), src, varName) {
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
// (left side of = or <- operators) contain an unpinned binding of varName.
func callArgumentPatternsBindVariable(node *TreeNode, src []byte, varName string) bool {
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
					if subtreeContainsUnpinnedIdentifier(arg.Child(0), src, varName) {
						return true
					}
				}
			}
		}
	}
	return false
}

func callHasDoBlock(node *TreeNode) bool {
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if node.Child(i).Kind() == "do_block" {
			return true
		}
	}
	return false
}

// stabBindsVariable returns true if the stab_clause's arguments (pattern)
// contain an unpinned identifier matching varName, meaning it creates a new
// binding. Pinned variables (^varName) reference the outer scope and do NOT
// create a new binding.
func stabBindsVariable(stabClause *TreeNode, src []byte, varName string) bool {
	for i := uint(0); i < uint(stabClause.ChildCount()); i++ {
		child := stabClause.Child(i)
		if child.Kind() == "arguments" {
			return subtreeContainsUnpinnedIdentifier(child, src, varName)
		}
	}
	return false
}

// subtreeContainsUnpinnedIdentifier returns true if any identifier node in the
// subtree has the given name AND is not pinned (^varName). Pinned variables
// reference an outer binding and do not create a new one.
func subtreeContainsUnpinnedIdentifier(node *TreeNode, src []byte, name string) bool {
	if node == nil {
		return false
	}
	// Skip pinned expressions: ^varName is a unary_operator with "^"
	if isPinOperator(node, src) {
		return false
	}
	if node.Kind() == "identifier" && node.Utf8Text(src) == name {
		return true
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		if subtreeContainsUnpinnedIdentifier(node.Child(i), src, name) {
			return true
		}
	}
	return false
}

// isPinOperator returns true if node is a unary_operator with the ^ operator.
func isPinOperator(node *TreeNode, src []byte) bool {
	if node.Kind() != "unary_operator" {
		return false
	}
	for i := uint(0); i < uint(node.ChildCount()); i++ {
		child := node.Child(i)
		if !child.IsNamed() && child.EndByte() > child.StartByte() && src[child.StartByte()] == '^' {
			return true
		}
	}
	return false
}
