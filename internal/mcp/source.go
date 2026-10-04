package mcp

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/remoteoss/dexter/internal/lsp"
)

// maxSourceBytes caps the size of one file that a tool reads. Elixir source
// files are far smaller; the cap keeps a wrong path from filling the daemon's
// memory.
const maxSourceBytes = 10 << 20

// sourceCache holds the file text that one tool call read, so that each file
// is read and tokenized at most once per call, and records which files came
// from unsaved editor buffers.
type sourceCache struct {
	mu      sync.Mutex
	views   map[string]*sourceView // nil for a file that cannot be read
	unsaved map[string]struct{}
	// conflicts are files read from disk that also have older unsaved
	// changes in an editor.
	conflicts map[string]struct{}
}

// errNotFound reports a file that does not exist.
var errNotFound = errors.New("file not found")

// userPath resolves a file path that the agent gave. A relative path is
// resolved against the project root. The path, with every symlink resolved,
// must be inside the project root, also with its symlinks resolved. The
// result is spelled under the root as the daemon spells it, so that it names
// the same file as the index and the editors.
func (h *Handler) userPath(p string) (string, error) {
	candidate := p
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(h.projectRoot, candidate)
	}
	candidate = filepath.Clean(candidate)
	root, err := filepath.EvalSymlinks(h.projectRoot)
	if err != nil {
		return "", fmt.Errorf("resolving the project root: %w", err)
	}
	real, err := filepath.EvalSymlinks(candidate)
	if errors.Is(err, fs.ErrNotExist) {
		// A file that is not on disk can still be open in an editor. Only a
		// path inside the root as spelled can name one.
		if rel, ok := inside(h.projectRoot, candidate); ok {
			path := filepath.Join(h.projectRoot, rel)
			if _, open := h.rt.UnsavedBuffer(path); open {
				return path, nil
			}
			return path, errNotFound
		}
		return "", outsideRootError(p, h.projectRoot)
	}
	if err != nil {
		return "", err
	}
	rel, ok := inside(root, real)
	if !ok {
		return "", outsideRootError(p, h.projectRoot)
	}
	return filepath.Join(h.projectRoot, rel), nil
}

func outsideRootError(p, root string) error {
	return fmt.Errorf("%s is outside the project root %s; dexter reads only files inside the project", p, root)
}

// inside returns path relative to root when it is root or under it.
func inside(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// readRegularFile reads a regular file of at most maxSourceBytes. Devices,
// FIFOs, sockets, and directories are refused before they are opened, so a
// read can neither block nor run without end.
func readRegularFile(path string) (string, time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", time.Time{}, errNotFound
		}
		return "", time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return "", time.Time{}, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxSourceBytes {
		return "", time.Time{}, fmt.Errorf("%s is %d bytes, more than the %d MB limit for one file", path, info.Size(), maxSourceBytes>>20)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = f.Close() }()
	// The path can change between the stat and the open.
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", time.Time{}, fmt.Errorf("%s changed while it was read; retry", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSourceBytes+1))
	if err != nil {
		return "", time.Time{}, err
	}
	if len(data) > maxSourceBytes {
		return "", time.Time{}, fmt.Errorf("%s is more than the %d MB limit for one file", path, maxSourceBytes>>20)
	}
	return string(data), opened.ModTime(), nil
}

// sourceView is one file as a tool call reads it. When an attached editor
// holds the file open with changes that are not saved, text is that buffer
// and disk is the saved file, which the index positions refer to.
type sourceView struct {
	text    string
	unsaved bool
	disk    string
	hasDisk bool
	// lines maps the saved file's lines, which the index positions refer
	// to, into the buffer.
	lines lineMap

	tf, diskTF *lsp.TokenizedFile
}

func newSourceView(text string, unsaved bool, disk string, hasDisk bool) *sourceView {
	v := &sourceView{text: text, unsaved: unsaved, disk: disk, hasDisk: hasDisk}
	if unsaved && hasDisk {
		v.lines = newLineMap(disk, text)
	}
	return v
}

// locate maps a 1-based line of the saved file, where the index puts a
// definition or a reference, to the same line in the text. inText is false
// when the buffer changed that line.
func (v *sourceView) locate(indexLine int) (line int, inText bool) {
	if !v.unsaved || !v.hasDisk {
		return indexLine, true
	}
	return v.lines.locate(indexLine)
}

func (v *sourceView) tokenized(inText bool) *lsp.TokenizedFile {
	if inText {
		if v.tf == nil {
			v.tf = lsp.NewTokenizedFile(v.text)
		}
		return v.tf
	}
	if v.diskTF == nil {
		v.diskTF = lsp.NewTokenizedFile(v.disk)
	}
	return v.diskTF
}

// sourceAt is one index position in a file as the user sees it.
type sourceAt struct {
	v    *sourceView
	line int // the 1-based line to show
	// changed is true when an unsaved buffer changed this line: the text and
	// the line are the saved file's.
	changed bool
}

func (a sourceAt) lineText() (string, bool) {
	if a.changed {
		return nthLine(a.v.disk, a.line)
	}
	return nthLine(a.v.text, a.line)
}

