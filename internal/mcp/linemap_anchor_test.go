package mcp

import (
	"fmt"
	"strings"
	"testing"
)

// anchorLines follows the longest increasing run of unique lines back from
// its last element. A moved line changes the order, and the run must still
// match only equal lines (an earlier version read index -1 and panicked).
func TestAnchorLines_MovedLine(t *testing.T) {
	// x1 moves from the start to the end: the run is x2, x3, x4.
	a := []string{"x1", "x2", "x3", "x4"}
	b := []string{"x2", "x3", "x4", "x1"}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("anchorLines panicked: %v", r)
		}
	}()
	var pairs [][2]int
	anchorLines(a, b, func(i, j int) { pairs = append(pairs, [2]int{i, j}) })
	for _, p := range pairs {
		if a[p[0]] != b[p[1]] {
			t.Fatalf("anchorLines matched different lines: %v", pairs)
		}
	}
}

// The public path: a buffer that differs from the saved file in more than
// maxDiffEdits lines falls back to anchorLines.
func TestLineMap_LargeMovedBlock(t *testing.T) {
	var disk, buf []string
	for i := 0; i < 1500; i++ {
		disk = append(disk, fmt.Sprintf("  def f%d, do: %d", i, i))
	}
	// Move the first 600 lines to the end of the buffer: 1200 inserted and
	// deleted lines, above maxDiffEdits, so newLineMap uses anchorLines.
	buf = append(buf, disk[600:]...)
	buf = append(buf, disk[:600]...)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("newLineMap panicked: %v", r)
		}
	}()
	m := newLineMap(strings.Join(disk, "\n"), strings.Join(buf, "\n"))
	for n := 1; n < len(m.to); n++ {
		j, ok := m.locate(n)
		if !ok {
			continue
		}
		if disk[n-1] != buf[j-1] {
			t.Fatalf("saved line %d (%q) mapped to buffer line %d (%q)", n, disk[n-1], j, buf[j-1])
		}
	}
}
