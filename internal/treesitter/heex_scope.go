package treesitter

// This file adds HEEX-aware variable scoping on top of the pure-Elixir scope
// model in variables.go. The Elixir model assumes a single continuous AST where
// a binding construct (for/with/case/fn) owns its body as a descendant do_block
// or stab_clause. HEEX breaks that assumption: `<%= for item <- @items do %>
// ...body... <% end %>` parses as FLAT SIBLING nodes — an opening `directive`
// whose expression is a `for` call with no do_block, the body as sibling nodes,
// and a separate `<% end %>` `directive`. Each expression fragment is parsed
// into its own Elixir sub-tree.
//
// To scope correctly, we model each HEEX binding as a byte range spanning the
// relevant HEEX nodes, assign each occurrence to its smallest active range, and
// filter results to the cursor's range. All bytes are in the top-most root's
// coordinate space, matching TreeNode.StartByte()/EndByte() and the occurrence
// positions produced by variables.go.

// bindingSite is the root-coordinate byte span of one binding identifier — e.g.
// the `item` in `for item <- @items do` or the `x` in `:let={x}`. These sites
// live outside the body range but belong to the same logical scope, so a rename
// must include them.
type bindingSite struct{ start, end uint }

// heexBindingRange models one variable binding that is active over a contiguous
// byte range in the HEEX template.
//
//   - for/with block: [start, end) = after the opening `<%= for ... do %>`
//     directive up to the START of the matching `<% end %>` directive (so any
//     code trailing `end` in that directive stays outside the body).
//   - case/cond clause arm: [start, end) = after the `<% pat -> %>` directive
//     up to the start of the next clause directive (or the closing `<% end %>`).
//   - :for/:let special attribute: [start, end) = the enclosing element's span.
type heexBindingRange struct {
	varName   string
	start     uint // first byte the binding is active
	end       uint // exclusive end of the active body
	bindNodes []bindingSite
}

// heexBlockKeywords are the Elixir control-flow keywords that open a `do` block
// when they appear at the head of a HEEX opening directive. if/unless/case/cond
// bind nothing themselves but must still be tracked so `<% end %>` matching stays
// balanced across nesting.
var heexBlockKeywords = map[string]bool{
	"for": true, "with": true, "case": true, "cond": true,
	"if": true, "unless": true,
}

// heexContainer walks the Tree.Root chain from node and returns the innermost
// enclosing HEEX (*Tree), or nil if node is in a pure-Elixir context. Returning
// the tree (rather than a bool) lets occurrence/scope paths collect ranges from
// it; the module-attribute check in isModuleAttributeIdent uses the != nil form.
func heexContainer(node *TreeNode) *Tree {
	for t := node.Tree; t != nil; {
		if t.Language == LangHeex {
			return t
		}
		if t.Root == nil {
			return nil
		}
		t = t.Root.Tree
	}
	return nil
}

// heexRangesInScope returns every HEEX binding range within the byte span of
// the resolved Elixir scope. Every occurrence is collected from this scope and
// the cursor lies inside it, so a range outside the scope span can neither
// contain an in-scope occurrence nor the cursor — it would never affect the
// filter, so computing it is wasted work. Templates in sibling defs are thus
// skipped, keeping the cost proportional to the scope rather than the whole
// file. The scope is still broader than the cursor's own HEEX container so that
// a loop/clause binding shadowing an outer variable is pruned when the cursor is
// in the outer Elixir scope. Returns nil for pure-Elixir documents (no
// sub-trees anywhere), keeping that path free.
func (t *Tree) heexRangesInScope(scope *TreeNode, src []byte) []heexBindingRange {
	if len(t.Branches) == 0 {
		return nil
	}
	var out []heexBindingRange
	t.collectHeexRangesInScope(scope.StartByte(), scope.EndByte(), src, &out)
	return out
}

