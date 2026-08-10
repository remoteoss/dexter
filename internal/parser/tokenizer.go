package parser

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// TokenKind identifies the kind of a lexed token.
//
//go:generate stringer -type=TokenKind
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
	TokHEEXOpenTag                    // <
	TokHEEXCloseTag                   // </
	TokHEEXOpenExpr                   // { or <% or <%=
	TokHEEXCloseExpr                  // } or %>
	TokNumber                         // integer or float literal
	TokComment                        // # to end of line
	TokEOL                            // newline
	TokEOF                            // end of input
	TokOther                          // anything else (operators, etc.)
)

// Token is a lexed token from an Elixir source file.
// Start and End are byte offsets into the source; source[Start:End] is the token text.
// Line is 1-based. If the token is interpolated within another token, Parent will indicate
// the index of its nearest parent. Container tokens are emitted [Container A B C], where
// Container[Start:End] indicates the full range and A, B, and C's ranges are strictly
// within the container's range. Top-level tokens have a Parent of -1.
type Token struct {
	Kind   TokenKind
	Start  int
	End    int
	Line   int
	Parent int
}

// TokenResult holds the output of Tokenize: the token stream and a line-starts
// table for O(1) byte-offset-to-column conversion. LineStarts[i] is the byte
// offset of the first character on line i+1 (0-indexed). Column for a token
// on line L at byte offset B is: B - LineStarts[L-1].
//
// Note: LineStarts only tracks newlines seen by the main tokenizer loop (bare
// newlines between tokens). Escaped newlines inside strings, heredocs, sigils,
// and interpolations increment Token.Line correctly but are NOT reflected in
// LineStarts. Callers needing column info for tokens inside multi-line string
// literals should compute it from byte offsets directly.
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
	_, _, _, result := tokenizeUntil(source, nil, nil)
	assignParents(result.Tokens)
	return result
}

// isContainer returns true if a token can enclose interpolated child tokens.
func isContainer(k TokenKind) bool {
	return k == TokString || k == TokHeredoc || k == TokSigil
}

// assignParents sets each token's Parent to the index of its innermost
// enclosing container (TokString/TokHeredoc/TokSigil), or -1 at top level.
func assignParents(tokens []Token) {
	var stack []int // indices of still-open container tokens only
	for i := range tokens {
		// drop containers that ended before this token begins
		for len(stack) > 0 && tokens[stack[len(stack)-1]].End <= tokens[i].Start {
			stack = stack[:len(stack)-1]
		}
		if len(stack) > 0 {
			tokens[i].Parent = stack[len(stack)-1]
		} else {
			tokens[i].Parent = -1
		}
		if isContainer(tokens[i].Kind) {
			stack = append(stack, i)
		}
	}
}

