package lsp

import (
	"slices"
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
}

// NewTokenizedFile tokenizes the text once for reuse across multiple queries.
func NewTokenizedFile(text string) *TokenizedFile {
	source := []byte(text)
	result := parser.TokenizeFull(source)
	return &TokenizedFile{
		source:     source,
		tokens:     result.Tokens,
		n:          len(result.Tokens),
		lineStarts: result.LineStarts,
	}
}

// NewTokenizedFileFromCache wraps pre-existing tokens (e.g. from DocumentStore cache).
func NewTokenizedFileFromCache(tokens []parser.Token, source []byte, lineStarts []int) *TokenizedFile {
	return &TokenizedFile{
		source:     source,
		tokens:     tokens,
		n:          len(tokens),
		lineStarts: lineStarts,
	}
}

// ExpressionAtCursor extracts the dotted expression at the given 0-based line
// and col, using the cached token stream.
func (tf *TokenizedFile) ExpressionAtCursor(line, col int) CursorContext {
	return ExpressionAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
}

// FullExpressionAtCursor extracts the complete dotted expression at the given
// 0-based line and col without truncating at the cursor's segment.
func (tf *TokenizedFile) FullExpressionAtCursor(line, col int) CursorContext {
	return FullExpressionAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
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
	for i := 0; i < tf.n; i++ {
		tok := tf.tokens[i]

		switch tok.Kind {
		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			j := tokNextSig(tf.tokens, tf.n, i+1)
			if j >= tf.n || tf.tokens[j].Kind != parser.TokIdent {
				continue
			}
			if parser.TokenText(tf.source, tf.tokens[j]) == functionName {
				return tok.Line, true
			}

		case parser.TokAttrType:
			j := tokNextSig(tf.tokens, tf.n, i+1)
			if j >= tf.n || tf.tokens[j].Kind != parser.TokIdent {
				continue
			}
			if parser.TokenText(tf.source, tf.tokens[j]) == functionName {
				return tok.Line, true
			}
		}
	}
	return 0, false
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

// VariableFieldAccess describes a `variable.field_prefix` context at the cursor.
type VariableFieldAccess struct {
	VariableName string
	FieldPrefix  string
	StartCol     int // column where the field prefix starts (for textEdit)
}

// VariableFieldAccessAtCursor detects whether the cursor is in a `variable.`
// or `variable.field_prefix` position and returns the variable name and partial
// field name. Returns ok=false if the cursor is not in such a position.
func (tf *TokenizedFile) VariableFieldAccessAtCursor(line, col int) (VariableFieldAccess, bool) {
	return VariableFieldAccessAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
}

func VariableFieldAccessAtCursor(tokens []parser.Token, source []byte, lineStarts []int, line, col int) (VariableFieldAccess, bool) {
	if line < 0 || line >= len(lineStarts) || col <= 0 {
		return VariableFieldAccess{}, false
	}

	lineStart := lineStarts[line]
	offset := parser.LineColToOffset(lineStarts, line, col)
	if offset <= lineStart {
		return VariableFieldAccess{}, false
	}

	idx := parser.TokenAtOffset(tokens, offset-1)
	if idx < 0 {
		return VariableFieldAccess{}, false
	}

	tok := tokens[idx]

	// Case 1: cursor right after dot — "variable.|"
	if tok.Kind == parser.TokDot {
		if idx < 1 {
			return VariableFieldAccess{}, false
		}
		prev := tokens[idx-1]
		if prev.Kind != parser.TokIdent {
			return VariableFieldAccess{}, false
		}
		varName := parser.TokenText(source, prev)
		if strings.HasPrefix(varName, "_") || parser.IsElixirKeyword(varName) {
			return VariableFieldAccess{}, false
		}
		return VariableFieldAccess{
			VariableName: varName,
			FieldPrefix:  "",
			StartCol:     tok.End - lineStart, // right after the dot
		}, true
	}

	// Case 2: cursor on field prefix — "variable.fie|"
	if tok.Kind == parser.TokIdent && idx >= 2 {
		dotTok := tokens[idx-1]
		if dotTok.Kind != parser.TokDot {
			return VariableFieldAccess{}, false
		}
		varTok := tokens[idx-2]
		if varTok.Kind != parser.TokIdent {
			return VariableFieldAccess{}, false
		}
		varName := parser.TokenText(source, varTok)
		if strings.HasPrefix(varName, "_") || parser.IsElixirKeyword(varName) {
			return VariableFieldAccess{}, false
		}
		// The field prefix is the portion of the current token up to the cursor
		fieldEnd := offset
		if fieldEnd > tok.End {
			fieldEnd = tok.End
		}
		fieldPrefix := string(source[tok.Start:fieldEnd])
		return VariableFieldAccess{
			VariableName: varName,
			FieldPrefix:  fieldPrefix,
			StartCol:     tok.Start - lineStart,
		}, true
	}

	return VariableFieldAccess{}, false
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

// StructCompletionContext describes completion inside `%Module{...}` field keys.
type StructCompletionContext struct {
	ModuleRef   string
	FieldPrefix string
	StartCol    int
}

type StructModuleRef struct {
	ModuleRef string
	Line      int
}

// StructModuleRefs returns module references used in struct literals, including
// incomplete `%Module` expressions before the opening brace has been typed.
func (tf *TokenizedFile) StructModuleRefs() []StructModuleRef {
	return StructModuleRefs(tf.tokens, tf.source)
}

func StructModuleRefs(tokens []parser.Token, source []byte) []StructModuleRef {
	var refs []StructModuleRef
	for i := 0; i < len(tokens); i++ {
		if tokens[i].Kind != parser.TokPercent {
			continue
		}
		j := tokNextSig(tokens, len(tokens), i+1)
		if j >= len(tokens) || tokens[j].Kind != parser.TokModule {
			continue
		}
		moduleRef, k := tokCollectModuleName(source, tokens, len(tokens), j)
		if moduleRef == "" {
			continue
		}
		k = tokNextSig(tokens, len(tokens), k)
		refs = append(refs, StructModuleRef{
			ModuleRef: moduleRef,
			Line:      tokens[i].Line - 1,
		})
		if k < len(tokens) && tokens[k].Kind == parser.TokOpenBrace {
			i = k
		} else {
			i = k - 1
		}
	}
	return refs
}

// StructCompletionContextAtCursor returns the struct module and current field
// prefix when the cursor is inside a struct literal/update key position.
func (tf *TokenizedFile) StructCompletionContextAtCursor(line, col int) (StructCompletionContext, bool) {
	return StructCompletionContextAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
}

// StructValueContextAtCursor reports whether the cursor is in a top-level value
// position inside a struct literal, e.g. `%User{name: |}`.
func (tf *TokenizedFile) StructValueContextAtCursor(line, col int) bool {
	return StructValueContextAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
}

func (tf *TokenizedFile) VariableNamesBeforeCursor(line, col int) []string {
	return VariableNamesBeforeCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
}

// VariableStructTypes returns a map of variable names to their struct module
// references for variables that are bound to struct literals via pattern matching
// or assignment before the given cursor position within the current function scope.
// The module references are unresolved (e.g. "User", "MyApp.User", "__MODULE__").
func (tf *TokenizedFile) VariableStructTypes(line, col int) map[string]string {
	return VariableStructTypes(tf.tokens, tf.source, tf.lineStarts, line, col)
}

// VariableStructTypes scans from the enclosing function definition to the cursor
// position and identifies variables bound to struct types via patterns like:
//
//	%User{} = user       (match on left, var on right)
//	user = %User{...}    (var on left, struct on right)
//	def foo(%User{} = user)  (function head pattern)
//
// Returns a map of variable name -> module reference string.
func VariableStructTypes(tokens []parser.Token, source []byte, lineStarts []int, line, col int) map[string]string {
	offset := parser.LineColToOffset(lineStarts, line, col)
	if offset < 0 {
		return nil
	}

	// Find the enclosing function definition.
	defIdx := -1
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Kind == parser.TokEOF || tok.Start >= offset {
			break
		}
		if isFunctionDefinitionToken(tok.Kind) {
			defIdx = i
		}
	}
	if defIdx < 0 {
		return nil
	}

	result := make(map[string]string)

	// Typespec inference: look backward from the def for a preceding @spec.
	// Parse parameter types and match positionally to function param names.
	specTypes := parseSpecParamTypes(tokens, source, defIdx)
	paramNames := parseFunctionParamNames(tokens, source, defIdx)
	if len(specTypes) == len(paramNames) && len(specTypes) > 0 {
		for i, specType := range specTypes {
			if specType != "" {
				// Only set if not already overridden by pattern match (added later)
				result[paramNames[i]] = specType
			}
		}
	}

	// Scan tokens from the function definition to the cursor.
	// We look for two patterns:
	//   Pattern A: %Module{...} = var   (struct on left, variable on right of =)
	//   Pattern B: var = %Module{...}   (variable on left, struct on right of =)
	for i := defIdx + 1; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Kind == parser.TokEOF || tok.Start >= offset {
			break
		}

		// Look for % which starts a struct literal
		if tok.Kind == parser.TokPercent {
			// Collect the module name after %
			j := tokNextSig(tokens, len(tokens), i+1)
			if j >= len(tokens) || tokens[j].Kind != parser.TokModule {
				continue
			}
			moduleRef, k := tokCollectModuleName(source, tokens, len(tokens), j)
			if moduleRef == "" {
				continue
			}

			// Find the matching close brace (or accept no brace for patterns like %User{} = var)
			braceIdx := tokNextSig(tokens, len(tokens), k)
			if braceIdx >= len(tokens) || tokens[braceIdx].Start >= offset {
				continue
			}

			// Must have an open brace to be a struct literal
			if tokens[braceIdx].Kind != parser.TokOpenBrace {
				continue
			}

			// Skip past the struct body to find the closing brace
			closeIdx := findMatchingCloseBrace(tokens, braceIdx)
			if closeIdx < 0 {
				continue
			}

			// Pattern A: %Module{...} = var (or %Module{...} = var = ...)
			// Look for = after the struct, then a variable
			afterClose := tokNextSig(tokens, len(tokens), closeIdx+1)
			if afterClose < len(tokens) && tokens[afterClose].Start < offset &&
				tokens[afterClose].Kind == parser.TokOther && tokenText(source, tokens[afterClose]) == "=" {
				// Look for variable after =
				varIdx := tokNextSig(tokens, len(tokens), afterClose+1)
				if varIdx < len(tokens) && tokens[varIdx].Start < offset && tokens[varIdx].Kind == parser.TokIdent {
					varName := parser.TokenText(source, tokens[varIdx])
					if !strings.HasPrefix(varName, "_") && !parser.IsElixirKeyword(varName) {
						// Exclude function calls (ident followed by open paren)
						nextAfterVar := tokNextSig(tokens, len(tokens), varIdx+1)
						if nextAfterVar < len(tokens) && tokens[nextAfterVar].Kind == parser.TokOpenParen {
							// This is a function call like get_user(), not a variable
						} else {
							result[varName] = moduleRef
						}
					}
				}
			}

			i = closeIdx
			continue
		}

		// Pattern B: var = %Module{...} or var \\ %Module{...} (default arg)
		if tok.Kind == parser.TokIdent {
			varName := parser.TokenText(source, tok)
			if strings.HasPrefix(varName, "_") || parser.IsElixirKeyword(varName) {
				continue
			}

			// Check if next significant token is = or \\
			eqIdx := tokNextSig(tokens, len(tokens), i+1)
			if eqIdx >= len(tokens) || tokens[eqIdx].Start >= offset {
				continue
			}
			isEquals := tokens[eqIdx].Kind == parser.TokOther && tokenText(source, tokens[eqIdx]) == "="
			isDefault := tokens[eqIdx].Kind == parser.TokBackslash
			if !isEquals && !isDefault {
				continue
			}

			// Check if next significant token after = is %
			pctIdx := tokNextSig(tokens, len(tokens), eqIdx+1)
			if pctIdx >= len(tokens) || tokens[pctIdx].Start >= offset {
				continue
			}
			if tokens[pctIdx].Kind != parser.TokPercent {
				continue
			}

			// Collect module name
			modIdx := tokNextSig(tokens, len(tokens), pctIdx+1)
			if modIdx >= len(tokens) || tokens[modIdx].Kind != parser.TokModule {
				continue
			}
			moduleRef, k := tokCollectModuleName(source, tokens, len(tokens), modIdx)
			if moduleRef == "" {
				continue
			}

			// Verify there's an open brace (confirms it's a struct literal, not just %Module)
			braceIdx := tokNextSig(tokens, len(tokens), k)
			if braceIdx >= len(tokens) || tokens[braceIdx].Kind != parser.TokOpenBrace {
				continue
			}

			result[varName] = moduleRef

			// Skip past the struct body
			closeIdx := findMatchingCloseBrace(tokens, braceIdx)
			if closeIdx >= 0 {
				i = closeIdx
			}
		}
	}

	return result
}

