package lsp

import (
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/remoteoss/dexter/internal/parser"
)

// TokenizedFile holds pre-tokenized source for efficient multi-operation queries.
// Use this when multiple tokenizer-based lookups will be performed on the same text.
type TokenizedFile struct {
	source     []byte
	tokens     []parser.Token
	n          int
	lineStarts []int
	// interp holds the code tokens inside #{} interpolations, kept out of the
	// main stream by the tokenizer. Cursor lookups fall back to it, so a call
	// written inside a string still resolves.
	interp []parser.Token
	// interpSpans are the byte ranges of the interpolations, for the arity of
	// a call inside one (parser.InterpCallArity). Nil when not known.
	interpSpans []parser.Span
}

// NewTokenizedFile tokenizes the text once for reuse across multiple queries.
func NewTokenizedFile(text string) *TokenizedFile {
	source := []byte(text)
	result := parser.TokenizeFull(source)
	return &TokenizedFile{
		source:      source,
		tokens:      result.Tokens,
		n:           len(result.Tokens),
		lineStarts:  result.LineStarts,
		interp:      result.Interp,
		interpSpans: result.InterpSpans,
	}
}

// NewTokenizedFileFromCache wraps pre-existing tokens (e.g. from DocumentStore cache).
func NewTokenizedFileFromCache(tokens []parser.Token, source []byte, lineStarts []int, interp []parser.Token) *TokenizedFile {
	return &TokenizedFile{
		source:     source,
		tokens:     tokens,
		n:          len(tokens),
		lineStarts: lineStarts,
		interp:     interp,
	}
}

// ExpressionAtCursor extracts the dotted expression at the given 0-based line
// and col, using the cached token stream.
func (tf *TokenizedFile) ExpressionAtCursor(line, col int) CursorContext {
	ctx := ExpressionAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
	if ctx.Empty() && len(tf.interp) > 0 {
		// The cursor sits inside a string literal, which is one token in the
		// main stream. The code inside #{} lives in the interpolation stream.
		ctx = ExpressionAtCursor(tf.interp, tf.source, tf.lineStarts, line, col)
	}
	return ctx
}

// FullExpressionAtCursor extracts the complete dotted expression at the given
// 0-based line and col without truncating at the cursor's segment.
func (tf *TokenizedFile) FullExpressionAtCursor(line, col int) CursorContext {
	ctx := FullExpressionAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
	if ctx.Empty() && len(tf.interp) > 0 {
		ctx = FullExpressionAtCursor(tf.interp, tf.source, tf.lineStarts, line, col)
	}
	return ctx
}

// FirstDefmodule returns the first defmodule name found, or "".
func (tf *TokenizedFile) FirstDefmodule() string {
	for i := 0; i < tf.n; i++ {
		if tf.tokens[i].Kind == parser.TokDefmodule {
			j := tokNextSig(tf.tokens, tf.n, i+1)
			name, _ := tokCollectModuleName(tf.source, tf.tokens, tf.n, j)
			if name != "" {
				return name
			}
		}
	}
	return ""
}

// ResolveModuleExpr replaces __MODULE__ in expr with the enclosing module name
// at the given 0-based line. If targetLine < 0, uses the first defmodule found.
func (tf *TokenizedFile) ResolveModuleExpr(expr string, targetLine int) string {
	if !strings.Contains(expr, "__MODULE__") {
		return expr
	}

	var moduleName string
	if targetLine >= 0 {
		moduleName = extractEnclosingModuleFromTokens(tf.source, tf.tokens, targetLine)
	}
	if moduleName == "" {
		moduleName = tf.FirstDefmodule()
	}

	if moduleName != "" {
		return strings.ReplaceAll(expr, "__MODULE__", moduleName)
	}
	return expr
}

// FindFunctionDefinition searches for a def/defp/defmacro/defmacrop or @type/@typep/@opaque
// matching the given function name. Returns the 1-based line number and true if found.
func (tf *TokenizedFile) FindFunctionDefinition(functionName string) (int, bool) {
	return tf.findDefinition(functionName, false)
}

// FindFunctionDefinitionArity is FindFunctionDefinition for the function of
// one arity: the first head whose arities (a range with default arguments)
// have it, else the first head of the name.
// Only heads in module, the module the call resolved to, count: a nested
// module's function of that name and arity is another function.
func (tf *TokenizedFile) FindFunctionDefinitionArity(functionName, module string, arity int) (int, bool) {
	for i := 0; i < tf.n; i++ {
		switch tf.tokens[i].Kind {
		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			name, nameIdx, ok := parser.StaticDeclarationName(tf.source, tf.tokens, tf.n, i)
			if !ok || name != functionName {
				continue
			}
			lo, hi, known := tokenArity(tf.source, tf.tokens, nameIdx)
			if !known || arity < lo || arity > hi {
				continue
			}
			if extractEnclosingModuleFromTokens(tf.source, tf.tokens, tf.tokens[i].Line-1) == module {
				return tf.tokens[i].Line, true
			}
		}
	}
	return tf.FindFunctionDefinition(functionName)
}

// FindTypeDefinition is FindFunctionDefinition with the opposite preference:
// use it when the cursor sits in a typespec, where a bare name refers to the
// type rather than to a function of the same name.
func (tf *TokenizedFile) FindTypeDefinition(functionName string) (int, bool) {
	return tf.findDefinition(functionName, true)
}

// findDefinition returns the line of the first matching definition. A module
// may declare both a type and a function under one name — Ecto.Schema has
// `@type schema` above `defmacro schema/2` — so file order alone cannot decide
// which one a bare reference means. preferType says which kind wins; the other
// kind is still returned when no match of the preferred kind exists.
func (tf *TokenizedFile) findDefinition(functionName string, preferType bool) (int, bool) {
	funcLine, typeLine := 0, 0

	for i := 0; i < tf.n; i++ {
		tok := tf.tokens[i]

		switch tok.Kind {
		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			if funcLine != 0 {
				continue
			}
			name, _, ok := parser.StaticDeclarationName(tf.source, tf.tokens, tf.n, i)
			if !ok {
				continue
			}
			if name == functionName {
				funcLine = tok.Line
				if !preferType {
					return funcLine, true
				}
			}

		case parser.TokAttrType:
			if typeLine != 0 {
				continue
			}
			name, _, ok := parser.StaticDeclarationName(tf.source, tf.tokens, tf.n, i)
			if !ok {
				continue
			}
			if name == functionName {
				typeLine = tok.Line
				if preferType {
					return typeLine, true
				}
			}
		}
	}

	if funcLine != 0 {
		return funcLine, true
	}
	if typeLine != 0 {
		return typeLine, true
	}
	return 0, false
}

// InTypespec reports whether the given 0-based line sits inside a @type,
// @typep, @opaque, @spec, @callback or @macrocallback declaration.
func (tf *TokenizedFile) InTypespec(targetLine int) bool {
	targetLine1 := targetLine + 1
	for i := 0; i < tf.n; i++ {
		tok := tf.tokens[i]
		if tok.Line > targetLine1 {
			break
		}
		switch tok.Kind {
		case parser.TokAttrType, parser.TokAttrSpec, parser.TokAttrCallback:
			end := parser.ScanTypespecEnd(tf.source, tf.tokens, tf.n, i)
			if end >= tf.n || tf.tokens[end].Line >= targetLine1 {
				return true
			}
			i = end
		}
	}
	return false
}

// ExtractAliasesInScope parses alias declarations visible at the given 0-based line.
func (tf *TokenizedFile) ExtractAliasesInScope(targetLine int) map[string]string {
	return extractAliasesFromTokens(tf.source, tf.tokens, targetLine)
}

// ExtractAliases parses all alias declarations from the tokenized file.
func (tf *TokenizedFile) ExtractAliases() map[string]string {
	return extractAliasesFromTokens(tf.source, tf.tokens, -1)
}

// ExtractImports returns all import declarations from the tokenized file.
func (tf *TokenizedFile) ExtractImports() []string {
	return extractImportsFromTokens(tf.source, tf.tokens)
}

// ExtractUses returns module names from all `use Module` declarations.
func (tf *TokenizedFile) ExtractUses() []string {
	return extractUsesFromTokens(tf.source, tf.tokens)
}

// ExtractUsesWithOpts parses all `use Module` declarations with keyword opts.
func (tf *TokenizedFile) ExtractUsesWithOpts(aliases map[string]string) []UseCall {
	return extractUsesWithOptsFromTokens(tf.source, tf.tokens, aliases)
}

// FindBufferFunctions scans the tokenized file for all function and type definitions.
func (tf *TokenizedFile) FindBufferFunctions() []BufferFunction {
	return findBufferFunctionsFromTokens(tf.source, tf.tokens)
}

// ExtractAliasBlockParent detects whether targetLine is inside a multi-line alias block.
func (tf *TokenizedFile) ExtractAliasBlockParent(targetLine int) (string, bool) {
	return extractAliasBlockParentFromTokens(tf.source, tf.tokens, targetLine)
}

// CompletionContext describes the token-aware completion prefix at the cursor.
type CompletionContext struct {
	Prefix   string
	AfterDot bool
	StartCol int
}

// Empty returns true if no completion should be offered at the cursor.
func (c CompletionContext) Empty() bool {
	return c.Prefix == "" && !c.AfterDot
}

// CompletionContextAtCursor extracts the completion prefix at the given 0-based
// line/column using the cached token stream. Unlike ExtractCompletionContext,
// this ignores strings/comments/heredocs and treats `::` distinctly from `:atom`.
func (tf *TokenizedFile) CompletionContextAtCursor(line, col int) CompletionContext {
	return CompletionContextAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
}

// CompletionContextAtCursor extracts the token-aware completion context at the
// given 0-based line/column.
func CompletionContextAtCursor(tokens []parser.Token, source []byte, lineStarts []int, line, col int) CompletionContext {
	if line < 0 || line >= len(lineStarts) || col <= 0 {
		return CompletionContext{}
	}

	lineStart := lineStarts[line]
	lineEnd := len(source)
	if line+1 < len(lineStarts) {
		lineEnd = lineStarts[line+1] - 1 // exclude the newline byte
	}
	maxCol := lineEnd - lineStart
	if maxCol < 0 {
		maxCol = 0
	}
	if col > maxCol {
		col = maxCol
	}

	offset := parser.LineColToOffset(lineStarts, line, col)
	if offset <= lineStart {
		return CompletionContext{}
	}

	idx := parser.TokenAtOffset(tokens, offset-1)
	if idx < 0 {
		return CompletionContext{}
	}

	tok := tokens[idx]
	if tok.Kind == parser.TokDot {
		exprIdx := idx - 1
		if exprIdx < 0 || !isCompletionSegmentToken(tokens[exprIdx].Kind) {
			return CompletionContext{}
		}
		startIdx := completionChainStart(tokens, exprIdx)
		prefix := buildCompletionPrefix(source, tokens, startIdx, exprIdx, tok.Start)
		if prefix == "" {
			return CompletionContext{}
		}
		return CompletionContext{
			Prefix:   prefix,
			AfterDot: true,
			StartCol: tokens[startIdx].Start - lineStart,
		}
	}

	if !isCompletionSegmentToken(tok.Kind) {
		return CompletionContext{}
	}

	startIdx := completionChainStart(tokens, idx)
	prefix := buildCompletionPrefix(source, tokens, startIdx, idx, offset)
	if prefix == "" {
		return CompletionContext{}
	}
	return CompletionContext{
		Prefix:   prefix,
		AfterDot: false,
		StartCol: tokens[startIdx].Start - lineStart,
	}
}

func completionChainStart(tokens []parser.Token, idx int) int {
	startIdx := idx
	for startIdx >= 2 {
		dotIdx := startIdx - 1
		prevIdx := startIdx - 2
		if tokens[dotIdx].Kind == parser.TokDot && isCompletionModuleToken(tokens[prevIdx].Kind) {
			startIdx = prevIdx
			continue
		}
		break
	}
	return startIdx
}

func buildCompletionPrefix(source []byte, tokens []parser.Token, startIdx, endIdx, endOffset int) string {
	var b strings.Builder
	for i := startIdx; i <= endIdx; i++ {
		tok := tokens[i]
		switch tok.Kind {
		case parser.TokDot:
			b.WriteByte('.')
		default:
			if !isCompletionSegmentToken(tok.Kind) {
				return ""
			}
			end := tok.End
			if i == endIdx && endOffset < end {
				end = endOffset
			}
			if end <= tok.Start {
				return ""
			}
			b.Write(source[tok.Start:end])
		}
	}
	return b.String()
}

func isCompletionModuleToken(k parser.TokenKind) bool {
	return k == parser.TokModule || k == parser.TokAtom
}

func isCompletionFunctionToken(k parser.TokenKind) bool {
	switch k {
	case parser.TokIdent,
		parser.TokDefmodule, parser.TokDefprotocol, parser.TokDefimpl,
		parser.TokDefstruct, parser.TokDefexception, parser.TokDefdelegate,
		parser.TokDefmacro, parser.TokDefmacrop, parser.TokDefguard,
		parser.TokDefguardp, parser.TokDefp, parser.TokDef,
		parser.TokAlias, parser.TokImport, parser.TokUse, parser.TokRequire,
		parser.TokDo, parser.TokEnd, parser.TokFn, parser.TokWhen:
		return true
	default:
		return false
	}
}

func isCompletionSegmentToken(k parser.TokenKind) bool {
	return isCompletionModuleToken(k) || isCompletionFunctionToken(k)
}

func isExprChar(b byte) bool {
	c := rune(b)
	return unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '.' || c == '?' || c == '!'
}

// CursorContext holds the result of token-based expression extraction at a
// cursor position. It replaces the combination of ExtractExpression +
// ExtractModuleAndFunction with a single token-aware lookup.
type CursorContext struct {
	// ModuleRef is the dot-joined module chain (e.g. "Foo.Bar"). Empty for
	// bare function calls.
	ModuleRef string
	// FunctionName is the lowercase identifier (e.g. "baz"). Empty for
	// module-only expressions like "Foo.Bar".
	FunctionName string
	// ExprStart is the 0-based column of the expression start on its line.
	ExprStart int
	// ExprEnd is the 0-based column one past the end of the expression.
	ExprEnd int
}

// Expr returns the combined dotted expression string (e.g. "Foo.Bar.baz").
func (c CursorContext) Expr() string {
	if c.ModuleRef == "" && c.FunctionName == "" {
		return ""
	}
	if c.ModuleRef == "" {
		return c.FunctionName
	}
	if c.FunctionName == "" {
		return c.ModuleRef
	}
	return c.ModuleRef + "." + c.FunctionName
}

// Empty returns true if no expression was found at the cursor.
func (c CursorContext) Empty() bool {
	return c.ModuleRef == "" && c.FunctionName == ""
}

// isExprToken returns true for token kinds that can be part of a dotted
// expression chain (Module.function or :atom.function).
func isExprToken(k parser.TokenKind) bool {
	return k == parser.TokModule || k == parser.TokIdent || k == parser.TokAtom
}

// ExpressionAtCursor extracts the dotted expression at the cursor position
// using the token stream. Unlike the char-based ExtractExpression, this
// correctly ignores expressions inside strings, comments, heredocs, sigils,
// and atoms.
//
// The returned expression is truncated to the cursor's segment (matching
// ExtractExpression behavior): cursor on "Foo" in "Foo.Bar.baz" returns
// only "Foo" as the module ref.
//
// line and col are 0-based.
func ExpressionAtCursor(tokens []parser.Token, source []byte, lineStarts []int, line, col int) CursorContext {
	return expressionAtCursorImpl(tokens, source, lineStarts, line, col, false)
}

// FullExpressionAtCursor is like ExpressionAtCursor but returns the complete
// dotted chain without truncating at the cursor's segment.
func FullExpressionAtCursor(tokens []parser.Token, source []byte, lineStarts []int, line, col int) CursorContext {
	return expressionAtCursorImpl(tokens, source, lineStarts, line, col, true)
}

// callArityAt returns the arity of the call of name at the cursor: the
// arguments, one more when a pipe gives the first, or the /N of a capture.
// ok is false when the cursor is not on such a call or its arity cannot be
// read, and for a definition head.
func (tf *TokenizedFile) callArityAt(line, col int, name string) (arity int, ok bool) {
	lo, hi, ok := tf.arityRangeAt(line, col, name)
	if !ok || lo != hi {
		return 0, false
	}
	return lo, true
}

// arityRangeAt returns the arities of the occurrence of name at the cursor:
// one for a call, a capture, or a keyword key, and a range for a head with
// default arguments. A call inside #{} is read from its interpolation. ok is
// false when the cursor is not on name or its arity cannot be read.
func (tf *TokenizedFile) arityRangeAt(line, col int, name string) (lo, hi int, ok bool) {
	offset := parser.LineColToOffset(tf.lineStarts, line, col)
	if offset < 0 {
		return 0, 0, false
	}
	at := func(toks []parser.Token) int {
		idx := parser.TokenAtOffset(toks, offset)
		if (idx < 0 || toks[idx].Kind != parser.TokIdent) && offset > 0 {
			idx = parser.TokenAtOffset(toks, offset-1)
		}
		if idx < 0 || toks[idx].Kind != parser.TokIdent || parser.TokenText(tf.source, toks[idx]) != name {
			return -1
		}
		return idx
	}
	if idx := at(tf.tokens); idx >= 0 {
		return tokenArity(tf.source, tf.tokens, idx)
	}
	if idx := at(tf.interp); idx >= 0 && tf.interpSpans != nil {
		if a := parser.InterpCallArity(tf.source, tf.interpSpans, tf.interp[idx]); a >= 0 {
			return a, a, true
		}
	}
	return 0, 0, false
}

