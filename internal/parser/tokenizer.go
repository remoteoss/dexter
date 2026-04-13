package parser

import (
	"unicode"
	"unicode/utf8"
)

// TokenKind identifies the kind of a lexed token.
type TokenKind byte

const (
	TokDefmodule     TokenKind = iota // defmodule
	TokDef                            // def
	TokDefp                           // defp
	TokDefmacro                       // defmacro
	TokDefmacrop                      // defmacrop
	TokDefguard                       // defguard
	TokDefguardp                      // defguardp
	TokDefdelegate                    // defdelegate
	TokDefprotocol                    // defprotocol
	TokDefimpl                        // defimpl
	TokDefstruct                      // defstruct
	TokDefexception                   // defexception
	TokAlias                          // alias
	TokImport                         // import
	TokUse                            // use
	TokRequire                        // require
	TokDo                             // do
	TokEnd                            // end
	TokFn                             // fn
	TokWhen                           // when
	TokIdent                          // lowercase identifier or _ prefixed
	TokModule                         // uppercase-starting identifier segment
	TokAttr                           // @identifier (general)
	TokAttrDoc                        // @doc, @moduledoc
	TokAttrSpec                       // @spec
	TokAttrType                       // @type, @typep, @opaque
	TokAttrBehaviour                  // @behaviour
	TokAttrCallback                   // @callback, @macrocallback
	TokString                         // "..." or '...' (content blanked)
	TokHeredoc                        // """...""" or '''...''' (content blanked)
	TokSigil                          // ~X... (content blanked)
	TokCharLiteral                    // ?x or ?\n etc.
	TokAtom                           // :foo or :"..." (colon-prefixed)
	TokDot                            // .
	TokComma                          // ,
	TokColon                          // : (keyword separator, not atom prefix)
	TokOpenParen                      // (
	TokCloseParen                     // )
	TokOpenBracket                    // [
	TokCloseBracket                   // ]
	TokOpenBrace                      // {
	TokCloseBrace                     // }
	TokOpenAngle                      // <<
	TokCloseAngle                     // >>
	TokPipe                           // |>
	TokBackslash                      // \\
	TokRightArrow                     // ->
	TokLeftArrow                      // <-
	TokAssoc                          // =>
	TokDoubleColon                    // ::
	TokPercent                        // %
	TokNumber                         // integer or float literal
	TokComment                        // # to end of line
	TokEOL                            // newline
	TokEOF                            // end of input
	TokOther                          // anything else (operators, etc.)
)

// Token is a lexed token from an Elixir source file.
// Start and End are byte offsets into the source; source[Start:End] is the token text.
// Line is 1-based.
type Token struct {
	Kind  TokenKind
	Start int
	End   int
	Line  int
}

// TokenResult holds the output of Tokenize: the token stream and a line-starts
// table for O(1) byte-offset-to-column conversion. LineStarts[i] is the byte
// offset of the first character on line i+1 (0-indexed). Column for a token
// on line L at byte offset B is: B - LineStarts[L-1].
type TokenResult struct {
	Tokens     []Token
	LineStarts []int
}

// keywordKinds maps keyword strings to their token kind.
// Checked after lexing a lowercase identifier.
var keywordKinds = map[string]TokenKind{
	"defmodule":    TokDefmodule,
	"defprotocol":  TokDefprotocol,
	"defimpl":      TokDefimpl,
	"defstruct":    TokDefstruct,
	"defexception": TokDefexception,
	"defdelegate":  TokDefdelegate,
	"defmacrop":    TokDefmacrop,
	"defmacro":     TokDefmacro,
	"defguardp":    TokDefguardp,
	"defguard":     TokDefguard,
	"defp":         TokDefp,
	"def":          TokDef,
	"alias":        TokAlias,
	"import":       TokImport,
	"use":          TokUse,
	"require":      TokRequire,
	"do":           TokDo,
	"end":          TokEnd,
	"fn":           TokFn,
	"when":         TokWhen,
}

// operatorAtomChars are the characters that can form operator atoms like :+, :&&, :>>>.
// Elixir allows these after a bare : to form atoms.
var operatorAtomChars = [256]bool{
	'+': true, '-': true, '*': true, '/': true,
	'=': true, '!': true, '<': true, '>': true,
	'|': true, '&': true, '^': true, '~': true,
	'@': true, '\\': true,
}

