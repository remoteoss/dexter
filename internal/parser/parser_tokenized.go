package parser

import "strings"

// parseTextFromTokens is the token-stream replacement for the line-based ParseText.
// It walks a []Token stream from the tokenizer and produces identical Definition
// and Reference output.
func parseTextFromTokens(path string, source []byte, tokens []Token) ([]Definition, []Reference, error) {
	var defs []Definition
	var refs []Reference

	type moduleFrame struct {
		name           string
		depth          int
		savedAliases   map[string]string
		savedInjectors map[string]bool
	}

	var moduleStack []moduleFrame
	depth := 0
	aliases := map[string]string{}
	injectors := map[string]bool{}

	n := len(tokens)

	tokenText := func(t Token) string {
		return string(source[t.Start:t.End])
	}

	nextSig := func(from int) int {
		for from < n && (tokens[from].Kind == TokEOL || tokens[from].Kind == TokComment) {
			from++
		}
		return from
	}

	// isUserModuleToken returns true if the TokModule token represents a user-defined
	// module name (starts with ASCII uppercase). Returns false for __MODULE__.
	isUserModuleToken := func(t Token) bool {
		return source[t.Start] >= 'A' && source[t.Start] <= 'Z'
	}

	collectModuleName := func(i int) (string, int) {
		if i >= n || tokens[i].Kind != TokModule {
			return "", i
		}
		var parts []string
		parts = append(parts, tokenText(tokens[i]))
		i++
		for i+1 < n && tokens[i].Kind == TokDot && tokens[i+1].Kind == TokModule {
			parts = append(parts, tokenText(tokens[i+1]))
			i += 2
		}
		return strings.Join(parts, "."), i
	}

	collectParamsFromTokens := func(i int) (int, int, []string, int) {
		if i >= n || tokens[i].Kind != TokOpenParen {
			return 0, 0, nil, i
		}
		i++ // consume open paren
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
			case TokOpenAngle:
				bracketDepth++
				hasContent = true
				i++
			case TokCloseAngle:
				bracketDepth--
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
					name := tokenText(tok)
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

	fixParamNames := func(names []string) []string {
		for idx, name := range names {
			if name == "" {
				names[idx] = "arg" + itoa(idx+1)
			}
		}
		return names
	}

	currentModule := func() string {
		if len(moduleStack) > 0 {
			return moduleStack[len(moduleStack)-1].name
		}
		return ""
	}

	// processModuleDef handles defmodule/defprotocol/defimpl.
	// It collects the module name, scans forward to consume the TokDo token,
	// increments depth, and pushes the module frame with the post-increment depth.
	processModuleDef := func(i int, kind string) int {
		kwLine := tokens[i-1].Line
		j := nextSig(i)
		name, j := collectModuleName(j)
		if name == "" {
			return i
		}
		if !strings.Contains(name, ".") && currentModule() != "" {
			name = currentModule() + "." + name
		}

		// Scan forward to find and consume TokDo (skipping "for: Module" etc.)
		scanPos := j
		for scanPos < n {
			if tokens[scanPos].Kind == TokDo {
				depth++
				scanPos++ // consume TokDo
				break
			}
			if tokens[scanPos].Kind == TokEOL || tokens[scanPos].Kind == TokEOF {
				break
			}
			scanPos++
		}

		moduleStack = append(moduleStack, moduleFrame{
			name:           name,
			depth:          depth,
			savedAliases:   copyMap(aliases),
			savedInjectors: copyBoolMap(injectors),
		})
		defs = append(defs, Definition{
			Module:   name,
			Line:     kwLine,
			FilePath: path,
			Kind:     kind,
		})
		return scanPos
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
		cm := currentModule()
		for j := lineStart; j < lineEnd; j++ {
			tok := tokens[j]

			// %Module{ struct literal
			if tok.Kind == TokPercent && j+1 < lineEnd && tokens[j+1].Kind == TokModule && isUserModuleToken(tokens[j+1]) {
				modName, k := collectModuleName(j + 1)
				if k < lineEnd && tokens[k].Kind == TokOpenBrace {
					resolved := ResolveModuleRef(modName, aliases, cm)
					if resolved != "" {
						refs = append(refs, Reference{Module: resolved, Line: tok.Line, FilePath: path, Kind: "call"})
					}
					j = k
					continue
				}
			}

			if tok.Kind != TokModule || !isUserModuleToken(tok) {
				continue
			}

			modName, k := collectModuleName(j)

			// Skip if preceded by % (struct literal already handled above)
			if j > 0 && tokens[j-1].Kind == TokPercent {
				j = k - 1
				continue
			}

			// Module.function call
			if k < lineEnd && tokens[k].Kind == TokDot && k+1 < lineEnd && tokens[k+1].Kind == TokIdent {
				funcName := tokenText(tokens[k+1])
				if !elixirKeyword[funcName] {
					resolved := ResolveModuleRef(modName, aliases, cm)
					if resolved != "" {
						refs = append(refs, Reference{Module: resolved, Function: funcName, Line: tok.Line, FilePath: path, Kind: "call"})
					}
				}
				j = k + 1
				continue
			}

			// Standalone module ref (skip self-references)
			if modName != cm {
				resolved := ResolveModuleRef(modName, aliases, cm)
				if resolved != "" {
					refs = append(refs, Reference{Module: resolved, Line: tok.Line, FilePath: path, Kind: "call"})
				}
			}
			j = k - 1
		}
	}

	// trackLineDepth scans tokens[lineStart:lineEnd] for TokDo/TokFn/TokEnd
	// and updates depth accordingly. TokEnd also checks for module stack pops.
	trackLineDepth := func(lineStart, lineEnd int) {
		for j := lineStart; j < lineEnd; j++ {
			switch tokens[j].Kind {
			case TokDo:
				depth++
			case TokFn:
				depth++
			case TokEnd:
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
			}
		}
	}

	// Main token walker
	i := 0
	for i < n {
		tok := tokens[i]

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
			i++
			continue

		case TokDo:
			depth++
			i++
			continue

		case TokFn:
			// fn always increments depth; its matching TokEnd will decrement it.
			// For inline fn..end on the same line, the depth goes up then back down
			// with net zero effect — matching the old parser's ContainsFn behavior.
			depth++
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
			i++
			j := nextSig(i)
			if j >= n || tokens[j].Kind != TokIdent {
				i = j
				goto extractRefsForLine
			}
			{
				funcName := tokenText(tokens[j])
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
			if cm != "" {
				defs = append(defs, Definition{
					Module:   cm,
					Function: "__struct__",
					Line:     tok.Line,
					FilePath: path,
					Kind:     "defstruct",
				})
			}
			i++
			goto extractRefsForLine

		case TokDefexception:
			cm := currentModule()
			if cm != "" {
				defs = append(defs, Definition{
					Module:   cm,
					Function: "__exception__",
					Line:     tok.Line,
					FilePath: path,
					Kind:     "defexception",
				})
			}
			i++
			goto extractRefsForLine

		case TokAlias:
			aliasLine := tok.Line
			i++
			j := nextSig(i)
			modName, k := collectModuleName(j)
			if modName == "" {
				i = k
				continue
			}
			cm := currentModule()

			// Multi-alias: alias MyApp.{Users, Accounts}
			if k < n && tokens[k].Kind == TokDot && k+1 < n && tokens[k+1].Kind == TokOpenBrace {
				parent := modName
				parentResolved := resolveModule(parent, cm)
				k += 2 // skip dot and open brace
				for k < n && tokens[k].Kind != TokCloseBrace && tokens[k].Kind != TokEOF {
					k = nextSig(k)
					if k >= n || tokens[k].Kind == TokCloseBrace {
						break
					}
					childName, newK := collectModuleName(k)
					if childName != "" {
						fullChild := parentResolved + "." + childName
						aliasKey := childName
						if dot := strings.LastIndexByte(childName, '.'); dot >= 0 {
							aliasKey = childName[dot+1:]
						}
						aliases[aliasKey] = fullChild
						emitModuleRef(fullChild, aliasLine, "alias")
					}
					k = newK
					if k < n && tokens[k].Kind == TokComma {
						k++
					}
				}
				i = k
				if i < n && tokens[i].Kind == TokCloseBrace {
					i++
				}
				continue
			}

			// Alias with as:
			nk := nextSig(k)
			if nk < n && tokens[nk].Kind == TokComma {
				afterComma := nextSig(nk + 1)
				if afterComma < n && tokens[afterComma].Kind == TokIdent && tokenText(tokens[afterComma]) == "as" {
					afterAs := nextSig(afterComma + 1)
					if afterAs < n && tokens[afterAs].Kind == TokColon {
						afterColon := nextSig(afterAs + 1)
						if afterColon < n && (tokens[afterColon].Kind == TokModule || tokens[afterColon].Kind == TokIdent) {
							asName := tokenText(tokens[afterColon])
							resolved := resolveModule(modName, cm)
							if !strings.Contains(resolved, "__MODULE__") {
								aliases[asName] = resolved
								refs = append(refs, Reference{Module: resolved, Line: aliasLine, FilePath: path, Kind: "alias"})
							}
							i = afterColon + 1
							continue
						}
					}
				}
			}

			// Simple alias
			{
				resolved := resolveModule(modName, cm)
				dot := strings.LastIndexByte(resolved, '.')
				var shortName string
				if dot >= 0 {
					shortName = resolved[dot+1:]
				} else {
					shortName = resolved
				}
				aliases[shortName] = resolved
				emitModuleRef(resolved, aliasLine, "alias")
			}
			i = k
			continue

		case TokImport:
			importLine := tok.Line
			i++
			j := nextSig(i)
			modName, k := collectModuleName(j)
			if modName != "" {
				resolved := resolveModule(modName, currentModule())
				if !strings.Contains(resolved, "__MODULE__") {
					refs = append(refs, Reference{Module: resolved, Line: importLine, FilePath: path, Kind: "import"})
					injectors[resolved] = true
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
				resolved := resolveModule(modName, currentModule())
				if !strings.Contains(resolved, "__MODULE__") {
					refs = append(refs, Reference{Module: resolved, Line: useLine, FilePath: path, Kind: "use"})
					injectors[resolved] = true
				}
			}
			i = k
			continue

		case TokRequire:
			i++
			goto extractRefsForLine

		case TokAttrType:
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
				i++
				j := nextSig(i)
				if j < n && tokens[j].Kind == TokIdent {
					name := tokenText(tokens[j])
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
					resolved := resolveModule(modName, cm)
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
			cm := currentModule()
			if cm != "" {
				attrLine := tok.Line
				attrText := tokenText(tok)
				kind := "callback"
				if attrText == "@macrocallback" {
					kind = "macrocallback"
				}
				i++
				j := nextSig(i)
				if j < n && tokens[j].Kind == TokIdent {
					name := tokenText(tokens[j])
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

		case TokAttrDoc, TokAttrSpec, TokAttr:
			i++
			goto extractRefsForLine

		case TokPercent:
			// %Module{ struct literal
			if i+1 < n && tokens[i+1].Kind == TokModule && isUserModuleToken(tokens[i+1]) {
				modName, k := collectModuleName(i + 1)
				if k < n && tokens[k].Kind == TokOpenBrace {
					cm := currentModule()
					resolved := ResolveModuleRef(modName, aliases, cm)
					if resolved != "" {
						refs = append(refs, Reference{Module: resolved, Line: tok.Line, FilePath: path, Kind: "call"})
					}
					i = k + 1
					continue
				}
			}
			i++
			continue

		case TokModule:
			// Skip __MODULE__ and other non-ASCII-uppercase module tokens
			if !isUserModuleToken(tok) {
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
						refs = append(refs, Reference{Module: resolved, Function: funcName, Line: tok.Line, FilePath: path, Kind: "call"})
					}
				}
				i = k + 2
				continue
			}

			// Standalone module ref (skip self-references)
			if modName != cm {
				resolved := ResolveModuleRef(modName, aliases, cm)
				if resolved != "" {
					refs = append(refs, Reference{Module: resolved, Line: tok.Line, FilePath: path, Kind: "call"})
				}
			}
			i = k
			continue

		case TokPipe:
			cm := currentModule()
			if cm != "" && len(injectors) > 0 {
				j := nextSig(i + 1)
				if j < n && tokens[j].Kind == TokIdent {
					name := tokenText(tokens[j])
					if !elixirKeyword[name] {
						for mod := range injectors {
							refs = append(refs, Reference{Module: mod, Function: name, Line: tokens[j].Line, FilePath: path, Kind: "call"})
						}
					}
				}
			}
			i++
			continue

		case TokIdent:
			cm := currentModule()
			if cm != "" && len(injectors) > 0 {
				isStatementStart := i == 0 || tokens[i-1].Kind == TokEOL || tokens[i-1].Kind == TokComment
				if isStatementStart {
					name := tokenText(tok)
					if !elixirKeyword[name] {
						j := i + 1
						if j < n {
							next := tokens[j]
							switch next.Kind {
							case TokDo:
								for mod := range injectors {
									refs = append(refs, Reference{Module: mod, Function: name, Line: tok.Line, FilePath: path, Kind: "call"})
								}
							case TokOpenParen:
								for mod := range injectors {
									refs = append(refs, Reference{Module: mod, Function: name, Line: tok.Line, FilePath: path, Kind: "call"})
								}
							case TokAtom:
								for mod := range injectors {
									refs = append(refs, Reference{Module: mod, Function: name, Line: tok.Line, FilePath: path, Kind: "call"})
								}
							case TokString, TokHeredoc, TokSigil:
								afterStr := nextSig(j + 1)
								if afterStr < n && tokens[afterStr].Kind == TokDo {
									for mod := range injectors {
										refs = append(refs, Reference{Module: mod, Function: name, Line: tok.Line, FilePath: path, Kind: "call"})
									}
								}
							}
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
			if currentModule() != "" && len(injectors) > 0 {
				for j := lineStart; j < lineEnd; j++ {
					if tokens[j].Kind == TokPipe {
						pj := nextSig(j + 1)
						if pj < lineEnd && tokens[pj].Kind == TokIdent {
							name := tokenText(tokens[pj])
							if !elixirKeyword[name] {
								for mod := range injectors {
									refs = append(refs, Reference{Module: mod, Function: name, Line: tokens[pj].Line, FilePath: path, Kind: "call"})
								}
							}
						}
					}
				}
			}

			// Advance past this line
			for i < n && tokens[i].Kind != TokEOL && tokens[i].Kind != TokEOF {
				i++
			}
		}
	}

	return defs, refs, nil
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