// knownNonStructTypes lists modules whose .t() type does not represent a struct.
var knownNonStructTypes = map[string]bool{
	"String":    true,
	"Integer":   true,
	"Float":     true,
	"Atom":      true,
	"BitString": true,
	"Reference": true,
	"Port":      true,
	"PID":       true,
	"Exception": true,
	"Macro":     true,
	"Macro.Env": true,
}

// parseSpecParamTypes looks backward from defIdx for a preceding @spec and
// extracts the struct-like types from it. Returns a slice where each element
// is the inferred module for that parameter position, or "" if not a struct type.
//
// Recognized patterns:
//   - t()         → "__MODULE__"
//   - Module.t()  → "Module" (unless it's a known non-struct type)
//   - anything else → ""
func parseSpecParamTypes(tokens []parser.Token, source []byte, defIdx int) []string {
	// Walk backward from defIdx to find the nearest preceding @spec.
	// Skip all tokens until we find @spec, another def, or a module-level boundary.
	specIdx := -1
	for i := defIdx - 1; i >= 0; i-- {
		tok := tokens[i]
		if tok.Kind == parser.TokAttrSpec {
			specIdx = i
			break
		}
		if isFunctionDefinitionToken(tok.Kind) || tok.Kind == parser.TokDefmodule || tok.Kind == parser.TokEnd {
			break
		}
	}
	if specIdx < 0 {
		return nil
	}

	// After @spec, we expect: func_name ( param_types ) :: return_type
	// Find the open paren of the spec
	j := tokNextSig(tokens, len(tokens), specIdx+1)
	if j >= len(tokens) || tokens[j].Kind != parser.TokIdent {
		return nil
	}

	parenIdx := tokNextSig(tokens, len(tokens), j+1)
	if parenIdx >= len(tokens) || tokens[parenIdx].Kind != parser.TokOpenParen {
		return nil
	}

	// Find the matching close paren
	closeIdx := findMatchingCloseParen(tokens, parenIdx)
	if closeIdx < 0 {
		return nil
	}

	// Check if there are no params (empty parens)
	first := tokNextSig(tokens, len(tokens), parenIdx+1)
	if first == closeIdx {
		return nil
	}

	// Parse each parameter type (comma-separated at depth 0)
	var paramTypes []string
	depth := 0
	typeStart := parenIdx + 1

	for i := parenIdx + 1; i <= closeIdx; i++ {
		tok := tokens[i]
		switch tok.Kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
			depth++
		case parser.TokCloseParen:
			if depth == 0 {
				// End of params — process last type
				paramTypes = append(paramTypes, classifySpecType(tokens, source, typeStart, i))
			} else {
				depth--
			}
		case parser.TokCloseBracket, parser.TokCloseBrace:
			depth--
		case parser.TokComma:
			if depth == 0 {
				paramTypes = append(paramTypes, classifySpecType(tokens, source, typeStart, i))
				typeStart = i + 1
			}
		}
	}

	return paramTypes
}