func expressionAtCursorImpl(tokens []parser.Token, source []byte, lineStarts []int, line, col int, full bool) CursorContext {
	offset := parser.LineColToOffset(lineStarts, line, col)
	if offset < 0 {
		return CursorContext{}
	}

	n := len(tokens)
	idx := parser.TokenAtOffset(tokens, offset)

	// If cursor lands between tokens, try the token just before (handles
	// cursor immediately after an identifier with no gap).
	if idx < 0 {
		idx = parser.TokenAtOffset(tokens, offset-1)
		if idx < 0 {
			return CursorContext{}
		}
	}

	tok := tokens[idx]

	// Cursor on a dot: advance to the next significant token so we include
	// the segment after the dot (matching old behavior: cursor on dot →
	// include next segment).
	if tok.Kind == parser.TokDot {
		next := idx + 1
		if next < n && isExprToken(tokens[next].Kind) {
			idx = next
			tok = tokens[idx]
		} else {
			return CursorContext{}
		}
	}

	// A cursor right after an identifier sits on the next token, such as
	// `(`, `,` or the end of the line. Use the identifier that ends there.
	if !isExprToken(tok.Kind) && idx > 0 && tokens[idx-1].End == offset && isExprToken(tokens[idx-1].Kind) {
		idx--
		tok = tokens[idx]
	}

	// Reject non-expression tokens (strings, comments, atoms, etc.)
	if !isExprToken(tok.Kind) {
		return CursorContext{}
	}

	// cursorIdx is the token the cursor is physically on — used for truncation
	cursorIdx := idx

	// Walk backward through Dot+Module/Ident chains to find the start
	startIdx := idx
	for startIdx >= 2 {
		dotIdx := startIdx - 1
		prevIdx := startIdx - 2
		if tokens[dotIdx].Kind == parser.TokDot && isExprToken(tokens[prevIdx].Kind) {
			startIdx = prevIdx
		} else {
			break
		}
	}

	// Walk forward through Dot+Module/Ident chains to find the end
	endIdx := idx
	for endIdx+2 < n {
		dotIdx := endIdx + 1
		nextIdx := endIdx + 2
		if tokens[dotIdx].Kind == parser.TokDot && isExprToken(tokens[nextIdx].Kind) {
			endIdx = nextIdx
		} else {
			break
		}
	}

	// Determine truncation point: include up to the cursor's segment
	truncEnd := endIdx
	if !full {
		truncEnd = cursorIdx
	}

	// Build module ref and function name from the token chain
	lineStart := 0
	if line < len(lineStarts) {
		lineStart = lineStarts[line]
	}

	var moduleParts []string
	functionName := ""

	for ti := startIdx; ti <= truncEnd; ti += 2 {
		t := tokens[ti]
		text := parser.TokenText(source, t)
		switch t.Kind {
		case parser.TokModule, parser.TokAtom:
			moduleParts = append(moduleParts, text)
		default:
			// TokIdent — this is the function name; stop here
			functionName = text
		}
		if functionName != "" {
			break
		}
	}

	moduleRef := ""
	if len(moduleParts) > 0 {
		moduleRef = strings.Join(moduleParts, ".")
	}

	exprStart := tokens[startIdx].Start - lineStart
	lastTok := tokens[truncEnd]
	if functionName != "" {
		// Find the ident token for end position
		for ti := startIdx; ti <= truncEnd; ti += 2 {
			if tokens[ti].Kind == parser.TokIdent {
				lastTok = tokens[ti]
				break
			}
		}
	}
	exprEnd := lastTok.End - lineStart

	return CursorContext{
		ModuleRef:    moduleRef,
		FunctionName: functionName,
		ExprStart:    exprStart,
		ExprEnd:      exprEnd,
	}
}

// ExtractModuleAndFunction splits a dotted expression into module reference and optional function name.
// Uppercase-starting parts are module segments, the first lowercase part is the function.
// Returns ("Foo.Bar", "baz") for "Foo.Bar.baz", ("Foo.Bar.Baz", "") for "Foo.Bar.Baz",
// ("", "do_something") for "do_something".
//
// Deprecated: Use ExpressionAtCursor which returns ModuleRef and FunctionName directly.
func ExtractModuleAndFunction(expr string) (moduleRef string, functionName string) {
	var moduleParts []string
	for _, part := range strings.Split(expr, ".") {
		if len(part) == 0 {
			continue
		}
		if unicode.IsUpper(rune(part[0])) {
			moduleParts = append(moduleParts, part)
		} else {
			functionName = part
			break
		}
	}
	if len(moduleParts) > 0 {
		moduleRef = strings.Join(moduleParts, ".")
	}
	return
}

// ExtractCompletionContext extracts the typing context for autocompletion.
// Unlike ExtractExpression (which requires the cursor on an expression char),
// this scans backward from col to handle incomplete expressions like "Foo.|".
// Returns the prefix text, whether the cursor is immediately after a dot,
// and the start column of the prefix (for building textEdit ranges).
func ExtractCompletionContext(line string, col int) (prefix string, afterDot bool, startCol int) {
	if col <= 0 || len(line) == 0 {
		return "", false, 0
	}
	if col > len(line) {
		col = len(line)
	}

	end := col - 1
	if end < 0 || !isExprChar(line[end]) {
		return "", false, 0
	}

	start := end
	for start > 0 && isExprChar(line[start-1]) {
		start--
	}

	// Include a leading colon for Erlang module references (:lists, :ets, etc.)
	if start > 0 && line[start-1] == ':' {
		start--
	}

	raw := line[start : end+1]

	// Trim trailing dots — "Foo." means afterDot=true, prefix="Foo"
	if strings.HasSuffix(raw, ".") {
		return strings.TrimSuffix(raw, "."), true, start
	}

	return raw, false, start
}

// ExtractAliasBlockParent detects whether the given 0-based line is inside
// a multi-line alias brace block (alias Parent.{ ... }). If so, it returns
// the resolved parent module name. This is used by the completion and hover
// handlers to resolve module names inside multi-line alias blocks.
func ExtractAliasBlockParent(lines []string, targetLine int) (string, bool) {
	if targetLine < 0 || targetLine >= len(lines) {
		return "", false
	}

	// Quick pre-check: scan backward for an "alias ...{" line without a
	// matching "}" on the same line. Pure string ops, no allocations in
	// the fast path, so this is nearly free for the 99% of hover/definition
	// requests that are not inside an alias block.
	found := false
	for i := targetLine; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "alias ") && strings.Contains(trimmed, "{") && !strings.Contains(trimmed, "}") {
			found = true
			break
		}
		// Any def/defp/defmodule means we've left the possible alias context.
		if strings.HasPrefix(trimmed, "def ") || strings.HasPrefix(trimmed, "defp ") || strings.HasPrefix(trimmed, "defmodule ") {
			break
		}
	}
	if !found {
		return "", false
	}

	source := []byte(strings.Join(lines, "\n"))
	return extractAliasBlockParentFromTokens(source, parser.Tokenize(source), targetLine)
}

func extractAliasBlockParentFromTokens(source []byte, tokens []parser.Token, targetLine int) (string, bool) {
	n := len(tokens)
	if targetLine < 0 || n == 0 {
		return "", false
	}

	targetLine1 := targetLine + 1

	// Find the token position for the target line
	targetIdx := n - 1
	for i, tok := range tokens {
		if tok.Line >= targetLine1 {
			targetIdx = i
			break
		}
	}

	// Check if target line has only a closing brace (no module content)
	hasModuleOnLine := false
	hasCloseBraceOnLine := false
	for i := targetIdx; i < n && tokens[i].Line == targetLine1; i++ {
		if tokens[i].Kind == parser.TokModule {
			hasModuleOnLine = true
		}
		if tokens[i].Kind == parser.TokCloseBrace {
			hasCloseBraceOnLine = true
		}
	}
	if hasCloseBraceOnLine && !hasModuleOnLine {
		return "", false
	}

	// Scan backward through tokens looking for "alias Parent.{" without matching "}"
	for i := targetIdx; i >= 0; i-- {
		tok := tokens[i]

		// If we see a closing brace before finding alias, we're not in an open block
		if tok.Kind == parser.TokCloseBrace && tok.Line < targetLine1 {
			return "", false
		}

		if tok.Kind != parser.TokAlias {
			continue
		}

		// Found alias — collect the module name
		j := tokNextSig(tokens, n, i+1)
		modName, k := tokCollectModuleName(source, tokens, n, j)
		if modName == "" {
			return "", false
		}

		// Check for ".{" after module name
		if k >= n || tokens[k].Kind != parser.TokDot {
			return "", false
		}
		k++
		if k >= n || tokens[k].Kind != parser.TokOpenBrace {
			return "", false
		}
		openBraceLine := tokens[k].Line

		// Check that "}" is NOT on the same line as "{"
		for m := k + 1; m < n; m++ {
			if tokens[m].Line > openBraceLine {
				break
			}
			if tokens[m].Kind == parser.TokCloseBrace {
				if tokens[m].Line == openBraceLine {
					return "", false // single-line alias block
				}
				break
			}
		}

		// We're inside a multi-line alias block — resolve the parent module
		parent := modName

		// Resolve __MODULE__ using enclosing module from token stream
		aliasLine := tok.Line - 1 // convert to 0-based
		enclosingModule := extractEnclosingModuleFromTokens(source, tokens, aliasLine)
		if enclosingModule != "" {
			parent = strings.ReplaceAll(parent, "__MODULE__", enclosingModule)
		}
		if strings.Contains(parent, "__MODULE__") {
			return "", false
		}
		return parent, true
	}

	return "", false
}

// tokParseModuleDef reads the defmodule, defprotocol, or defimpl whose
// keyword is tokens[from-1]: the module it defines inside currentModule (see
// parser.NestedModuleName), and where its body starts. A defimpl gives the
// protocol name as written: it is not nested, and its aliases are not
// resolved here.
func tokParseModuleDef(source []byte, tokens []parser.Token, from int, currentModule string) (name string, nextPos int, hasDo bool) {
	name, _, _, nextPos, hasDo = tokParseModuleDefAlias(source, tokens, from, currentModule)
	return name, nextPos, hasDo
}

// tokParseModuleDefAlias is tokParseModuleDef that also returns the alias a
// nested defmodule or defprotocol adds in currentModule.
func tokParseModuleDefAlias(source []byte, tokens []parser.Token, from int, currentModule string) (name, aliasShort, aliasFull string, nextPos int, hasDo bool) {
	n := len(tokens)
	j := tokNextSig(tokens, n, from)
	name, k := tokCollectModuleName(source, tokens, n, j)
	if name == "" {
		return "", "", "", from, false
	}
	if from == 0 || tokens[from-1].Kind != parser.TokDefimpl {
		name, aliasShort, aliasFull = parser.NestedModuleName(name, currentModule)
	}
	_, nextPos, hasDo = parser.ScanForwardToBlockDo(tokens, n, k)
	return name, aliasShort, aliasFull, nextPos, hasDo
}

// extractEnclosingModuleFromTokens finds the innermost defmodule enclosing the given 0-based line.
func extractEnclosingModuleFromTokens(source []byte, tokens []parser.Token, targetLine int) string {
	n := len(tokens)
	targetLine1 := targetLine + 1

	type moduleFrame struct {
		name  string
		depth int
	}
	var stack []moduleFrame
	depth := 0

	processModuleDef := func(i int) int {
		currentModule := ""
		if len(stack) > 0 {
			currentModule = stack[len(stack)-1].name
		}
		name, nextPos, hasDo := tokParseModuleDef(source, tokens, i, currentModule)
		if name == "" {
			return i
		}
		if hasDo {
			depth++
			stack = append(stack, moduleFrame{name: name, depth: depth})
		}
		return nextPos
	}

	for i := 0; i < n; i++ {
		tok := tokens[i]
		if tok.Line > targetLine1 {
			break
		}

		switch tok.Kind {
		case parser.TokDo, parser.TokFn:
			parser.TrackBlockDepth(tok.Kind, &depth)
		case parser.TokEnd:
			prevDepth := depth
			parser.TrackBlockDepth(tok.Kind, &depth)
			if len(stack) > 0 && stack[len(stack)-1].depth == prevDepth {
				stack = stack[:len(stack)-1]
			}
		case parser.TokDefmodule, parser.TokDefprotocol, parser.TokDefimpl:
			i = processModuleDef(i+1) - 1 // -1: loop post-increment will advance to the returned position
			continue
		}
	}

	if len(stack) > 0 {
		return stack[len(stack)-1].name
	}
	return ""
}

// IsDefmoduleLine returns true if the given 0-based line contains a defmodule
// keyword, and returns the module name being defined on that line.
func IsDefmoduleLine(text string, lineNum int) (string, bool) {
	// Fast path: check if the line even contains "defmodule"
	lines := strings.Split(text, "\n")
	if lineNum < 0 || lineNum >= len(lines) {
		return "", false
	}
	if !strings.Contains(lines[lineNum], "defmodule") {
		return "", false
	}

	// Tokenize just that line to extract the module name
	source := []byte(lines[lineNum])
	tokens := parser.Tokenize(source)
	n := len(tokens)

	for i := 0; i < n; i++ {
		if tokens[i].Kind == parser.TokDefmodule {
			j := tokNextSig(tokens, n, i+1)
			name, _ := tokCollectModuleName(source, tokens, n, j)
			if name != "" {
				return name, true
			}
		}
	}
	return "", false
}

// FindModuleAttributeDefinitionTokenized searches for the line where @attr_name
// is defined (assigned a value, not used). Returns the 1-based line number and
// true if found. Uses the tokenizer for accurate parsing.
func FindModuleAttributeDefinitionTokenized(text string, attrName string) (int, bool) {
	if reservedModuleAttrs[attrName] {
		return 0, false
	}

	source := []byte(text)
	tokens := parser.Tokenize(source)
	n := len(tokens)

	for i := 0; i < n; i++ {
		tok := tokens[i]
		if tok.Kind != parser.TokAttr {
			continue
		}

		// TokAttr includes the @ prefix, so extract the name
		attrText := parser.TokenText(source, tok)
		if len(attrText) < 2 || attrText[0] != '@' {
			continue
		}
		name := attrText[1:]
		if name != attrName {
			continue
		}

		// Match only line-start attributes (equivalent to ^\s*@attr from
		// the line-based parser), not references inside expressions.
		atLineStart := true
		for k := i - 1; k >= 0 && tokens[k].Kind != parser.TokEOL; k-- {
			if tokens[k].Kind != parser.TokComment {
				atLineStart = false
				break
			}
		}
		if !atLineStart {
			continue
		}

		// A definition needs a value token on the same line after @attr.
		j := i + 1
		for j < n && tokens[j].Kind == parser.TokComment {
			j++
		}
		if j >= n || tokens[j].Kind == parser.TokEOL || tokens[j].Line != tok.Line {
			continue
		}
		// Skip invalid `@attr @other_attr` patterns.
		if tokens[j].Kind == parser.TokAttr {
			continue
		}

		return tok.Line, true
	}
	return 0, false
}

// IsPipeContext returns true if the text before prefixStartCol on this line
// contains a pipe operator (|>), meaning the first argument is supplied by the
// pipe and should be omitted from the completion snippet.
//
// Theoretically, this could cause false positives for pipes within strings. If
// this becomes an annoying problem (I don't think it will) then we can fix.
func IsPipeContext(line string, prefixStartCol int) bool {
	before := line
	if prefixStartCol < len(line) {
		before = line[:prefixStartCol]
	}
	return strings.Contains(strings.TrimSpace(before), "|>")
}

type BufferFunction struct {
	Name   string
	Arity  int
	Kind   string
	Params string
}

// FindBufferFunctions scans document text for all function and type definitions.
// Returns a deduplicated list (multi-clause functions with the same arity appear once).
// Functions with default parameters emit one entry per callable arity.
// Private types (@typep) are included since they are accessible within the same file.
func FindBufferFunctions(text string) []BufferFunction {
	source := []byte(text)
	return findBufferFunctionsFromTokens(source, parser.Tokenize(source))
}

func findBufferFunctionsFromTokens(source []byte, tokens []parser.Token) []BufferFunction {
	n := len(tokens)
	seen := make(map[string]bool)
	var results []BufferFunction

	for i := 0; i < n; i++ {
		tok := tokens[i]

		switch tok.Kind {
		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			kind := parser.TokenText(source, tok)
			name, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
			if !ok {
				continue
			}
			j++
			pj := tokNextSig(tokens, n, j)
			maxArity := 0
			defaultCount := 0
			var paramNames []string
			if pj < n && tokens[pj].Kind == parser.TokOpenParen {
				maxArity, defaultCount, paramNames, _ = parser.CollectParams(source, tokens, n, pj)
				paramNames = parser.FixParamNames(paramNames)
			}
			minArity := maxArity - defaultCount
			for arity := minArity; arity <= maxArity; arity++ {
				key := name + "/" + strconv.Itoa(arity)
				if seen[key] {
					continue
				}
				seen[key] = true
				results = append(results, BufferFunction{
					Name:   name,
					Arity:  arity,
					Kind:   kind,
					Params: parser.JoinParams(paramNames, arity),
				})
			}

		case parser.TokAttrType:
			attrText := parser.TokenText(source, tok)
			kind := "type"
			switch attrText {
			case "@opaque":
				kind = "opaque"
			case "@typep":
				kind = "typep"
			}
			name, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
			if !ok {
				continue
			}
			arity := 0
			pj := tokNextSig(tokens, n, j+1)
			if pj < n && tokens[pj].Kind == parser.TokOpenParen {
				arity, _, _, _ = parser.CollectParams(source, tokens, n, pj)
			}
			key := name + "/" + strconv.Itoa(arity)
			if seen[key] {
				continue
			}
			seen[key] = true
			results = append(results, BufferFunction{Name: name, Arity: arity, Kind: kind})
		}
	}
	return results
}

// ExtractAliases parses all alias declarations from document text.
// Returns a map of short name -> full module name (not scope-aware).
func ExtractAliases(text string) map[string]string {
	return extractAliasesFromText(text, -1)
}

// ExtractAliasesInScope parses alias declarations visible at the given 0-based
// line. In Elixir, aliases are lexically scoped to the enclosing defmodule —
// a nested module does NOT inherit its parent's aliases.
func ExtractAliasesInScope(text string, targetLine int) map[string]string {
	return extractAliasesFromText(text, targetLine)
}

// extractAliasesFromText is the shared implementation using the tokenizer.
// When targetLine >= 0, only aliases from the module scope enclosing that
// 0-based line are returned. Uses a single pass over the token stream.
func extractAliasesFromText(text string, targetLine int) map[string]string {
	source := []byte(text)
	tokens := parser.Tokenize(source)
	return extractAliasesFromTokens(source, tokens, targetLine)
}