func (t *Tree) collectHeexRangesInScope(scopeStart, scopeEnd uint, src []byte, out *[]heexBindingRange) {
	for _, branch := range t.Branches {
		trunk := branch.TrunkNode()
		// Skip a branch whose span is disjoint from the scope: its ranges can
		// touch neither an in-scope occurrence nor the cursor.
		if trunk.EndByte() <= scopeStart || trunk.StartByte() >= scopeEnd {
			continue
		}
		if branch.Language == LangHeex {
			*out = append(*out, collectAllHeexBindingRanges(trunk, src)...)
		}
		branch.collectHeexRangesInScope(scopeStart, scopeEnd, src, out)
	}
}

// collectAllHeexBindingRanges walks the entire HEEX tree (never crossing into
// Elixir sub-trees) and returns every binding range. The depth-stack matcher
// runs over each node's direct children: a `<%= for ... do %>` opener and its
// matching `<% end %>` are always siblings within one container's child list,
// because HEEX requires balanced markup within each branch.
func collectAllHeexBindingRanges(heexRoot *TreeNode, src []byte) []heexBindingRange {
	var out []heexBindingRange
	collectHeexBindingRanges(heexRoot, src, &out)
	return out
}

func collectHeexBindingRanges(node *TreeNode, src []byte, out *[]heexBindingRange) {
	if node.Tree.Language != LangHeex {
		return // do not descend into Elixir sub-trees
	}

	// Block/clause ranges from this node's direct children (flat sibling list).
	collectSiblingBindingRanges(node, src, out)

	// :for/:let element-scoped bindings.
	if k := node.Kind(); k == "tag" || k == "component" {
		if startTag := heexNamedContainer(node); startTag != nil {
			collectSpecialAttrBindings(node, startTag, src, out)
		}
	}

	for i := uint(0); i < node.RawChildCount(); i++ {
		collectHeexBindingRanges(node.RawChild(i), src, out)
	}
}

// openHeexScope tracks one pending (unmatched) block on the depth stack.
type openHeexScope struct {
	isCaseLike   bool          // case/cond: bindings come from clause arms, not the opener
	openEnd      uint          // root-coord end byte of the opening directive
	blockVars    []string      // for/with: names bound in the opener
	blockSites   []bindingSite // for/with: binding sites in the opener
	clauseStart  uint          // case-like: end byte of the last `<% pat -> %>` directive
	clauseVars   []string      // case-like: names bound by the last clause arm
	clauseSites  []bindingSite // case-like: binding sites in the last clause arm
	clauseActive bool          // case-like: whether a clause arm is currently open
}

// collectSiblingBindingRanges runs the depth-stack matcher over the direct
// children of container, emitting block ranges (for/with) and clause-arm ranges
// (case/cond) as opening directives are matched with their `<% end %>`.
func collectSiblingBindingRanges(container *TreeNode, src []byte, out *[]heexBindingRange) {
	var stack []openHeexScope

	for i := uint(0); i < container.RawChildCount(); i++ {
		child := container.RawChild(i)
		if child.Kind() != "directive" {
			continue
		}

		// Closing directive: `<% end %>`. End the body at the START of this
		// directive: the `<% end %>` marker holds no occurrences, and — because a
		// closing directive may legally carry trailing code after `end` (e.g.
		// `<% end\n  IO.puts("x") %>`) — that trailing code is a sibling AFTER the
		// block, outside its scope, so it must not fall inside the range.
		if ending := heexChildByKind(child, "ending_expression_value"); ending != nil {
			elixir := container.Tree.Branches[ending.Node.Id()]
			if elixir != nil && isEndExpr(elixir.TrunkNode(), src) && len(stack) > 0 {
				top := stack[len(stack)-1]
				closeHeexScope(top, child.StartByte(), src, out)
				stack = stack[:len(stack)-1]
			}
			continue
		}

		partial := heexChildByKind(child, "partial_expression_value")
		if partial == nil {
			continue
		}
		elixir := container.Tree.Branches[partial.Node.Id()]
		if elixir == nil {
			continue
		}
		root := elixir.TrunkNode()

		// Block opener: `<%= for/with/case/cond/if/unless ... do %>`.
		if call := blockOpenerCall(root, src); call != nil {
			keyword := call.Child(0).Utf8Text(src)
			scope := openHeexScope{
				isCaseLike: keyword == "case" || keyword == "cond",
				openEnd:    child.EndByte(),
			}
			if keyword == "for" || keyword == "with" {
				scope.blockVars, scope.blockSites = arrowLhsBindings(call, src)
			}
			scope.clauseStart = child.EndByte()
			stack = append(stack, scope)
			continue
		}

		// Clause arm inside a case/cond block: `<% pat -> %>`.
		if len(stack) > 0 && stack[len(stack)-1].isCaseLike {
			top := &stack[len(stack)-1]
			// Close the previous arm at the start of this directive.
			if top.clauseActive {
				emitClauseRanges(*top, child.StartByte(), src, out)
			}
			pattern := clausePattern(root)
			top.clauseVars, top.clauseSites = patternBindings(pattern, src)
			top.clauseStart = child.EndByte()
			top.clauseActive = true
		}
	}
	// Unmatched openers (malformed templates) are discarded — safe outer-scope
	// fallback: no range means no filtering.
}

