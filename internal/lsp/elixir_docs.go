package lsp

import (
	"strings"

	"github.com/remoteoss/dexter/internal/parser"
)

// ExtractDocAbove scans backward from defLineIdx (0-based) to find @doc and @spec.
func (tf *TokenizedFile) ExtractDocAbove(defLineIdx int) (doc, spec string) {
	defLine1 := defLineIdx + 1

	// Find the token index for defLine1
	startIdx := -1
	for i, tok := range tf.tokens {
		if tok.Line >= defLine1 {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		return "", ""
	}

	// Scan backward to find the previous statement boundary to scope the search
	boundaryIdx := 0
	for i := startIdx - 1; i >= 0; i-- {
		if parser.IsStatementBoundaryToken(tf.tokens[i].Kind) {
			boundaryIdx = i + 1
			break
		}
	}

	var currentDoc string
	var currentSpec []string
	inSpecBlock := false

	// Instead of token by token, we iterate line by line in the token range
	// because specs and docs are line-oriented in the original code.
	startLine := tf.tokens[boundaryIdx].Line - 1
	endLine := defLineIdx

	lines := strings.Split(string(tf.source), "\n")
	if startLine < 0 {
		startLine = 0
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}

	inDocHeredoc := false
	var docLines []string

	for i := startLine; i < endLine; i++ {
		trimmed := strings.TrimSpace(lines[i])

		if inDocHeredoc {
			if trimmed == `"""` {
				inDocHeredoc = false
				currentDoc = dedentBlock(docLines)
				docLines = nil
			} else {
				docLines = append(docLines, lines[i])
			}
			continue
		}

		if inSpecBlock {
			if trimmed == "" || strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, "def") {
				inSpecBlock = false
			} else {
				currentSpec = append(currentSpec, lines[i])
				continue
			}
		}

		if trimmed == `@doc """` || trimmed == `@doc ~S"""` || trimmed == `@doc ~s"""` ||
			trimmed == `@typedoc """` || trimmed == `@typedoc ~S"""` || trimmed == `@typedoc ~s"""` {
			inDocHeredoc = true
			docLines = nil
			continue
		}

		if strings.HasPrefix(trimmed, `@doc "`) {
			currentDoc = extractQuotedString(trimmed[5:])
			continue
		}

		if strings.HasPrefix(trimmed, `@typedoc "`) {
			currentDoc = extractQuotedString(trimmed[9:])
			continue
		}

		if trimmed == "@doc false" || trimmed == "@typedoc false" {
			currentDoc = ""
			continue
		}

		if strings.HasPrefix(trimmed, "@spec ") {
			currentSpec = []string{lines[i]}
			inSpecBlock = true
			continue
		}
	}

	if len(currentSpec) > 0 {
		spec = strings.TrimSpace(strings.Join(currentSpec, "\n"))
	}
	return currentDoc, spec
}

// ExtractModuledoc scans forward from defLineIdx to find @moduledoc.
func (tf *TokenizedFile) ExtractModuledoc(defLineIdx int) string {
	defLine1 := defLineIdx + 1
	n := tf.n

	startIdx := -1
	for i, tok := range tf.tokens {
		if tok.Line >= defLine1 {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		return ""
	}

	// Scan forward within the module block
	for i := startIdx; i < n; i++ {
		tok := tf.tokens[i]

		// Stop if we hit a definition (we went too far)
		if tok.Kind == parser.TokDef || tok.Kind == parser.TokDefp || tok.Kind == parser.TokDefmacro || tok.Kind == parser.TokDefmacrop || tok.Kind == parser.TokEnd {
			break
		}

		if tok.Kind == parser.TokAttrDoc || tok.Kind == parser.TokAttr {
			attrText := parser.TokenText(tf.source, tok)
			if attrText == "@moduledoc" {
				j := parser.NextSigToken(tf.tokens, n, i+1)
				if j < n {
					nextTok := tf.tokens[j]
					if nextTok.Kind == parser.TokHeredoc || nextTok.Kind == parser.TokString {
						return extractDocFromStringToken(tf.source, nextTok)
					} else if nextTok.Kind == parser.TokIdent && parser.TokenText(tf.source, nextTok) == "false" {
						return ""
					}
				}
			}
		}
	}
	return ""
}

func extractDocFromStringToken(source []byte, tok parser.Token) string {
	text := string(source[tok.Start:tok.End])
	if tok.Kind == parser.TokHeredoc {
		// remove quotes
		lines := strings.Split(text, "\n")
		if len(lines) >= 2 {
			// strip first line `"""`
			lines = lines[1 : len(lines)-1]
			return dedentBlock(lines)
		}
		return ""
	}
	if tok.Kind == parser.TokString {
		return extractQuotedString(text)
	}
	return text
}