// extractAliasesFromTokens is the implementation that works with pre-tokenized data.
func extractAliasesFromTokens(source []byte, tokens []parser.Token, targetLine int) map[string]string {
	n := len(tokens)

	type moduleFrame struct {
		name  string
		depth int
		// id identifies this scope; parentScopes[id] is the enclosing one.
		id int
		// aliases in scope here, seeded from the parent (Elixir aliases are
		// lexically scoped, so a nested module sees the enclosing ones) and
		// extended as this scope declares more. Drives chain resolution.
		// Allocated lazily — most scopes declare none.
		aliases map[string]string
	}

	var stack []moduleFrame
	depth := 0
	var topAliases map[string]string

	// parentScopes[id] is the id of the scope enclosing scope id. Scope 0 is
	// the file itself; frames pop off the stack, so this keeps the chain
	// available for the ancestor walk at the end.
	parentScopes := []int{0}

	type aliasEntry struct {
		scope, line int
		short, full string
	}
	var allAliases []aliasEntry
	targetScope := -1 // -1: the target line's scope is not known yet
	unscoped := targetLine < 0
	// targetLine is 0-based; token.Line is 1-based
	targetLine1 := targetLine + 1

	currentScope := func() int {
		if len(stack) > 0 {
			return stack[len(stack)-1].id
		}
		return 0
	}

	currentModule := func() string {
		if len(stack) > 0 {
			return stack[len(stack)-1].name
		}
		return ""
	}

	resolve := func(s string) string {
		cm := currentModule()
		if cm != "" {
			return strings.ReplaceAll(s, "__MODULE__", cm)
		}
		return s
	}

	curAliases := func() map[string]string {
		if len(stack) > 0 {
			return stack[len(stack)-1].aliases
		}
		return topAliases
	}

	// record stores an alias both in the flat list (for the final scope filter)
	// and in the current scope's map (for chain resolution of later lines).
	record := func(line int, short, full string) {
		allAliases = append(allAliases, aliasEntry{currentScope(), line, short, full})
		if len(stack) > 0 {
			if stack[len(stack)-1].aliases == nil {
				stack[len(stack)-1].aliases = make(map[string]string, 8)
			}
			stack[len(stack)-1].aliases[short] = full
			return
		}
		if topAliases == nil {
			topAliases = make(map[string]string, 8)
		}
		topAliases[short] = full
	}

	// resolveRef turns a module reference on an alias/require line into a
	// canonical name: it rewrites the leading segment through the aliases
	// already declared in this scope (`alias A.B` then `require B, as: X`),
	// then substitutes __MODULE__.
	resolveRef := func(modRef string) string {
		return resolve(parser.ExpandAliasPrefix(modRef, curAliases()))
	}

	processModuleDef := func(i int) int {
		name, short, full, nextPos, hasDo := tokParseModuleDefAlias(source, tokens, i, currentModule())
		if name == "" {
			return i
		}
		if i > 0 && tokens[i-1].Kind == parser.TokDefimpl {
			// The protocol name is a module reference.
			if resolved := resolveRef(name); !strings.Contains(resolved, "__MODULE__") {
				name = resolved
			}
		} else if short != "" {
			// A nested module is an alias in the module around it, from
			// its defmodule line on; the frame below inherits it.
			record(tokens[i-1].Line, short, full)
		}
		if hasDo {
			depth++
			parentScopes = append(parentScopes, currentScope())
			frame := moduleFrame{name: name, depth: depth, id: len(parentScopes) - 1}
			// Copy in what the enclosing scope has declared so far, so a chain
			// crosses the defmodule: `alias My.App.Repo` in the parent then
			// `alias Repo.Helper` here expands to My.App.Repo.Helper.
			if inherited := curAliases(); len(inherited) > 0 {
				frame.aliases = make(map[string]string, len(inherited)+8)
				for short, full := range inherited {
					frame.aliases[short] = full
				}
			}
			stack = append(stack, frame)
		}
		return nextPos
	}

	for i := 0; i < n; i++ {
		tok := tokens[i]

		// Track target line's module scope (check before any depth changes)
		if !unscoped && targetScope < 0 && tok.Line >= targetLine1 {
			targetScope = currentScope()
		}

		switch tok.Kind {
		case parser.TokDo, parser.TokFn:
			parser.TrackBlockDepth(tok.Kind, &depth)
		case parser.TokEnd:
			prevDepth := depth
			parser.TrackBlockDepth(tok.Kind, &depth)
			if len(stack) > 0 && stack[len(stack)-1].depth == prevDepth {
				stack = stack[:len(stack)-1]
			}

		case parser.TokDefmodule, parser.TokDefprotocol, parser.TokDefimpl:
			i = processModuleDef(i+1) - 1 // -1: loop post-increment will advance to the returned position
			continue

		case parser.TokAlias:
			line := tok.Line
			j := tokNextSig(tokens, n, i+1)
			modName, k := tokCollectModuleName(source, tokens, n, j)
			if modName == "" {
				continue
			}

			// Multi-alias: alias Parent.{A, B, C}
			if children, nextPos, ok := parser.ScanMultiAliasChildren(source, tokens, n, k, true); ok {
				base := resolveRef(modName)
				if strings.Contains(base, "__MODULE__") {
					continue
				}
				for _, child := range children {
					record(line, parser.AliasShortName(child), base+"."+child)
				}
				i = nextPos - 1
				continue
			}

			// Check for alias Module, as: Name
			if asName, nextPos, ok := parser.ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
				resolved := resolveRef(modName)
				if !strings.Contains(resolved, "__MODULE__") {
					record(line, asName, resolved)
				}
				i = nextPos - 1
				continue
			}

			// Simple alias
			resolved := resolveRef(modName)
			if !strings.Contains(resolved, "__MODULE__") {
				record(line, parser.AliasShortName(resolved), resolved)
			}
			i = k - 1

		case parser.TokRequire:
			line := tok.Line
			j := tokNextSig(tokens, n, i+1)
			modName, k := tokCollectModuleName(source, tokens, n, j)
			if modName == "" {
				continue
			}

			// Check for require Module, as: Name
			if asName, nextPos, ok := parser.ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
				resolved := resolveRef(modName)
				if !strings.Contains(resolved, "__MODULE__") {
					record(line, asName, resolved)
				}
				i = nextPos - 1
				continue
			}
			i = k - 1
		}
	}

	// If targetLine was past all tokens, resolve now
	if !unscoped && targetScope < 0 {
		targetScope = currentScope()
	}

	// An alias is in scope at the target line if it was declared in the
	// target's own scope, or in an enclosing one before the target line.
	// Enclosing scopes need the line test: a parent alias written after the
	// nested module closed was never in scope inside it.
	inScope := func(a aliasEntry) bool {
		if unscoped || a.scope == targetScope {
			return true
		}
		if a.line >= targetLine1 {
			return false
		}
		for s := parentScopes[targetScope]; ; s = parentScopes[s] {
			if a.scope == s {
				return true
			}
			if s == 0 {
				return false
			}
		}
	}

	aliases := make(map[string]string)
	for _, a := range allAliases {
		if inScope(a) {
			aliases[a.short] = a.full
		}
	}
	return aliases
}

// Token-walking aliases for the shared parser helpers.
var (
	tokNextSig           = parser.NextSigToken
	tokCollectModuleName = parser.CollectModuleName
)

// ExtractImports parses all import declarations from document text.
// Returns a slice of full module names.
func ExtractImports(text string) []string {
	source := []byte(text)
	return extractImportsFromTokens(source, parser.Tokenize(source))
}

func extractImportsFromTokens(source []byte, tokens []parser.Token) []string {
	n := len(tokens)
	var imports []string
	for i := 0; i < n; i++ {
		if tokens[i].Kind != parser.TokImport {
			continue
		}
		j := tokNextSig(tokens, n, i+1)
		mod, _ := tokCollectModuleName(source, tokens, n, j)
		if mod != "" {
			imports = append(imports, mod)
		}
	}
	return imports
}

// skipToEndOfStatement advances from the given token index past the current statement
// (to the next TokEOL at bracket/block depth 0, or to end of tokens).
func skipToEndOfStatement(tokens []parser.Token, n, from int) int {
	depth := 0
	blockDepth := 0
	for i := from; i < n; i++ {
		switch tokens[i].Kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace, parser.TokOpenAngle:
			depth++
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace, parser.TokCloseAngle:
			if depth > 0 {
				depth--
			}
		case parser.TokDo, parser.TokFn:
			blockDepth++
		case parser.TokEnd:
			if blockDepth > 0 {
				blockDepth--
			}
		case parser.TokEOL, parser.TokEOF:
			if depth <= 0 && blockDepth <= 0 {
				return i
			}
		}
	}
	return n
}

// parseHelperQuoteBlock finds `def/defp helperName` in the source text, locates
// its `quote do` block, and extracts imports/uses/inline-defs/aliases from it.
// Uses the tokenizer for correct heredoc and multi-line handling.
func parseHelperQuoteBlock(lines []string, helperName string, fileAliases map[string]string) (imported []string, inlineDefs map[string][]inlineDef, transUses []string, optBindings []optBinding, aliases map[string]string) {
	imported, inlineDefs, transUses, _, optBindings, aliases, _, _ = parseHelperQuoteBlockDetailed(newHelperSource(lines), helperName, fileAliases, make(map[string]bool))
	return
}

// helperSource is a file for the helper parser, tokenized once: a __using__
// body can call many helpers, and each one is parsed from the whole file.
type helperSource struct {
	lines  []string
	source []byte
	tokens []parser.Token
}

func newHelperSource(lines []string) *helperSource {
	source := []byte(strings.Join(lines, "\n"))
	return &helperSource{lines: lines, source: source, tokens: parser.Tokenize(source)}
}

func parseHelperQuoteBlockDetailed(hs *helperSource, helperName string, fileAliases map[string]string, visited map[string]bool) (imported []string, inlineDefs map[string][]inlineDef, transUses []string, transUseCalls []UseCall, optBindings []optBinding, aliases map[string]string, derives []string, optUses []string) {
	if visited[helperName] {
		return
	}
	visited[helperName] = true
	defer delete(visited, helperName)

	source, tokens := hs.source, hs.tokens
	n := len(tokens)

	resolveAlias := func(modName string) string {
		if modName == unknownOpt {
			return modName
		}
		if resolved := parser.ResolveModuleRef(modName, aliases, ""); resolved != modName {
			return resolved
		}
		return parser.ResolveModuleRef(modName, fileAliases, "")
	}

	// Find def/defp helperName
	helperStart := -1
	for i := 0; i < n; i++ {
		if tokens[i].Kind != parser.TokDef && tokens[i].Kind != parser.TokDefp {
			continue
		}
		name, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
		if ok && name == helperName {
			// Find the TokDo that opens this function. Don't stop at TokEOL
			// because Elixir allows `do` on the next line after multi-line params.
			if _, nextPos, hasDo := parser.ScanForwardToBlockDo(tokens, n, j+1); hasDo {
				helperStart = nextPos
			}
			break
		}
	}
	if helperStart < 0 {
		return
	}

	// Find `quote do` inside the function body
	quoteBodyStart := -1
	depth := 1
	for i := helperStart; i < n && depth > 0; i++ {
		parser.TrackBlockDepth(tokens[i].Kind, &depth)
		switch tokens[i].Kind {
		case parser.TokIdent:
			if string(source[tokens[i].Start:tokens[i].End]) == "quote" {
				if _, nextPos, hasDo := parser.ScanForwardToBlockDo(tokens, n, i+1); hasDo {
					quoteBodyStart = nextPos
				}
			}
		}
		if quoteBodyStart >= 0 {
			break
		}
	}
	if quoteBodyStart < 0 {
		return
	}
	// The helper's statements before its quote bind its options variables
	// (`opts = Keyword.put_new(opts, :key, Mod)`) and its reads of one option
	// (`mod = opts[:key] || Mod`), as in a __using__ body.
	// unquote(__MODULE__) in the quote, and __MODULE__ in the code before
	// it, is the module that defines the helper.
	definingModule := extractEnclosingModuleFromTokens(source, tokens, tokens[helperStart].Line-1)
	ob := newOptBinder(source, tokens, definingModule, resolveAlias)
	vars := ob.vars
	depth = 1
	for i := helperStart; i < quoteBodyStart && depth > 0; i++ {
		parser.TrackBlockDepth(tokens[i].Kind, &depth)
		ob.block(tokens[i].Kind)
		ob.boundary(i)
		if i > 0 && tokens[i-1].Kind != parser.TokEOL && tokens[i-1].Kind != parser.TokComment {
			continue
		}
		switch tokens[i].Kind {
		case parser.TokIdent:
			if end, mods, handled := ob.assignment(i, fileAliases); handled {
				for _, m := range mods {
					optUses = append(optUses, resolveAlias(m))
				}
				i = end - 1
			}
		case parser.TokOpenBrace:
			if end, handled := ob.tupleAssignment(i); handled {
				i = end - 1
			}
		}
	}

	// Walk the quote do block (depth 1 = inside quote do, 0 = we hit its end)
	inlineDefs = make(map[string][]inlineDef)
	depth = 1
	for i := quoteBodyStart; i < n && depth > 0; i++ {
		tok := tokens[i]
		parser.TrackBlockDepth(tok.Kind, &depth)
		switch tok.Kind {

		case parser.TokImport:
			j := tokNextSig(tokens, n, i+1)
			if self, k, ok := unquotedSelfModule(source, tokens, j, definingModule); ok {
				imported = append(imported, self)
				i = k - 1
				continue
			}
			// import unquote(var), with var bound before the quote
			if j+2 < n && tokens[j].Kind == parser.TokIdent && parser.TokenText(source, tokens[j]) == "unquote" && tokens[j+1].Kind == parser.TokOpenParen && tokens[j+2].Kind == parser.TokIdent {
				if b, ok := ob.varToOpt[parser.TokenText(source, tokens[j+2])]; ok {
					optBindings = append(optBindings, optBinding{optKey: b.optKey, defaultMod: b.defaultMod, kind: "import", readEdits: b.readEdits})
				}
				i = skipToEndOfStatement(tokens, n, j) - 1
				continue
			}
			mod, _ := tokCollectModuleName(source, tokens, n, j)
			if mod != "" {
				imported = append(imported, resolveAlias(mod))
			}

		case parser.TokUse:
			j := useArgStart(tokens, n, i)
			self, k, isSelf := unquotedSelfModule(source, tokens, j, definingModule)
			// use unquote(var), or use unquote(<a read of an option>)
			if !isSelf && j+1 < n && tokens[j].Kind == parser.TokIdent && parser.TokenText(source, tokens[j]) == "unquote" && tokens[j+1].Kind == parser.TokOpenParen {
				if b, ok := ob.unquotedBinding(j + 1); ok {
					binding := optBinding{optKey: b.optKey, defaultMod: b.defaultMod, kind: "use", readEdits: b.readEdits}
					if end := matchingBracket(tokens, n, j+1); end > 0 {
						if c := tokNextSig(tokens, n, end+1); c < n && tokens[c].Kind == parser.TokComma {
							binding.forwardEdits, binding.forwardOpts, _ = forwardedOpts(source, tokens, n, c+1, vars, ob.quoteScope(fileAliases))
						}
					}
					optBindings = append(optBindings, binding)
				}
				i = skipToEndOfStatement(tokens, n, j) - 1
				continue
			}
			mod := self
			if !isSelf {
				mod, k = tokCollectModuleName(source, tokens, n, j)
			}
			if mod != "" {
				resolved := mod
				if !isSelf {
					resolved = resolveAlias(mod)
				}
				transUses = append(transUses, resolved)
				call := UseCall{Module: resolved}
				nk := tokNextSig(tokens, n, k)
				if nk < n && tokens[nk].Kind == parser.TokComma {
					call.Opts = useCallOpts(source, tokens, n, nk+1, ob.quoteScope(fileAliases))
					call.Which, call.WhichKey = tokCollectDispatchAtom(source, tokens, n, nk+1)
					call.forwardEdits, call.forwardOpts, call.callerWins = forwardedOpts(source, tokens, n, nk+1, vars, ob.quoteScope(fileAliases))
				}
				transUseCalls = append(transUseCalls, call)
			}

		case parser.TokAlias:
			j := tokNextSig(tokens, n, i+1)
			if self, k, ok := unquotedSelfModule(source, tokens, j, definingModule); ok {
				if aliases == nil {
					aliases = make(map[string]string)
				}
				if asName, nextPos, ok := parser.ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
					aliases[asName] = self
					k = nextPos
				} else {
					aliases[parser.AliasShortName(self)] = self
				}
				i = k - 1
				continue
			}
			modName, k := tokCollectModuleName(source, tokens, n, j)
			if modName == "" {
				continue
			}
			// Multi-alias: alias Parent.{A, B}
			if children, nextPos, ok := parser.ScanMultiAliasChildren(source, tokens, n, k, false); ok {
				base := resolveAlias(modName)
				for _, child := range children {
					if aliases == nil {
						aliases = make(map[string]string)
					}
					aliases[parser.AliasShortName(child)] = base + "." + child
				}
				i = nextPos - 1
				continue
			}
			// alias Module, as: Name
			if asName, nextPos, ok := parser.ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
				if aliases == nil {
					aliases = make(map[string]string)
				}
				aliases[asName] = resolveAlias(modName)
				i = nextPos - 1
				continue
			}
			// Simple alias
			resolved := resolveAlias(modName)
			if aliases == nil {
				aliases = make(map[string]string)
			}
			aliases[parser.AliasShortName(resolved)] = resolved
			i = k - 1

		case parser.TokRequire:
			// require Module, as: Name — injects an alias just like `alias`.
			j := tokNextSig(tokens, n, i+1)
			modName, k := tokCollectModuleName(source, tokens, n, j)
			if modName == "" {
				continue
			}
			if asName, nextPos, ok := parser.ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
				if aliases == nil {
					aliases = make(map[string]string)
				}
				aliases[asName] = resolveAlias(modName)
				i = nextPos - 1
				continue
			}
			i = k - 1

		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			kind := string(source[tok.Start:tok.End])
			defLine := tok.Line
			funcName, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
			if !ok {
				i = skipToEndOfStatement(tokens, n, j) - 1
				continue
			}
			j++
			pj := tokNextSig(tokens, n, j)
			nextPos := pj
			maxArity := 0
			defaultCount := 0
			var paramNames []string
			if pj < n && tokens[pj].Kind == parser.TokOpenParen {
				maxArity, defaultCount, paramNames, nextPos = collectParamsFromTokens(source, tokens, n, pj)
				paramNames = parser.FixParamNames(paramNames)
			}
			minArity := maxArity - defaultCount
			end := skipToEndOfStatement(tokens, n, nextPos)
			var delegateTo, delegateAs string
			if kind == "defdelegate" {
				delegateTo, delegateAs = delegateOptions(source, tokens, nextPos, end, resolveAlias)
			}
			for arity := minArity; arity <= maxArity; arity++ {
				inlineDefs[funcName] = append(inlineDefs[funcName], inlineDef{
					line:       defLine,
					arity:      arity,
					kind:       kind,
					params:     parser.JoinParams(paramNames, arity),
					delegateTo: delegateTo,
					delegateAs: delegateAs,
				})
			}
			i = end - 1

		case parser.TokDefstruct, parser.TokDefexception:
			addStructInlineDefs(inlineDefs, tok, source)

		case parser.TokAttr:
			if parser.TokenText(source, tok) == "@derive" {
				for _, p := range parser.DeriveProtocols(source, tokens, n, i+1) {
					derive := resolveAlias(p)
					// Inside a block of the quote (an if, a case, a fn), or
					// in an inline `if ..., do:`: the caller's options decide.
					if depth > 1 || inBranchStatement(source, tokens, i) {
						derive = conditionalDerivePrefix + derive
					}
					derives = append(derives, derive)
				}
			}

		case parser.TokIdent:
			if parser.TokenText(source, tok) != "unquote" {
				continue
			}
			j := tokNextSig(tokens, n, i+1)
			if j >= n || tokens[j].Kind != parser.TokOpenParen {
				continue
			}
			j = tokNextSig(tokens, n, j+1)
			if j >= n || tokens[j].Kind != parser.TokIdent {
				continue
			}
			nestedName := parser.TokenText(source, tokens[j])
			k := tokNextSig(tokens, n, j+1)
			if k >= n || tokens[k].Kind != parser.TokOpenParen {
				continue
			}
			nestedImports, nestedDefs, nestedUses, nestedCalls, nestedBindings, nestedAliases, nestedDerives, nestedOptUses := parseHelperQuoteBlockDetailed(hs, nestedName, fileAliases, visited)
			optUses = append(optUses, nestedOptUses...)
			imported = append(imported, nestedImports...)
			derives = append(derives, nestedDerives...)
			for name, defs := range nestedDefs {
				inlineDefs[name] = append(inlineDefs[name], defs...)
			}
			transUses = append(transUses, nestedUses...)
			transUseCalls = append(transUseCalls, nestedCalls...)
			optBindings = append(optBindings, nestedBindings...)
			for short, full := range nestedAliases {
				if aliases == nil {
					aliases = make(map[string]string)
				}
				aliases[short] = full
			}
		}
	}
	return
}

