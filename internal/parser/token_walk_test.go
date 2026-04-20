package parser

import (
	"reflect"
	"testing"
)

func TestIsStatementBoundaryToken_IncludesTypeAndCallbackAttrs(t *testing.T) {
	if !IsStatementBoundaryToken(TokAttrType) {
		t.Fatal("expected TokAttrType to be a statement boundary")
	}
	if !IsStatementBoundaryToken(TokAttrCallback) {
		t.Fatal("expected TokAttrCallback to be a statement boundary")
	}
}

func TestTrackBlockDepth(t *testing.T) {
	depth := 0

	TrackBlockDepth(TokEnd, &depth)
	if depth != 0 {
		t.Fatalf("unexpected negative depth handling: got %d", depth)
	}

	TrackBlockDepth(TokDo, &depth)
	TrackBlockDepth(TokFn, &depth)
	if depth != 2 {
		t.Fatalf("expected depth 2 after do+fn, got %d", depth)
	}

	TrackBlockDepth(TokEnd, &depth)
	TrackBlockDepth(TokEnd, &depth)
	if depth != 0 {
		t.Fatalf("expected depth back to 0, got %d", depth)
	}
}

func TestAliasShortName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Foo", "Foo"},
		{"Foo.Bar", "Bar"},
		{"Foo.Bar.Baz", "Baz"},
	}

	for _, tt := range tests {
		if got := AliasShortName(tt.in); got != tt.want {
			t.Fatalf("AliasShortName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestScanForwardToBlockDo(t *testing.T) {
	// Split-line do should still be found.
	source := []byte("def foo(\n  x\n)\ndo\n  x\nend\n")
	tokens := Tokenize(source)
	n := len(tokens)

	defIdx := -1
	for i, tok := range tokens {
		if tok.Kind == TokDef {
			defIdx = i
			break
		}
	}
	if defIdx < 0 {
		t.Fatal("missing TokDef in test source")
	}

	doIdx, _, hasDo := ScanForwardToBlockDo(tokens, n, defIdx+1)
	if !hasDo || doIdx < 0 || tokens[doIdx].Kind != TokDo {
		t.Fatalf("expected split-line TokDo to be found, got hasDo=%v doIdx=%d", hasDo, doIdx)
	}
}

func TestScanForwardToBlockDo_StopsAtStatementBoundary(t *testing.T) {
	source := []byte("def foo, do: :ok\ndef bar do\n  :ok\nend\n")
	tokens := Tokenize(source)
	n := len(tokens)

	firstDef := -1
	for i, tok := range tokens {
		if tok.Kind == TokDef {
			firstDef = i
			break
		}
	}
	if firstDef < 0 {
		t.Fatal("missing first TokDef in test source")
	}

	_, nextPos, hasDo := ScanForwardToBlockDo(tokens, n, firstDef+1)
	if hasDo {
		t.Fatal("unexpected TokDo detected for inline do: form")
	}
	if nextPos >= n || tokens[nextPos].Kind != TokDef {
		t.Fatalf("expected scan to stop at next TokDef boundary, got nextPos=%d kind=%v", nextPos, tokens[nextPos].Kind)
	}
}

func TestScanKeywordOptionValue(t *testing.T) {
	source := []byte("alias Foo.Bar, as: Baz")
	tokens := Tokenize(source)
	n := len(tokens)

	aliasIdx := -1
	for i, tok := range tokens {
		if tok.Kind == TokAlias {
			aliasIdx = i
			break
		}
	}
	if aliasIdx < 0 {
		t.Fatal("missing TokAlias in test source")
	}

	j := NextSigToken(tokens, n, aliasIdx+1)
	_, k := CollectModuleName(source, tokens, n, j)

	value, _, ok := ScanKeywordOptionValue(source, tokens, n, k, "as")
	if !ok {
		t.Fatal("expected as: option to be detected")
	}
	if value != "Baz" {
		t.Fatalf("expected alias value Baz, got %q", value)
	}

	if _, _, ok := ScanKeywordOptionValue(source, tokens, n, k, "nope"); ok {
		t.Fatal("unexpected option match for invalid key")
	}
}

func TestScanMultiAliasChildren(t *testing.T) {
	source := []byte("alias Parent.{A, B, C}")
	tokens := Tokenize(source)
	n := len(tokens)

	aliasIdx := -1
	for i, tok := range tokens {
		if tok.Kind == TokAlias {
			aliasIdx = i
			break
		}
	}
	if aliasIdx < 0 {
		t.Fatal("missing TokAlias in test source")
	}

	j := NextSigToken(tokens, n, aliasIdx+1)
	_, k := CollectModuleName(source, tokens, n, j)

	children, _, ok := ScanMultiAliasChildren(source, tokens, n, k, false)
	if !ok {
		t.Fatal("expected multi-alias children to be detected")
	}
	want := []string{"A", "B", "C"}
	if !reflect.DeepEqual(children, want) {
		t.Fatalf("children mismatch: got %v, want %v", children, want)
	}
}

func TestScanMultiAliasChildren_StopAtStatement(t *testing.T) {
	source := []byte("alias Parent.{A, def foo, do: :ok}")
	tokens := Tokenize(source)
	n := len(tokens)

	aliasIdx := -1
	for i, tok := range tokens {
		if tok.Kind == TokAlias {
			aliasIdx = i
			break
		}
	}
	if aliasIdx < 0 {
		t.Fatal("missing TokAlias in test source")
	}

	j := NextSigToken(tokens, n, aliasIdx+1)
	_, k := CollectModuleName(source, tokens, n, j)

	children, nextPos, ok := ScanMultiAliasChildren(source, tokens, n, k, true)
	if !ok {
		t.Fatal("expected scan to return even for malformed multi-alias")
	}
	if len(children) != 1 || children[0] != "A" {
		t.Fatalf("expected only first child before statement boundary, got %v", children)
	}
	if nextPos >= n || tokens[nextPos].Kind != TokDef {
		t.Fatalf("expected nextPos at TokDef boundary, got nextPos=%d kind=%v", nextPos, tokens[nextPos].Kind)
	}
}

// =============================================================================
// TokenWalker tests
// =============================================================================

func TestTokenWalker_BasicIteration(t *testing.T) {
	source := []byte("def foo do\n  :ok\nend")
	tokens := Tokenize(source)
	w := NewTokenWalker(source, tokens)

	var kinds []TokenKind
	for w.More() {
		kinds = append(kinds, w.CurrentKind())
		w.Advance()
	}

	if len(kinds) == 0 {
		t.Fatal("expected some tokens")
	}
	if kinds[0] != TokDef {
		t.Errorf("first token: got %v, want TokDef", kinds[0])
	}
}

func TestTokenWalker_DepthTracking(t *testing.T) {
	source := []byte("foo(bar(x), [y])")
	tokens := Tokenize(source)
	w := NewTokenWalker(source, tokens)

	// Find positions manually
	maxDepth := 0
	for w.More() {
		if w.Depth() > maxDepth {
			maxDepth = w.Depth()
		}
		w.Advance()
	}

	// Depth should hit 2 for nested parens
	if maxDepth < 2 {
		t.Errorf("expected max depth >= 2, got %d", maxDepth)
	}

	// After full iteration, should be back to 0
	if w.Depth() != 0 {
		t.Errorf("final depth: got %d, want 0", w.Depth())
	}
}

func TestTokenWalker_BlockDepthTracking(t *testing.T) {
	source := []byte("def foo do\n  fn -> :ok end\nend")
	tokens := Tokenize(source)
	w := NewTokenWalker(source, tokens)

	maxBlockDepth := 0
	for w.More() {
		if w.BlockDepth() > maxBlockDepth {
			maxBlockDepth = w.BlockDepth()
		}
		w.Advance()
	}

	// Block depth should hit 2 (def do + fn)
	if maxBlockDepth != 2 {
		t.Errorf("expected max block depth 2, got %d", maxBlockDepth)
	}

	// After full iteration, should be back to 0
	if w.BlockDepth() != 0 {
		t.Errorf("final block depth: got %d, want 0", w.BlockDepth())
	}
}

func TestTokenWalker_NegativeDepthClamp(t *testing.T) {
	// Start mid-expression with unmatched closing bracket
	source := []byte(") + x")
	tokens := Tokenize(source)
	w := NewTokenWalker(source, tokens)

	for w.More() {
		w.Advance()
	}

	// Depth should never go negative
	if w.Depth() < 0 {
		t.Errorf("depth went negative: %d", w.Depth())
	}
}

func TestTokenWalker_SkipToEndOfStatement(t *testing.T) {
	source := []byte("foo(x,\n  y)\nbar()")
	tokens := Tokenize(source)
	w := NewTokenWalker(source, tokens)

	w.SkipToEndOfStatement()

	// Should stop at EOL after the first complete statement
	if w.CurrentKind() != TokEOL {
		t.Errorf("expected TokEOL, got %v", w.CurrentKind())
	}
}

func TestTokenWalker_EnsureProgress(t *testing.T) {
	source := []byte("foo bar")
	tokens := Tokenize(source)
	w := NewTokenWalker(source, tokens)

	prevPos := w.Pos()
	// Simulate a function that doesn't advance
	w.EnsureProgress(prevPos)

	if w.Pos() == prevPos {
		t.Error("EnsureProgress should have advanced position")
	}
}

func TestTokenWalker_CollectModuleName(t *testing.T) {
	source := []byte("Foo.Bar.Baz")
	tokens := Tokenize(source)
	w := NewTokenWalker(source, tokens)

	name := w.CollectModuleName()
	if name != "Foo.Bar.Baz" {
		t.Errorf("got %q, want Foo.Bar.Baz", name)
	}
}

func TestTokenWalker_ScanForBlockDo(t *testing.T) {
	t.Run("do on same line", func(t *testing.T) {
		source := []byte("defmodule Foo do")
		tokens := Tokenize(source)
		w := NewTokenWalker(source, tokens)
		w.Advance() // skip defmodule
		w.SkipToNextSig()
		w.CollectModuleName() // skip Foo

		if !w.ScanForBlockDo() {
			t.Error("expected to find do")
		}
		if w.BlockDepth() != 1 {
			t.Errorf("block depth: got %d, want 1", w.BlockDepth())
		}
	})

	t.Run("do on next line", func(t *testing.T) {
		source := []byte("defmodule Foo\ndo")
		tokens := Tokenize(source)
		w := NewTokenWalker(source, tokens)
		w.Advance() // skip defmodule
		w.SkipToNextSig()
		w.CollectModuleName() // skip Foo

		if !w.ScanForBlockDo() {
			t.Error("expected to find do on next line")
		}
	})

	t.Run("inline do: form", func(t *testing.T) {
		source := []byte("def foo, do: :ok\ndef bar do")
		tokens := Tokenize(source)
		w := NewTokenWalker(source, tokens)
		w.Advance() // skip def
		w.SkipToNextSig()

		// Should NOT find the do from the next def
		if w.ScanForBlockDo() {
			t.Error("should not find block do for inline do: form")
		}
	})
}

func TestTokenWalker_IsModuleDefiningToken(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{"defmodule Foo do end", true},
		{"defprotocol P do end", true},
		{"defimpl P, for: M do end", true},
		{"def foo do end", false},
		{"alias Foo", false},
	}

	for _, tt := range tests {
		tokens := Tokenize([]byte(tt.source))
		w := NewTokenWalker([]byte(tt.source), tokens)

		got := w.IsModuleDefiningToken()
		if got != tt.want {
			t.Errorf("%q: got %v, want %v", tt.source, got, tt.want)
		}
	}
}
