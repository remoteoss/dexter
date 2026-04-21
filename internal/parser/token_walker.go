package parser

// TokenWalker provides a consistent interface for iterating over tokens with
// automatic depth tracking for brackets and blocks. It consolidates patterns
// that were previously duplicated across multiple functions, ensuring:
//   - Consistent depth tracking (never goes negative)
//   - Forward progress guarantees
//   - Proper handling of module-defining constructs (defmodule, defprotocol, defimpl)
//
// Usage:
//
//	w := NewTokenWalker(source, tokens)
//	for w.More() {
//	    tok := w.Current()
//	    // process token...
//	    w.Advance()
//	}
type TokenWalker struct {
	Source []byte
	Tokens []Token
	N      int

	pos        int
	depth      int // bracket depth: (), [], {}, <<>>
	blockDepth int // block depth: do/fn/end
}

// NewTokenWalker creates a walker starting at position 0.
func NewTokenWalker(source []byte, tokens []Token) *TokenWalker {
	return &TokenWalker{
		Source: source,
		Tokens: tokens,
		N:      len(tokens),
		pos:    0,
	}
}

// Pos returns the current position.
func (w *TokenWalker) Pos() int {
	return w.pos
}

// SetPos sets the current position (does not update depths automatically).
func (w *TokenWalker) SetPos(pos int) {
	w.pos = pos
}

// More returns true if there are more tokens to process.
func (w *TokenWalker) More() bool {
	return w.pos < w.N
}

// Current returns the token at the current position.
// Panics if position is out of bounds.
func (w *TokenWalker) Current() Token {
	return w.Tokens[w.pos]
}

// CurrentKind returns the kind of the current token, or TokEOF if at end.
func (w *TokenWalker) CurrentKind() TokenKind {
	if w.pos >= w.N {
		return TokEOF
	}
	return w.Tokens[w.pos].Kind
}

// CurrentText returns the text of the current token.
func (w *TokenWalker) CurrentText() string {
	if w.pos >= w.N {
		return ""
	}
	return TokenText(w.Source, w.Tokens[w.pos])
}

// Peek returns the token at pos+offset without advancing.
// Returns nil if out of bounds.
func (w *TokenWalker) Peek(offset int) *Token {
	idx := w.pos + offset
	if idx < 0 || idx >= w.N {
		return nil
	}
	return &w.Tokens[idx]
}

// PeekKind returns the kind at pos+offset, or TokEOF if out of bounds.
func (w *TokenWalker) PeekKind(offset int) TokenKind {
	if tok := w.Peek(offset); tok != nil {
		return tok.Kind
	}
	return TokEOF
}

// Advance moves forward by one token and updates depth counters.
func (w *TokenWalker) Advance() {
	if w.pos < w.N {
		w.trackDepths(w.Tokens[w.pos].Kind)
		w.pos++
	}
}

// AdvanceTo moves to a specific position (must be >= current pos).
// Does NOT track depths for skipped tokens — use when you know depths are irrelevant.
func (w *TokenWalker) AdvanceTo(pos int) {
	if pos > w.pos {
		w.pos = pos
	}
}

// AdvanceWithDepthTracking moves to a specific position while tracking depths
// for all intermediate tokens.
func (w *TokenWalker) AdvanceWithDepthTracking(pos int) {
	for w.pos < pos && w.pos < w.N {
		w.Advance()
	}
}

// SkipToNextSig advances to the next significant token (skipping EOL and comments).
func (w *TokenWalker) SkipToNextSig() {
	w.pos = NextSigToken(w.Tokens, w.N, w.pos)
}

// NextSigPos returns the position of the next significant token without advancing.
func (w *TokenWalker) NextSigPos() int {
	return NextSigToken(w.Tokens, w.N, w.pos)
}

// Depth returns the current bracket depth.
func (w *TokenWalker) Depth() int {
	return w.depth
}

// BlockDepth returns the current block depth.
func (w *TokenWalker) BlockDepth() int {
	return w.blockDepth
}

// AtBalancedPoint returns true when both bracket and block depths are zero.
func (w *TokenWalker) AtBalancedPoint() bool {
	return w.depth == 0 && w.blockDepth == 0
}