// ExtractUses returns module names from all `use Module` declarations.
func ExtractUses(text string) []string {
	source := []byte(text)
	return extractUsesFromTokens(source, parser.Tokenize(source))
}

func extractUsesFromTokens(source []byte, tokens []parser.Token) []string {
	n := len(tokens)
	var uses []string
	for i := 0; i < n; i++ {
		if tokens[i].Kind != parser.TokUse {
			continue
		}
		j := tokNextSig(tokens, n, i+1)
		mod, _ := tokCollectModuleName(source, tokens, n, j)
		if mod != "" {
			uses = append(uses, mod)
		}
	}
	return uses
}

// UseCall holds a `use Module` declaration with its keyword opts.
type UseCall struct {
	Module     string            // the module being used (alias-resolved)
	Opts       map[string]string // keyword args: opt_key → module name (alias-resolved)
	line       int               // 1-based source line; zero for synthesized transitive calls
	local      bool              // nested in an inline do: expression; cannot affect later lines
	token      int               // TokUse position; zero for synthesized transitive calls
	moduleExpr string            // source spelling, retained for scope-aware matching

	// Which is the literal atom passed as the second argument, e.g. "controller"
	// for `use MyAppWeb, :controller`. WhichKey is the first keyword key, e.g.
	// "live_view" for `use MyAppWeb, live_view: :no_sentry_context`. Both feed
	// atom-dispatch __using__ resolution and are empty otherwise.
	Which    string
	WhichKey string

	// forwardOpts is set for a use in a __using__ quote that passes the
	// body's options on (`use Mod, unquote(opts)`), with the edits that
	// those options had there.
	forwardOpts  bool
	forwardEdits []optEdit
	// callerWins is set when the options passed on win over the literal
	// ones of the use: `unquote(opts) ++ [k: X]` (the first value of a key
	// wins) and `Keyword.merge([k: X], unquote(opts))` (the second does).
	callerWins bool
}

// dispatchAtom is the name an atom-dispatch __using__ dispatches on, preferring
// the bare atom form. Empty when the `use` passes no literal atom, which keeps
// dispatch resolution off unless the target is named explicitly.
func (u UseCall) dispatchAtom() string {
	if u.Which != "" {
		return u.Which
	}
	if u.WhichKey != "" {
		return keywordDispatch + u.WhichKey
	}
	return ""
}

// keywordDispatch marks a dispatch atom that is the first key of a keyword
// list (`use MyAppWeb, live_view: ...`), not a literal atom (`use Mod, :x`):
// an atom-dispatch table takes both, but only a literal atom selects the
// atom clause of a __using__ (usingCacheEntry.atomBody).
const keywordDispatch = "\x01"

// ExtractUsesWithOpts parses all `use Module` and `use Module, key: Val`
// declarations, returning each as a UseCall. Aliases are resolved using the
// provided map. Handles opts spanning multiple lines via the tokenizer.
func ExtractUsesWithOpts(text string, aliases map[string]string) []UseCall {
	source := []byte(text)
	return extractUsesWithOptsFromTokens(source, parser.Tokenize(source), aliases)
}

// ExtractModuleUsesWithOpts is ExtractUsesWithOpts without the uses in a
// function body. A `use` in a __using__ quote (or in any other def) is code
// for the caller, not a use by the module whose file holds it, so a question
// about what a module itself uses must not see it.
func ExtractModuleUsesWithOpts(text string, aliases map[string]string) []UseCall {
	source := []byte(text)
	tokens := parser.Tokenize(source)
	calls := extractUsesWithOptsFromTokens(source, tokens, aliases)
	if len(calls) == 0 {
		return calls
	}
	// A use before the first definition is at module level; the usual file
	// has only those and needs no scan of its function bodies.
	firstDef := len(tokens)
	for i, t := range tokens {
		if t.Kind == parser.TokDef || t.Kind == parser.TokDefp || t.Kind == parser.TokDefmacro || t.Kind == parser.TokDefmacrop {
			firstDef = i
			break
		}
	}
	if calls[len(calls)-1].token < firstDef {
		return calls
	}
	bodies := functionBodySpans(tokens)
	if len(bodies) == 0 {
		return calls
	}
	kept := calls[:0]
	for _, c := range calls {
		inside := false
		for _, b := range bodies {
			if c.token > b[0] && c.token < b[1] {
				inside = true
				break
			}
		}
		if !inside {
			kept = append(kept, c)
		}
	}
	return kept
}

// usesVisibleAt keeps the use calls whose functions code at the 1-based line
// can call: a use at module level, and a use in the function body that holds
// the line. A use in a body (the quote of __using__ or of a helper) is code
// for the module that calls the macro, and reaches only that quote.
func usesVisibleAt(tokens []parser.Token, calls []UseCall, line int) []UseCall {
	if len(calls) == 0 {
		return calls
	}
	// The usual file has its uses before its first definition: nothing to do.
	firstDef := len(tokens)
	for i, t := range tokens {
		if t.Kind == parser.TokDef || t.Kind == parser.TokDefp || t.Kind == parser.TokDefmacro || t.Kind == parser.TokDefmacrop {
			firstDef = i
			break
		}
	}
	if calls[len(calls)-1].token < firstDef {
		return calls
	}
	bodies := functionBodySpans(tokens)
	lineOf := func(i int) int {
		if i >= len(tokens) {
			i = len(tokens) - 1
		}
		return tokens[i].Line
	}
	kept := make([]UseCall, 0, len(calls))
	for _, c := range calls {
		visible := true
		for _, b := range bodies {
			if c.token > b[0] && c.token < b[1] {
				visible = line >= lineOf(b[0]) && line <= lineOf(b[1])
				break
			}
		}
		if c.token == 0 || visible {
			kept = append(kept, c)
		}
	}
	return kept
}

// functionBodySpans returns the token spans [def, end] of the bodies of
// def, defp, defmacro, and defmacrop, outermost only.
func functionBodySpans(tokens []parser.Token) [][2]int {
	var spans [][2]int
	for i := 0; i < len(tokens); i++ {
		switch tokens[i].Kind {
		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop:
			end := blockEnd(tokens, i)
			spans = append(spans, [2]int{i, end})
			if end > i {
				i = end - 1
			}
		}
	}
	return spans
}

func extractUsesWithOptsFromTokens(source []byte, tokens []parser.Token, aliases map[string]string) []UseCall {
	n := len(tokens)
	var calls []UseCall
	isBlockKey := func(colon int) bool {
		if colon <= 0 || tokens[colon].Kind != parser.TokColon {
			return false
		}
		switch parser.TokenText(source, tokens[colon-1]) {
		case "do", "else", "rescue", "catch", "after":
			return true
		}
		return false
	}

	for i := 0; i < n; i++ {
		if tokens[i].Kind != parser.TokUse {
			continue
		}
		j := useArgStart(tokens, n, i)
		modName, k := tokCollectModuleName(source, tokens, n, j)
		if modName == "" {
			continue
		}
		module := parser.ResolveModuleRef(modName, aliases, "")
		local := false
		prev := i - 1
		for prev >= 0 && (tokens[prev].Kind == parser.TokComment || tokens[prev].Kind == parser.TokEOL) {
			prev--
		}
		if isBlockKey(prev) {
			local = true
		}
		for p := i - 1; p >= 0 && tokens[p].Line == tokens[i].Line; p-- {
			if parser.TokenText(source, tokens[p]) == ";" {
				break
			}
			if isBlockKey(p) {
				local = true
				break
			}
		}

		nk := tokNextSig(tokens, n, k)
		if nk < n && tokens[nk].Kind == parser.TokComma {
			opts := useCallOpts(source, tokens, n, nk+1, optScope{aliases: aliases})
			which, whichKey := tokCollectDispatchAtom(source, tokens, n, nk+1)
			calls = append(calls, UseCall{Module: module, Opts: opts, Which: which, WhichKey: whichKey, line: tokens[i].Line, local: local, token: i, moduleExpr: modName})
		} else {
			calls = append(calls, UseCall{Module: module, line: tokens[i].Line, local: local, token: i, moduleExpr: modName})
		}
		i = k
	}
	return calls
}

// useCallOpts are the options of a use whose second argument starts at
// tokens[pos]: a keyword list, or the options of a dispatch tuple
// (`use MyAppWeb, {:worker, key: Mod}`).
func useCallOpts(source []byte, tokens []parser.Token, n, pos int, sc optScope) map[string]string {
	if i := tokNextSig(tokens, n, pos); i+2 < n && tokens[i].Kind == parser.TokOpenBrace && tokens[i+1].Kind == parser.TokAtom && tokens[i+2].Kind == parser.TokComma {
		if closing := matchingBracket(tokens, n, i); closing > 0 {
			return collectKeywordOpts(source, tokens, closing, i+3, sc)
		}
	}
	return collectKeywordOpts(source, tokens, n, pos, sc)
}

// useArgStart is the first argument of the use at tokens[use]: after
// `use ` or the parenthesis of `use(`.
func useArgStart(tokens []parser.Token, n, use int) int {
	j := tokNextSig(tokens, n, use+1)
	if j < n && tokens[j].Kind == parser.TokOpenParen && tokens[j].Start == tokens[use].End {
		return tokNextSig(tokens, n, j+1)
	}
	return j
}

// tokCollectDispatchAtom reads the second argument of a `use` call for the two
// shapes an atom-dispatch __using__ accepts:
//
//	use MyAppWeb, :controller                     → which "controller"
//	use MyAppWeb, live_view: :no_sentry_context   → whichKey "live_view"
//
// Only a literal atom (or literal keyword key) is reported. A variable or a
// computed value yields empty strings, so the caller resolves nothing rather
// than guessing.
func tokCollectDispatchAtom(source []byte, tokens []parser.Token, n, pos int) (which, whichKey string) {
	i := tokNextSig(tokens, n, pos)
	if i >= n {
		return "", ""
	}
	switch tokens[i].Kind {
	case parser.TokAtom:
		return dispatchAtomName(parser.TokenText(source, tokens[i])), ""
	case parser.TokOpenBrace:
		j := tokNextSig(tokens, n, i+1)
		if j < n && tokens[j].Kind == parser.TokAtom {
			return dispatchAtomName(parser.TokenText(source, tokens[j])), ""
		}
	case parser.TokIdent:
		if i+1 < n && tokens[i+1].Kind == parser.TokColon {
			return "", parser.TokenText(source, tokens[i])
		}
	}
	return "", ""
}

func dispatchAtomName(text string) string {
	name := strings.TrimPrefix(text, ":")
	if len(name) >= 2 && name[0] == '"' && name[len(name)-1] == '"' {
		if unquoted, err := strconv.Unquote(name); err == nil {
			return unquoted
		}
	}
	return name
}

// atomKeyName is the name of the atom at tokens[i] (`:key` and `:"key"` give
// "key"), or "" when tokens[i] is not an atom.
func atomKeyName(source []byte, tokens []parser.Token, i int) string {
	if tokens[i].Kind != parser.TokAtom {
		return ""
	}
	t := parser.TokenText(source, tokens[i])
	if len(t) < 2 {
		return ""
	}
	return dispatchAtomName(t)
}

// keywordKeyAt reads a keyword key at tokens[i]: `key:` or the quoted
// `"key":`. It returns the key and the token after the colon.
func keywordKeyAt(source []byte, tokens []parser.Token, n, i int) (string, int, bool) {
	if i+1 >= n || tokens[i+1].Kind != parser.TokColon {
		return "", i, false
	}
	switch tokens[i].Kind {
	case parser.TokIdent:
		return parser.TokenText(source, tokens[i]), i + 2, true
	case parser.TokString:
		if tokens[i+1].Start != tokens[i].End {
			return "", i, false
		}
		if key, err := strconv.Unquote(parser.TokenText(source, tokens[i])); err == nil {
			return key, i + 2, true
		}
	}
	return "", i, false
}

// atomModuleName is the module that an atom such as `:"Elixir.MyApp.Mod"`
// names, or "".
func atomModuleName(source []byte, tokens []parser.Token, i int) string {
	if name := atomKeyName(source, tokens, i); strings.HasPrefix(name, "Elixir.") && len(name) > len("Elixir.") && name[len("Elixir.")] >= 'A' && name[len("Elixir.")] <= 'Z' {
		return name[len("Elixir."):]
	}
	return ""
}

// collectKeywordOpts scans tokens starting at pos for keyword pairs like
// `key: ModuleName` and `{:key, ModuleName}`, and returns a map of key → the
// module (see keywordValue; unknownOpt for a value that is not one). The
// values are read in sc.
func collectKeywordOpts(source []byte, tokens []parser.Token, n, pos int, sc optScope) map[string]string {
	result := make(map[string]string)
	i := tokNextSig(tokens, n, pos)
	for i < n {
		tok := tokens[i]
		// A tuple: `{:key, Mod}`, the tuple form of a pair, is an option.
		// Nothing inside a tuple is one (`{:key, [base: X]}` holds a value).
		if tok.Kind == parser.TokOpenBrace {
			if i+4 < n && tokens[i+1].Kind == parser.TokAtom && tokens[i+2].Kind == parser.TokComma {
				if key := atomKeyName(source, tokens, i+1); key != "" {
					if _, dup := result[key]; !dup {
						result[key] = keywordValue(source, tokens, n, tokNextSig(tokens, n, i+3), sc)
					}
				}
			}
			if c := matchingBracket(tokens, n, i); c > i {
				i = c + 1
				continue
			}
		}
		// Stop at EOL not followed by a continuation (keyword opt)
		if tok.Kind == parser.TokEOL {
			j := tokNextSig(tokens, n, i+1)
			if j >= n || tokens[j].Kind == parser.TokEOL || tokens[j].Kind == parser.TokEOF {
				break
			}
			// Check if next sig token is a keyword key (keyword opt)
			if _, _, ok := keywordKeyAt(source, tokens, n, j); ok {
				i = j
				continue
			}
			break
		}
		if tok.Kind == parser.TokEOF {
			break
		}
		// `[{:key, Mod} | rest]` is, as the argument of a macro, a list of
		// one `|` expression: it holds no options.
		if isConsBar(source, tokens, n, i) {
			return map[string]string{}
		}
		// Match: key colon Module
		if key, after, ok := keywordKeyAt(source, tokens, n, i); ok {
			k := tokNextSig(tokens, n, after)
			// A module, or a value that Dexter cannot read as one (a call,
			// a variable, `Mod.pick()`), which no default replaces. The
			// first value of a key wins, as in Keyword.get.
			if _, dup := result[key]; !dup {
				result[key] = keywordValue(source, tokens, n, k, sc)
			}
			// A bracketed value (`extra: [base: :x]`) is skipped whole:
			// its keys are not the options'.
			if k < n {
				switch tokens[k].Kind {
				case parser.TokOpenBracket, parser.TokOpenBrace, parser.TokOpenParen:
					if close := matchingBracket(tokens, n, k); close > 0 {
						i = close + 1
						continue
					}
				}
			}
		}
		i++
	}
	return result
}

// keywordValue reads the value of a keyword pair at tokens[k]: the module
// that a module name (aliases resolved) or an `:"Elixir.Mod"` atom names, or
// unknownOpt for any other value (a call, a variable, `Mod.pick()`). The
// value ends at a comma, an end of line, a closing bracket, or the end of
// the input.
func keywordValue(source []byte, tokens []parser.Token, n, k int, sc optScope) string {
	if k >= n {
		return unknownOpt
	}
	mod, end := "", k+1
	switch tokens[k].Kind {
	case parser.TokModule:
		var name string
		name, end = tokCollectModuleName(source, tokens, n, k)
		if name == "" {
			return unknownOpt
		}
		if self, isSelf := selfModuleRef(name, sc.self); isSelf {
			// In a quote, __MODULE__ is the caller's module.
			if sc.inQuote {
				return unknownOpt
			}
			mod = self
		} else {
			mod = parser.ResolveModuleRef(name, sc.aliases, "")
		}
		if mod == unknownOpt {
			return unknownOpt
		}
	case parser.TokAtom:
		if mod = atomModuleName(source, tokens, k); mod == "" {
			return unknownOpt
		}
	case parser.TokAttr:
		// A module attribute in macro code: its value. A macro gets one in
		// its arguments (`use Mod, key: @attr`) or in a quote as code, which
		// `use` does not take as a module.
		if !sc.macroCode || sc.inQuote {
			return unknownOpt
		}
		if mod = attrModule(source, tokens, k, sc.aliases); mod == "" {
			return unknownOpt
		}
	case parser.TokIdent:
		// A variable bound to a module: `dflt` in macro code, and
		// `unquote(dflt)` in a quote.
		name := parser.TokenText(source, tokens[k])
		if sc.inQuote {
			if name != "unquote" || k+3 >= n || tokens[k+1].Kind != parser.TokOpenParen || tokens[k+2].Kind != parser.TokIdent || tokens[k+3].Kind != parser.TokCloseParen {
				return unknownOpt
			}
			name, end = parser.TokenText(source, tokens[k+2]), k+4
		}
		if mod = sc.constant(name); mod == "" {
			return unknownOpt
		}
	default:
		return unknownOpt
	}
	end = skipComments(tokens, n, end)
	if end >= n {
		return mod
	}
	switch tokens[end].Kind {
	case parser.TokComma, parser.TokEOL, parser.TokEOF, parser.TokCloseBracket, parser.TokCloseParen, parser.TokCloseBrace:
		return mod
	case parser.TokDo:
		// `use Mod, base: X do`: not a value that goes on.
		return mod
	}
	return unknownOpt
}

