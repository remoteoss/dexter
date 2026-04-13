package parser

import (
	"fmt"
	"testing"
)

// tokenizeNoEOF runs Tokenize and strips the trailing TokEOF for cleaner assertions.
func tokenizeNoEOF(source string) []Token {
	tokens := Tokenize([]byte(source))
	if len(tokens) > 0 && tokens[len(tokens)-1].Kind == TokEOF {
		return tokens[:len(tokens)-1]
	}
	return tokens
}

// kindNames maps TokenKind to a human-readable name for test output.
var kindNames = map[TokenKind]string{
	TokDefmodule:     "TokDefmodule",
	TokDef:           "TokDef",
	TokDefp:          "TokDefp",
	TokDefmacro:      "TokDefmacro",
	TokDefmacrop:     "TokDefmacrop",
	TokDefguard:      "TokDefguard",
	TokDefguardp:     "TokDefguardp",
	TokDefdelegate:   "TokDefdelegate",
	TokDefprotocol:   "TokDefprotocol",
	TokDefimpl:       "TokDefimpl",
	TokDefstruct:     "TokDefstruct",
	TokDefexception:  "TokDefexception",
	TokAlias:         "TokAlias",
	TokImport:        "TokImport",
	TokUse:           "TokUse",
	TokRequire:       "TokRequire",
	TokDo:            "TokDo",
	TokEnd:           "TokEnd",
	TokFn:            "TokFn",
	TokWhen:          "TokWhen",
	TokIdent:         "TokIdent",
	TokModule:        "TokModule",
	TokAttr:          "TokAttr",
	TokAttrDoc:       "TokAttrDoc",
	TokAttrSpec:      "TokAttrSpec",
	TokAttrType:      "TokAttrType",
	TokAttrBehaviour: "TokAttrBehaviour",
	TokAttrCallback:  "TokAttrCallback",
	TokString:        "TokString",
	TokHeredoc:       "TokHeredoc",
	TokSigil:         "TokSigil",
	TokCharLiteral:   "TokCharLiteral",
	TokAtom:          "TokAtom",
	TokDot:           "TokDot",
	TokComma:         "TokComma",
	TokColon:         "TokColon",
	TokOpenParen:     "TokOpenParen",
	TokCloseParen:    "TokCloseParen",
	TokOpenBracket:   "TokOpenBracket",
	TokCloseBracket:  "TokCloseBracket",
	TokOpenBrace:     "TokOpenBrace",
	TokCloseBrace:    "TokCloseBrace",
	TokOpenAngle:     "TokOpenAngle",
	TokCloseAngle:    "TokCloseAngle",
	TokPipe:          "TokPipe",
	TokBackslash:     "TokBackslash",
	TokRightArrow:    "TokRightArrow",
	TokLeftArrow:     "TokLeftArrow",
	TokAssoc:         "TokAssoc",
	TokDoubleColon:   "TokDoubleColon",
	TokPercent:       "TokPercent",
	TokNumber:        "TokNumber",
	TokComment:       "TokComment",
	TokEOL:           "TokEOL",
	TokEOF:           "TokEOF",
	TokOther:         "TokOther",
}

func kindName(k TokenKind) string {
	if name, ok := kindNames[k]; ok {
		return name
	}
	return fmt.Sprintf("Token(%d)", int(k))
}

// assertKinds checks that the tokens produced have exactly the given kinds (ignoring EOL).
func assertKinds(t *testing.T, source string, expected []TokenKind) {
	t.Helper()
	tokens := tokenizeNoEOF(source)
	// Filter out EOL tokens for easier assertions unless EOL is in expected
	wantEOL := false
	for _, k := range expected {
		if k == TokEOL {
			wantEOL = true
			break
		}
	}
	var got []TokenKind
	for _, tok := range tokens {
		if tok.Kind == TokEOL && !wantEOL {
			continue
		}
		got = append(got, tok.Kind)
	}
	if len(got) != len(expected) {
		t.Errorf("source %q: got %d tokens, want %d", source, len(got), len(expected))
		t.Logf("  got:  %v", kindSlice(got))
		t.Logf("  want: %v", kindSlice(expected))
		return
	}
	for i, k := range expected {
		if got[i] != k {
			t.Errorf("source %q: token[%d] = %s, want %s", source, i, kindName(got[i]), kindName(k))
		}
	}
}

func kindSlice(kinds []TokenKind) []string {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = kindName(k)
	}
	return names
}

// assertText checks that a specific token (by index, excluding EOL) has the given text.
func assertText(t *testing.T, source string, index int, expected string) {
	t.Helper()
	tokens := tokenizeNoEOF(source)
	var filtered []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			filtered = append(filtered, tok)
		}
	}
	if index >= len(filtered) {
		t.Errorf("source %q: token index %d out of range (have %d tokens)", source, index, len(filtered))
		return
	}
	got := string([]byte(source)[filtered[index].Start:filtered[index].End])
	if got != expected {
		t.Errorf("source %q: token[%d] text = %q, want %q", source, index, got, expected)
	}
}

// TestTokenize_EmptySource verifies that an empty input produces just TokEOF.
func TestTokenize_EmptySource(t *testing.T) {
	tokens := Tokenize([]byte(""))
	if len(tokens) != 1 || tokens[0].Kind != TokEOF {
		t.Errorf("empty source: expected [TokEOF], got %v", tokens)
	}
}

// TestTokenize_BasicKeywords verifies that `defmodule Foo do` produces correct tokens.
func TestTokenize_BasicKeywords(t *testing.T) {
	assertKinds(t, "defmodule Foo do", []TokenKind{TokDefmodule, TokModule, TokDo})
	assertText(t, "defmodule Foo do", 1, "Foo")
}

// TestTokenize_IdentVsKeyword verifies that `define` is TokIdent, not TokDef.
func TestTokenize_IdentVsKeyword(t *testing.T) {
	assertKinds(t, "define", []TokenKind{TokIdent})
	assertText(t, "define", 0, "define")
	assertKinds(t, "def", []TokenKind{TokDef})
	assertKinds(t, "defmodule_helper", []TokenKind{TokIdent})
}

// TestTokenize_AllDefKeywords verifies all def-family keywords are recognized.
func TestTokenize_AllDefKeywords(t *testing.T) {
	cases := []struct {
		source string
		kind   TokenKind
	}{
		{"defmodule ", TokDefmodule},
		{"def ", TokDef},
		{"defp ", TokDefp},
		{"defmacro ", TokDefmacro},
		{"defmacrop ", TokDefmacrop},
		{"defguard ", TokDefguard},
		{"defguardp ", TokDefguardp},
		{"defdelegate ", TokDefdelegate},
		{"defprotocol ", TokDefprotocol},
		{"defimpl ", TokDefimpl},
		{"defstruct ", TokDefstruct},
		{"defexception ", TokDefexception},
		{"alias ", TokAlias},
		{"import ", TokImport},
		{"use ", TokUse},
		{"require ", TokRequire},
		{"do\n", TokDo},
		{"end", TokEnd},
		{"fn ", TokFn},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			tokens := Tokenize([]byte(tc.source))
			if tokens[0].Kind != tc.kind {
				t.Errorf("source %q: first token = %s, want %s", tc.source, kindName(tokens[0].Kind), kindName(tc.kind))
			}
		})
	}
}

// TestTokenize_String verifies that "hello" produces TokString.
func TestTokenize_String(t *testing.T) {
	assertKinds(t, `"hello"`, []TokenKind{TokString})
	assertText(t, `"hello"`, 0, `"hello"`)
}

// TestTokenize_StringWithInterpolation verifies that the whole interpolated string is one TokString.
func TestTokenize_StringWithInterpolation(t *testing.T) {
	source := `"hello #{World.name}"`
	assertKinds(t, source, []TokenKind{TokString})
	assertText(t, source, 0, source)
}

// TestTokenize_StringNestedInterpolation verifies nested strings inside interpolation are handled.
func TestTokenize_StringNestedInterpolation(t *testing.T) {
	source := `"#{foo("arg")}"`
	assertKinds(t, source, []TokenKind{TokString})
	assertText(t, source, 0, source)
}

// TestTokenize_CharLiteralQuote verifies that ?" is TokCharLiteral, not TokString.
func TestTokenize_CharLiteralQuote(t *testing.T) {
	assertKinds(t, `?"`, []TokenKind{TokCharLiteral})
	assertText(t, `?"`, 0, `?"`)
}

// TestTokenize_CharLiteralSingleQuote verifies that ?' is TokCharLiteral, not TokString/charlist.
func TestTokenize_CharLiteralSingleQuote(t *testing.T) {
	assertKinds(t, `?'`, []TokenKind{TokCharLiteral})
	assertText(t, `?'`, 0, `?'`)
}

