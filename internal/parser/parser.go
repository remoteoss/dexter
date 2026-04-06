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
	return ParseText(path, string(data))
}

// ParseText parses Elixir source text and returns definitions and references.
// The path is used to populate FilePath fields but the text is not read from disk.
func ParseText(path, text string) ([]Definition, []Reference, error) {
	type moduleFrame struct {
		name           string
		indent         int
		savedAliases   map[string]string
		savedInjectors map[string]bool
	}

	lines := strings.Split(text, "\n")
	var defs []Definition
	var refs []Reference
	var moduleStack []moduleFrame
	aliases := map[string]string{} // short name -> full module
	injectors := map[string]bool{} // modules from use/import that inject bare functions
	inHeredoc := false

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1

		// Heredoc tracking — handle both """ and '''
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
		if strings.IndexByte(line, '\'') >= 0 {
			quoteCount := strings.Count(line, `'''`)
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
					frame := moduleStack[len(moduleStack)-1]
					moduleStack = moduleStack[:len(moduleStack)-1]
					aliases = frame.savedAliases
					injectors = frame.savedInjectors
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
							Arity:    ExtractArity(line, name),
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
							Arity:    ExtractArity(line, name),
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
				moduleStack = append(moduleStack, moduleFrame{name: currentModule, indent: trimStart, savedAliases: copyMap(aliases), savedInjectors: copyBoolMap(injectors)})
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
				moduleStack = append(moduleStack, moduleFrame{name: currentModule, indent: trimStart, savedAliases: copyMap(aliases), savedInjectors: copyBoolMap(injectors)})
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
				moduleStack = append(moduleStack, moduleFrame{name: currentModule, indent: trimStart, savedAliases: copyMap(aliases), savedInjectors: copyBoolMap(injectors)})
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
					maxArity := ExtractArity(line, funcName)
					defaultCount := CountDefaultParams(line, funcName)
					minArity := maxArity - defaultCount

					var delegateTo, delegateAs string
					if kind == "defdelegate" {
						delegateTo, delegateAs = findDelegateToAndAs(lines, lineIdx, aliases, currentModule)
					}

					for arity := minArity; arity <= maxArity; arity++ {
						defs = append(defs, Definition{
							Module:     currentModule,
							Function:   funcName,
							Arity:      arity,
							Line:       lineNum,
							FilePath:   path,
							Kind:       kind,
							DelegateTo: delegateTo,
							DelegateAs: delegateAs,
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
		if !hasUppercase(line) {
			continue
		}
		{
			codeLine := stripCommentsAndStrings(line)

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

// ContainsDo returns true if the trimmed line ends with a block-opening " do"
// (not an inline "do:" keyword argument).
func ContainsDo(trimmed string) bool {
	return strings.HasSuffix(trimmed, " do") || strings.HasSuffix(trimmed, "\tdo")
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
	inside := rest[parenIdx+1:]
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

// CountDefaultParams counts the number of parameters with default values (\\)
// in a function definition line. Only counts defaults at the top-level param
// depth, not inside nested structures.
func CountDefaultParams(line string, funcName string) int {
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
	defaults := 0
	inside := rest[parenIdx+1:]
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

// skipStringLiteral advances past a string or charlist literal starting at
// position i in s (where s[i] is '"' or '\”). Returns the index of the
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

// stripCommentsAndStrings removes inline comments and replaces the content
// of string literals and sigils with spaces so that regex-based extraction
// doesn't produce false-positive references from comments, strings, or sigils.
func stripCommentsAndStrings(line string) string {
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
			i = blankQuoted(buf, i, '"')
			continue
		}
		// Single-quoted charlist
		if ch == '\'' {
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