// isConsBar reports whether tokens[i] is the `|` of a list cons (`[h | t]`),
// not a part of `||` or `|>`.
func isConsBar(source []byte, tokens []parser.Token, n, i int) bool {
	if tokens[i].Kind != parser.TokOther || parser.TokenText(source, tokens[i]) != "|" {
		return false
	}
	isBar := func(j int) bool {
		return j >= 0 && j < n && tokens[j].Kind == parser.TokOther && parser.TokenText(source, tokens[j]) == "|"
	}
	return (!isBar(i+1) || tokens[i+1].Start != tokens[i].End) && (!isBar(i-1) || tokens[i-1].End != tokens[i].Start)
}

// attrModule is the module that the module attribute at tokens[at] holds:
// the last `@name Mod` before it, at the start of a line. It is "" when that
// is not a module alone, or when there is none.
func attrModule(source []byte, tokens []parser.Token, at int, aliases map[string]string) string {
	name := parser.TokenText(source, tokens[at])
	for j := at - 1; j >= 0; j-- {
		if tokens[j].Kind != parser.TokAttr || parser.TokenText(source, tokens[j]) != name ||
			(j > 0 && tokens[j-1].Kind != parser.TokEOL && tokens[j-1].Kind != parser.TokComment) {
			continue
		}
		n := len(tokens)
		if j+1 >= n || tokens[j+1].Kind != parser.TokModule {
			return ""
		}
		mod, end := tokCollectModuleName(source, tokens, n, j+1)
		if e := skipComments(tokens, n, end); mod == "" || (e < n && tokens[e].Kind != parser.TokEOL && tokens[e].Kind != parser.TokEOF) {
			return ""
		}
		return parser.ResolveModuleRef(mod, aliases, "")
	}
	return ""
}

// skipComments is the first token from tokens[from] that is not a comment.
func skipComments(tokens []parser.Token, n, from int) int {
	for from < n && tokens[from].Kind == parser.TokComment {
		from++
	}
	return from
}

// matchingBracket returns the index of the bracket that closes
// tokens[open], or -1.
func matchingBracket(tokens []parser.Token, n, open int) int {
	depth := 0
	for j := open; j < n; j++ {
		switch tokens[j].Kind {
		case parser.TokOpenBracket, parser.TokOpenBrace, parser.TokOpenParen:
			depth++
		case parser.TokCloseBracket, parser.TokCloseBrace, parser.TokCloseParen:
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// optVars are the options variables of a __using__ body or a helper: the
// edits of each one, in source order (see optEdit). A variable that no
// statement assigns has none.
type optVars map[string][]optEdit

// derive is the edit list of a variable assigned from src with edits.
func (v optVars) derive(src string, edits []optEdit) []optEdit {
	return append(slices.Clone(v[src]), edits...)
}

// forwardedOpts reads the options argument of a use at tokens[pos]: when
// it passes the body's options on, the edits they have there and true. The
// options can be all of it (`unquote(opts)`, `unquote(Keyword.put_new(opts,
// :k, X))`) or mixed with literal ones (`[k: X] ++ unquote(opts)`,
// `[{:k, X}, unquote_splicing(opts)]`, `Keyword.merge(...)`); callerWins is
// then whether they win over the literal ones.
func forwardedOpts(source []byte, tokens []parser.Token, n, pos int, vars optVars, sc optScope) (edits []optEdit, ok, callerWins bool) {
	start := tokNextSig(tokens, n, pos)
	if start >= n {
		return nil, false, false
	}
	merge := start+3 < n && tokens[start].Kind == parser.TokModule && parser.TokenText(source, tokens[start]) == "Keyword" &&
		tokens[start+1].Kind == parser.TokDot && parser.TokenText(source, tokens[start+2]) == "merge" && tokens[start+3].Kind == parser.TokOpenParen
	end := skipToEndOfStatement(tokens, n, start)
	// The unquote at the top level of the argument, of its list, or of the
	// merge call.
	depth, arg := 0, 0
	var openers []parser.TokenKind
	for i := start; i < end && i < n; i++ {
		switch tokens[i].Kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
			depth++
			openers = append(openers, tokens[i].Kind)
			continue
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace:
			depth--
			if len(openers) > 0 {
				openers = openers[:len(openers)-1]
			}
			continue
		case parser.TokComma:
			if merge && depth == 1 {
				arg++
			}
			continue
		}
		if depth > 1 || tokens[i].Kind != parser.TokIdent || i+1 >= n || tokens[i+1].Kind != parser.TokOpenParen {
			continue
		}
		if name := parser.TokenText(source, tokens[i]); name != "unquote" && name != "unquote_splicing" {
			continue
		}
		if (len(openers) > 0 && openers[len(openers)-1] == parser.TokOpenBrace) || !optionsPosition(source, tokens, start, i) {
			// A value, such as `adapter: unquote(adapter)`, not the options.
			continue
		}
		e, found := unquotedOpts(source, tokens, n, i+2, vars, sc.macro())
		if !found {
			return nil, false, false
		}
		if merge {
			return e, true, arg == 1
		}
		// The first value of a key wins: the options win when they come
		// first (`unquote(opts) ++ [k: X]`, `[unquote_splicing(opts), k: X]`)
		// and something follows.
		closing := matchingBracket(tokens, n, i+1)
		first := i == start || (tokens[start].Kind == parser.TokOpenBracket && tokNextSig(tokens, n, start+1) == i)
		if !first || closing < 0 {
			return e, true, false
		}
		next := tokNextSig(tokens, n, closing+1)
		return e, true, next < end && next < n && tokens[next].Kind != parser.TokEOL && tokens[next].Kind != parser.TokEOF && tokens[next].Kind != parser.TokCloseBracket &&
			tokens[next].Kind != parser.TokCloseParen
	}
	return nil, false, false
}

// optionsPosition reports whether the unquote at tokens[i] stands for a whole
// options list in the argument that starts at tokens[start]: the argument
// itself, an operand of `++`, a list element, or an argument of
// Keyword.merge. A keyword value (`k: unquote(v)`) is not one,
// and the caller checks that it is not in a tuple.
func optionsPosition(source []byte, tokens []parser.Token, start, i int) bool {
	if i == start {
		return true
	}
	prev := i - 1
	for prev > start && (tokens[prev].Kind == parser.TokEOL || tokens[prev].Kind == parser.TokComment) {
		prev--
	}
	switch tokens[prev].Kind {
	case parser.TokOpenBracket, parser.TokOpenParen, parser.TokComma:
		return true
	case parser.TokOther:
		// The tail of `[{:k, X} | unquote(opts)]` is not: a cons in a quote
		// is a `|` expression, which holds no options.
		t := parser.TokenText(source, tokens[prev])
		return t == "+" || t == "++"
	}
	return false
}

// unquotedOpts reads the inside of an unquote at tokens[inner]: a variable
// or an edit of one.
func unquotedOpts(source []byte, tokens []parser.Token, n, inner int, vars optVars, sc optScope) ([]optEdit, bool) {
	inner = tokNextSig(tokens, n, inner)
	if inner+1 < n && tokens[inner].Kind == parser.TokIdent && tokens[inner+1].Kind == parser.TokCloseParen {
		return slices.Clone(vars[parser.TokenText(source, tokens[inner])]), true
	}
	if src, edits, _, _, ok := readKeywordEdits(source, tokens, n, inner, sc); ok {
		return vars.derive(src, edits), true
	}
	return nil, false
}

// readKeywordEdits reads an expression that edits an options variable, at
// tokens[at]: `Keyword.put(opts, :key, Mod)` (also put_new, delete, drop,
// take, and merge with a literal), the same steps piped from the variable
// (`opts |> Keyword.put_new(:key, Mod) |> ...`), or the variable alone. It
// returns the variable, the edits, the modules the edits put (edges of the
// use-chain graph), and the token after the expression.
func readKeywordEdits(source []byte, tokens []parser.Token, n, at int, sc optScope) (src string, edits []optEdit, mods []string, end int, ok bool) {
	i := tokNextSig(tokens, n, at)
	piped := false
	if src, edits, end, ok := concatEdits(source, tokens, n, i, sc); ok {
		for _, e := range edits {
			if e.mod != unknownOpt {
				mods = append(mods, e.mod)
			}
		}
		return src, edits, mods, end, true
	}
	if i < n && tokens[i].Kind == parser.TokIdent {
		src, end = parser.TokenText(source, tokens[i]), i+1
		next := nextPipe(tokens, n, end)
		if next < 0 {
			// The variable alone: it ends the statement.
			if e := tokNextSig(tokens, n, end); e >= n || tokens[e].Kind == parser.TokEOL || tokens[e].Kind == parser.TokEOF || tokens[e].Kind == parser.TokCloseParen {
				return src, nil, nil, end, true
			}
			return "", nil, nil, at, false
		}
		i, piped = next, true
	}
	for {
		step, stepEnd, stepOK := keywordStep(source, tokens, n, i, piped, sc)
		if !stepOK {
			return "", nil, nil, at, false
		}
		if !piped {
			if step.src == "" {
				return "", nil, nil, at, false
			}
			src = step.src
		}
		edits = append(edits, step.edits...)
		for _, e := range step.edits {
			// Keyword.update puts its default when the key is not set.
			if (e.op == "put" || e.op == "put_new" || e.op == "update") && e.mod != unknownOpt {
				mods = append(mods, e.mod)
			}
		}
		end = stepEnd
		next := nextPipe(tokens, n, end)
		if next < 0 {
			return src, edits, mods, end, true
		}
		i, piped = next, true
	}
}

// concatEdits reads `opts ++ [key: Mod]` and `[key: Mod] ++ opts` at
// tokens[i], up to the end of the statement. The first value of a key wins in
// a read, so a key after the options is put_new, and one before them is put.
func concatEdits(source []byte, tokens []parser.Token, n, i int, sc optScope) (src string, edits []optEdit, end int, ok bool) {
	// isConcat reports whether `++` (two tokens) is at tokens[j].
	isConcat := func(j int) bool {
		return j+1 < n && tokens[j].Kind == parser.TokOther && tokens[j+1].Kind == parser.TokOther &&
			parser.TokenText(source, tokens[j]) == "+" && parser.TokenText(source, tokens[j+1]) == "+" && tokens[j+1].Start == tokens[j].End
	}
	stmtEnd := func(j int) bool {
		j = skipComments(tokens, n, j)
		return j >= n || tokens[j].Kind == parser.TokEOL || tokens[j].Kind == parser.TokEOF || tokens[j].Kind == parser.TokCloseParen
	}
	list := func(open int) (map[string]string, int, bool) {
		if open >= n || tokens[open].Kind != parser.TokOpenBracket {
			return nil, 0, false
		}
		closing := matchingBracket(tokens, n, open)
		if closing < 0 {
			return nil, 0, false
		}
		return collectKeywordOpts(source, tokens, closing+1, open+1, sc), closing + 1, true
	}
	op := "put_new"
	var opts map[string]string
	if i < n && tokens[i].Kind == parser.TokIdent {
		c := tokNextSig(tokens, n, i+1)
		if !isConcat(c) {
			return "", nil, 0, false
		}
		var listOK bool
		opts, end, listOK = list(tokNextSig(tokens, n, c+2))
		if !listOK || !stmtEnd(end) {
			return "", nil, 0, false
		}
		src = parser.TokenText(source, tokens[i])
	} else {
		var listOK bool
		opts, end, listOK = list(i)
		c := tokNextSig(tokens, n, end)
		if !listOK || !isConcat(c) {
			return "", nil, 0, false
		}
		v := tokNextSig(tokens, n, c+2)
		if v >= n || tokens[v].Kind != parser.TokIdent || !stmtEnd(v+1) {
			return "", nil, 0, false
		}
		src, end, op = parser.TokenText(source, tokens[v]), v+1, "put"
	}
	for key, mod := range sortedOpts(opts) {
		edits = append(edits, optEdit{op: op, key: key, mod: mod})
	}
	return src, edits, end, true
}

// nextPipe returns the token after a `|>` that follows tokens[from] (also on
// the next line), or -1.
func nextPipe(tokens []parser.Token, n, from int) int {
	j := tokNextSig(tokens, n, from)
	if j < n && tokens[j].Kind == parser.TokPipe {
		return tokNextSig(tokens, n, j+1)
	}
	return -1
}

type keywordStepResult struct {
	src   string // the variable of the first argument; empty in a pipe
	edits []optEdit
}

// keywordStep reads one `Keyword.op(args)` at tokens[i]. In a pipe the
// options are the piped value, so the arguments start one later.
func keywordStep(source []byte, tokens []parser.Token, n, i int, piped bool, sc optScope) (keywordStepResult, int, bool) {
	var r keywordStepResult
	if i+3 >= n || tokens[i].Kind != parser.TokModule || parser.TokenText(source, tokens[i]) != "Keyword" ||
		tokens[i+1].Kind != parser.TokDot || tokens[i+2].Kind != parser.TokIdent || tokens[i+3].Kind != parser.TokOpenParen {
		return r, i, false
	}
	op := parser.TokenText(source, tokens[i+2])
	args, end := splitCallArgs(tokens, n, i+3)
	if end < 0 {
		return r, i, false
	}
	if !piped {
		if len(args) == 0 {
			return r, i, false
		}
		first := args[0]
		single := func(a [2]int) bool { return tokens[a[0]].Kind == parser.TokIdent && a[1] == a[0]+1 }
		// Keyword.merge(defaults, opts), where defaults is a variable bound
		// to a keyword literal: the options are the second argument.
		_, firstIsLiteral := sc.literal(source, tokens, first)
		mergeIntoLiteral := op == "merge" && len(args) == 2 && single(args[1]) && (!single(first) || firstIsLiteral)
		if single(first) && !mergeIntoLiteral {
			r.src = parser.TokenText(source, tokens[first[0]])
			args = args[1:]
		} else if mergeIntoLiteral {
			// Keyword.merge([key: Mod], opts): the options win.
			r.src = parser.TokenText(source, tokens[args[1][0]])
			if lit, ok := sc.literal(source, tokens, first); ok {
				// defaults = [key: Mod]; Keyword.merge(defaults, opts)
				for key, mod := range sortedOpts(lit) {
					r.edits = append(r.edits, optEdit{op: "put_new", key: key, mod: mod})
				}
				return r, end, true
			}
			if !keywordLiteral(source, tokens, n, first) {
				// Keyword.merge(defaults, opts): a key that the options do
				// not hold can come from defaults.
				r.edits = []optEdit{{op: "unknown_new"}}
				return r, end, true
			}
			for key, mod := range sortedOpts(collectKeywordOpts(source, tokens, first[1], listStart(tokens, n, first[0]), sc)) {
				r.edits = append(r.edits, optEdit{op: "put_new", key: key, mod: mod})
			}
			return r, end, true
		} else {
			return r, i, false
		}
	}
	atomKey := func(a [2]int) string {
		if a[1] == a[0]+1 {
			return atomKeyName(source, tokens, a[0])
		}
		return ""
	}
	value := func(a [2]int) string {
		if a[1] == a[0]+1 {
			if mod := atomModuleName(source, tokens, a[0]); mod != "" {
				return mod
			}
		}
		return keywordValue(source, tokens, a[1], a[0], sc)
	}
	atomList := func(a [2]int) ([]string, bool) {
		if tokens[a[0]].Kind != parser.TokOpenBracket {
			return nil, false
		}
		var keys []string
		for j := a[0] + 1; j < a[1]-1; j++ {
			switch tokens[j].Kind {
			case parser.TokAtom:
				keys = append(keys, atomKeyName(source, tokens, j))
			case parser.TokComma, parser.TokEOL, parser.TokComment:
			default:
				return nil, false
			}
		}
		return keys, true
	}
	switch op {
	case "put", "put_new":
		if len(args) != 2 || atomKey(args[0]) == "" {
			return r, i, false
		}
		r.edits = []optEdit{{op: op, key: atomKey(args[0]), mod: value(args[1])}}
	case "put_new_lazy":
		// The function gives the value: Dexter cannot read it.
		if len(args) != 2 || atomKey(args[0]) == "" {
			return r, i, false
		}
		r.edits = []optEdit{{op: "put_new", key: atomKey(args[0]), mod: unknownOpt}}
	case "update":
		if len(args) != 3 || atomKey(args[0]) == "" {
			return r, i, false
		}
		r.edits = []optEdit{{op: "update", key: atomKey(args[0]), mod: value(args[1])}}
	case "validate!":
		// Keyword.validate!(opts, [:key, key: Default]) gives the options,
		// with the defaults for the keys that they do not hold.
		if len(args) != 1 || !keywordLiteral(source, tokens, n, args[0]) || tokens[args[0][0]].Kind != parser.TokOpenBracket {
			return r, i, false
		}
		for key, mod := range sortedOpts(collectKeywordOpts(source, tokens, args[0][1], args[0][0]+1, sc)) {
			r.edits = append(r.edits, optEdit{op: "put_new", key: key, mod: mod})
		}
	case "update!":
		// The function gives the new value (and with no value, it raises).
		if len(args) != 2 || atomKey(args[0]) == "" {
			return r, i, false
		}
		r.edits = []optEdit{{op: "put", key: atomKey(args[0]), mod: unknownOpt}}
	case "delete":
		if len(args) != 1 || atomKey(args[0]) == "" {
			return r, i, false
		}
		r.edits = []optEdit{{op: "delete", key: atomKey(args[0])}}
	case "drop":
		keys, ok := atomList(args[0])
		if len(args) != 1 || !ok {
			return r, i, false
		}
		for _, k := range keys {
			r.edits = append(r.edits, optEdit{op: "delete", key: k})
		}
	case "take":
		keys, ok := atomList(args[0])
		if len(args) != 1 || !ok {
			return r, i, false
		}
		r.edits = []optEdit{{op: "take", keys: keys}}
	case "merge":
		// Keyword.merge(opts, key: Mod): the keys win. A bare keyword list
		// is all the arguments after the options.
		if len(args) == 0 {
			return r, i, false
		}
		if _, _, bare := keywordKeyAt(source, tokens, n, args[0][0]); bare {
			args = [][2]int{{args[0][0], args[len(args)-1][1]}}
		}
		if len(args) != 1 {
			return r, i, false
		}
		if lit, ok := sc.literal(source, tokens, args[0]); ok {
			// extra = [key: Mod]; Keyword.merge(opts, extra)
			for key, mod := range sortedOpts(lit) {
				r.edits = append(r.edits, optEdit{op: "put", key: key, mod: mod})
			}
			break
		}
		if !keywordLiteral(source, tokens, n, args[0]) {
			// Keyword.merge(opts, extra): every key can come from extra.
			r.edits = []optEdit{{op: "unknown"}}
			break
		}
		for key, mod := range sortedOpts(collectKeywordOpts(source, tokens, args[0][1], listStart(tokens, n, args[0][0]), sc)) {
			r.edits = append(r.edits, optEdit{op: "put", key: key, mod: mod})
		}
	default:
		return r, i, false
	}
	return r, end, true
}

// splitCallArgs returns the token ranges [start, end) of the arguments of
// the call whose open parenthesis is tokens[open], and the token after its
// close parenthesis (-1 when it does not close).
func splitCallArgs(tokens []parser.Token, n, open int) ([][2]int, int) {
	var args [][2]int
	depth := 0
	start := -1
	for j := open + 1; j < n; j++ {
		k := tokens[j].Kind
		if k == parser.TokEOL || k == parser.TokComment {
			continue
		}
		if start < 0 && k != parser.TokCloseParen {
			start = j
		}
		switch k {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace, parser.TokDo, parser.TokFn:
			depth++
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace, parser.TokEnd:
			if depth == 0 {
				if start >= 0 {
					args = append(args, [2]int{start, lastSigBefore(tokens, j) + 1})
				}
				return args, j + 1
			}
			depth--
		case parser.TokComma:
			if depth == 0 {
				args = append(args, [2]int{start, lastSigBefore(tokens, j) + 1})
				start = -1
			}
		}
	}
	return nil, -1
}

func lastSigBefore(tokens []parser.Token, j int) int {
	j--
	for j >= 0 && (tokens[j].Kind == parser.TokEOL || tokens[j].Kind == parser.TokComment) {
		j--
	}
	return j
}

// keywordLiteral reports whether the argument arg is a keyword literal:
// `[key: Mod]`, `[{:key, Mod}]`, or the bare `key: Mod`.
func keywordLiteral(source []byte, tokens []parser.Token, n int, arg [2]int) bool {
	if tokens[arg[0]].Kind == parser.TokOpenBracket {
		return matchingBracket(tokens, n, arg[0]) == arg[1]-1
	}
	_, _, ok := keywordKeyAt(source, tokens, n, arg[0])
	return ok
}

// listStart is the first token of a keyword literal: inside `[...]`, or at
// i for the bare `key: Mod` form.
func listStart(tokens []parser.Token, n, i int) int {
	if i < n && tokens[i].Kind == parser.TokOpenBracket {
		return i + 1
	}
	return i
}

// sortedOpts returns opts in key order, so the edits do not depend on map
// order.
func sortedOpts(opts map[string]string) func(yield func(string, string) bool) {
	return func(yield func(string, string) bool) {
		keys := make([]string, 0, len(opts))
		for k := range opts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !yield(k, opts[k]) {
				return
			}
		}
	}
}

// usingBody is everything a single `quote do` block injects into a consumer
// module. A plain `__using__` has exactly one; an atom-dispatch entrypoint has
// one per dispatch target.
type usingBody struct {
	imports    []string               // modules imported, source order
	inlineDefs map[string][]inlineDef // function name → defs in the quote do block
	transUses  []string               // modules used inside the body (double-use chains)
	// optUses are the modules that option edits put (`Keyword.put_new(opts,
	// :base, Mod)`): edges of the use-chain graph, which lists the modules
	// that can inject a name. A lookup for one caller follows the options
	// instead, so that a caller's own option is not passed over.
	optUses     []string
	transCalls  []UseCall         // transitive uses with dispatch atom and opts preserved
	optBindings []optBinding      // dynamic imports/uses resolved from opts
	aliases     map[string]string // alias short name → full module
	derives     []string          // protocols an @derive in the body derives for the caller
	branchUses  []string          // transUses inside a branch of the body
}

func (b *usingBody) isEmpty() bool {
	return b == nil || (len(b.imports) == 0 && len(b.inlineDefs) == 0 &&
		len(b.transUses) == 0 && len(b.optUses) == 0 && len(b.transCalls) == 0 && len(b.optBindings) == 0 && len(b.aliases) == 0)
}

// graphUses are the edges of the use-chain graph from the body: its uses
// and the modules that its option edits put.
func (b *usingBody) graphUses() []string {
	if len(b.optUses) == 0 {
		return b.transUses
	}
	return append(slices.Clone(b.transUses), b.optUses...)
}

func (b *usingBody) hasTransCall(moduleName string) bool {
	for i := range b.transCalls {
		if b.transCalls[i].Module == moduleName {
			return true
		}
	}
	return false
}

// usingDispatchParam reports the parameter name of an atom-dispatch __using__
// clause, i.e. one whose entire body delegates to a sibling function named by
// the atom the consumer passed:
//
//	defmacro __using__(which) when is_atom(which), do: apply(__MODULE__, which, [])
//	defmacro __using__([{which, opts}]) when is_atom(which), do: apply(__MODULE__, which, [opts])
//
// It returns "" unless apply/3 targets __MODULE__ *and* dispatches on that
// clause's own parameter. A literal function name or another module means the
// call is not atom dispatch and must not be treated as such.
func usingDispatchParam(source []byte, tokens []parser.Token) string {
	n := len(tokens)
	for i := 0; i < n; i++ {
		if tokens[i].Kind != parser.TokDefmacro {
			continue
		}
		name, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
		if !ok || name != "__using__" {
			continue
		}
		param, after := usingClauseParam(source, tokens, n, j+1)
		if param == "" {
			continue
		}
		if applyDispatchesOn(source, tokens, n, after, param) {
			return param
		}
	}
	return ""
}

// usingClauseParam reads the first parameter name of a __using__ clause,
// accepting both `__using__(which)` and `__using__([{which, opts}])`. It
// returns the name and the index to continue scanning from. A wildcard or
// pattern that binds no plain name yields "".
func usingClauseParam(source []byte, tokens []parser.Token, n, from int) (string, int) {
	i := tokNextSig(tokens, n, from)
	if i >= n || tokens[i].Kind != parser.TokOpenParen {
		return "", from
	}
	// Skip list/tuple wrappers of the [{which, opts}] form to reach the first ident.
	i = tokNextSig(tokens, n, i+1)
	for i < n && (tokens[i].Kind == parser.TokOpenBracket || tokens[i].Kind == parser.TokOpenBrace) {
		i = tokNextSig(tokens, n, i+1)
	}
	if i >= n || tokens[i].Kind != parser.TokIdent {
		return "", from
	}
	name := parser.TokenText(source, tokens[i])
	if name == "" || strings.HasPrefix(name, "_") {
		return "", from
	}
	return name, i + 1
}

// applyDispatchesOn reports whether an `apply(__MODULE__, <param>, ...)` call
// appears in the clause body starting at from, before the next __using__ clause.
func applyDispatchesOn(source []byte, tokens []parser.Token, n, from int, param string) bool {
	for i := from; i < n; i++ {
		// Stop at the next clause so one clause's body cannot vouch for another.
		if tokens[i].Kind == parser.TokDefmacro || tokens[i].Kind == parser.TokDef {
			return false
		}
		if tokens[i].Kind != parser.TokIdent || parser.TokenText(source, tokens[i]) != "apply" {
			continue
		}
		j := tokNextSig(tokens, n, i+1)
		if j >= n || tokens[j].Kind != parser.TokOpenParen {
			continue
		}
		j = tokNextSig(tokens, n, j+1)
		if j >= n || tokens[j].Kind != parser.TokModule || parser.TokenText(source, tokens[j]) != "__MODULE__" {
			continue
		}
		j = tokNextSig(tokens, n, j+1)
		if j >= n || tokens[j].Kind != parser.TokComma {
			continue
		}
		j = tokNextSig(tokens, n, j+1)
		if j < n && tokens[j].Kind == parser.TokIdent && parser.TokenText(source, tokens[j]) == param {
			return true
		}
	}
	return false
}

// sourceForModule limits runtime __using__ parsing to one module in files that
// define multiple modules. Leading newlines preserve the original token line
// numbers used for definition locations.
func sourceForModule(text, moduleName string) (string, bool) {
	source := []byte(text)
	tokens := parser.Tokenize(source)
	type frame struct {
		name  string
		depth int
		start int
	}
	var stack []frame
	depth := 0

	for i := 0; i < len(tokens); i++ {
		switch tokens[i].Kind {
		case parser.TokDo, parser.TokFn:
			parser.TrackBlockDepth(tokens[i].Kind, &depth)
		case parser.TokEnd:
			prevDepth := depth
			parser.TrackBlockDepth(tokens[i].Kind, &depth)
			if len(stack) == 0 || stack[len(stack)-1].depth != prevDepth {
				continue
			}
			closed := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if closed.name == moduleName {
				prefix := strings.Repeat("\n", tokens[closed.start].Line-1)
				return prefix + string(source[tokens[closed.start].Start:tokens[i].End]), true
			}
		case parser.TokDefmodule:
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1].name
			}
			name, nextPos, hasDo := tokParseModuleDef(source, tokens, i+1, parent)
			if name == "" || !hasDo {
				continue
			}
			depth++
			stack = append(stack, frame{name: name, depth: depth, start: i})
			i = nextPos - 1
		}
	}
	return "", false
}