func Tokenize(source []byte) []Token {
	return TokenizeFull(source).Tokens
}

func TokenizeFull(source []byte) TokenResult {
	tokens := make([]Token, 0, len(source)/8)
	lineStarts := make([]int, 1, 64)
	lineStarts[0] = 0 // line 1 starts at byte 0
	line := 1
	i := 0
	afterDot := false // true when the last significant token was TokDot

	for i < len(source) {
		ch := source[i]

		// Whitespace, newlines, and comments don't affect afterDot — they preserve it.
		// Everything else clears it (except the dot case which sets it).
		switch {
		case ch == '\n':
			tokens = append(tokens, Token{Kind: TokEOL, Start: i, End: i + 1, Line: line})
			line++
			i++
			lineStarts = append(lineStarts, i)
			continue

		case ch == ' ' || ch == '\t' || ch == '\r':
			i++
			continue

		case ch == '#':
			start := i
			for i < len(source) && source[i] != '\n' {
				i++
			}
			tokens = append(tokens, Token{Kind: TokComment, Start: start, End: i, Line: line})
			continue

		case ch == '?':
			start := i
			startLine := line
			i++ // consume '?'
			if i < len(source) {
				if source[i] == '\\' {
					i++ // consume backslash
					if i < len(source) {
						if source[i] == 'x' || source[i] == 'X' {
							// hex escape: \xFF
							i++
							for i < len(source) && isHexDigit(source[i]) {
								i++
							}
						} else if source[i] >= '0' && source[i] <= '7' {
							// octal escape
							i++
							for i < len(source) && source[i] >= '0' && source[i] <= '7' {
								i++
							}
						} else {
							i++ // single char escape like \n \t \\
						}
					}
				} else {
					i++ // any other single char
				}
			}
			tokens = append(tokens, Token{Kind: TokCharLiteral, Start: start, End: i, Line: startLine})

		case ch == '"':
			// Check for heredoc
			if i+2 < len(source) && source[i+1] == '"' && source[i+2] == '"' {
				start := i
				startLine := line
				i += 3 // consume opening """
				// scan to closing """ on its own line
				i, line = scanHeredocContent(source, i, line, '"')
				tokens = append(tokens, Token{Kind: TokHeredoc, Start: start, End: i, Line: startLine})
			} else {
				start := i
				startLine := line
				i++ // consume opening "
				i, line = scanStringContent(source, i, line, '"')
				tokens = append(tokens, Token{Kind: TokString, Start: start, End: i, Line: startLine})
			}

		case ch == '\'':
			// Check for heredoc
			if i+2 < len(source) && source[i+1] == '\'' && source[i+2] == '\'' {
				start := i
				startLine := line
				i += 3 // consume opening '''
				i, line = scanHeredocContent(source, i, line, '\'')
				tokens = append(tokens, Token{Kind: TokHeredoc, Start: start, End: i, Line: startLine})
			} else {
				start := i
				startLine := line
				i++ // consume opening '
				i, line = scanStringContent(source, i, line, '\'')
				tokens = append(tokens, Token{Kind: TokString, Start: start, End: i, Line: startLine})
			}

		case ch == '~':
			// Sigil: ~ followed by letter(s) then delimiter.
			// Single-char sigils: ~r, ~s, ~S, etc.
			// Multi-char sigils (Elixir 1.15+): ~HTML, ~HEEX — uppercase only.
			if i+1 < len(source) && isLetter(source[i+1]) {
				start := i
				startLine := line
				sigilLetter := source[i+1]
				i += 2 // consume ~ and first letter
				// Multi-char sigils: continue reading uppercase letters
				if isUpper(sigilLetter) {
					for i < len(source) && isUpper(source[i]) {
						i++
					}
				}
				if i < len(source) {
					i, line = scanSigilContent(source, i, line, sigilLetter)
				}
				tokens = append(tokens, Token{Kind: TokSigil, Start: start, End: i, Line: startLine})
			} else {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 1, Line: line})
				i++
			}

		case ch == ':':
			if i+1 < len(source) && source[i+1] == ':' {
				tokens = append(tokens, Token{Kind: TokDoubleColon, Start: i, End: i + 2, Line: line})
				i += 2
			} else if i+1 < len(source) && source[i+1] == '"' {
				// Atom with quoted string: :"..."
				start := i
				startLine := line
				i += 2 // consume :"
				i, line = scanStringContent(source, i, line, '"')
				tokens = append(tokens, Token{Kind: TokAtom, Start: start, End: i, Line: startLine})
			} else if i+1 < len(source) && source[i+1] == '\'' {
				// Atom with quoted charlist: :'...'
				start := i
				startLine := line
				i += 2 // consume :'
				i, line = scanStringContent(source, i, line, '\'')
				tokens = append(tokens, Token{Kind: TokAtom, Start: start, End: i, Line: startLine})
			} else if i+1 < len(source) && (isLower(source[i+1]) || source[i+1] == '_' || isUpperAtomStart(source, i+1)) {
				start := i
				i++ // consume ':'
				i = scanIdentContinue(source, i)
				tokens = append(tokens, Token{Kind: TokAtom, Start: start, End: i, Line: line})
			} else if i+1 < len(source) && source[i+1] >= 0x80 {
				r, size := utf8.DecodeRune(source[i+1:])
				if r != utf8.RuneError && unicode.IsLetter(r) {
					start := i
					i++
					i += size
					i = scanIdentContinue(source, i)
					tokens = append(tokens, Token{Kind: TokAtom, Start: start, End: i, Line: line})
				} else {
					tokens = append(tokens, Token{Kind: TokColon, Start: i, End: i + 1, Line: line})
					i++
				}
			} else if i+1 < len(source) && operatorAtomChars[source[i+1]] {
				// Operator atom: :+, :-, :&&, :>>>, etc.
				start := i
				i++ // consume ':'
				for i < len(source) && operatorAtomChars[source[i]] {
					i++
				}
				tokens = append(tokens, Token{Kind: TokAtom, Start: start, End: i, Line: line})
			} else {
				tokens = append(tokens, Token{Kind: TokColon, Start: i, End: i + 1, Line: line})
				i++
			}

		case ch == '@':
			if i+1 < len(source) && (isLower(source[i+1]) || source[i+1] == '_') {
				start := i
				i++ // consume '@'
				i = scanIdentContinue(source, i)
				tokens = append(tokens, Token{Kind: classifyAttr(source, start, i), Start: start, End: i, Line: line})
			} else if i+1 < len(source) && source[i+1] >= 0x80 {
				// Check for Unicode lowercase letter after @
				r, _ := utf8.DecodeRune(source[i+1:])
				if r != utf8.RuneError && unicode.IsLetter(r) && unicode.IsLower(r) {
					start := i
					i++ // consume '@'
					i = scanIdentContinue(source, i)
					tokens = append(tokens, Token{Kind: classifyAttr(source, start, i), Start: start, End: i, Line: line})
				} else {
					tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 1, Line: line})
					i++
				}
			} else {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 1, Line: line})
				i++
			}

		case ch == '.':
			if i+2 < len(source) && source[i+1] == '.' && source[i+2] == '.' {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 3, Line: line})
				i += 3
			} else if i+1 < len(source) && source[i+1] == '.' {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 2, Line: line})
				i += 2
			} else {
				tokens = append(tokens, Token{Kind: TokDot, Start: i, End: i + 1, Line: line})
				i++
				afterDot = true
				continue
			}

		case ch == ',':
			tokens = append(tokens, Token{Kind: TokComma, Start: i, End: i + 1, Line: line})
			i++

		case ch == '(':
			tokens = append(tokens, Token{Kind: TokOpenParen, Start: i, End: i + 1, Line: line})
			i++

		case ch == ')':
			tokens = append(tokens, Token{Kind: TokCloseParen, Start: i, End: i + 1, Line: line})
			i++

		case ch == '[':
			tokens = append(tokens, Token{Kind: TokOpenBracket, Start: i, End: i + 1, Line: line})
			i++

		case ch == ']':
			tokens = append(tokens, Token{Kind: TokCloseBracket, Start: i, End: i + 1, Line: line})
			i++

		case ch == '{':
			tokens = append(tokens, Token{Kind: TokOpenBrace, Start: i, End: i + 1, Line: line})
			i++

		case ch == '}':
			tokens = append(tokens, Token{Kind: TokCloseBrace, Start: i, End: i + 1, Line: line})
			i++

		case ch == '<':
			if i+1 < len(source) && source[i+1] == '<' {
				tokens = append(tokens, Token{Kind: TokOpenAngle, Start: i, End: i + 2, Line: line})
				i += 2
			} else if i+1 < len(source) && source[i+1] == '-' {
				tokens = append(tokens, Token{Kind: TokLeftArrow, Start: i, End: i + 2, Line: line})
				i += 2
			} else {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 1, Line: line})
				i++
			}

		case ch == '>':
			if i+1 < len(source) && source[i+1] == '>' {
				tokens = append(tokens, Token{Kind: TokCloseAngle, Start: i, End: i + 2, Line: line})
				i += 2
			} else {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 1, Line: line})
				i++
			}

		case ch == '|':
			if i+1 < len(source) && source[i+1] == '>' {
				tokens = append(tokens, Token{Kind: TokPipe, Start: i, End: i + 2, Line: line})
				i += 2
			} else {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 1, Line: line})
				i++
			}

		case ch == '\\':
			if i+1 < len(source) && source[i+1] == '\\' {
				tokens = append(tokens, Token{Kind: TokBackslash, Start: i, End: i + 2, Line: line})
				i += 2
			} else {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 1, Line: line})
				i++
			}

		case ch == '-':
			if i+1 < len(source) && source[i+1] == '>' {
				tokens = append(tokens, Token{Kind: TokRightArrow, Start: i, End: i + 2, Line: line})
				i += 2
			} else {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 1, Line: line})
				i++
			}

		case ch == '=':
			if i+1 < len(source) && source[i+1] == '>' {
				tokens = append(tokens, Token{Kind: TokAssoc, Start: i, End: i + 2, Line: line})
				i += 2
			} else {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 1, Line: line})
				i++
			}

		case ch == '%':
			tokens = append(tokens, Token{Kind: TokPercent, Start: i, End: i + 1, Line: line})
			i++

		case ch >= '0' && ch <= '9':
			start := i
			i++
			// Hex: 0x, Octal: 0o, Binary: 0b
			if ch == '0' && i < len(source) {
				switch source[i] {
				case 'x', 'X':
					i++
					for i < len(source) && (isHexDigit(source[i]) || source[i] == '_') {
						i++
					}
					tokens = append(tokens, Token{Kind: TokNumber, Start: start, End: i, Line: line})
					continue
				case 'o', 'O':
					i++
					for i < len(source) && ((source[i] >= '0' && source[i] <= '7') || source[i] == '_') {
						i++
					}
					tokens = append(tokens, Token{Kind: TokNumber, Start: start, End: i, Line: line})
					continue
				case 'b', 'B':
					i++
					for i < len(source) && (source[i] == '0' || source[i] == '1' || source[i] == '_') {
						i++
					}
					tokens = append(tokens, Token{Kind: TokNumber, Start: start, End: i, Line: line})
					continue
				}
			}
			// Decimal digits (with optional underscores)
			for i < len(source) && (isDigit(source[i]) || source[i] == '_') {
				i++
			}
			// Float: decimal point followed by digit
			if i < len(source) && source[i] == '.' && i+1 < len(source) && isDigit(source[i+1]) {
				i++ // consume '.'
				for i < len(source) && (isDigit(source[i]) || source[i] == '_') {
					i++
				}
			}
			// Scientific notation
			if i < len(source) && (source[i] == 'e' || source[i] == 'E') {
				i++
				if i < len(source) && (source[i] == '+' || source[i] == '-') {
					i++
				}
				for i < len(source) && (isDigit(source[i]) || source[i] == '_') {
					i++
				}
			}
			tokens = append(tokens, Token{Kind: TokNumber, Start: start, End: i, Line: line})

		case ch == '_':
			start := i
			if i+9 < len(source) && string(source[i:i+10]) == "__MODULE__" && !isIdentContinueAt(source, i+10) {
				tokens = append(tokens, Token{Kind: TokModule, Start: i, End: i + 10, Line: line})
				i += 10
			} else {
				i++
				i = scanIdentContinue(source, i)
				word := string(source[start:i])
				if !afterDot {
					if kind, ok := keywordKinds[word]; ok && !isIdentContinueAt(source, i) && !isKeywordKey(source, i) {
						tokens = append(tokens, Token{Kind: kind, Start: start, End: i, Line: line})
					} else {
						tokens = append(tokens, Token{Kind: TokIdent, Start: start, End: i, Line: line})
					}
				} else {
					tokens = append(tokens, Token{Kind: TokIdent, Start: start, End: i, Line: line})
				}
			}

		case isUpper(ch):
			start := i
			i++
			i = scanIdentContinueMod(source, i)
			tokens = append(tokens, Token{Kind: TokModule, Start: start, End: i, Line: line})

		case isLower(ch):
			start := i
			i++
			i = scanIdentContinue(source, i)
			word := string(source[start:i])
			if !afterDot {
				if kind, ok := keywordKinds[word]; ok && !isIdentContinueAt(source, i) && !isKeywordKey(source, i) {
					tokens = append(tokens, Token{Kind: kind, Start: start, End: i, Line: line})
				} else {
					tokens = append(tokens, Token{Kind: TokIdent, Start: start, End: i, Line: line})
				}
			} else {
				tokens = append(tokens, Token{Kind: TokIdent, Start: start, End: i, Line: line})
			}

		default:
			// Check for multi-byte UTF-8 rune (Unicode identifiers)
			if ch >= 0x80 {
				r, size := utf8.DecodeRune(source[i:])
				if r != utf8.RuneError && unicode.IsLetter(r) {
					start := i
					isModuleStart := unicode.IsUpper(r)
					i += size
					if isModuleStart {
						i = scanIdentContinueMod(source, i)
						tokens = append(tokens, Token{Kind: TokModule, Start: start, End: i, Line: line})
					} else {
						i = scanIdentContinue(source, i)
						tokens = append(tokens, Token{Kind: TokIdent, Start: start, End: i, Line: line})
					}
				} else {
					// Non-letter Unicode or invalid UTF-8 — skip the whole rune
					tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + size, Line: line})
					i += size
				}
			} else {
				tokens = append(tokens, Token{Kind: TokOther, Start: i, End: i + 1, Line: line})
				i++
			}
		}

		afterDot = false
	}

	tokens = append(tokens, Token{Kind: TokEOF, Start: len(source), End: len(source), Line: line})
	return TokenResult{Tokens: tokens, LineStarts: lineStarts}
}