// classifySpecType examines the tokens from start (inclusive) to end (exclusive)
// and determines if it represents a struct type.
//
// Returns "__MODULE__" for bare t(), the module name for Module.t(),
// or "" for anything else.
func classifySpecType(tokens []parser.Token, source []byte, start, end int) string {
	// Collect significant tokens in this range
	var sigTokens []int
	for i := start; i < end; i++ {
		if tokens[i].Kind != parser.TokEOL && tokens[i].Kind != parser.TokComment {
			sigTokens = append(sigTokens, i)
		}
	}

	if len(sigTokens) == 0 {
		return ""
	}

	// Pattern: t()  — just TokIdent("t"), TokOpenParen, TokCloseParen
	if len(sigTokens) == 3 {
		if tokens[sigTokens[0]].Kind == parser.TokIdent &&
			parser.TokenText(source, tokens[sigTokens[0]]) == "t" &&
			tokens[sigTokens[1]].Kind == parser.TokOpenParen &&
			tokens[sigTokens[2]].Kind == parser.TokCloseParen {
			return "__MODULE__"
		}
	}

	// Pattern: Module.t() or Module.Sub.t()
	// Tokens: Module, Dot, ... , Dot, t, (, )
	// The last 4 tokens should be: Dot, Ident("t"), OpenParen, CloseParen
	if len(sigTokens) >= 5 {
		last := len(sigTokens) - 1
		if tokens[sigTokens[last]].Kind == parser.TokCloseParen &&
			tokens[sigTokens[last-1]].Kind == parser.TokOpenParen &&
			tokens[sigTokens[last-2]].Kind == parser.TokIdent &&
			parser.TokenText(source, tokens[sigTokens[last-2]]) == "t" &&
			tokens[sigTokens[last-3]].Kind == parser.TokDot &&
			tokens[sigTokens[0]].Kind == parser.TokModule {

			// Collect the module name from the leading tokens (everything before the last .t())
			moduleRef, _ := tokCollectModuleName(source, tokens, len(tokens), sigTokens[0])
			if moduleRef != "" && !knownNonStructTypes[moduleRef] {
				return moduleRef
			}
		}
	}

	return ""
}

// parseFunctionParamNames extracts parameter names from a function definition
// head starting at defIdx. Returns a slice of parameter names in order.
func parseFunctionParamNames(tokens []parser.Token, source []byte, defIdx int) []string {
	// After def/defp, expect: func_name ( params )
	funcIdx := tokNextSig(tokens, len(tokens), defIdx+1)
	if funcIdx >= len(tokens) || tokens[funcIdx].Kind != parser.TokIdent {
		return nil
	}

	parenIdx := tokNextSig(tokens, len(tokens), funcIdx+1)
	if parenIdx >= len(tokens) || tokens[parenIdx].Kind != parser.TokOpenParen {
		return nil
	}

	closeIdx := findMatchingCloseParen(tokens, parenIdx)
	if closeIdx < 0 {
		return nil
	}

	// Check for empty parens
	first := tokNextSig(tokens, len(tokens), parenIdx+1)
	if first == closeIdx {
		return nil
	}

	// Parse comma-separated params at depth 0.
	// For each param, find the "root" identifier — the actual param name.
	// This handles patterns like: %User{} = user, user \\ default, user
	var names []string
	depth := 0
	paramStart := parenIdx + 1

	for i := parenIdx + 1; i <= closeIdx; i++ {
		tok := tokens[i]
		switch tok.Kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
			depth++
		case parser.TokCloseParen:
			if depth == 0 {
				names = append(names, extractParamName(tokens, source, paramStart, i))
			} else {
				depth--
			}
		case parser.TokCloseBracket, parser.TokCloseBrace:
			depth--
		case parser.TokComma:
			if depth == 0 {
				names = append(names, extractParamName(tokens, source, paramStart, i))
				paramStart = i + 1
			}
		}
	}

	return names
}

// extractParamName finds the parameter name from a function head parameter
// expression. Handles patterns like:
//   - user                           → "user"
//   - %User{} = user                 → "user"
//   - user \\ %User{}                → "user"
//   - %User{name: name} = user       → "user"
func extractParamName(tokens []parser.Token, source []byte, start, end int) string {
	// Strategy: find identifiers at depth 0 that aren't keywords and aren't
	// preceded by a dot. Prefer the one after = if present, otherwise the first one.
	var firstIdent string
	var afterEquals string
	sawEquals := false
	depth := 0

	for i := start; i < end; i++ {
		tok := tokens[i]
		switch tok.Kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
			depth++
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace:
			depth--
		case parser.TokOther:
			if depth == 0 && tokenText(source, tok) == "=" {
				sawEquals = true
			}
		case parser.TokBackslash:
			// default arg — the param name is whatever we already found
			if firstIdent != "" {
				return firstIdent
			}
		case parser.TokIdent:
			if depth != 0 {
				continue
			}
			name := parser.TokenText(source, tok)
			if strings.HasPrefix(name, "_") || parser.IsElixirKeyword(name) {
				continue
			}
			// Skip if preceded by dot (struct field access / module function)
			prev := prevSignificantToken(tokens, i)
			if prev >= 0 && tokens[prev].Kind == parser.TokDot {
				continue
			}
			// Skip if followed by open paren (function call)
			next := tokNextSig(tokens, len(tokens), i+1)
			if next < end && tokens[next].Kind == parser.TokOpenParen {
				continue
			}
			if firstIdent == "" {
				firstIdent = name
			}
			if sawEquals {
				afterEquals = name
			}
		}
	}

	if afterEquals != "" {
		return afterEquals
	}
	return firstIdent
}

// VariableFunctionCall describes a variable assigned from a module function call,
// e.g. `user = Accounts.get_user(id)`.
type VariableFunctionCall struct {
	VarName  string
	Module   string // unresolved, e.g. "Accounts"
	Function string
	Arity    int
	Line     int // 0-based line of the assignment
}

// VariableFunctionCalls scans from the enclosing function definition to the cursor
// and finds variables assigned from module function calls: `var = Module.func(...)`.
// Only detects simple top-level assignments, not nested or piped expressions.
func (tf *TokenizedFile) VariableFunctionCalls(line, col int) []VariableFunctionCall {
	return VariableFunctionCalls(tf.tokens, tf.source, tf.lineStarts, line, col)
}

