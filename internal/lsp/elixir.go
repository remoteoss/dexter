package lsp

import (
	"regexp"
	"strings"
	"unicode"

	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
)

// ExtractExpression returns the full dotted expression around the cursor position.
// Line is the text content, col is 0-based character offset.
// For example, on "  Foo.Bar.baz(123)" with col=9, returns "Foo.Bar.baz".
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

	isExprChar := func(b byte) bool {
		c := rune(b)
		return unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '.' || c == '?' || c == '!'
	}

	// If cursor is not on an expression character, return empty
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

	return line[start : end+1]
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

var (
	aliasMultiRe    = regexp.MustCompile(`^\s*alias\s+([A-Za-z0-9_.]+)\.{([^}]+)}`)
	importRe        = regexp.MustCompile(`^\s*import\s+([A-Za-z0-9_.]+)`)
	defmoduleRe     = regexp.MustCompile(`^\s*defmodule\s+([A-Za-z0-9_.]+)\s+do`)
	moduleAttrDefRe = regexp.MustCompile(`^\s*@([a-z_][a-z0-9_]*)\s+[^@]`)
)

// extractCurrentModule returns the first defmodule name found in the text.
func extractCurrentModule(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if m := defmoduleRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// ExtractAliases parses all alias declarations from document text.
// Returns a map of short name -> full module name.
// Handles: "alias A.B.C", "alias A.B.C, as: D", "alias A.B.{C, D}", and __MODULE__ references.
func ExtractAliases(text string) map[string]string {
	currentModule := extractCurrentModule(text)
	resolve := func(s string) string {
		if currentModule != "" {
			return strings.ReplaceAll(s, "__MODULE__", currentModule)
		}
		return s
	}

	aliases := make(map[string]string)
	for _, line := range strings.Split(text, "\n") {
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
