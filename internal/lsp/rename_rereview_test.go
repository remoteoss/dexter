package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"golang.org/x/text/unicode/norm"

	"github.com/remoteoss/dexter/internal/parser"
)

// Where the file system keeps NFC and NFD names apart (Linux), they are two
// files, and an editor that has one open must not hold the other.
func TestDocKey_NormalizationOnlyWhereFileSystemIgnoresIt(t *testing.T) {
	dir := t.TempDir()
	nfc := filepath.Join(dir, "lib", norm.NFC.String("béta.ex"))
	nfd := filepath.Join(dir, "lib", norm.NFD.String("béta.ex"))
	if err := os.MkdirAll(filepath.Dir(nfc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nfc, []byte(safetyCallB), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nfd, []byte("defmodule MyApp.Other do\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a, b := readFile(t, nfc), readFile(t, nfd); a == b {
		// One file under two names: the file system ignores the
		// normalization, and the key must join the names.
		if docKey(string(uri.File(nfc))) != docKey(string(uri.File(nfd))) {
			t.Fatal("the file system joins NFC and NFD names, but the keys differ")
		}
		return
	}
	if docKey(string(uri.File(nfc))) == docKey(string(uri.File(nfd))) {
		t.Fatalf("NFC and NFD names are two files on %s, but they have one key", runtime.GOOS)
	}

	servers, st := sessionsAt(t, dir, 2)
	a, b := servers[0], servers[1]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	defs, refs, err := parser.ParseFile(nfc)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.IndexFileWithRefs(nfc, defs, refs); err != nil {
		t.Fatal(err)
	}
	openIn(t, b, nfd, "defmodule MyApp.Other do\n  # unsaved\nend\n")
	edit, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
	if err != nil {
		t.Fatal(err)
	}
	a.index.backgroundWork.Wait()
	if got := readFile(t, nfc); !strings.Contains(got, "MyApp.Alpha.bar(1)") {
		t.Errorf("the NFC file, which no editor has open, was not written:\n%s", got)
	}
	if len(collectEdits(edit, nfc)) != 0 {
		t.Errorf("the NFC file went into the edit although only the NFD file is open")
	}
}

// Editors change a buffer they load without a change by the user: Neovim's
// 'fixeol' adds a final newline, and CRLF can show as LF. A headless rename
// treats such a buffer as clean.
func TestSameText(t *testing.T) {
	cases := []struct {
		buffer, disk string
		want         bool
	}{
		{"a\nb\n", "a\nb\n", true},
		{"a\nb\n", "a\nb", true},
		{"a\nb", "a\nb\n", true},
		{"a\nb\n", "a\r\nb\r\n", true},
		{"a\nb\n", "a\r\nb", true},
		{"a\nb\n\n", "a\nb", false},
		{"a\nc\n", "a\nb\n", false},
		{"a\nb\n ", "a\nb\n", false},
	}
	for _, c := range cases {
		if got := sameText(c.buffer, c.disk); got != c.want {
			t.Errorf("sameText(%q, %q) = %v, want %v", c.buffer, c.disk, got, c.want)
		}
	}
}

func TestRenameHeadless_FixeolAndCRLFBufferIsClean(t *testing.T) {
	for name, c := range map[string]struct{ disk, buffer string }{
		"fixeol": {strings.TrimSuffix(safetyCallB, "\n"), safetyCallB},
		"crlf":   {strings.ReplaceAll(safetyCallB, "\n", "\r\n"), safetyCallB},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			servers, st := sessionsAt(t, dir, 2)
			core, editor := servers[0], servers[1]
			indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
			indexFile(t, st, dir, "lib/beta.ex", c.disk)
			bPath := filepath.Join(dir, "lib/beta.ex")
			openIn(t, editor, bPath, c.buffer)
			if _, err := core.RenameFunctionHeadless(context.Background(), "MyApp.Alpha", "foo", "bar"); err != nil {
				t.Fatal(err)
			}
			core.index.backgroundWork.Wait()
			if got := readFile(t, bPath); !strings.Contains(got, "MyApp.Alpha.bar(1)") {
				t.Errorf("beta.ex not written:\n%q", got)
			}
		})
	}
}

