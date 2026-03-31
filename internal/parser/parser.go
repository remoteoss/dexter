package parser

import (
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
)

var (
	defmoduleRe    = regexp.MustCompile(`^\s*defmodule\s+([A-Za-z0-9_.]+)\s+do`)
	defprotocolRe  = regexp.MustCompile(`^\s*defprotocol\s+([A-Za-z0-9_.]+)\s+do`)
	defimplRe      = regexp.MustCompile(`^\s*defimpl\s+([A-Za-z0-9_.]+)`)
	defstructRe    = regexp.MustCompile(`^\s*defstruct\s`)
	defexceptionRe = regexp.MustCompile(`^\s*defexception\s`)
	delegateToRe   = regexp.MustCompile(`to:\s*([A-Za-z0-9_.]+)`)
	delegateAsRe   = regexp.MustCompile(`as:\s*:?([a-z_][a-z0-9_?!]*)`)
)

type Definition struct {
	Module     string
	Function   string
	Line       int
	FilePath   string
	Kind       string
	DelegateTo string
	DelegateAs string // for defdelegate with as: — the function name in the target module
}

func ParseFile(path string) ([]Definition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(data), "\n")
	var defs []Definition
	var moduleStack []string
	aliases := map[string]string{} // short name -> full module
	inHeredoc := false

	for lineIdx, line := range lines {
		lineNum := lineIdx + 1
		trimmed := strings.TrimSpace(line)

		quoteCount := strings.Count(line, `"""`)
		if quoteCount > 0 {
			if quoteCount >= 2 {
				continue
			}
			inHeredoc = !inHeredoc
			continue
		}

		if inHeredoc {
			continue
		}

		if trimmed == "end" && len(moduleStack) > 1 {
			moduleStack = moduleStack[:len(moduleStack)-1]
		}

		currentModule := ""
		if len(moduleStack) > 0 {
			currentModule = moduleStack[len(moduleStack)-1]
		}

		// Track aliases, resolving __MODULE__ to the current module name
		resolveModule := func(s string) string {
			if currentModule != "" {
				return strings.ReplaceAll(s, "__MODULE__", currentModule)
			}
			return s
		}
		if m := AliasAsRe.FindStringSubmatch(line); m != nil {
			resolved := resolveModule(m[1])
			// Skip if we can't resolve __MODULE__ (no current module yet)
			if !strings.Contains(resolved, "__MODULE__") {
				aliases[m[2]] = resolved
			}
		} else if m := AliasRe.FindStringSubmatch(line); m != nil {
			resolved := resolveModule(m[1])
			parts := strings.Split(resolved, ".")
			shortName := parts[len(parts)-1]
			aliases[shortName] = resolved
		}

		if m := defmoduleRe.FindStringSubmatch(line); m != nil {
			currentModule = m[1]
			moduleStack = append(moduleStack, currentModule)
			defs = append(defs, Definition{
				Module:   currentModule,
				Line:     lineNum,
				FilePath: path,
				Kind:     "module",
			})
			continue
		}

		if m := defprotocolRe.FindStringSubmatch(line); m != nil {
			currentModule = m[1]
			moduleStack = append(moduleStack, currentModule)
			defs = append(defs, Definition{
				Module:   currentModule,
				Line:     lineNum,
				FilePath: path,
				Kind:     "defprotocol",
			})
			continue
		}

		if m := defimplRe.FindStringSubmatch(line); m != nil {
			currentModule = m[1]
			moduleStack = append(moduleStack, currentModule)
			defs = append(defs, Definition{
				Module:   currentModule,
				Line:     lineNum,
				FilePath: path,
				Kind:     "defimpl",
			})
			continue
		}

		if currentModule != "" {
			if m := FuncDefRe.FindStringSubmatch(line); m != nil {
				kind := m[1]
				funcName := m[2]
				def := Definition{
					Module:   currentModule,
					Function: funcName,
					Line:     lineNum,
					FilePath: path,
					Kind:     kind,
				}
				if kind == "defdelegate" {
					def.DelegateTo, def.DelegateAs = findDelegateToAndAs(lines, lineIdx, aliases, currentModule)
				}
				defs = append(defs, def)
				continue
			}

			if defstructRe.MatchString(line) {
				defs = append(defs, Definition{
					Module:   currentModule,
					Function: "__struct__",
					Line:     lineNum,
					FilePath: path,
					Kind:     "defstruct",
				})
			}
			if defexceptionRe.MatchString(line) {
				defs = append(defs, Definition{
					Module:   currentModule,
					Function: "__exception__",
					Line:     lineNum,
					FilePath: path,
					Kind:     "defexception",
				})
			}
		}
	}

	return defs, nil
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

func IsElixirFile(path string) bool {
	extension := filepath.Ext(path)
	return extension == ".ex" || extension == ".exs"
}

// WalkElixirFiles walks root, skipping _build/.git/node_modules directories,
// and calls fn for each .ex/.exs file found.
func WalkElixirFiles(root string, fn func(path string, info os.FileInfo) error) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "_build" || base == ".git" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !IsElixirFile(path) {
			return nil
		}
		return fn(path, info)
	})
}

