package mcp

import "strings"

// Bounds of the exact line diff. On a 10,000-line file, a few edits cost about
// 0.3 ms and 900 edits about 2 ms and 5 MB; outside the bounds lineMap uses
// anchorLines, which is not always minimal but labels only lines it cannot
// place.
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
	if len(midA) == 0 || len(midB) == 0 {
		return m
	}
	match := func(i, j int) { m.to[prefix+i+1] = int32(prefix + j + 1) }
	if len(midA)+len(midB) > maxDiffLines || !matchLines(midA, midB, match) {
		anchorLines(midA, midB, match)
	}
	return m
}

// anchorLines is the diff above the bounds of matchLines (patience diff). The
// lines that occur once in a and once in b are anchors; the longest series of
// anchors in the same order in both texts is matched, and each gap between
// two anchors is matched at its two ends and, when it is small, with
// matchLines. It is O(n log n), and only lines that differ, or that it cannot
// place, are left unmatched.
func anchorLines(a, b []string, match func(i, j int)) {
	type count struct{ na, nb, jb int }
	counts := make(map[string]*count, len(a))
	for _, l := range a {
		c := counts[l]
		if c == nil {
			c = &count{}
			counts[l] = c
		}
		c.na++
	}
	for j, l := range b {
		if c := counts[l]; c != nil {
			c.nb++
			c.jb = j
		}
	}
	type pair struct{ i, j int }
	var unique []pair
	for i, l := range a {
		if c := counts[l]; c.na == 1 && c.nb == 1 {
			unique = append(unique, pair{i, c.jb})
		}
	}
	// The longest increasing run of j, in the order of i.
	tails := make([]int, 0, len(unique)) // index into unique of the last pair of each length
	prev := make([]int, len(unique))
	for k, p := range unique {
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if unique[tails[mid]].j < p.j {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		prev[k] = -1
		if lo > 0 {
			prev[k] = tails[lo-1]
		}
		if lo == len(tails) {
			tails = append(tails, k)
		} else {
			tails[lo] = k
		}
	}
	anchors := make([]pair, len(tails))
	for k, n := len(tails)-1, len(tails); n > 0; n-- {
		anchors[n-1] = unique[k]
		k = prev[k]
	}
	anchors = append(anchors, pair{len(a), len(b)}) // the end, not a match

	i0, j0 := 0, 0
	for _, an := range anchors {
		i, j := i0, j0
		for i < an.i && j < an.j && a[i] == b[j] {
			match(i, j)
			i++
			j++
		}
		x, y := an.i, an.j
		for x > i && y > j && a[x-1] == b[y-1] {
			x--
			y--
		}
		if i < x && j < y && (x-i)+(y-j) <= maxDiffLines {
			matchLines(a[i:x], b[j:y], func(gi, gj int) { match(i+gi, j+gj) })
		}
		for k := 0; x+k < an.i; k++ {
			match(x+k, y+k)
		}
		if an.i < len(a) {
			match(an.i, an.j)
		}
		i0, j0 = an.i+1, an.j+1
	}
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
// when the script needs more than maxDiffEdits edits, and returns false.
func matchLines(a, b []string, match func(i, j int)) bool {
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
				return true
			}
		}
	}
	return false
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
