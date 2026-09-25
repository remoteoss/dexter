package parser

import "strings"

// QualifiedCall is the syntactic part of a Module.function call. Module is not
// alias-resolved; callers apply the scope that is active at the token position.
type QualifiedCall struct {
	Module   string
	Function string
	Arity    int
	NameEnd  int
}

// QualifiedCallAt recognizes the same qualified expression used by reference
// extraction and call-graph extraction. Calls without parentheses retain an
// unknown arity instead of guessing from expression boundaries.
func QualifiedCallAt(source []byte, tokens []Token, n, pos int) (QualifiedCall, bool) {
	if pos < 0 || pos >= n || tokens[pos].Kind != TokModule || !isCallableModuleToken(source, tokens[pos]) {
		return QualifiedCall{}, false
	}
	module, k := CollectModuleName(source, tokens, n, pos)
	if k+1 >= n || tokens[k].Kind != TokDot || tokens[k+1].Kind != TokIdent {
		return QualifiedCall{}, false
	}
	call := QualifiedCall{
		Module:   module,
		Function: TokenText(source, tokens[k+1]),
		Arity:    UnknownArity,
		NameEnd:  k + 2,
	}
	if call.NameEnd < n && tokens[call.NameEnd].Kind == TokOpenParen {
		arity, ok := ParenthesizedCallArity(tokens, n, call.NameEnd)
		if ok {
			call.Arity = arity
		}
	}
	return call, true
}

// ParenthesizedCallArity counts top-level arguments without evaluating them.
func ParenthesizedCallArity(tokens []Token, n, open int) (int, bool) {
	if open < 0 || open >= n || tokens[open].Kind != TokOpenParen {
		return 0, false
	}
	brackets := 1
	blocks := 0
	args := 0
	hasValue := false
	for i := open + 1; i < n; i++ {
		switch tokens[i].Kind {
		case TokOpenParen, TokOpenBracket, TokOpenBrace, TokOpenAngle:
			brackets++
			hasValue = true
		case TokCloseParen:
			brackets--
			if brackets == 0 {
				if hasValue {
					args++
				}
				return args, true
			}
		case TokCloseBracket, TokCloseBrace, TokCloseAngle:
			if brackets > 1 {
				brackets--
			}
		case TokDo, TokFn:
			blocks++
			hasValue = true
		case TokEnd:
			if blocks > 0 {
				blocks--
			}
		case TokComma:
			if brackets == 1 && blocks == 0 {
				args++
				hasValue = false
			}
		case TokEOL, TokComment:
		default:
			hasValue = true
		}
	}
	return 0, false
}

// ScanKeywordDoBody finds a `do:` body in a definition head and returns the
// token range of its expression. It does not treat physical lines as scopes;
// bracketed, block, and piped expressions can continue across lines.
func ScanKeywordDoBody(source []byte, tokens []Token, n, from int) (start, end int, ok bool) {
	doPos := -1
	for i := from; i+1 < n; i++ {
		if IsStatementBoundaryToken(tokens[i].Kind) {
			return 0, 0, false
		}
		if tokens[i].Kind == TokIdent && TokenText(source, tokens[i]) == "do" && tokens[i+1].Kind == TokColon {
			doPos = i
			break
		}
	}
	if doPos < 0 {
		return 0, 0, false
	}
	start = doPos + 2
	depth := 0
	blocks := 0
	seen := false
	lastSig := -1
	for i := start; i < n; i++ {
		tok := tokens[i]
		switch tok.Kind {
		case TokOpenParen, TokOpenBracket, TokOpenBrace, TokOpenAngle:
			depth++
			seen = true
			lastSig = i
		case TokCloseParen, TokCloseBracket, TokCloseBrace, TokCloseAngle:
			if depth > 0 {
				depth--
			}
			seen = true
			lastSig = i
		case TokDo, TokFn:
			blocks++
			seen = true
			lastSig = i
		case TokEnd:
			if blocks > 0 {
				blocks--
				seen = true
				lastSig = i
				continue
			}
			if depth == 0 {
				return start, i, seen
			}
		case TokEOL:
			if !seen || depth > 0 || blocks > 0 {
				continue
			}
			next := NextSigToken(tokens, n, i+1)
			if (next < n && (tokens[next].Kind == TokPipe || tokens[next].Kind == TokDot)) || expressionContinuesAfter(source, tokens, lastSig) {
				continue
			}
			return start, i, true
		case TokComment:
			continue
		case TokEOF:
			return start, i, seen
		case TokOther:
			if depth == 0 && blocks == 0 && TokenText(source, tok) == ";" {
				return start, i, seen
			}
			seen = true
			lastSig = i
		default:
			seen = true
			lastSig = i
		}
	}
	return start, n, seen
}

