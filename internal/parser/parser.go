package parser

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Shared regex patterns used by both the parser and the LSP.
var (
	AliasRe   = regexp.MustCompile(`^\s*alias\s+([A-Za-z0-9_.]+)`)
	AliasAsRe = regexp.MustCompile(`^\s*alias\s+([A-Za-z0-9_.]+)\s*,\s*as:\s*([A-Za-z0-9_]+)`)
	FuncDefRe = regexp.MustCompile(`^\s*(defp?|defmacrop?|defguardp?|defdelegate)\s+([a-z_][a-z0-9_?!]*)[\s(,]`)
	TypeDefRe = regexp.MustCompile(`^\s*@(typep?|opaque)\s+([a-z_][a-z0-9_?!]*)`)
)

var (
	DefmoduleRe    = regexp.MustCompile(`^\s*defmodule\s+([A-Za-z0-9_.]+)\s+do`)
	delegateToRe   = regexp.MustCompile(`to:\s*([A-Za-z0-9_.]+)`)
	delegateAsRe   = regexp.MustCompile(`as:\s*:?([a-z_][a-z0-9_?!]*)`)
	newStatementRe = regexp.MustCompile(`^\s*(defdelegate|defp?|defmacrop?|defguardp?|alias|import|@|end)\b`)
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

// Line represents a line of source code with its content and original line number.
// The original line number is the line (1-indexed) where this line appears in the source file,
// or for joined lines, the line where the joined group begins.
type Line struct {
	Content string
	OrigNum int // 1-indexed original line number
}

// joinContinuedLines joins lines ending with a bare backslash (line continuation)
// with the next line. A trailing \\ only counts as continuation when it is outside
// string literals and comments. Returns a slice of Line structs with original line numbers.
func joinContinuedLines(lines []string) []Line {
	var result []Line
	i := 0
	for i < len(lines) {
		origNum := i + 1 // 1-indexed
		line := lines[i]
		for hasTrailingBackslash(line) && i+1 < len(lines) {
			i++
			line = strings.TrimRight(line[:len(line)-1], " \t") + " " + lines[i]
		}
		result = append(result, Line{Content: line, OrigNum: origNum})
		i++
	}
	return result
}

// JoinContinuedLines is the exported version that returns just strings.
// Used by tests and other code that doesn't need line number tracking.
// JoinLines applies all three joining passes (continued lines, bracket lines,
// trailing comma) and returns the joined content strings. This is the same
// pipeline used by ParseText.
func JoinLines(raw []string) []string {
	joined := joinContinuedLines(raw)
	joined = joinBracketLines(joined)
	joined = joinTrailingComma(joined)
	result := make([]string, len(joined))
	for i, l := range joined {
		result[i] = l.Content
	}
	return result
}

func JoinContinuedLines(lines []string) []string {
	joined := joinContinuedLines(lines)
	result := make([]string, len(joined))
	for i, l := range joined {
		result[i] = l.Content
	}
	return result
}

// hasTrailingBackslash returns true if line ends with a \\ that is outside strings/comments.
func hasTrailingBackslash(line string) bool {
	j := len(line) - 1
	for j >= 0 && (line[j] == ' ' || line[j] == '\t' || line[j] == '\r') {
		j--
	}
	if j < 0 || line[j] != '\\' {
		return false
	}
	inSingle := false
	inDouble := false
	for k := 0; k < j; k++ {
		ch := line[k]
		if ch == '\\' && (inSingle || inDouble) {
			k++
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
		} else if ch == '\'' && !inDouble {
			inSingle = !inSingle
		}
		if ch == '#' && !inSingle && !inDouble {
			return false
		}
	}
	return !inSingle && !inDouble
}

// joinBracketLines joins lines that have unclosed brackets (parentheses, square
// brackets, or curly braces). Brackets inside string literals, charlists,
// comments, and sigils are ignored. The joined line retains the line number of
// the first line in the group.
func joinBracketLines(lines []Line) []Line {
	var result []Line
	i := 0
	for i < len(lines) {
		origNum := lines[i].OrigNum // Keep the first line's original number
		line := lines[i].Content
		depth := bracketDepth(line)
		for depth > 0 && i+1 < len(lines) {
			i++
			line = line + " " + lines[i].Content
			depth = bracketDepth(line)
		}
		result = append(result, Line{Content: line, OrigNum: origNum})
		i++
	}
	return result
}

// JoinBracketLines is the exported version that takes and returns strings.
// Used by tests and other code that doesn't need line number tracking.
func JoinBracketLines(lines []string) []string {
	lineObjs := make([]Line, len(lines))
	for i, l := range lines {
		lineObjs[i] = Line{Content: l, OrigNum: i + 1}
	}
	joined := joinBracketLines(lineObjs)
	result := make([]string, len(joined))
	for i, l := range joined {
		result[i] = l.Content
	}
	return result
}

// joinTrailingComma joins lines where a directive (alias, import, use, require)
// ends with a trailing comma and the next line is a continuation (starts with
// whitespace followed by a keyword or keyword-like argument). This handles
// multi-line constructs like:
//
//	alias MyModule.MySubModule,
//	  as: Something
//
//	use SomeModule,
//	  key: Val
//
// These have no unclosed brackets, so joinBracketLines doesn't catch them.
func joinTrailingComma(lines []Line) []Line {
	var result []Line
	i := 0
	for i < len(lines) {
		origNum := lines[i].OrigNum
		content := lines[i].Content

		if isDirectiveWithTrailingComma(content) {
			// Join with subsequent continuation lines
			for i+1 < len(lines) {
				next := lines[i+1].Content
				trimmed := strings.TrimSpace(next)
				// Skip blank lines — they signal a new statement
				if trimmed == "" {
					break
				}
				// Skip comment-only lines inside the continuation
				if strings.HasPrefix(trimmed, "#") {
					i++
					continue
				}
				if !LooksLikeKeywordOpt(trimmed) {
					break
				}
				content = content + " " + trimmed
				i++
			}
		}

		result = append(result, Line{Content: content, OrigNum: origNum})
		i++
	}
	return result
}

// directivePrefixes are the keywords that can have multi-line comma continuations.
var directivePrefixes = []string{"alias ", "import ", "use ", "require "}

// isDirectiveWithTrailingComma returns true if the line starts with a directive
// keyword and ends with a comma (outside strings/comments), with no unclosed brackets.
func isDirectiveWithTrailingComma(line string) bool {
	trimmed := strings.TrimLeft(line, " \t")
	for _, prefix := range directivePrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			// Check the line ends with a trailing comma (outside strings/comments)
			stripped := StripCommentsAndStrings(trimmed)
			stripped = strings.TrimRight(stripped, " \t\r")
			return strings.HasSuffix(stripped, ",")
		}
	}
	return false
}