// closeHeexScope emits the ranges for a block being closed at endByte.
func closeHeexScope(scope openHeexScope, endByte uint, src []byte, out *[]heexBindingRange) {
	if scope.isCaseLike {
		if scope.clauseActive {
			emitClauseRanges(scope, endByte, src, out)
		}
		return
	}
	for _, name := range uniqueNames(scope.blockVars) {
		*out = append(*out, heexBindingRange{
			varName:   name,
			start:     scope.openEnd,
			end:       endByte,
			bindNodes: perVarSites(scope.blockSites, src, name),
		})
	}
}

// emitClauseRanges emits the ranges for the currently-open clause arm, ending at
// endByte (the start of the next arm or the closing directive).
func emitClauseRanges(scope openHeexScope, endByte uint, src []byte, out *[]heexBindingRange) {
	for _, name := range uniqueNames(scope.clauseVars) {
		*out = append(*out, heexBindingRange{
			varName:   name,
			start:     scope.clauseStart,
			end:       endByte,
			bindNodes: perVarSites(scope.clauseSites, src, name),
		})
	}
}

// uniqueNames de-duplicates names while preserving first-seen order, so a
// variable bound more than once in a pattern yields a single range.
func uniqueNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	var out []string
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// blockOpenerCall returns the Elixir `call` node at the head of an opening
// directive's sub-tree when it is a control-flow block opener (for/with/case/
// cond/if/unless), or nil otherwise. Detection is structural: the source's first
// named child is a `call` whose target identifier is a block keyword. The
// trailing (ERROR) node holding the dangling `do` is never inspected.
func blockOpenerCall(root *TreeNode, src []byte) *TreeNode {
	call := root.NamedChild(0)
	if call == nil || call.Kind() != "call" || call.ChildCount() == 0 {
		return nil
	}
	first := call.Child(0)
	if first.Kind() != "identifier" || !heexBlockKeywords[first.Utf8Text(src)] {
		return nil
	}
	return call
}

// clausePattern returns the pattern node of a `<% pat -> %>` clause arm — the
// first named child of the sub-tree (the `->` is a trailing ERROR node). Returns
// nil when there is no pattern (e.g. a bare separator).
func clausePattern(root *TreeNode) *TreeNode {
	child := root.NamedChild(0)
	if child != nil && child.Kind() == "ERROR" {
		return nil
	}
	return child
}

