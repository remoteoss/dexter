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
	if len(line) == 0 {
		return ""
	}
	if col >= len(line) {
		col = len(line) - 1
	}
	if col < 0 {
		col = 0
	}

	if !isExprChar(line[col]) {
		return ""
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

	// Scan right from the cursor to find the next dot, which marks the end of
	// the current segment. If the cursor lands on a dot, skip it so that the
	// next segment is included (hovering on "MyApp|.Repo" resolves MyApp.Repo).
	searchFrom := cursorOffset
	if fullExpr[searchFrom] == '.' {
		searchFrom++
	}
	nextDot := strings.IndexByte(fullExpr[searchFrom:], '.')
	if nextDot == -1 {
		return fullExpr
	}
	return fullExpr[:searchFrom+nextDot]
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
// Returns the prefix text and whether the cursor is immediately after a dot.
func ExtractCompletionContext(line string, col int) (prefix string, afterDot bool) {
	if col <= 0 || len(line) == 0 {
		return "", false
	}
	if col > len(line) {
		col = len(line)
	}

	end := col - 1
	if end < 0 || !isExprChar(line[end]) {
		return "", false
	}

	start := end
	for start > 0 && isExprChar(line[start-1]) {
		start--
	}

	raw := line[start : end+1]

	// Trim trailing dots — "Foo." means afterDot=true, prefix="Foo"
	if strings.HasSuffix(raw, ".") {
		return strings.TrimSuffix(raw, "."), true
	}

	return raw, false
}

type BufferFunction struct {
	Name  string
	Arity int
	Kind  string
}

// FindBufferFunctions scans document text for all function definitions.
// Returns a deduplicated list (multi-clause functions with the same arity appear once).
func FindBufferFunctions(text string) []BufferFunction {
	seen := make(map[string]bool)
	var results []BufferFunction
	for _, line := range strings.Split(text, "\n") {
		if m := parser.FuncDefRe.FindStringSubmatch(line); m != nil {
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
	keywordModuleRe = regexp.MustCompile(`Keyword\.(?:put_new|put|pop!?)\([^,]+,\s*:[a-z_]+,\s*([A-Z][A-Za-z0-9_.]+)\)`)
)

// ExtractAliases parses all alias declarations from document text.
// Returns a map of short name -> full module name.
// Handles: "alias A.B.C", "alias A.B.C, as: D", "alias A.B.{C, D}", and __MODULE__ references.
func ExtractAliases(text string) map[string]string {
	var currentModule string
	aliases := make(map[string]string)

	resolve := func(s string) string {
		if currentModule != "" {
			return strings.ReplaceAll(s, "__MODULE__", currentModule)
		}
		return s
	}

	for _, line := range strings.Split(text, "\n") {
		if currentModule == "" {
			if m := parser.DefmoduleRe.FindStringSubmatch(line); m != nil {
				currentModule = m[1]
			}
		}

		// alias A.B.C, as: D
		if m := parser.AliasAsRe.FindStringSubmatch(line); m != nil {
			resolved := resolve(m[1])
			if !strings.Contains(resolved, "__MODULE__") {
				aliases[m[2]] = resolved
			}
			continue
		}
		// alias A.B.{C, D, E}
		if m := aliasMultiRe.FindStringSubmatch(line); m != nil {
			base := resolve(m[1])
			if strings.Contains(base, "__MODULE__") {
				continue
			}
			for _, name := range strings.Split(m[2], ",") {
				name = strings.TrimSpace(name)
				if len(name) > 0 && unicode.IsUpper(rune(name[0])) {
					aliases[name] = base + "." + name
				}
			}
			continue
		}
		// alias A.B.C
		if m := parser.AliasRe.FindStringSubmatch(line); m != nil {
			fullMod := resolve(m[1])
			if strings.Contains(fullMod, "__MODULE__") {
				continue
			}
			parts := strings.Split(fullMod, ".")
			shortName := parts[len(parts)-1]
			aliases[shortName] = fullMod
			continue
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

// inlineDef records a function or macro defined directly inside a __using__
// quote do block. These definitions get injected into any module that `use`s
// the parent module.
type inlineDef struct {
	line  int // 1-based line number in the source file
	arity int
	kind  string // "def", "defp", "defmacro", etc.
}

// extractUsingImports returns the modules imported within the defmacro __using__
// body, with file-level aliases resolved. Results are in source order; callers
// should iterate in reverse to respect "last import wins" semantics.
func extractUsingImports(text string) []string {
	imported, _, _ := parseUsingBody(text)
	return imported
}

// extractUsingInlineDefs returns 1-based line numbers of def/defmacro
// declarations for functionName inside the defmacro __using__ body.
func extractUsingInlineDefs(text, functionName string) []int {
	_, inlineDefs, _ := parseUsingBody(text)
	var lines []int
	for _, d := range inlineDefs[functionName] {
		lines = append(lines, d.line)
	}
	return lines
}

// parseUsingBody finds the defmacro __using__ block in text and scans its body
// for import statements, inline function definitions, and transitive use calls.
// Returns imported module names, all inline defs keyed by function name, and
// module names from `use` statements inside the body (for double-use chains).
// The full maps are returned so callers can cache them once.
func parseUsingBody(text string) (imported []string, inlineDefs map[string][]inlineDef, transUses []string) {
	lines := strings.Split(text, "\n")
	fileAliases := ExtractAliases(text)

	usingIdx := -1
	usingIndent := 0
	for i, line := range lines {
		if usingDefRe.MatchString(line) {
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
		if resolved, ok := fileAliases[modName]; ok {
			return resolved
		}
		if parts := strings.SplitN(modName, ".", 2); len(parts) == 2 {
			if resolved, ok := fileAliases[parts[0]]; ok {
				return resolved + "." + parts[1]
			}
		}
		return modName
	}

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

		if m := importRe.FindStringSubmatch(line); m != nil {
			imported = append(imported, resolveAlias(m[1]))
			continue
		}

		if m := useRe.FindStringSubmatch(line); m != nil {
			modName := m[1]
			// Skip `use unquote(var)` — the module is dynamic and can't be
			// resolved statically. Module hints from Keyword.put_new/put/pop
			// below handle the common case.
			if modName != "unquote" {
				transUses = append(transUses, resolveAlias(modName))
			}
			continue
		}

		// Detect module names passed through opts via Keyword.put_new/put/pop.
		// For example: Keyword.put_new(opts, :oban_module, Oban.Pro.Worker)
		// These modules are typically used transitively deeper in the chain
		// via `use unquote(var)` and can't be resolved statically otherwise.
		//
		// This is a bit hacky — it matches any Keyword call with a module
		// value, not just ones whose variable feeds into a `use unquote(var)`.
		// A proper fix would link the variable name to the unquote call, but
		// that requires a two-pass scan. The false positives are harmless
		// (just extra nil lookups) and the pattern doesn't appear in practice.
		if m := keywordModuleRe.FindStringSubmatch(line); m != nil {
			transUses = append(transUses, resolveAlias(m[1]))
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
		}
	}
	return 0, false
}