// TestTokenize_CharLiteralHash verifies that ?# is TokCharLiteral, not a comment.
func TestTokenize_CharLiteralHash(t *testing.T) {
	assertKinds(t, `?#`, []TokenKind{TokCharLiteral})
	assertText(t, `?#`, 0, `?#`)
}

// TestTokenize_CharLiteralEscape verifies backslash escape char literals.
func TestTokenize_CharLiteralEscape(t *testing.T) {
	assertKinds(t, `?\n`, []TokenKind{TokCharLiteral})
	assertText(t, `?\n`, 0, `?\n`)
	assertKinds(t, `?\\`, []TokenKind{TokCharLiteral})
}

// TestTokenize_Atom verifies that :foo produces TokAtom.
func TestTokenize_Atom(t *testing.T) {
	assertKinds(t, ":foo", []TokenKind{TokAtom})
	assertText(t, ":foo", 0, ":foo")
}

// TestTokenize_AtomQuoted verifies that :"hello" produces TokAtom.
func TestTokenize_AtomQuoted(t *testing.T) {
	assertKinds(t, `:"hello world"`, []TokenKind{TokAtom})
	assertText(t, `:"hello world"`, 0, `:"hello world"`)
}

// TestTokenize_DoubleColon verifies that :: is TokDoubleColon, not an atom.
func TestTokenize_DoubleColon(t *testing.T) {
	assertKinds(t, "::", []TokenKind{TokDoubleColon})
}

// TestTokenize_KeywordKey verifies that `as: Foo` is TokIdent + TokColon + TokModule (not atom).
func TestTokenize_KeywordKey(t *testing.T) {
	assertKinds(t, "as: Foo", []TokenKind{TokIdent, TokColon, TokModule})
	assertText(t, "as: Foo", 0, "as")
}

// TestTokenize_Sigil verifies that a sigil produces a single TokSigil and no tokens inside.
func TestTokenize_Sigil(t *testing.T) {
	assertKinds(t, "~s(alias Fake.Module)", []TokenKind{TokSigil})
}

// TestTokenize_SigilBracketNested verifies nested brackets inside sigil are handled.
func TestTokenize_SigilBracketNested(t *testing.T) {
	assertKinds(t, "~s(foo (bar) baz)", []TokenKind{TokSigil})
}

// TestTokenize_SigilModifier verifies sigil with trailing modifier letters.
func TestTokenize_SigilModifier(t *testing.T) {
	assertKinds(t, "~r/foo/i", []TokenKind{TokSigil})
	assertText(t, "~r/foo/i", 0, "~r/foo/i")
}

// TestTokenize_SigilHeredoc verifies that a heredoc sigil produces TokSigil (not TokHeredoc).
func TestTokenize_SigilHeredoc(t *testing.T) {
	source := "~s\"\"\"\nhello\n\"\"\""
	assertKinds(t, source, []TokenKind{TokSigil})
}

// TestTokenize_Heredoc verifies that """ heredocs produce TokHeredoc.
func TestTokenize_Heredoc(t *testing.T) {
	source := "\"\"\"\nhello\n\"\"\""
	assertKinds(t, source, []TokenKind{TokHeredoc})
}

// TestTokenize_HeredocSingleQuote verifies ”' heredocs produce TokHeredoc.
func TestTokenize_HeredocSingleQuote(t *testing.T) {
	source := "'''\nhello\n'''"
	assertKinds(t, source, []TokenKind{TokHeredoc})
}

// TestTokenize_ModuleName verifies `MyApp.Accounts` produces TokModule, TokDot, TokModule.
func TestTokenize_ModuleName(t *testing.T) {
	assertKinds(t, "MyApp.Accounts", []TokenKind{TokModule, TokDot, TokModule})
	assertText(t, "MyApp.Accounts", 0, "MyApp")
	assertText(t, "MyApp.Accounts", 2, "Accounts")
}

// TestTokenize_Comment verifies that # starts a comment and no keywords are recognized inside.
func TestTokenize_Comment(t *testing.T) {
	assertKinds(t, "# defmodule Foo", []TokenKind{TokComment})
}

// TestTokenize_Attribute verifies that @doc produces TokAttrDoc.
func TestTokenize_Attribute(t *testing.T) {
	assertKinds(t, "@doc", []TokenKind{TokAttrDoc})
	assertText(t, "@doc", 0, "@doc")
}

// TestTokenize_AttrOther verifies that @ not followed by identifier is TokOther.
func TestTokenize_AttrOther(t *testing.T) {
	assertKinds(t, "@ ", []TokenKind{TokOther})
}

// TestTokenize_SpecLine verifies the token kinds for a @spec line.
func TestTokenize_SpecLine(t *testing.T) {
	source := "@spec foo(String.t()) :: {:ok, User.t()}"
	tokens := tokenizeNoEOF(source)
	var kinds []TokenKind
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			kinds = append(kinds, tok.Kind)
		}
	}
	expected := []TokenKind{
		TokAttrSpec,    // @spec
		TokIdent,       // foo
		TokOpenParen,   // (
		TokModule,      // String
		TokDot,         // .
		TokIdent,       // t
		TokOpenParen,   // (
		TokCloseParen,  // )
		TokCloseParen,  // )
		TokDoubleColon, // ::
		TokOpenBrace,   // {
		TokAtom,        // :ok
		TokComma,       // ,
		TokModule,      // User
		TokDot,         // .
		TokIdent,       // t
		TokOpenParen,   // (
		TokCloseParen,  // )
		TokCloseBrace,  // }
	}
	if len(kinds) != len(expected) {
		t.Errorf("spec line: got %d tokens, want %d", len(kinds), len(expected))
		t.Logf("  got:  %v", kindSlice(kinds))
		t.Logf("  want: %v", kindSlice(expected))
		return
	}
	for i, k := range expected {
		if kinds[i] != k {
			t.Errorf("spec line: token[%d] = %s, want %s", i, kindName(kinds[i]), kindName(k))
		}
	}
}

// TestTokenize_LineNumbers verifies that newlines increment line numbers correctly.
func TestTokenize_LineNumbers(t *testing.T) {
	source := "foo\nbar\nbaz"
	tokens := Tokenize([]byte(source))
	// foo is on line 1, bar on line 2, baz on line 3
	var idents []Token
	for _, tok := range tokens {
		if tok.Kind == TokIdent {
			idents = append(idents, tok)
		}
	}
	if len(idents) != 3 {
		t.Fatalf("expected 3 idents, got %d", len(idents))
	}
	if idents[0].Line != 1 {
		t.Errorf("foo: line = %d, want 1", idents[0].Line)
	}
	if idents[1].Line != 2 {
		t.Errorf("bar: line = %d, want 2", idents[1].Line)
	}
	if idents[2].Line != 3 {
		t.Errorf("baz: line = %d, want 3", idents[2].Line)
	}
}

// TestTokenize_MultilineString verifies that line numbers are tracked inside multi-line strings.
func TestTokenize_MultilineString(t *testing.T) {
	source := "\"line1\nline2\"\nafter"
	tokens := Tokenize([]byte(source))
	var afterTok Token
	for _, tok := range tokens {
		if tok.Kind == TokIdent {
			afterTok = tok
		}
	}
	if afterTok.Line != 3 {
		t.Errorf("'after' token: line = %d, want 3", afterTok.Line)
	}
}

// TestTokenize_ModuleSpecial verifies that __MODULE__ is TokModule.
func TestTokenize_ModuleSpecial(t *testing.T) {
	assertKinds(t, "__MODULE__", []TokenKind{TokModule})
	assertText(t, "__MODULE__", 0, "__MODULE__")
}

// TestTokenize_Structural verifies structural tokens: <<, >>, |>, \\.
func TestTokenize_Structural(t *testing.T) {
	assertKinds(t, "<<", []TokenKind{TokOpenAngle})
	assertKinds(t, ">>", []TokenKind{TokCloseAngle})
	assertKinds(t, "|>", []TokenKind{TokPipe})
	assertKinds(t, `\\`, []TokenKind{TokBackslash})
	assertKinds(t, "->", []TokenKind{TokRightArrow})
	assertKinds(t, "=>", []TokenKind{TokAssoc})
}

// TestTokenize_Dots verifies that . is TokDot, .. and ... are TokOther.
func TestTokenize_Dots(t *testing.T) {
	assertKinds(t, ".", []TokenKind{TokDot})
	assertKinds(t, "..", []TokenKind{TokOther})
	assertKinds(t, "...", []TokenKind{TokOther})
}