func VariableFunctionCalls(tokens []parser.Token, source []byte, lineStarts []int, line, col int) []VariableFunctionCall {
	offset := parser.LineColToOffset(lineStarts, line, col)
	if offset < 0 {
		return nil
	}

	defIdx := -1
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Kind == parser.TokEOF || tok.Start >= offset {
			break
		}
		if isFunctionDefinitionToken(tok.Kind) {
			defIdx = i
		}
	}
	if defIdx < 0 {
		return nil
	}

	var results []VariableFunctionCall
	seen := make(map[string]int) // varName -> index in results (last wins)

	for i := defIdx + 1; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Kind == parser.TokEOF || tok.Start >= offset {
			break
		}

		// Look for: ident = Module.func(...)
		if tok.Kind != parser.TokIdent {
			continue
		}

		varName := parser.TokenText(source, tok)
		if strings.HasPrefix(varName, "_") || parser.IsElixirKeyword(varName) {
			continue
		}

		// Next must be =
		eqIdx := tokNextSig(tokens, len(tokens), i+1)
		if eqIdx >= len(tokens) || tokens[eqIdx].Start >= offset {
			continue
		}
		if tokens[eqIdx].Kind != parser.TokOther || tokenText(source, tokens[eqIdx]) != "=" {
			continue
		}

		// Next must be Module (uppercase)
		modIdx := tokNextSig(tokens, len(tokens), eqIdx+1)
		if modIdx >= len(tokens) || tokens[modIdx].Start >= offset {
			continue
		}
		if tokens[modIdx].Kind != parser.TokModule {
			continue
		}

		// Collect the full module name (e.g. "MyApp.Accounts")
		moduleRef, afterMod := tokCollectModuleName(source, tokens, len(tokens), modIdx)
		if moduleRef == "" {
			continue
		}

		// Next must be . then function name
		dotIdx := tokNextSig(tokens, len(tokens), afterMod)
		if dotIdx >= len(tokens) || tokens[dotIdx].Kind != parser.TokDot {
			continue
		}

		funcIdx := tokNextSig(tokens, len(tokens), dotIdx+1)
		if funcIdx >= len(tokens) || tokens[funcIdx].Start >= offset {
			continue
		}
		if tokens[funcIdx].Kind != parser.TokIdent {
			continue
		}
		funcName := parser.TokenText(source, tokens[funcIdx])

		// Check if next token is ( for parenthesized call, or an argument for no-paren call
		nextIdx := tokNextSig(tokens, len(tokens), funcIdx+1)
		var arity int
		var skipTo int

		if nextIdx < len(tokens) && tokens[nextIdx].Kind == parser.TokOpenParen {
			// Parenthesized call: count args inside parens
			arity = countCallArity(tokens, source, nextIdx)
			closeIdx := findMatchingCloseParen(tokens, nextIdx)
			if closeIdx >= 0 {
				skipTo = closeIdx
			} else {
				skipTo = nextIdx
			}
		} else if nextIdx < len(tokens) && isCallArgStartToken(tokens[nextIdx].Kind) {
			// No-paren call: count args until newline or closing delimiter
			arity, skipTo = countNoParenCallArity(tokens, nextIdx)
		} else {
			continue
		}

		call := VariableFunctionCall{
			VarName:  varName,
			Module:   moduleRef,
			Function: funcName,
			Arity:    arity,
			Line:     tok.Line - 1,
		}

		if idx, ok := seen[varName]; ok {
			results[idx] = call
		} else {
			seen[varName] = len(results)
			results = append(results, call)
		}

		i = skipTo
	}

	return results
}

// isCallArgStartToken returns true if the token kind can start a function argument
// in a no-paren call. This excludes operators, closing delimiters, and newlines.
func isCallArgStartToken(k parser.TokenKind) bool {
	switch k {
	case parser.TokIdent, parser.TokModule, parser.TokAtom, parser.TokNumber,
		parser.TokString, parser.TokHeredoc, parser.TokSigil, parser.TokCharLiteral,
		parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace, parser.TokOpenAngle,
		parser.TokPercent, parser.TokAttr,
		parser.TokFn:
		return true
	default:
		return false
	}
}

// countNoParenCallArity counts arguments in a no-paren function call starting
// at firstArg. Arguments end at a newline, closing delimiter, or certain keywords.
// Returns the arity and the index of the last token in the call.
func countNoParenCallArity(tokens []parser.Token, firstArg int) (int, int) {
	depth := 0
	commas := 0
	lastIdx := firstArg

	for i := firstArg; i < len(tokens); i++ {
		tok := tokens[i]
		switch tok.Kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
			depth++
			lastIdx = i
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace:
			if depth == 0 {
				// Hit an outer closing delimiter — end of call
				return commas + 1, lastIdx
			}
			depth--
			lastIdx = i
		case parser.TokComma:
			if depth == 0 {
				commas++
			}
			lastIdx = i
		case parser.TokEOL:
			if depth == 0 {
				return commas + 1, lastIdx
			}
		case parser.TokEOF:
			return commas + 1, lastIdx
		case parser.TokDo:
			if depth == 0 {
				return commas + 1, lastIdx
			}
		default:
			lastIdx = i
		}
	}
	return commas + 1, lastIdx
}

// countCallArity counts the number of arguments in a function call starting
// at the open paren. Returns 0 for empty parens, 1+ for calls with arguments.
func countCallArity(tokens []parser.Token, source []byte, openParen int) int {
	depth := 1
	hasContent := false
	commas := 0
	for i := openParen + 1; i < len(tokens); i++ {
		switch tokens[i].Kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
			depth++
			hasContent = true
		case parser.TokCloseParen:
			depth--
			if depth == 0 {
				if hasContent {
					return commas + 1
				}
				return 0
			}
		case parser.TokCloseBracket, parser.TokCloseBrace:
			depth--
		case parser.TokComma:
			if depth == 1 {
				commas++
			}
			hasContent = true
		case parser.TokEOF:
			return 0
		default:
			if !isWhitespaceToken(tokens[i].Kind) {
				hasContent = true
			}
		}
	}
	return 0
}

func isWhitespaceToken(k parser.TokenKind) bool {
	return k == parser.TokEOL || k == parser.TokComment
}

// findMatchingCloseParen finds the matching ) for the ( at tokens[openIdx].
func findMatchingCloseParen(tokens []parser.Token, openIdx int) int {
	depth := 1
	for i := openIdx + 1; i < len(tokens); i++ {
		switch tokens[i].Kind {
		case parser.TokOpenParen:
			depth++
		case parser.TokCloseParen:
			depth--
			if depth == 0 {
				return i
			}
		case parser.TokEOF:
			return -1
		}
	}
	return -1
}

// findMatchingCloseBrace finds the matching } for the { at tokens[openIdx].
// Returns -1 if not found.
func findMatchingCloseBrace(tokens []parser.Token, openIdx int) int {
	depth := 1
	for i := openIdx + 1; i < len(tokens); i++ {
		switch tokens[i].Kind {
		case parser.TokOpenBrace:
			depth++
		case parser.TokCloseBrace:
			depth--
			if depth == 0 {
				return i
			}
		case parser.TokEOF:
			return -1
		}
	}
	return -1
}

// tokenText returns the source text for a token as a string.
func tokenText(source []byte, tok parser.Token) string {
	return string(source[tok.Start:tok.End])
}

