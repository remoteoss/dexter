package mcp

import "strings"

// Bounds of the line diff. On a 10,000-line file, a few edits cost about
// 0.3 ms and 900 edits about 2 ms and 5 MB; outside the bounds lineMap falls
// back to the lines that both texts share at the start and at the end.
const (
	maxDiffLines = 50000 // lines of both texts after the shared start and end
	maxDiffEdits = 1000  // inserted plus deleted lines
)

// lineMap maps each 1-based line of the saved file (where the index puts
// definitions and references) to the same line in an editor buffer, or to 0
// when the buffer changed or deleted that line. A line counts as equal
// whatever its line ending, so a buffer that differs from the disk only in
// CRLF against LF changes no line.
type lineMap struct {
	to []int32 // to[i] is the buffer line of saved line i; index 0 is unused
}

func newLineMap(disk, text string) lineMap {
	a, b := splitLines(disk), splitLines(text)
	m := lineMap{to: make([]int32, len(a)+1)}
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		m.to[prefix+1] = int32(prefix + 1)
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		m.to[len(a)-suffix] = int32(len(b) - suffix)
		suffix++
	}
	midA, midB := a[prefix:len(a)-suffix], b[prefix:len(b)-suffix]
	if len(midA) == 0 || len(midB) == 0 || len(midA)+len(midB) > maxDiffLines {
		return m
	}
	matchLines(midA, midB, func(i, j int) {
		m.to[prefix+i+1] = int32(prefix + j + 1)
	})
	return m
}

// locate returns the buffer line of saved line n, or false when the buffer
// changed it.
func (m lineMap) locate(n int) (int, bool) {
	if n < 1 || n >= len(m.to) || m.to[n] == 0 {
		return n, false
	}
	return int(m.to[n]), true
}

// splitLines splits text into lines without their line endings.
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// matchLines reports the pairs of equal lines of a shortest edit script from a
// to b (Myers, "An O(ND) Difference Algorithm"), in order. It reports nothing
// when the script needs more than maxDiffEdits edits.
func matchLines(a, b []string, match func(i, j int)) {
	n, m := len(a), len(b)
	maxD := n + m
	if maxD > maxDiffEdits {
		maxD = maxDiffEdits
	}
	off := maxD + 1
	v := make([]int32, 2*maxD+3)
	// trace[d] is v[off-d-1 : off+d+2] before step d, for the backtrack.
	var trace [][]int32
	for d := 0; d <= maxD; d++ {
		trace = append(trace, append([]int32(nil), v[off-d-1:off+d+2]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = int(v[off+k+1])
			} else {
				x = int(v[off+k-1]) + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = int32(x)
			if x >= n && y >= m {
				backtrack(trace, n, m, match)
				return
			}
		}
	}
}

func backtrack(trace [][]int32, n, m int, match func(i, j int)) {
	type pair struct{ i, j int }
	var pairs []pair
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d] // v[k] is at index k+d+1
		at := func(k int) int { return int(v[k+d+1]) }
		k := x - y
		var prevK int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			x--
			y--
			pairs = append(pairs, pair{x, y})
		}
		x, y = prevX, prevY
	}
	for i := len(pairs) - 1; i >= 0; i-- {
		match(pairs[i].i, pairs[i].j)
	}
}
