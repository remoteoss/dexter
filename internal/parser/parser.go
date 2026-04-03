package parser

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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

	type moduleFrame struct {
		name   string
		indent int // leading whitespace count when defmodule was found
	}

	lines := strings.Split(string(data), "\n")
	var defs []Definition
	var refs []Reference
	var moduleStack []moduleFrame
	aliases := map[string]string{} // short name -> full module
	injectors := map[string]bool{} // modules from use/import that inject bare functions
	inHeredoc := false

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1

		// Heredoc tracking — only scan for """ if line contains a double-quote
		if strings.IndexByte(line, '"') >= 0 {
			quoteCount := strings.Count(line, `"""`)
			if quoteCount > 0 {
				if quoteCount >= 2 {
					continue
				}
				inHeredoc = !inHeredoc
				continue
			}
		}

		if inHeredoc {
			continue
		}

		// Find first non-whitespace character for fast pre-filtering.
		trimStart := 0
		for trimStart < len(line) && (line[trimStart] == ' ' || line[trimStart] == '\t') {
			trimStart++
		}
		if trimStart >= len(line) {
			continue
		}
		first := line[trimStart]
		rest := line[trimStart:] // line content from first non-whitespace char

		// 'e' — check for "end" to pop module stack; otherwise fall through
		if first == 'e' {
			if len(moduleStack) > 0 && strings.TrimRight(rest, " \t\r") == "end" {
				if moduleStack[len(moduleStack)-1].indent == trimStart {
					moduleStack = moduleStack[:len(moduleStack)-1]
				}
				continue
			}
			// Not "end" — may be a bare macro call like "embedded_schema do"
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

		// '@' — type definitions (@type, @typep, @opaque)
		if first == '@' {
			if currentModule != "" {
				var kind string
				var afterKw string
				if strings.HasPrefix(rest, "@typep") && len(rest) > 6 && (rest[6] == ' ' || rest[6] == '\t') {
					kind = "typep"
					afterKw = strings.TrimLeft(rest[6:], " \t")
				} else if strings.HasPrefix(rest, "@type") && len(rest) > 5 && (rest[5] == ' ' || rest[5] == '\t') {
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
							Arity:    ExtractArity(line, name),
							Line:     lineNum,
							FilePath: path,
							Kind:     kind,
						})
					}
				}
			}
			continue
		}

		// 'd' — defmodule, defprotocol, defimpl, def*, defstruct, defexception
		if first == 'd' && strings.HasPrefix(rest, "def") {
			if name, ok := scanDefKeyword(rest, "defmodule"); ok {
				if !strings.Contains(name, ".") && currentModule != "" {
					name = currentModule + "." + name
				}
				currentModule = name
				moduleStack = append(moduleStack, moduleFrame{name: currentModule, indent: trimStart})
				defs = append(defs, Definition{
					Module:   currentModule,
					Line:     lineNum,
					FilePath: path,
					Kind:     "module",
				})
				continue
			}

			if name, ok := scanDefKeyword(rest, "defprotocol"); ok {
				currentModule = name
				moduleStack = append(moduleStack, moduleFrame{name: currentModule, indent: trimStart})
				defs = append(defs, Definition{
					Module:   currentModule,
					Line:     lineNum,
					FilePath: path,
					Kind:     "defprotocol",
				})
				continue
			}

			if name, ok := scanDefKeyword(rest, "defimpl"); ok {
				currentModule = name
				moduleStack = append(moduleStack, moduleFrame{name: currentModule, indent: trimStart})
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
					def := Definition{
						Module:   currentModule,
						Function: funcName,
						Arity:    ExtractArity(line, funcName),
						Line:     lineNum,
						FilePath: path,
						Kind:     kind,
					}
					if kind == "defdelegate" {
						def.DelegateTo, def.DelegateAs = findDelegateToAndAs(lines, lineIdx, aliases, currentModule)
					}
					defs = append(defs, def)
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

		// Extract Module.function call references from any line.
		// Quick check: moduleCallRe requires an uppercase letter, so skip lines without one.
		if !hasUppercase(line) {
			continue
		}
		{
			codeLine := stripCommentsAndStrings(line)
			for _, match := range moduleCallRe.FindAllStringSubmatch(codeLine, -1) {
				modRef := match[1]
				funcName := match[2]

				if elixirKeyword[funcName] {
					continue
				}

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
					continue
				}

				refs = append(refs, Reference{
					Module:   resolved,
					Function: funcName,
					Line:     lineNum,
					FilePath: path,
					Kind:     "call",
				})
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

// findDelegateTo searches the current line and up to 5 subsequent lines for a to: target,
// then resolves it via aliases.
func findDelegateToAndAs(lines []string, startIdx int, aliases map[string]string, currentModule string) (string, string) {
	end := startIdx + 6
	if end > len(lines) {
		end = len(lines)
	}

	var targetModule, targetFunc string
	for i := startIdx; i < end; i++ {
		// A new statement on any line after the first means the current defdelegate ended
		if i > startIdx && newStatementRe.MatchString(lines[i]) {
			break
		}
		if m := delegateToRe.FindStringSubmatch(lines[i]); m != nil && targetModule == "" {
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
		if m := delegateAsRe.FindStringSubmatch(lines[i]); m != nil && targetFunc == "" {
			targetFunc = m[1]
		}
	}
	return targetModule, targetFunc
}

// ExtractArity counts the number of arguments in a function definition line.
// It finds the first parenthesized argument list after the function name and
// counts top-level commas, respecting nested parens/brackets/braces.
func ExtractArity(line string, funcName string) int {
	idx := strings.Index(line, funcName)
	if idx < 0 {
		return 0
	}
	rest := line[idx+len(funcName):]

	parenIdx := strings.IndexByte(rest, '(')
	if parenIdx < 0 {
		return 0
	}

	depth := 1
	commas := 0
	hasContent := false
	for _, ch := range rest[parenIdx+1:] {
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

func resolveModule(s, currentModule string) string {
	if currentModule != "" {
		return strings.ReplaceAll(s, "__MODULE__", currentModule)
	}
	return s
}

// moduleCallRe matches Module.function calls — an uppercase module segment
// followed by a dot and a lowercase function name.
var moduleCallRe = regexp.MustCompile(`([A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)*)\.([a-z_][a-z0-9_?!]*)`)

func hasUppercase(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

// stripCommentsAndStrings removes inline comments and replaces the content
// of string literals with spaces so that regex-based extraction doesn't
// produce false-positive references from comments or string values.
func stripCommentsAndStrings(line string) string {
	buf := []byte(line)
	i := 0
	for i < len(buf) {
		ch := buf[i]
		// Skip escaped characters
		if ch == '\\' && i+1 < len(buf) {
			i += 2
			continue
		}
		// String literal (double-quoted)
		if ch == '"' {
			j := i + 1
			for j < len(buf) {
				if buf[j] == '\\' && j+1 < len(buf) {
					j += 2
					continue
				}
				if buf[j] == '"' {
					// Blank out string contents (keep quotes for structure)
					for k := i + 1; k < j; k++ {
						buf[k] = ' '
					}
					i = j + 1
					break
				}
				j++
			}
			if j >= len(buf) {
				// Unterminated string — blank to end
				for k := i + 1; k < len(buf); k++ {
					buf[k] = ' '
				}
				break
			}
			continue
		}
		// Single-quoted charlist
		if ch == '\'' {
			j := i + 1
			for j < len(buf) {
				if buf[j] == '\\' && j+1 < len(buf) {
					j += 2
					continue
				}
				if buf[j] == '\'' {
					for k := i + 1; k < j; k++ {
						buf[k] = ' '
					}
					i = j + 1
					break
				}
				j++
			}
			if j >= len(buf) {
				for k := i + 1; k < len(buf); k++ {
					buf[k] = ' '
				}
				break
			}
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