// laterNonListUsingClause reports whether a __using__ clause of the same
// module, from token i on, does not take a keyword list. A nested module's
// clauses are not the module's own, and the scan ends with the module.
func laterNonListUsingClause(source []byte, tokens []parser.Token, i int) bool {
	n := len(tokens)
	keyword := func(j int) bool { return j+1 < n && tokens[j+1].Kind == parser.TokColon } // `do:`
	// depth counts the blocks opened from i on; the module's own end takes it
	// below zero. nested is the depth inside a nested defmodule, or -1.
	depth, nested := 0, -1
	for ; i < n; i++ {
		switch tokens[i].Kind {
		case parser.TokDo:
			if !keyword(i) {
				depth++
			}
		case parser.TokFn:
			depth++
		case parser.TokEnd:
			depth--
			if nested >= 0 && depth < nested {
				nested = -1
			}
			if depth < 0 {
				return false // the end of the module
			}
		case parser.TokDefmodule:
			// Only a block form (`defmodule Inner do ... end`) has clauses
			// to skip; `defmodule Tiny, do: ...` has no end.
			if doIdx, _, hasDo := parser.ScanForwardToBlockDo(tokens, n, i+1); nested < 0 && hasDo && !keyword(doIdx) {
				nested = depth + 1
			}
		case parser.TokDefmacro:
			if nested >= 0 {
				continue
			}
			if name, j, ok := parser.StaticDeclarationName(source, tokens, n, i); ok && name == "__using__" && !usingClauseTakesList(source, tokens, n, j+1) {
				return true
			}
		}
	}
	return false
}

// usingClauseTakesList reports whether the __using__ clause whose head
// continues at tokens[i] (after the name) can take a keyword list, the
// argument of `use Mod` and `use Mod, key: value`: not a clause for an atom
// (`(:worker)`, `(which) when is_atom(which)`) or a tuple.
func usingClauseTakesList(source []byte, tokens []parser.Token, n, i int) bool {
	open := tokNextSig(tokens, n, i)
	if open >= n || tokens[open].Kind != parser.TokOpenParen {
		return true
	}
	p := tokNextSig(tokens, n, open+1)
	if p >= n {
		return true
	}
	switch tokens[p].Kind {
	case parser.TokAtom, parser.TokOpenBrace, parser.TokString:
		return false
	case parser.TokIdent:
		closing := tokNextSig(tokens, n, p+1)
		if closing >= n || tokens[closing].Kind != parser.TokCloseParen {
			return true
		}
		w := tokNextSig(tokens, n, closing+1)
		if w+2 < n && tokens[w].Kind == parser.TokWhen && tokens[w+1].Kind == parser.TokIdent && tokens[w+2].Kind == parser.TokOpenParen {
			switch parser.TokenText(source, tokens[w+1]) {
			case "is_atom", "is_tuple", "is_binary":
				return false
			}
		}
	}
	return true
}