func (a sourceAt) tokenized() *lsp.TokenizedFile { return a.v.tokenized(!a.changed) }

// label is the suffix of a location whose text did not come from the buffer
// the user sees.
func (a sourceAt) label() string {
	if a.changed {
		return " (saved text; this part is changed in an unsaved editor buffer)"
	}
	return ""
}

// readSource returns the text of path as the user sees it: the newest buffer
// that an attached editor holds open when it differs from the disk, otherwise
// the file on disk. Text from an unsaved buffer is recorded, so the answer
// says so.
func (h *Handler) readSource(path string) (string, error) {
	v, err := h.view(path)
	if err != nil {
		return "", err
	}
	return v.text, nil
}

// at finds the index position path:indexLine in the text the user sees.
func (h *Handler) at(path string, indexLine int) (sourceAt, bool) {
	v, err := h.view(path)
	if err != nil {
		return sourceAt{}, false
	}
	line, inText := v.locate(indexLine)
	return sourceAt{v: v, line: line, changed: !inText}, true
}

// view reads path once per tool call.
func (h *Handler) view(path string) (*sourceView, error) {
	c := &h.sources
	c.mu.Lock()
	if cached, ok := c.views[path]; ok {
		c.mu.Unlock()
		if cached == nil {
			return nil, errNotFound
		}
		return cached, nil
	}
	c.mu.Unlock()

	v, conflict, err := h.loadSource(path)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.views == nil {
		c.views = make(map[string]*sourceView)
	}
	c.views[path] = v
	if v != nil && v.unsaved {
		if c.unsaved == nil {
			c.unsaved = make(map[string]struct{})
		}
		c.unsaved[path] = struct{}{}
	}
	if conflict {
		if c.conflicts == nil {
			c.conflicts = make(map[string]struct{})
		}
		c.conflicts[path] = struct{}{}
	}
	return v, err
}

// loadSource reads path as the user sees it. A buffer that an editor holds
// open is used only when it has changes that the editor has not saved and the
// file on disk did not change after them. When both changed, the disk wins,
// because the index follows the disk, and conflict is true.
func (h *Handler) loadSource(path string) (v *sourceView, conflict bool, err error) {
	disk, mtime, diskErr := readRegularFile(path)
	if buffer, open := h.rt.UnsavedBuffer(path); open {
		switch {
		case diskErr == nil && disk == buffer.Text:
			return newSourceView(disk, false, "", false), false, nil
		case diskErr == nil && mtime.After(buffer.ChangedAt):
			return newSourceView(disk, false, "", false), true, nil
		default:
			return newSourceView(buffer.Text, true, disk, diskErr == nil), false, nil
		}
	}
	if diskErr != nil {
		return nil, false, diskErr
	}
	return newSourceView(disk, false, "", false), false, nil
}

// sourceLine returns the text of one index position. A file that no editor
// holds open is scanned only up to the line, so a reference list does not
// read whole files.
func (h *Handler) sourceLine(path string, indexLine int) (text string, line int, label string, ok bool) {
	c := &h.sources
	c.mu.Lock()
	_, cached := c.views[path]
	c.mu.Unlock()
	if !cached {
		if _, open := h.rt.UnsavedBuffer(path); !open {
			text, ok := h.lsp.FileLine(path, indexLine)
			return text, indexLine, "", ok
		}
	}
	a, ok := h.at(path, indexLine)
	if !ok {
		return "", 0, "", false
	}
	text, ok = a.lineText()
	return text, a.line, a.label(), ok
}

// nthLine returns the 1-based line n of text.
func nthLine(text string, n int) (string, bool) {
	if n < 1 {
		return "", false
	}
	for i := 1; ; i++ {
		end := strings.IndexByte(text, '\n')
		if i == n {
			if end < 0 {
				return strings.TrimSuffix(text, "\r"), true
			}
			return strings.TrimSuffix(text[:end], "\r"), true
		}
		if end < 0 {
			return "", false
		}
		text = text[end+1:]
	}
}

// unsavedNote names the files whose text came from unsaved editor buffers in
// this call, and the files that changed on disk after an editor's unsaved
// changes to them, or is empty when there are none.
func (h *Handler) unsavedNote() string {
	c := &h.sources
	c.mu.Lock()
	unsaved := h.relPaths(c.unsaved)
	conflicts := h.relPaths(c.conflicts)
	c.mu.Unlock()
	var notes []string
	if len(unsaved) > 0 {
		notes = append(notes, "Note: read from unsaved editor buffers (the files on disk differ; line numbers are the buffer's): "+strings.Join(unsaved, ", "))
	}
	if len(conflicts) > 0 {
		notes = append(notes, "Note: read from disk, but an editor also has unsaved changes to these files, made before the files on disk changed; the two may conflict: "+strings.Join(conflicts, ", "))
	}
	return strings.Join(notes, "\n")
}

func (h *Handler) relPaths(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, h.relPath(p))
	}
	sort.Strings(out)
	return out
}