// scanStringContent scans from after the opening delimiter to (and including) the matching closing delimiter.
// Returns the new position (after closing delimiter) and updated line count.
// Handles escape sequences and #{} interpolation with proper brace depth tracking.
func scanStringContent(source []byte, i, line int, delim byte) (int, int) {
	for i < len(source) {
		ch := source[i]
		if ch == '\n' {
			line++
			i++
		} else if ch == '\\' {
			i += 2 // skip backslash and next char
		} else if ch == '#' && i+1 < len(source) && source[i+1] == '{' {
			i += 2 // consume #{
			i, line = scanInterpolation(source, i, line)
		} else if ch == delim {
			i++ // consume closing delimiter
			return i, line
		} else {
			i++
		}
	}
	return i, line
}

// scanInterpolation scans the body of a #{} interpolation block, starting after the #{.
// Tracks brace depth and properly handles nested strings, char literals, and sigils
// so that } inside those constructs doesn't prematurely close the interpolation.
func scanInterpolation(source []byte, i, line int) (int, int) {
	depth := 1
	for i < len(source) && depth > 0 {
		c := source[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '\\':
			i += 2
		case c == '"' || c == '\'':
			innerDelim := c
			i++
			i, line = scanStringContent(source, i, line, innerDelim)
		case c == '?' && i+1 < len(source):
			// Character literal inside interpolation: ?} must not close the interpolation.
			i++ // consume '?'
			if source[i] == '\\' {
				i += 2 // escape sequence like ?\n
			} else {
				i++ // single char like ?} or ?a
			}
		case c == '~' && i+1 < len(source) && isLetter(source[i+1]):
			// Sigil inside interpolation: ~r/}/ must not close the interpolation.
			sigilLetter := source[i+1]
			i += 2 // consume ~ and letter
			if i < len(source) {
				i, line = scanSigilContent(source, i, line, sigilLetter)
			}
		case c == '#' && i+1 < len(source) && source[i+1] == '{':
			// Nested interpolation (inside a heredoc or similar)
			i += 2
			i, line = scanInterpolation(source, i, line)
		case c == '{':
			depth++
			i++
		case c == '}':
			depth--
			i++
		default:
			i++
		}
	}
	return i, line
}

