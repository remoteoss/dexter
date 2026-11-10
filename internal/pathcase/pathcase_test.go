package pathcase

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// caseInsensitiveDir returns a directory on a volume that ignores case, or
// skips the test.
func caseInsensitiveDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	probe := filepath.Join(dir, "Probe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(strings.ToLower(probe)); err != nil || !caseInsensitive {
		t.Skip("the volume is case-sensitive")
	}
	return dir
}

func TestSpelledBelowFindsTheOldSpellingOfARename(t *testing.T) {
	root := caseInsensitiveDir(t)
	if err := os.MkdirAll(filepath.Join(root, "lib", "Area"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "lib", "Area", "kept.ex")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !SpelledBelow(root, file) {
		t.Fatal("a path in the stored spelling is not spelled right")
	}
	if err := os.Rename(filepath.Join(root, "lib", "Area"), filepath.Join(root, "lib", "area")); err != nil {
		t.Fatal(err)
	}
	if SpelledBelow(root, file) {
		t.Fatal("the old spelling of a renamed directory is spelled right")
	}
	if SpelledBelow(root, filepath.Join(root, "lib", "Area")) {
		t.Fatal("the old spelling of the directory itself is spelled right")
	}
	if !SpelledBelow(root, filepath.Join(root, "lib", "area", "kept.ex")) {
		t.Fatal("the new spelling is not spelled right")
	}
	// The root keeps the spelling that the caller typed.
	typed := filepath.Join(filepath.Dir(root), strings.ToUpper(filepath.Base(root)))
	if !SpelledBelow(typed, filepath.Join(typed, "lib", "area", "kept.ex")) {
		t.Fatal("a root typed in another case made a path below it misspelled")
	}
	if !SpelledBelow(root, filepath.Join(root, "lib", "missing.ex")) {
		t.Fatal("a path that is not there counts as misspelled")
	}
}