func expressionContinuesAfter(source []byte, tokens []Token, pos int) bool {
	if pos < 0 || pos >= len(tokens) {
		return false
	}
	switch tokens[pos].Kind {
	case TokComma, TokPipe, TokDot, TokBackslash, TokRightArrow, TokLeftArrow, TokAssoc:
		return true
	case TokOther:
		return TokenText(source, tokens[pos]) != ";"
	}
	return false
}

// StaticDeclarationName returns the literal name declared by a function,
// type, spec, or callback token. Macro-generated declaration heads use
// unquote/unquote_splicing as placeholders rather than literal names; those
// return ok=false so every token-based consumer treats them consistently.
func StaticDeclarationName(source []byte, tokens []Token, n, declarationIdx int) (name string, nameIdx int, ok bool) {
	if declarationIdx < 0 || declarationIdx >= n {
		return "", n, false
	}

	nameIdx = NextSigToken(tokens, n, declarationIdx+1)
	if nameIdx >= n {
		return "", nameIdx, false
	}

	switch tokens[declarationIdx].Kind {
	case TokDef, TokDefp, TokDefmacro, TokDefmacrop, TokDefguard, TokDefguardp, TokDefdelegate:
		if !isValidFuncNameToken(tokens[nameIdx].Kind) {
			return "", nameIdx, false
		}
	case TokAttrType, TokAttrSpec, TokAttrCallback:
		if tokens[nameIdx].Kind != TokIdent {
			return "", nameIdx, false
		}
	default:
		return "", nameIdx, false
	}

	name = TokenText(source, tokens[nameIdx])
	if name == "unquote" || name == "unquote_splicing" {
		return "", nameIdx, false
	}
	return name, nameIdx, true
}

// CollectBareParams reads a function head whose parameters are not wrapped in
// parentheses. It stops at a guard, block do, or keyword do and returns the
// same arity information as CollectParams.
func CollectBareParams(source []byte, tokens []Token, n, from int) (arity, defaults int, names []string, end int) {
	depth := 0
	hasParam := false
	hasDefault := false
	paramName := ""
	finishParam := func() {
		if !hasParam {
			return
		}
		arity++
		if hasDefault {
			defaults++
		}
		names = append(names, paramName)
		hasParam = false
		hasDefault = false
		paramName = ""
	}

	for i := from; i < n; i++ {
		tok := tokens[i]
		if depth == 0 {
			switch tok.Kind {
			case TokDo, TokWhen, TokEOF, TokEnd:
				finishParam()
				return arity, defaults, names, i
			case TokComma:
				finishParam()
				continue
			case TokIdent:
				if i+1 < n && tokens[i+1].Kind == TokColon {
					keyword := TokenText(source, tok)
					if keyword == "do" || keyword == "to" || keyword == "as" {
						finishParam()
						return arity, defaults, names, i
					}
				}
			}
			if i > from && IsStatementBoundaryToken(tok.Kind) {
				finishParam()
				return arity, defaults, names, i
			}
		}

		switch tok.Kind {
		case TokOpenParen, TokOpenBracket, TokOpenBrace, TokOpenAngle:
			depth++
			hasParam = true
		case TokCloseParen, TokCloseBracket, TokCloseBrace, TokCloseAngle:
			if depth > 0 {
				depth--
			}
			hasParam = true
		case TokBackslash:
			if depth == 0 {
				hasDefault = true
			}
			hasParam = true
		case TokEOL, TokComment:
			continue
		default:
			if depth == 0 && paramName == "" && tok.Kind == TokIdent {
				paramName = TokenText(source, tok)
			}
			hasParam = true
		}
	}
	finishParam()
	return arity, defaults, names, n
}

