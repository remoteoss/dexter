package lsp

import (
	"strings"

	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

func (s *Server) hoverFromFile(function string, result store.LookupResult) (*protocol.Hover, error) {
	doc, spec, signature, ok := s.hoverParts(function, result)
	if !ok {
		return nil, nil
	}
	return hoverOf(doc, spec, signature), nil
}

// hoverParts reads the doc, spec, and signature of the definition at result.
// A module's (function "") doc is its @moduledoc. `@doc false` is no doc.
func (s *Server) hoverParts(function string, result store.LookupResult) (doc, spec, signature string, ok bool) {
	text, _, ok := s.readFileText(result.FilePath)
	if !ok {
		return "", "", "", false
	}

	lines := strings.Split(text, "\n")
	defIdx := result.Line - 1

	if defIdx < 0 || defIdx >= len(lines) {
		return "", "", "", false
	}

	if function == "" {
		tf := NewTokenizedFile(text)
		doc = tf.ExtractModuledoc(defIdx)
		signature = strings.TrimSpace(lines[defIdx])
		signature = strings.TrimSuffix(signature, " do")
	} else {
		if docMayBeAbove(lines, defIdx) {
			doc, spec = NewTokenizedFile(text).ExtractDocAbove(defIdx)
		}
		signature = extractSignature(lines, defIdx)
	}
	if strings.TrimSpace(doc) == "false" {
		doc = ""
	}
	return doc, spec, signature, true
}

// defArities returns the arities of the def of function at result's line:
// one, or a range with default arguments. ok is false when the head cannot be
// read.
func (s *Server) defArities(function string, result store.LookupResult) (lo, hi int, ok bool) {
	text, _, read := s.readFileText(result.FilePath)
	if !read {
		return 0, 0, false
	}
	tf := NewTokenizedFile(text)
	for i, t := range tf.tokens {
		if t.Line < result.Line {
			continue
		}
		if t.Line > result.Line {
			break
		}
		if t.Kind == parser.TokIdent && parser.TokenText(tf.source, t) == function {
			if lo, hi, known := tokenArity(tf.source, tf.tokens, i); known {
				return lo, hi, true
			}
			return 0, 0, false
		}
	}
	return 0, 0, false
}

// docMayBeAbove reports whether a @doc or @spec can be above the def at
// defIdx. The code line above it (past blank and comment lines) decides: an
// `end`, another definition, a directive, or a line that opens a block
// cannot be followed by the def's attributes, so the file need not be
// tokenized. Any other line, such as an attribute, the end of a heredoc, or
// the last line of a multi-line @spec, can.
func docMayBeAbove(lines []string, defIdx int) bool {
	for i := defIdx - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "end" || strings.HasSuffix(line, " do") || strings.HasSuffix(line, " ->") {
			return false
		}
		for _, prefix := range []string{"def ", "defp ", "defmacro ", "defmacrop ", "defdelegate ", "defguard ", "defguardp ", "defmodule ", "use ", "alias ", "import ", "require "} {
			if strings.HasPrefix(line, prefix) {
				return false
			}
		}
		return true
	}
	return false
}

// hoverOf is the hover of the parts, or nil when they are all empty.
func hoverOf(doc, spec, signature string) *protocol.Hover {
	content := formatHoverContent(doc, spec, signature)
	if content == "" {
		return nil
	}
	return &protocol.Hover{
		Contents: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: content,
		},
	}
}

// callbackDoc returns the @doc of the callback that the def at hit of
// module.function implements, for a def with no doc of its own: a callback of
// that name and arity in a behaviour that hit's file or the module's files
// declare (with @behaviour, also in a __using__ quote, or a `use` of a
// module with callbacks). The store is asked first; the def's file is read
// for its arities only when a behaviour has a callback of the name.
func (s *Server) callbackDoc(module, function string, hit store.LookupResult) string {
	files := []string{hit.FilePath}
	if mods, err := s.store.LookupModule(module); err == nil {
		for _, m := range mods {
			files = append(files, m.FilePath)
		}
	}
	var candidates []store.CallbackResult
	seen := map[string]bool{}
	for _, file := range files {
		behaviours, _ := s.store.LookupBehavioursForFile(file)
		for _, b := range behaviours {
			if seen[b] {
				continue
			}
			seen[b] = true
			if cbs, err := s.store.LookupCallbackDef(b, function); err == nil {
				candidates = append(candidates, cbs...)
			}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	lo, hi, known := s.defArities(function, hit)
	if !known {
		lo, hi = hit.Arity, hit.Arity
	}
	for _, cb := range candidates {
		if cb.Arity < lo || cb.Arity > hi {
			continue
		}
		if doc, _, _, ok := s.hoverParts(function, store.LookupResult{FilePath: cb.FilePath, Line: cb.Line}); ok && doc != "" {
			return doc
		}
	}
	return ""
}

func (s *Server) hoverFromBuffer(tf *TokenizedFile, text string, defIdx int) (*protocol.Hover, error) {
	lines := strings.Split(text, "\n")
	doc, spec := tf.ExtractDocAbove(defIdx)
	signature := extractSignature(lines, defIdx)

	content := formatHoverContent(doc, spec, signature)
	if content == "" {
		return nil, nil
	}

	return &protocol.Hover{
		Contents: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: content,
		},
	}, nil
}

func extractSignature(lines []string, defIdx int) string {
	if defIdx < 0 || defIdx >= len(lines) {
		return ""
	}
	sig := strings.TrimSpace(lines[defIdx])
	sig = strings.TrimSuffix(sig, " do")
	sig = strings.TrimSuffix(sig, ",")
	return sig
}

func extractQuotedString(s string) string {
	if len(s) < 2 || s[0] != '"' {
		return ""
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			return s[1:i]
		}
	}
	return ""
}

func dedentBlock(lines []string) string {
	if len(lines) == 0 {
		return ""
	}

	minIndent := -1
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if minIndent < 0 || indent < minIndent {
			minIndent = indent
		}
	}

	if minIndent <= 0 {
		return strings.TrimSpace(strings.Join(lines, "\n"))
	}

	var result []string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			result = append(result, "")
		} else if len(line) >= minIndent {
			result = append(result, line[minIndent:])
		} else {
			result = append(result, strings.TrimSpace(line))
		}
	}

	return strings.TrimSpace(strings.Join(result, "\n"))
}

func formatHoverContent(doc, spec, signature string) string {
	var parts []string

	if signature != "" || spec != "" {
		var codeBlock strings.Builder
		codeBlock.WriteString("```elixir\n")
		if signature != "" {
			codeBlock.WriteString(signature)
			codeBlock.WriteString("\n")
		}
		if spec != "" {
			codeBlock.WriteString(spec)
			codeBlock.WriteString("\n")
		}
		codeBlock.WriteString("```")
		parts = append(parts, codeBlock.String())
	}

	if doc != "" {
		parts = append(parts, doc)
	}

	return strings.Join(parts, "\n\n")
}
