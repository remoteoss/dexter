package lsp

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/remoteoss/dexter/internal/parser"
)

func isExprChar(b byte) bool {
	c := rune(b)
	return unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '.' || c == '?' || c == '!'
}

// ExtractExpression returns the dotted expression up to and including the
// segment the cursor is on. Line is the text content, col is a 0-based
// character offset.
//
// Examples (cursor position marked with |):
//
//	"MyApp.Re|po.all"  →  "MyApp.Repo"
//	"MyApp.Repo.a|ll"  →  "MyApp.Repo.all"
//	"Ti|ger.Repo.all"  →  "MyApp"
//	"MyApp|.Repo.all"  →  "MyApp.Repo"   (cursor on dot → include next segment)
func ExtractExpression(line string, col int) string {
	expr, _ := extractExpressionBounds(line, col)
	return expr
}

// extractExpressionBounds returns the same expression as ExtractExpression plus
// the start column (0-based) of that expression within the line. Returns ("", 0)
// when there is no expression at the cursor position.
func extractExpressionBounds(line string, col int) (expr string, startCol int) {
	if len(line) == 0 {
		return "", 0
	}
	if col >= len(line) {
		col = len(line) - 1
	}
	if col < 0 {
		col = 0
	}
	if !isExprChar(line[col]) {
		return "", 0
	}

	start := col
	for start > 0 && isExprChar(line[start-1]) {
		start--
	}
	end := col
	for end+1 < len(line) && isExprChar(line[end+1]) {
		end++
	}

	fullExpr := line[start : end+1]
	cursorOffset := col - start
	searchFrom := cursorOffset
	if fullExpr[searchFrom] == '.' {
		searchFrom++
	}
	nextDot := strings.IndexByte(fullExpr[searchFrom:], '.')
	if nextDot == -1 {
		return fullExpr, start
	}
	return fullExpr[:searchFrom+nextDot], start
}

// ExtractFullExpression returns the complete dotted expression at the cursor
// position without truncating at the cursor's segment. Unlike ExtractExpression
// which returns "DocuSign" when the cursor is on "DocuSign.Client.request",
// this returns the entire "DocuSign.Client.request".
func ExtractFullExpression(line string, col int) string {
	if len(line) == 0 || col < 0 {
		return ""
	}
	if col >= len(line) {
		col = len(line) - 1
	}
	if !isExprChar(line[col]) {
		return ""
	}
	// Reuse the same boundary scan as extractExpressionBounds, but pass col
	// at the end of the expression so no truncation occurs.
	end := col
	for end+1 < len(line) && isExprChar(line[end+1]) {
		end++
	}
	expr, _ := extractExpressionBounds(line, end)
	return expr
}

// ExtractModuleAndFunction splits a dotted expression into module reference and optional function name.
// Uppercase-starting parts are module segments, the first lowercase part is the function.
// Returns ("Foo.Bar", "baz") for "Foo.Bar.baz", ("Foo.Bar.Baz", "") for "Foo.Bar.Baz",
// ("", "do_something") for "do_something".
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
	// matching "}" on the same line.  Pure string ops, no allocations in
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

	// If the current line is just a closing brace (e.g. "  }"), we're past the block.
	// But if it has module content before the brace (e.g. "  Services.MakePayment }"),
	// we're still inside the alias block on the last line.
	currentLine := strings.TrimSpace(parser.StripCommentsAndStrings(lines[targetLine]))
	if strings.Contains(currentLine, "}") {
		withoutBrace := strings.TrimSpace(strings.Replace(currentLine, "}", "", 1))
		withoutBrace = strings.TrimRight(withoutBrace, ", ")
		if withoutBrace == "" {
			return "", false
		}
	}

	// Scan backward from the current line looking for the opening "alias Parent.{"
	for i := targetLine; i >= 0; i-- {
		line := lines[i]
		stripped := strings.TrimSpace(parser.StripCommentsAndStrings(line))

		// If we encounter a closing brace scanning backward, we're not in an open block
		if i < targetLine && strings.Contains(stripped, "}") {
			return "", false
		}

		// Look for "alias Module.{" pattern
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "alias ") {
			// Skip blank/comment lines
			if stripped == "" {
				continue
			}
			// Any other statement means we've left the alias context
			continue
		}

		// Found an alias line — check if it opens a brace block
		afterAlias := strings.TrimSpace(trimmed[6:])
		moduleName := parser.ScanModuleName(afterAlias)
		if moduleName == "" {
			return "", false
		}
		remaining := afterAlias[len(moduleName):]
		remainingStripped := strings.TrimSpace(parser.StripCommentsAndStrings(remaining))

		if !strings.HasPrefix(remainingStripped, "{") {
			return "", false
		}
		// Has opening { — check that } is NOT on this same line
		if strings.Contains(remainingStripped, "}") {
			return "", false
		}

		// We're inside a multi-line alias block — resolve the parent module
		parent := strings.TrimRight(moduleName, ".")

		// Resolve __MODULE__
		enclosingModule := extractEnclosingModule(lines, i)
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

