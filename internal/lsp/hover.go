package lsp

import (
	"strings"

	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/store"
)

func (s *Server) hoverFromFile(function string, result store.LookupResult) (*protocol.Hover, error) {
	text, _, ok := s.readFileText(result.FilePath)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	defIdx := result.Line - 1

	if defIdx < 0 || defIdx >= len(lines) {
		return nil, nil
	}

	tf := NewTokenizedFile(text)
	var doc, spec, signature string

	if function == "" {
		doc = tf.ExtractModuledoc(defIdx)
		signature = strings.TrimSpace(lines[defIdx])
		signature = strings.TrimSuffix(signature, " do")
	} else {
		doc, spec = tf.ExtractDocAbove(defIdx)
		signature = extractSignature(lines, defIdx)
	}

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