// parseDispatchBodies parses every `def name do quote do ... end end` in text
// into its own usingBody, keyed by function name. Callers select one by the
// literal atom at the `use` site, so nothing is merged across targets.
func parseDispatchBodies(text string) map[string]*usingBody {
	source := []byte(text)
	tokens := parser.Tokenize(source)
	if usingDispatchParam(source, tokens) == "" {
		return nil
	}

	hs := &helperSource{lines: strings.Split(text, "\n"), source: source, tokens: tokens}
	fileAliases := extractAliasesFromTokens(source, tokens, -1)

	n := len(tokens)
	seen := make(map[string]bool)
	bodies := make(map[string]*usingBody)
	for i := 0; i < n; i++ {
		if tokens[i].Kind != parser.TokDef {
			continue
		}
		name, _, ok := parser.StaticDeclarationName(source, tokens, n, i)
		if !ok {
			continue
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		imported, inlineDefs, transUses, transCalls, optBindings, aliases, derives, optUses := parseHelperQuoteBlockDetailed(hs, name, fileAliases, make(map[string]bool))
		body := &usingBody{
			imports:     imported,
			inlineDefs:  inlineDefs,
			transUses:   transUses,
			optUses:     optUses,
			transCalls:  transCalls,
			optBindings: optBindings,
			aliases:     aliases,
			derives:     derives,
		}
		if !body.isEmpty() {
			bodies[name] = body
		}
	}
	if len(bodies) == 0 {
		return nil
	}
	return bodies
}

// inlineDef records a function or macro defined directly inside a __using__
// quote do block. These definitions get injected into any module that `use`s
// the parent module.
type inlineDef struct {
	line   int // 1-based line number in the source file
	arity  int
	kind   string // "def", "defp", "defmacro", etc.
	params string // comma-separated parameter names
	// For a defdelegate: the module of its to: (aliases resolved; empty for
	// `to: __MODULE__`, which in a quote is the calling module) and its as:.
	delegateTo, delegateAs string
}

// conditionalDerivePrefix marks a derive in usingBody.derives that is inside
// a branch of the __using__ body, so the caller's options decide it.
const conditionalDerivePrefix = "?"

// addStructInlineDefs records the functions that a defstruct or defexception
// in an injected body defines in the module that uses it, as the parser does
// for one in a module's own source.
func addStructInlineDefs(inlineDefs map[string][]inlineDef, tok parser.Token, source []byte) {
	kind := string(source[tok.Start:tok.End])
	for _, f := range parser.StructFunctions {
		inlineDefs["__struct__"] = append(inlineDefs["__struct__"], inlineDef{line: tok.Line, arity: f.Arity, kind: kind, params: f.Params})
	}
}

// parseUsingBody finds the defmacro __using__ block in text and scans its body
// for import statements, inline function definitions, transitive use calls,
// dynamic opt-driven imports (e.g. `import unquote(mod)` where `mod` comes from
// a Keyword.get on opts), and alias declarations that get injected into the
// consumer module.
//
// Uses the tokenizer so that heredocs, multi-line expressions, and comments are
// handled correctly without line-joining heuristics.
func parseUsingBody(text string) (imported []string, inlineDefs map[string][]inlineDef, transUses []string, optBindings []optBinding, aliases map[string]string) {
	imported, inlineDefs, transUses, _, optBindings, aliases, _, _, _ = parseUsingBodyDetailed(text)
	return
}

// parseUsingBodyDetailed also returns the protocols that an `@derive` in the
// body derives for each module that uses it, and the transitive uses that are
// inside a branch (if, case, ...) of the body.
func parseUsingBodyDetailed(text string) (imported []string, inlineDefs map[string][]inlineDef, transUses []string, transCalls []UseCall, optBindings []optBinding, aliases map[string]string, derives []string, branchUses []string, optUses []string) {
	return parseUsingBodyClause(text, false, nil)
}

// parseUsingBodyClause is parseUsingBodyDetailed for one clause of
// __using__: with atomClause, the first clause that does not take a keyword
// list (`defmacro __using__(which) when is_atom(which)`), which a `use Mod,
// :name` selects. bothKinds, when not nil, is set when the module has a
// clause of each kind, so that the caller parses the other one only then.
func parseUsingBodyClause(text string, atomClause bool, bothKinds *bool) (imported []string, inlineDefs map[string][]inlineDef, transUses []string, transCalls []UseCall, optBindings []optBinding, aliases map[string]string, derives []string, branchUses []string, optUses []string) {
	source := []byte(text)
	tokens := parser.Tokenize(source)
	n := len(tokens)

	nextSig := func(from int) int {
		return tokNextSig(tokens, n, from)
	}

	collectModuleName := func(i int) (string, int) {
		return tokCollectModuleName(source, tokens, n, i)
	}

	// Check if this module uses ExUnit.CaseTemplate
	usesCaseTemplate := false
	for i := 0; i < n; i++ {
		if tokens[i].Kind == parser.TokUse {
			j := nextSig(i + 1)
			mod, _ := collectModuleName(j)
			if mod == "ExUnit.CaseTemplate" {
				usesCaseTemplate = true
				break
			}
		}
	}

	// Find the __using__ entry point: defmacro __using__ or ExUnit.CaseTemplate `using`
	usingBodyStart := -1
	usingDepth := -1

	// firstClauseStart is the body of the first __using__ clause, which a
	// module whose clauses all take an atom (atom dispatch) keeps.
	firstClauseStart := -1
	// modules holds the block depth of each open defmodule: the text is the
	// source of one module, and a __using__ of a module nested in it is not
	// its own.
	var modules []int
	blockDepth := 0
	for i := 0; i < n; i++ {
		tok := tokens[i]
		switch tok.Kind {
		case parser.TokDo, parser.TokFn:
			parser.TrackBlockDepth(tok.Kind, &blockDepth)
		case parser.TokEnd:
			if len(modules) > 0 && modules[len(modules)-1] == blockDepth {
				modules = modules[:len(modules)-1]
			}
			parser.TrackBlockDepth(tok.Kind, &blockDepth)
		case parser.TokDefmodule:
			if _, nextPos, hasDo := parser.ScanForwardToBlockDo(tokens, n, i+1); hasDo {
				blockDepth++
				modules = append(modules, blockDepth)
				i = nextPos - 1
				continue
			}
		}
		if tok.Kind == parser.TokDefmacro && len(modules) <= 1 {
			name, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
			if ok && name == "__using__" {
				// Scan forward to find TokDo; Elixir allows split-line heads.
				_, nextPos, hasDo := parser.ScanForwardToBlockDo(tokens, n, j+1)
				if !usingClauseTakesList(source, tokens, n, j+1) {
					if atomClause && hasDo {
						usingBodyStart = nextPos
						usingDepth = 1
						break
					}
					// A clause for an atom or a tuple: the first clause that
					// takes the options of `use Mod, key: value` comes
					// before it, if there is one.
					if hasDo && firstClauseStart < 0 {
						firstClauseStart = nextPos
					}
					continue
				}
				if atomClause {
					continue
				}
				if hasDo {
					usingBodyStart = nextPos
					usingDepth = 1 // inside the defmacro do..end
				}
				if bothKinds != nil {
					*bothKinds = firstClauseStart >= 0 || laterNonListUsingClause(source, tokens, i+1)
				}
				break
			}
		}
		// ExUnit.CaseTemplate: `using do` or `using opts do`
		if usesCaseTemplate && tok.Kind == parser.TokIdent && string(source[tok.Start:tok.End]) == "using" {
			// Must be at statement start
			if i == 0 || tokens[i-1].Kind == parser.TokEOL {
				if _, nextPos, hasDo := parser.ScanForwardToBlockDo(tokens, n, i+1); hasDo {
					usingBodyStart = nextPos
					usingDepth = 1
				}
				if usingBodyStart >= 0 {
					break
				}
			}
		}
	}
	if usingBodyStart < 0 && firstClauseStart >= 0 && !atomClause {
		usingBodyStart, usingDepth = firstClauseStart, 1
	}
	if usingBodyStart < 0 {
		return
	}
	// unquote(__MODULE__) in the quote is the module that defines __using__:
	// unquote evaluates it where the macro is defined.
	definingModule := extractEnclosingModuleFromTokens(source, tokens, tokens[usingBodyStart].Line-1)

	// Extract file-level aliases for resolution (reuse already-tokenized data)
	lines := strings.Split(text, "\n")
	hs := &helperSource{lines: lines, source: source, tokens: tokens}
	fileAliases := extractAliasesFromTokens(source, tokens, -1)

	inlineDefs = make(map[string][]inlineDef)

	resolveAlias := func(modName string) string {
		if modName == unknownOpt {
			return modName
		}
		if resolved := parser.ResolveModuleRef(modName, aliases, ""); resolved != modName {
			return resolved
		}
		return parser.ResolveModuleRef(modName, fileAliases, "")
	}

	ob := newOptBinder(source, tokens, definingModule, resolveAlias)
	// vars are the edits of each options variable so far (see optVars).
	vars, varToOpt := ob.vars, ob.varToOpt
	// mergeHelperQuote merges what the helper's quote injects. open is the
	// helper call's open parenthesis: a variable passed as its first
	// argument (`build(opts)`) brings its edits to the helper's uses that
	// pass the options on.
	mergeHelperQuote := func(helperName string, open int) {
		helperImported, helperDefs, helperTransUses, helperCalls, helperBindings, helperAliases, helperDerives, helperOptUses := parseHelperQuoteBlockDetailed(hs, helperName, fileAliases, make(map[string]bool))
		optUses = append(optUses, helperOptUses...)
		// The argument: the body's options variable (`build(opts)`), or an
		// edit of it (`build(Keyword.put_new(opts, :key, Mod))`).
		if arg := nextSig(open + 1); open < n && arg < n {
			var edits []optEdit
			if next := nextSig(arg + 1); tokens[arg].Kind == parser.TokIdent && next < n && (tokens[next].Kind == parser.TokComma || tokens[next].Kind == parser.TokCloseParen) {
				edits = slices.Clone(vars[parser.TokenText(source, tokens[arg])]) // `build(opts, env)`
			} else if src, argEdits, mods, _, ok := readKeywordEdits(source, tokens, n, arg, ob.macroScope(fileAliases)); ok && src != "" {
				edits = append(slices.Clone(vars[src]), argEdits...)
				for _, m := range mods {
					optUses = append(optUses, resolveAlias(m))
				}
			}
			if len(edits) > 0 {
				for c := range helperCalls {
					if helperCalls[c].forwardOpts {
						helperCalls[c].forwardEdits = append(slices.Clone(edits), helperCalls[c].forwardEdits...)
					}
				}
				// The helper reads its options as the body passes them.
				for b := range helperBindings {
					if helperBindings[b].optKey != "" {
						helperBindings[b].readEdits = append(slices.Clone(edits), helperBindings[b].readEdits...)
					}
					if helperBindings[b].forwardOpts {
						helperBindings[b].forwardEdits = append(slices.Clone(edits), helperBindings[b].forwardEdits...)
					}
				}
			}
		}
		imported = append(imported, helperImported...)
		derives = append(derives, helperDerives...)
		for name, defs := range helperDefs {
			inlineDefs[name] = append(inlineDefs[name], defs...)
		}
		transUses = append(transUses, helperTransUses...)
		transCalls = append(transCalls, helperCalls...)
		optBindings = append(optBindings, helperBindings...)
		for short, full := range helperAliases {
			if aliases == nil {
				aliases = make(map[string]string)
			}
			aliases[short] = full
		}
	}

	// Walk tokens inside the __using__ body
	depth := usingDepth
	i := usingBodyStart
	// branches holds the depth inside each open if/unless/case/cond/with
	// block: what is inside one is injected only when the branch is taken,
	// which depends on the caller's options.
	var branches []int
	// quotes holds the depth inside each open `quote do` block: a match there
	// is code of the caller, not of the macro, so it does not bind the
	// macro's variables (`opts = unquote(opts)` in a quote).
	var quotes []int
	// clauses counts the clause boundaries (else, ->, rescue, ...) of each
	// open branch statement, parallel to branches; branchIDs names it.
	var clauses, branchIDs []int
	nextBranchID := 0
	// callClause is the branch statement and clause of each use call made
	// in a branch, by its index in transCalls.
	callClause := make(map[int][2]int)
	for i < n && depth > 0 {
		tok := tokens[i]
		ob.boundary(i)
		if len(branches) > 0 && depth == branches[len(branches)-1] && isClauseBoundary(source, tokens, i) {
			clauses[len(clauses)-1]++
		}

		switch tok.Kind {
		case parser.TokDo, parser.TokFn, parser.TokEnd:
			parser.TrackBlockDepth(tok.Kind, &depth)
			ob.block(tok.Kind)
			for len(branches) > 0 && branches[len(branches)-1] > depth {
				branches = branches[:len(branches)-1]
				clauses = clauses[:len(clauses)-1]
				branchIDs = branchIDs[:len(branchIDs)-1]
			}
			for len(quotes) > 0 && quotes[len(quotes)-1] > depth {
				quotes = quotes[:len(quotes)-1]
			}
			i++
		case parser.TokEOL, parser.TokComment, parser.TokString, parser.TokHeredoc,
			parser.TokSigil, parser.TokAtom, parser.TokNumber, parser.TokCharLiteral,
			parser.TokEOF:
			i++

		case parser.TokImport:
			i++
			j := nextSig(i)
			// import unquote(__MODULE__), or unquote(__MODULE__).Sub
			if self, k, ok := unquotedSelfModule(source, tokens, j, definingModule); ok {
				imported = append(imported, self)
				i = k
				continue
			}
			// import unquote(var)
			if j < n && tokens[j].Kind == parser.TokIdent && string(source[tokens[j].Start:tokens[j].End]) == "unquote" {
				if j+2 < n && tokens[j+1].Kind == parser.TokOpenParen && tokens[j+2].Kind == parser.TokIdent {
					varName := source[tokens[j+2].Start:tokens[j+2].End]
					if b, ok := varToOpt[string(varName)]; ok {
						optBindings = append(optBindings, optBinding{optKey: b.optKey, defaultMod: b.defaultMod, kind: "import", readEdits: b.readEdits})
					}
				}
				i = skipToEndOfStatement(tokens, n, j)
				continue
			}
			// import Module
			modName, k := collectModuleName(j)
			if modName != "" {
				imported = append(imported, resolveAlias(modName))
			}
			i = k

		case parser.TokUse:
			i++
			j := useArgStart(tokens, n, i-1)
			self, selfEnd, isSelf := unquotedSelfModule(source, tokens, j, definingModule)
			// use unquote(var), or use unquote(<a read of an option>)
			if !isSelf && j+1 < n && tokens[j].Kind == parser.TokIdent && string(source[tokens[j].Start:tokens[j].End]) == "unquote" && tokens[j+1].Kind == parser.TokOpenParen {
				if b, ok := ob.unquotedBinding(j + 1); ok {
					binding := optBinding{optKey: b.optKey, defaultMod: b.defaultMod, kind: "use", readEdits: b.readEdits}
					if end := matchingBracket(tokens, n, j+1); end > 0 {
						if c := nextSig(end + 1); c < n && tokens[c].Kind == parser.TokComma {
							binding.forwardEdits, binding.forwardOpts, _ = forwardedOpts(source, tokens, n, c+1, vars, ob.quoteScope(fileAliases))
						}
					}
					optBindings = append(optBindings, binding)
				}
				i = skipToEndOfStatement(tokens, n, j)
				continue
			}
			// use Module
			modName, k := collectModuleName(j)
			if isSelf {
				modName, k = self, selfEnd
			}
			if modName != "" {
				resolved := modName
				if !isSelf {
					resolved = resolveAlias(modName)
				}
				transUses = append(transUses, resolved)
				if len(branches) > 0 {
					branchUses = append(branchUses, resolved)
				}
				call := UseCall{Module: resolved}
				nk := nextSig(k)
				if nk < n && tokens[nk].Kind == parser.TokComma {
					call.Opts = useCallOpts(source, tokens, n, nk+1, ob.quoteScope(fileAliases))
					call.Which, call.WhichKey = tokCollectDispatchAtom(source, tokens, n, nk+1)
					call.forwardEdits, call.forwardOpts, call.callerWins = forwardedOpts(source, tokens, n, nk+1, vars, ob.quoteScope(fileAliases))
				}
				if len(branches) > 0 {
					callClause[len(transCalls)] = [2]int{branchIDs[len(branchIDs)-1], clauses[len(clauses)-1]}
				}
				transCalls = append(transCalls, call)
			}
			i = k

		case parser.TokAlias:
			i++
			j := nextSig(i)
			if self, k, ok := unquotedSelfModule(source, tokens, j, definingModule); ok {
				if aliases == nil {
					aliases = make(map[string]string)
				}
				if asName, nextPos, ok := parser.ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
					aliases[asName] = self
					k = nextPos
				} else {
					aliases[parser.AliasShortName(self)] = self
				}
				i = k
				continue
			}
			modName, k := collectModuleName(j)
			if modName == "" {
				i = k
				continue
			}
			// Multi-alias: alias Parent.{A, B}
			if children, nextPos, ok := parser.ScanMultiAliasChildren(source, tokens, n, k, false); ok {
				parent := resolveAlias(modName)
				for _, childName := range children {
					if aliases == nil {
						aliases = make(map[string]string)
					}
					aliases[parser.AliasShortName(childName)] = parent + "." + childName
				}
				i = nextPos
				continue
			}
			// alias Module, as: Name
			if asName, nextPos, ok := parser.ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
				if aliases == nil {
					aliases = make(map[string]string)
				}
				aliases[asName] = resolveAlias(modName)
				i = nextPos - 1
				continue
			}
			// Simple alias
			resolved := resolveAlias(modName)
			if aliases == nil {
				aliases = make(map[string]string)
			}
			aliases[parser.AliasShortName(resolved)] = resolved
			i = k

		case parser.TokRequire:
			// require Module, as: Name — injects an alias just like `alias`.
			i++
			j := nextSig(i)
			modName, k := collectModuleName(j)
			if modName == "" {
				i = k
				continue
			}
			if asName, nextPos, ok := parser.ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
				if aliases == nil {
					aliases = make(map[string]string)
				}
				aliases[asName] = resolveAlias(modName)
				i = nextPos - 1
				continue
			}
			i = k

		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			kind := string(source[tok.Start:tok.End])
			defLine := tok.Line
			funcName, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
			if !ok {
				i = skipToEndOfStatement(tokens, n, j)
				continue
			}
			j++
			pj := nextSig(j)
			nextPos := pj
			maxArity := 0
			defaultCount := 0
			var paramNames []string
			if pj < n && tokens[pj].Kind == parser.TokOpenParen {
				maxArity, defaultCount, paramNames, nextPos = collectParamsFromTokens(source, tokens, n, pj)
				paramNames = parser.FixParamNames(paramNames)
			}
			minArity := maxArity - defaultCount
			end := skipToEndOfStatement(tokens, n, nextPos)
			var delegateTo, delegateAs string
			if kind == "defdelegate" {
				delegateTo, delegateAs = delegateOptions(source, tokens, nextPos, end, resolveAlias)
			}
			for arity := minArity; arity <= maxArity; arity++ {
				inlineDefs[funcName] = append(inlineDefs[funcName], inlineDef{
					line:       defLine,
					arity:      arity,
					kind:       kind,
					params:     parser.JoinParams(paramNames, arity),
					delegateTo: delegateTo,
					delegateAs: delegateAs,
				})
			}
			i = end
			continue

		case parser.TokDefstruct, parser.TokDefexception:
			addStructInlineDefs(inlineDefs, tok, source)
			i++

		case parser.TokAttr:
			if tok.End-tok.Start == 7 && string(source[tok.Start:tok.End]) == "@derive" {
				for _, p := range parser.DeriveProtocols(source, tokens, n, i+1) {
					derive := resolveAlias(p)
					if len(branches) > 0 || inBranchStatement(source, tokens, i) {
						// Marked: the caller's options decide.
						derive = conditionalDerivePrefix + derive
					}
					derives = append(derives, derive)
				}
			}
			i++

		case parser.TokModule:
			// Check for Keyword.put/put_new(opts, :key, Module) heuristic
			modText := string(source[tok.Start:tok.End])
			if modText == "Keyword" && i+2 < n && tokens[i+1].Kind == parser.TokDot && tokens[i+2].Kind == parser.TokIdent {
				funcName := string(source[tokens[i+2].Start:tokens[i+2].End])
				if funcName == "put" || funcName == "put_new" {
					_, atomKey, defaultMod, endJ := ob.scanKeywordCall(i)
					if atomKey != "" && defaultMod != "" && defaultMod != unknownOpt {
						optUses = append(optUses, resolveAlias(defaultMod))
					}
					i = endJ
					continue
				}
				if funcName == "get" || funcName == "pop" {
					_, atomKey, defaultMod, endJ := ob.scanKeywordCall(i)
					if atomKey != "" {
						// This is just a bare Keyword.get/pop, not an assignment.
						// Only var = Keyword.get/pop patterns are handled in the TokIdent case.
						_ = defaultMod
						i = endJ
						continue
					}
				}
			}
			i++

		case parser.TokIdent:
			identName := string(source[tok.Start:tok.End])
			switch identName {
			case "if", "unless", "case", "cond", "with":
				if _, _, hasDo := parser.ScanForwardToMacroCallBlockDo(tokens, n, i+1); hasDo {
					// The do is counted when the walk reaches it.
					branches = append(branches, depth+1)
					clauses = append(clauses, 0)
					nextBranchID++
					branchIDs = append(branchIDs, nextBranchID)
				}
			case "quote":
				if _, _, hasDo := parser.ScanForwardToMacroCallBlockDo(tokens, n, i+1); hasDo {
					quotes = append(quotes, depth+1)
				}
			}
			isStmtStart := i == 0 || tokens[i-1].Kind == parser.TokEOL || tokens[i-1].Kind == parser.TokComment
			j := nextSig(i + 1)

			// Phoenix-style __using__ bodies commonly inject a helper's quote with
			// `unquote(prelude(opts))`. Follow that helper just as helper quote
			// parsing follows nested unquotes.
			if identName == "unquote" && j < n && tokens[j].Kind == parser.TokOpenParen {
				helper := nextSig(j + 1)
				if helper < n && tokens[helper].Kind == parser.TokIdent {
					afterHelper := nextSig(helper + 1)
					if afterHelper < n && tokens[afterHelper].Kind == parser.TokOpenParen {
						mergeHelperQuote(string(source[tokens[helper].Start:tokens[helper].End]), afterHelper)
						i = skipToEndOfStatement(tokens, n, i)
						continue
					}
				}
			}

			// var = <an edit of the options, a read of one option, a module,
			// or a value that Dexter cannot read>.
			if isStmtStart && len(quotes) == 0 {
				if end, mods, handled := ob.assignment(i, fileAliases); handled {
					for _, m := range mods {
						optUses = append(optUses, resolveAlias(m))
					}
					i = end
					continue
				}
			}
			// Check for bare function call that delegates to a helper:
			// helper_name(opts) where helper_name is a def/defp in the same file.
			// Only at statement start to avoid matching function calls inside expressions.
			if isStmtStart && j < n && tokens[j].Kind == parser.TokOpenParen && !parser.IsElixirKeyword(identName) {
				mergeHelperQuote(identName, j)
				i = skipToEndOfStatement(tokens, n, i)
				continue
			}
			i++

		case parser.TokOpenBrace:
			// {mod, opts} = Keyword.pop(opts, :key, Default), and
			// {:ok, mod} = Keyword.fetch(opts, :key)
			if len(quotes) == 0 {
				if end, handled := ob.tupleAssignment(i); handled {
					i = end
					continue
				}
			}
			i++

		default:
			i++
		}
	}
	unknownWhenClausesDiffer(transCalls, callClause)
	return
}

// isClauseBoundary reports whether token i starts another clause of a
// block: `else`, `rescue`, `catch`, or `after` at the start of a line (not
// the `else:` of a keyword list), or the `->` of a clause.
func isClauseBoundary(source []byte, tokens []parser.Token, i int) bool {
	tok := tokens[i]
	switch tok.Kind {
	case parser.TokRightArrow:
		return true
	case parser.TokIdent:
		switch string(source[tok.Start:tok.End]) {
		case "else", "rescue", "catch", "after":
		default:
			return false
		}
		if i > 0 && tokens[i-1].Kind != parser.TokEOL && tokens[i-1].Kind != parser.TokComment {
			return false
		}
		return i+1 >= len(tokens) || tokens[i+1].Kind != parser.TokColon
	}
	return false
}