// extractEnclosingModule finds the innermost defmodule enclosing the given line.
func extractEnclosingModule(lines []string, targetLine int) string {
	type moduleFrame struct {
		name  string
		depth int
	}
	var stack []moduleFrame
	depth := 0
	inHeredoc := false

	for i := 0; i <= targetLine && i < len(lines); i++ {
		var skip bool
		inHeredoc, skip = parser.CheckHeredoc(lines[i], inHeredoc)
		if skip {
			continue
		}
		stripped := strings.TrimSpace(parser.StripCommentsAndStrings(strings.TrimSpace(lines[i])))

		if parser.IsEnd(stripped) {
			if len(stack) > 0 && stack[len(stack)-1].depth == depth {
				stack = stack[:len(stack)-1]
			}
			depth--
			if depth < 0 {
				depth = 0
			}
		}
		if parser.OpensBlock(stripped) {
			depth++
		}
		if m := parser.DefmoduleRe.FindStringSubmatch(strings.TrimSpace(lines[i])); m != nil {
			name := m[1]
			if !strings.Contains(name, ".") && len(stack) > 0 {
				name = stack[len(stack)-1].name + "." + name
			}
			stack = append(stack, moduleFrame{name, depth})
		}
	}

	if len(stack) > 0 {
		return stack[len(stack)-1].name
	}
	return ""
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
	seen := make(map[string]bool)
	var results []BufferFunction
	for _, line := range strings.Split(text, "\n") {
		if m := parser.FuncDefRe.FindStringSubmatch(line); m != nil {
			name := m[2]
			paramContent := parser.FindParamContent(line, name)
			maxArity := parser.ArityFromParams(paramContent)
			minArity := maxArity - parser.DefaultsFromParams(paramContent)
			allParamNames := parser.ExtractParamNames(line, name)
			for arity := minArity; arity <= maxArity; arity++ {
				key := name + "/" + strconv.Itoa(arity)
				if !seen[key] {
					seen[key] = true
					results = append(results, BufferFunction{Name: name, Arity: arity, Kind: m[1], Params: parser.JoinParams(allParamNames, arity)})
				}
			}
		} else if m := parser.TypeDefRe.FindStringSubmatch(line); m != nil {
			name := m[2]
			arity := parser.ExtractArity(line, name)
			key := name + "/" + strconv.Itoa(arity)
			if !seen[key] {
				seen[key] = true
				results = append(results, BufferFunction{Name: name, Arity: arity, Kind: m[1]})
			}
		}
	}
	return results
}

