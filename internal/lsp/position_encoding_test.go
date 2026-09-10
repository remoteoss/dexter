package lsp

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWireToByteCol(t *testing.T) {
	// "🥺" is the bottom emoji from the rust-analyzer bug: 4 UTF-8 bytes,
	// 2 UTF-16 units, 1 code point. Every unit disagrees, which is the point.
	tests := []struct {
		name string
		line string
		col  int
		enc  PositionEncoding
		want int
	}{
		{"ascii utf-16 is identity", "def foo do", 4, EncodingUTF16, 4},
		{"ascii utf-8 is identity", "def foo do", 4, EncodingUTF8, 4},
		{"negative clamps to zero", "abc", -5, EncodingUTF16, 0},
		{"zero is zero", "abc", 0, EncodingUTF16, 0},

		// é is 2 bytes, 1 utf-16 unit -> drift of 1 per character.
		{"after e-acute", "\"Réunion\" => Money", 13, EncodingUTF16, 14},
		// ã é í: three 2-byte characters -> drift of 3.
		{"after three accents", "\"São Tomé and Príncipe\" => M", 26, EncodingUTF16, 29},
		// • is 3 bytes, 1 utf-16 unit -> drift of 2.
		{"after bullet", "• Money", 2, EncodingUTF16, 4},
		// 🥺 is 4 bytes, 2 utf-16 units -> drift of 2.
		{"after bottom emoji", "🥺 Money", 3, EncodingUTF16, 5},

		// utf-32 counts code points, so an astral char costs 1, not 2.
		{"utf-32 after emoji", "🥺 Money", 2, EncodingUTF32, 5},
		{"utf-32 after bullet", "• Money", 2, EncodingUTF32, 4},

		// Clamping: past the end of the line is legal input.
		{"past end clamps", "abc", 99, EncodingUTF16, 3},
		{"past end clamps utf-8", "abc", 99, EncodingUTF8, 3},
		{"past end with multibyte clamps", "é", 99, EncodingUTF16, 2},
		{"empty line", "", 5, EncodingUTF16, 0},

		// The corrupting case: a column landing inside a character must snap
		// back to its first byte, never into the middle of it.
		{"inside surrogate pair snaps to start", "🥺x", 1, EncodingUTF16, 0},
		{"utf-8 col inside emoji snaps back", "🥺x", 2, EncodingUTF8, 0},
		{"utf-8 col inside 2-byte char snaps back", "éx", 1, EncodingUTF8, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WireToByteCol(tt.line, tt.col, tt.enc)
			if got != tt.want {
				t.Errorf("WireToByteCol(%q, %d, %s) = %d, want %d", tt.line, tt.col, tt.enc, got, tt.want)
			}
			// Whatever we return must be a valid slice point, always.
			if got < 0 || got > len(tt.line) {
				t.Fatalf("offset %d out of range for %q", got, tt.line)
			}
			if got < len(tt.line) && !utf8.RuneStart(tt.line[got]) {
				t.Errorf("offset %d is not a character boundary in %q", got, tt.line)
			}
		})
	}
}

func TestByteToWireCol(t *testing.T) {
	tests := []struct {
		name string
		line string
		b    int
		enc  PositionEncoding
		want int
	}{
		{"ascii identity", "def foo", 4, EncodingUTF16, 4},
		{"utf-8 identity", "🥺 Money", 5, EncodingUTF8, 5},
		{"negative clamps", "abc", -1, EncodingUTF16, 0},
		{"after e-acute", "\"Réunion\" => Money", 14, EncodingUTF16, 13},
		{"after three accents", "\"São Tomé and Príncipe\" => M", 29, EncodingUTF16, 26},
		{"after bullet", "• Money", 4, EncodingUTF16, 2},
		{"after bottom emoji", "🥺 Money", 5, EncodingUTF16, 3},
		{"utf-32 after emoji", "🥺 Money", 5, EncodingUTF32, 2},
		{"past end clamps", "abc", 99, EncodingUTF16, 3},
		{"empty line", "", 3, EncodingUTF16, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ByteToWireCol(tt.line, tt.b, tt.enc); got != tt.want {
				t.Errorf("ByteToWireCol(%q, %d, %s) = %d, want %d", tt.line, tt.b, tt.enc, got, tt.want)
			}
		})
	}
}

// TestPositionEncodingRoundTrip is the property that makes edits safe: a byte
// offset sent to the client and read back must land where it started.
// Rename correctness depends on it directly.
func TestPositionEncodingRoundTrip(t *testing.T) {
	lines := []string{
		"def render(conn, params) do",
		"\"São Tomé and Príncipe\" => Money.new(:EUR, \"19.95\")",
		"• *Payroll run:* #{SlackHelpers.link(url)}",
		"🥺🥺🥺 def foo do",
		"",
		"é",
		strings.Repeat("ü", 50) + " Money.new()",
		"mixed 🥺 é • ascii tail",
	}
	for _, enc := range []PositionEncoding{EncodingUTF8, EncodingUTF16, EncodingUTF32} {
		for _, line := range lines {
			// Every character boundary must survive a round trip.
			for b := range line {
				if !utf8.RuneStart(line[b]) {
					continue
				}
				wire := ByteToWireCol(line, b, enc)
				back := WireToByteCol(line, wire, enc)
				if back != b {
					t.Errorf("enc=%s line=%q: byte %d -> wire %d -> byte %d", enc, line, b, wire, back)
				}
			}
			// The end of the line is a valid position too.
			wire := ByteToWireCol(line, len(line), enc)
			if back := WireToByteCol(line, wire, enc); back != len(line) {
				t.Errorf("enc=%s line=%q: end %d -> wire %d -> byte %d", enc, line, len(line), wire, back)
			}
		}
	}
}

// TestWireToByteColNeverSplitsCharacter feeds every column a hostile or
// out-of-sync client could send and asserts we never hand back an offset that
// would slice a character in half. Go yields mojibake rather than panicking
// there, so this is the only thing standing between a bad column and a
// corrupted buffer.
func TestWireToByteColNeverSplitsCharacter(t *testing.T) {
	lines := []string{"🥺 Money", "é • 🥺 x", "São Tomé", "plain ascii"}
	for _, enc := range []PositionEncoding{EncodingUTF8, EncodingUTF16, EncodingUTF32} {
		for _, line := range lines {
			for col := -3; col < len(line)+5; col++ {
				got := WireToByteCol(line, col, enc)
				if got < 0 || got > len(line) {
					t.Fatalf("enc=%s line=%q col=%d: offset %d out of range", enc, line, col, got)
				}
				if got < len(line) && !utf8.RuneStart(line[got]) {
					t.Errorf("enc=%s line=%q col=%d: offset %d splits a character", enc, line, col, got)
				}
			}
		}
	}
}
