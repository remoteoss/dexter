package lsp

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
)

// findTokenColumn returns the start column (0-based) of the first whole-token
// occurrence of token in lineText, or -1 if not found.
//
// A "whole token" means the match is not immediately preceded or followed by
// an identifier character, so "foo" does not match inside "foo_bar".
func findTokenColumn(lineText, token string) int {
	cols := findAllTokenColumns(lineText, token)
	if len(cols) == 0 {
		return -1
	}
	return cols[0]
}

// findAllTokenColumns returns the start columns (0-based) of all non-overlapping
// whole-token occurrences of token in lineText.
func findAllTokenColumns(lineText, token string) []int {
	if token == "" {
		return nil
	}
	var cols []int
	start := 0
	for {
		idx := strings.Index(lineText[start:], token)
		if idx < 0 {
			break
		}
		abs := start + idx
		if !isTokenBoundary(lineText, abs, len(token)) {
			start = abs + 1
			continue
		}
		cols = append(cols, abs)
		start = abs + len(token)
	}
	return cols
}

// findFunctionTokenColumns returns columns for token occurrences that are
// function references (calls, definitions, specs), filtering out keyword-syntax
// occurrences like `resource_type: value` where the token is an atom key.
// A keyword occurrence is one where ':' immediately follows the token (but not
// '::' which is a type separator).
func findFunctionTokenColumns(lineText, token string) []int {
	cols := findAllTokenColumns(lineText, token)
	var result []int
	for _, col := range cols {
		end := col + len(token)
		if end < len(lineText) && lineText[end] == ':' && (end+1 >= len(lineText) || lineText[end+1] != ':') {
			continue
		}
		result = append(result, col)
	}
	return result
}

// isTokenBoundary returns true when the substring [pos, pos+length) in s is
// not immediately preceded or followed by an identifier character.
func isTokenBoundary(s string, pos, length int) bool {
	if pos > 0 {
		r, _ := utf8.DecodeLastRuneInString(s[:pos])
		if r != utf8.RuneError && isRenameIdentChar(r) {
			return false
		}
	}
	end := pos + length
	if end < len(s) {
		r, _ := utf8.DecodeRuneInString(s[end:])
		if r != utf8.RuneError && isRenameIdentChar(r) {
			return false
		}
	}
	return true
}

// isRenameIdentChar returns true for characters that can appear inside an
// Elixir identifier (letters, digits, underscore, ?, !).
func isRenameIdentChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '?' || r == '!'
}