func VariableNamesBeforeCursor(tokens []parser.Token, source []byte, lineStarts []int, line, col int) []string {
	offset := parser.LineColToOffset(lineStarts, line, col)
	if offset < 0 {
		return nil
	}

	defIdx := -1
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Kind == parser.TokEOF || tok.Start >= offset {
			break
		}
		if isFunctionDefinitionToken(tok.Kind) {
			defIdx = i
		}
	}
	if defIdx < 0 {
		return nil
	}

	seen := make(map[string]bool)
	var names []string
	for i := defIdx + 1; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Kind == parser.TokEOF || tok.Start >= offset {
			break
		}
		if tok.Kind != parser.TokIdent {
			continue
		}
		name := parser.TokenText(source, tok)
		if strings.HasPrefix(name, "_") || parser.IsElixirKeyword(name) {
			continue
		}
		prev := prevSignificantToken(tokens, i)
		if prev >= 0 {
			if tokens[prev].Kind == parser.TokDot || isFunctionDefinitionToken(tokens[prev].Kind) {
				continue
			}
		}
		next := tokNextSig(tokens, len(tokens), i+1)
		if next < len(tokens) {
			if tokens[next].Kind == parser.TokColon {
				continue
			}
			if tokens[next].Kind == parser.TokOpenParen {
				continue
			}
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

func isFunctionDefinitionToken(kind parser.TokenKind) bool {
	switch kind {
	case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
		parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
		return true
	default:
		return false
	}
}

func StructValueContextAtCursor(tokens []parser.Token, source []byte, lineStarts []int, line, col int) bool {
	if line < 0 || line >= len(lineStarts) || col < 0 {
		return false
	}
	offset := parser.LineColToOffset(lineStarts, line, col)
	if offset < 0 {
		return false
	}

	openIdx := enclosingOpenBraceBeforeOffset(tokens, offset)
	if openIdx < 0 {
		return false
	}
	if _, ok := structModuleBeforeOpenBrace(tokens, source, openIdx); !ok {
		return false
	}

	return structValuePositionAtOffset(tokens, openIdx, offset)
}

// StructCompletionContextAtCursor returns struct-key completion context at the
// given 0-based line/column. It intentionally rejects value positions, so
// `%User{name: |}` does not ask for field completions.
func StructCompletionContextAtCursor(tokens []parser.Token, source []byte, lineStarts []int, line, col int) (StructCompletionContext, bool) {
	if line < 0 || line >= len(lineStarts) || col < 0 {
		return StructCompletionContext{}, false
	}
	offset := parser.LineColToOffset(lineStarts, line, col)
	if offset < 0 {
		return StructCompletionContext{}, false
	}

	openIdx := enclosingOpenBraceBeforeOffset(tokens, offset)
	if openIdx < 0 {
		return StructCompletionContext{}, false
	}

	moduleRef, ok := structModuleBeforeOpenBrace(tokens, source, openIdx)
	if !ok {
		return StructCompletionContext{}, false
	}

	fieldPrefix, startOffset, ok := structFieldPrefixAtOffset(tokens, source, openIdx, offset)
	if !ok {
		return StructCompletionContext{}, false
	}

	return StructCompletionContext{
		ModuleRef:   moduleRef,
		FieldPrefix: fieldPrefix,
		StartCol:    startOffset - lineStarts[line],
	}, true
}

func enclosingOpenBraceBeforeOffset(tokens []parser.Token, offset int) int {
	depth := 0
	for i := len(tokens) - 1; i >= 0; i-- {
		tok := tokens[i]
		if tok.Kind == parser.TokEOF || tok.Start >= offset {
			continue
		}
		switch tok.Kind {
		case parser.TokCloseBrace:
			depth++
		case parser.TokOpenBrace:
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

func prevSignificantToken(tokens []parser.Token, before int) int {
	for i := before - 1; i >= 0; i-- {
		switch tokens[i].Kind {
		case parser.TokEOL, parser.TokComment:
			continue
		default:
			return i
		}
	}
	return -1
}

func structModuleBeforeOpenBrace(tokens []parser.Token, source []byte, openIdx int) (string, bool) {
	endIdx := prevSignificantToken(tokens, openIdx)
	if endIdx < 0 || tokens[endIdx].Kind != parser.TokModule {
		return "", false
	}

	startIdx := endIdx
	for startIdx >= 2 && tokens[startIdx-1].Kind == parser.TokDot && tokens[startIdx-2].Kind == parser.TokModule {
		startIdx -= 2
	}

	percentIdx := prevSignificantToken(tokens, startIdx)
	if percentIdx < 0 || tokens[percentIdx].Kind != parser.TokPercent {
		return "", false
	}

	moduleRef, nextIdx := tokCollectModuleName(source, tokens, len(tokens), startIdx)
	if moduleRef == "" || nextIdx != openIdx {
		return "", false
	}
	return moduleRef, true
}

func structFieldPrefixAtOffset(tokens []parser.Token, source []byte, openIdx, offset int) (string, int, bool) {
	segmentStartOffset := tokens[openIdx].End
	parenDepth, bracketDepth, braceDepth := 0, 0, 0
	inValue := false

	for i := openIdx + 1; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Kind == parser.TokEOF || tok.Start >= offset {
			break
		}

		if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 {
			switch tok.Kind {
			case parser.TokComma:
				segmentStartOffset = tok.End
				inValue = false
				continue
			case parser.TokColon, parser.TokAssoc:
				inValue = true
				continue
			case parser.TokOther:
				if parser.TokenText(source, tok) == "|" && !inValue {
					segmentStartOffset = tok.End
					continue
				}
			}
		}

		switch tok.Kind {
		case parser.TokOpenParen:
			parenDepth++
		case parser.TokCloseParen:
			if parenDepth > 0 {
				parenDepth--
			}
		case parser.TokOpenBracket:
			bracketDepth++
		case parser.TokCloseBracket:
			if bracketDepth > 0 {
				bracketDepth--
			}
		case parser.TokOpenBrace:
			braceDepth++
		case parser.TokCloseBrace:
			if braceDepth == 0 {
				return "", 0, false
			}
			braceDepth--
		}
	}

	if parenDepth != 0 || bracketDepth != 0 || braceDepth != 0 {
		return "", 0, false
	}
	if inValue {
		return "", 0, false
	}
	if segmentStartOffset == tokens[openIdx].End && hasTopLevelStructUpdatePipeAhead(tokens, source, openIdx, offset) {
		return "", 0, false
	}

	if offset > 0 {
		if idx := parser.TokenAtOffset(tokens, offset-1); idx >= 0 {
			tok := tokens[idx]
			if tok.Start >= segmentStartOffset && tok.Kind == parser.TokIdent {
				end := offset
				if end > tok.End {
					end = tok.End
				}
				if end > tok.Start {
					return string(source[tok.Start:end]), tok.Start, true
				}
			}
			switch tok.Kind {
			case parser.TokOpenBrace, parser.TokComma, parser.TokPipe, parser.TokEOL, parser.TokComment:
				return "", offset, true
			}
		}
	}

	return "", offset, true
}

func structValuePositionAtOffset(tokens []parser.Token, openIdx, offset int) bool {
	parenDepth, bracketDepth, braceDepth := 0, 0, 0
	inValue := false

	for i := openIdx + 1; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Kind == parser.TokEOF || tok.Start >= offset {
			break
		}

		if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 {
			switch tok.Kind {
			case parser.TokComma:
				inValue = false
				continue
			case parser.TokColon, parser.TokAssoc:
				inValue = true
				continue
			case parser.TokCloseBrace:
				return false
			}
		}

		switch tok.Kind {
		case parser.TokOpenParen:
			parenDepth++
		case parser.TokCloseParen:
			if parenDepth > 0 {
				parenDepth--
			}
		case parser.TokOpenBracket:
			bracketDepth++
		case parser.TokCloseBracket:
			if bracketDepth > 0 {
				bracketDepth--
			}
		case parser.TokOpenBrace:
			braceDepth++
		case parser.TokCloseBrace:
			if braceDepth == 0 {
				return false
			}
			braceDepth--
		}
	}

	return inValue && parenDepth == 0 && bracketDepth == 0 && braceDepth == 0
}

func hasTopLevelStructUpdatePipeAhead(tokens []parser.Token, source []byte, openIdx, offset int) bool {
	parenDepth, bracketDepth, braceDepth := 0, 0, 0
	for i := openIdx + 1; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.Kind == parser.TokEOF {
			return false
		}
		if tok.Start < offset {
			continue
		}

		if parenDepth == 0 && bracketDepth == 0 && braceDepth == 0 {
			switch tok.Kind {
			case parser.TokColon, parser.TokAssoc, parser.TokComma, parser.TokCloseBrace:
				return false
			case parser.TokOther:
				if parser.TokenText(source, tok) == "|" {
					return true
				}
			}
		}

		switch tok.Kind {
		case parser.TokOpenParen:
			parenDepth++
		case parser.TokCloseParen:
			if parenDepth > 0 {
				parenDepth--
			}
		case parser.TokOpenBracket:
			bracketDepth++
		case parser.TokCloseBracket:
			if bracketDepth > 0 {
				bracketDepth--
			}
		case parser.TokOpenBrace:
			braceDepth++
		case parser.TokCloseBrace:
			if braceDepth > 0 {
				braceDepth--
			}
		}
	}
	return false
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

func tokParseModuleDef(source []byte, tokens []parser.Token, from int, currentModule string) (name string, nextPos int, hasDo bool) {
	n := len(tokens)
	j := tokNextSig(tokens, n, from)
	name, k := tokCollectModuleName(source, tokens, n, j)
	if name == "" {
		return "", from, false
	}
	if !strings.Contains(name, ".") && currentModule != "" {
		name = currentModule + "." + name
	}
	_, nextPos, hasDo = parser.ScanForwardToBlockDo(tokens, n, k)
	return name, nextPos, hasDo
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
			stack = append(stack, moduleFrame{name, depth})
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
			j := tokNextSig(tokens, n, i+1)
			if j >= n || tokens[j].Kind != parser.TokIdent {
				continue
			}
			name := parser.TokenText(source, tokens[j])
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
			j := tokNextSig(tokens, n, i+1)
			if j >= n || tokens[j].Kind != parser.TokIdent {
				continue
			}
			name := parser.TokenText(source, tokens[j])
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
	}

	var stack []moduleFrame
	depth := 0

	type aliasEntry struct {
		scope, short, full string
	}
	var allAliases []aliasEntry
	var targetModule string
	unscoped := targetLine < 0
	// targetLine is 0-based; token.Line is 1-based
	targetLine1 := targetLine + 1

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

	processModuleDef := func(i int) int {
		name, nextPos, hasDo := tokParseModuleDef(source, tokens, i, currentModule())
		if name == "" {
			return i
		}
		if hasDo {
			depth++
			stack = append(stack, moduleFrame{name, depth})
		}
		return nextPos
	}

	for i := 0; i < n; i++ {
		tok := tokens[i]

		// Track target line's module scope (check before any depth changes)
		if !unscoped && targetModule == "" && tok.Line >= targetLine1 {
			targetModule = currentModule()
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
			cm := currentModule()
			j := tokNextSig(tokens, n, i+1)
			modName, k := tokCollectModuleName(source, tokens, n, j)
			if modName == "" {
				continue
			}

			// Multi-alias: alias Parent.{A, B, C}
			if children, nextPos, ok := parser.ScanMultiAliasChildren(source, tokens, n, k, true); ok {
				base := resolve(modName)
				if strings.Contains(base, "__MODULE__") {
					continue
				}
				for _, child := range children {
					allAliases = append(allAliases, aliasEntry{cm, parser.AliasShortName(child), base + "." + child})
				}
				i = nextPos - 1
				continue
			}

			// Check for alias Module, as: Name
			if asName, nextPos, ok := parser.ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
				resolved := resolve(modName)
				if !strings.Contains(resolved, "__MODULE__") {
					allAliases = append(allAliases, aliasEntry{cm, asName, resolved})
				}
				i = nextPos - 1
				continue
			}

			// Simple alias
			resolved := resolve(modName)
			if !strings.Contains(resolved, "__MODULE__") {
				allAliases = append(allAliases, aliasEntry{cm, parser.AliasShortName(resolved), resolved})
			}
			i = k - 1

		case parser.TokRequire:
			cm := currentModule()
			j := tokNextSig(tokens, n, i+1)
			modName, k := tokCollectModuleName(source, tokens, n, j)
			if modName == "" {
				continue
			}

			// Check for require Module, as: Name
			if asName, nextPos, ok := parser.ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
				resolved := resolve(modName)
				if !strings.Contains(resolved, "__MODULE__") {
					allAliases = append(allAliases, aliasEntry{cm, asName, resolved})
				}
				i = nextPos - 1
				continue
			}
			i = k - 1
		}
	}

	// If targetLine was past all tokens, resolve now
	if !unscoped && targetModule == "" {
		targetModule = currentModule()
	}

	aliases := make(map[string]string)
	for _, a := range allAliases {
		if unscoped || a.scope == targetModule {
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
	source := []byte(strings.Join(lines, "\n"))
	tokens := parser.Tokenize(source)
	n := len(tokens)

	resolveAlias := func(modName string) string {
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
		j := tokNextSig(tokens, n, i+1)
		if j < n && tokens[j].Kind == parser.TokIdent && string(source[tokens[j].Start:tokens[j].End]) == helperName {
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
				j := tokNextSig(tokens, n, i+1)
				if j < n && tokens[j].Kind == parser.TokDo {
					quoteBodyStart = j + 1
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

	// Walk the quote do block (depth 1 = inside quote do, 0 = we hit its end)
	inlineDefs = make(map[string][]inlineDef)
	depth = 1
	for i := quoteBodyStart; i < n && depth > 0; i++ {
		tok := tokens[i]
		parser.TrackBlockDepth(tok.Kind, &depth)
		switch tok.Kind {

		case parser.TokImport:
			j := tokNextSig(tokens, n, i+1)
			mod, _ := tokCollectModuleName(source, tokens, n, j)
			if mod != "" {
				imported = append(imported, resolveAlias(mod))
			}

		case parser.TokUse:
			j := tokNextSig(tokens, n, i+1)
			mod, _ := tokCollectModuleName(source, tokens, n, j)
			if mod != "" {
				transUses = append(transUses, resolveAlias(mod))
			}

		case parser.TokAlias:
			j := tokNextSig(tokens, n, i+1)
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

		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			kind := string(source[tok.Start:tok.End])
			defLine := tok.Line
			j := tokNextSig(tokens, n, i+1)
			if j >= n || tokens[j].Kind != parser.TokIdent {
				continue
			}
			funcName := string(source[tokens[j].Start:tokens[j].End])
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
			for arity := minArity; arity <= maxArity; arity++ {
				inlineDefs[funcName] = append(inlineDefs[funcName], inlineDef{
					line:   defLine,
					arity:  arity,
					kind:   kind,
					params: parser.JoinParams(paramNames, arity),
				})
			}
			i = skipToEndOfStatement(tokens, n, nextPos) - 1
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
	Module string            // the module being used (alias-resolved)
	Opts   map[string]string // keyword args: opt_key → module name (alias-resolved)
}

// ExtractUsesWithOpts parses all `use Module` and `use Module, key: Val`
// declarations, returning each as a UseCall. Aliases are resolved using the
// provided map. Handles opts spanning multiple lines via the tokenizer.
func ExtractUsesWithOpts(text string, aliases map[string]string) []UseCall {
	source := []byte(text)
	return extractUsesWithOptsFromTokens(source, parser.Tokenize(source), aliases)
}

func extractUsesWithOptsFromTokens(source []byte, tokens []parser.Token, aliases map[string]string) []UseCall {
	n := len(tokens)
	var calls []UseCall

	for i := 0; i < n; i++ {
		if tokens[i].Kind != parser.TokUse {
			continue
		}
		j := tokNextSig(tokens, n, i+1)
		modName, k := tokCollectModuleName(source, tokens, n, j)
		if modName == "" {
			continue
		}
		module := parser.ResolveModuleRef(modName, aliases, "")

		nk := tokNextSig(tokens, n, k)
		if nk < n && tokens[nk].Kind == parser.TokComma {
			opts := tokCollectKeywordModuleOpts(source, tokens, n, nk+1, aliases)
			calls = append(calls, UseCall{Module: module, Opts: opts})
		} else {
			calls = append(calls, UseCall{Module: module})
		}
		i = k
	}
	return calls
}

// tokCollectKeywordModuleOpts scans tokens starting at pos for keyword pairs
// like `key: ModuleName` and returns a map of key → resolved module name.
// Only includes entries whose value is a module (starts with uppercase).
func tokCollectKeywordModuleOpts(source []byte, tokens []parser.Token, n, pos int, aliases map[string]string) map[string]string {
	result := make(map[string]string)
	i := tokNextSig(tokens, n, pos)
	for i < n {
		tok := tokens[i]
		// Stop at EOL not followed by a continuation (keyword opt)
		if tok.Kind == parser.TokEOL {
			j := tokNextSig(tokens, n, i+1)
			if j >= n || tokens[j].Kind == parser.TokEOL || tokens[j].Kind == parser.TokEOF {
				break
			}
			// Check if next sig token is an ident followed by colon (keyword opt)
			if tokens[j].Kind == parser.TokIdent {
				jj := j + 1
				if jj < n && tokens[jj].Kind == parser.TokColon {
					i = j
					continue
				}
			}
			break
		}
		if tok.Kind == parser.TokEOF {
			break
		}
		// Match: ident colon Module
		if tok.Kind == parser.TokIdent {
			if i+1 < n && tokens[i+1].Kind == parser.TokColon {
				key := parser.TokenText(source, tok)
				k := tokNextSig(tokens, n, i+2)
				if k < n && tokens[k].Kind == parser.TokModule {
					modName, _ := tokCollectModuleName(source, tokens, n, k)
					if modName != "" {
						result[key] = parser.ResolveModuleRef(modName, aliases, "")
					}
				}
			}
		}
		i++
	}
	return result
}

// inlineDef records a function or macro defined directly inside a __using__
// quote do block. These definitions get injected into any module that `use`s
// the parent module.
type inlineDef struct {
	line   int // 1-based line number in the source file
	arity  int
	kind   string // "def", "defp", "defmacro", etc.
	params string // comma-separated parameter names
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

	for i := 0; i < n; i++ {
		tok := tokens[i]
		if tok.Kind == parser.TokDefmacro {
			j := nextSig(i + 1)
			if j < n && tokens[j].Kind == parser.TokIdent && string(source[tokens[j].Start:tokens[j].End]) == "__using__" {
				// Scan forward to find TokDo; Elixir allows split-line heads.
				if _, nextPos, hasDo := parser.ScanForwardToBlockDo(tokens, n, j+1); hasDo {
					usingBodyStart = nextPos
					usingDepth = 1 // inside the defmacro do..end
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
	if usingBodyStart < 0 {
		return
	}

	// Extract file-level aliases for resolution (reuse already-tokenized data)
	lines := strings.Split(text, "\n")
	fileAliases := extractAliasesFromTokens(source, tokens, -1)

	inlineDefs = make(map[string][]inlineDef)

	resolveAlias := func(modName string) string {
		if resolved := parser.ResolveModuleRef(modName, aliases, ""); resolved != modName {
			return resolved
		}
		return parser.ResolveModuleRef(modName, fileAliases, "")
	}

	type varBinding struct {
		optKey     string
		defaultMod string
	}
	varToOpt := make(map[string]varBinding)

	// scanKeywordCall checks if tokens starting at i match:
	//   Keyword.{get|pop|put|put_new|fetch|fetch!|pop!|pop_lazy}(ident, :key [, Default])
	// Returns (funcName, argIdent, atomKey, defaultModule, endPos) or empty strings if no match.
	scanKeywordCall := func(i int) (string, string, string, int) {
		// Expect: TokModule("Keyword") TokDot TokIdent(funcName) TokOpenParen
		if i+3 >= n {
			return "", "", "", i
		}
		if tokens[i].Kind != parser.TokModule || string(source[tokens[i].Start:tokens[i].End]) != "Keyword" {
			return "", "", "", i
		}
		if tokens[i+1].Kind != parser.TokDot {
			return "", "", "", i
		}
		if tokens[i+2].Kind != parser.TokIdent {
			return "", "", "", i
		}
		funcName := string(source[tokens[i+2].Start:tokens[i+2].End])
		j := nextSig(i + 3)
		if j >= n || tokens[j].Kind != parser.TokOpenParen {
			return "", "", "", i
		}
		j++ // skip (

		// Skip first argument (the opts variable) up to comma
		depth := 1
		for j < n && depth > 0 {
			switch tokens[j].Kind {
			case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
				depth++
			case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace:
				depth--
				if depth == 0 {
					return funcName, "", "", j + 1
				}
			case parser.TokComma:
				if depth == 1 {
					j++
					goto foundFirstComma
				}
			}
			j++
		}
		return funcName, "", "", j
	foundFirstComma:

		// Expect :atom_key
		j = nextSig(j)
		if j >= n || tokens[j].Kind != parser.TokAtom {
			return funcName, "", "", skipToEndOfStatement(tokens, n, j)
		}
		atomText := string(source[tokens[j].Start:tokens[j].End])
		atomKey := ""
		if len(atomText) > 1 && atomText[0] == ':' {
			atomKey = atomText[1:]
		}
		j++

		// Check for optional comma + default module
		j = nextSig(j)
		if j >= n {
			return funcName, atomKey, "", j
		}
		if tokens[j].Kind == parser.TokCloseParen {
			return funcName, atomKey, "", j + 1
		}
		if tokens[j].Kind == parser.TokComma {
			j = nextSig(j + 1)
			defaultMod, endJ := collectModuleName(j)
			if defaultMod != "" {
				// Advance to close paren
				for endJ < n && tokens[endJ].Kind != parser.TokCloseParen {
					endJ++
				}
				if endJ < n {
					endJ++
				}
				return funcName, atomKey, defaultMod, endJ
			}
		}
		// Skip to end
		return funcName, atomKey, "", skipToEndOfStatement(tokens, n, j)
	}

	// Walk tokens inside the __using__ body
	depth := usingDepth
	i := usingBodyStart
	for i < n && depth > 0 {
		tok := tokens[i]

		switch tok.Kind {
		case parser.TokDo, parser.TokFn, parser.TokEnd:
			parser.TrackBlockDepth(tok.Kind, &depth)
			i++
		case parser.TokEOL, parser.TokComment, parser.TokString, parser.TokHeredoc,
			parser.TokSigil, parser.TokAtom, parser.TokNumber, parser.TokCharLiteral,
			parser.TokEOF:
			i++

		case parser.TokImport:
			i++
			j := nextSig(i)
			// import unquote(var)
			if j < n && tokens[j].Kind == parser.TokIdent && string(source[tokens[j].Start:tokens[j].End]) == "unquote" {
				if j+2 < n && tokens[j+1].Kind == parser.TokOpenParen && tokens[j+2].Kind == parser.TokIdent {
					varName := source[tokens[j+2].Start:tokens[j+2].End]
					if b, ok := varToOpt[string(varName)]; ok {
						optBindings = append(optBindings, optBinding{optKey: b.optKey, defaultMod: b.defaultMod, kind: "import"})
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
			j := nextSig(i)
			// use unquote(var)
			if j < n && tokens[j].Kind == parser.TokIdent && string(source[tokens[j].Start:tokens[j].End]) == "unquote" {
				if j+2 < n && tokens[j+1].Kind == parser.TokOpenParen && tokens[j+2].Kind == parser.TokIdent {
					varName := source[tokens[j+2].Start:tokens[j+2].End]
					if b, ok := varToOpt[string(varName)]; ok {
						optBindings = append(optBindings, optBinding{optKey: b.optKey, defaultMod: b.defaultMod, kind: "use"})
					}
				}
				i = skipToEndOfStatement(tokens, n, j)
				continue
			}
			// use Module
			modName, k := collectModuleName(j)
			if modName != "" {
				transUses = append(transUses, resolveAlias(modName))
			}
			i = k

		case parser.TokAlias:
			i++
			j := nextSig(i)
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

		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			kind := string(source[tok.Start:tok.End])
			defLine := tok.Line
			i++
			j := nextSig(i)
			if j >= n || tokens[j].Kind != parser.TokIdent {
				i = j
				continue
			}
			funcName := string(source[tokens[j].Start:tokens[j].End])
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
			for arity := minArity; arity <= maxArity; arity++ {
				inlineDefs[funcName] = append(inlineDefs[funcName], inlineDef{
					line:   defLine,
					arity:  arity,
					kind:   kind,
					params: parser.JoinParams(paramNames, arity),
				})
			}
			i = skipToEndOfStatement(tokens, n, nextPos)
			continue

		case parser.TokModule:
			// Check for Keyword.put/put_new(opts, :key, Module) heuristic
			modText := string(source[tok.Start:tok.End])
			if modText == "Keyword" && i+2 < n && tokens[i+1].Kind == parser.TokDot && tokens[i+2].Kind == parser.TokIdent {
				funcName := string(source[tokens[i+2].Start:tokens[i+2].End])
				if funcName == "put" || funcName == "put_new" {
					_, atomKey, defaultMod, endJ := scanKeywordCall(i)
					if atomKey != "" && defaultMod != "" {
						transUses = append(transUses, resolveAlias(defaultMod))
					}
					i = endJ
					continue
				}
				if funcName == "get" || funcName == "pop" {
					_, atomKey, defaultMod, endJ := scanKeywordCall(i)
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
			isStmtStart := i == 0 || tokens[i-1].Kind == parser.TokEOL || tokens[i-1].Kind == parser.TokComment
			j := nextSig(i + 1)

			// Check for var = Keyword.{get,pop,put,put_new,...}(opts, :key, Default)
			// or var = ModuleName
			if isStmtStart && j < n && tokens[j].Kind == parser.TokOther && string(source[tokens[j].Start:tokens[j].End]) == "=" {
				k := nextSig(j + 1)
				if k < n && tokens[k].Kind == parser.TokModule && string(source[tokens[k].Start:tokens[k].End]) == "Keyword" {
					funcName, atomKey, defaultMod, endJ := scanKeywordCall(k)
					switch funcName {
					case "get", "pop", "pop!":
						if atomKey != "" {
							varToOpt[identName] = varBinding{optKey: atomKey, defaultMod: resolveAlias(defaultMod)}
						}
					case "fetch", "fetch!", "pop_lazy":
						if atomKey != "" {
							varToOpt[identName] = varBinding{optKey: atomKey}
						}
					case "put", "put_new":
						if atomKey != "" && defaultMod != "" {
							transUses = append(transUses, resolveAlias(defaultMod))
						}
					}
					i = endJ
					continue
				}
				// var = ModuleName
				if k < n && tokens[k].Kind == parser.TokModule {
					modName, endK := collectModuleName(k)
					if modName != "" {
						// Check it's a simple assignment (next sig token is EOL or EOF)
						peek := nextSig(endK)
						if peek >= n || tokens[peek].Kind == parser.TokEOL || tokens[peek].Kind == parser.TokEOF {
							varToOpt[identName] = varBinding{defaultMod: resolveAlias(modName)}
							i = endK
							continue
						}
					}
				}
			}
			// Check for bare function call that delegates to a helper:
			// helper_name(opts) where helper_name is a def/defp in the same file.
			// Only at statement start to avoid matching function calls inside expressions.
			if isStmtStart && j < n && tokens[j].Kind == parser.TokOpenParen && !parser.IsElixirKeyword(identName) {
				helperImported, helperDefs, helperTransUses, helperBindings, helperAliases := parseHelperQuoteBlock(lines, identName, fileAliases)
				if helperImported != nil {
					imported = append(imported, helperImported...)
					for hk, hv := range helperDefs {
						inlineDefs[hk] = append(inlineDefs[hk], hv...)
					}
					transUses = append(transUses, helperTransUses...)
					optBindings = append(optBindings, helperBindings...)
				}
				for hk, hv := range helperAliases {
					if aliases == nil {
						aliases = make(map[string]string)
					}
					aliases[hk] = hv
				}
				i = skipToEndOfStatement(tokens, n, i)
				continue
			}
			i++

		case parser.TokOpenBrace:
			// Check for {var, _} = Keyword.pop(opts, :key, Default)
			j := nextSig(i + 1)
			if j < n && tokens[j].Kind == parser.TokIdent {
				varName := string(source[tokens[j].Start:tokens[j].End])
				// Scan forward to find } = Keyword.pop pattern
				k := j + 1
				braceDepth := 1
				for k < n && braceDepth > 0 {
					switch tokens[k].Kind {
					case parser.TokOpenBrace:
						braceDepth++
					case parser.TokCloseBrace:
						braceDepth--
					}
					k++
				}
				// k is now past }
				eq := nextSig(k)
				if eq < n && tokens[eq].Kind == parser.TokOther && string(source[tokens[eq].Start:tokens[eq].End]) == "=" {
					kw := nextSig(eq + 1)
					if kw < n && tokens[kw].Kind == parser.TokModule && string(source[tokens[kw].Start:tokens[kw].End]) == "Keyword" {
						funcName, atomKey, defaultMod, endJ := scanKeywordCall(kw)
						if (funcName == "pop" || funcName == "pop!") && atomKey != "" {
							varToOpt[string(varName)] = varBinding{optKey: atomKey, defaultMod: resolveAlias(defaultMod)}
						} else if (funcName == "fetch" || funcName == "fetch!" || funcName == "pop_lazy") && atomKey != "" {
							varToOpt[string(varName)] = varBinding{optKey: atomKey}
						}
						i = endJ
						continue
					}
				}
			}
			i++

		default:
			i++
		}
	}
	return
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
	return ModuleAttributeAtCursor(tf.tokens, tf.source, tf.lineStarts, line, col)
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

// FindBareFunctionCalls scans text for unqualified calls to functionName,
// including direct calls like functionName(...) and pipe calls like |> functionName.
// Returns 1-based line numbers. Definition lines are excluded.
func FindBareFunctionCalls(text string, functionName string) []int {
	source := []byte(text)
	tokens := parser.Tokenize(source)
	n := len(tokens)

	seenLines := make(map[int]bool)
	defLines := make(map[int]bool)

	// First pass: identify definition lines to exclude
	for i := 0; i < n; i++ {
		tok := tokens[i]
		switch tok.Kind {
		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			j := tokNextSig(tokens, n, i+1)
			if j < n && tokens[j].Kind == parser.TokIdent {
				if parser.TokenText(source, tokens[j]) == functionName {
					defLines[tok.Line] = true
				}
			}
		case parser.TokAttrSpec, parser.TokAttrCallback:
			// Skip @spec and @callback lines that define this function
			j := tokNextSig(tokens, n, i+1)
			if j < n && tokens[j].Kind == parser.TokIdent {
				if parser.TokenText(source, tokens[j]) == functionName {
					defLines[tok.Line] = true
				}
			}
		}
	}

	// Second pass: find bare calls
	for i := 0; i < n; i++ {
		tok := tokens[i]

		if tok.Kind != parser.TokIdent {
			continue
		}
		if parser.TokenText(source, tok) != functionName {
			continue
		}
		if defLines[tok.Line] {
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
			j := tokNextSig(tokens, n, i+1)
			if j >= n || tokens[j].Kind != parser.TokIdent {
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
