//go:build !darwin

package lsp

// foldsCase reports whether the volume that holds dir ignores case. Dexter
// treats paths as case-sensitive outside macOS, as the rest of the index does.
func foldsCase(string) bool { return false }

// ignoresNormalization is true where the file systems treat the NFC and NFD
// spellings of a name as one file: APFS and HFS+ on macOS. Elsewhere (ext4,
// for one) they are two files, and a key must not join them.
const ignoresNormalization = false