// IsStatementBoundaryToken reports whether kind starts a new statement or closes
// the current one, so forward scans should stop before consuming later syntax.
func IsStatementBoundaryToken(kind TokenKind) bool {
	switch kind {
	case TokEOF, TokEnd,
		TokDefmodule, TokDefprotocol, TokDefimpl,
		TokDef, TokDefp, TokDefmacro, TokDefmacrop,
		TokDefguard, TokDefguardp, TokDefdelegate,
		TokAttrType, TokAttrCallback:
		return true
	}
	return false
}

// ScanTypespecEnd returns the first token outside the typespec that starts at
// start. Typespecs commonly span lines inside brackets, after :: or a comma,
// and before a trailing `when`; an ordinary newline at depth zero ends them.
// Keeping this scanner here lets index parsing and open-document lookups use
// exactly the same boundary rules.
func ScanTypespecEnd(source []byte, tokens []Token, n, start int) int {
	depth := 0
	lastSig := start
	for i := start + 1; i < n; i++ {
		tok := tokens[i]
		switch tok.Kind {
		case TokOpenParen, TokOpenBracket, TokOpenBrace, TokOpenAngle:
			depth++
			lastSig = i
		case TokCloseParen, TokCloseBracket, TokCloseBrace, TokCloseAngle:
			if depth > 0 {
				depth--
			}
			lastSig = i
		case TokComment:
			continue
		case TokEOL:
			if depth > 0 {
				continue
			}
			next := NextSigToken(tokens, n, i+1)
			if typespecContinuesAtLineBreak(source, tokens, n, lastSig, next) {
				continue
			}
			return i
		case TokEOF:
			return i
		default:
			if depth == 0 {
				if tok.Kind == TokOther && TokenText(source, tok) == ";" {
					return i
				}
				if i > start+1 && IsStatementBoundaryToken(tok.Kind) {
					return i
				}
			}
			lastSig = i
		}
	}
	return n
}

func typespecContinuesAtLineBreak(source []byte, tokens []Token, n, last, next int) bool {
	if next < n {
		switch tokens[next].Kind {
		case TokWhen:
			return true
		case TokOther:
			// A leading union operator continues `@type t :: A.t()\n | B.t()`.
			if TokenText(source, tokens[next]) == "|" {
				return true
			}
		}
	}
	if last < 0 || last >= n {
		return false
	}
	switch tokens[last].Kind {
	case TokDoubleColon, TokComma, TokWhen, TokRightArrow, TokLeftArrow,
		TokAssoc, TokPipe, TokDot, TokBackslash, TokColon:
		return true
	case TokOther:
		return TokenText(source, tokens[last]) != ";"
	}
	return false
}

// ScanForwardToBlockDo scans tokens[from:] for a block-opening TokDo.
// It does not stop at EOL because Elixir allows split-line heads with `do`
// on the next line. It stops at statement-boundary tokens so malformed or
// inline `, do:` forms do not steal a later construct's block opener.
func ScanForwardToBlockDo(tokens []Token, n, from int) (doIdx, nextPos int, hasDo bool) {
	for j := from; j < n; j++ {
		switch tokens[j].Kind {
		case TokDo:
			return j, j + 1, true
		default:
			if IsStatementBoundaryToken(tokens[j].Kind) {
				return -1, j, false
			}
		}
	}
	return -1, n, false
}