// TestTokenize_KeywordInsideString verifies that keywords inside strings are not emitted.
func TestTokenize_KeywordInsideString(t *testing.T) {
	assertKinds(t, `"defmodule"`, []TokenKind{TokString})
}

// TestTokenize_KeywordInsideComment verifies that keywords inside comments are not emitted.
func TestTokenize_KeywordInsideComment(t *testing.T) {
	assertKinds(t, "# defmodule Foo", []TokenKind{TokComment})
}

// TestTokenize_KeywordFollowedByIdentChar verifies keywords need word boundary.
func TestTokenize_KeywordFollowedByIdentChar(t *testing.T) {
	// defp followed by _ shouldn't produce TokDefp
	assertKinds(t, "defp_helper", []TokenKind{TokIdent})
	// def followed by 2 shouldn't produce TokDef
	assertKinds(t, "def2", []TokenKind{TokIdent})
}

// TestTokenize_DefWithParens verifies def followed by ( is a keyword.
func TestTokenize_DefWithParens(t *testing.T) {
	assertKinds(t, "def(", []TokenKind{TokDef, TokOpenParen})
}

// TestTokenize_FullModule verifies a realistic module header parses correctly.
func TestTokenize_FullModule(t *testing.T) {
	source := "defmodule MyApp.Accounts do\n  @moduledoc false\nend"
	tokens := tokenizeNoEOF(source)
	var kinds []TokenKind
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			kinds = append(kinds, tok.Kind)
		}
	}
	expected := []TokenKind{
		TokDefmodule,
		TokModule, TokDot, TokModule,
		TokDo,
		TokAttrDoc, // @moduledoc
		TokIdent,   // false — not a keyword, just an identifier
		TokEnd,
	}
	if len(kinds) != len(expected) {
		t.Errorf("full module: got %d tokens, want %d", len(kinds), len(expected))
		t.Logf("  got:  %v", kindSlice(kinds))
		t.Logf("  want: %v", kindSlice(expected))
		return
	}
	for i, k := range expected {
		if kinds[i] != k {
			t.Errorf("full module: token[%d] = %s, want %s", i, kindName(kinds[i]), kindName(k))
		}
	}
}

// TestTokenize_Charlist verifies single-quoted charlists are TokString.
func TestTokenize_Charlist(t *testing.T) {
	assertKinds(t, `'hello'`, []TokenKind{TokString})
}

// TestTokenize_SigilWithAngleBrackets verifies angle bracket sigils.
func TestTokenize_SigilWithAngleBrackets(t *testing.T) {
	assertKinds(t, "~s<hello world>", []TokenKind{TokSigil})
}

// TestTokenize_SigilWithSquareBrackets verifies square bracket sigils.
func TestTokenize_SigilWithSquareBrackets(t *testing.T) {
	assertKinds(t, "~w[foo bar baz]", []TokenKind{TokSigil})
}

// TestTokenize_AtomWithBang verifies atoms with ! work correctly.
func TestTokenize_AtomWithBang(t *testing.T) {
	assertKinds(t, ":ok!", []TokenKind{TokAtom})
	assertText(t, ":ok!", 0, ":ok!")
}

// TestTokenize_BinaryPattern verifies << and >> tokens in binary syntax.
func TestTokenize_BinaryPattern(t *testing.T) {
	assertKinds(t, "<<foo>>", []TokenKind{TokOpenAngle, TokIdent, TokCloseAngle})
}

// TestTokenize_PipeChain verifies |> produces TokPipe.
func TestTokenize_PipeChain(t *testing.T) {
	assertKinds(t, "foo |> bar", []TokenKind{TokIdent, TokPipe, TokIdent})
}

// TestTokenize_DefaultParam verifies \\ produces TokBackslash.
func TestTokenize_DefaultParam(t *testing.T) {
	assertKinds(t, `def foo(x \\ 0)`, []TokenKind{TokDef, TokIdent, TokOpenParen, TokIdent, TokBackslash, TokNumber, TokCloseParen})
}

// TestTokenize_HeredocLineNumbers verifies that line numbers are tracked inside heredocs.
func TestTokenize_HeredocLineNumbers(t *testing.T) {
	source := "\"\"\"\nline1\nline2\n\"\"\"\nafter"
	tokens := Tokenize([]byte(source))
	var afterTok Token
	for _, tok := range tokens {
		if tok.Kind == TokIdent && string([]byte(source)[tok.Start:tok.End]) == "after" {
			afterTok = tok
		}
	}
	if afterTok.Line != 5 {
		t.Errorf("'after' token: line = %d, want 5", afterTok.Line)
	}
}

// TestTokenize_SigilHeredocLineNumbers verifies line numbers after a sigil heredoc.
func TestTokenize_SigilHeredocLineNumbers(t *testing.T) {
	source := "~s\"\"\"\nhello\nworld\n\"\"\"\nafter"
	tokens := Tokenize([]byte(source))
	var afterTok Token
	for _, tok := range tokens {
		if tok.Kind == TokIdent && string([]byte(source)[tok.Start:tok.End]) == "after" {
			afterTok = tok
		}
	}
	if afterTok.Line != 5 {
		t.Errorf("'after' token after sigil heredoc: line = %d, want 5", afterTok.Line)
	}
}

// --- Edge cases from Elixir tokenizer cross-check ---

func TestTokenize_NestedInterpolation(t *testing.T) {
	// "This is a #{var("#{that}", here)}" → single TokString, then the code after is parsed correctly
	source := `"This is #{var("#{that}", here)}" + x`
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	// Should be: TokString, TokOther(+), TokIdent(x)
	if len(nonEOL) < 3 {
		t.Fatalf("expected at least 3 non-EOL tokens, got %d", len(nonEOL))
	}
	if nonEOL[0].Kind != TokString {
		t.Errorf("token[0] = %s, want TokString", kindName(nonEOL[0].Kind))
	}
	// The last token should be the identifier "x"
	last := nonEOL[len(nonEOL)-1]
	if last.Kind != TokIdent || string([]byte(source)[last.Start:last.End]) != "x" {
		t.Errorf("last token = %s %q, want TokIdent \"x\"", kindName(last.Kind), string([]byte(source)[last.Start:last.End]))
	}
}

func TestTokenize_CharLiteralCloseBraceInInterpolation(t *testing.T) {
	// "#{?}}" — ?} is a char literal, the second } closes the interpolation
	source := `"#{?}}" <> rest`
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	// Should be: TokString("#{?}}"), TokOther(<>), TokIdent(rest)
	if len(nonEOL) < 2 {
		t.Fatalf("expected at least 2 tokens, got %d", len(nonEOL))
	}
	if nonEOL[0].Kind != TokString {
		t.Errorf("token[0] = %s, want TokString", kindName(nonEOL[0].Kind))
	}
	lastTok := nonEOL[len(nonEOL)-1]
	if lastTok.Kind != TokIdent || string([]byte(source)[lastTok.Start:lastTok.End]) != "rest" {
		t.Errorf("last token = %s %q, want TokIdent \"rest\"", kindName(lastTok.Kind), string([]byte(source)[lastTok.Start:lastTok.End]))
	}
}

func TestTokenize_SigilInsideInterpolation(t *testing.T) {
	// "#{~r/pat}tern/}" — the } inside the regex is part of the sigil, not interpolation
	source := `"#{~r/pat}tern/}" <> rest`
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) < 2 {
		t.Fatalf("expected at least 2 tokens, got %d", len(nonEOL))
	}
	if nonEOL[0].Kind != TokString {
		t.Errorf("token[0] = %s, want TokString", kindName(nonEOL[0].Kind))
	}
	lastTok := nonEOL[len(nonEOL)-1]
	if lastTok.Kind != TokIdent || string([]byte(source)[lastTok.Start:lastTok.End]) != "rest" {
		t.Errorf("last token = %s %q, want TokIdent \"rest\"", kindName(lastTok.Kind), string([]byte(source)[lastTok.Start:lastTok.End]))
	}
}

func TestTokenize_UppercaseSigilNoEscapes(t *testing.T) {
	// ~S"contains \" stuff" — uppercase sigil, backslash is NOT escape.
	// The \" is literal \ then " which closes the sigil.
	// So ~S"contains \" is the sigil, then stuff" is separate tokens.
	source := `~S"contains \" stuff`
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) < 1 {
		t.Fatal("expected at least 1 token")
	}
	if nonEOL[0].Kind != TokSigil {
		t.Errorf("token[0] = %s, want TokSigil", kindName(nonEOL[0].Kind))
	}
	// The sigil should end at the first unescaped " (which is the \" — no escape in uppercase sigil)
	sigilText := string([]byte(source)[nonEOL[0].Start:nonEOL[0].End])
	expected := `~S"contains \"`
	if sigilText != expected {
		t.Errorf("sigil text = %q, want %q", sigilText, expected)
	}
}

