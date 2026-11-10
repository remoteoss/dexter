package lsp

import (
	"strings"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

// structKeyAt returns the struct module expression and the key when the
// cursor is on a key of a struct literal or update: `domain` in
// `%Site{domain: d}` or `%Site{site | domain: d}`. ok is false elsewhere.
func (tf *TokenizedFile) structKeyAt(line, col int) (moduleRef, key string, ok bool) {
	offset := parser.LineColToOffset(tf.lineStarts, line, col)
	if offset < 0 {
		return "", "", false
	}
	toks := tf.tokens
	idx := parser.TokenAtOffset(toks, offset)
	if (idx < 0 || toks[idx].Kind != parser.TokIdent) && offset > 0 {
		idx = parser.TokenAtOffset(toks, offset-1)
	}
	if idx < 0 || toks[idx].Kind != parser.TokIdent || idx+1 >= len(toks) ||
		toks[idx+1].Kind != parser.TokColon || toks[idx+1].Start != toks[idx].End {
		return "", "", false
	}
	// The brace that opens the literal: the first unclosed one before the key.
	depth := 0
	for j := idx - 1; j >= 0; j-- {
		switch toks[j].Kind {
		case parser.TokCloseBrace, parser.TokCloseBracket, parser.TokCloseParen:
			depth++
		case parser.TokOpenBracket, parser.TokOpenParen:
			if depth == 0 {
				return "", "", false
			}
			depth--
		case parser.TokOpenBrace:
			if depth > 0 {
				depth--
				continue
			}
			// %Mod{ : the module tokens and dots (or __MODULE__), then %.
			k := j - 1
			for k >= 0 && (toks[k].Kind == parser.TokModule || toks[k].Kind == parser.TokDot ||
				(toks[k].Kind == parser.TokIdent && parser.TokenText(tf.source, toks[k]) == "__MODULE__")) {
				k--
			}
			if k < 0 || k == j-1 || toks[k].Kind != parser.TokPercent {
				return "", "", false
			}
			return string(tf.source[toks[k+1].Start:toks[j-1].End]), parser.TokenText(tf.source, toks[idx]), true
		case parser.TokDo, parser.TokEnd, parser.TokFn:
			return "", "", false
		}
	}
	return "", "", false
}

// schemaFieldMacros declare a struct key in an Ecto schema.
var schemaFieldMacros = map[string]bool{
	"field": true, "embeds_one": true, "embeds_many": true,
	"belongs_to": true, "has_one": true, "has_many": true, "many_to_many": true,
}

// structKeyDefinition returns the line in module's source that declares the
// struct key: the key in its defstruct, or the schema field macro that names
// it. Without one, it is the defstruct or schema line. ok is false when the
// module has no struct in source.
func (s *Server) structKeyDefinition(module, key string) (path string, line int, ok bool) {
	mods, err := s.store.LookupModule(module)
	if err != nil || len(mods) == 0 {
		return "", 0, false
	}
	def := mods[0]
	text, _, read := s.readFileText(def.FilePath)
	if !read {
		return "", 0, false
	}
	tf := NewTokenizedFile(text)
	toks, src := tf.tokens, tf.source
	atom := ":" + key
	structLine := 0
	// depth counts do/fn blocks from the module's line; the module's body
	// is at moduleDepth. A nested module's body (from its defmodule until
	// depth is back at nestedAt) holds its own struct, not this one's.
	depth, moduleDepth, nestedAt := 0, -1, -1
scan:
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.Line < def.Line {
			continue
		}
		switch t.Kind {
		case parser.TokDo, parser.TokFn:
			depth++
			if moduleDepth < 0 {
				moduleDepth = depth
			}
		case parser.TokEnd:
			depth--
			if nestedAt >= 0 && depth == nestedAt {
				nestedAt = -1
			}
			if moduleDepth >= 0 && depth < moduleDepth {
				break scan
			}
		case parser.TokDefmodule:
			if moduleDepth >= 0 && nestedAt < 0 {
				// The do of the nested module's own head: the scan stops at
				// the end of the statement, so a one-line
				// `defmodule Short, do: ...` does not take a later block's do.
				if _, _, hasDo := parser.ScanForwardToMacroCallBlockDo(toks, len(toks), i+1); hasDo {
					nestedAt = depth
				} else {
					// A one-line nested module: skip its statement.
					i = statementEnd(toks, i+1)
					continue
				}
			}
		}
		if nestedAt >= 0 {
			continue
		}
		switch t.Kind {
		case parser.TokDefstruct:
			structLine = t.Line
			end := parser.NextSigToken(toks, len(toks), i+1)
			depth := 0
			for j := end; j < len(toks); j++ {
				k := toks[j].Kind
				switch k {
				case parser.TokOpenBracket, parser.TokOpenParen, parser.TokOpenBrace:
					depth++
				case parser.TokCloseBracket, parser.TokCloseParen, parser.TokCloseBrace:
					depth--
				}
				if (k == parser.TokEOL && depth <= 0) || depth < 0 {
					break
				}
				if depth == 1 && ((k == parser.TokAtom && parser.TokenText(src, toks[j]) == atom) ||
					(k == parser.TokIdent && parser.TokenText(src, toks[j]) == key && j+1 < len(toks) && toks[j+1].Kind == parser.TokColon)) {
					return def.FilePath, toks[j].Line, true
				}
			}
		case parser.TokIdent:
			name := parser.TokenText(src, t)
			if name == "schema" || name == "embedded_schema" {
				if structLine == 0 {
					structLine = t.Line
				}
				continue
			}
			if schemaFieldMacros[name] && i+1 < len(toks) && toks[i+1].Kind == parser.TokAtom && parser.TokenText(src, toks[i+1]) == atom {
				return def.FilePath, t.Line, true
			}
		}
	}
	if structLine == 0 {
		return "", 0, false
	}
	return def.FilePath, structLine, true
}

// statementEnd returns the index of the end of line that ends the statement
// starting at from: the first EOL outside brackets that does not follow a
// comma or a colon (`defmodule Short,` and `do:` go on to the next line).
func statementEnd(toks []parser.Token, from int) int {
	depth := 0
	last := parser.TokEOL
	for j := from; j < len(toks); j++ {
		k := toks[j].Kind
		switch k {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
			depth++
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace:
			depth--
		case parser.TokEOL:
			if depth <= 0 && last != parser.TokComma && last != parser.TokColon {
				return j
			}
			continue
		case parser.TokComment:
			continue
		}
		last = k
	}
	return len(toks)
}

// structKeyLocation is the definition of the struct key at the cursor, or ok
// false when the cursor is not on one.
func (s *Server) structKeyLocation(tf *TokenizedFile, filePath string, line, col int) (store.LookupResult, bool) {
	moduleRef, key, ok := tf.structKeyAt(line, col)
	if !ok {
		return store.LookupResult{}, false
	}
	var module string
	if rest, ok := strings.CutPrefix(moduleRef, "__MODULE__"); ok {
		module = extractEnclosingModuleFromTokens(tf.source, tf.tokens, line) + rest
	} else {
		aliases := tf.ExtractAliasesInScope(line)
		s.mergeAliasesFromUseTokenized(tf, aliases)
		module = s.resolveModuleWithNesting(moduleRef, aliases, filePath, line)
	}
	path, defLine, found := s.structKeyDefinition(module, key)
	if !found {
		return store.LookupResult{}, false
	}
	return store.LookupResult{FilePath: path, Line: defLine}, true
}