// tokenizeUntil tokenizes the given source until the given terminator is reached
// in a non-ignored context (ignored when it appears within a comment or string literal).
// If an opener sequence is provided, nested opener/terminator pairs will be ignored, e.g.
// open/close curly braces. Returns the byte offset and line number that tokenizing
// stopped along with the token result. Returns `true` if the provided terminator
// appeared and was consumed.
func tokenizeUntil(source, opener, terminator []byte) (int, int, bool, TokenResult) {
	tokenCapacity := len(source) / 8
	lineCapacity := 64
	if terminator != nil {
		// use a smaller initial capacity when scanning for a terminator, since the typical
		// case is tokenizing a small expression within an interpolation
		tokenCapacity = 16
		lineCapacity = 8

		if bytes.ContainsRune(terminator, '\n') {
			panic("Terminator may not contain newline")
		}
	}
	if opener != nil && bytes.ContainsRune(opener, '\n') {
		panic("Opener may not contain newline")
	}

	tokens := make([]Token, 0, tokenCapacity)
	lineStarts := make([]int, 1, lineCapacity)
	lineStarts[0] = 0 // line 1 starts at byte 0
	line := 1
	i := 0
	nestingLevel := 0
	afterDot := false // true when the last significant token was TokDot

	for i < len(source) {
		ch := source[i]

		if opener != nil && bytes.HasPrefix(source[i:], opener) {
			nestingLevel++
		}

		if terminator != nil && bytes.HasPrefix(source[i:], terminator) {
			if nestingLevel == 0 {
				return i, line, true, TokenResult{Tokens: tokens, LineStarts: lineStarts}
			}
			nestingLevel--
		}

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
							if source[i] == '\n' {
								line++
								lineStarts = append(lineStarts, i+1)
							}
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
				tokens = append(tokens, Token{Kind: TokHeredoc, Start: start, End: -1, Line: startLine})
				tokenIdx := len(tokens) - 1
				// scan to closing """ on its own line
				i, line, _ = scanHeredocContent(source, i, line, '"', &lineStarts, &tokens)
				tokens[tokenIdx].End = i
			} else {
				start := i
				startLine := line
				i++ // consume opening "
				tokens = append(tokens, Token{Kind: TokString, Start: start, End: -1, Line: startLine})
				tokenIdx := len(tokens) - 1
				i, line = scanStringContent(source, i, line, '"', &lineStarts, &tokens)
				tokens[tokenIdx].End = i
			}

		case ch == '\'':
			// Check for heredoc
			if i+2 < len(source) && source[i+1] == '\'' && source[i+2] == '\'' {
				start := i
				startLine := line
				i += 3 // consume opening '''
				tokens = append(tokens, Token{Kind: TokHeredoc, Start: start, End: -1, Line: startLine})
				tokenIdx := len(tokens) - 1
				i, line, _ = scanHeredocContent(source, i, line, '\'', &lineStarts, &tokens)
				tokens[tokenIdx].End = i
			} else {
				start := i
				startLine := line
				i++ // consume opening '
				tokens = append(tokens, Token{Kind: TokString, Start: start, End: -1, Line: startLine})
				tokenIdx := len(tokens) - 1
				i, line = scanStringContent(source, i, line, '\'', &lineStarts, &tokens)
				tokens[tokenIdx].End = i
			}

		case ch == '~':
			// Sigil: ~ followed by letter(s) then delimiter.
			// Single-char sigils: ~r, ~s, ~S, etc.
			// Multi-char sigils (Elixir 1.15+): ~HTML, ~HEEX — uppercase only.
			if i+1 < len(source) && isLetter(source[i+1]) {
				i, line = scanSigil(source, i, line, &lineStarts, &tokens)
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
				tokens = append(tokens, Token{Kind: TokAtom, Start: start, End: -1, Line: startLine})
				tokenIdx := len(tokens) - 1
				i, line = scanStringContent(source, i, line, '"', &lineStarts, &tokens)
				tokens[tokenIdx].End = i
			} else if i+1 < len(source) && source[i+1] == '\'' {
				// Atom with quoted charlist: :'...'
				start := i
				startLine := line
				i += 2 // consume :'
				tokens = append(tokens, Token{Kind: TokAtom, Start: start, End: -1, Line: startLine})
				tokenIdx := len(tokens) - 1
				i, line = scanStringContent(source, i, line, '\'', &lineStarts, &tokens)
				tokens[tokenIdx].End = i
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
	return i, line, false, TokenResult{Tokens: tokens, LineStarts: lineStarts}
}

// scanStringContent scans from after the opening delimiter to (and including) the matching closing delimiter.
// Returns the new position (after closing delimiter) and updated line count.
// Handles escape sequences and #{} interpolation with proper brace depth tracking.
func scanStringContent(source []byte, i, line int, delim byte, lineStarts *[]int, tokens *[]Token) (int, int) {
	for i < len(source) {
		ch := source[i]
		if ch == '\n' {
			line++
			i++
			*lineStarts = append(*lineStarts, i)
		} else if ch == '\\' && i+1 < len(source) {
			if source[i+1] == '\n' {
				line++
				*lineStarts = append(*lineStarts, i+2)
			}
			i += 2 // skip backslash and next char
		} else if ch == '#' && i+1 < len(source) && source[i+1] == '{' {
			i += 2 // consume #{
			i, line = scanInterpolation(source, i, line, lineStarts, tokens)
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
func scanInterpolation(source []byte, i, line int, lineStarts *[]int, tokens *[]Token) (int, int) {
	start := i
	startLine := line

	i_, line_, terminated, result := tokenizeUntil(source[start:], []byte("{"), []byte("}"))
	for _, t := range result.Tokens {
		if t.Kind != TokEOF {
			*tokens = append(*tokens, Token{Kind: t.Kind, Start: t.Start + start, End: t.End + start, Line: t.Line + startLine - 1})
		}
	}
	for _, lineStart := range result.LineStarts[1:] {
		*lineStarts = append(*lineStarts, lineStart+start)
	}

	i += i_
	line += line_ - 1

	// consume terminator, if it was encountered
	if terminated {
		i++
	}

	return i, line
}

// scanHeredocContent scans from after the opening """ (or ”') to (and including) the closing """ on its own line.
// The closing delimiter must appear at the start of a line (possibly with leading whitespace). Returns `true` if
// the closing delimiter appeared in full and was consumed.
func scanHeredocContent(source []byte, i, line int, delim byte, lineStarts *[]int, tokens *[]Token) (int, int, bool) {
	for i < len(source) {
		ch := source[i]
		if ch == '\n' {
			line++
			i++
			*lineStarts = append(*lineStarts, i)
			// Check if the next non-space chars are the closing delimiter
			j := i
			for j < len(source) && (source[j] == ' ' || source[j] == '\t') {
				j++
			}
			if j+2 < len(source) && source[j] == delim && source[j+1] == delim && source[j+2] == delim {
				i = j + 3 // consume closing delimiter
				return i, line, true
			}
		} else if ch == '\\' && i+1 < len(source) {
			if source[i+1] == '\n' {
				line++
				*lineStarts = append(*lineStarts, i+2)
			}
			i += 2
		} else if ch == '#' && i+1 < len(source) && source[i+1] == '{' {
			i += 2
			i, line = scanInterpolation(source, i, line, lineStarts, tokens)
		} else {
			i++
		}
	}
	return i, line, false
}

func scanSigilCharacters(source []byte, i int) int {
	firstLetter := source[i]
	i++

	// Multi-char sigils: continue reading uppercase letters/numbers
	if isUpper(firstLetter) {
		for i < len(source) && (isUpper(source[i]) || isDigit(source[i])) {
			i++
		}
	}

	return i
}

// scanSigil scans from the start of a sigil to its closing delimiter, including any trailing
// modifier letters. Returns new position and updated line count, adding any tokens encountered
// along the way if `tokens` is provided.
func scanSigil(source []byte, i, line int, lineStarts *[]int, tokens *[]Token) (int, int) {
	if i >= len(source) {
		return i, line
	}

	// sigilLetter is the letter after ~ (e.g. 's' in ~s, 'S' in ~S). Uppercase sigil
	// letters mean the content is "raw" — backslash is NOT an escape character.
	start := i
	startLine := line
	i++ // consume ~

	i = scanSigilCharacters(source, i)
	sigilChars := string(source[start+1 : i])

	if i == len(source) {
		return i, line
	}

	// only lowercase sigils process escapes
	// (this means HEEX's sigil_H won't double-tokenize interpolations)
	escapes := isLower([]byte(sigilChars)[0])
	openCh := source[i]

	var contentsStart, contentsEnd int

	// Check for heredoc sigil: ~s""" or ~S"""
	if openCh == '"' && i+2 < len(source) && source[i+1] == '"' && source[i+2] == '"' {
		i += 3 // consume """
		contentsStart = i
		var terminated bool
		if escapes {
			i, line, terminated = scanHeredocContent(source, i, line, '"', lineStarts, tokens)
		} else {
			i, line, terminated = scanRawHeredocContent(source, i, line, '"', lineStarts)
		}
		if terminated {
			contentsEnd = i - 3
		} else {
			contentsEnd = i
		}
	} else if openCh == '\'' && i+2 < len(source) && source[i+1] == '\'' && source[i+2] == '\'' {
		i += 3 // consume '''
		contentsStart = i
		var terminated bool
		if escapes {
			i, line, terminated = scanHeredocContent(source, i, line, '\'', lineStarts, tokens)
		} else {
			i, line, terminated = scanRawHeredocContent(source, i, line, '\'', lineStarts)
		}
		if terminated {
			contentsEnd = i - 3
		} else {
			contentsEnd = i
		}
	} else {
		i++ // consume opening delimiter
		contentsStart = i

		var closeCh byte
		nested := false
		terminated := false

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
					*lineStarts = append(*lineStarts, i)
				} else if escapes && ch == '\\' && i+1 < len(source) {
					if source[i+1] == '\n' {
						line++
						*lineStarts = append(*lineStarts, i+2)
					}
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
			terminated = depth == 0
		} else {
			for i < len(source) {
				ch := source[i]
				if ch == '\n' {
					line++
					i++
					*lineStarts = append(*lineStarts, i)
				} else if escapes && ch == '\\' && i+1 < len(source) {
					if source[i+1] == '\n' {
						line++
						*lineStarts = append(*lineStarts, i+2)
					}
					i += 2
				} else if ch == closeCh {
					i++ // consume closing delimiter
					terminated = true
					break
				} else {
					i++
				}
			}
		}

		if terminated {
			contentsEnd = i - 1
			// Consume trailing modifier letters (e.g. the 'i' in ~r/foo/i)
			for i < len(source) && isLetter(source[i]) {
				i++
			}
		} else {
			contentsEnd = i
		}
	}

	// emit tokens if requested
	if tokens != nil {
		if contentsEnd == contentsStart {
			// empty sigil
			*tokens = append(*tokens, Token{Kind: TokSigil, Start: start, End: i, Line: line})
		} else {
			scanSigilContents(sigilChars, source, start, i, contentsStart, contentsEnd, startLine, tokens)
		}
	}

	return i, line
}

func scanSigilContents(sigilChars string, source []byte, start, end, contentsStart, contentsEnd, line int, tokens *[]Token) {
	// always emit a token for the sigil
	*tokens = append(*tokens, Token{Kind: TokSigil, Start: start, End: end, Line: line})

	// only scan the contents of HEEX `~H` sigils
	if sigilChars != "H" {
		return
	}

	// lineStarts has already been updated by `scanHeredocContent` / `scanRawHeredocContent`
	result := TokenizeHeex(source[contentsStart:contentsEnd])
	for _, t := range result.Tokens {
		if t.Kind != TokEOF {
			*tokens = append(*tokens, Token{Kind: t.Kind, Start: t.Start + contentsStart, End: t.End + contentsStart, Line: t.Line + line - 1})
		}
	}
}

// A HEEX tag encountered during tokenization.
type heexTag struct {
	// The tag's name, e.g. `div`, `.bar`, or `Foo.bar`.
	Name []byte
	// Whether the tag is self-closing; when `false`, a matching closing tag
	// is likely expected to be present somewhere later in the source.
	SelfClosing bool
	// Whether {..} interpolation is enabled within this tag's descendents
	// (the `phx-no-curly-interpolation` attribute is set). This has no effect
	// on self-closing tags, since they have no descendents.
	NoInterpolate bool
}

type heexTagStackNode struct {
	tag         *heexTag
	interpolate bool
}

// A stack of HEEX tags representing all ancestor nodes to the node currently being tokenized.
// Useful for tracking whether {..} curly interpolation is currently enabled, as disabling it
// on a single tag disables it for all of that tag's descendents.
type heexTagStack struct {
	tags []heexTagStackNode
}

// Create an empty HEEX tag stack. Curly interpolation is enabled by default in the root.
func newHeexTagStack() *heexTagStack {
	return &heexTagStack{
		tags: make([]heexTagStackNode, 0, 8),
	}
}

func (s *heexTagStack) current() *heexTagStackNode {
	if len(s.tags) == 0 {
		return nil
	}
	return &s.tags[len(s.tags)-1]
}

// Whether {..} curly brace interpolation is currently enabled in the stack.
func (s *heexTagStack) curlyInterpolate() bool {
	if cur := s.current(); cur != nil {
		return cur.interpolate
	}
	return true
}

// Descend into the given tag.
func (s *heexTagStack) enter(t *heexTag) {
	s.tags = append(s.tags, heexTagStackNode{
		tag:         t,
		interpolate: s.curlyInterpolate() && !t.NoInterpolate,
	})
}

// Move up and out of the given tag. In well-formed HTML, close tags should
// always match the most recently entered tag.
func (s *heexTagStack) leave() {
	if len(s.tags) > 0 {
		s.tags = s.tags[:len(s.tags)-1]
	}
}

func TokenizeHeex(source []byte) TokenResult {
	tokens := make([]Token, 0, len(source)/8)
	lineStarts := make([]int, 1, 64)
	lineStarts[0] = 0 // line 1 starts at byte 0
	line := 1
	i := 0

	scanComment := func(delim string, i, line int, lineStarts *[]int) (int, int) {
		for i < len(source) {
			if bytes.HasPrefix(source[i:], []byte(delim)) {
				i += len(delim)
				break
			}
			if source[i] == '\n' {
				line++
				*lineStarts = append(*lineStarts, i+1)
			}
			i++
		}
		return i, line
	}

	scanInterpolation := func(i, line int, opener, terminator string, tokens *[]Token) (int, int) {
		openerStart := i - len(opener)

		// consume "=" output indicator from "<%=" special form prefix
		if opener == "<%" && i < len(source) && source[i] == '=' {
			i++
		}

		*tokens = append(*tokens, Token{Kind: TokHEEXOpenExpr, Start: openerStart, End: i, Line: line})

		start := i
		startLine := line

		// lineStarts has already been updated during heredoc scanning
		i_, line_, terminated, result := tokenizeUntil(source[start:], []byte(opener), []byte(terminator))
		for _, t := range result.Tokens {
			if t.Kind != TokEOF {
				*tokens = append(*tokens, Token{Kind: t.Kind, Start: t.Start + start, End: t.End + start, Line: t.Line + startLine - 1})
			}
		}

		i += i_
		line += line_ - 1

		if terminated {
			i += len(terminator)
			*tokens = append(*tokens, Token{Kind: TokHEEXCloseExpr, Start: i - len(terminator), End: i, Line: line})
		}

		return i, line
	}

	scanTagName := func(i, line int, tokens *[]Token) (int, int, []byte) {
		start := i
		for i < len(source) {
			switch {
			// <.foo
			case source[i] == '.':
				*tokens = append(*tokens, Token{Kind: TokDot, Start: i, End: i + 1, Line: line})
				i++

				start := i
				for i < len(source) && (isLetter(source[i]) || isDigit(source[i]) || source[i] == '_' || source[i] == '-') {
					i++
				}
				if i == start {
					return i, line, source[start:i]
				}

				if isUpper(source[start]) {
					*tokens = append(*tokens, Token{Kind: TokModule, Start: start, End: i, Line: line})
				} else {
					*tokens = append(*tokens, Token{Kind: TokIdent, Start: start, End: i, Line: line})
					return i, line, source[start:i]
				}

			// <div OR <Foo.bar
			case isLetter(source[i]):
				start := i
				for i < len(source) && (isLetter(source[i]) || isDigit(source[i]) || source[i] == '_' || source[i] == '-') {
					i++
				}
				if i == start {
					return i, line, source[start:i]
				}

				if i < len(source) && isUpper(source[start]) && source[i] == '.' {
					// if this is a module name, keep looking for addn'l modules or function in chain
					*tokens = append(*tokens, Token{Kind: TokModule, Start: start, End: i, Line: line})
				} else {
					// otherwise, this is a HTML tag name
					return i, line, source[start:i]
				}

			default:
				return i, line, source[start:i]
			}
		}

		return i, line, source[start:i]
	}

	scanTagAttr := func(i, line int, lineStarts *[]int, tokens *[]Token) (int, int, []byte) {
		start := i
		var quoteChar byte
		for i < len(source) {
			ch := source[i]

			switch {
			case ch == '\n':
				*tokens = append(*tokens, Token{Kind: TokEOL, Start: i, End: i + 1, Line: line})
				line++
				i++
				*lineStarts = append(*lineStarts, i)

			case quoteChar == 0 && isWhitespace(ch):
				return i, line, source[start:i]

			case quoteChar == 0 && (ch == '\'' || ch == '"'):
				quoteChar = ch
				i++

			case quoteChar != 0 && ch == '\\':
				i++
				if i < len(source) {
					i++
				}

			case quoteChar != 0 && ch == quoteChar:
				i++
				return i, line, source[start:i]

			case quoteChar != 0:
				i++

			case ch == '{':
				i++
				i, line = scanInterpolation(i, line, "{", "}", tokens)
				return i, line, source[start:i]

			case ch == '>':
				return i, line, source[start:i]

			default:
				i++
			}
		}

		return i, line, source[start:i]
	}

	scanTag := func(i, line int, lineStarts *[]int, tokens *[]Token) (int, int, heexTag) {
		t := heexTag{}
		hasName := false
		for i < len(source) {
			switch {
			case source[i] == '\n':
				*tokens = append(*tokens, Token{Kind: TokEOL, Start: i, End: i + 1, Line: line})
				line++
				i++
				*lineStarts = append(*lineStarts, i)

			case isWhitespace(source[i]):
				i++

			// HTML tag name, function, or module/function
			case !hasName:
				i, line, t.Name = scanTagName(i, line, tokens)
				hasName = true

			// attribute
			case isLetter(source[i]):
				var attr []byte
				i, line, attr = scanTagAttr(i, line, lineStarts, tokens)
				if slices.Equal(attr, []byte("phx-no-curly-interpolation")) {
					t.NoInterpolate = true
				}

			// HEEX special attribute ":if={}", ":for={}", ":let={}", ":type={}"
			case source[i] == ':':
				i++
				i, line, _ = scanTagAttr(i, line, lineStarts, tokens)

			// dynamic attributes "<div {dynamic_attrs()}"
			case source[i] == '{':
				i++
				i, line = scanInterpolation(i, line, "{", "}", tokens)

			// self-closing tag
			case source[i] == '/':
				i++
				if i < len(source) && source[i] == '>' {
					i++
					t.SelfClosing = true
					return i, line, t
				}

			// finish open tag
			case source[i] == '>':
				i++
				return i, line, t

			default:
				i++
			}
		}

		return i, line, t
	}

	tagStack := newHeexTagStack()

	// Tokenization is disabled within <script>/<style> tags; this tracks the exact sequence that
	// we must encounter, e.g. "</script>", to resume tokenizing. No whitespace or newlines are
	// allowed within the close tag, so we can match on an exact byte sequence.
	var skipUntilCloseTag []byte

	for i < len(source) {
		ch := source[i]

		switch {
		case ch == '\n':
			tokens = append(tokens, Token{Kind: TokEOL, Start: i, End: i + 1, Line: line})
			line++
			i++
			lineStarts = append(lineStarts, i)

		case isWhitespace(ch):
			i++

		case skipUntilCloseTag != nil:
			if bytes.HasPrefix(source[i:], skipUntilCloseTag) {
				tokens = append(tokens, Token{Kind: TokHEEXCloseTag, Start: i, End: i + 2, Line: line})
				i += len(skipUntilCloseTag)
				tagStack.leave()
				skipUntilCloseTag = nil
			} else if i+1 < len(source) && ch == '<' && source[i+1] == '%' {
				i += 2
				// EEx interpolation (<% .. %> and <%= .. %>) are still active within script/style tags
				i, line = scanInterpolation(i, line, "<%", "%>", &tokens)
			} else {
				i++
			}

		case ch == '<':
			// consume <
			i++
			if i < len(source) {
				if source[i] == '!' && i+2 < len(source) && source[i+1] == '-' && source[i+2] == '-' {
					// HTML comment "<!--"
					i += 3
					start := i - 4
					startLine := line
					i, line = scanComment("-->", i, line, &lineStarts)
					tokens = append(tokens, Token{Kind: TokComment, Start: start, End: i, Line: startLine})
				} else if source[i] == '%' {
					// consume %
					i++
					if i+2 < len(source) && source[i] == '!' && source[i+1] == '-' && source[i+2] == '-' {
						// HEEX comment "<%!--"
						i += 3
						start := i - 5
						startLine := line
						i, line = scanComment("--%>", i, line, &lineStarts)
						tokens = append(tokens, Token{Kind: TokComment, Start: start, End: i, Line: startLine})
					} else if i < len(source) {
						// EEX interpolation "<%" or "<%="
						i, line = scanInterpolation(i, line, "<%", "%>", &tokens)
					}
				} else if source[i] == '/' {
					i++
					tokens = append(tokens, Token{Kind: TokHEEXCloseTag, Start: i - 2, End: i, Line: line})
					i, line, _ = scanTagName(i, line, &tokens)
					tagStack.leave()
					if i < len(source) && source[i] == '>' {
						i++
					}
				} else {
					// HTML tag "<div"
					// HEEX component "<.foo", "<Foo.bar"
					tokens = append(tokens, Token{Kind: TokHEEXOpenTag, Start: i - 1, End: i, Line: line})

					var t heexTag
					i, line, t = scanTag(i, line, &lineStarts, &tokens)
					if !t.SelfClosing {
						tagStack.enter(&t)
					}

					if slices.Equal(t.Name, []byte("script")) {
						skipUntilCloseTag = []byte("</script>")
					} else if slices.Equal(t.Name, []byte("style")) {
						skipUntilCloseTag = []byte("</style>")
					}
				}
			}

		case tagStack.curlyInterpolate() && ch == '{':
			// HEEX interpolation "{"
			i++
			i, line = scanInterpolation(i, line, "{", "}", &tokens)

		default:
			i++
		}
	}

	tokens = append(tokens, Token{Kind: TokEOF, Start: len(source), End: len(source), Line: line})
	return TokenResult{
		Tokens:     tokens,
		LineStarts: lineStarts,
	}
}

// scanRawHeredocContent scans a heredoc body where backslash is NOT an escape character
// (used by uppercase sigils like ~S"""). Only tracks newlines and looks for closing delimiter.
// Returns true if the closing delimiter appeared and was consumed in full.
func scanRawHeredocContent(source []byte, i, line int, delim byte, lineStarts *[]int) (int, int, bool) {
	for i < len(source) {
		ch := source[i]
		if ch == '\n' {
			line++
			i++
			*lineStarts = append(*lineStarts, i)
			j := i
			for j < len(source) && (source[j] == ' ' || source[j] == '\t') {
				j++
			}
			if j+2 < len(source) && source[j] == delim && source[j+1] == delim && source[j+2] == delim {
				i = j + 3
				return i, line, true
			}
		} else {
			i++
		}
	}
	return i, line, false
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

// isWhitespace returns true for space, tab, and carriage return.
func isWhitespace(ch byte) bool {
	return ch == ' ' || ch == '\t' || ch == '\r'
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

// DebugTokens returns a string represention similar to %+v for a slice of tokens.
func DebugTokens(source []byte, tokens []Token) string {
	var s strings.Builder

	for _, t := range tokens {
		s.WriteString(t.Debug(source))
	}

	return s.String()
}

// Debug returns a string representation similar to %+v for a token.
func (token Token) Debug(source []byte) string {
	switch token.Kind {
	case TokDot, TokEOL, TokEOF, TokOpenBrace, TokCloseBrace, TokHEEXOpenTag, TokHEEXCloseTag:
		return fmt.Sprintf("%s (%d:%d)\n", token.Kind.String(), token.Start, token.End)

	default:
		return fmt.Sprintf("%s (%d:%d) %#v\n", token.Kind.String(), token.Start, token.End, TokenText(source, token))
	}
}