// scanHeredocContent scans from after the opening """ (or ”') to (and including) the closing """ on its own line.
// The closing delimiter must appear at the start of a line (possibly with leading whitespace).
func scanHeredocContent(source []byte, i, line int, delim byte) (int, int) {
	for i < len(source) {
		ch := source[i]
		if ch == '\n' {
			line++
			i++
			// Check if the next non-space chars are the closing delimiter
			j := i
			for j < len(source) && (source[j] == ' ' || source[j] == '\t') {
				j++
			}
			if j+2 < len(source) && source[j] == delim && source[j+1] == delim && source[j+2] == delim {
				i = j + 3 // consume closing delimiter
				return i, line
			}
		} else if ch == '\\' {
			i += 2
		} else if ch == '#' && i+1 < len(source) && source[i+1] == '{' {
			i += 2
			i, line = scanInterpolation(source, i, line)
		} else {
			i++
		}
	}
	return i, line
}

// scanSigilContent scans from the opening delimiter of a sigil to its closing delimiter,
// including any trailing modifier letters. Returns new position and updated line count.
// sigilLetter is the letter after ~ (e.g. 's' in ~s, 'S' in ~S). Uppercase sigil letters
// mean the content is "raw" — backslash is NOT an escape character.
func scanSigilContent(source []byte, i, line int, sigilLetter byte) (int, int) {
	if i >= len(source) {
		return i, line
	}

	escapes := isLower(sigilLetter) // only lowercase sigils process escapes
	openCh := source[i]

	// Check for heredoc sigil: ~s""" or ~S"""
	if openCh == '"' && i+2 < len(source) && source[i+1] == '"' && source[i+2] == '"' {
		i += 3 // consume """
		if escapes {
			i, line = scanHeredocContent(source, i, line, '"')
		} else {
			i, line = scanRawHeredocContent(source, i, line, '"')
		}
		return i, line
	}
	if openCh == '\'' && i+2 < len(source) && source[i+1] == '\'' && source[i+2] == '\'' {
		i += 3 // consume '''
		if escapes {
			i, line = scanHeredocContent(source, i, line, '\'')
		} else {
			i, line = scanRawHeredocContent(source, i, line, '\'')
		}
		return i, line
	}

	i++ // consume opening delimiter

	var closeCh byte
	nested := false

	switch openCh {
	case '(':
		closeCh = ')'
		nested = true
	case '[':
		closeCh = ']'
		nested = true
	case '{':
		closeCh = '}'
		nested = true
	case '<':
		closeCh = '>'
		nested = true
	default:
		closeCh = openCh
		nested = false
	}

	if nested {
		depth := 1
		for i < len(source) && depth > 0 {
			ch := source[i]
			if ch == '\n' {
				line++
				i++
			} else if escapes && ch == '\\' && i+1 < len(source) {
				i += 2
			} else if ch == openCh {
				depth++
				i++
			} else if ch == closeCh {
				depth--
				i++
			} else {
				i++
			}
		}
	} else {
		for i < len(source) {
			ch := source[i]
			if ch == '\n' {
				line++
				i++
			} else if escapes && ch == '\\' && i+1 < len(source) {
				i += 2
			} else if ch == closeCh {
				i++ // consume closing delimiter
				break
			} else {
				i++
			}
		}
	}

	// Consume trailing modifier letters (e.g. the 'i' in ~r/foo/i)
	for i < len(source) && isLetter(source[i]) {
		i++
	}

	return i, line
}

