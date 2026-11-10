package parser

import "strings"

// parseTextFromTokens is the token-stream replacement for the line-based ParseText.
// It walks a []Token stream from the tokenizer and produces identical Definition
// and Reference output.
func parseTextFromTokens(path string, source []byte, tokens, interp []Token) ([]Definition, []Reference, error) {
	var defs []Definition
	var refs []Reference

	type moduleFrame struct {
		name           string
		depth          int
		savedAliases   map[string]string
		savedInjectors map[string]bool
		savedFilters   map[string]importFilter
	}

	var moduleStack []moduleFrame
	depth := 0

	aliases := map[string]string{}
	injectors := map[string]bool{}
	// importFilters holds the only: or except: names of an imported
	// injector; an injector without an entry brings in every name.
	importFilters := map[string]importFilter{}
	// injected are the refs that emitInjected added, with the module at
	// their line: a call of a name that the module defines is its own
	// function, and the ref is dropped at the end.
	type injectedRef struct {
		idx    int
		module string
		arity  int // -1 when the call does not show it
	}
	var injected []injectedRef

	n := len(tokens)

	// A definition inside a quote is code that a macro generates in its
	// caller, not a function of the module that holds the quote, so it is not
	// indexed as one. This is true for every quote in a function body: a
	// macro's, `__using__`'s, and a helper's (`def controller do quote ...`).
	// What `__using__` and its helpers inject is resolved from the source of
	// their bodies (parseUsingBody), and the compiled caller's BEAM records
	// where a generated function was declared. The same rule covers defstruct,
	// defexception, and a defmodule or defimpl in a quote.
	// macroBodies holds the depth inside each open do-block of a function body,
	// innermost last. quoteDepth is the depth inside an open `quote do` block
	// in one, or zero. quoteLine is the line of an inline `quote do:`.
	// A body is a macro's when its function is a defmacro or defmacrop other
	// than __using__: a `use` in its quote runs in the caller, while one in a
	// __using__ or helper quote is a link that the use chain follows.
	// A body's saved scope is the aliases, injectors, and import filters
	// before its first alias or import, which are lexical: they end with the
	// function. A body without one copies nothing.
	type functionBody struct {
		depth int
		macro bool

		saved     bool
		aliases   map[string]string
		injectors map[string]bool
		filters   map[string]importFilter
	}
	var macroBodies []functionBody
	quoteDepth, quoteLine := 0, 0
	// quoteInMacro is whether the open quote is in a macro's body.
	quoteInMacro := false
	// inlineMacroLine is the line of the last function written with `, do:`,
	// whose quote can follow on the next line; inlineMacro is whether that
	// function is a macro.
	inlineMacroLine, inlineMacro := 0, false
	inQuote := func(line int) bool {
		return (quoteDepth > 0 && depth >= quoteDepth) || (quoteLine != 0 && quoteLine == line)
	}
	// quoteInjectors and quoteFilters are the injectors and import filters
	// before the open quote block: an import or use in a quote is code for
	// the caller, so it applies to the quote only and ends with it.
	var quoteInjectors map[string]bool
	var quoteFilters map[string]importFilter
	markQuote := func(at, depthAt int, macro bool) {
		quoteInMacro = macro
		switch form, line := quoteForm(source, tokens, n, at+1); form {
		case quoteBlock:
			// The do is counted when the walk reaches it.
			quoteDepth = depthAt + 1
			quoteInjectors, quoteFilters = copyBoolMap(injectors), copyImportFilters(importFilters)
		case quoteInline:
			quoteLine = line
		}
	}
	leaveBlocks := func() {
		if quoteDepth > depth {
			quoteDepth = 0
			if quoteInjectors != nil {
				injectors, importFilters = quoteInjectors, quoteFilters
				quoteInjectors, quoteFilters = nil, nil
			}
		}
		for len(macroBodies) > 0 && macroBodies[len(macroBodies)-1].depth > depth {
			if b := macroBodies[len(macroBodies)-1]; b.saved {
				aliases, injectors, importFilters = b.aliases, b.injectors, b.filters
			}
			macroBodies = macroBodies[:len(macroBodies)-1]
		}
	}
	// saveBodyScope saves the scope of the innermost function body before an
	// alias or import in it changes the scope.
	saveBodyScope := func() {
		if len(macroBodies) == 0 {
			return
		}
		if b := &macroBodies[len(macroBodies)-1]; !b.saved && depth >= b.depth {
			b.saved = true
			b.aliases, b.injectors, b.filters = copyMap(aliases), copyBoolMap(injectors), copyImportFilters(importFilters)
		}
	}

	tokenText := func(t Token) string {
		return TokenText(source, t)
	}

	nextSig := func(from int) int {
		return NextSigToken(tokens, n, from)
	}

	collectModuleName := func(i int) (string, int) {
		return CollectModuleName(source, tokens, n, i)
	}

	collectParamsFromTokens := func(i int) (int, int, []string, int) {
		return CollectParams(source, tokens, n, i)
	}

	fixParamNames := func(names []string) []string {
		return FixParamNames(names)
	}

	currentModule := func() string {
		if len(moduleStack) > 0 {
			return moduleStack[len(moduleStack)-1].name
		}
		return ""
	}
	// selfModuleAt is the module that `__MODULE__` names at line: the current
	// module, or none in a quote, where it is the module that calls the
	// macro.
	selfModuleAt := func(line int) string {
		if inQuote(line) {
			return ""
		}
		return currentModule()
	}

	// processModuleDef handles defmodule/defprotocol/defimpl.
	// It collects the module name, scans forward to consume the TokDo token,
	// increments depth, and pushes the module frame with the post-increment depth.
	// For inline `, do:` modules the definition is still emitted but no frame is
	// pushed (no do..end scope to track).
	processModuleDef := func(i int, kind string) int {
		kwLine := tokens[i-1].Line
		quoted := inQuote(kwLine)
		j := nextSig(i)
		name, j := collectModuleName(j)
		if name == "" {
			return i
		}
		if kind == "defimpl" {
			// The protocol name is resolved like any module reference; it is
			// not nested (`defimpl Inspect, for: __MODULE__` inside MyApp.User
			// implements Inspect).
			if resolved := ResolveModuleRef(name, aliases, currentModule()); resolved != "" {
				name = resolved
			}
		} else {
			var short, full string
			name, short, full = NestedModuleName(name, currentModule())
			// A nested module is an alias in the module around it, from here
			// to its end, as if it had `alias Outer.Short`. The frame pushed
			// below saves the aliases with it, so it lasts after this module
			// ends.
			if short != "" && !quoted {
				aliases[short] = full
			}
		}

		// Always emit the module definition — even `, do:` one-liners must be
		// tracked so callers can find the module. A module in a quote is
		// defined where the macro expands, not here; its frame is still pushed
		// so that its do-block is counted.
		if !quoted {
			defs = append(defs, Definition{
				Module:   name,
				Line:     kwLine,
				FilePath: path,
				Kind:     kind,
			})
		}

		// Scan forward to find and consume TokDo (skipping "for: Module" etc.).
		// Do not stop at TokEOL — Elixir allows `defmodule Name` then `do` on the
		// next line; stopping at EOL left TokDo to the main loop (double-counting
		// depth) so inner `end` did not pop the inner module frame.
		// Stop at statement-boundary tokens to avoid stealing a later module's TokDo
		// when the current module uses the `, do:` keyword form.
		_, scanPos, hasDo := ScanForwardToBlockDo(tokens, n, j)
		if kind == "defimpl" {
			// `defimpl Proto, for: Mod` (or `for: [A, B]`) references each
			// module it implements the protocol for; the walk below skips
			// these tokens.
			for k := j; k+1 < scanPos; k++ {
				if tokens[k].Kind != TokIdent || tokenText(tokens[k]) != "for" || tokens[k+1].Kind != TokColon {
					continue
				}
				m := nextSig(k + 2)
				inList := m < n && tokens[m].Kind == TokOpenBracket
				if inList {
					m = nextSig(m + 1)
				}
				for m < scanPos {
					mod, next := collectModuleName(m)
					if mod == "" {
						break
					}
					if resolved := ResolveModuleRef(mod, aliases, currentModule()); resolved != "" {
						refs = append(refs, Reference{Module: resolved, Line: tokens[m].Line, FilePath: path, Kind: "impl"})
					}
					if !inList {
						break
					}
					m = nextSig(next)
					if m < n && tokens[m].Kind == TokComma {
						m = nextSig(m + 1)
						continue
					}
					break
				}
				break
			}
		}
		if hasDo {
			depth++
			moduleStack = append(moduleStack, moduleFrame{
				name:           name,
				depth:          depth,
				savedAliases:   copyMap(aliases),
				savedInjectors: copyBoolMap(injectors),
				savedFilters:   copyImportFilters(importFilters),
			})
		}
		return scanPos
	}

	// Module references inside a typespec name types, not functions:
	// `@spec put_meta(Ecto.Schema.schema(), meta)` refers to the type
	// Ecto.Schema.schema, not to the macro of that name. They are recorded
	// under their own kind so a lookup for a function can leave them out and a
	// lookup for a type can pick them out. The token range uses the same
	// statement-boundary scanner as open-document lookup.
	typespecStart, typespecEnd := -1, -1
	callKind := func(pos int) string {
		if pos >= typespecStart && pos < typespecEnd {
			return "typespec"
		}
		return "call"
	}

	// emitInjected records an unqualified call of name as a ref to each
	// injector that can bring the name in.
	emitInjected := func(name string, pos int) {
		cm := currentModule()
		arity := -2 // not read yet
		for mod := range injectors {
			if f, ok := importFilters[mod]; ok && !f.allows(name) {
				continue
			}
			if arity == -2 {
				arity = callArity(source, tokens, n, pos)
			}
			injected = append(injected, injectedRef{len(refs), cm, arity})
			refs = append(refs, Reference{Module: mod, Function: name, Line: tokens[pos].Line, FilePath: path, Kind: callKind(pos)})
		}
	}

	emitModuleRef := func(modName string, line int, kind string) {
		resolved := resolveModule(modName, currentModule())
		if !strings.Contains(resolved, "__MODULE__") {
			refs = append(refs, Reference{Module: resolved, Line: line, FilePath: path, Kind: kind})
		}
	}

	scanDelegateOpts := func(i int) (string, string) {
		var delegateTo, delegateAs string
		bracketDepth := 0
		for i < n {
			tok := tokens[i]
			if tok.Kind == TokEOF {
				break
			}
			if bracketDepth == 0 {
				switch tok.Kind {
				case TokEnd, TokDef, TokDefp, TokDefmacro, TokDefmacrop,
					TokDefguard, TokDefguardp, TokDefdelegate, TokDefmodule,
					TokDefprotocol, TokDefimpl, TokAlias, TokImport:
					return delegateTo, delegateAs
				}
			}
			switch tok.Kind {
			case TokOpenParen, TokOpenBracket, TokOpenBrace:
				bracketDepth++
			case TokCloseParen, TokCloseBracket, TokCloseBrace:
				bracketDepth--
			}
			if tok.Kind == TokIdent {
				text := tokenText(tok)
				if text == "to" && i+1 < n && tokens[i+1].Kind == TokColon {
					j := nextSig(i + 2)
					modName, _ := collectModuleName(j)
					if modName != "" {
						target := modName
						if currentModule() != "" {
							target = strings.ReplaceAll(target, "__MODULE__", currentModule())
						}
						if resolved, ok := aliases[target]; ok {
							delegateTo = resolved
						} else if parts := strings.SplitN(target, ".", 2); len(parts) == 2 {
							if resolved, ok := aliases[parts[0]]; ok {
								delegateTo = resolved + "." + parts[1]
							} else {
								delegateTo = target
							}
						} else {
							delegateTo = target
						}
					}
				}
				if text == "as" && i+1 < n && tokens[i+1].Kind == TokColon {
					j := nextSig(i + 2)
					if j < n {
						switch tokens[j].Kind {
						case TokAtom:
							atomText := tokenText(tokens[j])
							if len(atomText) > 1 && atomText[0] == ':' {
								delegateAs = atomText[1:]
							}
						case TokIdent:
							delegateAs = tokenText(tokens[j])
						}
					}
				}
			}
			i++
		}
		return delegateTo, delegateAs
	}

	// extractModuleRefs emits call/struct refs for module references in a token range.
	// Only processes TokModule tokens that start with ASCII uppercase (matching old regex behavior).
	extractModuleRefs := func(lineStart, lineEnd int) {
		refs = collectModuleRefs(source, path, tokens, lineStart, lineEnd, aliases, currentModule(), selfModuleAt(tokens[lineStart].Line), callKind, refs)
	}

	// flushInterpRefs emits the refs written inside a #{} interpolation. The
	// tokenizer keeps those tokens in their own stream (see TokenResult.Interp),
	// so they are drained here in byte order as the walker passes them, which
	// is what gives them the aliases and the enclosing module in force at that
	// point in the file.
	interpPos := 0
	flushInterpRefs := func(upTo int) {
		start := interpPos
		for interpPos < len(interp) && interp[interpPos].Start < upTo {
			interpPos++
		}
		if interpPos > start {
			refs = collectModuleRefs(source, path, interp, start, interpPos, aliases, currentModule(), selfModuleAt(interp[start].Line), callRefKind, refs)
		}
	}

	// trackLineDepth scans tokens[lineStart:lineEnd] for TokDo/TokFn/TokEnd
	// and updates depth accordingly. TokEnd also checks for module stack pops.
	trackLineDepth := func(lineStart, lineEnd int) {
		for j := lineStart; j < lineEnd; j++ {
			switch tokens[j].Kind {
			case TokDo, TokFn:
				TrackBlockDepth(tokens[j].Kind, &depth)
			case TokEnd:
				prevDepth := depth
				TrackBlockDepth(tokens[j].Kind, &depth)
				if len(moduleStack) > 0 && moduleStack[len(moduleStack)-1].depth == prevDepth {
					frame := moduleStack[len(moduleStack)-1]
					moduleStack = moduleStack[:len(moduleStack)-1]
					aliases = frame.savedAliases
					injectors = frame.savedInjectors
					importFilters = frame.savedFilters
				}
				leaveBlocks()
			}
		}
	}

	// Main token walker
	i := 0
	for i < n {
		tok := tokens[i]

		if interpPos < len(interp) {
			flushInterpRefs(tok.Start)
		}

		switch tok.Kind {
		case TokEOL, TokComment, TokString, TokHeredoc, TokSigil,
			TokCharLiteral, TokAtom, TokNumber, TokOther,
			TokDot, TokColon, TokOpenParen, TokCloseParen,
			TokOpenBracket, TokCloseBracket, TokOpenBrace, TokCloseBrace,
			TokOpenAngle, TokCloseAngle, TokBackslash, TokRightArrow,
			TokLeftArrow, TokAssoc, TokDoubleColon, TokComma, TokWhen:
			i++
			continue

		case TokEOF:
			i = n
			continue

		case TokEnd:
			prevDepth := depth
			TrackBlockDepth(tok.Kind, &depth)
			if len(moduleStack) > 0 && moduleStack[len(moduleStack)-1].depth == prevDepth {
				frame := moduleStack[len(moduleStack)-1]
				moduleStack = moduleStack[:len(moduleStack)-1]
				aliases = frame.savedAliases
				injectors = frame.savedInjectors
				importFilters = frame.savedFilters
			}
			leaveBlocks()
			i++
			continue

		case TokDo, TokFn:
			TrackBlockDepth(tok.Kind, &depth)
			i++
			continue

		case TokDefmodule:
			i++
			i = processModuleDef(i, "module")
			continue

		case TokDefprotocol:
			i++
			i = processModuleDef(i, "defprotocol")
			continue

		case TokDefimpl:
			i++
			i = processModuleDef(i, "defimpl")
			continue

		case TokDef, TokDefp, TokDefmacro, TokDefmacrop, TokDefguard, TokDefguardp, TokDefdelegate:
			cm := currentModule()
			if cm == "" {
				i++
				continue
			}
			kind := tokenText(tok)
			defLine := tok.Line
			declarationIdx := i
			funcName, j, ok := StaticDeclarationName(source, tokens, n, declarationIdx)
			if !ok {
				i = j
				goto extractRefsForLine
			}
			{
				j++

				pj := nextSig(j)
				maxArity := 0
				defaultCount := 0
				var paramNames []string
				if pj < n && tokens[pj].Kind == TokOpenParen {
					maxArity, defaultCount, paramNames, pj = collectParamsFromTokens(pj)
					paramNames = fixParamNames(paramNames)
				}

				var delegateTo, delegateAs string
				if kind == "defdelegate" {
					delegateTo, delegateAs = scanDelegateOpts(pj)
				}

				// Decided before the head is scanned below, which can mark a
				// quote on this same line (`defmacro m, do: quote do: ...`).
				insideQuote := inQuote(defLine)
				if kind == "def" || kind == "defp" || kind == "defmacro" || kind == "defmacrop" {
					macro := (kind == "defmacro" || kind == "defmacrop") && funcName != "__using__"
					if opensDoBlock(tokens, n, pj) {
						// The do on this line is counted when the line is walked
						// below, so the body is one deeper than here.
						macroBodies = append(macroBodies, functionBody{depth: depth + 1, macro: macro})
					} else {
						inlineMacroLine, inlineMacro = defLine, macro
					}
					// The walk below takes the rest of this line, so a quote on
					// it (`, do: quote do`) is found here.
					if quoteDepth == 0 {
						lineDepth := depth
						for k := pj; k < n && tokens[k].Kind != TokEOL && tokens[k].Kind != TokEOF; k++ {
							switch tokens[k].Kind {
							case TokDo, TokFn:
								lineDepth++
							case TokEnd:
								lineDepth--
							case TokIdent:
								if tokens[k].End-tokens[k].Start == 5 && string(source[tokens[k].Start:tokens[k].End]) == "quote" {
									markQuote(k, lineDepth, macro)
								}
							}
							if quoteDepth != 0 || quoteLine == defLine {
								break
							}
						}
					}
				}
				if insideQuote {
					i = j
					goto extractRefsForLine
				}

				minArity := maxArity - defaultCount
				for arity := minArity; arity <= maxArity; arity++ {
					params := JoinParams(paramNames, arity)
					defs = append(defs, Definition{
						Module:     cm,
						Function:   funcName,
						Arity:      arity,
						Line:       defLine,
						FilePath:   path,
						Kind:       kind,
						DelegateTo: delegateTo,
						DelegateAs: delegateAs,
						Params:     params,
					})
				}
				i = j
			}
			goto extractRefsForLine

		case TokDefstruct:
			cm := currentModule()
			if cm != "" && !inQuote(tok.Line) {
				defs = appendStructDefs(defs, cm, path, tok.Line, "defstruct")
			}
			i++
			goto extractRefsForLine

		case TokDefexception:
			cm := currentModule()
			// The exception's struct; `__exception__` is a key of it, not a
			// function.
			if cm != "" && !inQuote(tok.Line) {
				defs = appendStructDefs(defs, cm, path, tok.Line, "defexception")
			}
			i++
			goto extractRefsForLine

		case TokAlias:
			saveBodyScope()
			aliasLine := tok.Line
			i++
			j := nextSig(i)
			modName, k := collectModuleName(j)
			if modName == "" {
				i = k
				continue
			}
			cm := currentModule()

			// The module name on an alias line is itself resolved through the
			// aliases already in scope: `alias A.B` then `alias B.C`.
			modName = ExpandAliasPrefix(modName, aliases)

			// Multi-alias: alias MyApp.{Users, Accounts}
			if children, nextPos, ok := ScanMultiAliasChildren(source, tokens, n, k, false); ok {
				parentResolved := resolveModule(modName, cm)
				for _, childName := range children {
					fullChild := parentResolved + "." + childName
					aliases[AliasShortName(childName)] = fullChild
					emitModuleRef(fullChild, aliasLine, "alias")
				}
				i = nextPos
				continue
			}

			// Alias with as:
			if asName, nextPos, ok := ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
				resolved := resolveModule(modName, cm)
				if !strings.Contains(resolved, "__MODULE__") {
					aliases[asName] = resolved
					refs = append(refs, Reference{Module: resolved, Line: aliasLine, FilePath: path, Kind: "alias"})
				}
				i = nextPos
				continue
			}

			// Simple alias
			{
				resolved := resolveModule(modName, cm)
				aliases[AliasShortName(resolved)] = resolved
				emitModuleRef(resolved, aliasLine, "alias")
			}
			i = k
			continue

		case TokImport:
			saveBodyScope()
			importLine := tok.Line
			i++
			j := nextSig(i)
			modName, k := collectModuleName(j)
			if modName != "" {
				resolved := ResolveModuleRef(modName, aliases, currentModule())
				if resolved != "" {
					refs = append(refs, Reference{Module: resolved, Line: importLine, FilePath: path, Kind: "import"})
					injectors[resolved] = true
					// `import Mod, only: [fun: 1]` names Mod.fun, on the line
					// of the key, and brings in only the names of the list;
					// except: brings in the others.
					only, except := ScanImportOptionKeys(source, tokens, n, k)
					switch {
					case len(only) > 0:
						f := importFilter{only: map[string]bool{}}
						for _, key := range only {
							name := tokenText(tokens[key])
							f.only[name] = true
							refs = append(refs, Reference{Module: resolved, Function: name, Line: tokens[key].Line, FilePath: path, Kind: "import_only"})
						}
						importFilters[resolved] = f
					case len(except) > 0:
						f := importFilter{except: map[string]bool{}}
						for _, key := range except {
							f.except[tokenText(tokens[key])] = true
						}
						importFilters[resolved] = f
					default:
						delete(importFilters, resolved)
					}
				}
			}
			i = k
			continue

		case TokUse:
			useLine := tok.Line
			i++
			j := nextSig(i)
			modName, k := collectModuleName(j)
			if modName != "" {
				resolved := ResolveModuleRef(modName, aliases, currentModule())
				if resolved != "" {
					if inQuote(useLine) && quoteInMacro {
						// A `use` in the quote of a macro (not __using__)
						// runs in the module that calls the macro: it names
						// the module, but this module does not use it.
						refs = append(refs, Reference{Module: resolved, Line: useLine, FilePath: path, Kind: "quoted_use"})
					} else {
						refs = append(refs, Reference{Module: resolved, Line: useLine, FilePath: path, Kind: "use"})
						injectors[resolved] = true
						// What a __using__ imports is not known here.
						delete(importFilters, resolved)
					}
				}
			}
			i = k
			continue

		case TokRequire:
			requireLine := tok.Line
			i++
			j := nextSig(i)
			modName, k := collectModuleName(j)
			if modName == "" {
				i = k
				goto extractRefsForLine
			}
			cm := currentModule()

			// Check for require Module, as: Name
			if asName, nextPos, ok := ScanKeywordOptionValue(source, tokens, n, k, "as"); ok {
				resolved := ResolveModuleRef(modName, aliases, cm)
				if resolved != "" {
					aliases[asName] = resolved
					refs = append(refs, Reference{Module: resolved, Line: requireLine, FilePath: path, Kind: "require"})
				}
				i = nextPos
				continue
			}

			// Simple require (no as:) — still emit reference but no alias
			resolved := ResolveModuleRef(modName, aliases, cm)
			if resolved != "" {
				refs = append(refs, Reference{Module: resolved, Line: requireLine, FilePath: path, Kind: "require"})
			}
			i = k
			continue

		case TokAttrType:
			declarationIdx := i
			typespecStart = i
			typespecEnd = ScanTypespecEnd(source, tokens, n, i)
			cm := currentModule()
			if cm != "" {
				attrLine := tok.Line
				attrText := tokenText(tok)
				kind := "type"
				switch attrText {
				case "@opaque":
					kind = "opaque"
				case "@typep":
					i++
					goto extractRefsForLine
				}
				name, j, ok := StaticDeclarationName(source, tokens, n, declarationIdx)
				if ok {
					arity := 0
					pj := nextSig(j + 1)
					if pj < n && tokens[pj].Kind == TokOpenParen {
						arity, _, _, _ = collectParamsFromTokens(pj)
					}
					defs = append(defs, Definition{
						Module:   cm,
						Function: name,
						Arity:    arity,
						Line:     attrLine,
						FilePath: path,
						Kind:     kind,
					})
				}
				i = j
			} else {
				i++
			}
			goto extractRefsForLine

		case TokAttrBehaviour:
			cm := currentModule()
			if cm != "" {
				attrLine := tok.Line
				i++
				j := nextSig(i)
				modName, k := collectModuleName(j)
				if modName != "" {
					// `alias Ecto.Changeset.Relation` then `@behaviour Relation`.
					resolved := resolveModule(ExpandAliasPrefix(modName, aliases), cm)
					if !strings.Contains(resolved, "__MODULE__") {
						refs = append(refs, Reference{Module: resolved, Line: attrLine, FilePath: path, Kind: "behaviour"})
					}
				}
				i = k
			} else {
				i++
			}
			goto extractRefsForLine

		case TokAttrCallback:
			declarationIdx := i
			typespecStart = i
			typespecEnd = ScanTypespecEnd(source, tokens, n, i)
			cm := currentModule()
			if cm != "" {
				attrLine := tok.Line
				attrText := tokenText(tok)
				kind := "callback"
				if attrText == "@macrocallback" {
					kind = "macrocallback"
				}
				name, j, ok := StaticDeclarationName(source, tokens, n, declarationIdx)
				if ok {
					arity := 0
					pj := nextSig(j + 1)
					if pj < n && tokens[pj].Kind == TokOpenParen {
						arity, _, _, _ = collectParamsFromTokens(pj)
					}
					defs = append(defs, Definition{
						Module:   cm,
						Function: name,
						Arity:    arity,
						Line:     attrLine,
						FilePath: path,
						Kind:     kind,
					})
				}
				i = j
			} else {
				i++
			}
			goto extractRefsForLine

		case TokAttrSpec:
			typespecStart = i
			typespecEnd = ScanTypespecEnd(source, tokens, n, i)
			i++
			goto extractRefsForLine

		case TokAttrDoc, TokAttr:
			// @derive Protocol, {Protocol, opts}, or a list of them: each is an
			// implementation of the protocol for this module, which the
			// protocol's __deriving__ (or its Any implementation) generates.
			// The row is a reference to the protocol, with the deriving module
			// in its function column. In a quote, it derives for the caller.
			if tok.Kind == TokAttr && tok.End-tok.Start == 7 && string(source[tok.Start:tok.End]) == "@derive" {
				if cm := currentModule(); cm != "" && !inQuote(tok.Line) {
					for _, p := range DeriveProtocols(source, tokens, n, i+1) {
						resolved := ResolveModuleRef(p, aliases, cm)
						if resolved != "" && !strings.Contains(resolved, "__MODULE__") {
							refs = append(refs, Reference{Module: resolved, Function: cm, Line: tok.Line, FilePath: path, Kind: "derive"})
						}
					}
				}
			}
			// An attribute read inside an expression (`f(@handler, from(x))`)
			// is a value: the walk goes on, and finds the calls after it.
			if tok.Kind == TokAttr && i > 0 && tokens[i-1].Kind != TokEOL && tokens[i-1].Kind != TokComment {
				i++
				continue
			}
			i++
			goto extractRefsForLine

		case TokPercent:
			// %Module{ struct literal
			if i+1 < n && tokens[i+1].Kind == TokModule && isUserModule(source, tokens[i+1]) {
				modName, k := collectModuleName(i + 1)
				if k < n && tokens[k].Kind == TokOpenBrace {
					cm := currentModule()
					resolved := ResolveModuleRef(modName, aliases, cm)
					if resolved != "" {
						refs = append(refs, Reference{Module: resolved, Line: tok.Line, FilePath: path, Kind: callKind(i)})
					}
					i = k + 1
					continue
				}
			}
			i++
			continue

		case TokModule:
			// `__MODULE__.fun()` and `__MODULE__.Sub.fun()` call a function of
			// the module (or of a nested one), which the walk knows.
			if tokenText(tok) == "__MODULE__" {
				if ref, k, ok := selfModuleCall(source, tokens, n, i, selfModuleAt(tok.Line)); ok {
					ref.FilePath, ref.Kind = path, callKind(i)
					refs = append(refs, ref)
					i = k
				} else {
					// `__MODULE__.Sub` names cm.Sub, not a module Sub.
					_, i = collectModuleName(i)
				}
				continue
			}
			// Skip __MODULE__ and other non-ASCII-uppercase module tokens
			if !isUserModule(source, tok) {
				i++
				continue
			}

			cm := currentModule()
			modName, k := collectModuleName(i)

			// Skip if preceded by % (struct literal handled by TokPercent case)
			if i > 0 && tokens[i-1].Kind == TokPercent {
				i = k
				continue
			}

			// Module.function call
			if k < n && tokens[k].Kind == TokDot && k+1 < n && tokens[k+1].Kind == TokIdent {
				funcName := tokenText(tokens[k+1])
				if !elixirKeyword[funcName] {
					resolved := ResolveModuleRef(modName, aliases, cm)
					if resolved != "" {
						refs = append(refs, Reference{Module: resolved, Function: funcName, Line: tok.Line, FilePath: path, Kind: callKind(i)})
					}
				}
				i = k + 2
				continue
			}

			// Standalone module ref (skip self-references)
			if modName != cm {
				resolved := ResolveModuleRef(modName, aliases, cm)
				if resolved != "" {
					refs = append(refs, Reference{Module: resolved, Line: tok.Line, FilePath: path, Kind: callKind(i)})
				}
			}
			i = k
			continue

		case TokPipe:
			// Also outside any module: a script's top-level import applies
			// to its top-level calls.
			if len(injectors) > 0 {
				j := nextSig(i + 1)
				if j < n && tokens[j].Kind == TokIdent {
					name := tokenText(tokens[j])
					if !elixirKeyword[name] {
						emitInjected(name, j)
					}
				}
			}
			i++
			continue

		case TokIdent:
			if quoteDepth == 0 && tok.End-tok.Start == 5 && string(source[tok.Start:tok.End]) == "quote" {
				if len(macroBodies) > 0 {
					markQuote(i, depth, macroBodies[len(macroBodies)-1].macro)
				} else if followsInlineMacroHead(source, tokens, i, inlineMacroLine) {
					markQuote(i, depth, inlineMacro)
				}
			}
			if len(injectors) > 0 {
				isStatementStart := i == 0 || tokens[i-1].Kind == TokEOL || tokens[i-1].Kind == TokComment
				// A parenthesized bare call can be nested inside another call,
				// collection, or keyword value. It is still a candidate for a
				// function imported by use/import, just like a statement-level call.
				// Anonymous-function calls have a TokDot before the opening paren,
				// and a qualified or variable call (`adapter.insert(1)`) has one
				// before the name, so neither matches.
				isParenthesizedCall := i+1 < n && tokens[i+1].Kind == TokOpenParen && (i == 0 || tokens[i-1].Kind != TokDot)
				if !isParenthesizedCall && captureArity(source, tokens, n, i) >= 0 {
					// &name/2 captures the imported function.
					if name := tokenText(tok); !elixirKeyword[name] {
						emitInjected(name, i)
					}
					i++
					continue
				}
				if !isStatementStart && !isParenthesizedCall && tokens[i-1].Kind != TokDot && NoParenArgCount(source, tokens, i) >= 0 {
					// A call without parentheses inside an expression:
					// `value = name arg`, `do: name arg`.
					if name := tokenText(tok); !elixirKeyword[name] {
						emitInjected(name, i)
					}
					i++
					continue
				}
				if isStatementStart || isParenthesizedCall {
					name := tokenText(tok)
					if !elixirKeyword[name] {
						emit := false
						j := i + 1
						if j < n {
							switch tokens[j].Kind {
							case TokDo:
								// macro_name do
								emit = true
							case TokOpenParen:
								// macro_name(...)
								emit = true
							case TokAtom:
								// macro_name :atom
								emit = true
							default:
								// Scan forward to see if a block-opening `do` follows the
								// arguments, respecting bracket depth and statement boundaries.
								_, _, hasDo := ScanForwardToMacroCallBlockDo(tokens, n, j)
								emit = hasDo
								if !emit && isStatementStart {
									// Injected DSL calls commonly omit both parentheses and a do
									// block (`authorize_if always()`). At statement start, an
									// argument-shaped next token distinguishes those calls from
									// assignments and operators such as `value = 1`.
									switch tokens[j].Kind {
									case TokIdent, TokModule, TokString, TokHeredoc, TokSigil,
										TokCharLiteral, TokNumber, TokOpenBrace,
										TokPercent:
										emit = true
									case TokOpenBracket:
										// `name [1]` is a call; `name[key]` is access on a
										// variable.
										emit = tokens[j].Start > tok.End
									}
								}
							}
						}
						if emit {
							emitInjected(name, i)
						}
					}
				}
			}
			i++
			continue
		}

		i++
		continue

	extractRefsForLine:
		{
			triggerLine := tok.Line
			lineStart := i
			for lineStart > 0 && tokens[lineStart-1].Line == triggerLine && tokens[lineStart-1].Kind != TokEOL {
				lineStart--
			}
			lineEnd := i
			for lineEnd < n && tokens[lineEnd].Kind != TokEOL && tokens[lineEnd].Kind != TokEOF {
				lineEnd++
			}

			// Track depth changes (TokDo/TokFn/TokEnd) on this line so that
			// def/defp/case/fn blocks that open here are properly counted.
			trackLineDepth(lineStart, lineEnd)

			extractModuleRefs(lineStart, lineEnd)

			// Check for pipe calls on this line
			if len(injectors) > 0 {
				for j := lineStart; j < lineEnd; j++ {
					if tokens[j].Kind == TokPipe {
						pj := nextSig(j + 1)
						if pj < lineEnd && tokens[pj].Kind == TokIdent {
							name := tokenText(tokens[pj])
							if !elixirKeyword[name] {
								emitInjected(name, pj)
							}
						}
					}
				}
			}

			// Unqualified calls, with or without parentheses, and captures,
			// on a def line: a one-line body (`def f(x), do: cast(x)`,
			// `do: cast x`), a guard, a default argument.
			// The walk skips the rest of the line, and the declared name is
			// not a call.
			if len(injectors) > 0 && lineStart < lineEnd && isDefinitionToken(tokens[lineStart].Kind) {
				_, nameIdx, _ := StaticDeclarationName(source, tokens, n, lineStart)
				for j := lineStart + 1; j+1 < lineEnd; j++ {
					if j == nameIdx || tokens[j].Kind != TokIdent || tokens[j-1].Kind == TokDot || tokens[j-1].Kind == TokPipe {
						continue
					}
					if tokens[j+1].Kind != TokOpenParen && captureArity(source, tokens, n, j) < 0 && NoParenArgCount(source, tokens, j) < 0 {
						continue
					}
					name := tokenText(tokens[j])
					if elixirKeyword[name] {
						continue
					}
					emitInjected(name, j)
				}
			}

			// Advance past this line
			for i < n && tokens[i].Kind != TokEOL && tokens[i].Kind != TokEOF {
				i++
			}
		}
	}

	if interpPos < len(interp) {
		flushInterpRefs(len(source) + 1)
	}

	if len(injected) > 0 {
		type nameArity struct {
			name  string
			arity int
		}
		defined := map[string]map[nameArity]bool{}
		for _, d := range defs {
			switch d.Kind {
			case "def", "defp", "defmacro", "defmacrop", "defguard", "defguardp", "defdelegate":
			default:
				// A @callback or @type of the name is not a function that
				// a call can reach.
				continue
			}
			if defined[d.Module] == nil {
				defined[d.Module] = map[nameArity]bool{}
			}
			defined[d.Module][nameArity{d.Function, d.Arity}] = true
		}
		// A call whose arity the module defines is the module's own
		// function. A call whose arity is not known stays.
		drop := map[int]bool{}
		for _, r := range injected {
			if r.arity >= 0 && defined[r.module][nameArity{refs[r.idx].Function, r.arity}] {
				drop[r.idx] = true
			}
		}
		if len(drop) > 0 {
			kept := refs[:0]
			for i, r := range refs {
				if !drop[i] {
					kept = append(kept, r)
				}
			}
			refs = kept
		}
	}
	return defs, dedupeRefs(refs), nil
}