// trackDepths updates depth counters based on token kind, with clamping to prevent
// negative values (which cause bugs when starting mid-expression).
func (w *TokenWalker) trackDepths(kind TokenKind) {
	switch kind {
	case TokOpenParen, TokOpenBracket, TokOpenBrace, TokOpenAngle:
		w.depth++
	case TokCloseParen, TokCloseBracket, TokCloseBrace, TokCloseAngle:
		if w.depth > 0 {
			w.depth--
		}
	case TokDo, TokFn:
		w.blockDepth++
	case TokEnd:
		if w.blockDepth > 0 {
			w.blockDepth--
		}
	}
}

// CollectModuleName collects a module name starting at current position.
// Returns the name and advances the walker past the module name tokens.
func (w *TokenWalker) CollectModuleName() string {
	name, nextPos := CollectModuleName(w.Source, w.Tokens, w.N, w.pos)
	w.pos = nextPos
	return name
}

// ScanForBlockDo scans forward for a block-opening TokDo.
// Returns true and advances past the do if found.
// Returns false and advances to the boundary token if not found.
func (w *TokenWalker) ScanForBlockDo() bool {
	doIdx, nextPos, hasDo := ScanForwardToBlockDo(w.Tokens, w.N, w.pos)
	if hasDo {
		w.pos = nextPos
		w.blockDepth++
		return true
	}
	w.pos = nextPos
	_ = doIdx
	return false
}

// ScanKeywordOption looks for `key: Value` after the current position.
// If found, returns the value and advances past it.
func (w *TokenWalker) ScanKeywordOption(key string) (value string, ok bool) {
	value, nextPos, ok := ScanKeywordOptionValue(w.Source, w.Tokens, w.N, w.pos, key)
	if ok {
		w.pos = nextPos
	}
	return value, ok
}

// IsModuleDefiningToken returns true if the current token is defmodule, defprotocol, or defimpl.
func (w *TokenWalker) IsModuleDefiningToken() bool {
	if w.pos >= w.N {
		return false
	}
	switch w.Tokens[w.pos].Kind {
	case TokDefmodule, TokDefprotocol, TokDefimpl:
		return true
	}
	return false
}

// IsFunctionDefiningToken returns true if the current token is a function definition keyword.
func (w *TokenWalker) IsFunctionDefiningToken() bool {
	if w.pos >= w.N {
		return false
	}
	switch w.Tokens[w.pos].Kind {
	case TokDef, TokDefp, TokDefmacro, TokDefmacrop, TokDefguard, TokDefguardp, TokDefdelegate:
		return true
	}
	return false
}

// IsStatementBoundary returns true if the current token is a statement boundary.
func (w *TokenWalker) IsStatementBoundary() bool {
	if w.pos >= w.N {
		return true
	}
	return IsStatementBoundaryToken(w.Tokens[w.pos].Kind)
}

// SkipToEndOfStatement advances past the current statement to EOL/EOF at depth 0.
func (w *TokenWalker) SkipToEndOfStatement() {
	for w.pos < w.N {
		kind := w.Tokens[w.pos].Kind
		switch kind {
		case TokOpenParen, TokOpenBracket, TokOpenBrace, TokOpenAngle:
			w.depth++
		case TokCloseParen, TokCloseBracket, TokCloseBrace, TokCloseAngle:
			if w.depth > 0 {
				w.depth--
			}
		case TokDo, TokFn:
			w.blockDepth++
		case TokEnd:
			if w.blockDepth > 0 {
				w.blockDepth--
			}
		case TokEOL, TokEOF:
			if w.depth <= 0 && w.blockDepth <= 0 {
				return
			}
		}
		w.pos++
	}
}

// EnsureProgress guarantees the walker advances by at least one token.
// Call this in loops where external functions might not advance position.
func (w *TokenWalker) EnsureProgress(prevPos int) {
	if w.pos == prevPos && w.pos < w.N {
		w.pos++
	}
}

// TokenAt returns the token at the given position, or nil if out of bounds.
func (w *TokenWalker) TokenAt(pos int) *Token {
	if pos < 0 || pos >= w.N {
		return nil
	}
	return &w.Tokens[pos]
}

// TextAt returns the text of the token at the given position.
func (w *TokenWalker) TextAt(pos int) string {
	if pos < 0 || pos >= w.N {
		return ""
	}
	return TokenText(w.Source, w.Tokens[pos])
}
