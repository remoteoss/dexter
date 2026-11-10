//go:build darwin

package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

// caseInsensitiveDirs caches, for each directory, whether its volume ignores
// case. A rename and every document lookup compute keys, so the probe runs
// once for each directory.
var caseInsensitiveDirs sync.Map // dir → bool

// foldsCase reports whether the volume that holds dir ignores case. APFS and
// HFS+ ignore case by default, but a volume can be formatted case-sensitive.
func foldsCase(dir string) bool {
	if v, ok := caseInsensitiveDirs.Load(dir); ok {
		return v.(bool)
	}
	r := probeCaseInsensitive(dir)
	caseInsensitiveDirs.Store(dir, r)
	return r
}

// probeCaseInsensitive looks for the nearest directory, from dir upward, whose
// name has a letter, and stats it under its name with the case swapped. When
// no such directory exists, it answers with the macOS default.
func probeCaseInsensitive(dir string) bool {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		base := filepath.Base(d)
		if swapped := swapCase(base); swapped != base {
			if a, err := os.Stat(d); err == nil {
				b, err := os.Stat(filepath.Join(filepath.Dir(d), swapped))
				return err == nil && os.SameFile(a, b)
			}
		}
		if parent := filepath.Dir(d); parent == d {
			return true
		}
	}
}

func swapCase(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsUpper(r) {
			return unicode.ToLower(r)
		}
		return unicode.ToUpper(r)
	}, s)
}

// ignoresNormalization is true where the file systems treat the NFC and NFD
// spellings of a name as one file: APFS and HFS+ on macOS. Elsewhere (ext4,
// for one) they are two files, and a key must not join them.
const ignoresNormalization = true
