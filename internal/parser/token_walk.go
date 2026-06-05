package parser

import "strings"

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