func TestTokenize_LowercaseSigilEscapes(t *testing.T) {
	// ~s"contains \" stuff" — lowercase sigil, backslash IS escape.
	source := `~s"contains \" stuff"`
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) != 1 {
		t.Fatalf("expected 1 token, got %d: %v", len(nonEOL), kindSlice(kindsOf(nonEOL)))
	}
	if nonEOL[0].Kind != TokSigil {
		t.Errorf("token[0] = %s, want TokSigil", kindName(nonEOL[0].Kind))
	}
	sigilText := string([]byte(source)[nonEOL[0].Start:nonEOL[0].End])
	if sigilText != source {
		t.Errorf("sigil text = %q, want %q", sigilText, source)
	}
}

// --- Broken/incomplete code tests (LSP context: code is frequently mid-edit) ---

func TestTokenize_UnterminatedString(t *testing.T) {
	// User is mid-edit, string not closed
	source := "def foo do\n  x = \"hello\nend"
	tokens := Tokenize([]byte(source))
	// Must not panic. Should produce tokens and end with TokEOF.
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
}

func TestTokenize_UnterminatedHeredoc(t *testing.T) {
	source := "def foo do\n  @doc \"\"\"\n  some docs\n"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
}

func TestTokenize_UnterminatedSigil(t *testing.T) {
	source := "~r(pattern"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
	if tokens[0].Kind != TokSigil {
		t.Errorf("token[0] = %s, want TokSigil", kindName(tokens[0].Kind))
	}
}

func TestTokenize_UnterminatedInterpolation(t *testing.T) {
	// String with unclosed interpolation: "hello #{world
	source := "\"hello #{world\nend"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
}

func TestTokenize_TrailingBackslashInString(t *testing.T) {
	// String ending with backslash at EOF: "hello\
	source := "\"hello\\"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
}

func TestTokenize_LoneQuestion(t *testing.T) {
	// ? at end of file
	source := "?"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
	// Should emit TokCharLiteral (even if malformed)
	if tokens[0].Kind != TokCharLiteral {
		t.Errorf("token[0] = %s, want TokCharLiteral", kindName(tokens[0].Kind))
	}
}

func TestTokenize_LoneTilde(t *testing.T) {
	// ~ at end of file
	source := "~"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
}

func TestTokenize_TildeLetterNoDelimiter(t *testing.T) {
	// ~r at end of file (no delimiter)
	source := "~r"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
}

func TestTokenize_EmptyString(t *testing.T) {
	source := `""`
	assertKinds(t, source, []TokenKind{TokString})
}

func TestTokenize_PartialDefmodule(t *testing.T) {
	// User is typing "defmo" — not a full keyword yet
	source := "defmo"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) != 1 || nonEOL[0].Kind != TokIdent {
		t.Errorf("partial keyword 'defmo' should be TokIdent, got %v", kindSlice(kindsOf(nonEOL)))
	}
}

func TestTokenize_CodeAfterUnterminatedSigil(t *testing.T) {
	// Even if a sigil eats the rest of the file, we must not panic or loop forever
	source := "~s(unclosed sigil\ndefmodule Foo do\nend"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
}

func TestTokenize_AtSignAtEOF(t *testing.T) {
	source := "@"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
}

func TestTokenize_ColonAtEOF(t *testing.T) {
	source := ":"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF at end")
	}
	if tokens[0].Kind != TokColon {
		t.Errorf("token[0] = %s, want TokColon", kindName(tokens[0].Kind))
	}
}

func TestTokenize_UppercaseSigilHeredocNoEscapes(t *testing.T) {
	// ~S""" should not process escapes
	source := "~S\"\"\"\ncontains \\\" stuff\n\"\"\"\nafter"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	// Should be: TokSigil (the whole heredoc), TokIdent("after")
	if len(nonEOL) < 2 {
		t.Fatalf("expected at least 2 tokens, got %d", len(nonEOL))
	}
	if nonEOL[0].Kind != TokSigil {
		t.Errorf("token[0] = %s, want TokSigil", kindName(nonEOL[0].Kind))
	}
	lastTok := nonEOL[len(nonEOL)-1]
	if lastTok.Kind != TokIdent || string([]byte(source)[lastTok.Start:lastTok.End]) != "after" {
		t.Errorf("last token = %s %q, want TokIdent \"after\"", kindName(lastTok.Kind), string([]byte(source)[lastTok.Start:lastTok.End]))
	}
}

// --- Edge cases from Elixir tokenizer test suite cross-check ---

func TestTokenize_EscapedInterpolation(t *testing.T) {
	// \#{ inside a string is NOT interpolation — it's a literal #{
	source := `"hello \#{world}" <> rest`
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	// Should be: TokString("hello \#{world}"), TokOther(<), TokOther(>), TokIdent(rest)
	if nonEOL[0].Kind != TokString {
		t.Errorf("token[0] = %s, want TokString", kindName(nonEOL[0].Kind))
	}
	lastTok := nonEOL[len(nonEOL)-1]
	if lastTok.Kind != TokIdent || string([]byte(source)[lastTok.Start:lastTok.End]) != "rest" {
		t.Errorf("last token = %s %q, want TokIdent \"rest\"", kindName(lastTok.Kind), string([]byte(source)[lastTok.Start:lastTok.End]))
	}
}

func TestTokenize_SigilEscapedDelimiter(t *testing.T) {
	// ~s(f\(oo) — escaped paren inside sigil is literal, not nesting
	source := `~s(f\(oo) + x`
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if nonEOL[0].Kind != TokSigil {
		t.Errorf("token[0] = %s, want TokSigil", kindName(nonEOL[0].Kind))
	}
	sigilText := string([]byte(source)[nonEOL[0].Start:nonEOL[0].End])
	if sigilText != `~s(f\(oo)` {
		t.Errorf("sigil text = %q, want %q", sigilText, `~s(f\(oo)`)
	}
	lastTok := nonEOL[len(nonEOL)-1]
	if lastTok.Kind != TokIdent || string([]byte(source)[lastTok.Start:lastTok.End]) != "x" {
		t.Errorf("last token = %s %q, want TokIdent \"x\"", kindName(lastTok.Kind), string([]byte(source)[lastTok.Start:lastTok.End]))
	}
}

func TestTokenize_DotAfterNewline(t *testing.T) {
	// Dot on next line is a valid remote call continuation
	source := "Foo\n.bar"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	// TokModule("Foo"), TokDot, TokIdent("bar")
	if len(nonEOL) != 3 {
		t.Fatalf("expected 3 tokens, got %d: %v", len(nonEOL), kindSlice(kindsOf(nonEOL)))
	}
	if nonEOL[0].Kind != TokModule {
		t.Errorf("token[0] = %s, want TokModule", kindName(nonEOL[0].Kind))
	}
	if nonEOL[1].Kind != TokDot {
		t.Errorf("token[1] = %s, want TokDot", kindName(nonEOL[1].Kind))
	}
	if nonEOL[2].Kind != TokIdent {
		t.Errorf("token[2] = %s, want TokIdent", kindName(nonEOL[2].Kind))
	}
}

func TestTokenize_AtomWithOperatorName(t *testing.T) {
	// Atoms can be operator names: :+, :==, :||
	source := ":+ :== :||"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	// These would not match our ident-based atom scanner — they become TokColon + TokOther
	// That's acceptable for Dexter's purposes. Just verify no panic.
	if len(nonEOL) == 0 {
		t.Error("expected some tokens")
	}
}

func TestTokenize_HeredocOpeningMustEndLine(t *testing.T) {
	// """ followed by content on the same line is NOT a heredoc — it's an empty string + string
	// Actually in Elixir, content after opening """ on same line is allowed and part of heredoc
	// But closing """ must be on its own line
	source := "\"\"\"\ncontent\n\"\"\""
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) != 1 || nonEOL[0].Kind != TokHeredoc {
		t.Errorf("expected single TokHeredoc, got %v", kindSlice(kindsOf(nonEOL)))
	}
}

func TestTokenize_HeredocWithIndentedClosing(t *testing.T) {
	// Closing """ can have leading whitespace
	source := "\"\"\"\n  content\n  \"\"\""
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) != 1 || nonEOL[0].Kind != TokHeredoc {
		t.Errorf("expected single TokHeredoc, got %v", kindSlice(kindsOf(nonEOL)))
	}
}