// An editor that types while a rename stages its files does not wait for
// the rename; only the short check and replace at the end hold documents.
func TestRename_DidChangeDoesNotWaitForStaging(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 2)
	a, b := servers[0], servers[1]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	indexFile(t, st, dir, "lib/other.ex", "defmodule MyApp.Other do\n  def x, do: 1\nend\n")
	oPath := filepath.Join(dir, "lib/other.ex")
	openIn(t, b, oPath, readFile(t, oPath))

	staging := make(chan struct{})
	release := make(chan struct{})
	orig := renameWriteTemp
	t.Cleanup(func() { renameWriteTemp = orig })
	var once sync.Once
	renameWriteTemp = func(f *os.File, data []byte) error {
		once.Do(func() {
			close(staging)
			<-release
		})
		return orig(f, data)
	}

	done := make(chan error, 1)
	go func() {
		_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
		done <- err
	}()
	<-staging
	changed := make(chan struct{})
	go func() {
		_ = b.DidChange(context.Background(), &protocol.DidChangeTextDocumentParams{
			TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri.File(oPath)}, Version: 2},
			ContentChanges: []protocol.TextDocumentContentChangeEvent{{Text: "defmodule MyApp.Other do\nend\n"}},
		})
		close(changed)
	}()
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Error("didChange waited for the rename to stage its files")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-changed
}

// A file that another editor opens after the rename read it, and before the
// writes, stops the rename cleanly: no file changes, no temp file stays.
func TestRename_FileOpenedBeforeCommitAborts(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 2)
	a, b := servers[0], servers[1]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	bPath := filepath.Join(dir, "lib/beta.ex")
	t.Cleanup(func() { renameTestHookBeforeCommit = nil })
	renameTestHookBeforeCommit = func() { openIn(t, b, bPath, safetyUnsavB) }

	_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
	if err == nil || !strings.Contains(err.Error(), "opened "+filepath.Join("lib", "beta.ex")+" while the rename ran") {
		t.Fatalf("want a refusal that names beta.ex, got %v", err)
	}
	a.index.backgroundWork.Wait()
	if got := readFile(t, bPath); got != safetyCallB {
		t.Errorf("beta.ex changed:\n%s", got)
	}
	if got := readFile(t, filepath.Join(dir, "lib/alpha.ex")); got != safetyDefA {
		t.Errorf("alpha.ex changed:\n%s", got)
	}
	noTempFiles(t, dir)
}

// blockingShow is an editor whose window/showMessage does not return until
// the test lets it.
type blockingShow struct {
	protocol.Client
	shown   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingShow) ShowMessage(context.Context, *protocol.ShowMessageParams) error {
	c.once.Do(func() { close(c.shown) })
	<-c.release
	return nil
}

// A rename sends its messages after it ends, so an editor that is slow to
// read them does not hold up the next rename.
func TestRename_MessagesSentAfterTheRenameEnds(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 3)
	a, b, c := servers[0], servers[1], servers[2]
	slow := &blockingShow{shown: make(chan struct{}), release: make(chan struct{})}
	a.client = slow
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	indexFile(t, st, dir, "lib/gamma.ex", "defmodule MyApp.Gamma do\n  def baz(x), do: x\nend\n")
	openIn(t, c, filepath.Join(dir, "lib/beta.ex"), safetyCallB) // A gets the Info notice

	doneA := make(chan error, 1)
	go func() {
		_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
		doneA <- err
	}()
	<-slow.shown
	doneB := make(chan error, 1)
	go func() {
		_, err := b.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/gamma.ex"))), 1, 6, "qux"))
		doneB <- err
	}()
	select {
	case err := <-doneB:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("a rename waited for another editor to read its message")
	}
	close(slow.release)
	if err := <-doneA; err != nil {
		t.Fatal(err)
	}
}

// A rename removes the temp files that a stopped rename left in the
// directories it writes, and no other file.
func TestRename_RemovesStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot run a child process")
	}
	stale := filepath.Join(dir, "lib", parser.RenameTempName("beta.ex", cmd.Process.Pid, 7))
	keep := filepath.Join(dir, "lib", ".beta.ex.swp")
	for _, p := range []string{stale, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("the stale temp file is still there")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("a file that is not a rename temp file was removed")
	}
}