// ScanForwardToMacroCallBlockDo reports whether a block-opening `do` follows a
// bare macro call head starting at `from` (the token just after the macro name).
//
// Unlike ScanForwardToBlockDo, it does not blindly scan to the next statement
// keyword. A bare macro call's `do` belongs to the same logical statement, so we
// track bracket depth — a `do` nested inside parens/brackets/braces opens a
// nested construct's block, not the macro's — and we treat an end-of-line at
// bracket depth zero as a statement separator: once one is seen, any token other
// than `do` begins a new statement and the scan stops. This prevents an
// assignment or plain function call (`changeset = build_changeset(...)`) from
// being mistaken for a macro-with-do-block just because a later statement on a
// following line happens to open a `do`.
//
// A line that ends in a comma at bracket depth zero is an exception: a dangling
// comma is never a valid statement terminator in Elixir, so it marks a multi-line
// keyword-argument head (`test "x",\n  async: true do`) and the scan continues.
func ScanForwardToMacroCallBlockDo(tokens []Token, n, from int) (doIdx, nextPos int, hasDo bool) {
	scanDepth := 0
	seenEOLAtZero := false
	lastSigKind := TokEOL
	for k := from; k < n; k++ {
		switch tokens[k].Kind {
		case TokDo:
			if scanDepth == 0 {
				return k, k + 1, true
			}
			lastSigKind = TokDo
		case TokEOL, TokComment:
			// A trailing comma means the head continues on the next line, so this
			// is not a statement boundary. Comments do not reset lastSigKind.
			if scanDepth == 0 && lastSigKind != TokComma {
				seenEOLAtZero = true
			}
		case TokOpenParen, TokOpenBracket, TokOpenBrace:
			scanDepth++
			seenEOLAtZero = false
			lastSigKind = tokens[k].Kind
		case TokCloseParen, TokCloseBracket, TokCloseBrace:
			scanDepth--
			lastSigKind = tokens[k].Kind
		case TokEOF:
			return -1, k, false
		default:
			// At depth 0, after an end-of-line, any non-do token starts a new statement.
			if scanDepth == 0 && seenEOLAtZero {
				return -1, k, false
			}
			lastSigKind = tokens[k].Kind
		}
	}
	return -1, n, false
}

// TrackBlockDepth updates the block depth counter for do/fn/end tokens.
func TrackBlockDepth(kind TokenKind, depth *int) {
	switch kind {
	case TokDo, TokFn:
		*depth += 1
	case TokEnd:
		if *depth > 0 {
			*depth -= 1
		}
	}
}

// AliasShortName returns the alias key for a module path.
func AliasShortName(name string) string {
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		return name[dot+1:]
	}
	return name
}

// ScanKeywordOptionValue scans for `key: Value` immediately after the token at
// from (typically the position after a parsed module expression) and returns the
// Value token text when present. nextPos points one past the Value token.
func ScanKeywordOptionValue(source []byte, tokens []Token, n, from int, key string) (value string, nextPos int, ok bool) {
	nk := NextSigToken(tokens, n, from)
	if nk >= n || tokens[nk].Kind != TokComma {
		return "", from, false
	}
	afterComma := NextSigToken(tokens, n, nk+1)
	if afterComma >= n || tokens[afterComma].Kind != TokIdent || TokenText(source, tokens[afterComma]) != key {
		return "", from, false
	}
	afterKey := NextSigToken(tokens, n, afterComma+1)
	if afterKey >= n || tokens[afterKey].Kind != TokColon {
		return "", from, false
	}
	afterColon := NextSigToken(tokens, n, afterKey+1)
	if afterColon >= n {
		return "", from, false
	}
	if tokens[afterColon].Kind != TokModule && tokens[afterColon].Kind != TokIdent {
		return "", from, false
	}
	return TokenText(source, tokens[afterColon]), afterColon + 1, true
}

// ScanMultiAliasChildren collects child module names from `alias Parent.{A, B}`.
// It expects `from` to point at the token after the parent module expression.
// When stopAtStatement is true, it aborts on statement keywords inside the brace
// body so malformed input does not swallow later declarations.
func ScanMultiAliasChildren(source []byte, tokens []Token, n, from int, stopAtStatement bool) (children []string, nextPos int, ok bool) {
	if from >= n || tokens[from].Kind != TokDot || from+1 >= n || tokens[from+1].Kind != TokOpenBrace {
		return nil, from, false
	}
	k := from + 2
	for k < n && tokens[k].Kind != TokCloseBrace && tokens[k].Kind != TokEOF {
		k = NextSigToken(tokens, n, k)
		if k >= n || tokens[k].Kind == TokCloseBrace {
			break
		}
		if stopAtStatement {
			switch tokens[k].Kind {
			case TokDef, TokDefp, TokDefmacro, TokDefmacrop,
				TokDefmodule, TokEnd, TokImport, TokUse, TokAlias:
				return children, k, true
			}
		}
		child, nk := CollectModuleName(source, tokens, n, k)
		if child != "" {
			children = append(children, child)
		}
		if nk == k {
			k++
		} else {
			k = nk
		}
		if k < n && tokens[k].Kind == TokComma {
			k++
		}
	}
	if k < n && tokens[k].Kind == TokCloseBrace {
		k++
	}
	return children, k, true
}