// callRefKind is the kind every interpolated reference carries: a typespec
// holds no string, so the typespec/call distinction cannot arise there.
func callRefKind(int) string { return "call" }

// collectModuleRefs walks toks[from:to) and appends a reference for every
// module name it finds: `Mod.fun(...)` as a call, `%Mod{}` and a bare `Mod` as
// a module reference. It serves both token streams — the file's own tokens and
// the interpolation tokens the tokenizer keeps beside them — so the two cannot
// drift apart. kindAt gives the reference kind for a token index, which is how
// a typespec reference is told apart from a call.
func collectModuleRefs(source []byte, path string, toks []Token, from, to int, aliases map[string]string, cm, selfModule string, kindAt func(int) string, refs []Reference) []Reference {
	tn := len(toks)
	for j := from; j < to; j++ {
		tok := toks[j]

		// %Module{ struct literal
		if tok.Kind == TokPercent && j+1 < to && toks[j+1].Kind == TokModule && isUserModule(source, toks[j+1]) {
			modName, k := CollectModuleName(source, toks, tn, j+1)
			if k < to && toks[k].Kind == TokOpenBrace {
				if resolved := ResolveModuleRef(modName, aliases, cm); resolved != "" {
					refs = append(refs, Reference{Module: resolved, Line: tok.Line, FilePath: path, Kind: kindAt(j)})
				}
				j = k
				continue
			}
		}

		if tok.Kind == TokModule && TokenText(source, tok) == "__MODULE__" {
			if ref, k, ok := selfModuleCall(source, toks, tn, j, selfModule); ok && k <= to {
				ref.FilePath, ref.Kind = path, kindAt(j)
				refs = append(refs, ref)
				j = k - 1
			} else {
				// `__MODULE__.Sub` names cm.Sub, not a module Sub.
				_, k := CollectModuleName(source, toks, tn, j)
				j = k - 1
			}
			continue
		}

		if tok.Kind != TokModule || !isUserModule(source, tok) {
			continue
		}

		modName, k := CollectModuleName(source, toks, tn, j)

		// Skip if preceded by % (struct literal already handled above)
		if j > 0 && toks[j-1].Kind == TokPercent {
			j = k - 1
			continue
		}

		// Module.function call
		if k < to && toks[k].Kind == TokDot && k+1 < to && toks[k+1].Kind == TokIdent {
			funcName := TokenText(source, toks[k+1])
			if !elixirKeyword[funcName] {
				if resolved := ResolveModuleRef(modName, aliases, cm); resolved != "" {
					refs = append(refs, Reference{Module: resolved, Function: funcName, Line: tok.Line, FilePath: path, Kind: kindAt(j)})
				}
			}
			j = k + 1
			continue
		}

		// Standalone module ref (skip self-references)
		if modName != cm {
			if resolved := ResolveModuleRef(modName, aliases, cm); resolved != "" {
				refs = append(refs, Reference{Module: resolved, Line: tok.Line, FilePath: path, Kind: kindAt(j)})
			}
		}
		j = k - 1
	}
	return refs
}

