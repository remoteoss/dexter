package parser

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// IsElixirKeyword returns true if the name is an Elixir language keyword
// (control flow or definition keyword) rather than a user-defined macro.
func IsElixirKeyword(name string) bool {
	return elixirKeyword[name]
}

// elixirKeyword is the set of Elixir language constructs that take do blocks
// but are NOT user-defined macros — excluded from bare macro call tracking.
var elixirKeyword = map[string]bool{
	// Control flow
	"if": true, "unless": true, "cond": true, "case": true,
	"try": true, "receive": true, "for": true, "with": true,
	"fn": true, "do": true, "end": true, "else": true,
	"after": true, "catch": true, "rescue": true,
	"quote": true, "unquote": true, "when": true,
	"and": true, "or": true, "not": true, "in": true,
	// Definition keywords — def lines end with " do" but are definitions, not calls
	"def": true, "defp": true, "defmacro": true, "defmacrop": true,
	"defguard": true, "defguardp": true, "defdelegate": true,
	"defmodule": true, "defprotocol": true, "defimpl": true,
	"defstruct": true, "defexception": true,
}

type Definition struct {
	Module     string
	Function   string
	Arity      int
	Line       int
	FilePath   string
	Kind       string
	DelegateTo string
	DelegateAs string // for defdelegate with as: — the function name in the target module
	Params     string // comma-separated parameter names for this arity
}

type Reference struct {
	Module   string // fully-resolved module name
	Function string // function name (empty for module-only refs like alias/import/use)
	Line     int
	FilePath string
	Kind     string // "call", "alias", "import", "use"
}

func ParseFile(path string) ([]Definition, []Reference, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return ParseText(path, string(data))
}

// ParseText parses Elixir source text and returns definitions and references.
// The path is used to populate FilePath fields but the text is not read from disk.
func ParseText(path, text string) ([]Definition, []Reference, error) {
	source := []byte(text)
	tokens := Tokenize(source)
	return parseTextFromTokens(path, source, tokens)
}

// ScanModuleName reads a module name ([A-Za-z0-9_.]+) from the start of s.
func ScanModuleName(s string) string {
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '.' {
			i++
		} else {
			break
		}
	}
	if i == 0 {
		return ""
	}
	return s[:i]
}

// ScanFuncName reads a function/type name ([a-z_][a-z0-9_?!]*) from the start of s.
func ScanFuncName(s string) string {
	if len(s) == 0 {
		return ""
	}
	c := s[0]
	if (c < 'a' || c > 'z') && c != '_' {
		return ""
	}
	i := 1
	for i < len(s) {
		c = s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '?' || c == '!' {
			i++
		} else {
			break
		}
	}
	return s[:i]
}

// funcDefKeywords is ordered longest-first to avoid prefix ambiguity
// (e.g. "defmacrop" before "defmacro", "defp" before "def").
var funcDefKeywords = []string{
	"defdelegate",
	"defmacrop",
	"defmacro",
	"defguardp",
	"defguard",
	"defp",
	"def",
}

// ScanFuncDef checks if rest matches a function definition keyword followed by
// whitespace and a function name. Returns the kind, name, and true if matched.
func ScanFuncDef(rest string) (string, string, bool) {
	for _, kw := range funcDefKeywords {
		if !strings.HasPrefix(rest, kw) {
			continue
		}
		after := rest[len(kw):]
		// Must be followed by whitespace
		if len(after) == 0 || (after[0] != ' ' && after[0] != '\t') {
			continue
		}
		after = strings.TrimLeft(after, " \t")
		name := ScanFuncName(after)
		if name == "" {
			continue
		}
		// Verify next char is whitespace, '(', or ','
		afterName := after[len(name):]
		if len(afterName) > 0 {
			c := afterName[0]
			if c != ' ' && c != '\t' && c != '(' && c != ',' && c != '\n' && c != '\r' {
				continue
			}
		}
		return kw, name, true
	}
	return "", "", false
}

// FindParamContent locates funcName in line, finds the opening parenthesis
// after it, and returns the substring starting after that '('. Returns ""
// if funcName is not found or has no parenthesized arguments.
// This allows callers that need both arity and default counts to avoid
// repeating the Index + IndexByte lookup.
func FindParamContent(line, funcName string) string {
	idx := strings.Index(line, funcName)
	if idx < 0 {
		return ""
	}
	rest := line[idx+len(funcName):]
	parenIdx := strings.IndexByte(rest, '(')
	if parenIdx < 0 {
		return ""
	}
	return rest[parenIdx+1:]
}

// ArityFromParams counts the number of top-level arguments in the parameter
// content string (as returned by FindParamContent). Respects nested
// parens/brackets/braces and skips string literals.
func ArityFromParams(inside string) int {
	if inside == "" {
		return 0
	}
	depth := 1
	commas := 0
	hasContent := false
	for i := 0; i < len(inside); i++ {
		ch := inside[i]
		// Skip string and charlist literals
		if ch == '"' || ch == '\'' {
			i = skipStringLiteral(inside, i)
			hasContent = true
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				if hasContent {
					return commas + 1
				}
				return 0
			}
		case ',':
			if depth == 1 {
				commas++
			}
		}
		if depth == 1 && ch != ' ' && ch != '\t' && ch != '\n' {
			hasContent = true
		}
	}
	if hasContent {
		return commas + 1
	}
	return 0
}