// scanRawHeredocContent scans a heredoc body where backslash is NOT an escape character
// (used by uppercase sigils like ~S"""). Only tracks newlines and looks for closing delimiter.
func scanRawHeredocContent(source []byte, i, line int, delim byte) (int, int) {
	for i < len(source) {
		ch := source[i]
		if ch == '\n' {
			line++
			i++
			j := i
			for j < len(source) && (source[j] == ' ' || source[j] == '\t') {
				j++
			}
			if j+2 < len(source) && source[j] == delim && source[j+1] == delim && source[j+2] == delim {
				i = j + 3
				return i, line
			}
		} else {
			i++
		}
	}
	return i, line
}

// isLetter returns true for ASCII [a-zA-Z].
func isLetter(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

// isLower returns true for ASCII [a-z].
func isLower(ch byte) bool {
	return ch >= 'a' && ch <= 'z'
}

// isUpper returns true for ASCII [A-Z].
func isUpper(ch byte) bool {
	return ch >= 'A' && ch <= 'Z'
}

// isDigit returns true for [0-9].
func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

// isHexDigit returns true for [0-9a-fA-F].
func isHexDigit(ch byte) bool {
	return isDigit(ch) || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}

// isIdentContinue returns true for ASCII characters valid after the first character of a lowercase identifier.
func isIdentContinue(ch byte) bool {
	return isLetter(ch) || isDigit(ch) || ch == '_' || ch == '?' || ch == '!' || ch == '@'
}

// isIdentContinueMod returns true for ASCII characters valid in module name identifiers (no ? or !).
func isIdentContinueMod(ch byte) bool {
	return isLetter(ch) || isDigit(ch) || ch == '_' || ch == '@'
}

// scanIdentContinue advances i past identifier continuation characters,
// including multi-byte UTF-8 letters/digits. For lowercase identifiers,
// allows ? and ! as the final character (Elixir convention).
func scanIdentContinue(source []byte, i int) int {
	for i < len(source) {
		ch := source[i]
		if isIdentContinue(ch) {
			i++
			continue
		}
		// Check for multi-byte UTF-8 letter/digit
		if ch >= 0x80 {
			r, size := utf8.DecodeRune(source[i:])
			if r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
				i += size
				continue
			}
		}
		break
	}
	return i
}

