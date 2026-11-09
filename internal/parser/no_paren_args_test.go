package parser

import "testing"

func TestNoParenArgCount(t *testing.T) {
	tests := []struct {
		src  string
		want int
	}{
		{"foo a\n", 1},
		{"foo a, b\n", 2},
		{"foo :a, key: 1, other: 2\n", 2},
		{"foo key: 1\n", 1},
		{"foo [1, 2], %{a: 1}, {x, y}\n", 3},
		{"foo a,\n  b\n", 2},
		{"foo bar(1, 2), 3\n", 2},
		{"foo a do\n  1\nend\n", 2},
		{"foo \"s\" <> x\n", 1},
		{"foo fn x -> x end, 1\n", 2},
		{"foo @attr\n", 1},
		{"foo\n", -1},
		{"foo.bar\n", -1},
		{"foo[1]\n", -1},
		{"foo + 1\n", -1},
		{"foo in list\n", -1},
		{"foo(1)\n", -1},
		// A comma in a bitstring separates segments, not arguments.
		{"foo x, <<a, b>>\n", 2},
		{"foo <<1, 2>>\n", 1},
		{"foo <<< 1\n", -1},
		{"foo x >>> 1, y\n", 2},
		// A capture is an argument; a unary minus is not surely one.
		{"foo &bar/1\n", 1},
		{"foo &(&1 + 1), x\n", 2},
		{"foo && bar\n", -1},
		{"foo -1\n", -1},
	}
	for _, tt := range tests {
		toks := Tokenize([]byte(tt.src))
		if got := NoParenArgCount([]byte(tt.src), toks, 0); got != tt.want {
			t.Errorf("NoParenArgCount(%q) = %d, want %d", tt.src, got, tt.want)
		}
	}
}