// camelToSnake converts a CamelCase string to snake_case, treating consecutive
// uppercase runs as acronyms.
// Examples:
//
//	"Accounts"    → "accounts"
//	"SomeUser"    → "some_user"
//	"MyApp"       → "my_app"
//	"HTTPClient"  → "http_client"
//	"MyHTTPClient"→ "my_http_client"
func camelToSnake(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if unicode.IsUpper(r) {
			// Insert underscore if:
			// 1. Not the first character, AND
			// 2. Either preceded by a lowercase/digit (word→Word boundary),
			//    OR preceded by uppercase AND followed by lowercase (acronym→Word: HTTPClient → http_client).
			if i > 0 {
				prevLower := !unicode.IsUpper(runes[i-1])
				nextLower := i+1 < len(runes) && !unicode.IsUpper(runes[i+1])
				if prevLower || (unicode.IsUpper(runes[i-1]) && nextLower) {
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// moduleLastSegment returns the last dot-separated segment of a module name.
// "MyApp.Accounts.User" → "User"
func moduleLastSegment(module string) string {
	if dot := strings.LastIndexByte(module, '.'); dot >= 0 {
		return module[dot+1:]
	}
	return module
}

// moduleToExpectedBase returns the conventional base name (without extension)
// for the last segment of a module name (CamelCase → snake_case).
// "MyApp.Accounts" → "accounts"
// "MyApp.SomeUser" → "some_user"
func moduleToExpectedBase(module string) string {
	return camelToSnake(moduleLastSegment(module))
}

// fileMatchesModuleConvention returns true if the file's base name (ignoring
// extension) matches the expected snake_case name for the module. Handles
// both .ex and .exs files.
func fileMatchesModuleConvention(filePath, module string) bool {
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	nameWithoutExt := strings.TrimSuffix(base, ext)
	return nameWithoutExt == moduleToExpectedBase(module)
}

// conventionalNewPath returns the new file path after a module rename,
// preserving the original file extension (.ex or .exs).
func conventionalNewPath(filePath, oldModule, newModule string) string {
	ext := filepath.Ext(filePath)
	return filepath.Join(filepath.Dir(filePath), moduleToExpectedBase(newModule)+ext)
}

// isValidFunctionName returns true if name is a valid Elixir function name.
func isValidFunctionName(name string) bool {
	return len(name) > 0 && parser.ScanFuncName(name) == name
}

var delegateAsLineRe = regexp.MustCompile(`,?\s*as:\s*:[a-z_][a-z0-9_?!]*`)
var delegateAsValueRe = regexp.MustCompile(`(as:\s*):([a-z_][a-z0-9_?!]*)`)

// delegateStatementSpan returns the range of lines [startLine, endLine) that
// make up a multi-line defdelegate statement starting at startLine (0-based).
// A continuation line starts with whitespace followed by a keyword arg (to:, as:).
func delegateStatementSpan(lines []string, startLine int) (int, int) {
	end := startLine + 1
	for end < len(lines) {
		trimmed := strings.TrimSpace(lines[end])
		if strings.HasPrefix(trimmed, "to:") || strings.HasPrefix(trimmed, "as:") {
			end++
		} else {
			break
		}
	}
	return startLine, end
}

// updateDelegateAs modifies a defdelegate statement (possibly multi-line) to
// add, update, or remove the `as:` option so that the delegate points to
// newTargetName. If facadeName == newTargetName, any existing as: is removed.
//
// lines is the full file content; startLine is the 0-based line where the
// defdelegate begins. Returns the replacement lines for the statement span.
func updateDelegateAs(lines []string, startLine int, facadeName, newTargetName string) (updatedLines []string, spanStart, spanEnd int) {
	spanStart, spanEnd = delegateStatementSpan(lines, startLine)
	span := make([]string, spanEnd-spanStart)
	copy(span, lines[spanStart:spanEnd])

	if facadeName == newTargetName {
		// Remove as: clause from whichever line has it
		for i, line := range span {
			span[i] = delegateAsLineRe.ReplaceAllString(line, "")
		}
		// Remove lines that are now empty (contained only as: :name)
		var filtered []string
		for _, line := range span {
			if strings.TrimSpace(line) != "" {
				filtered = append(filtered, line)
			}
		}
		return filtered, spanStart, spanEnd
	}

	// Check if any line in the span has an existing as:
	for i, line := range span {
		if delegateAsValueRe.MatchString(line) {
			span[i] = delegateAsValueRe.ReplaceAllString(line, "${1}:"+newTargetName)
			return span, spanStart, spanEnd
		}
	}

	// No existing as: — append to the last line of the statement
	lastIdx := len(span) - 1
	trimmed := strings.TrimRight(span[lastIdx], " \t\r\n")
	span[lastIdx] = trimmed + ", as: :" + newTargetName
	return span, spanStart, spanEnd
}

// isValidModuleName returns true if name is a valid Elixir module name.
func isValidModuleName(name string) bool {
	if len(name) == 0 {
		return false
	}
	for _, segment := range strings.Split(name, ".") {
		if len(segment) == 0 {
			return false
		}
		if !unicode.IsUpper(rune(segment[0])) {
			return false
		}
		for _, r := range segment[1:] {
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
				return false
			}
		}
	}
	return true
}
