package lsp

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// exemptFromPositionGuard are the files allowed to build a protocol.Position
// by hand. Only the conversion helpers themselves qualify.
var exemptFromPositionGuard = map[string]bool{
	"position_encoding.go": true,
}

// TestNoUnconvertedPositions fails when a protocol.Position is built with a
// column that did not come through the position-encoding funnels.
//
// The compiler cannot catch this: a raw protocol.Position{...} compiles
// perfectly well and is wrong only on lines containing non-ASCII, which no
// ordinary test exercises. A miss here is not a crash but a silently
// misplaced edit — on rename, a corrupted file — so the check has to be
// mechanical rather than left to review.
//
// To add a legitimate exception, end the line with the marker comment below
// and say in the surrounding code why the column is already correct.
func TestNoUnconvertedPositions(t *testing.T) {
	const marker = "// position-encoding: converted"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		if strings.HasSuffix(name, "_test.go") || exemptFromPositionGuard[name] {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "protocol.Position{") {
				continue
			}
			trimmed := strings.TrimSpace(line)
			// A column of zero is the start of the line in every encoding.
			if strings.Contains(trimmed, "Character: 0}") || strings.Contains(trimmed, "Character: 0,") {
				continue
			}
			if strings.Contains(trimmed, marker) {
				continue
			}
			offenders = append(offenders, name+":"+strconv.Itoa(i+1)+": "+trimmed)
		}
	}

	if len(offenders) > 0 {
		t.Errorf("protocol.Position built without a position-encoding conversion.\n"+
			"Use s.outPos (when the file's lines are at hand) or s.outPosLine\n"+
			"(when only one line is), so the column reaches the client in the\n"+
			"unit the protocol expects. If the column is already converted, end\n"+
			"the line with %q.\n\n%s",
			marker, strings.Join(offenders, "\n"))
	}
}