var (
	moduleAttrDefRe = regexp.MustCompile(`^\s*@([a-z_][a-z0-9_]*)\s+[^@]`)
)

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
		j := tokNextSig(tokens, n, i)
		name, k := tokCollectModuleName(source, tokens, n, j)
		if name == "" {
			return i
		}
		if !strings.Contains(name, ".") && currentModule() != "" {
			name = currentModule() + "." + name
		}
		// Scan forward to consume TokDo. Do not stop at TokEOL because
		// Elixir allows `defmodule Foo` then `do` on the next line.
		// Stop at statement-boundary tokens to avoid stealing a later
		// module's TokDo when the current module uses `, do:` form.
		for pos := k; pos < n; pos++ {
			switch tokens[pos].Kind {
			case parser.TokDo:
				depth++
				stack = append(stack, moduleFrame{name, depth})
				return pos + 1
			case parser.TokEOF, parser.TokEnd,
				parser.TokDefmodule, parser.TokDefprotocol, parser.TokDefimpl,
				parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
				parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
				return pos
			}
		}
		return k
	}

	for i := 0; i < n; i++ {
		tok := tokens[i]

		// Track target line's module scope (check before any depth changes)
		if !unscoped && targetModule == "" && tok.Line >= targetLine1 {
			targetModule = currentModule()
		}

		switch tok.Kind {
		case parser.TokDo:
			depth++
		case parser.TokFn:
			depth++
		case parser.TokEnd:
			if len(stack) > 0 && stack[len(stack)-1].depth == depth {
				stack = stack[:len(stack)-1]
			}
			depth--
			if depth < 0 {
				depth = 0
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
			if k < n && tokens[k].Kind == parser.TokDot && k+1 < n && tokens[k+1].Kind == parser.TokOpenBrace {
				base := resolve(modName)
				if strings.Contains(base, "__MODULE__") {
					continue
				}
				k += 2 // skip .{
				for k < n && tokens[k].Kind != parser.TokCloseBrace && tokens[k].Kind != parser.TokEOF {
					k = tokNextSig(tokens, n, k)
					if k >= n || tokens[k].Kind == parser.TokCloseBrace {
						break
					}
					// Bail out if we hit a new statement keyword inside the braces
					switch tokens[k].Kind {
					case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
						parser.TokDefmodule, parser.TokEnd, parser.TokImport, parser.TokUse, parser.TokAlias:
						goto bracesDone
					}
					child, nk := tokCollectModuleName(source, tokens, n, k)
					if child != "" {
						aliasKey := child
						if dot := strings.LastIndexByte(child, '.'); dot >= 0 {
							aliasKey = child[dot+1:]
						}
						allAliases = append(allAliases, aliasEntry{cm, aliasKey, base + "." + child})
					}
					if nk == k {
						k++ // ensure forward progress
					} else {
						k = nk
					}
					if k < n && tokens[k].Kind == parser.TokComma {
						k++
					}
				}
			bracesDone:
				if k < n && tokens[k].Kind == parser.TokCloseBrace {
					k++
				}
				i = k - 1
				continue
			}

			// Check for alias Module, as: Name
			nk := tokNextSig(tokens, n, k)
			if nk < n && tokens[nk].Kind == parser.TokComma {
				ac := tokNextSig(tokens, n, nk+1)
				if ac < n && tokens[ac].Kind == parser.TokIdent && tokText(source, tokens[ac]) == "as" {
					ac2 := tokNextSig(tokens, n, ac+1)
					if ac2 < n && tokens[ac2].Kind == parser.TokColon {
						ac3 := tokNextSig(tokens, n, ac2+1)
						if ac3 < n && (tokens[ac3].Kind == parser.TokModule || tokens[ac3].Kind == parser.TokIdent) {
							resolved := resolve(modName)
							if !strings.Contains(resolved, "__MODULE__") {
								allAliases = append(allAliases, aliasEntry{cm, tokText(source, tokens[ac3]), resolved})
							}
							i = ac3
							continue
						}
					}
				}
			}

			// Simple alias
			resolved := resolve(modName)
			if !strings.Contains(resolved, "__MODULE__") {
				short := resolved
				if dot := strings.LastIndexByte(resolved, '.'); dot >= 0 {
					short = resolved[dot+1:]
				}
				allAliases = append(allAliases, aliasEntry{cm, short, resolved})
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
	tokText              = parser.TokenText
)

// ExtractImports parses all import declarations from document text.
// Returns a slice of full module names.
func ExtractImports(text string) []string {
	source := []byte(text)
	tokens := parser.Tokenize(source)
	n := len(tokens)
	var imports []string
	for i := 0; i < n; i++ {
		if tokens[i].Kind == parser.TokImport {
			j := tokNextSig(tokens, n, i+1)
			mod, _ := tokCollectModuleName(source, tokens, n, j)
			if mod != "" {
				imports = append(imports, mod)
			}
		}
	}
	return imports
}

// parseHelperQuoteBlock finds `def/defp helperName` in the source text, locates
// its `quote do` block, and extracts imports/uses/inline-defs/aliases from it.
// Uses the tokenizer for correct heredoc and multi-line handling.
func parseHelperQuoteBlock(lines []string, helperName string, fileAliases map[string]string) (imported []string, inlineDefs map[string][]inlineDef, transUses []string, optBindings []optBinding, aliases map[string]string) {
	source := []byte(strings.Join(lines, "\n"))
	tokens := parser.Tokenize(source)
	n := len(tokens)
	txt := func(t parser.Token) string { return string(source[t.Start:t.End]) }

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
		if j < n && tokens[j].Kind == parser.TokIdent && txt(tokens[j]) == helperName {
			// Find the TokDo that opens this function
			for k := j + 1; k < n; k++ {
				if tokens[k].Kind == parser.TokDo {
					helperStart = k + 1
					break
				}
				if tokens[k].Kind == parser.TokEOL {
					break
				}
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
		switch tokens[i].Kind {
		case parser.TokDo:
			depth++
		case parser.TokFn:
			depth++
		case parser.TokEnd:
			depth--
		case parser.TokIdent:
			if txt(tokens[i]) == "quote" {
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

	// skipInlineDefBody advances from a def head position (typically right after
	// the parameter list) to the token after the def body. For one-line defs
	// (`def foo, do: ...`) it returns the end-of-line token index.
	skipInlineDefBody := func(from int) int {
		// First, scan the current statement header.
		k := from
		for k < n {
			switch tokens[k].Kind {
			case parser.TokDo:
				afterDo := tokNextSig(tokens, n, k+1)
				// `do:` one-line form — no matching `end` to skip.
				if afterDo < n && tokens[afterDo].Kind == parser.TokColon {
					return k
				}
				// Block form (`... do ... end`) — skip nested do/fn..end.
				bodyDepth := 1
				k++
				for k < n && bodyDepth > 0 {
					switch tokens[k].Kind {
					case parser.TokDo, parser.TokFn:
						bodyDepth++
					case parser.TokEnd:
						bodyDepth--
					}
					k++
				}
				return k
			case parser.TokEOL, parser.TokEOF:
				return k
			}
			k++
		}
		return k
	}

	// Walk the quote do block (depth 1 = inside quote do, 0 = we hit its end)
	inlineDefs = make(map[string][]inlineDef)
	depth = 1
	for i := quoteBodyStart; i < n && depth > 0; i++ {
		tok := tokens[i]
		switch tok.Kind {
		case parser.TokDo:
			depth++
		case parser.TokFn:
			depth++
		case parser.TokEnd:
			depth--

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
			if k < n && tokens[k].Kind == parser.TokDot && k+1 < n && tokens[k+1].Kind == parser.TokOpenBrace {
				base := resolveAlias(modName)
				k += 2
				for k < n && tokens[k].Kind != parser.TokCloseBrace && tokens[k].Kind != parser.TokEOF {
					k = tokNextSig(tokens, n, k)
					if k >= n || tokens[k].Kind == parser.TokCloseBrace {
						break
					}
					child, nk := tokCollectModuleName(source, tokens, n, k)
					if child != "" {
						if aliases == nil {
							aliases = make(map[string]string)
						}
						aliasKey := child
						if dot := strings.LastIndexByte(child, '.'); dot >= 0 {
							aliasKey = child[dot+1:]
						}
						aliases[aliasKey] = base + "." + child
					}
					k = nk
					if k < n && tokens[k].Kind == parser.TokComma {
						k++
					}
				}
				continue
			}
			// alias Module, as: Name
			nk := tokNextSig(tokens, n, k)
			if nk < n && tokens[nk].Kind == parser.TokComma {
				ac := tokNextSig(tokens, n, nk+1)
				if ac < n && tokens[ac].Kind == parser.TokIdent && txt(tokens[ac]) == "as" {
					ac2 := tokNextSig(tokens, n, ac+1)
					if ac2 < n && tokens[ac2].Kind == parser.TokColon {
						ac3 := tokNextSig(tokens, n, ac2+1)
						if ac3 < n && (tokens[ac3].Kind == parser.TokModule || tokens[ac3].Kind == parser.TokIdent) {
							if aliases == nil {
								aliases = make(map[string]string)
							}
							aliases[txt(tokens[ac3])] = resolveAlias(modName)
							continue
						}
					}
				}
			}
			// Simple alias
			resolved := resolveAlias(modName)
			if aliases == nil {
				aliases = make(map[string]string)
			}
			if dot := strings.LastIndexByte(resolved, '.'); dot >= 0 {
				aliases[resolved[dot+1:]] = resolved
			} else {
				aliases[resolved] = resolved
			}

		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			kind := txt(tok)
			defLine := tok.Line
			j := tokNextSig(tokens, n, i+1)
			if j >= n || tokens[j].Kind != parser.TokIdent {
				continue
			}
			funcName := txt(tokens[j])
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
			i = skipInlineDefBody(nextPos) - 1
		}
	}
	return
}

// ExtractUses returns module names from all `use Module` declarations.
func ExtractUses(text string) []string {
	source := []byte(text)
	tokens := parser.Tokenize(source)
	n := len(tokens)
	var uses []string
	for i := 0; i < n; i++ {
		if tokens[i].Kind == parser.TokUse {
			j := tokNextSig(tokens, n, i+1)
			mod, _ := tokCollectModuleName(source, tokens, n, j)
			if mod != "" {
				uses = append(uses, mod)
			}
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
	tokens := parser.Tokenize(source)
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

		// Check for comma after module name → keyword opts follow
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
				key := tokText(source, tok)
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

	txt := func(t parser.Token) string { return string(source[t.Start:t.End]) }

	nextSig := func(from int) int {
		for from < n {
			k := tokens[from].Kind
			if k != parser.TokEOL && k != parser.TokComment {
				return from
			}
			from++
		}
		return n
	}

	collectModuleName := func(i int) (string, int) {
		if i >= n || tokens[i].Kind != parser.TokModule {
			return "", i
		}
		var parts []string
		parts = append(parts, txt(tokens[i]))
		i++
		for i+1 < n && tokens[i].Kind == parser.TokDot && tokens[i+1].Kind == parser.TokModule {
			parts = append(parts, txt(tokens[i+1]))
			i += 2
		}
		return strings.Join(parts, "."), i
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
			if j < n && tokens[j].Kind == parser.TokIdent && txt(tokens[j]) == "__using__" {
				// Scan forward to find TokDo, then body starts after it
				for k := j + 1; k < n; k++ {
					if tokens[k].Kind == parser.TokDo {
						usingBodyStart = k + 1
						usingDepth = 1 // inside the defmacro do..end
						break
					}
					if tokens[k].Kind == parser.TokEOL {
						break
					}
				}
				break
			}
		}
		// ExUnit.CaseTemplate: `using do` or `using opts do`
		if usesCaseTemplate && tok.Kind == parser.TokIdent && txt(tok) == "using" {
			// Must be at statement start
			if i == 0 || tokens[i-1].Kind == parser.TokEOL {
				for k := i + 1; k < n; k++ {
					if tokens[k].Kind == parser.TokDo {
						usingBodyStart = k + 1
						usingDepth = 1
						break
					}
					if tokens[k].Kind == parser.TokEOL {
						break
					}
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

	// Extract file-level aliases for resolution
	lines := strings.Split(text, "\n")
	fileAliases := extractAliasesFromText(text, -1)

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

	// skipToEndOfStatement advances i past the current statement (to the next
	// TokEOL at bracket depth 0, or to end of tokens).
	skipToEndOfStatement := func(i int) int {
		depth := 0
		for i < n {
			switch tokens[i].Kind {
			case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
				depth++
			case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace:
				depth--
			case parser.TokEOL:
				if depth <= 0 {
					return i
				}
			case parser.TokEOF:
				return i
			}
			i++
		}
		return i
	}

	// skipInlineDefBody advances from a def head position (typically right after
	// params) to after the inline def body. This keeps statements inside inline
	// function bodies from being treated as top-level __using__ statements.
	skipInlineDefBody := func(from int) int {
		headerEnd := skipToEndOfStatement(from)
		// Look for block-style `do` in the current def head.
		for k := from; k < n && k < headerEnd; k++ {
			if tokens[k].Kind != parser.TokDo {
				continue
			}
			afterDo := nextSig(k + 1)
			// `do:` one-line form; no trailing `end` to consume.
			if afterDo < n && tokens[afterDo].Kind == parser.TokColon {
				return headerEnd
			}
			// Block form (`... do ... end`) — skip nested do/fn..end.
			bodyDepth := 1
			k++
			for k < n && bodyDepth > 0 {
				switch tokens[k].Kind {
				case parser.TokDo, parser.TokFn:
					bodyDepth++
				case parser.TokEnd:
					bodyDepth--
				}
				k++
			}
			return k
		}
		// No `do` in the head (e.g. malformed input); fall back to statement end.
		return headerEnd
	}

	// scanKeywordCall checks if tokens starting at i match:
	//   Keyword.{get|pop|put|put_new|fetch|fetch!|pop!|pop_lazy}(ident, :key [, Default])
	// Returns (funcName, argIdent, atomKey, defaultModule, endPos) or empty strings if no match.
	scanKeywordCall := func(i int) (string, string, string, int) {
		// Expect: TokModule("Keyword") TokDot TokIdent(funcName) TokOpenParen
		if i+3 >= n {
			return "", "", "", i
		}
		if tokens[i].Kind != parser.TokModule || txt(tokens[i]) != "Keyword" {
			return "", "", "", i
		}
		if tokens[i+1].Kind != parser.TokDot {
			return "", "", "", i
		}
		if tokens[i+2].Kind != parser.TokIdent {
			return "", "", "", i
		}
		funcName := txt(tokens[i+2])
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
			return funcName, "", "", skipToEndOfStatement(j)
		}
		atomText := txt(tokens[j])
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
		return funcName, atomKey, "", skipToEndOfStatement(j)
	}

	// Walk tokens inside the __using__ body
	depth := usingDepth
	i := usingBodyStart
	for i < n && depth > 0 {
		tok := tokens[i]

		switch tok.Kind {
		case parser.TokDo:
			depth++
			i++
		case parser.TokFn:
			depth++
			i++
		case parser.TokEnd:
			depth--
			i++
		case parser.TokEOL, parser.TokComment, parser.TokString, parser.TokHeredoc,
			parser.TokSigil, parser.TokAtom, parser.TokNumber, parser.TokCharLiteral,
			parser.TokEOF:
			i++

		case parser.TokImport:
			i++
			j := nextSig(i)
			// import unquote(var)
			if j < n && tokens[j].Kind == parser.TokIdent && txt(tokens[j]) == "unquote" {
				if j+2 < n && tokens[j+1].Kind == parser.TokOpenParen && tokens[j+2].Kind == parser.TokIdent {
					varName := txt(tokens[j+2])
					if b, ok := varToOpt[varName]; ok {
						optBindings = append(optBindings, optBinding{optKey: b.optKey, defaultMod: b.defaultMod, kind: "import"})
					}
				}
				i = skipToEndOfStatement(j)
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
			if j < n && tokens[j].Kind == parser.TokIdent && txt(tokens[j]) == "unquote" {
				if j+2 < n && tokens[j+1].Kind == parser.TokOpenParen && tokens[j+2].Kind == parser.TokIdent {
					varName := txt(tokens[j+2])
					if b, ok := varToOpt[varName]; ok {
						optBindings = append(optBindings, optBinding{optKey: b.optKey, defaultMod: b.defaultMod, kind: "use"})
					}
				}
				i = skipToEndOfStatement(j)
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
			if k < n && tokens[k].Kind == parser.TokDot && k+1 < n && tokens[k+1].Kind == parser.TokOpenBrace {
				parent := resolveAlias(modName)
				k += 2 // skip .{
				for k < n && tokens[k].Kind != parser.TokCloseBrace && tokens[k].Kind != parser.TokEOF {
					k = nextSig(k)
					if k >= n || tokens[k].Kind == parser.TokCloseBrace {
						break
					}
					childName, newK := collectModuleName(k)
					if childName != "" {
						if aliases == nil {
							aliases = make(map[string]string)
						}
						aliasKey := childName
						if dot := strings.LastIndexByte(childName, '.'); dot >= 0 {
							aliasKey = childName[dot+1:]
						}
						aliases[aliasKey] = parent + "." + childName
					}
					k = newK
					if k < n && tokens[k].Kind == parser.TokComma {
						k++
					}
				}
				if k < n && tokens[k].Kind == parser.TokCloseBrace {
					k++
				}
				i = k
				continue
			}
			// alias Module, as: Name
			nk := nextSig(k)
			if nk < n && tokens[nk].Kind == parser.TokComma {
				afterComma := nextSig(nk + 1)
				if afterComma < n && tokens[afterComma].Kind == parser.TokIdent && txt(tokens[afterComma]) == "as" {
					afterAs := nextSig(afterComma + 1)
					if afterAs < n && tokens[afterAs].Kind == parser.TokColon {
						afterColon := nextSig(afterAs + 1)
						if afterColon < n && (tokens[afterColon].Kind == parser.TokModule || tokens[afterColon].Kind == parser.TokIdent) {
							asName := txt(tokens[afterColon])
							if aliases == nil {
								aliases = make(map[string]string)
							}
							aliases[asName] = resolveAlias(modName)
							i = afterColon + 1
							continue
						}
					}
				}
			}
			// Simple alias
			resolved := resolveAlias(modName)
			if aliases == nil {
				aliases = make(map[string]string)
			}
			dot := strings.LastIndexByte(resolved, '.')
			if dot >= 0 {
				aliases[resolved[dot+1:]] = resolved
			} else {
				aliases[resolved] = resolved
			}
			i = k

		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			kind := txt(tok)
			defLine := tok.Line
			i++
			j := nextSig(i)
			if j >= n || tokens[j].Kind != parser.TokIdent {
				i = j
				continue
			}
			funcName := txt(tokens[j])
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
			i = skipInlineDefBody(nextPos)
			continue

		case parser.TokModule:
			// Check for Keyword.put/put_new(opts, :key, Module) heuristic
			modText := txt(tok)
			if modText == "Keyword" && i+2 < n && tokens[i+1].Kind == parser.TokDot && tokens[i+2].Kind == parser.TokIdent {
				funcName := txt(tokens[i+2])
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
			identName := txt(tok)
			isStmtStart := i == 0 || tokens[i-1].Kind == parser.TokEOL || tokens[i-1].Kind == parser.TokComment
			j := nextSig(i + 1)

			// Check for var = Keyword.{get,pop,put,put_new,...}(opts, :key, Default)
			// or var = ModuleName
			if isStmtStart && j < n && tokens[j].Kind == parser.TokOther && txt(tokens[j]) == "=" {
				k := nextSig(j + 1)
				if k < n && tokens[k].Kind == parser.TokModule && txt(tokens[k]) == "Keyword" {
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
				i = skipToEndOfStatement(i)
				continue
			}
			i++

		case parser.TokOpenBrace:
			// Check for {var, _} = Keyword.pop(opts, :key, Default)
			j := nextSig(i + 1)
			if j < n && tokens[j].Kind == parser.TokIdent {
				varName := txt(tokens[j])
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
				if eq < n && tokens[eq].Kind == parser.TokOther && txt(tokens[eq]) == "=" {
					kw := nextSig(eq + 1)
					if kw < n && tokens[kw].Kind == parser.TokModule && txt(tokens[kw]) == "Keyword" {
						funcName, atomKey, defaultMod, endJ := scanKeywordCall(kw)
						if (funcName == "pop" || funcName == "pop!") && atomKey != "" {
							varToOpt[varName] = varBinding{optKey: atomKey, defaultMod: resolveAlias(defaultMod)}
						} else if (funcName == "fetch" || funcName == "fetch!" || funcName == "pop_lazy") && atomKey != "" {
							varToOpt[varName] = varBinding{optKey: atomKey}
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

// ExtractModuleAttribute returns the attribute name if the cursor is on a @attr reference,
// otherwise returns "". For example, on "@endpoint_scopes" returns "endpoint_scopes".
func ExtractModuleAttribute(line string, col int) string {
	if col >= len(line) {
		return ""
	}
	// Scan back to find a leading @
	start := col
	for start > 0 && (unicode.IsLetter(rune(line[start-1])) || unicode.IsDigit(rune(line[start-1])) || line[start-1] == '_') {
		start--
	}
	if start > 0 && line[start-1] == '@' {
		start--
	} else if start < len(line) && line[start] == '@' {
		// cursor is on the @ itself
	} else {
		return ""
	}
	if start >= len(line) || line[start] != '@' {
		return ""
	}
	end := start + 1
	for end < len(line) && (unicode.IsLetter(rune(line[end])) || unicode.IsDigit(rune(line[end])) || line[end] == '_') {
		end++
	}
	name := line[start+1 : end]
	if len(name) == 0 {
		return ""
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
	if reservedModuleAttrs[attrName] {
		return 0, false
	}
	for i, line := range strings.Split(text, "\n") {
		if m := moduleAttrDefRe.FindStringSubmatch(line); m != nil && m[1] == attrName {
			return i + 1, true
		}
	}
	return 0, false
}

// FindFunctionDefinition searches the document text for a def/defp/defmacro/defmacrop
// matching the given function name. Returns the 1-based line number and true if found.
func FindFunctionDefinition(text string, functionName string) (int, bool) {
	for i, line := range strings.Split(text, "\n") {
		if m := parser.FuncDefRe.FindStringSubmatch(line); m != nil {
			if m[2] == functionName {
				return i + 1, true
			}
			continue // FuncDefRe and TypeDefRe match different line prefixes
		}
		if m := parser.TypeDefRe.FindStringSubmatch(line); m != nil {
			if m[2] == functionName {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// FindBareFunctionCalls scans text for unqualified calls to functionName,
// including direct calls like functionName(...) and pipe calls like |> functionName.
// Returns 1-based line numbers. Definition lines are excluded.
func FindBareFunctionCalls(text string, functionName string) []int {
	var lineNums []int
	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := parser.FuncDefRe.FindStringSubmatch(trimmed); m != nil && m[2] == functionName {
			continue
		}
		if strings.HasPrefix(trimmed, "@spec ") || strings.HasPrefix(trimmed, "@callback ") {
			continue
		}

		found := false

		// Direct bare call: functionName( — but NOT Module.functionName(
		for _, col := range findAllTokenColumns(line, functionName) {
			if col == 0 || line[col-1] != '.' {
				afterToken := line[col+len(functionName):]
				afterTrimmed := strings.TrimLeft(afterToken, " \t")
				if strings.HasPrefix(afterTrimmed, "(") {
					found = true
					break
				}
			}
		}

		// Pipe call: |> functionName
		if !found {
			for pipeSearch := line; ; {
				idx := strings.Index(pipeSearch, "|>")
				if idx < 0 {
					break
				}
				afterPipe := strings.TrimLeft(pipeSearch[idx+2:], " \t")
				if col := findTokenColumn(afterPipe, functionName); col == 0 {
					found = true
					break
				}
				pipeSearch = pipeSearch[idx+2:]
			}
		}

		if found {
			lineNums = append(lineNums, i+1)
		}
	}
	return lineNums
}

// ExtractCallContext scans backward from (lineNum, col) in text to find the
// innermost open function call. Returns the function expression (e.g.
// "Enum.map" or "my_func"), the 0-based argument index, and true if found.
func ExtractCallContext(text string, lineNum, col int) (funcExpr string, argIndex int, ok bool) {
	lines := strings.Split(text, "\n")
	if lineNum >= len(lines) {
		return "", 0, false
	}
	// Clamp col to line length
	if col > len(lines[lineNum]) {
		col = len(lines[lineNum])
	}

	// Convert (lineNum, col) to a flat byte offset
	offset := 0
	for i := 0; i < lineNum; i++ {
		offset += len(lines[i]) + 1 // +1 for newline
	}
	offset += col

	if offset > len(text) {
		offset = len(text)
	}

	// Scan backward tracking nesting depth
	depth := 0
	commas := 0
	inString := false

	for i := offset - 1; i >= 0; i-- {
		ch := text[i]

		// String skip: when we hit a closing ", scan backward to find the opening "
		if ch == '"' && !inString {
			inString = true
			continue
		}
		if inString {
			if ch == '"' {
				// Count preceding backslashes — an odd number means the quote is escaped
				backslashes := 0
				for j := i - 1; j >= 0 && text[j] == '\\'; j-- {
					backslashes++
				}
				if backslashes%2 == 0 {
					inString = false
				}
			}
			continue
		}

		switch ch {
		case ')', ']', '}':
			depth++
		case '[', '{':
			if depth > 0 {
				depth--
			} else {
				// Inside a list/map/tuple, not a function call
				return "", 0, false
			}
		case '(':
			if depth > 0 {
				depth--
			} else {
				// Found the opening paren of our call — extract the function name before it
				// Scan backward from i-1 to find the expression
				exprEnd := i - 1
				// Skip whitespace between expression and paren
				for exprEnd >= 0 && (text[exprEnd] == ' ' || text[exprEnd] == '\t' || text[exprEnd] == '\n' || text[exprEnd] == '\r') {
					exprEnd--
				}
				if exprEnd < 0 {
					return "", 0, false
				}
				// Find the start of the expression
				exprStart := exprEnd
				for exprStart > 0 && isExprChar(text[exprStart-1]) {
					exprStart--
				}
				funcExpr = text[exprStart : exprEnd+1]
				if funcExpr == "" {
					return "", 0, false
				}
				// Skip Elixir keywords that take parens (if, case, etc.)
				if parser.IsElixirKeyword(funcExpr) {
					return "", 0, false
				}
				return funcExpr, commas, true
			}
		case ',':
			if depth == 0 {
				commas++
			}
		}
	}
	return "", 0, false
}

// extractParamNames reads the function definition line at defIdx and returns
// the parameter names. Falls back to positional names (arg1, arg2, ...) for
// complex patterns.

func extractParamNames(lines []string, defIdx int) []string {
	if defIdx < 0 || defIdx >= len(lines) {
		return nil
	}
	line := lines[defIdx]
	m := parser.FuncDefRe.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	return parser.ExtractParamNames(line, m[2])
}
