package lsp

import (
	"strings"

	"go.lsp.dev/protocol"

	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/store"
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

	var doc, spec, signature string

	if function == "" {
		doc = extractModuledoc(lines, defIdx)
		signature = strings.TrimSpace(lines[defIdx])
		signature = strings.TrimSuffix(signature, " do")
	} else {
		doc, spec = extractDocAbove(lines, defIdx)
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

func (s *Server) hoverFromBuffer(text string, defIdx int) (*protocol.Hover, error) {
	lines := strings.Split(text, "\n")
	doc, spec := extractDocAbove(lines, defIdx)
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

// extractDocAbove scans the region above a function definition to find the
// @doc content and @spec that precede it.
func extractDocAbove(lines []string, defIdx int) (doc, spec string) {
	// Scan backward to find the previous function/module boundary so we don't
	// have to process the entire file — the relevant doc block is always between
	// the previous definition and this one. We must skip heredoc content so that
	// example code inside @doc blocks (e.g. "defmodule MyApp.Worker do") doesn't
	// get mistaken for a real boundary.
	start := 0
	inHeredocBack := false
	for i := defIdx - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if !inHeredocBack && (trimmed == `"""` || trimmed == `'''`) {
			inHeredocBack = true
			continue
		}
		if inHeredocBack {
			if strings.HasSuffix(trimmed, `"""`) || strings.HasSuffix(trimmed, `'''`) {
				inHeredocBack = false
			}
			continue
		}
		if parser.FuncDefRe.MatchString(lines[i]) || parser.DefmoduleRe.MatchString(lines[i]) || parser.TypeDefRe.MatchString(lines[i]) {
			start = i + 1
			break
		}
	}

	var currentDoc string
	var currentSpec []string
	inDocHeredoc := false
	var docLines []string
	inSpecBlock := false

	for i := start; i < defIdx; i++ {
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

		if parser.FuncDefRe.MatchString(lines[i]) || parser.DefmoduleRe.MatchString(lines[i]) || parser.TypeDefRe.MatchString(lines[i]) {
			currentDoc = ""
			currentSpec = nil
		}
	}

	if len(currentSpec) > 0 {
		spec = strings.TrimSpace(strings.Join(currentSpec, "\n"))
	}

	return currentDoc, spec
}

// extractModuledoc scans forward from a defmodule line to find the @moduledoc content.
func extractModuledoc(lines []string, moduleIdx int) string {
	for i := moduleIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])

		if trimmed == "" {
			continue
		}

		if trimmed == `@moduledoc """` || trimmed == `@moduledoc ~S"""` || trimmed == `@moduledoc ~s"""` {
			var docLines []string
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == `"""` {
					return dedentBlock(docLines)
				}
				docLines = append(docLines, lines[j])
			}
			return ""
		}

		if strings.HasPrefix(trimmed, `@moduledoc "`) {
			return extractQuotedString(trimmed[len("@moduledoc "):])
		}

		if trimmed == "@moduledoc false" {
			return ""
		}

		if strings.HasPrefix(trimmed, "use ") || strings.HasPrefix(trimmed, "import ") ||
			strings.HasPrefix(trimmed, "alias ") || strings.HasPrefix(trimmed, "require ") ||
			strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.HasPrefix(trimmed, "def") || trimmed == "end" {
			break
		}
	}

	return ""
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