// DefaultsFromParams counts parameters with default values (\\) in the
// parameter content string (as returned by FindParamContent). Only counts
// defaults at the top-level param depth, not inside nested structures.
func DefaultsFromParams(inside string) int {
	if inside == "" {
		return 0
	}
	depth := 1
	defaults := 0
	for i := 0; i < len(inside); i++ {
		ch := inside[i]
		// Skip string and charlist literals
		if ch == '"' || ch == '\'' {
			i = skipStringLiteral(inside, i)
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
			if depth == 0 {
				return defaults
			}
		case '\\':
			if depth == 1 && i+1 < len(inside) && inside[i+1] == '\\' {
				defaults++
				i++ // skip the second backslash
			}
		}
	}
	return defaults
}

// ExtractArity counts the number of arguments in a function definition line.
// It finds the first parenthesized argument list after the function name and
// counts top-level commas, respecting nested parens/brackets/braces.
func ExtractArity(line string, funcName string) int {
	return ArityFromParams(FindParamContent(line, funcName))
}

// ExtractParamNames extracts readable parameter names from a function
// definition line. Returns nil if the line can't be parsed. For complex
// patterns (e.g. %{name: name}), falls back to positional names like "arg1".
func ExtractParamNames(line, funcName string) []string {
	idx := strings.Index(line, funcName)
	if idx < 0 {
		return nil
	}
	rest := line[idx+len(funcName):]
	parenIdx := strings.IndexByte(rest, '(')
	if parenIdx < 0 {
		return nil
	}

	inside := rest[parenIdx+1:]
	depth := 1
	var end int
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

	var params []string
	depth = 0
	start := 0
	for i := 0; i < len(paramStr); i++ {
		switch paramStr[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '<':
			if i+1 < len(paramStr) && paramStr[i+1] == '<' {
				depth++
				i++
			}
		case '>':
			if i+1 < len(paramStr) && paramStr[i+1] == '>' {
				depth--
				i++
			}
		case ',':
			if depth == 0 {
				params = append(params, strings.TrimSpace(paramStr[start:i]))
				start = i + 1
			}
		}
	}
	params = append(params, strings.TrimSpace(paramStr[start:]))

	var names []string
	for i, p := range params {
		if bsIdx := strings.Index(p, "\\\\"); bsIdx >= 0 {
			p = strings.TrimSpace(p[:bsIdx])
		}
		name := scanParamName(p, i)
		names = append(names, name)
	}
	return names
}

// JoinParams returns a comma-separated string of the first `arity` parameter
// names extracted from a function definition. Returns "" when names is nil or
// shorter than arity.
func JoinParams(names []string, arity int) string {
	if names == nil || arity > len(names) {
		return ""
	}
	return strings.Join(names[:arity], ",")
}

func scanParamName(param string, index int) string {
	param = strings.TrimSpace(param)
	if name := ScanFuncName(param); name != "" && name != "_" {
		return name
	}
	// Handle "pattern = variable" (e.g. %User{} = user, [_ | _] = list)
	if eqIdx := strings.LastIndex(param, "="); eqIdx >= 0 {
		after := strings.TrimSpace(param[eqIdx+1:])
		if name := ScanFuncName(after); name != "" && name != "_" {
			return name
		}
	}
	return "arg" + strconv.Itoa(index+1)
}

// skipStringLiteral advances past a string or charlist literal starting at
// position i in s (where s[i] is '"' or '\"). Returns the index of the
// closing quote character so that the outer for-loop's i++ lands past it.
func skipStringLiteral(s string, i int) int {
	quote := s[i]
	for j := i + 1; j < len(s); j++ {
		if s[j] == '\\' {
			j++ // skip escaped character
			continue
		}
		if s[j] == quote {
			return j
		}
	}
	return len(s) - 1
}

func resolveModule(s, currentModule string) string {
	if currentModule != "" {
		return strings.ReplaceAll(s, "__MODULE__", currentModule)
	}
	return s
}

// ResolveModuleRef resolves a module reference through aliases and __MODULE__.
// Returns "" if the reference contains unresolvable __MODULE__.
func ResolveModuleRef(modRef string, aliases map[string]string, currentModule string) string {
	resolved := modRef
	if full, ok := aliases[modRef]; ok {
		resolved = full
	} else if parts := strings.SplitN(modRef, ".", 2); len(parts) == 2 {
		if full, ok := aliases[parts[0]]; ok {
			resolved = full + "." + parts[1]
		}
	}
	resolved = resolveModule(resolved, currentModule)
	if strings.Contains(resolved, "__MODULE__") {
		return ""
	}
	return resolved
}

func copyMap(m map[string]string) map[string]string {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func copyBoolMap(m map[string]bool) map[string]bool {
	cp := make(map[string]bool, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func IsElixirFile(path string) bool {
	extension := filepath.Ext(path)
	return extension == ".ex" || extension == ".exs"
}

// WalkElixirFiles walks root, skipping _build/.git/node_modules directories,
// and calls fn for each .ex/.exs file found.
func WalkElixirFiles(root string, fn func(path string, d fs.DirEntry) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if base == "_build" || base == ".git" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !IsElixirFile(path) {
			return nil
		}
		return fn(path, d)
	})
}