// scanIdentContinueMod advances i past module identifier continuation characters
// (no ? or !), including multi-byte UTF-8 letters/digits.
func scanIdentContinueMod(source []byte, i int) int {
	for i < len(source) {
		ch := source[i]
		if isIdentContinueMod(ch) {
			i++
			continue
		}
		if ch >= 0x80 {
			r, size := utf8.DecodeRune(source[i:])
			if r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
				i += size
				continue
			}
		}
		break
	}
	return i
}

// isIdentContinueAt checks if the byte at position i in source is an identifier
// continuation character, including multi-byte UTF-8 letters/digits.
func isIdentContinueAt(source []byte, i int) bool {
	if i >= len(source) {
		return false
	}
	ch := source[i]
	if isIdentContinue(ch) {
		return true
	}
	if ch >= 0x80 {
		r, size := utf8.DecodeRune(source[i:])
		_ = size
		return r != utf8.RuneError && (unicode.IsLetter(r) || unicode.IsDigit(r))
	}
	return false
}

// isUpperAtomStart returns true if source[i] starts an uppercase letter
// (either ASCII A-Z or a multi-byte uppercase Unicode letter).
// Elixir allows :Foo atoms (though they're typically aliases).
func isUpperAtomStart(source []byte, i int) bool {
	if i >= len(source) {
		return false
	}
	if isUpper(source[i]) {
		return true
	}
	if source[i] >= 0x80 {
		r, _ := utf8.DecodeRune(source[i:])
		return r != utf8.RuneError && unicode.IsUpper(r)
	}
	return false
}