func TestTokenize_SigilWithModifiers(t *testing.T) {
	// ~r/foo/iu — modifiers after closing delimiter
	source := `~r/foo/iu + x`
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if nonEOL[0].Kind != TokSigil {
		t.Errorf("token[0] = %s, want TokSigil", kindName(nonEOL[0].Kind))
	}
	sigilText := string([]byte(source)[nonEOL[0].Start:nonEOL[0].End])
	if sigilText != "~r/foo/iu" {
		t.Errorf("sigil text = %q, want %q", sigilText, "~r/foo/iu")
	}
}

func TestTokenize_QuotedAtomWithInterpolation(t *testing.T) {
	// :"hello #{world}" — quoted atom with interpolation
	source := `:\"hello #{world}\" <> rest`
	// This doesn't actually produce the right escape in Go... let me use a raw approach:
	src := []byte{
		':', '"', 'h', 'e', 'l', 'l', 'o', ' ',
		'#', '{', 'w', 'o', 'r', 'l', 'd', '}',
		'"', ' ', '<', '>', ' ', 'r', 'e', 's', 't',
	}
	_ = source
	tokens := Tokenize(src)
	if tokens[0].Kind != TokAtom {
		t.Errorf("token[0] = %s, want TokAtom", kindName(tokens[0].Kind))
	}
}

func TestTokenize_NumberLiterals(t *testing.T) {
	assertKinds(t, "42", []TokenKind{TokNumber})
	assertKinds(t, "1_000_000", []TokenKind{TokNumber})
	assertKinds(t, "0xFF", []TokenKind{TokNumber})
	assertKinds(t, "0b101", []TokenKind{TokNumber})
	assertKinds(t, "0o777", []TokenKind{TokNumber})
	assertKinds(t, "3.14", []TokenKind{TokNumber})
	assertKinds(t, "1.0e10", []TokenKind{TokNumber})
	assertKinds(t, "1.0e-3", []TokenKind{TokNumber})
	// Multiple numbers with operators
	assertKinds(t, "1 + 2", []TokenKind{TokNumber, TokOther, TokNumber})
}

func TestTokenize_OnlyWhitespace(t *testing.T) {
	source := "   \t\t  \n  \n"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF")
	}
}

func TestTokenize_NullBytesInSource(t *testing.T) {
	// Binary content in file — should not panic
	source := []byte("def foo\x00do\nend")
	tokens := Tokenize(source)
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF")
	}
}

func TestTokenize_TripleColons(t *testing.T) {
	// ::: — should not cause issues
	source := ":::"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF")
	}
}

func TestTokenize_DoubleColonInSpec(t *testing.T) {
	// @spec foo() :: {:ok, term()}
	source := "@spec foo() :: {:ok, term()}"
	assertKinds(t, source, []TokenKind{
		TokAttrSpec,    // @spec
		TokIdent,       // foo
		TokOpenParen,   // (
		TokCloseParen,  // )
		TokDoubleColon, // ::
		TokOpenBrace,   // {
		TokAtom,        // :ok
		TokComma,       // ,
		TokIdent,       // term
		TokOpenParen,   // (
		TokCloseParen,  // )
		TokCloseBrace,  // }
	})
}

func TestTokenize_CharLiteralBeforeString(t *testing.T) {
	// ?", "hello" — char literal then string, not confused
	source := `?", "hello"`
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) < 3 {
		t.Fatalf("expected at least 3 tokens, got %d: %v", len(nonEOL), kindSlice(kindsOf(nonEOL)))
	}
	if nonEOL[0].Kind != TokCharLiteral {
		t.Errorf("token[0] = %s, want TokCharLiteral", kindName(nonEOL[0].Kind))
	}
	// Find the string token
	found := false
	for _, tok := range nonEOL {
		if tok.Kind == TokString {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a TokString token for \"hello\"")
	}
}

// --- Unicode identifier tests ---

func TestTokenize_UnicodeModuleName(t *testing.T) {
	// Élixir is a valid module name with Unicode
	source := "defmodule Élixir do end"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) < 3 {
		t.Fatalf("expected at least 3 tokens, got %d: %v", len(nonEOL), kindSlice(kindsOf(nonEOL)))
	}
	if nonEOL[0].Kind != TokDefmodule {
		t.Errorf("token[0] = %s, want TokDefmodule", kindName(nonEOL[0].Kind))
	}
	if nonEOL[1].Kind != TokModule {
		t.Errorf("token[1] = %s, want TokModule", kindName(nonEOL[1].Kind))
	}
	text := string([]byte(source)[nonEOL[1].Start:nonEOL[1].End])
	if text != "Élixir" {
		t.Errorf("module name = %q, want %q", text, "Élixir")
	}
}

func TestTokenize_UnicodeLowercaseIdent(t *testing.T) {
	// ólá is a valid lowercase identifier
	source := "ólá = 1"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) < 1 {
		t.Fatal("expected at least 1 token")
	}
	if nonEOL[0].Kind != TokIdent {
		t.Errorf("token[0] = %s, want TokIdent", kindName(nonEOL[0].Kind))
	}
	text := string([]byte(source)[nonEOL[0].Start:nonEOL[0].End])
	if text != "ólá" {
		t.Errorf("ident text = %q, want %q", text, "ólá")
	}
}

func TestTokenize_UnicodeAtom(t *testing.T) {
	// :ólá is a valid atom
	source := ":ólá"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) != 1 {
		t.Fatalf("expected 1 token, got %d: %v", len(nonEOL), kindSlice(kindsOf(nonEOL)))
	}
	if nonEOL[0].Kind != TokAtom {
		t.Errorf("token[0] = %s, want TokAtom", kindName(nonEOL[0].Kind))
	}
	text := string([]byte(source)[nonEOL[0].Start:nonEOL[0].End])
	if text != ":ólá" {
		t.Errorf("atom text = %q, want %q", text, ":ólá")
	}
}

func TestTokenize_UnicodeInModuleDotChain(t *testing.T) {
	// Élixir.Módulo.foo
	source := "Élixir.Módulo.foo"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	// TokModule("Élixir"), TokDot, TokModule("Módulo"), TokDot, TokIdent("foo")
	if len(nonEOL) != 5 {
		t.Fatalf("expected 5 tokens, got %d: %v", len(nonEOL), kindSlice(kindsOf(nonEOL)))
	}
	if nonEOL[0].Kind != TokModule {
		t.Errorf("token[0] = %s, want TokModule", kindName(nonEOL[0].Kind))
	}
	if nonEOL[2].Kind != TokModule {
		t.Errorf("token[2] = %s, want TokModule", kindName(nonEOL[2].Kind))
	}
}

func TestTokenize_JapaneseAtom(t *testing.T) {
	// :こんにちは — CJK characters in atom
	source := ":こんにちは"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if len(nonEOL) != 1 || nonEOL[0].Kind != TokAtom {
		t.Errorf("expected single TokAtom, got %v", kindSlice(kindsOf(nonEOL)))
	}
}

func TestTokenize_MixedASCIIUnicodeIdent(t *testing.T) {
	// http_сервер — Latin + Cyrillic
	source := "http_сервер = 1"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if nonEOL[0].Kind != TokIdent {
		t.Errorf("token[0] = %s, want TokIdent", kindName(nonEOL[0].Kind))
	}
	text := string([]byte(source)[nonEOL[0].Start:nonEOL[0].End])
	if text != "http_сервер" {
		t.Errorf("ident text = %q, want %q", text, "http_сервер")
	}
}

func TestTokenize_EmojiIsNotIdentifier(t *testing.T) {
	// 🎉 is not a letter — should be TokOther
	source := "🎉 + x"
	tokens := tokenizeNoEOF(source)
	var nonEOL []Token
	for _, tok := range tokens {
		if tok.Kind != TokEOL {
			nonEOL = append(nonEOL, tok)
		}
	}
	if nonEOL[0].Kind == TokIdent || nonEOL[0].Kind == TokModule {
		t.Errorf("emoji should not be TokIdent or TokModule, got %s", kindName(nonEOL[0].Kind))
	}
	// The "x" at the end should still be a valid TokIdent
	lastTok := nonEOL[len(nonEOL)-1]
	if lastTok.Kind != TokIdent {
		t.Errorf("last token = %s, want TokIdent", kindName(lastTok.Kind))
	}
}

func TestTokenize_AttrWithUnicode(t *testing.T) {
	// @módulo — attribute name containing Unicode
	source := "@módulo"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected TokEOF")
	}
	if tokens[0].Kind != TokAttr {
		t.Errorf("token[0] = %s, want TokAttr", kindName(tokens[0].Kind))
	}
	text := string([]byte(source)[tokens[0].Start:tokens[0].End])
	if text != "@módulo" {
		t.Errorf("attr text = %q, want %q", text, "@módulo")
	}
}

// --- Keyword list edge cases ---

