package lsp

import (
	"unicode/utf8"

	"go.lsp.dev/protocol"
)

// PositionEncoding is the unit both sides use to count Position.Character.
//
// The unit is a property of the protocol, not of the file. Elixir sources are
// UTF-8 on disk either way; what changes here is only how a cursor column is
// counted along a line.
//
// LSP made UTF-16 the default because it grew out of VS Code, whose strings
// are UTF-16 internally, and that is what Dexter uses. LSP 3.17 added a way to
// negotiate something else (the client offers general.positionEncodings and
// the server echoes its pick in capabilities.positionEncoding), and Dexter
// deliberately does not: a client only sends utf-8 if the server asks for it,
// so declaring nothing means every client speaks UTF-16 and there is exactly
// one path to get right. Negotiating utf-8 would skip the conversion below,
// but that conversion costs ~25ns on an ASCII line and ~0.2ms on the largest
// references reply we produce, which is under 1% of that request. The other
// encodings stay implemented and tested so the choice can be revisited
// cheaply.
type PositionEncoding string

const (
	// EncodingUTF8 counts UTF-8 bytes. Dexter's internal representation, so
	// this is the fast path: every conversion is the identity.
	EncodingUTF8 PositionEncoding = "utf-8"
	// EncodingUTF16 counts UTF-16 code units. The protocol default, and the
	// only encoding a client is required to support.
	EncodingUTF16 PositionEncoding = "utf-16"
	// EncodingUTF32 counts Unicode code points. Some Emacs clients prefer it.
	EncodingUTF32 PositionEncoding = "utf-32"
)

// runeUnits is how many units of enc one rune occupies.
func runeUnits(r rune, enc PositionEncoding) int {
	if enc == EncodingUTF16 && r > 0xFFFF {
		// Astral code points are a surrogate pair: two UTF-16 units for one
		// code point. This is the case the bottom-emoji bug turned on.
		return 2
	}
	return 1
}

// isASCII reports whether line is pure ASCII, in which case every encoding
// counts it identically and conversion is the identity.
//
// Almost every line of almost every file takes this path, so it is worth the
// scan: it keeps the conversion off the hot path for ordinary code.
func isASCII(line string) bool {
	for i := 0; i < len(line); i++ {
		if line[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// WireToByteCol converts a column as the client counts it into a UTF-8 byte
// offset into line.
//
// Two rules keep this from corrupting text, both learned from other servers:
//
//   - A column past the end of the line clamps to the end rather than
//     erroring. Clients do send these (a diagnostic at end of a truncated
//     line, a cursor past the last character), and gopls treats it as a bug
//     to reject them.
//   - A column landing inside a multi-byte character snaps back to that
//     character's first byte. Go, unlike Rust, does not panic when a string
//     is sliced off a character boundary — it quietly yields mojibake — so
//     nothing downstream would catch this for us.
func WireToByteCol(line string, col int, enc PositionEncoding) int {
	if col <= 0 {
		return 0
	}
	if enc == EncodingUTF8 || isASCII(line) {
		if col >= len(line) {
			return len(line)
		}
		// Snap back to a boundary: a client that disagrees with us about the
		// encoding can otherwise point us into the middle of a character.
		for col > 0 && !utf8.RuneStart(line[col]) {
			col--
		}
		return col
	}

	units := 0
	for i, r := range line {
		n := runeUnits(r, enc)
		if units+n > col {
			// col is at, or inside, this character. Either way its start is
			// the only byte offset that keeps the string valid.
			return i
		}
		units += n
	}
	return len(line)
}

// ByteToWireCol converts a UTF-8 byte offset into line into a column as the
// client counts it. It is the inverse of WireToByteCol.
//
// This direction is the one that matters for correctness of edits: a range
// the server emits in the wrong unit makes the editor replace the wrong span
// of text, which silently corrupts the file on rename.
func ByteToWireCol(line string, b int, enc PositionEncoding) int {
	if b <= 0 {
		return 0
	}
	if b > len(line) {
		b = len(line)
	}
	if enc == EncodingUTF8 || isASCII(line) {
		return b
	}

	units := 0
	for i, r := range line {
		if i >= b {
			break
		}
		units += runeUnits(r, enc)
	}
	return units
}

// lineAt returns line n of lines, or "" when n is out of range. Conversion
// treats a missing line as empty rather than failing: a stale position from
// the client is not worth refusing a request over.
func lineAt(lines []string, n int) string {
	if n < 0 || n >= len(lines) {
		return ""
	}
	return lines[n]
}

// inCol converts a column as the client sent it into a byte offset into
// lines[n]. Every handler that reads Position.Character goes through here.
func (s *Server) inCol(lines []string, n int, ch uint32) int {
	if s.positionEncoding == EncodingUTF8 {
		// Dexter's native unit: nothing to do, and no line lookup either.
		return int(ch)
	}
	return WireToByteCol(lineAt(lines, n), int(ch), s.positionEncoding)
}

// outCol converts a byte offset into lines[n] into a column the client will
// read correctly.
//
// This is the direction that decides whether an edit lands on the right
// characters, so every Position the server emits must come from here, via
// outPos or outPosLine below.
func (s *Server) outCol(lines []string, n int, b int) uint32 {
	if s.positionEncoding == EncodingUTF8 {
		return uint32(b)
	}
	return uint32(ByteToWireCol(lineAt(lines, n), b, s.positionEncoding))
}

// outPos builds a Position on line n from a byte offset.
func (s *Server) outPos(lines []string, n, byteCol int) protocol.Position {
	return protocol.Position{Line: uint32(n), Character: s.outCol(lines, n, byteCol)}
}

// outPosLine builds a Position when only the one line's text is at hand,
// which is the case for results pointing into a file other than the one the
// request named (call hierarchy, cross-file rename edits).
func (s *Server) outPosLine(line string, n, byteCol int) protocol.Position {
	if s.positionEncoding == EncodingUTF8 {
		return protocol.Position{Line: uint32(n), Character: uint32(byteCol)}
	}
	return protocol.Position{
		Line:      uint32(n),
		Character: uint32(ByteToWireCol(line, byteCol, s.positionEncoding)),
	}
}
