//go:build unix

package mcp

import (
	"path/filepath"
	"syscall"
	"testing"
)

// Regression: a FIFO blocked the read forever.
func TestFileOutline_RefusesFIFO(t *testing.T) {
	e := setupProject(t)
	if err := syscall.Mkfifo(filepath.Join(e.root, "lib", "pipe.ex"), 0644); err != nil {
		t.Fatal(err)
	}
	wantContains(t, e.callToolExpectError("dexter_file_outline", map[string]any{"file": "lib/pipe.ex"}), "not a regular file")
}