// isEndExpr reports whether the Elixir sub-tree of a directive is a bare `end`.
// `<% end %>` parses as a source containing a single identifier "end".
//
// Though it's a bit niche, an EEX expression may have additional code after its
// `end` keyword, e.g.
//
//	<%= if true do %>
//	  Hello, world!
//	<% end
//	  IO.puts("Hello again.") %>
//
// This function only inspects the first child, so it will still return `true`.
func isEndExpr(root *TreeNode, src []byte) bool {
	child := root.NamedChild(0)
	return child != nil && child.Kind() == "identifier" && child.Utf8Text(src) == "end"
}

// heexChildByKind returns the first direct raw HEEX child of parent with the
// given kind (no branch crossing), or nil.
func heexChildByKind(parent *TreeNode, kind string) *TreeNode {
	for i := uint(0); i < parent.RawChildCount(); i++ {
		if child := parent.RawChild(i); child.Kind() == kind {
			return child
		}
	}
	return nil
}

// heexNamedContainer returns the start_tag/start_component/self_closing_component
// child of a tag/component node, which holds any special_attribute nodes.
func heexNamedContainer(node *TreeNode) *TreeNode {
	for i := uint(0); i < node.RawChildCount(); i++ {
		child := node.RawChild(i)
		switch child.Kind() {
		case "start_tag", "start_component", "self_closing_component", "self_closing_tag":
			return child
		}
	}
	return nil
}

// collectSpecialAttrBindings emits ranges for :for/:let special_attribute nodes
// on startTag, scoped to the enclosing element's byte span.
func collectSpecialAttrBindings(element, startTag *TreeNode, src []byte, out *[]heexBindingRange) {
	elemStart, elemEnd := element.StartByte(), element.EndByte()

	for i := uint(0); i < startTag.RawChildCount(); i++ {
		attr := startTag.RawChild(i)
		if attr.Kind() != "special_attribute" {
			continue
		}
		nameNode := heexChildByKind(attr, "special_attribute_name")
		exprValue := specialAttrExprValue(attr)
		if nameNode == nil || exprValue == nil {
			continue
		}
		elixir := startTag.Tree.Branches[exprValue.Node.Id()]
		if elixir == nil {
			continue
		}
		root := elixir.TrunkNode()

		var names []string
		var sites []bindingSite
		switch nameNode.Utf8Text(src) {
		case ":for":
			// `:for={item <- @items}` — bind the lhs of the `<-` generator.
			names, sites = arrowLhsFromExpr(root, src)
		case ":let":
			// `:let={x}` (or a destructuring pattern) — bind the whole pattern.
			names, sites = patternBindings(root, src)
		default:
			continue
		}
		for _, name := range names {
			*out = append(*out, heexBindingRange{
				varName:   name,
				start:     elemStart,
				end:       elemEnd,
				bindNodes: sites,
			})
		}
	}
}

// specialAttrExprValue returns the expression_value node inside a special
// attribute's `{...}` expression.
func specialAttrExprValue(attr *TreeNode) *TreeNode {
	expr := heexChildByKind(attr, "expression")
	if expr == nil {
		return nil
	}
	return heexChildByKind(expr, "expression_value")
}