func TestTokenize_KeywordList(t *testing.T) {
	// [:foo, :bar, three: :four, five: :six]
	source := "[:foo, :bar, three: :four, five: :six]"
	assertKinds(t, source, []TokenKind{
		TokOpenBracket,  // [
		TokAtom,         // :foo
		TokComma,        // ,
		TokAtom,         // :bar
		TokComma,        // ,
		TokIdent,        // three
		TokColon,        // :
		TokAtom,         // :four
		TokComma,        // ,
		TokIdent,        // five
		TokColon,        // :
		TokAtom,         // :six
		TokCloseBracket, // ]
	})
}

func TestTokenize_KeywordListWithTuples(t *testing.T) {
	// [:foo, :bar, {:three, :four}]
	source := "[:foo, :bar, {:three, :four}]"
	assertKinds(t, source, []TokenKind{
		TokOpenBracket,  // [
		TokAtom,         // :foo
		TokComma,        // ,
		TokAtom,         // :bar
		TokComma,        // ,
		TokOpenBrace,    // {
		TokAtom,         // :three
		TokComma,        // ,
		TokAtom,         // :four
		TokCloseBrace,   // }
		TokCloseBracket, // ]
	})
}

func TestTokenize_KeywordArgInFunctionCall(t *testing.T) {
	// func(arg, key: value, other: thing)
	source := "func(arg, key: value, other: thing)"
	assertKinds(t, source, []TokenKind{
		TokIdent,      // func
		TokOpenParen,  // (
		TokIdent,      // arg
		TokComma,      // ,
		TokIdent,      // key
		TokColon,      // :
		TokIdent,      // value
		TokComma,      // ,
		TokIdent,      // other
		TokColon,      // :
		TokIdent,      // thing
		TokCloseParen, // )
	})
}

func TestTokenize_AliasWithKeywordAs(t *testing.T) {
	// alias MyApp.Foo, as: Bar
	source := "alias MyApp.Foo, as: Bar"
	assertKinds(t, source, []TokenKind{
		TokAlias,  // alias
		TokModule, // MyApp
		TokDot,    // .
		TokModule, // Foo
		TokComma,  // ,
		TokIdent,  // as
		TokColon,  // :
		TokModule, // Bar
	})
}

func TestTokenize_UseWithKeywordOpts(t *testing.T) {
	// use Phoenix.Controller, namespace: MyApp.Web
	source := "use Phoenix.Controller, namespace: MyApp.Web"
	assertKinds(t, source, []TokenKind{
		TokUse,    // use
		TokModule, // Phoenix
		TokDot,    // .
		TokModule, // Controller
		TokComma,  // ,
		TokIdent,  // namespace
		TokColon,  // :
		TokModule, // MyApp
		TokDot,    // .
		TokModule, // Web
	})
}

func TestTokenize_MapLiteral(t *testing.T) {
	// %{name: "foo", age: 1}
	source := `%{name: "foo", age: 1}`
	assertKinds(t, source, []TokenKind{
		TokPercent,    // %
		TokOpenBrace,  // {
		TokIdent,      // name
		TokColon,      // :
		TokString,     // "foo"
		TokComma,      // ,
		TokIdent,      // age
		TokColon,      // :
		TokNumber,     // 1
		TokCloseBrace, // }
	})
}

func TestTokenize_StructLiteral(t *testing.T) {
	// %User{name: "foo"}
	source := `%User{name: "foo"}`
	assertKinds(t, source, []TokenKind{
		TokPercent,    // %
		TokModule,     // User
		TokOpenBrace,  // {
		TokIdent,      // name
		TokColon,      // :
		TokString,     // "foo"
		TokCloseBrace, // }
	})
}

func TestTokenize_DefdelegateWithKeywordOpts(t *testing.T) {
	// defdelegate foo(x), to: Mod, as: :bar
	source := "defdelegate foo(x), to: Mod, as: :bar"
	assertKinds(t, source, []TokenKind{
		TokDefdelegate, // defdelegate
		TokIdent,       // foo
		TokOpenParen,   // (
		TokIdent,       // x
		TokCloseParen,  // )
		TokComma,       // ,
		TokIdent,       // to
		TokColon,       // :
		TokModule,      // Mod
		TokComma,       // ,
		TokIdent,       // as
		TokColon,       // :
		TokAtom,        // :bar
	})
}

func TestTokenize_AfterDotKeywordBecomesIdent(t *testing.T) {
	// foo.do should emit TokIdent, not TokDo
	assertKinds(t, "foo.do", []TokenKind{
		TokIdent, // foo
		TokDot,   // .
		TokIdent, // do (de-keyworded because after dot)
	})
	// foo.end should emit TokIdent, not TokEnd
	assertKinds(t, "foo.end", []TokenKind{
		TokIdent, // foo
		TokDot,   // .
		TokIdent, // end
	})
	// foo.def should emit TokIdent, not TokDef
	assertKinds(t, "foo.def", []TokenKind{
		TokIdent, // foo
		TokDot,   // .
		TokIdent, // def
	})
	// foo.fn should emit TokIdent, not TokFn
	assertKinds(t, "foo.fn", []TokenKind{
		TokIdent, // foo
		TokDot,   // .
		TokIdent, // fn
	})
}

func TestTokenize_AfterDotKeywordWithNewline(t *testing.T) {
	// afterDot persists through whitespace and newlines
	source := "foo.\n  do"
	assertKinds(t, source, []TokenKind{
		TokIdent, TokDot, TokEOL, TokIdent,
	})
}

func TestTokenize_AfterDotKeywordWithComment(t *testing.T) {
	// afterDot persists through comments
	source := "foo. # comment\ndo"
	assertKinds(t, source, []TokenKind{
		TokIdent, TokDot, TokComment, TokEOL, TokIdent,
	})
}

func TestTokenize_AfterDotClearedByNonDot(t *testing.T) {
	// afterDot does NOT persist through other tokens
	// foo.bar, do — the comma clears afterDot, so "do" is a keyword
	assertKinds(t, "foo.bar, do", []TokenKind{
		TokIdent, TokDot, TokIdent, TokComma, TokDo,
	})
}

func TestTokenize_OperatorAtoms(t *testing.T) {
	assertKinds(t, ":+", []TokenKind{TokAtom})
	assertKinds(t, ":-", []TokenKind{TokAtom})
	assertKinds(t, ":&&", []TokenKind{TokAtom})
	assertKinds(t, ":>>>", []TokenKind{TokAtom})
	assertKinds(t, ":||", []TokenKind{TokAtom})
	assertKinds(t, ":|>", []TokenKind{TokAtom})
	assertKinds(t, ":!", []TokenKind{TokAtom})
	assertKinds(t, ":~", []TokenKind{TokAtom})
	assertKinds(t, ":\\\\", []TokenKind{TokAtom})
}

func TestTokenize_IdentWithAt(t *testing.T) {
	// Elixir allows @ inside identifiers (e.g. a@b)
	assertKinds(t, "a@b", []TokenKind{TokIdent})

	tokens := tokenizeNoEOF("a@b")
	if string([]byte("a@b")[tokens[0].Start:tokens[0].End]) != "a@b" {
		t.Errorf("expected a@b to be a single identifier")
	}
}

func TestTokenize_When(t *testing.T) {
	// when as keyword in guard clause
	assertKinds(t, "def foo(x) when is_integer(x) do", []TokenKind{
		TokDef, TokIdent, TokOpenParen, TokIdent, TokCloseParen,
		TokWhen, TokIdent, TokOpenParen, TokIdent, TokCloseParen, TokDo,
	})
	// when: as keyword key → TokIdent, not TokWhen
	assertKinds(t, "[when: true]", []TokenKind{
		TokOpenBracket, TokIdent, TokColon, TokIdent, TokCloseBracket,
	})
}

func TestTokenize_KeywordAsKeywordKey(t *testing.T) {
	// do: inline syntax → TokIdent, not TokDo
	assertKinds(t, "def foo, do: :bar", []TokenKind{
		TokDef, TokIdent, TokComma, TokIdent, TokColon, TokAtom,
	})
	// end: as keyword key
	assertKinds(t, "[end: 1]", []TokenKind{
		TokOpenBracket, TokIdent, TokColon, TokNumber, TokCloseBracket,
	})
	// fn: as keyword key
	assertKinds(t, "[fn: 1]", []TokenKind{
		TokOpenBracket, TokIdent, TokColon, TokNumber, TokCloseBracket,
	})
	// do without colon is still TokDo
	assertKinds(t, "do", []TokenKind{TokDo})
	// do:: (double colon) — do is still TokDo, :: is TokDoubleColon
	assertKinds(t, "do::", []TokenKind{TokDo, TokDoubleColon})
}

