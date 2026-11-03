package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
)

// TestParsedSource_LinesMatchTokenScans checks that the one-pass maps of
// parsedSource give what the per-line token scans give, on every line of the
// test fixtures.
func TestParsedSource_LinesMatchTokenScans(t *testing.T) {
	root := "testdata"
	n := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".ex") || strings.HasSuffix(p, ".exs")) {
			return nil
		}
		data, _ := os.ReadFile(p)
		res := parser.TokenizeFull(data)
		ps := &parsedSource{text: string(data), src: data, tokens: res.Tokens, interp: res.Interp, lineStarts: res.LineStarts}
		tf := NewTokenizedFile(string(data))
		lines := strings.Count(string(data), "\n") + 1
		for l := 1; l <= lines; l++ {
			n++
			if a, b := ps.moduleAt(l), extractEnclosingModuleFromTokens(data, res.Tokens, l-1); a != b {
				t.Fatalf("%s:%d moduleAt %q want %q", p, l, a, b)
			}
			if a, b := ps.inTypespec(l), tf.InTypespec(l-1); a != b {
				t.Fatalf("%s:%d inTypespec %v want %v", p, l, a, b)
			}
		}
		return nil
	})
	if n == 0 {
		t.Fatal("no fixture lines")
	}
}