// classifyAttr returns the specific TokAttr* kind for known attribute names,
// or TokAttr for everything else. source[start:end] includes the leading '@'.
func classifyAttr(source []byte, start, end int) TokenKind {
	name := source[start+1 : end] // strip '@'
	switch {
	case bytesEqual(name, "doc") || bytesEqual(name, "moduledoc"):
		return TokAttrDoc
	case bytesEqual(name, "spec"):
		return TokAttrSpec
	case bytesEqual(name, "type") || bytesEqual(name, "typep") || bytesEqual(name, "opaque"):
		return TokAttrType
	case bytesEqual(name, "behaviour"):
		return TokAttrBehaviour
	case bytesEqual(name, "callback") || bytesEqual(name, "macrocallback"):
		return TokAttrCallback
	default:
		return TokAttr
	}
}

func bytesEqual(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		if b[i] != s[i] {
			return false
		}
	}
	return true
}

// isKeywordKey checks if source[i] is ':' not followed by another ':'.
// When true, the preceding keyword (do, end, fn, when, etc.) is being used as a
// keyword-list key (e.g. `do: :something`) and should emit TokIdent instead.
func isKeywordKey(source []byte, i int) bool {
	return i < len(source) && source[i] == ':' && (i+1 >= len(source) || source[i+1] != ':')
}