func TestTokenizeFull_LineStarts(t *testing.T) {
	source := "defmodule Foo do\n  def bar do\n    :ok\n  end\nend\n"
	result := TokenizeFull([]byte(source))

	// 6 lines (5 newlines + line 1)
	if len(result.LineStarts) != 6 {
		t.Fatalf("expected 6 line starts, got %d: %v", len(result.LineStarts), result.LineStarts)
	}
	if result.LineStarts[0] != 0 {
		t.Errorf("line 1 should start at 0, got %d", result.LineStarts[0])
	}
	// Line 2 starts after "defmodule Foo do\n" = 17 bytes
	if result.LineStarts[1] != 17 {
		t.Errorf("line 2 should start at 17, got %d", result.LineStarts[1])
	}

	// Verify column calculation: "def" on line 2 starts at byte 19 (2 spaces + "def")
	// Column = offset - lineStarts[line-1] = 19 - 17 = 2 (0-based)
	defTok := result.Tokens[0]
	for _, tok := range result.Tokens {
		if tok.Kind == TokDef {
			defTok = tok
			break
		}
	}
	col := defTok.Start - result.LineStarts[defTok.Line-1]
	if col != 2 {
		t.Errorf("def column should be 2 (0-based), got %d", col)
	}
}

func TestTokenize_LeftArrow(t *testing.T) {
	// <- in for comprehension
	assertKinds(t, "for x <- list do", []TokenKind{
		TokIdent, TokIdent, TokLeftArrow, TokIdent, TokDo,
	})
	// << is still TokOpenAngle, not confused with <-
	assertKinds(t, "<<x>>", []TokenKind{TokOpenAngle, TokIdent, TokCloseAngle})
}

func TestTokenize_MultiCharSigil(t *testing.T) {
	// ~HTML is a multi-char uppercase sigil (Elixir 1.15+)
	assertKinds(t, `~HTML"""<div>hello</div>"""`, []TokenKind{TokSigil})
	assertKinds(t, `~HEEX"<div />"`, []TokenKind{TokSigil})
	assertKinds(t, `~JSON[{"a": 1}]`, []TokenKind{TokSigil})

	// Verify the full token text is captured
	tokens := tokenizeNoEOF(`~HTML"""<div>hello</div>"""`)
	source := `~HTML"""<div>hello</div>"""`
	if string(source[tokens[0].Start:tokens[0].End]) != source {
		t.Errorf("expected full sigil text, got %q", string(source[tokens[0].Start:tokens[0].End]))
	}

	// Multi-char sigils are raw (no escape processing), like uppercase single-char
	raw := `~HTML"""hello\nworld"""`
	tokensRaw := tokenizeNoEOF(raw)
	if len(tokensRaw) != 1 || tokensRaw[0].Kind != TokSigil {
		t.Errorf("expected single TokSigil for raw multi-char sigil")
	}

	// Single lowercase letter is still just one char — ~sigil is NOT multi-char
	// ~s followed by ( is a normal single-char sigil
	assertKinds(t, `~s(hello)`, []TokenKind{TokSigil})
}

func TestTokenize_BrokenCodeRecovery(t *testing.T) {
	// Unterminated string consumes to EOF (Elixir allows multi-line strings).
	// The key property: we don't crash and always produce EOF.
	source := "\"unterminated\ndefmodule Foo do\nend"
	tokens := Tokenize([]byte(source))
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Fatal("expected EOF token at end")
	}

	// If the broken string is accidentally closed by a later quote, code after recovers.
	source2 := "\"oops\" def bar, do: :ok"
	tokens2 := Tokenize([]byte(source2))
	if tokens2[len(tokens2)-1].Kind != TokEOF {
		t.Fatal("expected EOF token at end")
	}
	hasDef := false
	for _, tok := range tokens2 {
		if tok.Kind == TokDef {
			hasDef = true
		}
	}
	if !hasDef {
		t.Error("expected to find TokDef after closed string")
	}
}

func TestTokenize_UnterminatedQuotedAtom(t *testing.T) {
	// :"foo without closing quote — should not panic, should produce tokens
	source := `:\"foo`
	tokens := Tokenize([]byte(source))
	if len(tokens) == 0 {
		t.Fatal("expected at least one token")
	}
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected EOF token at end")
	}
}

func TestTokenize_PartialUTF8(t *testing.T) {
	// Truncated UTF-8 sequence: é is 0xC3 0xA9, send only 0xC3
	source := []byte{0xC3}
	tokens := Tokenize(source)
	if len(tokens) == 0 {
		t.Fatal("expected at least one token")
	}
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected EOF token at end")
	}
}

func TestTokenize_PartialUTF8InIdentifier(t *testing.T) {
	// Valid identifier start, then truncated UTF-8
	source := []byte{'f', 'o', 'o', 0xC3}
	tokens := Tokenize(source)
	hasIdent := false
	for _, tok := range tokens {
		if tok.Kind == TokIdent {
			hasIdent = true
		}
	}
	if !hasIdent {
		t.Error("expected TokIdent for 'foo' before truncated UTF-8")
	}
	if tokens[len(tokens)-1].Kind != TokEOF {
		t.Error("expected EOF token at end")
	}
}

func TestTokenize_MidEditIncompleteFunction(t *testing.T) {
	// User is mid-edit: typed "def " and hasn't finished
	source := "defmodule Foo do\n  def \nend"
	assertKinds(t, source, []TokenKind{
		TokDefmodule, TokModule, TokDo, TokEOL,
		TokDef, TokEOL,
		TokEnd,
	})
}

func TestTokenize_MidEditIncompletePipe(t *testing.T) {
	// User is mid-edit with a pipe: "foo |> "
	source := "foo |> "
	assertKinds(t, source, []TokenKind{
		TokIdent, TokPipe,
	})
}

func TestTokenize_MultipleConsecutiveErrors(t *testing.T) {
	// Multiple broken constructs — must not panic, must always reach EOF.
	// Unterminated strings/sigils consume greedily, so we can't guarantee
	// recovery of tokens after them. The invariant is: no crash, always EOF.
	cases := []string{
		"\"unterminated\n~r/unclosed\n:'also broken\ndef valid_func do\nend",
		"~s[[[[\n\n\n",
		":\"\n:\"\n:\"\ndef foo, do: :ok",
		"?",
		"~",
		"@\n@\n@",
		"::::",
		"...",
		"<<<>>><<<>>>",
	}
	for _, source := range cases {
		tokens := Tokenize([]byte(source))
		if len(tokens) == 0 {
			t.Errorf("source %q: expected at least one token", source)
			continue
		}
		if tokens[len(tokens)-1].Kind != TokEOF {
			t.Errorf("source %q: expected EOF at end, got %s", source, kindName(tokens[len(tokens)-1].Kind))
		}
	}
}

