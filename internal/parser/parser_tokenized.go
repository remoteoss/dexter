package parser

import (
	"sort"
	"strconv"
	"strings"
)

// parseTextFromTokens is the token-stream replacement for the line-based ParseText.
// It walks a []Token stream from the tokenizer and produces identical Definition
// and Reference output.
func parseTextFromTokens(path string, source []byte, tokens, interp []Token) ([]Definition, []Reference, error) {
	defs, refs, _, err := parseTextFromTokensInternal(path, source, tokens, interp, false)
	return defs, refs, err
}

func parseTextFromTokensWithCalls(path string, source []byte, tokens, interp []Token) ([]Definition, []Reference, []CallEdge, error) {
	return parseTextFromTokensInternal(path, source, tokens, interp, true)
}

func parseTextFromTokensInternal(path string, source []byte, tokens, interp []Token, collectCalls bool) ([]Definition, []Reference, []CallEdge, error) {
	var defs []Definition
	var refs []Reference
	var callSet, localCallSet map[CallEdge]struct{}
	if collectCalls {
		callSet = make(map[CallEdge]struct{})
		localCallSet = make(map[CallEdge]struct{})
	}

	type moduleFrame struct {
		name             string
		depth            int
		savedAliases     map[string]string
		savedCallAliases map[string]string
		savedInjectors   map[string]bool
	}

	var moduleStack []moduleFrame
	type functionFrame struct {
		id                FunctionID
		depth             int
		savedCallAliases  map[string]string
		callAliasesShared bool
	}
	var functionStack []functionFrame
	type callAliasBlockFrame struct {
		depth  int
		saved  map[string]string
		shared bool
	}
	var callAliasBlocks []callAliasBlockFrame
	var inlineFunction *struct {
		id                FunctionID
		start, end        int
		savedCallAliases  map[string]string
		callAliasesShared bool
	}
	depth := 0
	aliases := map[string]string{}
	callAliases := map[string]string{}
	var clauseGuardAliases map[string]string
	injectors := map[string]bool{}

	n := len(tokens)
	typespecStart, typespecEnd := -1, -1

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

	currentFunction := func(pos int) (FunctionID, bool) {
		if inlineFunction != nil && pos >= inlineFunction.start && pos < inlineFunction.end {
			return inlineFunction.id, true
		}
		if len(functionStack) == 0 {
			return FunctionID{}, false
		}
		return functionStack[len(functionStack)-1].id, true
	}

	activeCallAliases := func() map[string]string {
		if clauseGuardAliases != nil {
			return clauseGuardAliases
		}
		return callAliases
	}

	popScopes := func(prevDepth int) {
		if len(callAliasBlocks) > 0 && callAliasBlocks[len(callAliasBlocks)-1].depth == prevDepth {
			callAliases = callAliasBlocks[len(callAliasBlocks)-1].saved
			callAliasBlocks = callAliasBlocks[:len(callAliasBlocks)-1]
		}
		if len(functionStack) > 0 && functionStack[len(functionStack)-1].depth == prevDepth {
			callAliases = functionStack[len(functionStack)-1].savedCallAliases
			functionStack = functionStack[:len(functionStack)-1]
		}
		if len(moduleStack) > 0 && moduleStack[len(moduleStack)-1].depth == prevDepth {
			frame := moduleStack[len(moduleStack)-1]
			moduleStack = moduleStack[:len(moduleStack)-1]
			aliases = frame.savedAliases
			callAliases = frame.savedCallAliases
			injectors = frame.savedInjectors
		}
	}

	ensureCallAliasesScoped := func() {
		if !collectCalls {
			return
		}
		if inlineFunction != nil && inlineFunction.callAliasesShared {
			callAliases = copyMap(callAliases)
			inlineFunction.callAliasesShared = false
			return
		}
		if len(callAliasBlocks) > 0 && callAliasBlocks[len(callAliasBlocks)-1].shared {
			callAliases = copyMap(callAliases)
			callAliasBlocks[len(callAliasBlocks)-1].shared = false
			return
		}
		if len(functionStack) > 0 && functionStack[len(functionStack)-1].callAliasesShared {
			callAliases = copyMap(callAliases)
			functionStack[len(functionStack)-1].callAliasesShared = false
		}
	}

	prevSig := func(from int) int {
		for i := from - 1; i >= 0; i-- {
			if tokens[i].Kind != TokEOL && tokens[i].Kind != TokComment {
				return i
			}
		}
		return -1
	}

	isStabGuard := func(from int) bool {
		brackets := 0
		for j := from; j < n; j++ {
			switch tokens[j].Kind {
			case TokOpenParen, TokOpenBracket, TokOpenBrace, TokOpenAngle:
				brackets++
			case TokCloseParen, TokCloseBracket, TokCloseBrace, TokCloseAngle:
				if brackets > 0 {
					brackets--
				}
			case TokRightArrow:
				if brackets == 0 {
					return true
				}
			case TokLeftArrow, TokDo, TokEnd,
				TokDef, TokDefp, TokDefmacro, TokDefmacrop,
				TokDefguard, TokDefguardp, TokDefdelegate:
				if brackets == 0 {
					return false
				}
			}
		}
		return false
	}

	emitEdge := func(caller FunctionID, callee FunctionID, kind string, local bool) {
		if !collectCalls {
			return
		}
		edge := CallEdge{Caller: caller, Callee: callee, Kind: kind}
		if local {
			localCallSet[edge] = struct{}{}
		} else {
			callSet[edge] = struct{}{}
		}
	}

	emitCapture := func(pos int, caller FunctionID) {
		if pos+3 >= n || tokens[pos].Kind != TokOther || tokenText(tokens[pos]) != "&" {
			return
		}
		start := nextSig(pos + 1)
		local := false
		module := caller.Module
		namePos := start
		if start < n && tokens[start].Kind == TokModule {
			modName, k := collectModuleName(start)
			if k >= n || tokens[k].Kind != TokDot {
				return
			}
			namePos = nextSig(k + 1)
			module = ResolveModuleRef(modName, activeCallAliases(), currentModule())
		} else {
			local = true
		}
		if namePos >= n || tokens[namePos].Kind != TokIdent {
			return
		}
		slash := nextSig(namePos + 1)
		arityPos := nextSig(slash + 1)
		if slash >= n || tokens[slash].Kind != TokOther || tokenText(tokens[slash]) != "/" || arityPos >= n || tokens[arityPos].Kind != TokNumber {
			return
		}
		arity, err := strconv.Atoi(tokenText(tokens[arityPos]))
		if err != nil || module == "" {
			return
		}
		emitEdge(caller, FunctionID{Module: module, Function: tokenText(tokens[namePos]), Arity: arity}, "capture", local)
	}

	emitLocalCall := func(pos int, caller FunctionID) {
		if !collectCalls {
			return
		}
		if pos+1 >= n || tokens[pos].Kind != TokIdent || tokens[pos+1].Kind != TokOpenParen || elixirKeyword[tokenText(tokens[pos])] {
			return
		}
		if prev := prevSig(pos); prev >= 0 && (tokens[prev].Kind == TokDot || tokens[prev].Kind == TokDef || tokens[prev].Kind == TokDefp || tokens[prev].Kind == TokDefmacro || tokens[prev].Kind == TokDefmacrop || tokens[prev].Kind == TokDefguard || tokens[prev].Kind == TokDefguardp || tokens[prev].Kind == TokDefdelegate) {
			return
		}
		arity, ok := ParenthesizedCallArity(tokens, n, pos+1)
		if !ok {
			return
		}
		if prev := prevSig(pos); prev >= 0 && tokens[prev].Kind == TokPipe {
			arity++
		}
		emitEdge(caller, FunctionID{Module: caller.Module, Function: tokenText(tokens[pos]), Arity: arity}, "call", true)
	}

	// processModuleDef handles defmodule/defprotocol/defimpl.
	// It collects the module name, scans forward to consume the TokDo token,
	// increments depth, and pushes the module frame with the post-increment depth.
	// For inline `, do:` modules the definition is still emitted but no frame is
	// pushed (no do..end scope to track).
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

		// Always emit the module definition — even `, do:` one-liners must be
		// tracked so callers can find the module.
		defs = append(defs, Definition{
			Module:   name,
			Line:     kwLine,
			FilePath: path,
			Kind:     kind,
		})

		// Scan forward to find and consume TokDo (skipping "for: Module" etc.).
		// Do not stop at TokEOL — Elixir allows `defmodule Name` then `do` on the
		// next line; stopping at EOL left TokDo to the main loop (double-counting
		// depth) so inner `end` did not pop the inner module frame.
		// Stop at statement-boundary tokens to avoid stealing a later module's TokDo
		// when the current module uses the `, do:` keyword form.
		_, scanPos, hasDo := ScanForwardToBlockDo(tokens, n, j)
		if hasDo {
			depth++
			moduleStack = append(moduleStack, moduleFrame{
				name:             name,
				depth:            depth,
				savedAliases:     copyMap(aliases),
				savedCallAliases: copyMap(callAliases),
				savedInjectors:   copyBoolMap(injectors),
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
	callKind := func(pos int) string {
		if pos >= typespecStart && pos < typespecEnd {
			return "typespec"
		}
		return "call"
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
						if resolved, ok := callAliases[target]; ok {
							delegateTo = resolved
						} else if parts := strings.SplitN(target, ".", 2); len(parts) == 2 {
							if resolved, ok := callAliases[parts[0]]; ok {
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
		refs = collectModuleRefs(source, path, tokens, lineStart, lineEnd, aliases, currentModule(), callKind, nil, refs)
	}

	// flushInterpRefs emits the refs written inside a #{} interpolation. The
	// tokenizer keeps those tokens in their own stream (see TokenResult.Interp),
	// so they are drained here in byte order as the walker passes them, which
	// is what gives them the aliases and the enclosing module in force at that
	// point in the file.
	interpPos := 0
	flushInterpRefs := func(upTo int, caller FunctionID, hasCaller bool) {
		start := interpPos
		for interpPos < len(interp) && interp[interpPos].Start < upTo {
			interpPos++
		}
		if interpPos > start {
			var onCall func(QualifiedCall)
			if collectCalls && hasCaller {
				onCall = func(call QualifiedCall) {
					module := ResolveModuleRef(call.Module, activeCallAliases(), currentModule())
					if module != "" {
						emitEdge(caller, FunctionID{Module: module, Function: call.Function, Arity: UnknownArity}, "call", false)
					}
				}
			}
			refs = collectModuleRefs(source, path, interp, start, interpPos, aliases, currentModule(), callRefKind, onCall, refs)
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
				popScopes(prevDepth)
			}
		}
	}

	// Main token walker
	functionHeadEnd := -1
	suppressBareRefsLine := -1
	i := 0
	for i < n {
		tok := tokens[i]
		inFunctionHead := functionHeadEnd >= 0 && i < functionHeadEnd
		if functionHeadEnd >= 0 && i >= functionHeadEnd {
			functionHeadEnd = -1
			inFunctionHead = false
		}

		if interpPos < len(interp) {
			caller, hasCaller := currentFunction(i)
			if !hasCaller && i > 0 {
				caller, hasCaller = currentFunction(i - 1)
			}
			flushInterpRefs(tok.Start, caller, hasCaller)
		}
		if inlineFunction != nil && i >= inlineFunction.end {
			callAliases = inlineFunction.savedCallAliases
			inlineFunction = nil
		}
		callShaped := tok.Kind == TokIdent && i+1 < n && tokens[i+1].Kind == TokOpenParen
		captureShaped := tok.Kind == TokOther && tok.End == tok.Start+1 && source[tok.Start] == '&'
		if collectCalls && (callShaped || captureShaped) {
			caller, ok := currentFunction(i)
			if ok {
				switch {
				case captureShaped:
					if !inFunctionHead {
						emitCapture(i, caller)
					}
				case callShaped:
					if inFunctionHead {
						break
					}
					emitLocalCall(i, caller)
				}
			}
		}

		switch tok.Kind {
		case TokEOL, TokComment, TokString, TokHeredoc, TokSigil,
			TokCharLiteral, TokAtom, TokNumber, TokOther,
			TokDot, TokColon, TokOpenParen, TokCloseParen,
			TokOpenBracket, TokCloseBracket, TokOpenBrace, TokCloseBrace,
			TokOpenAngle, TokCloseAngle, TokBackslash,
			TokLeftArrow, TokAssoc, TokDoubleColon, TokComma:
			i++
			continue

		case TokEOF:
			i = n
			continue

		case TokEnd:
			prevDepth := depth
			TrackBlockDepth(tok.Kind, &depth)
			popScopes(prevDepth)
			i++
			continue

		case TokDo, TokFn:
			if collectCalls {
				if _, ok := currentFunction(i); ok {
					callAliasBlocks = append(callAliasBlocks, callAliasBlockFrame{
						depth: depth + 1, saved: callAliases, shared: true,
					})
				}
			}
			TrackBlockDepth(tok.Kind, &depth)
			i++
			continue

		case TokRightArrow:
			if len(callAliasBlocks) > 0 {
				frame := &callAliasBlocks[len(callAliasBlocks)-1]
				callAliases = frame.saved
				frame.shared = true
			}
			clauseGuardAliases = nil
			i++
			continue

		case TokWhen:
			if len(callAliasBlocks) > 0 && isStabGuard(i+1) {
				clauseGuardAliases = callAliasBlocks[len(callAliasBlocks)-1].saved
			}
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
			suppressBareRefsLine = defLine
			declarationIdx := i
			funcName, j, ok := StaticDeclarationName(source, tokens, n, declarationIdx)
			if !ok {
				i = j
				goto extractRefsForLine
			}
			{
				nameIdx := j
				j++

				pj := nextSig(j)
				maxArity := 0
				defaultCount := 0
				var paramNames []string
				if pj < n && tokens[pj].Kind == TokOpenParen {
					maxArity, defaultCount, paramNames, pj = collectParamsFromTokens(pj)
					paramNames = fixParamNames(paramNames)
				} else {
					maxArity, defaultCount, paramNames, pj = CollectBareParams(source, tokens, n, pj)
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

				caller := FunctionID{Module: cm, Function: funcName, Arity: maxArity}
				if kind == "defdelegate" && delegateTo != "" {
					targetFunction := delegateAs
					if targetFunction == "" {
						targetFunction = funcName
					}
					emitEdge(caller, FunctionID{Module: delegateTo, Function: targetFunction, Arity: maxArity}, "delegate", false)
				}
				for arity := minArity; arity < maxArity; arity++ {
					emitEdge(
						FunctionID{Module: cm, Function: funcName, Arity: arity},
						caller,
						"default",
						false,
					)
				}
				doIdx, _, hasDo := ScanForwardToBlockDo(tokens, n, pj)
				if hasDo {
					functionHeadEnd = doIdx + 1
					if collectCalls {
						functionStack = append(functionStack, functionFrame{
							id: caller, depth: depth + 1,
							savedCallAliases: callAliases, callAliasesShared: true,
						})
					}
				} else {
					start, end, ok := ScanKeywordDoBody(source, tokens, n, pj)
					if ok {
						functionHeadEnd = start
						if collectCalls {
							inlineFunction = &struct {
								id                FunctionID
								start, end        int
								savedCallAliases  map[string]string
								callAliasesShared bool
							}{
								id: caller, start: start, end: end,
								savedCallAliases: callAliases, callAliasesShared: true,
							}
						}
					}
				}
				i = nameIdx + 1
			}
			continue

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

			// The module name on an alias line is itself resolved through the
			// aliases already in scope: `alias A.B` then `alias B.C`.
			callModName := ExpandAliasPrefix(modName, callAliases)
			modName = ExpandAliasPrefix(modName, aliases)
			ensureCallAliasesScoped()

			// Multi-alias: alias MyApp.{Users, Accounts}
			if children, nextPos, ok := ScanMultiAliasChildren(source, tokens, n, k, false); ok {
				parentResolved := resolveModule(modName, cm)
				callParentResolved := resolveModule(callModName, cm)
				for _, childName := range children {
					fullChild := parentResolved + "." + childName
					aliases[AliasShortName(childName)] = fullChild
					callAliases[AliasShortName(childName)] = callParentResolved + "." + childName
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
					callAliases[asName] = resolveModule(callModName, cm)
					refs = append(refs, Reference{Module: resolved, Line: aliasLine, FilePath: path, Kind: "alias"})
				}
				i = nextPos
				continue
			}

			// Simple alias
			{
				resolved := resolveModule(modName, cm)
				aliases[AliasShortName(resolved)] = resolved
				callResolved := resolveModule(callModName, cm)
				callAliases[AliasShortName(callResolved)] = callResolved
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
				resolved := ResolveModuleRef(modName, aliases, currentModule())
				if resolved != "" {
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
				resolved := ResolveModuleRef(modName, aliases, currentModule())
				if resolved != "" {
					refs = append(refs, Reference{Module: resolved, Line: useLine, FilePath: path, Kind: "use"})
					injectors[resolved] = true
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
					ensureCallAliasesScoped()
					aliases[asName] = resolved
					callAliases[asName] = ResolveModuleRef(modName, callAliases, cm)
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
			userModule := isUserModule(source, tok)
			if !userModule {
				if collectCalls && !inFunctionHead && callKind(i) == "call" {
					if call, ok := QualifiedCallAt(source, tokens, n, i); ok {
						prev := prevSig(i)
						capture := prev >= 0 && tokens[prev].Kind == TokOther && tokenText(tokens[prev]) == "&"
						if !capture {
							if caller, ok := currentFunction(i); ok {
								arity := call.Arity
								if arity != UnknownArity && prev >= 0 && tokens[prev].Kind == TokPipe {
									arity++
								}
								resolved := ResolveModuleRef(call.Module, activeCallAliases(), currentModule())
								if resolved != "" {
									emitEdge(caller, FunctionID{Module: resolved, Function: call.Function, Arity: arity}, "call", false)
								}
							}
						}
					}
				}
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
			if call, ok := QualifiedCallAt(source, tokens, n, i); ok {
				if !elixirKeyword[call.Function] {
					resolved := ResolveModuleRef(call.Module, aliases, cm)
					if resolved != "" {
						refs = append(refs, Reference{Module: resolved, Function: call.Function, Line: tok.Line, FilePath: path, Kind: callKind(i)})
						prev := prevSig(i)
						capture := prev >= 0 && tokens[prev].Kind == TokOther && tokenText(tokens[prev]) == "&"
						if collectCalls && !inFunctionHead && callKind(i) == "call" && !capture {
							if caller, ok := currentFunction(i); ok {
								arity := call.Arity
								if arity != UnknownArity {
									if prev >= 0 && tokens[prev].Kind == TokPipe {
										arity++
									}
								}
								callResolved := ResolveModuleRef(call.Module, activeCallAliases(), cm)
								if callResolved != "" {
									emitEdge(caller, FunctionID{Module: callResolved, Function: call.Function, Arity: arity}, "call", false)
								}
							}
						}
					}
				}
				i = call.NameEnd
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
			cm := currentModule()
			if cm != "" && len(injectors) > 0 {
				j := nextSig(i + 1)
				if j < n && tokens[j].Kind == TokIdent {
					name := tokenText(tokens[j])
					if !elixirKeyword[name] {
						for mod := range injectors {
							refs = append(refs, Reference{Module: mod, Function: name, Line: tokens[j].Line, FilePath: path, Kind: callKind(j)})
						}
					}
				}
			}
			i++
			continue

		case TokIdent:
			text := tokenText(tok)
			blockBranch := text == "else" || text == "after" || text == "rescue" || text == "catch"
			prev := prevSig(i)
			qualifiedIdent := prev >= 0 && tokens[prev].Kind == TokDot
			if blockBranch && !qualifiedIdent && (i+1 >= n || tokens[i+1].Kind != TokColon) && len(callAliasBlocks) > 0 {
				frame := &callAliasBlocks[len(callAliasBlocks)-1]
				callAliases = frame.saved
				frame.shared = true
				clauseGuardAliases = nil
				i++
				continue
			}
			cm := currentModule()
			if tok.Line != suppressBareRefsLine && cm != "" && len(injectors) > 0 {
				isStatementStart := i == 0 || tokens[i-1].Kind == TokEOL || tokens[i-1].Kind == TokComment
				// A parenthesized bare call can be nested inside another call,
				// collection, or keyword value. It is still a candidate for a
				// function imported by use/import, just like a statement-level call.
				// Qualified and anonymous-function calls have a TokDot before the
				// opening paren and therefore do not match this fast check.
				isParenthesizedCall := i+1 < n && tokens[i+1].Kind == TokOpenParen
				if isStatementStart || isParenthesizedCall {
					name := text
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
										TokCharLiteral, TokNumber, TokOpenBracket, TokOpenBrace,
										TokPercent:
										emit = true
									}
								}
							}
						}
						if emit {
							for mod := range injectors {
								refs = append(refs, Reference{Module: mod, Function: name, Line: tok.Line, FilePath: path, Kind: callKind(i)})
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
									refs = append(refs, Reference{Module: mod, Function: name, Line: tokens[pj].Line, FilePath: path, Kind: callKind(pj)})
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

	if interpPos < len(interp) {
		flushInterpRefs(len(source)+1, FunctionID{}, false)
	}

	if !collectCalls {
		return defs, dedupeRefs(refs), nil, nil
	}

	known := make(map[FunctionID]struct{}, len(defs))
	for _, def := range defs {
		if def.Function != "" {
			known[FunctionID{Module: def.Module, Function: def.Function, Arity: def.Arity}] = struct{}{}
		}
	}
	for edge := range localCallSet {
		if _, ok := known[edge.Callee]; ok {
			callSet[edge] = struct{}{}
		}
	}
	edges := make([]CallEdge, 0, len(callSet))
	for edge := range callSet {
		edges = append(edges, edge)
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Caller.Module != b.Caller.Module {
			return a.Caller.Module < b.Caller.Module
		}
		if a.Caller.Function != b.Caller.Function {
			return a.Caller.Function < b.Caller.Function
		}
		if a.Caller.Arity != b.Caller.Arity {
			return a.Caller.Arity < b.Caller.Arity
		}
		if a.Callee.Module != b.Callee.Module {
			return a.Callee.Module < b.Callee.Module
		}
		if a.Callee.Function != b.Callee.Function {
			return a.Callee.Function < b.Callee.Function
		}
		if a.Callee.Arity != b.Callee.Arity {
			return a.Callee.Arity < b.Callee.Arity
		}
		return a.Kind < b.Kind
	})
	return defs, dedupeRefs(refs), edges, nil
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
func collectModuleRefs(source []byte, path string, toks []Token, from, to int, aliases map[string]string, cm string, kindAt func(int) string, onCall func(QualifiedCall), refs []Reference) []Reference {
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

		if tok.Kind != TokModule {
			continue
		}
		if !isUserModule(source, tok) {
			if onCall != nil && isCallableModuleToken(source, tok) {
				if call, ok := QualifiedCallAt(source, toks, tn, j); ok && call.NameEnd <= to {
					onCall(call)
					j = call.NameEnd - 1
				}
			}
			continue
		}

		modName, k := CollectModuleName(source, toks, tn, j)

		// Skip if preceded by % (struct literal already handled above)
		if j > 0 && toks[j-1].Kind == TokPercent {
			j = k - 1
			continue
		}

		// Module.function call
		if call, ok := QualifiedCallAt(source, toks, tn, j); ok && call.NameEnd <= to {
			if !elixirKeyword[call.Function] {
				if resolved := ResolveModuleRef(call.Module, aliases, cm); resolved != "" {
					refs = append(refs, Reference{Module: resolved, Function: call.Function, Line: tok.Line, FilePath: path, Kind: kindAt(j)})
				}
			}
			if onCall != nil && kindAt(j) == "call" {
				onCall(call)
			}
			j = call.NameEnd - 1
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

func isCallableModuleToken(source []byte, t Token) bool {
	return isUserModule(source, t) || TokenText(source, t) == "__MODULE__"
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
