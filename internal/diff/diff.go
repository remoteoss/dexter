// Package diff renders unified diffs for the MCP rename tool. It is a small,
// self-contained line-based LCS diff, not a general-purpose diff library.
package diff

import (
	"fmt"
	"strings"
)

const contextLines = 3

// noEOL marks a final line that is not newline-terminated. It makes such a
// line compare unequal to the same content with a newline (matching git
// semantics) and tells the renderer to emit the "\ No newline" annotation.
const noEOL = "\x00noeol"

// Unified returns a unified diff between oldText and newText, labelled
// `--- a/oldName` / `+++ b/newName`, or "" when the texts are equal. Missing
// trailing newlines are annotated so standard tools (git apply, patch -p1)
// can apply the output.
func Unified(oldName, newName, oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	oldLines := splitLines(oldText)
	newLines := splitLines(newText)

	ops := diffOps(oldLines, newLines)
	hunks := buildHunks(ops)
	if len(hunks) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n", oldName)
	fmt.Fprintf(&b, "+++ b/%s\n", newName)
	for _, h := range hunks {
		oldCount := 0
		newCount := 0
		for _, op := range h.ops {
			switch op.kind {
			case opEqual:
				oldCount++
				newCount++
			case opDelete:
				oldCount++
			case opInsert:
				newCount++
			}
		}
		fmt.Fprintf(&b, "@@ -%s +%s @@\n", hunkRange(h.oldStart, oldCount), hunkRange(h.newStart, newCount))
		for _, op := range h.ops {
			text, noNL := strings.CutSuffix(op.text, noEOL)
			switch op.kind {
			case opEqual:
				b.WriteString(" " + text + "\n")
			case opDelete:
				b.WriteString("-" + text + "\n")
			case opInsert:
				b.WriteString("+" + text + "\n")
			}
			if noNL {
				b.WriteString("\\ No newline at end of file\n")
			}
		}
	}
	return b.String()
}

// hunkRange renders a unified-diff range: "start,count", with count omitted
// when it is 1 and start clamped for empty ranges.
func hunkRange(start, count int) string {
	if count == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	if count == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	return fmt.Sprintf("%d,%d", start+1, count)
}

// splitLines splits text into lines without terminators. A final line that is
// not newline-terminated gets the noEOL sentinel appended.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // trailing newline: drop the phantom element
	} else {
		lines[len(lines)-1] += noEOL
	}
	return lines
}

type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind opKind
	text string
}

// diffOps computes an edit script via LCS with a trimmed common prefix/suffix
// (keeps the DP table small for the typical rename diff: long files, few edits).
func diffOps(oldLines, newLines []string) []diffOp {
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	midOld := oldLines[prefix : len(oldLines)-suffix]
	midNew := newLines[prefix : len(newLines)-suffix]

	var ops []diffOp
	for i := 0; i < prefix; i++ {
		ops = append(ops, diffOp{opEqual, oldLines[i]})
	}

	// LCS table over the middle section.
	n, m := len(midOld), len(midNew)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if midOld[i] == midNew[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case midOld[i] == midNew[j]:
			ops = append(ops, diffOp{opEqual, midOld[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{opDelete, midOld[i]})
			i++
		default:
			ops = append(ops, diffOp{opInsert, midNew[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{opDelete, midOld[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{opInsert, midNew[j]})
	}

	for k := 0; k < suffix; k++ {
		ops = append(ops, diffOp{opEqual, oldLines[len(oldLines)-suffix+k]})
	}
	return ops
}

type hunk struct {
	oldStart, newStart int // 0-based index of the first line in the hunk
	ops                []diffOp
}

// buildHunks groups changed ops with surrounding context, coalescing hunks
// whose context would overlap.
func buildHunks(ops []diffOp) []hunk {
	var hunks []hunk
	i := 0
	for i < len(ops) {
		if ops[i].kind == opEqual {
			i++
			continue
		}
		// Found a change: back up for leading context.
		start := i - contextLines
		if start < 0 {
			start = 0
		}
		// Extend through subsequent changes, allowing up to 2*contextLines of
		// equal lines between changes before closing the hunk.
		end := i
		equalRun := 0
		for j := i; j < len(ops); j++ {
			if ops[j].kind == opEqual {
				equalRun++
				if equalRun > 2*contextLines {
					break
				}
			} else {
				equalRun = 0
				end = j
			}
		}
		stop := end + 1 + contextLines
		if stop > len(ops) {
			stop = len(ops)
		}

		h := hunk{ops: ops[start:stop]}
		h.oldStart, h.newStart = hunkStart(ops, start)
		hunks = append(hunks, h)
		i = stop
	}
	return hunks
}

// hunkStart returns the old/new line indices at ops[start].
func hunkStart(ops []diffOp, start int) (oldStart, newStart int) {
	for _, op := range ops[:start] {
		switch op.kind {
		case opEqual:
			oldStart++
			newStart++
		case opDelete:
			oldStart++
		case opInsert:
			newStart++
		}
	}
	return oldStart, newStart
}
