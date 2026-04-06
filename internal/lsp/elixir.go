package lsp

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
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

type BufferFunction struct {
	Name  string
	Arity int
	Kind  string
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
			maxArity := parser.ExtractArity(line, name)
			minArity := maxArity - parser.CountDefaultParams(line, name)
			for arity := minArity; arity <= maxArity; arity++ {
				key := name + "/" + strconv.Itoa(arity)
				if !seen[key] {
					seen[key] = true
					results = append(results, BufferFunction{Name: name, Arity: arity, Kind: m[1]})
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
	aliasMultiRe    = regexp.MustCompile(`^\s*alias\s+([A-Za-z0-9_.]+)\.{([^}]+)}`)
	importRe        = regexp.MustCompile(`^\s*import\s+([A-Za-z0-9_.]+)`)
	useRe           = regexp.MustCompile(`^\s*use\s+([A-Za-z0-9_.]+)`)
	usingDefRe      = regexp.MustCompile(`^\s*defmacro\s+__using__`)
	moduleAttrDefRe = regexp.MustCompile(`^\s*@([a-z_][a-z0-9_]*)\s+[^@]`)
	keywordModuleRe = regexp.MustCompile(`Keyword\.(?:put_new|put)\([^,]+,\s*:[a-z_]+,\s*([A-Z][A-Za-z0-9_.]+)\)`)

	// Dynamic opt-binding patterns inside __using__ bodies.
	// var = Keyword.get/pop(opts, :key, Default)
	varKeywordWithDefaultRe = regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\s*=\s*Keyword\.(?:get|pop)\s*\([^,]+,\s*:([a-z_][a-z0-9_]*),\s*([A-Z][A-Za-z0-9_.]+)\)`)
	// {var, _} = Keyword.pop(opts, :key, Default) — tuple destructuring
	varTupleKeywordRe = regexp.MustCompile(`^\s*\{([a-z_][a-z0-9_]*),\s*[^}]+\}\s*=\s*Keyword\.pop\s*\([^,]+,\s*:([a-z_][a-z0-9_]*),\s*([A-Z][A-Za-z0-9_.]+)\)`)
	// var = Keyword.fetch/fetch!/pop!/pop_lazy(opts, :key) — no parseable default
	varKeywordNoDefaultRe = regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\s*=\s*Keyword\.(?:fetch!?|pop!|pop_lazy)\s*\([^,]+,\s*:([a-z_][a-z0-9_]*)\b`)
	// var = ModuleName (simple assignment to a capitalized module)
	varSimpleModuleRe = regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\s*=\s*([A-Z][A-Za-z0-9_.]+)\s*$`)

	// import/use with unquote(var) — captures the var name
	importUnquoteRe = regexp.MustCompile(`^\s*import\s+unquote\(([a-z_][a-z0-9_]*)\)`)
	useUnquoteRe    = regexp.MustCompile(`^\s*use\s+unquote\(([a-z_][a-z0-9_]*)\)`)

	// ExUnit.CaseTemplate detection and `using` macro form
	caseTemplateRe      = regexp.MustCompile(`^\s*use\s+ExUnit\.CaseTemplate\b`)
	caseTemplateUsingRe = regexp.MustCompile(`^\s*using\b`)
	// quote do block inside a helper function
	quoteDoRe = regexp.MustCompile(`^\s*quote\s+do\b`)
	// bare function call: function_name( — used to detect delegation to a helper
	bareCallRe = regexp.MustCompile(`^\s*([a-z_][a-z0-9_]*)\s*\(`)

	// use Module, key: Val, key2: Val2  — captures (module, opts_string)
	useWithOptsRe = regexp.MustCompile(`^\s*use\s+([A-Za-z0-9_.]+)\s*,\s*(.+)$`)
	// individual key: Module pairs in opts
	optKeyModuleRe = regexp.MustCompile(`([a-z_][a-z0-9_]*):\s*([A-Z][A-Za-z0-9_.]+)`)
)

// ExtractAliases parses all alias declarations from document text.
// Returns a map of short name -> full module name (not scope-aware).
func ExtractAliases(text string) map[string]string {
	return extractAliasesFromLines(strings.Split(text, "\n"), -1)
}

// ExtractAliasesInScope parses alias declarations visible at the given 0-based
// line. In Elixir, aliases are lexically scoped to the enclosing defmodule —
// a nested module does NOT inherit its parent's aliases.
func ExtractAliasesInScope(text string, targetLine int) map[string]string {
	return extractAliasesFromLines(strings.Split(text, "\n"), targetLine)
}

// extractAliasesFromLines is the shared implementation. When targetLine >= 0, only
// aliases from the module scope enclosing that line are returned.
// Uses a single pass: collects all aliases keyed by their module scope, then
// returns only those matching the target line's scope.
func extractAliasesFromLines(lines []string, targetLine int) map[string]string {
	type moduleFrame struct {
		name  string
		depth int // do..end nesting depth when this module was opened
	}

	var stack []moduleFrame
	depth := 0

	// Per-scope alias collection: module name → alias map
	var allAliases []struct {
		scope string
		short string
		full  string
	}
	var targetModule string
	unscoped := targetLine < 0

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track do..end nesting
		if trimmed == "end" {
			if len(stack) > 0 && stack[len(stack)-1].depth == depth {
				stack = stack[:len(stack)-1]
			}
			depth--
			if depth < 0 {
				depth = 0
			}
		}

		if m := parser.DefmoduleRe.FindStringSubmatch(line); m != nil {
			name := m[1]
			if !strings.Contains(name, ".") && len(stack) > 0 {
				name = stack[len(stack)-1].name + "." + name
			}
			depth++
			stack = append(stack, moduleFrame{name, depth})
		} else if parser.ContainsDo(trimmed) {
			depth++
		}

		currentModule := ""
		if len(stack) > 0 {
			currentModule = stack[len(stack)-1].name
		}

		if i == targetLine {
			targetModule = currentModule
		}

		resolve := func(s string) string {
			if currentModule != "" {
				return strings.ReplaceAll(s, "__MODULE__", currentModule)
			}
			return s
		}

		// Collect alias declarations
		if m := parser.AliasAsRe.FindStringSubmatch(line); m != nil {
			resolved := resolve(m[1])
			if !strings.Contains(resolved, "__MODULE__") {
				allAliases = append(allAliases, struct {
					scope, short, full string
				}{currentModule, m[2], resolved})
			}
		} else if m := aliasMultiRe.FindStringSubmatch(line); m != nil {
			base := resolve(m[1])
			if !strings.Contains(base, "__MODULE__") {
				for _, name := range strings.Split(m[2], ",") {
					name = strings.TrimSpace(name)
					if len(name) > 0 && unicode.IsUpper(rune(name[0])) {
						allAliases = append(allAliases, struct {
							scope, short, full string
						}{currentModule, name, base + "." + name})
					}
				}
			}
		} else if m := parser.AliasRe.FindStringSubmatch(line); m != nil {
			fullMod := resolve(m[1])
			if !strings.Contains(fullMod, "__MODULE__") {
				parts := strings.Split(fullMod, ".")
				allAliases = append(allAliases, struct {
					scope, short, full string
				}{currentModule, parts[len(parts)-1], fullMod})
			}
		}
	}

	// Filter by scope
	aliases := make(map[string]string)
	for _, a := range allAliases {
		if unscoped || a.scope == targetModule {
			aliases[a.short] = a.full
		}
	}
	return aliases
}

// ExtractImports parses all import declarations from document text.
// Returns a slice of full module names.
func ExtractImports(text string) []string {
	var imports []string
	for _, line := range strings.Split(text, "\n") {
		if m := importRe.FindStringSubmatch(line); m != nil {
			imports = append(imports, m[1])
		}
	}
	return imports
}

// parseHelperQuoteBlock finds `def/defp helperName` in lines, locates its
// `quote do` block, and extracts imports/uses/inline-defs from it. Returns
// nil slices if the function or its quote block can't be found.
func parseHelperQuoteBlock(lines []string, helperName string, fileAliases map[string]string) (imported []string, inlineDefs map[string][]inlineDef, transUses []string, optBindings []optBinding) {
	resolveAlias := func(modName string) string {
		return parser.ResolveModuleRef(modName, fileAliases, "")
	}

	// Find the def/defp for helperName
	funcIdx := -1
	funcIndent := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		rest := ""
		if strings.HasPrefix(trimmed, "defp ") {
			rest = trimmed[5:]
		} else if strings.HasPrefix(trimmed, "def ") {
			rest = trimmed[4:]
		}
		if rest != "" && strings.HasPrefix(rest, helperName) {
			after := rest[len(helperName):]
			if after == "" || after[0] == '(' || after[0] == ' ' || after[0] == '\t' || after[0] == ',' {
				funcIdx = i
				funcIndent = len(line) - len(strings.TrimLeft(line, " \t"))
				break
			}
		}
	}
	if funcIdx < 0 {
		return
	}

	// Find the quote do block inside the function
	quoteIdx := -1
	quoteIndent := 0
	for i := funcIdx + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent <= funcIndent && (trimmed == "end" || parser.FuncDefRe.MatchString(line)) {
			break
		}
		if quoteDoRe.MatchString(line) {
			quoteIdx = i
			quoteIndent = indent
			break
		}
	}
	if quoteIdx < 0 {
		return
	}

	// Parse the quote do block for imports/uses/inline-defs
	inlineDefs = make(map[string][]inlineDef)
	for i := quoteIdx + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent <= quoteIndent && (trimmed == "end" || parser.FuncDefRe.MatchString(line)) {
			break
		}

		if m := importRe.FindStringSubmatch(line); m != nil {
			imported = append(imported, resolveAlias(m[1]))
			continue
		}
		if m := useRe.FindStringSubmatch(line); m != nil {
			transUses = append(transUses, resolveAlias(m[1]))
			continue
		}
		if m := parser.FuncDefRe.FindStringSubmatch(line); m != nil {
			funcName := m[2]
			inlineDefs[funcName] = append(inlineDefs[funcName], inlineDef{
				line:  i + 1,
				arity: parser.ExtractArity(line, funcName),
				kind:  m[1],
			})
		}
	}
	return
}

// ExtractUses returns module names from all `use Module` declarations.
func ExtractUses(text string) []string {
	var uses []string
	for _, line := range strings.Split(text, "\n") {
		if m := useRe.FindStringSubmatch(line); m != nil {
			uses = append(uses, m[1])
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
// provided map.
func ExtractUsesWithOpts(text string, aliases map[string]string) []UseCall {
	var calls []UseCall
	for _, line := range strings.Split(text, "\n") {
		// use Module, key: Val
		if m := useWithOptsRe.FindStringSubmatch(line); m != nil {
			module := parser.ResolveModuleRef(m[1], aliases, "")
			calls = append(calls, UseCall{Module: module, Opts: ParseKeywordModuleOpts(m[2], aliases)})
			continue
		}
		// plain use Module
		if m := useRe.FindStringSubmatch(line); m != nil {
			calls = append(calls, UseCall{Module: parser.ResolveModuleRef(m[1], aliases, "")})
		}
	}
	return calls
}

// ParseKeywordModuleOpts parses an Elixir keyword list string (e.g. "mod: Hammox, repo: MyRepo")
// into a map of key → value. Only entries whose value starts with an uppercase letter
// (module names) are included. Alias resolution is applied to module values.
func ParseKeywordModuleOpts(optsStr string, aliases map[string]string) map[string]string {
	result := make(map[string]string)
	for _, m := range optKeyModuleRe.FindAllStringSubmatch(optsStr, -1) {
		result[m[1]] = parser.ResolveModuleRef(m[2], aliases, "")
	}
	return result
}

// inlineDef records a function or macro defined directly inside a __using__
// quote do block. These definitions get injected into any module that `use`s
// the parent module.
type inlineDef struct {
	line  int // 1-based line number in the source file
	arity int
	kind  string // "def", "defp", "defmacro", etc.
}

// parseUsingBody finds the defmacro __using__ block in text and scans its body
// for import statements, inline function definitions, transitive use calls, and
// dynamic opt-driven imports (e.g. `import unquote(mod)` where `mod` comes from
// a Keyword.get on opts).
func parseUsingBody(text string) (imported []string, inlineDefs map[string][]inlineDef, transUses []string, optBindings []optBinding) {
	lines := strings.Split(text, "\n")
	fileAliases := extractAliasesFromLines(lines, -1)

	// Check if this module uses ExUnit.CaseTemplate (which provides the `using` macro)
	usesCaseTemplate := false
	for _, line := range lines {
		if caseTemplateRe.MatchString(line) {
			usesCaseTemplate = true
			break
		}
	}

	usingIdx := -1
	usingIndent := 0
	inHeredoc := false
	for i, line := range lines {
		if strings.IndexByte(line, '"') >= 0 {
			if count := strings.Count(line, `"""`); count > 0 {
				if count < 2 {
					inHeredoc = !inHeredoc
				}
				continue
			}
		}
		if strings.IndexByte(line, '\'') >= 0 {
			if count := strings.Count(line, `'''`); count > 0 {
				if count < 2 {
					inHeredoc = !inHeredoc
				}
				continue
			}
		}
		if inHeredoc {
			continue
		}
		if usingDefRe.MatchString(line) {
			usingIdx = i
			usingIndent = len(line) - len(strings.TrimLeft(line, " \t"))
			break
		}
		// ExUnit.CaseTemplate's `using opts do` form — only when the module
		// explicitly uses ExUnit.CaseTemplate (or a known subclass).
		if usesCaseTemplate && caseTemplateUsingRe.MatchString(line) {
			usingIdx = i
			usingIndent = len(line) - len(strings.TrimLeft(line, " \t"))
			break
		}
	}
	if usingIdx < 0 {
		return
	}

	inlineDefs = make(map[string][]inlineDef)

	resolveAlias := func(modName string) string {
		return parser.ResolveModuleRef(modName, fileAliases, "")
	}

	// varToOpt tracks variables bound from opts: var_name → {optKey, defaultMod}
	type varBinding struct {
		optKey     string
		defaultMod string
	}
	varToOpt := make(map[string]varBinding)

	for i := usingIdx + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		// Stop at another definition or closing end at the same indentation level
		if indent <= usingIndent && (parser.FuncDefRe.MatchString(line) || trimmed == "end") {
			break
		}

		// Detect var = Keyword.get/pop(opts, :key, Default)
		if m := varKeywordWithDefaultRe.FindStringSubmatch(line); m != nil {
			varToOpt[m[1]] = varBinding{optKey: m[2], defaultMod: resolveAlias(m[3])}
			continue
		}
		// Detect {var, _} = Keyword.pop(opts, :key, Default) — tuple destructuring
		if m := varTupleKeywordRe.FindStringSubmatch(line); m != nil {
			varToOpt[m[1]] = varBinding{optKey: m[2], defaultMod: resolveAlias(m[3])}
			continue
		}
		// Detect var = Keyword.fetch/fetch!/pop!/pop_lazy(opts, :key) — no parseable default
		if m := varKeywordNoDefaultRe.FindStringSubmatch(line); m != nil {
			varToOpt[m[1]] = varBinding{optKey: m[2]}
			continue
		}
		// Detect var = ModuleName (simple assignment to a module constant)
		if m := varSimpleModuleRe.FindStringSubmatch(line); m != nil {
			varToOpt[m[1]] = varBinding{defaultMod: resolveAlias(m[2])}
			continue
		}
		// Keyword.put_new/put with a module default: the module may be passed
		// into a transitive `use` via unquote(opts) — keep as a heuristic.
		if m := keywordModuleRe.FindStringSubmatch(line); m != nil {
			transUses = append(transUses, resolveAlias(m[1]))
		}

		// import unquote(var) — dynamic import, goes into optBindings only so
		// consumer-provided opts take priority over the default.
		if m := importUnquoteRe.FindStringSubmatch(line); m != nil {
			varName := m[1]
			if b, ok := varToOpt[varName]; ok {
				optBindings = append(optBindings, optBinding{optKey: b.optKey, defaultMod: b.defaultMod, kind: "import"})
			}
			continue
		}

		// use unquote(var) — dynamic use, goes into optBindings only.
		if m := useUnquoteRe.FindStringSubmatch(line); m != nil {
			varName := m[1]
			if b, ok := varToOpt[varName]; ok {
				optBindings = append(optBindings, optBinding{optKey: b.optKey, defaultMod: b.defaultMod, kind: "use"})
			}
			continue
		}

		if m := importRe.FindStringSubmatch(line); m != nil {
			imported = append(imported, resolveAlias(m[1]))
			continue
		}

		if m := useRe.FindStringSubmatch(line); m != nil {
			transUses = append(transUses, resolveAlias(m[1]))
			continue
		}

		// Delegation to a helper function: `using_block(opts)` or similar.
		// Find that function's definition in the same file and parse its
		// quote do block, which contains the actual imports/uses to inject.
		if m := bareCallRe.FindStringSubmatch(line); m != nil {
			helperName := m[1]
			if helperImported, helperDefs, helperTransUses, helperBindings := parseHelperQuoteBlock(lines, helperName, fileAliases); helperImported != nil {
				imported = append(imported, helperImported...)
				for k, v := range helperDefs {
					inlineDefs[k] = append(inlineDefs[k], v...)
				}
				transUses = append(transUses, helperTransUses...)
				optBindings = append(optBindings, helperBindings...)
			}
			continue
		}

		if m := parser.FuncDefRe.FindStringSubmatch(line); m != nil {
			funcName := m[2]
			inlineDefs[funcName] = append(inlineDefs[funcName], inlineDef{
				line:  i + 1,
				arity: parser.ExtractArity(line, funcName),
				kind:  m[1],
			})
		}
	}
	return
}

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

	// Find the function name via FuncDefRe, then locate its param list
	m := parser.FuncDefRe.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	funcName := m[2]

	idx := strings.Index(line, funcName)
	if idx < 0 {
		return nil
	}
	rest := line[idx+len(funcName):]
	parenIdx := strings.IndexByte(rest, '(')
	if parenIdx < 0 {
		return nil
	}

	// Extract the content inside the outermost parens
	inside := rest[parenIdx+1:]
	depth := 1
	end := 0
	for i := 0; i < len(inside); i++ {
		switch inside[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				end = i
				goto found
			}
		}
	}
	return nil

found:
	paramStr := inside[:end]
	if strings.TrimSpace(paramStr) == "" {
		return nil
	}

	// Split by commas at depth 0
	var params []string
	depth = 0
	start := 0
	for i := 0; i < len(paramStr); i++ {
		switch paramStr[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				params = append(params, strings.TrimSpace(paramStr[start:i]))
				start = i + 1
			}
		}
	}
	params = append(params, strings.TrimSpace(paramStr[start:]))

	// Extract a readable name from each param
	var names []string
	for i, p := range params {
		// Strip default value (\\)
		if bsIdx := strings.Index(p, "\\\\"); bsIdx >= 0 {
			p = strings.TrimSpace(p[:bsIdx])
		}
		name := extractParamName(p, i)
		names = append(names, name)
	}
	return names
}

// extractParamName tries to pull a clean variable name from a single parameter.
// Returns a positional fallback for complex patterns.
func extractParamName(param string, index int) string {
	param = strings.TrimSpace(param)
	if name := parser.ScanFuncName(param); name != "" && name != "_" {
		return name
	}
	return "arg" + strconv.Itoa(index+1)
}
