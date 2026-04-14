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