// isUserModule reports whether a TokModule token names a user-defined module
// (an ASCII uppercase start), as opposed to __MODULE__.
func isUserModule(source []byte, t Token) bool {
	return source[t.Start] >= 'A' && source[t.Start] <= 'Z'
}

// dedupeRefs removes exact duplicate rows from refs, preserving first-occurrence
// order. The same call site can be emitted more than once — a piped call is
// reachable both from the TokPipe case and from the pipe scan inside
// extractRefsForLine — and injector fan-out can repeat a row when a module is
// reached through two paths. Identical rows are indistinguishable to every
// query (no query counts refs; the References handler dedupes by file+line
// anyway), so dropping them cannot change a result. It is worth doing here
// because this runs in the parallel parse workers, while the rows it removes
// would otherwise cost time in the single-threaded SQLite writer.
func dedupeRefs(refs []Reference) []Reference {
	if len(refs) < 2 {
		return refs
	}
	seen := make(map[Reference]struct{}, len(refs))
	out := refs[:0]
	for _, r := range refs {
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// Exported token-walking helpers shared with the LSP package.

// LineColToOffset converts a 0-based (line, col) pair to a byte offset using
// the LineStarts table from TokenizeFull. Returns -1 if out of range.
func LineColToOffset(lineStarts []int, line, col int) int {
	if line < 0 || line >= len(lineStarts) {
		return -1
	}
	return lineStarts[line] + col
}

// TokenAtOffset returns the index of the token containing byteOffset, or -1
// if the offset falls in a gap between tokens (whitespace) or is out of range.
// Uses binary search for O(log n) lookup.
func TokenAtOffset(tokens []Token, byteOffset int) int {
	lo, hi := 0, len(tokens)-1
	for lo <= hi {
		mid := lo + (hi-lo)/2
		t := tokens[mid]
		if byteOffset < t.Start {
			hi = mid - 1
		} else if byteOffset >= t.End {
			lo = mid + 1
		} else {
			return mid
		}
	}
	return -1
}

func TokenText(source []byte, t Token) string {
	return string(source[t.Start:t.End])
}

func NextSigToken(tokens []Token, n, from int) int {
	for from < n && (tokens[from].Kind == TokEOL || tokens[from].Kind == TokComment) {
		from++
	}
	return from
}

// isValidFuncNameToken reports whether a token kind can appear as a function
// name after def/defp/defmacro/defmacrop/defguard/defguardp/defdelegate.
// In addition to ordinary identifiers (TokIdent), this includes tokens like
// TokDefstruct and TokDefprotocol that are used as function names in the
// stdlib (e.g. `defmacro defstruct(fields)` in Kernel).
func isValidFuncNameToken(kind TokenKind) bool {
	return kind == TokIdent ||
		kind == TokDefstruct || kind == TokDefexception ||
		kind == TokDefprotocol || kind == TokDefimpl
}

func CollectModuleName(source []byte, tokens []Token, n, i int) (string, int) {
	if i >= n || tokens[i].Kind != TokModule {
		return "", i
	}
	var parts []string
	parts = append(parts, string(source[tokens[i].Start:tokens[i].End]))
	i++
	for i+1 < n && tokens[i].Kind == TokDot && tokens[i+1].Kind == TokModule {
		parts = append(parts, string(source[tokens[i+1].Start:tokens[i+1].End]))
		i += 2
	}
	return strings.Join(parts, "."), i
}

func CollectParams(source []byte, tokens []Token, n, i int) (int, int, []string, int) {
	if i >= n || tokens[i].Kind != TokOpenParen {
		return 0, 0, nil, i
	}
	i++
	bracketDepth := 1
	commas := 0
	defaults := 0
	hasContent := false
	var paramNames []string
	currentParamName := ""
	seenDefault := false

	for i < n && bracketDepth > 0 {
		tok := tokens[i]
		switch tok.Kind {
		case TokOpenParen, TokOpenBracket, TokOpenBrace:
			bracketDepth++
			hasContent = true
			i++
		case TokOpenAngle:
			bracketDepth++
			hasContent = true
			i++
		case TokCloseAngle:
			bracketDepth--
			i++
		case TokCloseParen, TokCloseBracket, TokCloseBrace:
			bracketDepth--
			if bracketDepth == 0 {
				if hasContent {
					if seenDefault {
						defaults++
					}
					paramNames = append(paramNames, currentParamName)
				}
				i++
				return commas + boolToInt(hasContent), defaults, paramNames, i
			}
			i++
		case TokComma:
			if bracketDepth == 1 {
				commas++
				if seenDefault {
					defaults++
				}
				paramNames = append(paramNames, currentParamName)
				currentParamName = ""
				seenDefault = false
			}
			i++
		case TokBackslash:
			if bracketDepth == 1 {
				seenDefault = true
			}
			hasContent = true
			i++
		case TokIdent:
			if bracketDepth == 1 && currentParamName == "" {
				name := string(source[tok.Start:tok.End])
				if name != "_" {
					currentParamName = name
				}
			}
			hasContent = true
			i++
		case TokOther:
			if bracketDepth == 1 && tok.End-tok.Start == 1 && source[tok.Start] == '=' {
				currentParamName = ""
			}
			hasContent = true
			i++
		case TokEOL, TokComment:
			i++
		default:
			hasContent = true
			i++
		}
	}
	if hasContent {
		if seenDefault {
			defaults++
		}
		paramNames = append(paramNames, currentParamName)
		return commas + 1, defaults, paramNames, i
	}
	return 0, 0, nil, i
}

func FixParamNames(names []string) []string {
	for idx, name := range names {
		if name == "" {
			names[idx] = "arg" + itoa(idx+1)
		}
	}
	return names
}

// opensDoBlock reports whether the declaration whose head ends before tokens[from]
// opens a do-block, rather than taking `, do:` or having no body. The block
// opens on the head's line, or after a guard that mix format puts on the next
// line (`defmacro m(x)` then `when is_atom(x) do`).
func opensDoBlock(tokens []Token, n, from int) bool {
	parens := 0
	for k := from; k < n; k++ {
		switch tokens[k].Kind {
		case TokDo:
			return true
		case TokOpenParen, TokOpenBracket, TokOpenBrace:
			parens++
		case TokCloseParen, TokCloseBracket, TokCloseBrace:
			parens--
		case TokComma:
			if parens <= 0 {
				return false
			}
		case TokEOL:
			if parens > 0 || tokens[k-1].Kind == TokWhen {
				continue
			}
			if next := NextSigToken(tokens, n, k); next < n && tokens[next].Kind == TokWhen {
				continue
			}
			return false
		case TokEOF:
			return false
		}
	}
	return false
}

type quoteShape int

const (
	quoteNone quoteShape = iota
	quoteBlock
	quoteInline
)

// quoteForm reads what follows a `quote` identifier: options such as
// `location: :keep` or `bind_quoted: [...]`, which may span lines inside
// brackets, then either a do-block or an inline `do:`. For the inline form it
// also returns the line of the `do:`.
func quoteForm(source []byte, tokens []Token, n, from int) (quoteShape, int) {
	balance := 0
	// A trailing comma carries the options on to the next line.
	continued := false
	for k := from; k < n; k++ {
		kind := tokens[k].Kind
		switch kind {
		case TokOpenParen, TokOpenBracket, TokOpenBrace:
			balance++
		case TokCloseParen, TokCloseBracket, TokCloseBrace:
			balance--
			if balance < 0 {
				return quoteNone, 0
			}
		case TokDo:
			if balance == 0 {
				return quoteBlock, 0
			}
		case TokIdent:
			if TokenText(source, tokens[k]) == "do" && k+1 < n && tokens[k+1].Kind == TokColon {
				return quoteInline, tokens[k].Line
			}
		case TokEOL:
			if balance == 0 && !continued {
				return quoteNone, 0
			}
		case TokEOF:
			return quoteNone, 0
		}
		if kind != TokEOL && kind != TokComment {
			continued = kind == TokComma
		}
	}
	return quoteNone, 0
}

// followsInlineMacroHead reports whether the token at i comes right after the
// `do:` that ends the head of a macro on line, as in
//
//	defmacro route(verb, path), do:
//	  quote do
func followsInlineMacroHead(source []byte, tokens []Token, i, line int) bool {
	if line == 0 {
		return false
	}
	k := i - 1
	for k >= 0 && (tokens[k].Kind == TokEOL || tokens[k].Kind == TokComment) {
		k--
	}
	return k >= 1 && tokens[k].Kind == TokColon && tokens[k-1].Kind == TokIdent &&
		TokenText(source, tokens[k-1]) == "do" && tokens[k-1].Line == line
}

// isDefinitionToken reports whether kind starts a function definition.
func isDefinitionToken(kind TokenKind) bool {
	switch kind {
	case TokDef, TokDefp, TokDefmacro, TokDefmacrop, TokDefguard, TokDefguardp, TokDefdelegate:
		return true
	}
	return false
}

// importFilter is the only: or except: list of an import.
type importFilter struct {
	only, except map[string]bool
}

func (f importFilter) allows(name string) bool {
	if f.only != nil {
		return f.only[name]
	}
	return !f.except[name]
}

func copyImportFilters(m map[string]importFilter) map[string]importFilter {
	cp := make(map[string]importFilter, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// callArity returns the arity of the unqualified call whose name is at pos:
// the arguments in its parentheses, or after it with no parentheses
// (trailing keyword pairs are one), one more for a value piped into it and
// one more for a do block. It returns -1 when the tokens do not show the
// arity (a name alone can be a variable or a call with no arguments).
func callArity(source []byte, tokens []Token, n, pos int) int {
	if a := captureArity(source, tokens, n, pos); a >= 0 {
		return a
	}
	piped := false
	if prev := prevSigToken(tokens, pos-1); prev >= 0 && tokens[prev].Kind == TokPipe {
		piped = true
	}
	open := pos + 1
	if open >= n || tokens[open].Kind != TokOpenParen {
		args := NoParenArgCount(source, tokens, pos)
		if piped {
			// `value |> name`, `value |> name arg`
			return 1 + max(args, 0)
		}
		return args
	}
	args := CallArgCount(tokens, open)
	if args < 0 {
		return -1
	}
	if piped {
		args++
	}
	return args
}

// CallArgCount counts the arguments of the call whose open parenthesis is
// tokens[open]: trailing keyword pairs (`f(a, b: 1, c: 2)`) are one, and a
// do block after the parentheses is one more, unless the call is an
// argument of another call (in `case f(x) do`, the do is case's). It returns
// -1 when the parentheses do not close.
func CallArgCount(tokens []Token, open int) int {
	n := len(tokens)
	depth := 0
	args := 0
	inKeywords := false
	argStart := true
	closeIdx := -1
	angles := 0 // open bitstrings, whose commas separate segments
scan:
	for i := open + 1; i < n; i++ {
		t := tokens[i]
		switch t.Kind {
		case TokEOL, TokComment:
			continue
		case TokEOF:
			return -1
		case TokOpenAngle, TokCloseAngle:
			angles = bitstringDepth(tokens, i, angles)
		}
		if argStart && depth == 0 && angles == 0 && t.Kind != TokCloseParen {
			argStart = false
			isKey := t.Kind == TokIdent && i+1 < n && tokens[i+1].Kind == TokColon
			if !isKey || !inKeywords {
				args++
			}
			if isKey {
				inKeywords = true
			}
		}
		switch t.Kind {
		case TokOpenParen, TokOpenBracket, TokOpenBrace, TokDo, TokFn:
			depth++
		case TokCloseParen, TokCloseBracket, TokCloseBrace, TokEnd:
			if depth == 0 {
				if t.Kind != TokCloseParen {
					return -1
				}
				closeIdx = i
				break scan
			}
			depth--
		case TokComma:
			if depth == 0 && angles == 0 {
				argStart = true
			}
		}
	}
	if closeIdx < 0 {
		return -1
	}
	if next := closeIdx + 1; next < n && tokens[next].Kind == TokDo && ownsDoBlock(tokens, open-1) {
		args++
	}
	return args
}

// NoParenArgCount counts the arguments of a call written without
// parentheses, `name a, b` or `Mod.name a, b` (trailing keyword pairs are
// one, and a do block that the call owns is one more), with the name at
// tokens[pos]. It returns -1 when the tokens after the name do not start an
// argument on the same line after a space, so the name is not surely such a
// call (`name`, `name.field`, `name[0]`, `name + 1`, `name in list`).
func NoParenArgCount(source []byte, tokens []Token, pos int) int {
	n := len(tokens)
	j := pos + 1
	if j >= n || tokens[j].Start == tokens[pos].End {
		return -1
	}
	switch tokens[j].Kind {
	case TokIdent:
		switch TokenText(source, tokens[j]) {
		case "in", "and", "or", "not", "when":
			return -1
		}
	case TokModule, TokString, TokHeredoc, TokSigil, TokCharLiteral, TokNumber, TokAtom,
		TokOpenBracket, TokOpenBrace, TokPercent, TokAttr, TokFn:
	case TokOpenAngle:
		// `name <<1, 2>>`, but not `name <<< 1`.
		if bitstringDepth(tokens, j, 0) == 0 {
			return -1
		}
	case TokOther:
		// A capture, `name &other/1` or `name &(&1 + 1)`. A unary minus
		// (`name -1`) is left out: after a variable it is a subtraction.
		if TokenText(source, tokens[j]) != "&" || j+1 >= n || tokens[j+1].Start != tokens[j].End {
			return -1
		}
		switch tokens[j+1].Kind {
		case TokIdent, TokModule, TokOpenParen, TokNumber:
		default:
			return -1
		}
	default:
		return -1
	}
	depth := 0
	args := 0
	inKeywords := false
	argStart := true
	angles := 0
scan:
	for i := j; i < n; i++ {
		t := tokens[i]
		switch t.Kind {
		case TokOpenAngle, TokCloseAngle:
			angles = bitstringDepth(tokens, i, angles)
		}
		switch t.Kind {
		case TokComment:
			continue
		case TokEOF:
			break scan
		case TokEOL:
			if depth > 0 || angles > 0 {
				continue
			}
			// A line that ends with a comma continues the arguments.
			if prev := prevSigToken(tokens, i-1); prev >= 0 && tokens[prev].Kind == TokComma {
				continue
			}
			break scan
		}
		if depth == 0 && angles == 0 {
			switch t.Kind {
			case TokDo:
				if ownsDoBlock(tokens, pos) {
					args++
				}
				break scan
			case TokEnd, TokCloseParen, TokCloseBracket, TokCloseBrace, TokRightArrow, TokWhen:
				break scan
			}
		}
		if argStart && depth == 0 && angles == 0 {
			argStart = false
			isKey := t.Kind == TokIdent && i+1 < n && tokens[i+1].Kind == TokColon
			if !isKey || !inKeywords {
				args++
			}
			if isKey {
				inKeywords = true
			}
		}
		switch t.Kind {
		case TokOpenParen, TokOpenBracket, TokOpenBrace, TokDo, TokFn:
			depth++
		case TokCloseParen, TokCloseBracket, TokCloseBrace, TokEnd:
			depth--
		case TokComma:
			if depth == 0 && angles == 0 {
				argStart = true
			}
		}
	}
	return args
}

// bitstringDepth returns the count of open bitstrings after tokens[i], a
// `<<` or `>>`, when angles were open before it. `<<<` and `>>>` (Bitwise)
// tokenize as `<<` or `>>` with an operator byte right after; they open and
// close nothing, nor does a `>>` with no `<<` open.
func bitstringDepth(tokens []Token, i, angles int) int {
	if i+1 < len(tokens) && tokens[i+1].Kind == TokOther && tokens[i+1].Start == tokens[i].End {
		return angles
	}
	if tokens[i].Kind == TokOpenAngle {
		return angles + 1
	}
	if angles > 0 {
		return angles - 1
	}
	return angles
}

// ownsDoBlock reports whether a do block after the call whose name is
// tokens[name] (with its qualifier, `Mod.name`) is the call's: the call
// starts its statement or follows an operator (`x = f(a) do`). After a name
// (`case f(a) do`), a comma, or an open bracket, the call is an argument of
// another call, which owns the block.
func ownsDoBlock(tokens []Token, name int) bool {
	k := name
	for k-1 >= 0 && tokens[k-1].Kind == TokDot && k-2 >= 0 && (tokens[k-2].Kind == TokModule || tokens[k-2].Kind == TokIdent) {
		k -= 2
	}
	if k == 0 {
		return true
	}
	switch tokens[k-1].Kind {
	case TokEOL, TokComment, TokOther, TokPipe:
		return true
	}
	return false
}

// prevSigToken returns the index of the last token at or before i that is
// not an end of line or a comment, or -1.
func prevSigToken(tokens []Token, i int) int {
	for i >= 0 && (tokens[i].Kind == TokEOL || tokens[i].Kind == TokComment) {
		i--
	}
	return i
}

// captureArity returns the arity of a capture `&name/2` whose name is at
// pos, or -1 when the name is not captured that way.
func captureArity(source []byte, tokens []Token, n, pos int) int {
	if pos == 0 || tokens[pos-1].Kind != TokOther || TokenText(source, tokens[pos-1]) != "&" {
		return -1
	}
	if pos+2 >= n || tokens[pos+1].Kind != TokOther || TokenText(source, tokens[pos+1]) != "/" || tokens[pos+2].Kind != TokNumber {
		return -1
	}
	a := 0
	for _, c := range TokenText(source, tokens[pos+2]) {
		if c < '0' || c > '9' {
			return -1
		}
		a = a*10 + int(c-'0')
	}
	return a
}

// selfModuleCall reads `__MODULE__.fun` or `__MODULE__.Sub.fun` at toks[i],
// a call of a function of cm (or of a module nested in it). k is the token
// after the function name. ok is false outside a module and for a
// `__MODULE__` that no function call follows.
func selfModuleCall(source []byte, toks []Token, n, i int, cm string) (ref Reference, k int, ok bool) {
	if cm == "" {
		return Reference{}, i, false
	}
	modName, k := CollectModuleName(source, toks, n, i)
	if k+1 >= n || toks[k].Kind != TokDot || toks[k+1].Kind != TokIdent {
		return Reference{}, i, false
	}
	funcName := TokenText(source, toks[k+1])
	if elixirKeyword[funcName] {
		return Reference{}, i, false
	}
	return Reference{Module: cm + strings.TrimPrefix(modName, "__MODULE__"), Function: funcName, Line: toks[i].Line}, k + 2, true
}

// StructFunctions are the functions that defstruct and defexception define:
// __struct__/0 and __struct__/1, with the parameter name Kernel gives the
// second.
var StructFunctions = [2]struct {
	Arity  int
	Params string
}{{0, ""}, {1, "kv"}}

func appendStructDefs(defs []Definition, module, path string, line int, kind string) []Definition {
	for _, f := range StructFunctions {
		defs = append(defs, Definition{
			Module:   module,
			Function: "__struct__",
			Arity:    f.Arity,
			Line:     line,
			FilePath: path,
			Kind:     kind,
			Params:   f.Params,
		})
	}
	return defs
}

// DeriveProtocols returns the protocols, as written, that an `@derive` value
// starting at tokens[from] names: `P`, `{P, opts}`, `[P, {Q, opts}]`, with or
// without parentheses. Modules in the options are not protocols.
func DeriveProtocols(source []byte, tokens []Token, n, from int) []string {
	var out []string
	// stack holds the open brackets inside the value. A protocol is a module
	// that is the value itself, an element of the outer list, or the first
	// element of a tuple in one of those places.
	var stack []TokenKind
	outer := func() int {
		// The number of brackets that do not count: a leading parenthesis.
		if len(stack) > 0 && stack[0] == TokOpenParen {
			return 1
		}
		return 0
	}
	prevSig := TokAttr
	for k := from; k < n; k++ {
		kind := tokens[k].Kind
		switch kind {
		case TokEOF:
			return out
		case TokEOL, TokComment:
			if len(stack) == 0 {
				return out
			}
			continue
		case TokOpenParen, TokOpenBracket, TokOpenBrace:
			stack = append(stack, kind)
		case TokCloseParen, TokCloseBracket, TokCloseBrace:
			if len(stack) == 0 {
				return out
			}
			stack = stack[:len(stack)-1]
		case TokModule:
			name, next := CollectModuleName(source, tokens, n, k)
			if name != "" && derivePosition(stack[outer():], prevSig) {
				out = append(out, name)
			}
			if next > k {
				k = next - 1
			}
			prevSig = TokModule
			continue
		}
		prevSig = kind
	}
	return out
}

// derivePosition reports whether a module at this nesting, after prevSig, is
// a protocol in an @derive value.
func derivePosition(stack []TokenKind, prevSig TokenKind) bool {
	switch len(stack) {
	case 0:
		return prevSig == TokAttr || prevSig == TokOpenParen
	case 1:
		switch stack[0] {
		case TokOpenBracket:
			return prevSig == TokOpenBracket || prevSig == TokComma
		case TokOpenBrace:
			return prevSig == TokOpenBrace
		}
	case 2:
		return stack[0] == TokOpenBracket && stack[1] == TokOpenBrace && prevSig == TokOpenBrace
	}
	return false
}