// LooksLikeKeywordOpt returns true if trimmed starts with a lowercase
// identifier followed by ':' (e.g. "as: Something", "name: \"foo\"").
// Used by both the parser's line joining and the LSP's multiline use opts.
func LooksLikeKeywordOpt(trimmed string) bool {
	for i := 0; i < len(trimmed); i++ {
		ch := trimmed[i]
		if ch == ':' && i > 0 {
			return true
		}
		if !IsLowerIdentChar(ch) {
			return false
		}
	}
	return false
}

// bracketDepth returns the net bracket depth of a line: positive if there are
// unclosed brackets. Only counts (, [, { outside strings, charlists, sigils,
// and comments. Handles <<>> bitstring syntax.
func bracketDepth(line string) int {
	depth := 0
	i := 0
	for i < len(line) {
		ch := line[i]
		// Skip escaped characters inside strings/charlists
		if ch == '\\' && i+1 < len(line) {
			i += 2
			continue
		}
		// Comment — stop counting
		if ch == '#' {
			break
		}
		// Double-quoted string
		if ch == '"' {
			i = skipQuoted(line, i+1, '"')
			continue
		}
		// Single-quoted charlist
		if ch == '\'' {
			i = skipQuoted(line, i+1, '\'')
			continue
		}
		// Sigil
		if ch == '~' && i+1 < len(line) {
			next := line[i+1]
			if (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') {
				i = skipSigil(line, i+2)
				continue
			}
		}
		// Bitstring << >>
		if ch == '<' && i+1 < len(line) && line[i+1] == '<' {
			// Skip over <<, but don't count as bracket
			i += 2
			continue
		}
		if ch == '>' && i+1 < len(line) && line[i+1] == '>' {
			i += 2
			continue
		}
		switch ch {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		}
		i++
	}
	return depth
}

// skipQuoted advances past a quoted literal content until the closing quote.
func skipQuoted(line string, i int, quote byte) int {
	for i < len(line) {
		if line[i] == '\\' && i+1 < len(line) {
			i += 2
			continue
		}
		if line[i] == quote {
			return i + 1
		}
		// Interpolation in double-quoted strings
		if quote == '"' && line[i] == '#' && i+1 < len(line) && line[i+1] == '{' {
			i += 2
			// Track nested braces in interpolation
			braceDepth := 1
			for i < len(line) && braceDepth > 0 {
				if line[i] == '\\' && i+1 < len(line) {
					i += 2
					continue
				}
				switch line[i] {
				case '{':
					braceDepth++
				case '}':
					braceDepth--
				}
				if braceDepth > 0 {
					i++
				}
			}
			continue
		}
		i++
	}
	return i
}

// skipSigil advances past a sigil's content until the closing delimiter.
func skipSigil(line string, i int) int {
	if i >= len(line) {
		return i
	}
	// Determine delimiter
	ch := line[i]
	var openDelim, closeDelim byte
	switch ch {
	case '(', '[', '{', '<':
		openDelim = ch
		switch ch {
		case '(':
			closeDelim = ')'
		case '[':
			closeDelim = ']'
		case '{':
			closeDelim = '}'
		case '<':
			closeDelim = '>'
		}
	default:
		// Non-bracket delimiter (e.g. /, |, ")
		closeDelim = ch
		openDelim = ch
	}
	i++ // skip opening delimiter
	depth := 1
	for i < len(line) && depth > 0 {
		if line[i] == '\\' && i+1 < len(line) {
			i += 2
			continue
		}
		if openDelim != closeDelim {
			switch line[i] {
			case openDelim:
				depth++
			case closeDelim:
				depth--
			}
		} else {
			if line[i] == closeDelim {
				depth--
			}
		}
		if depth > 0 {
			i++
		}
	}
	if i < len(line) {
		i++ // skip closing delimiter
	}
	// Skip optional modifiers (e.g. ~r/pattern/s)
	for i < len(line) && ((line[i] >= 'a' && line[i] <= 'z') || (line[i] >= 'A' && line[i] <= 'Z')) {
		i++
	}
	return i
}