func TestTokenize_FullModuleIntegration(t *testing.T) {
	source := `defmodule MyApp.Accounts.User do
  @moduledoc """
  A user account.
  """

  use Ecto.Schema
  alias MyApp.Accounts.Role
  import MyApp.Helpers, only: [normalize: 1]

  @primary_key {:id, :binary_id, autogenerate: true}

  defstruct [:name, :email, active?: true]

  defmodule Settings do
    @moduledoc false

    defstruct theme: "dark", locale: "en"

    def default do
      %__MODULE__{}
    end
  end

  @spec changeset(t(), map()) :: Ecto.Changeset.t()
  def changeset(%__MODULE__{} = user, attrs \\ %{}) do
    user
    |> cast(attrs, [:name, :email])
    |> validate_required([:email])
  end

  defp normalize_email(email) when is_binary(email) do
    email
    |> String.downcase()
    |> String.trim()
  end

  defmacro __using__(_opts) do
    quote do
      import unquote(__MODULE__)
    end
  end

  defdelegate find(id), to: MyApp.Accounts.Repo, as: :get_user

  @type t :: %__MODULE__{
    name: String.t(),
    email: String.t()
  }
end
`
	tokens := Tokenize([]byte(source))

	type expected struct {
		kind TokenKind
		text string
	}
	want := []expected{
		{TokDefmodule, "defmodule"},
		{TokModule, "MyApp"},
		{TokDot, "."},
		{TokModule, "Accounts"},
		{TokDot, "."},
		{TokModule, "User"},
		{TokDo, "do"},
		{TokEOL, "\n"},
		{TokAttrDoc, "@moduledoc"},
		{TokHeredoc, "\"\"\"\n  A user account.\n  \"\"\""},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		{TokUse, "use"},
		{TokModule, "Ecto"},
		{TokDot, "."},
		{TokModule, "Schema"},
		{TokEOL, "\n"},
		{TokAlias, "alias"},
		{TokModule, "MyApp"},
		{TokDot, "."},
		{TokModule, "Accounts"},
		{TokDot, "."},
		{TokModule, "Role"},
		{TokEOL, "\n"},
		{TokImport, "import"},
		{TokModule, "MyApp"},
		{TokDot, "."},
		{TokModule, "Helpers"},
		{TokComma, ","},
		{TokIdent, "only"},
		{TokColon, ":"},
		{TokOpenBracket, "["},
		{TokIdent, "normalize"},
		{TokColon, ":"},
		{TokNumber, "1"},
		{TokCloseBracket, "]"},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		{TokAttr, "@primary_key"},
		{TokOpenBrace, "{"},
		{TokAtom, ":id"},
		{TokComma, ","},
		{TokAtom, ":binary_id"},
		{TokComma, ","},
		{TokIdent, "autogenerate"},
		{TokColon, ":"},
		{TokIdent, "true"},
		{TokCloseBrace, "}"},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		{TokDefstruct, "defstruct"},
		{TokOpenBracket, "["},
		{TokAtom, ":name"},
		{TokComma, ","},
		{TokAtom, ":email"},
		{TokComma, ","},
		{TokIdent, "active?"},
		{TokColon, ":"},
		{TokIdent, "true"},
		{TokCloseBracket, "]"},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		// nested module
		{TokDefmodule, "defmodule"},
		{TokModule, "Settings"},
		{TokDo, "do"},
		{TokEOL, "\n"},
		{TokAttrDoc, "@moduledoc"},
		{TokIdent, "false"},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		{TokDefstruct, "defstruct"},
		{TokIdent, "theme"},
		{TokColon, ":"},
		{TokString, "\"dark\""},
		{TokComma, ","},
		{TokIdent, "locale"},
		{TokColon, ":"},
		{TokString, "\"en\""},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		{TokDef, "def"},
		{TokIdent, "default"},
		{TokDo, "do"},
		{TokEOL, "\n"},
		{TokPercent, "%"},
		{TokModule, "__MODULE__"},
		{TokOpenBrace, "{"},
		{TokCloseBrace, "}"},
		{TokEOL, "\n"},
		{TokEnd, "end"},
		{TokEOL, "\n"},
		{TokEnd, "end"},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		// @spec changeset(t(), map()) :: Ecto.Changeset.t()
		{TokAttrSpec, "@spec"},
		{TokIdent, "changeset"},
		{TokOpenParen, "("},
		{TokIdent, "t"},
		{TokOpenParen, "("},
		{TokCloseParen, ")"},
		{TokComma, ","},
		{TokIdent, "map"},
		{TokOpenParen, "("},
		{TokCloseParen, ")"},
		{TokCloseParen, ")"},
		{TokDoubleColon, "::"},
		{TokModule, "Ecto"},
		{TokDot, "."},
		{TokModule, "Changeset"},
		{TokDot, "."},
		{TokIdent, "t"},
		{TokOpenParen, "("},
		{TokCloseParen, ")"},
		{TokEOL, "\n"},
		// def changeset(%__MODULE__{} = user, attrs \\ %{}) do
		{TokDef, "def"},
		{TokIdent, "changeset"},
		{TokOpenParen, "("},
		{TokPercent, "%"},
		{TokModule, "__MODULE__"},
		{TokOpenBrace, "{"},
		{TokCloseBrace, "}"},
		{TokOther, "="},
		{TokIdent, "user"},
		{TokComma, ","},
		{TokIdent, "attrs"},
		{TokBackslash, "\\\\"},
		{TokPercent, "%"},
		{TokOpenBrace, "{"},
		{TokCloseBrace, "}"},
		{TokCloseParen, ")"},
		{TokDo, "do"},
		{TokEOL, "\n"},
		{TokIdent, "user"},
		{TokEOL, "\n"},
		{TokPipe, "|>"},
		{TokIdent, "cast"},
		{TokOpenParen, "("},
		{TokIdent, "attrs"},
		{TokComma, ","},
		{TokOpenBracket, "["},
		{TokAtom, ":name"},
		{TokComma, ","},
		{TokAtom, ":email"},
		{TokCloseBracket, "]"},
		{TokCloseParen, ")"},
		{TokEOL, "\n"},
		{TokPipe, "|>"},
		{TokIdent, "validate_required"},
		{TokOpenParen, "("},
		{TokOpenBracket, "["},
		{TokAtom, ":email"},
		{TokCloseBracket, "]"},
		{TokCloseParen, ")"},
		{TokEOL, "\n"},
		{TokEnd, "end"},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		// defp normalize_email(email) when is_binary(email) do
		{TokDefp, "defp"},
		{TokIdent, "normalize_email"},
		{TokOpenParen, "("},
		{TokIdent, "email"},
		{TokCloseParen, ")"},
		{TokWhen, "when"},
		{TokIdent, "is_binary"},
		{TokOpenParen, "("},
		{TokIdent, "email"},
		{TokCloseParen, ")"},
		{TokDo, "do"},
		{TokEOL, "\n"},
		{TokIdent, "email"},
		{TokEOL, "\n"},
		{TokPipe, "|>"},
		{TokModule, "String"},
		{TokDot, "."},
		{TokIdent, "downcase"},
		{TokOpenParen, "("},
		{TokCloseParen, ")"},
		{TokEOL, "\n"},
		{TokPipe, "|>"},
		{TokModule, "String"},
		{TokDot, "."},
		{TokIdent, "trim"},
		{TokOpenParen, "("},
		{TokCloseParen, ")"},
		{TokEOL, "\n"},
		{TokEnd, "end"},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		// defmacro __using__(_opts) do
		{TokDefmacro, "defmacro"},
		{TokIdent, "__using__"},
		{TokOpenParen, "("},
		{TokIdent, "_opts"},
		{TokCloseParen, ")"},
		{TokDo, "do"},
		{TokEOL, "\n"},
		{TokIdent, "quote"},
		{TokDo, "do"},
		{TokEOL, "\n"},
		{TokImport, "import"},
		{TokIdent, "unquote"},
		{TokOpenParen, "("},
		{TokModule, "__MODULE__"},
		{TokCloseParen, ")"},
		{TokEOL, "\n"},
		{TokEnd, "end"},
		{TokEOL, "\n"},
		{TokEnd, "end"},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		// defdelegate find(id), to: MyApp.Accounts.Repo, as: :get_user
		{TokDefdelegate, "defdelegate"},
		{TokIdent, "find"},
		{TokOpenParen, "("},
		{TokIdent, "id"},
		{TokCloseParen, ")"},
		{TokComma, ","},
		{TokIdent, "to"},
		{TokColon, ":"},
		{TokModule, "MyApp"},
		{TokDot, "."},
		{TokModule, "Accounts"},
		{TokDot, "."},
		{TokModule, "Repo"},
		{TokComma, ","},
		{TokIdent, "as"},
		{TokColon, ":"},
		{TokAtom, ":get_user"},
		{TokEOL, "\n"},
		{TokEOL, "\n"},
		// @type t :: %__MODULE__{...}
		{TokAttrType, "@type"},
		{TokIdent, "t"},
		{TokDoubleColon, "::"},
		{TokPercent, "%"},
		{TokModule, "__MODULE__"},
		{TokOpenBrace, "{"},
		{TokEOL, "\n"},
		{TokIdent, "name"},
		{TokColon, ":"},
		{TokModule, "String"},
		{TokDot, "."},
		{TokIdent, "t"},
		{TokOpenParen, "("},
		{TokCloseParen, ")"},
		{TokComma, ","},
		{TokEOL, "\n"},
		{TokIdent, "email"},
		{TokColon, ":"},
		{TokModule, "String"},
		{TokDot, "."},
		{TokIdent, "t"},
		{TokOpenParen, "("},
		{TokCloseParen, ")"},
		{TokEOL, "\n"},
		{TokCloseBrace, "}"},
		{TokEOL, "\n"},
		{TokEnd, "end"},
		{TokEOL, "\n"},
		{TokEOF, ""},
	}

	if len(tokens) != len(want) {
		t.Fatalf("token count mismatch: got %d, want %d", len(tokens), len(want))
	}
	for i, w := range want {
		tok := tokens[i]
		gotText := string(source[tok.Start:tok.End])
		if tok.Kind != w.kind || gotText != w.text {
			t.Errorf("token[%d]: got {%s, %q}, want {%s, %q}",
				i, kindName(tok.Kind), gotText, kindName(w.kind), w.text)
		}
	}
}

func kindsOf(tokens []Token) []TokenKind {
	kinds := make([]TokenKind, len(tokens))
	for i, tok := range tokens {
		kinds[i] = tok.Kind
	}
	return kinds
}