// unknownWhenClausesDiffer makes the options of use calls unknown when two
// clauses of one branch statement (the do and else of an if, two case
// clauses) use the same module with different options. Which clause runs
// depends on values that Dexter does not evaluate, so neither clause's
// options are sure; with unknown options, a lookup gives the use line
// instead of the answer of one clause.
func unknownWhenClausesDiffer(calls []UseCall, callClause map[int][2]int) {
	if len(callClause) < 2 {
		return
	}
	type group struct {
		stmt   int
		module string
	}
	byGroup := make(map[group][]int)
	for k, c := range callClause {
		g := group{c[0], calls[k].Module}
		byGroup[g] = append(byGroup[g], k)
	}
	for _, ks := range byGroup {
		differ := false
		for _, a := range ks {
			for _, b := range ks {
				if callClause[a][1] != callClause[b][1] && !sameUseOptions(calls[a], calls[b]) {
					differ = true
				}
			}
		}
		if !differ {
			continue
		}
		for _, k := range ks {
			calls[k].Opts = nil
			calls[k].forwardOpts = true
			calls[k].forwardEdits = []optEdit{{op: "unknown"}}
			calls[k].callerWins = false
		}
	}
}

func sameUseOptions(a, b UseCall) bool {
	return maps.Equal(a.Opts, b.Opts) && a.forwardOpts == b.forwardOpts && a.callerWins == b.callerWins &&
		a.Which == b.Which && a.WhichKey == b.WhichKey && slices.EqualFunc(a.forwardEdits, b.forwardEdits, func(x, y optEdit) bool {
		return x.op == y.op && x.key == y.key && x.mod == y.mod && slices.Equal(x.keys, y.keys)
	})
}

// collectParamsFromTokens delegates to the shared parser implementation.
var collectParamsFromTokens = parser.CollectParams

// ModuleAttributeAtCursor returns the attribute name if the cursor is on a
// @attr reference, otherwise returns "". For example, on "@endpoint_scopes"
// returns "endpoint_scopes". Uses the token stream to correctly ignore
// attributes inside strings, comments, and heredocs.
func ModuleAttributeAtCursor(tokens []parser.Token, source []byte, lineStarts []int, line, col int) string {
	offset := parser.LineColToOffset(lineStarts, line, col)
	if offset < 0 {
		return ""
	}

	idx := parser.TokenAtOffset(tokens, offset)
	if idx < 0 {
		return ""
	}

	tok := tokens[idx]
	if tok.Kind != parser.TokAttr {
		return ""
	}

	text := parser.TokenText(source, tok)
	if len(text) <= 1 {
		return ""
	}
	return text[1:] // strip leading '@'
}

// ExtractModuleAttribute is the TokenizedFile method version of ModuleAttributeAtCursor.
func (tf *TokenizedFile) ModuleAttributeAtCursor(line, col int) string {
	name := ModuleAttributeAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
	if name == "" && len(tf.interp) > 0 {
		// "#{@base_url}/path" — the attribute is inside a string literal, so
		// it lives in the interpolation stream.
		name = ModuleAttributeAtCursor(tf.interp, tf.source, tf.lineStarts, line, col)
	}
	return name
}

// reservedModuleAttrs are Elixir built-in module attributes that are not
// user-defined and should not be jumped to.
var reservedModuleAttrs = map[string]bool{
	"moduledoc": true, "doc": true, "typedoc": true,
	"spec": true, "type": true, "typep": true, "opaque": true,
	"behaviour": true, "callback": true, "macrocallback": true,
	"optional_callbacks": true, "impl": true, "derive": true,
	"enforce_keys": true, "deprecated": true, "dialyzer": true,
	"compile": true, "vsn": true, "on_load": true, "nifs": true,
}

// FindModuleAttributeDefinition searches for the line where @attr_name is defined
// (assigned a value, not used). Returns the 1-based line number and true if found.
// Returns false for reserved Elixir module attributes.
func FindModuleAttributeDefinition(text string, attrName string) (int, bool) {
	return FindModuleAttributeDefinitionTokenized(text, attrName)
}

// findBareCallLines returns the 1-based lines of the unqualified calls to
// functionName in the tokens of source: functionName(...), functionName arg,
// |> functionName, and &functionName/1. Definition lines are excluded.
func findBareCallLines(source []byte, tokens, interp []parser.Token, functionName string) []int {
	n := len(tokens)

	seenLines := make(map[int]bool)

	for i := 0; i < n; i++ {
		tok := tokens[i]

		if tok.Kind != parser.TokIdent {
			continue
		}
		if parser.TokenText(source, tok) != functionName {
			continue
		}
		// The name of a def, @spec, or @callback head is not a call. A call
		// on the same line is: `def f(x), do: f(x, [])` calls f/2.
		if i > 0 && isHeadKeyword(tokens[i-1].Kind) {
			continue
		}

		// Check this is a bare call (not preceded by dot)
		if i > 0 && tokens[i-1].Kind == parser.TokDot {
			continue
		}

		// Check it's followed by ( or preceded by |>
		isCall := false
		j := tokNextSig(tokens, n, i+1)
		if j < n && tokens[j].Kind == parser.TokOpenParen {
			isCall = true
		}
		// A capture without a module: &functionName/arity
		if !isCall && i > 0 && tokens[i-1].Kind == parser.TokOther && parser.TokenText(source, tokens[i-1]) == "&" {
			isCall = true
		}
		// A call without parentheses: functionName arg, ...
		if !isCall && parser.NoParenArgCount(source, tokens, i) >= 0 {
			isCall = true
		}
		// A call whose only argument is a do block: `functionName do ... end`
		// (not `if name do`, where the do is if's).
		if !isCall && j < n && tokens[j].Kind == parser.TokDo && parser.CallArity(source, tokens, i) >= 1 {
			isCall = true
		}
		// Check for pipe call: |> functionName
		if !isCall && i > 0 {
			// Look back for |> (may have EOL/comments between)
			for k := i - 1; k >= 0; k-- {
				if tokens[k].Kind == parser.TokPipe {
					isCall = true
					break
				}
				if tokens[k].Kind != parser.TokEOL && tokens[k].Kind != parser.TokComment {
					break
				}
			}
		}

		if isCall && !seenLines[tok.Line] {
			seenLines[tok.Line] = true
		}
	}

	// Calls inside #{} interpolations. The interpolation stream holds no
	// parentheses, so a call is an identifier followed by '(' in the source;
	// without one it can be a variable of the same name, and it stays.
	for i, tok := range interp {
		if tok.Kind != parser.TokIdent || parser.TokenText(source, tok) != functionName {
			continue
		}
		if i > 0 && interp[i-1].Kind == parser.TokDot && interp[i-1].End == tok.Start {
			continue
		}
		j := tok.End
		for j < len(source) && (source[j] == ' ' || source[j] == '\t') {
			j++
		}
		if j < len(source) && source[j] == '(' {
			seenLines[tok.Line] = true
		}
	}

	var lineNums []int
	for line := range seenLines {
		lineNums = append(lineNums, line)
	}
	// Sort for deterministic output
	slices.Sort(lineNums)
	return lineNums
}

// CallContextAtCursor scans backward through the token stream from (lineNum, col)
// to find the innermost open function call. Returns the function expression (e.g.
// "Enum.map" or "my_func"), the 0-based argument index, and true if found.
// Handles both parenthesized calls like `Enum.map(list, fun)` and paren-less
// calls like `IO.puts "hello"` or `import MyApp.Repo`.
func CallContextAtCursor(tokens []parser.Token, source []byte, lineStarts []int, lineNum, col int) (funcExpr string, argIndex int, ok bool) {
	offset := parser.LineColToOffset(lineStarts, lineNum, col)
	if offset < 0 {
		return "", 0, false
	}

	startIdx := tokenAtOrBeforeOffset(tokens, offset)
	if startIdx < 0 {
		return "", 0, false
	}

	// If cursor is inside a comment, bail out (strings may be arguments)
	if tokens[startIdx].Kind == parser.TokComment {
		return "", 0, false
	}

	// If cursor is exactly on a closing delimiter, step back one token so the
	// scan sees us as *inside* the call rather than outside the balanced pair.
	scanIdx := startIdx
	switch tokens[scanIdx].Kind {
	case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace:
		if scanIdx > 0 {
			scanIdx--
		}
	}

	// Try parenthesized call first
	if expr, argIdx, found := callContextParen(tokens, source, scanIdx); found {
		return expr, argIdx, true
	}

	// Try paren-less call: scan backward on the same line for
	// `func_or_module.func arg1, arg2` patterns.
	return callContextNoParen(tokens, source, startIdx)
}

// tokenAtOrBeforeOffset returns the index of the token at or just before the
// given byte offset. Returns -1 if no suitable token exists.
func tokenAtOrBeforeOffset(tokens []parser.Token, offset int) int {
	idx := parser.TokenAtOffset(tokens, offset)
	if idx >= 0 {
		return idx
	}
	// Cursor is between tokens — find the last token starting before offset
	for i := len(tokens) - 1; i >= 0; i-- {
		if tokens[i].Start < offset {
			return i
		}
	}
	return -1
}

// collectDotChain walks backward from tokens[j] collecting a dotted identifier
// chain (e.g. Module.SubModule.func). Returns the expression string or "".
func collectDotChain(tokens []parser.Token, source []byte, j int) string {
	var parts []string
	for j >= 0 {
		t := tokens[j]
		if isCallableToken(t.Kind) {
			parts = append(parts, parser.TokenText(source, t))
			if j-1 >= 0 && tokens[j-1].Kind == parser.TokDot {
				j -= 2
				continue
			}
			break
		}
		break
	}
	if len(parts) == 0 {
		return ""
	}
	for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
		parts[l], parts[r] = parts[r], parts[l]
	}
	return strings.Join(parts, ".")
}

// callContextParen scans backward from startIdx looking for an unmatched open
// paren to identify a parenthesized function call.
//
// All bracket types (paren, bracket, brace) are tracked in a unified depth
// counter so that commas inside nested containers are not counted toward the
// outer call's argument index.
func callContextParen(tokens []parser.Token, source []byte, startIdx int) (string, int, bool) {
	depth := 0
	commas := 0

	for i := startIdx; i >= 0; i-- {
		tok := tokens[i]

		switch tok.Kind {
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace, parser.TokCloseAngle:
			depth++
		case parser.TokOpenBracket, parser.TokOpenBrace, parser.TokOpenAngle:
			if depth > 0 {
				depth--
			} else {
				// Unmatched open bracket/brace/angle — we exited a container
				// that is itself an argument. Reset comma count for this
				// nesting level and keep scanning for the enclosing call.
				commas = 0
			}
		case parser.TokOpenParen:
			if depth > 0 {
				depth--
			} else {
				j := i - 1
				// Anonymous call: callback.(arg) — skip the dot
				if j >= 0 && tokens[j].Kind == parser.TokDot {
					j--
				}
				expr := collectDotChain(tokens, source, j)
				if expr == "" || parser.IsElixirKeyword(expr) {
					return "", 0, false
				}
				return expr, commas, true
			}
		case parser.TokComma:
			if depth == 0 {
				commas++
			}
		}
	}
	return "", 0, false
}

// isCallableToken returns true if the token kind can be the name of a
// paren-less function/macro call.
func isCallableToken(kind parser.TokenKind) bool {
	switch kind {
	case parser.TokIdent, parser.TokModule,
		parser.TokImport, parser.TokAlias, parser.TokUse, parser.TokRequire:
		return true
	default:
		return false
	}
}

// isArgStartToken returns true if the token kind can appear as the beginning
// of a function argument (i.e., it's a value-like token, not an operator).
func isArgStartToken(kind parser.TokenKind) bool {
	switch kind {
	case parser.TokIdent, parser.TokModule, parser.TokNumber,
		parser.TokString, parser.TokHeredoc, parser.TokSigil,
		parser.TokAtom, parser.TokCharLiteral,
		parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace,
		parser.TokOpenAngle, parser.TokPercent,
		parser.TokAttr, parser.TokFn:
		return true
	default:
		return false
	}
}

// callContextNoParen detects paren-less function calls by scanning backward
// for a pattern like `ident arg, arg` or `Module.func arg, arg` where the
// function name is separated from its arguments by whitespace (no parens).
//
// Follows Elixir's own approach (Code.Fragment): if the preceding token is an
// identifier separated by whitespace from the next token, it's a no-paren call.
func callContextNoParen(tokens []parser.Token, source []byte, startIdx int) (string, int, bool) {
	depth := 0
	commas := 0

	for i := startIdx; i >= 0; i-- {
		tok := tokens[i]

		switch tok.Kind {
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace, parser.TokCloseAngle:
			depth++
		case parser.TokOpenParen:
			if depth > 0 {
				depth--
			} else {
				return "", 0, false
			}
		case parser.TokOpenBracket, parser.TokOpenBrace, parser.TokOpenAngle:
			if depth > 0 {
				depth--
			} else {
				commas = 0
			}
		case parser.TokComma:
			if depth == 0 {
				commas++
			}
		default:
			if depth == 0 && isCallableToken(tok.Kind) {
				if i+1 < len(tokens) {
					next := tokens[i+1]
					// Part of a dotted chain — keep scanning
					if next.Kind == parser.TokDot {
						continue
					}
					// Must be separated by whitespace AND followed by a
					// value-like token (not an operator like =, ->, etc.)
					if next.Start > tok.End && isArgStartToken(next.Kind) {
						expr := collectDotChain(tokens, source, i)
						if expr == "" || parser.IsElixirKeyword(expr) {
							return "", 0, false
						}
						return expr, commas, true
					}
				}
			}
		}
	}
	return "", 0, false
}

// CallContextAtCursor is the TokenizedFile method version.
func (tf *TokenizedFile) CallContextAtCursor(line, col int) (funcExpr string, argIndex int, ok bool) {
	return CallContextAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
}

// extractParamNames reads the function definition line at defIdx and returns
// the parameter names. Falls back to positional names (arg1, arg2, ...) for
// complex patterns.
func extractParamNames(lines []string, defIdx int) []string {
	if defIdx < 0 || defIdx >= len(lines) {
		return nil
	}

	// Tokenize just the single line for efficiency
	source := []byte(lines[defIdx])
	tokens := parser.Tokenize(source)
	n := len(tokens)

	for i := 0; i < n; i++ {
		tok := tokens[i]

		switch tok.Kind {
		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			_, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
			if !ok {
				return nil
			}
			j++
			pj := tokNextSig(tokens, n, j)
			if pj >= n || tokens[pj].Kind != parser.TokOpenParen {
				return nil
			}
			_, _, paramNames, _ := parser.CollectParams(source, tokens, n, pj)
			return parser.FixParamNames(paramNames)
		}
	}
	return nil
}

// delegateOptions reads the to: and as: options of a defdelegate in
// tokens[from:to]. resolve resolves the module through the aliases in scope.
func delegateOptions(source []byte, tokens []parser.Token, from, to int, resolve func(string) string) (delegateTo, delegateAs string) {
	n := len(tokens)
	for i := from; i < to && i+1 < n; i++ {
		if tokens[i].Kind != parser.TokIdent || tokens[i+1].Kind != parser.TokColon {
			continue
		}
		switch parser.TokenText(source, tokens[i]) {
		case "to":
			if mod, _ := tokCollectModuleName(source, tokens, n, tokNextSig(tokens, n, i+2)); mod != "" && !strings.Contains(mod, "__MODULE__") {
				delegateTo = resolve(mod)
			}
		case "as":
			if j := tokNextSig(tokens, n, i+2); j < n && tokens[j].Kind == parser.TokAtom {
				delegateAs = strings.TrimPrefix(parser.TokenText(source, tokens[j]), ":")
			}
		}
	}
	return delegateTo, delegateAs
}

// inBranchStatement reports whether the token at i is in a statement that
// an if, unless, case, cond, or with starts, as in the inline form `if
// unquote(json), do: @derive(Jason.Encoder)`: whether it runs depends on
// the condition. The block form is found by the depth of its do.
func inBranchStatement(source []byte, tokens []parser.Token, i int) bool {
	depth := 0
	j := i - 1
	for ; j >= 0; j-- {
		switch tokens[j].Kind {
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace:
			depth++
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
			depth--
		case parser.TokDo, parser.TokEnd, parser.TokFn:
			if depth <= 0 {
				// A block starts or ends before the statement.
				return false
			}
		case parser.TokEOL:
			if depth > 0 {
				continue
			}
			// A statement goes on after a comma or a keyword colon.
			if k := prevSigTokenIndex(tokens, j-1); k >= 0 && (tokens[k].Kind == parser.TokComma || tokens[k].Kind == parser.TokColon) {
				continue
			}
			return startsBranch(source, tokens, j+1)
		}
	}
	return startsBranch(source, tokens, 0)
}

func startsBranch(source []byte, tokens []parser.Token, i int) bool {
	for i < len(tokens) && (tokens[i].Kind == parser.TokEOL || tokens[i].Kind == parser.TokComment) {
		i++
	}
	if i >= len(tokens) || tokens[i].Kind != parser.TokIdent {
		return false
	}
	switch parser.TokenText(source, tokens[i]) {
	case "if", "unless", "case", "cond", "with":
		return true
	}
	return false
}

func prevSigTokenIndex(tokens []parser.Token, i int) int {
	for i >= 0 && (tokens[i].Kind == parser.TokEOL || tokens[i].Kind == parser.TokComment) {
		i--
	}
	return i
}

// unquotedSelfModule reads `unquote(__MODULE__)`, with an optional `.Sub`
// after it, at tokens[j] in a quote: the module that defines the macro
// (definingModule), since unquote evaluates __MODULE__ where the macro is
// defined. A plain __MODULE__ in a quote is the calling module, and is not
// read here. k is the token after the name.
func unquotedSelfModule(source []byte, tokens []parser.Token, j int, definingModule string) (module string, k int, ok bool) {
	n := len(tokens)
	if definingModule == "" || j+3 >= n || tokens[j].Kind != parser.TokIdent || parser.TokenText(source, tokens[j]) != "unquote" ||
		tokens[j+1].Kind != parser.TokOpenParen || tokens[j+2].Kind != parser.TokModule ||
		parser.TokenText(source, tokens[j+2]) != "__MODULE__" || tokens[j+3].Kind != parser.TokCloseParen {
		return "", j, false
	}
	module, k = definingModule, j+4
	for k+1 < n && tokens[k].Kind == parser.TokDot && tokens[k+1].Kind == parser.TokModule && tokens[k+1].Start == tokens[k].End {
		module += "." + parser.TokenText(source, tokens[k+1])
		k += 2
	}
	return module, k, true
}