// ParseText parses Elixir source text and returns definitions and references.
// The path is used to populate FilePath fields but the text is not read from disk.
func ParseText(path, text string) ([]Definition, []Reference, error) {
	type moduleFrame struct {
		name           string
		depth          int // do..end/fn..end nesting depth when this module was opened
		savedAliases   map[string]string
		savedInjectors map[string]bool
	}

	origLines := strings.Split(text, "\n")
	lines := joinContinuedLines(origLines)
	lines = joinBracketLines(lines)
	lines = joinTrailingComma(lines)
	var defs []Definition
	var refs []Reference
	var moduleStack []moduleFrame
	depth := 0
	aliases := map[string]string{} // short name -> full module
	injectors := map[string]bool{} // modules from use/import that inject bare functions
	inHeredoc := false

	// Multi-line sigil tracking: when a sigil opener (~X with bracket delimiter)
	// doesn't close on the same line, we track the closing delimiter and depth
	// so subsequent lines inside the sigil are skipped.
	var sigilCloser byte
	var sigilDepth int

	for lineIdx, line := range lines {
		lineNum := line.OrigNum // Use original line number, not joined index
		content := line.Content

		// Skip lines inside a multi-line sigil
		if sigilCloser != 0 {
			for j := 0; j < len(content); j++ {
				if content[j] == '\\' && j+1 < len(content) {
					j++
					continue
				}
				if content[j] == sigilCloser {
					sigilDepth--
					if sigilDepth == 0 {
						sigilCloser = 0
						break
					}
				} else if isSigilBracketOpener(content[j], sigilCloser) {
					sigilDepth++
				}
			}
			if sigilCloser != 0 {
				continue // sigil still open, skip this line
			}
			// Sigil closed on this line — it's just a closing delimiter, skip it
			continue
		}

		// Check for a sigil opener that doesn't close on this line.
		// Must run before CheckHeredoc so ~s""" is treated as a sigil, not heredoc.
		if opener, ok := findUnclosedSigil(content); ok {
			sigilCloser = opener.closer
			sigilDepth = opener.depth
			continue
		}

		var skip bool
		inHeredoc, skip = CheckHeredoc(content, inHeredoc)
		if skip {
			continue
		}

		// Find first non-whitespace character for fast pre-filtering.
		trimStart := 0
		for trimStart < len(content) && (content[trimStart] == ' ' || content[trimStart] == '\t') {
			trimStart++
		}
		if trimStart >= len(content) {
			continue
		}
		first := content[trimStart]
		rest := content[trimStart:] // line content from first non-whitespace char

		strippedRest := strings.TrimRight(StripCommentsAndStrings(rest), " \t\r")

		// 'e' — check for "end" to pop module stack; otherwise fall through
		if first == 'e' {
			if IsEnd(strippedRest) {
				if len(moduleStack) > 0 && moduleStack[len(moduleStack)-1].depth == depth {
					frame := moduleStack[len(moduleStack)-1]
					moduleStack = moduleStack[:len(moduleStack)-1]
					aliases = frame.savedAliases
					injectors = frame.savedInjectors
				}
				depth--
				if depth < 0 {
					depth = 0
				}
				continue
			}
			// Not "end" — may be a bare macro call like "embedded_schema do"
		}

		// Track block-opening keywords (do..end and fn..end) for depth counting.
		// This covers defmodule/def/defp/case/cond/fn/etc. — any construct closed by "end".
		if OpensBlock(strippedRest) {
			depth++
		}

		currentModule := ""
		if len(moduleStack) > 0 {
			currentModule = moduleStack[len(moduleStack)-1].name
		}

		// 'a' — alias tracking (+ emit alias ref)
		if first == 'a' {
			if strings.HasPrefix(rest, "alias") && len(rest) > 5 && (rest[5] == ' ' || rest[5] == '\t') {
				afterAlias := strings.TrimLeft(rest[5:], " \t")
				moduleName := ScanModuleName(afterAlias)
				if moduleName != "" {
					remaining := afterAlias[len(moduleName):]
					remaining = strings.TrimLeft(remaining, " \t")

					// Multi-alias: alias MyApp.{Accounts, Users}
					// ScanModuleName consumes the trailing "." so remaining starts with "{"
					if strings.HasPrefix(remaining, "{") {
						braceEnd := strings.IndexByte(remaining, '}')
						if braceEnd >= 0 {
							inner := remaining[1:braceEnd]
							// Trim trailing dot from parent module name
							parent := strings.TrimRight(moduleName, ".")
							parentResolved := resolveModule(parent, currentModule)
							for _, segment := range strings.Split(inner, ",") {
								segment = strings.TrimSpace(segment)
								childName := ScanModuleName(segment)
								if childName != "" {
									fullChild := parentResolved + "." + childName
									aliasKey := childName
									if dot := strings.LastIndexByte(childName, '.'); dot >= 0 {
										aliasKey = childName[dot+1:]
									}
									aliases[aliasKey] = fullChild
									if !strings.Contains(fullChild, "__MODULE__") {
										refs = append(refs, Reference{Module: fullChild, Line: lineNum, FilePath: path, Kind: "alias"})
									}
								}
							}
							continue
						}
					}

					if strings.HasPrefix(remaining, ", as:") {
						asStr := strings.TrimLeft(remaining[5:], " \t") // skip ", as:"
						asName := scanIdentifier(asStr)
						if asName != "" {
							resolved := resolveModule(moduleName, currentModule)
							if !strings.Contains(resolved, "__MODULE__") {
								aliases[asName] = resolved
								refs = append(refs, Reference{Module: resolved, Line: lineNum, FilePath: path, Kind: "alias"})
							}
						}
					} else if strings.HasPrefix(remaining, ",as:") {
						asStr := strings.TrimLeft(remaining[4:], " \t") // skip ",as:"
						asName := scanIdentifier(asStr)
						if asName != "" {
							resolved := resolveModule(moduleName, currentModule)
							if !strings.Contains(resolved, "__MODULE__") {
								aliases[asName] = resolved
								refs = append(refs, Reference{Module: resolved, Line: lineNum, FilePath: path, Kind: "alias"})
							}
						}
					} else {
						resolved := resolveModule(moduleName, currentModule)
						dot := strings.LastIndexByte(resolved, '.')
						var shortName string
						if dot >= 0 {
							shortName = resolved[dot+1:]
						} else {
							shortName = resolved
						}
						aliases[shortName] = resolved
						if !strings.Contains(resolved, "__MODULE__") {
							refs = append(refs, Reference{Module: resolved, Line: lineNum, FilePath: path, Kind: "alias"})
						}
					}
					continue
				}
			}
			// Not an alias — fall through to ref extraction
			goto extractCallRefs
		}

		// 'i' — import (refs only)
		if first == 'i' {
			if strings.HasPrefix(rest, "import") && len(rest) > 6 && (rest[6] == ' ' || rest[6] == '\t') {
				afterImport := strings.TrimLeft(rest[6:], " \t")
				moduleName := ScanModuleName(afterImport)
				if moduleName != "" {
					resolved := resolveModule(moduleName, currentModule)
					if !strings.Contains(resolved, "__MODULE__") {
						refs = append(refs, Reference{Module: resolved, Line: lineNum, FilePath: path, Kind: "import"})
						injectors[resolved] = true
					}
					continue
				}
			}
			goto extractCallRefs
		}

		// 'u' — use (refs only)
		if first == 'u' {
			if strings.HasPrefix(rest, "use") && len(rest) > 3 && (rest[3] == ' ' || rest[3] == '\t') {
				afterUse := strings.TrimLeft(rest[3:], " \t")
				moduleName := ScanModuleName(afterUse)
				if moduleName != "" {
					resolved := resolveModule(moduleName, currentModule)
					if !strings.Contains(resolved, "__MODULE__") {
						refs = append(refs, Reference{Module: resolved, Line: lineNum, FilePath: path, Kind: "use"})
						injectors[resolved] = true
					}
					continue
				}
			}
			goto extractCallRefs
		}

		// '@' — type definitions (@type, @opaque) and @behaviour refs
		// @typep is private-to-file and not indexed.
		if first == '@' {
			if currentModule != "" {
				var kind string
				var afterKw string
				if strings.HasPrefix(rest, "@type") && len(rest) > 5 && (rest[5] == ' ' || rest[5] == '\t') {
					kind = "type"
					afterKw = strings.TrimLeft(rest[5:], " \t")
				} else if strings.HasPrefix(rest, "@opaque") && len(rest) > 7 && (rest[7] == ' ' || rest[7] == '\t') {
					kind = "opaque"
					afterKw = strings.TrimLeft(rest[7:], " \t")
				}
				if kind != "" {
					name := ScanFuncName(afterKw)
					if name != "" {
						defs = append(defs, Definition{
							Module:   currentModule,
							Function: name,
							Arity:    ExtractArity(content, name),
							Line:     lineNum,
							FilePath: path,
							Kind:     kind,
						})
					}
				}

				// @behaviour ModuleName — record as a ref so module renames update it
				if strings.HasPrefix(rest, "@behaviour") && len(rest) > 10 && (rest[10] == ' ' || rest[10] == '\t') {
					afterBehaviour := strings.TrimLeft(rest[10:], " \t")
					moduleName := ScanModuleName(afterBehaviour)
					if moduleName != "" {
						resolved := resolveModule(moduleName, currentModule)
						if !strings.Contains(resolved, "__MODULE__") {
							refs = append(refs, Reference{Module: resolved, Line: lineNum, FilePath: path, Kind: "behaviour"})
						}
					}
				}

				// @callback/@macrocallback — index as definitions for go-to-declaration and go-to-implementation.
				// Check @macrocallback first since it shares a prefix with @callback.
				var callbackKind string
				var afterCallbackKw string
				if strings.HasPrefix(rest, "@macrocallback") && len(rest) > 14 && (rest[14] == ' ' || rest[14] == '\t') {
					callbackKind = "macrocallback"
					afterCallbackKw = strings.TrimLeft(rest[14:], " \t")
				} else if strings.HasPrefix(rest, "@callback") && len(rest) > 9 && (rest[9] == ' ' || rest[9] == '\t') {
					callbackKind = "callback"
					afterCallbackKw = strings.TrimLeft(rest[9:], " \t")
				}
				if callbackKind != "" {
					name := ScanFuncName(afterCallbackKw)
					if name != "" {
						defs = append(defs, Definition{
							Module:   currentModule,
							Function: name,
							Arity:    ExtractArity(content, name),
							Line:     lineNum,
							FilePath: path,
							Kind:     callbackKind,
						})
					}
				}

			}
			// Don't continue — fall through to extractCallRefs so that module
			// references in @spec/@type/@callback annotations are captured
			// (e.g. User.t() in "@spec get_user() :: User.t()").
			goto extractCallRefs
		}

		// 'd' — defmodule, defprotocol, defimpl, def*, defstruct, defexception
		if first == 'd' && strings.HasPrefix(rest, "def") {
			if name, ok := scanDefKeyword(rest, "defmodule"); ok {
				if !strings.Contains(name, ".") && currentModule != "" {
					name = currentModule + "." + name
				}
				currentModule = name
				moduleStack = append(moduleStack, moduleFrame{name: currentModule, depth: depth, savedAliases: copyMap(aliases), savedInjectors: copyBoolMap(injectors)})
				defs = append(defs, Definition{
					Module:   currentModule,
					Line:     lineNum,
					FilePath: path,
					Kind:     "module",
				})
				continue
			}

			if name, ok := scanDefKeyword(rest, "defprotocol"); ok {
				if !strings.Contains(name, ".") && currentModule != "" {
					name = currentModule + "." + name
				}
				currentModule = name
				moduleStack = append(moduleStack, moduleFrame{name: currentModule, depth: depth, savedAliases: copyMap(aliases), savedInjectors: copyBoolMap(injectors)})
				defs = append(defs, Definition{
					Module:   currentModule,
					Line:     lineNum,
					FilePath: path,
					Kind:     "defprotocol",
				})
				continue
			}

			if name, ok := scanDefKeyword(rest, "defimpl"); ok {
				if !strings.Contains(name, ".") && currentModule != "" {
					name = currentModule + "." + name
				}
				currentModule = name
				moduleStack = append(moduleStack, moduleFrame{name: currentModule, depth: depth, savedAliases: copyMap(aliases), savedInjectors: copyBoolMap(injectors)})
				defs = append(defs, Definition{
					Module:   currentModule,
					Line:     lineNum,
					FilePath: path,
					Kind:     "defimpl",
				})
				continue
			}

			if currentModule != "" {
				if kind, funcName, ok := ScanFuncDef(rest); ok {
					paramContent := FindParamContent(content, funcName)
					maxArity := ArityFromParams(paramContent)
					defaultCount := DefaultsFromParams(paramContent)
					minArity := maxArity - defaultCount

					var delegateTo, delegateAs string
					if kind == "defdelegate" {
						delegateTo, delegateAs = findDelegateToAndAs(lines, lineIdx, aliases, currentModule)
					}

					allParamNames := ExtractParamNames(content, funcName)

					for arity := minArity; arity <= maxArity; arity++ {
						params := JoinParams(allParamNames, arity)
						defs = append(defs, Definition{
							Module:     currentModule,
							Function:   funcName,
							Arity:      arity,
							Line:       lineNum,
							FilePath:   path,
							Kind:       kind,
							DelegateTo: delegateTo,
							DelegateAs: delegateAs,
							Params:     params,
						})
					}
					// Don't continue — line may contain refs like: def foo, do: Repo.all()
					goto extractCallRefs
				}

				if strings.HasPrefix(rest, "defstruct ") || strings.HasPrefix(rest, "defstruct\t") {
					defs = append(defs, Definition{
						Module:   currentModule,
						Function: "__struct__",
						Line:     lineNum,
						FilePath: path,
						Kind:     "defstruct",
					})
				}
				if strings.HasPrefix(rest, "defexception ") || strings.HasPrefix(rest, "defexception\t") {
					defs = append(defs, Definition{
						Module:   currentModule,
						Function: "__exception__",
						Line:     lineNum,
						FilePath: path,
						Kind:     "defexception",
					})
				}
			}
			// Fall through to ref extraction for lines like: def foo, do: Mod.func()
		}

	extractCallRefs:
		// Detect bare calls to functions/macros from use'd/import'd modules.
		// Detect bare calls to functions/macros from use'd/import'd modules.
		if currentModule != "" && len(injectors) > 0 {
			trimmedRest := strings.TrimRight(rest, " \t\r")
			if strings.HasSuffix(trimmedRest, " do") || strings.HasSuffix(trimmedRest, "\tdo") {
				// Macro call with do block: embedded_schema do, schema "t" do, test "x" do
				name := ScanFuncName(rest)
				if name != "" && !elixirKeyword[name] {
					for mod := range injectors {
						refs = append(refs, Reference{Module: mod, Function: name, Line: lineNum, FilePath: path, Kind: "call"})
					}
				}
			} else {
				// DSL call at line start: field :name, :string  or  cast(struct, params)
				name := ScanFuncName(rest)
				if name != "" && !elixirKeyword[name] {
					after := rest[len(name):]
					if len(after) > 0 && (after[0] == '(' || (after[0] == ' ' && len(after) > 1 && after[1] == ':')) {
						for mod := range injectors {
							refs = append(refs, Reference{Module: mod, Function: name, Line: lineNum, FilePath: path, Kind: "call"})
						}
					}
				}
				// Pipe call: |> cast_embed(:jobs), |> validate_required(...)
				if idx := strings.Index(rest, "|>"); idx >= 0 {
					afterPipe := strings.TrimLeft(rest[idx+2:], " \t")
					name := ScanFuncName(afterPipe)
					if name != "" && !elixirKeyword[name] {
						for mod := range injectors {
							refs = append(refs, Reference{Module: mod, Function: name, Line: lineNum, FilePath: path, Kind: "call"})
						}
					}
				}
			}
		}

		// Extract Module.function calls and %Module{} struct literals from any line.
		if !hasUppercase(content) {
			continue
		}
		{
			codeLine := StripCommentsAndStrings(content)

			// Module.function calls (including type refs like User.t())
			for _, match := range moduleCallRe.FindAllStringSubmatch(codeLine, -1) {
				modRef, funcName := match[1], match[2]
				if elixirKeyword[funcName] {
					continue
				}
				resolved := ResolveModuleRef(modRef, aliases, currentModule)
				if resolved != "" {
					refs = append(refs, Reference{Module: resolved, Function: funcName, Line: lineNum, FilePath: path, Kind: "call"})
				}
			}

			// %Module{} struct literals and pattern matches
			for _, match := range structLiteralRe.FindAllStringSubmatch(codeLine, -1) {
				resolved := ResolveModuleRef(match[1], aliases, currentModule)
				if resolved != "" {
					refs = append(refs, Reference{Module: resolved, Line: lineNum, FilePath: path, Kind: "call"})
				}
			}

			// Standalone module references: @impl GenServer, @derive [Jason.Encoder],
			// rescue e in MyError, is_struct(x, User), etc.
			for _, match := range standaloneModuleRe.FindAllStringSubmatchIndex(codeLine, -1) {
				modStart, modEnd := match[2], match[3]
				modRef := codeLine[modStart:modEnd]
				// Skip Module.function — already caught by moduleCallRe
				if modEnd < len(codeLine) && codeLine[modEnd] == '.' &&
					modEnd+1 < len(codeLine) && ((codeLine[modEnd+1] >= 'a' && codeLine[modEnd+1] <= 'z') || codeLine[modEnd+1] == '_') {
					continue
				}
				// Skip %Module{ — already caught by structLiteralRe
				if modStart > 0 && codeLine[modStart-1] == '%' {
					continue
				}
				// Skip self-references to the current module
				if modRef == currentModule {
					continue
				}
				resolved := ResolveModuleRef(modRef, aliases, currentModule)
				if resolved != "" {
					refs = append(refs, Reference{Module: resolved, Line: lineNum, FilePath: path, Kind: "call"})
				}
			}
		}
	}

	return defs, refs, nil
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

// scanIdentifier reads an identifier ([A-Za-z0-9_]+) from the start of s.
func scanIdentifier(s string) string {
	i := 0
	for i < len(s) {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
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

// scanDefKeyword checks if rest starts with keyword (e.g. "defmodule") followed
// by whitespace and a module name. For defmodule/defprotocol, requires " do" after.
func scanDefKeyword(rest, keyword string) (string, bool) {
	if !strings.HasPrefix(rest, keyword) {
		return "", false
	}
	after := rest[len(keyword):]
	if len(after) == 0 || (after[0] != ' ' && after[0] != '\t') {
		return "", false
	}
	after = strings.TrimLeft(after, " \t")
	name := ScanModuleName(after)
	if name == "" {
		return "", false
	}
	if keyword == "defimpl" {
		return name, true
	}
	remaining := strings.TrimLeft(after[len(name):], " \t")
	if remaining == "do" || strings.HasPrefix(remaining, "do ") || strings.HasPrefix(remaining, "do\t") || strings.HasPrefix(remaining, "do\r") {
		return name, true
	}
	return "", false
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

// CheckHeredoc updates the inHeredoc state for a given line. Returns the new
// inHeredoc state and whether this line is a heredoc boundary or content that
// should be skipped by callers doing line-by-line analysis.
func CheckHeredoc(line string, inHeredoc bool) (newState bool, skip bool) {
	// Strip sigil content before checking for heredoc markers so that
	// """ or ''' inside ~s(...) doesn't toggle heredoc mode.
	stripped := stripSigils(line)
	if strings.IndexByte(stripped, '"') >= 0 {
		if c := countHeredocMarkers(stripped, '"'); c > 0 {
			if c < 2 {
				inHeredoc = !inHeredoc
			}
			return inHeredoc, true
		}
	}
	if strings.IndexByte(stripped, '\'') >= 0 {
		if c := countHeredocMarkers(stripped, '\''); c > 0 {
			if c < 2 {
				inHeredoc = !inHeredoc
			}
			return inHeredoc, true
		}
	}
	return inHeredoc, inHeredoc
}

// stripSigils replaces sigil content with spaces, preserving the opening ~X
// and closing delimiter so that heredoc detection only sees code, not string
// content. This is a simplified version of StripCommentsAndStrings that only
// handles sigils.
func stripSigils(line string) string {
	if !strings.ContainsRune(line, '~') {
		return line
	}
	buf := []byte(line)
	i := 0
	for i < len(buf) {
		if buf[i] == '\\' && i+1 < len(buf) {
			i += 2
			continue
		}
		if buf[i] == '"' {
			// Skip string content so we don't match ~ inside strings
			i++
			for i < len(buf) {
				if buf[i] == '\\' && i+1 < len(buf) {
					i += 2
					continue
				}
				if buf[i] == '"' {
					i++
					break
				}
				i++
			}
			continue
		}
		if buf[i] == '~' && i+1 < len(buf) {
			next := buf[i+1]
			if (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') {
				sigilStart := i + 2
				if sigilStart < len(buf) {
					i = blankSigilForHeredoc(buf, sigilStart)
					continue
				}
			}
		}
		i++
	}
	return string(buf)
}

// blankSigilForHeredoc blanks sigil content (replacing with spaces) starting
// at the opening delimiter. Returns the index after the closing delimiter +
// modifiers.
func blankSigilForHeredoc(buf []byte, i int) int {
	if i >= len(buf) {
		return i
	}
	opener := buf[i]
	var closer byte
	switch opener {
	case '(':
		closer = ')'
	case '[':
		closer = ']'
	case '{':
		closer = '}'
	case '<':
		closer = '>'
	default:
		closer = opener
	}
	i++ // skip opening delimiter
	depth := 1
	for i < len(buf) && depth > 0 {
		if buf[i] == '\\' && i+1 < len(buf) {
			buf[i] = ' '
			buf[i+1] = ' '
			i += 2
			continue
		}
		if buf[i] == closer {
			depth--
			if depth == 0 {
				i++
				// skip modifiers
				for i < len(buf) && ((buf[i] >= 'a' && buf[i] <= 'z') || (buf[i] >= 'A' && buf[i] <= 'Z')) {
					i++
				}
				return i
			}
		} else if closer != opener && buf[i] == opener {
			depth++
		}
		if depth > 0 {
			buf[i] = ' '
			i++
		}
	}
	return i
}

// sigilBracketPairs maps bracket openers to their closers.
var sigilBracketPairs = map[byte]byte{
	'(': ')',
	'[': ']',
	'{': '}',
	'<': '>',
}

// sigilResult holds the closer byte and remaining depth after scanning a line
// for an unclosed sigil.
type sigilResult struct {
	closer byte
	depth  int
}

// findUnclosedSigil scans content for a sigil (~X followed by a bracket delimiter)
// that doesn't close on the same line. Returns the closer and remaining depth.
func findUnclosedSigil(content string) (sigilResult, bool) {
	for i := 0; i < len(content); i++ {
		// Skip past strings so we don't match ~ inside them
		if content[i] == '"' {
			i = skipQuoted(content, i+1, '"')
			i-- // outer loop will i++
			continue
		}
		if content[i] == '\'' {
			i = skipQuoted(content, i+1, '\'')
			i--
			continue
		}
		if content[i] == '~' && i+1 < len(content) {
			next := content[i+1]
			if (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') {
				sigilStart := i + 2
				if sigilStart >= len(content) {
					continue
				}
				delim := content[sigilStart]
				closer, isBracket := sigilBracketPairs[delim]
				if !isBracket {
					continue
				}
				// Scan to end of line tracking bracket depth
				depth := 1
				for j := sigilStart + 1; j < len(content); j++ {
					if content[j] == '\\' && j+1 < len(content) {
						j++
						continue
					}
					if content[j] == closer {
						depth--
						if depth == 0 {
							break
						}
					} else if content[j] == delim {
						depth++
					}
				}
				if depth > 0 {
					return sigilResult{closer: closer, depth: depth}, true
				}
			}
		}
	}
	return sigilResult{}, false
}

// isSigilBracketOpener returns true if ch is a bracket opener that matches
// the given closer (i.e. closer ')' matches opener '(').
func isSigilBracketOpener(ch byte, closer byte) bool {
	for o, c := range sigilBracketPairs {
		if c == closer && o == ch {
			return true
		}
	}
	return false
}

// countHeredocMarkers counts occurrences of triple-quote markers (\"\"\" or \”'\)
// in the line, but only when they appear outside of single-line string literals.
func countHeredocMarkers(line string, quote byte) int {
	inString := false
	count := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' && inString {
			i++ // skip escaped char
			continue
		}
		if line[i] == quote {
			if inString {
				inString = false
				continue
			}
			// Check for triple quote
			if i+2 < len(line) && line[i+1] == quote && line[i+2] == quote {
				count++
				i += 2
				continue
			}
			inString = true
		}
	}
	return count
}

// ContainsDo returns true if the trimmed line ends with a block-opening " do"
// (not an inline "do:" keyword argument).
func ContainsDo(trimmed string) bool {
	return trimmed == "do" || strings.HasSuffix(trimmed, " do") || strings.HasSuffix(trimmed, "\tdo")
}

// IsEnd returns true if the trimmed line starts with the block-closing "end"
// keyword. It distinguishes "end" from identifiers like "endpoint" by checking
// that the character after "end" is not an identifier character.
func IsEnd(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "end") {
		return false
	}
	// "end" at end of string, or followed by a non-identifier char
	return len(trimmed) == 3 || !isIdentChar(trimmed[3])
}

// OpensBlock returns true if the trimmed line opens a block that will be closed
// by a matching "end". This covers both do..end blocks and fn..end blocks.
func OpensBlock(trimmed string) bool {
	return ContainsDo(trimmed) || ContainsFn(trimmed)
}

// ContainsFn returns true if the line opens an anonymous function block
// (fn ... -> on its own line) that will be closed by a matching "end".
// It returns false when the fn...end is entirely on one line.
// Callers should pass input through StripCommentsAndStrings first.
func ContainsFn(code string) bool {
	if !containsFnKeyword(code) {
		return false
	}
	// Must not have a matching end on the same line (inline fn...end).
	if idx := strings.LastIndex(code, " end"); idx >= 0 {
		if IsEnd(strings.TrimSpace(code[idx:])) {
			return false
		}
	}
	return true
}

// containsFnKeyword returns true if code contains "fn" as a standalone keyword,
// not part of a longer identifier. The character before "fn" must be a
// non-identifier char (or start of string), and the character after must also
// be a non-identifier char (or end of string).
func containsFnKeyword(code string) bool {
	for i := 0; i <= len(code)-2; i++ {
		if code[i] != 'f' || code[i+1] != 'n' {
			continue
		}
		// Check character before: must be start of string or non-identifier.
		// ':' before means it's an atom (:fn), not the keyword.
		if i > 0 && (isIdentChar(code[i-1]) || code[i-1] == ':') {
			continue
		}
		// Check character after: must be end of string or non-identifier.
		// ':' after means it's a keyword key (fn: value), not the keyword.
		if i+2 < len(code) && (isIdentChar(code[i+2]) || code[i+2] == ':') {
			continue
		}
		return true
	}
	return false
}

func isIdentChar(b byte) bool {
	return IsIdentChar(b)
}

func IsIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

func IsLowerIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

// findDelegateTo searches the current line and up to 5 subsequent lines for a to: target,
// then resolves it via aliases.
func findDelegateToAndAs(lines []Line, startIdx int, aliases map[string]string, currentModule string) (string, string) {
	end := startIdx + 6
	if end > len(lines) {
		end = len(lines)
	}

	var targetModule, targetFunc string
	for i := startIdx; i < end; i++ {
		// A new statement on any line after the first means the current defdelegate ended
		if i > startIdx && newStatementRe.MatchString(lines[i].Content) {
			break
		}
		if m := delegateToRe.FindStringSubmatch(lines[i].Content); m != nil && targetModule == "" {
			target := m[1]
			// Resolve __MODULE__ directly in to: field
			if currentModule != "" {
				target = strings.ReplaceAll(target, "__MODULE__", currentModule)
			}
			if resolved, ok := aliases[target]; ok {
				// Exact alias match: "to: Services" where Services -> MyApp.HRIS.Services
				targetModule = resolved
			} else if parts := strings.SplitN(target, ".", 2); len(parts) == 2 {
				// Partial alias: "to: Services.Foo" where Services -> MyApp.HRIS.Services
				if resolved, ok := aliases[parts[0]]; ok {
					targetModule = resolved + "." + parts[1]
				} else {
					targetModule = target
				}
			} else {
				targetModule = target
			}
		}
		if m := delegateAsRe.FindStringSubmatch(lines[i].Content); m != nil && targetFunc == "" {
			targetFunc = m[1]
		}
	}
	return targetModule, targetFunc
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

// CountDefaultParams counts the number of parameters with default values (\\)
// in a function definition line. Only counts defaults at the top-level param
// depth, not inside nested structures.
func CountDefaultParams(line string, funcName string) int {
	return DefaultsFromParams(FindParamContent(line, funcName))
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

// moduleCallRe matches Module.function calls — an uppercase module segment
// followed by a dot and a lowercase function name.
var moduleCallRe = regexp.MustCompile(`([A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)*)\.([a-z_][a-z0-9_?!]*)`)

// structLiteralRe matches %Module{...} struct literals and pattern matches.
var structLiteralRe = regexp.MustCompile(`%([A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)*)\{`)

// standaloneModuleRe matches module names that appear without a .function or %{
// suffix — covers @impl GenServer, @derive [Jason.Encoder], rescue e in MyError,
// is_struct(x, User), etc. The negative lookahead for . and { is handled in code.
var standaloneModuleRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.%])([A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)*)`)

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

func hasUppercase(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

// StripCommentsAndStrings removes inline comments and replaces the content
// of string literals and sigils with spaces so that regex-based extraction
// doesn't produce false-positive references from comments, strings, or sigils.
func StripCommentsAndStrings(line string) string {
	// Fast path: skip allocation if line has no strings, comments, or sigils
	if !strings.ContainsAny(line, "\"'#~") {
		return line
	}
	buf := []byte(line)
	i := 0
	for i < len(buf) {
		ch := buf[i]
		// Skip escaped characters
		if ch == '\\' && i+1 < len(buf) {
			i += 2
			continue
		}
		// Sigil: ~s(...), ~r/.../,  etc.
		if ch == '~' && i+1 < len(buf) {
			next := buf[i+1]
			sigilStart := i + 2
			// Sigil letter (uppercase or lowercase)
			if (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') {
				if sigilStart < len(buf) {
					i = blankSigil(buf, sigilStart)
					continue
				}
			}
			i++
			continue
		}
		// String literal (double-quoted)
		if ch == '"' {
			// Check for char literal: ?\" is the integer value of ", not a string start
			if i > 0 && buf[i-1] == '?' {
				i++
				continue
			}
			i = blankQuoted(buf, i, '"')
			continue
		}
		// Single-quoted charlist
		if ch == '\'' {
			// Check for char literal: ?' is also a char literal
			if i > 0 && buf[i-1] == '?' {
				i++
				continue
			}
			i = blankQuoted(buf, i, '\'')
			continue
		}
		// Comment — blank everything from here to end of line
		if ch == '#' {
			for k := i; k < len(buf); k++ {
				buf[k] = ' '
			}
			break
		}
		i++
	}
	return string(buf)
}

// blankQuoted blanks the contents of a quoted literal (string or charlist)
// starting at buf[i] (which is the opening quote). Returns the index after
// the closing quote.
func blankQuoted(buf []byte, i int, quote byte) int {
	j := i + 1
	for j < len(buf) {
		if buf[j] == '\\' && j+1 < len(buf) {
			buf[j] = ' '
			buf[j+1] = ' '
			j += 2
			continue
		}
		// Handle interpolation: #{...} can contain nested strings/braces
		if quote == '"' && buf[j] == '#' && j+1 < len(buf) && buf[j+1] == '{' {
			buf[j] = ' '
			buf[j+1] = ' '
			j += 2
			braceDepth := 1
			for j < len(buf) && braceDepth > 0 {
				if buf[j] == '\\' && j+1 < len(buf) {
					buf[j] = ' '
					buf[j+1] = ' '
					j += 2
					continue
				}
				if buf[j] == '"' || buf[j] == '\'' {
					// Nested string inside interpolation — blank it recursively
					nestedQuote := buf[j]
					j++
					for j < len(buf) {
						if buf[j] == '\\' && j+1 < len(buf) {
							buf[j] = ' '
							buf[j+1] = ' '
							j += 2
							continue
						}
						if buf[j] == nestedQuote {
							buf[j] = ' '
							j++
							break
						}
						buf[j] = ' '
						j++
					}
					continue
				}
				if buf[j] == '{' {
					braceDepth++
				} else if buf[j] == '}' {
					braceDepth--
					if braceDepth == 0 {
						buf[j] = ' '
						j++
						break
					}
				}
				buf[j] = ' '
				j++
			}
			continue
		}
		if buf[j] == quote {
			i = j + 1
			return i
		}
		buf[j] = ' '
		j++
	}
	// Unterminated — blank to end
	for k := i + 1; k < len(buf); k++ {
		buf[k] = ' '
	}
	return len(buf)
}

// blankSigil blanks the contents of a sigil starting at buf[i] (the opening
// delimiter character, after the ~X). Returns the index after the closing
// delimiter + modifiers.
func blankSigil(buf []byte, i int) int {
	opener := buf[i]
	var closer byte
	switch opener {
	case '(':
		closer = ')'
	case '[':
		closer = ']'
	case '{':
		closer = '}'
	case '<':
		closer = '>'
	case '/', '|', '"', '\'':
		closer = opener
	default:
		return i + 1
	}
	j := i + 1
	depth := 1
	for j < len(buf) {
		if buf[j] == '\\' && j+1 < len(buf) {
			buf[j] = ' '
			buf[j+1] = ' '
			j += 2
			continue
		}
		if buf[j] == closer {
			depth--
			if depth == 0 {
				j++
				// Skip trailing sigil modifiers (letters)
				for j < len(buf) && ((buf[j] >= 'a' && buf[j] <= 'z') || (buf[j] >= 'A' && buf[j] <= 'Z')) {
					buf[j] = ' '
					j++
				}
				return j
			}
		} else if closer != opener && buf[j] == opener {
			depth++
		}
		buf[j] = ' '
		j++
	}
	return len(buf)
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