// arrowLhsFromExpr extracts binding names/sites from the lhs of a `<-` operator
// anywhere in an expression sub-tree (used for `:for={x <- xs}`).
func arrowLhsFromExpr(root *TreeNode, src []byte) ([]string, []bindingSite) {
	var names []string
	var sites []bindingSite
	var walk func(n *TreeNode)
	walk = func(n *TreeNode) {
		if n == nil {
			return
		}
		if n.Kind() == "binary_operator" && n.ChildCount() >= 3 && n.Child(1).Utf8Text(src) == "<-" {
			ns, ss := patternBindings(n.Child(0), src)
			names = append(names, ns...)
			sites = append(sites, ss...)
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return names, sites
}

// arrowLhsBindings collects binding names/sites from the lhs of every `<-`
// operator in a for/with call's arguments (handles multiple generators).
func arrowLhsBindings(call *TreeNode, src []byte) ([]string, []bindingSite) {
	var names []string
	var sites []bindingSite
	for i := uint(0); i < call.ChildCount(); i++ {
		args := call.Child(i)
		if args.Kind() != "arguments" {
			continue
		}
		for j := uint(0); j < args.ChildCount(); j++ {
			arg := args.Child(j)
			if arg.Kind() == "binary_operator" && arg.ChildCount() >= 3 && arg.Child(1).Utf8Text(src) == "<-" {
				ns, ss := patternBindings(arg.Child(0), src)
				names = append(names, ns...)
				sites = append(sites, ss...)
			}
		}
	}
	return names, sites
}

// patternBindings collects unpinned, non-wildcard, non-call identifier names and
// their byte sites from a pattern subtree.
func patternBindings(pattern *TreeNode, src []byte) ([]string, []bindingSite) {
	var names []string
	var sites []bindingSite
	var walk func(n *TreeNode)
	walk = func(n *TreeNode) {
		if n == nil || isPinOperator(n, src) {
			return
		}
		if n.Kind() == "identifier" {
			name := n.Utf8Text(src)
			if name != "_" && !isDefinitionKeyword(name) && !isFunctionNameInCall(n, src) {
				names = append(names, name)
				sites = append(sites, bindingSite{start: n.StartByte(), end: n.EndByte()})
			}
			return
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(pattern)
	return names, sites
}

// lineStartOffsets returns the byte offset of the start of each line in src.
func lineStartOffsets(src []byte) []uint {
	offsets := []uint{0}
	for i, b := range src {
		if b == '\n' {
			offsets = append(offsets, uint(i)+1)
		}
	}
	return offsets
}

// occByteOffset converts an occurrence's (line, startCol) to a root-coord byte
// offset. tree-sitter columns are byte offsets within the line, matching the
// offsets produced by lineStartOffsets.
func occByteOffset(occ VariableOccurrence, lineStarts []uint) uint {
	if int(occ.Line) >= len(lineStarts) {
		return ^uint(0)
	}
	return lineStarts[occ.Line] + occ.StartCol
}

// filterOccurrencesByHeexRange restricts occurrences to the HEEX scope active at
// cursorByte for varName. ranges is the full set for the template; only ranges
// matching varName participate. Occurrences unaffected by any HEEX range are
// returned unchanged.
func filterOccurrencesByHeexRange(occurrences []VariableOccurrence, src []byte, varName string, cursorByte uint, ranges []heexBindingRange) []VariableOccurrence {
	varRanges := rangesForVar(ranges, varName)
	if len(varRanges) == 0 {
		return occurrences
	}

	lineStarts := lineStartOffsets(src)
	active := findActiveRange(cursorByte, varRanges)

	var out []VariableOccurrence
	if active != nil {
		// Cursor inside a HEEX binding scope: keep only occurrences in the active
		// range's body or on one of its binding sites.
		for _, occ := range occurrences {
			ob := occByteOffset(occ, lineStarts)
			if inRangeBodyOrSites(ob, *active) {
				out = append(out, occ)
			}
		}
		return out
	}

	// Cursor in the outer scope: drop occurrences inside any HEEX binding range.
	for _, occ := range occurrences {
		ob := occByteOffset(occ, lineStarts)
		if !inAnyRange(ob, varRanges) {
			out = append(out, occ)
		}
	}
	return out
}

// heexCollisionReachable reports whether a newName identifier at candByte would
// actually collide with the cursor variable (at cursorByte) under HEEX scoping.
// A HEEX binding range confines its variable to its body, so collision reach
// must equal rename reach:
//
//   - Cursor inside an active range for its own variable: the rename only touches
//     that range, so a candidate collides only if it lies in that same range's
//     body or binding sites.
//   - Cursor in the outer scope: the rename touches the outer scope, so a
//     candidate confined to any newName binding range is invisible and cannot
//     collide; a candidate outside all such ranges can.
func heexCollisionReachable(cursorByte, candByte uint, cursorVar, newName string, ranges []heexBindingRange) bool {
	cursorRanges := rangesForVar(ranges, cursorVar)
	if active := findActiveRange(cursorByte, cursorRanges); active != nil {
		return inRangeBodyOrSites(candByte, *active)
	}
	return !inAnyRange(candByte, rangesForVar(ranges, newName))
}

// rangesForVar returns the subset of ranges bound to varName.
func rangesForVar(ranges []heexBindingRange, varName string) []heexBindingRange {
	var out []heexBindingRange
	for _, r := range ranges {
		if r.varName == varName {
			out = append(out, r)
		}
	}
	return out
}

// findActiveRange returns the smallest range (by body width) whose body or
// binding sites contain cursorByte, or nil if none.
func findActiveRange(cursorByte uint, ranges []heexBindingRange) *heexBindingRange {
	var best *heexBindingRange
	for i := range ranges {
		r := &ranges[i]
		if !inRangeBodyOrSites(cursorByte, *r) {
			continue
		}
		if best == nil || (r.end-r.start) < (best.end-best.start) {
			best = r
		}
	}
	return best
}

func inRangeBodyOrSites(offset uint, r heexBindingRange) bool {
	if offset >= r.start && offset < r.end {
		return true
	}
	for _, s := range r.bindNodes {
		if offset >= s.start && offset < s.end {
			return true
		}
	}
	return false
}

func inAnyRange(offset uint, ranges []heexBindingRange) bool {
	for _, r := range ranges {
		if inRangeBodyOrSites(offset, r) {
			return true
		}
	}
	return false
}

// perVarSites returns only the binding sites whose text at [start,end) equals
// name, so a range emitted for one variable does not claim a sibling variable's
// binding site (e.g. `for x <- xs, y <- ys` emits separate ranges for x and y).
func perVarSites(sites []bindingSite, src []byte, name string) []bindingSite {
	var out []bindingSite
	for _, s := range sites {
		if int(s.end) <= len(src) && string(src[s.start:s.end]) == name {
			out = append(out, s)
		}
	}
	return out
}

// addHeexScopeVariables augments the visible-variable set with names bound by
// HEEX binding ranges active at cursorByte. Used by FindVariablesInScope so
// autocomplete surfaces in-scope loop/:let vars whose binding site sits after
// the cursor (which the before-cursor base collector misses).
func addHeexScopeVariables(cursorByte uint, seen map[string]bool, variables *[]string, ranges []heexBindingRange) {
	for _, r := range ranges {
		if cursorByte >= r.start && cursorByte < r.end && !seen[r.varName] {
			seen[r.varName] = true
			*variables = append(*variables, r.varName)
		}
	}
}

// filterHeexScopeVariables drops names that are confined to a HEEX binding range
// not active at cursorByte. A name is kept when it has no HEEX range (pure
// Elixir), when a range for it is active at the cursor, or when it also appears
// outside all its HEEX ranges (an outer-scope binding shadowed by the HEEX one).
func filterHeexScopeVariables(names []string, scope *TreeNode, src []byte, cursorByte uint, ranges []heexBindingRange) []string {
	var out []string
	for _, name := range names {
		varRanges := rangesForVar(ranges, name)
		if len(varRanges) == 0 || findActiveRange(cursorByte, varRanges) != nil {
			out = append(out, name)
			continue
		}
		// Confined to inactive range(s): keep only if bound in the outer scope too.
		if identifierOutsideRanges(scope, src, name, varRanges) {
			out = append(out, name)
		}
	}
	return out
}

// identifierOutsideRanges reports whether an identifier named name appears in
// scope at a byte offset outside every range in varRanges.
func identifierOutsideRanges(scope *TreeNode, src []byte, name string, varRanges []heexBindingRange) bool {
	for _, node := range findAllNonCallIdentifiers(scope, src, name) {
		if !inAnyRange(node.StartByte(), varRanges) {
			return true
		}
	}
	return false
}
