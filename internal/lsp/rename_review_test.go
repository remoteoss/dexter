package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"golang.org/x/text/unicode/norm"

	"github.com/remoteoss/dexter/internal/parser"
)

// Two renames in two sessions at the same time must both reach a closed file
// that both change. Each reads the file and writes it whole, so without one
// rename at a time the second write drops the first rename.
func TestRename_ConcurrentRenamesKeepBothChanges(t *testing.T) {
	const gamma = "defmodule MyApp.Gamma do\n  def baz(x), do: x\nend\n"
	const both = "defmodule MyApp.Beta do\n  def run, do: MyApp.Alpha.foo(1)\n  def go, do: MyApp.Gamma.baz(2)\nend\n"
	for i := 0; i < 30; i++ {
		dir := t.TempDir()
		servers, st := sessionsAt(t, dir, 2)
		a, b := servers[0], servers[1]
		indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
		indexFile(t, st, dir, "lib/gamma.ex", gamma)
		indexFile(t, st, dir, "lib/beta.ex", both)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for j, r := range []struct {
			s    *Server
			file string
		}{{a, "lib/alpha.ex"}, {b, "lib/gamma.ex"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				name := map[int]string{0: "bar", 1: "qux"}[j]
				_, errs[j] = r.s.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, r.file))), 1, 6, name))
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		got := readFile(t, filepath.Join(dir, "lib/beta.ex"))
		if !strings.Contains(got, "MyApp.Alpha.bar(1)") || !strings.Contains(got, "MyApp.Gamma.qux(2)") {
			t.Fatalf("run %d: one rename was lost:\n%s", i, got)
		}
	}
}

// noTempFiles checks that a rename left no temp file in dir.
func noTempFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".dexter-") && strings.HasSuffix(d.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", p)
		}
		return nil
	})
}

// A write that fails while the new texts are prepared (a full disk, for
// example) must leave every file as it was: no file empty, none partly
// written, none renamed.
func TestRename_WriteFailureChangesNothing(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	shown := &shownMessages{}
	a.client = shown
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	const other = "defmodule MyApp.Other do\n  def go, do: MyApp.Alpha.foo(2)\nend\n"
	indexFile(t, st, dir, "lib/other.ex", other)

	orig := renameWriteTemp
	t.Cleanup(func() { renameWriteTemp = orig })
	renameWriteTemp = func(f *os.File, data []byte) error {
		if strings.Contains(f.Name(), "beta.ex") {
			_, _ = f.Write(data[:len(data)/2])
			return errors.New("no space left on device")
		}
		return orig(f, data)
	}

	_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
	if err == nil || !strings.Contains(err.Error(), "beta.ex") || !strings.Contains(err.Error(), "No file was changed") {
		t.Fatalf("want a refusal that names beta.ex, got %v", err)
	}
	a.index.backgroundWork.Wait()
	for path, want := range map[string]string{"lib/alpha.ex": safetyDefA, "lib/beta.ex": safetyCallB, "lib/other.ex": other} {
		if got := readFile(t, filepath.Join(dir, path)); got != want {
			t.Errorf("%s changed:\n%s", path, got)
		}
	}
	noTempFiles(t, dir)
}

// A replace that fails takes back every replace done before it: the rename
// is all or nothing, and the message names the file that failed.
func TestRename_ReplaceFailureTakesTheRenameBack(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	shown := &shownMessages{}
	a.client = shown
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	const other = "defmodule MyApp.Other do\n  def go, do: MyApp.Alpha.foo(2)\nend\n"
	indexFile(t, st, dir, "lib/other.ex", other)

	orig := renameSwap
	t.Cleanup(func() { renameSwap = orig })
	renameSwap = func(tmp, target string) error {
		if strings.HasSuffix(target, "beta.ex") {
			time.Sleep(20 * time.Millisecond) // the other replaces are done first
			return errors.New("input/output error")
		}
		return orig(tmp, target)
	}

	_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
	if err == nil || !strings.Contains(err.Error(), "could not replace "+filepath.Join("lib", "beta.ex")+" (input/output error). The files it had already replaced were restored; no file was changed") {
		t.Fatalf("want the all-or-nothing refusal, got %v", err)
	}
	a.index.backgroundWork.Wait()
	for rel, want := range map[string]string{"lib/alpha.ex": safetyDefA, "lib/beta.ex": safetyCallB, "lib/other.ex": other} {
		if got := readFile(t, filepath.Join(dir, rel)); got != want {
			t.Errorf("%s changed:\n%s", rel, got)
		}
	}
	noTempFiles(t, dir)
}

// Without a backup (a file system without hard links), a replace that was
// done cannot be taken back; the message names exactly those files.
func TestRename_ReplaceFailureWithoutBackupIsReportedExactly(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	shown := &shownMessages{}
	a.client = shown
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)

	origReplace, origBackup, origSwap := renameReplace, renameBackup, renameSwap
	t.Cleanup(func() { renameReplace, renameBackup, renameSwap = origReplace, origBackup, origSwap })
	renameSwap = func(string, string) error { return errSwapUnsupported }
	renameBackup = func(string, string) error { return errors.New("operation not supported") }
	renameReplace = func(from, to string) error {
		if strings.HasSuffix(to, "beta.ex") {
			time.Sleep(20 * time.Millisecond)
			return errors.New("input/output error")
		}
		return origReplace(from, to)
	}
	_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
	if err == nil {
		t.Fatal("want an error")
	}
	if got := readFile(t, filepath.Join(dir, "lib/beta.ex")); got != safetyCallB {
		t.Errorf("beta.ex changed although its replace failed:\n%s", got)
	}
	msgs := shown.all()
	if len(msgs) != 1 || !strings.Contains(msgs[0], "could not change 1 files: "+filepath.Join("lib", "alpha.ex")) ||
		!strings.Contains(msgs[0], "the rename failed at "+filepath.Join("lib", "beta.ex")) {
		t.Errorf("message does not say exactly what changed: %q", msgs)
	}
	noTempFiles(t, dir)
}

// A move whose new file cannot be put in place leaves the old file, and no
// part of the new one; the rest of the rename is taken back.
func TestRenameModule_FailedMoveLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	orig := renameReplace
	t.Cleanup(func() { renameReplace = orig })
	renameReplace = func(from, to string) error {
		if strings.HasSuffix(to, "gamma.ex") {
			time.Sleep(20 * time.Millisecond)
			return errors.New("input/output error")
		}
		return orig(from, to)
	}
	_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/beta.ex"))), 1, 22, "Gamma"))
	if err == nil || !strings.Contains(err.Error(), "no file was changed") {
		t.Fatalf("want the all-or-nothing refusal, got %v", err)
	}
	a.index.backgroundWork.Wait()
	if got := readFile(t, filepath.Join(dir, "lib/alpha.ex")); got != safetyDefA {
		t.Errorf("alpha.ex changed although its move failed:\n%s", got)
	}
	if got := readFile(t, filepath.Join(dir, "lib/beta.ex")); got != safetyCallB {
		t.Errorf("beta.ex was not restored:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "lib/gamma.ex")); err == nil {
		t.Error("a partial gamma.ex was left")
	}
	noTempFiles(t, dir)
}

// The atomic replace keeps the file mode and writes through a symlink.
func TestRename_ReplaceKeepsModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	real := filepath.Join(dir, "shared", "beta_real.ex")
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(safetyCallB), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "lib", "beta.ex")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	indexFile(t, st, dir, "lib/other.ex", "defmodule MyApp.Other do\n  def go, do: MyApp.Alpha.foo(2)\nend\n")
	if err := os.Chmod(filepath.Join(dir, "lib/other.ex"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Index the link like the walk does: by its own path.
	defsB, refsB, err := parser.ParseFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.IndexFileWithRefs(link, defsB, refsB); err != nil {
		t.Fatal(err)
	}

	if _, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar")); err != nil {
		t.Fatal(err)
	}
	a.index.backgroundWork.Wait()
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("lib/beta.ex is no longer a symlink")
	}
	if got := readFile(t, real); !strings.Contains(got, "MyApp.Alpha.bar(1)") {
		t.Errorf("the link target was not renamed:\n%s", got)
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode of the link target = %v, want 0600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(dir, "lib/other.ex")); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode of other.ex = %v, want 0640", fi.Mode().Perm())
	}
}

// A buffer row that the rename's index goroutine writes after the editor
// closed the buffer without saving would keep text that is on no disk.
func TestRename_BufferClosedBeforeReindexIndexesDisk(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	aPath, bPath := filepath.Join(dir, "lib/alpha.ex"), filepath.Join(dir, "lib/beta.ex")
	openIn(t, a, aPath, safetyDefA)
	openIn(t, a, bPath, safetyCallB)

	a.index.reindexing.Lock() // a long pass holds the lock
	if _, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(aPath)), 1, 6, "bar")); err != nil {
		a.index.reindexing.Unlock()
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, p := range []string{aPath, bPath} {
		if err := a.DidClose(ctx, &protocol.DidCloseTextDocumentParams{TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(p)}}); err != nil {
			t.Fatal(err)
		}
	}
	a.index.reindexing.Unlock()
	a.index.backgroundWork.Wait()

	if defs, _ := st.LookupFunction("MyApp.Alpha", "bar"); len(defs) != 0 {
		t.Error("the index has bar, which is on no disk and in no buffer")
	}
	if refs, _ := st.LookupReferences("MyApp.Alpha", "bar"); len(refs) != 0 {
		t.Errorf("the index has references to bar: %+v", refs)
	}
}

// An editor can spell a file with decomposed Unicode (NFD), with other case
// (on a volume that ignores case), or with "./" in it. Each must still count
// as that file being open.
func TestRename_OtherSpellingsOfAnOpenFile(t *testing.T) {
	cases := map[string]func(dir, path string) string{
		"nfd": func(_, path string) string { return "file://" + norm.NFD.String(path) },
		"dot": func(dir, _ string) string {
			return "file://" + filepath.ToSlash(dir) + "/lib/./béta.ex"
		},
		"case": func(dir, _ string) string {
			return "file://" + filepath.ToSlash(dir) + "/LIB/BÉTA.ex"
		},
	}
	for name, spell := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if name == "case" {
				a, err1 := os.Stat(dir)
				b, err2 := os.Stat(strings.ToUpper(dir))
				if err1 != nil || err2 != nil || !os.SameFile(a, b) {
					t.Skip("the volume keeps case apart")
				}
			}
			servers, st := sessionsAt(t, dir, 2)
			a, b := servers[0], servers[1]
			indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
			indexFile(t, st, dir, "lib/béta.ex", safetyCallB)
			bPath := filepath.Join(dir, "lib", "béta.ex")
			if name == "nfd" {
				// On Linux the NFD name is another file; see
				// TestDocKey_NormalizationOnlyWhereFileSystemIgnoresIt.
				x, err1 := os.Stat(bPath)
				y, err2 := os.Stat(norm.NFD.String(bPath))
				if err1 != nil || err2 != nil || !os.SameFile(x, y) {
					t.Skip("the file system keeps NFC and NFD names apart")
				}
			}
			bURI := spell(dir, bPath)
			if err := b.DidOpen(context.Background(), &protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{URI: protocol.DocumentURI(bURI), Text: safetyUnsavB}}); err != nil {
				t.Fatal(err)
			}
			edit, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
			if err != nil {
				t.Fatal(err)
			}
			a.index.backgroundWork.Wait()
			if got := readFile(t, bPath); got != safetyCallB {
				t.Errorf("béta.ex was written on disk under the other editor's buffer:\n%s", got)
			}
			if len(collectEdits(edit, bPath)) == 0 {
				t.Errorf("béta.ex is not in A's edit: %+v", edit)
			}
			// Closing under the same spelling releases the file.
			if err := b.DidClose(context.Background(), &protocol.DidCloseTextDocumentParams{TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(bURI)}}); err != nil {
				t.Fatal(err)
			}
			if busy := a.index.openDocs.heldTexts(a.docs, bPath); len(busy) != 0 {
				t.Errorf("the registry still has %v open after close", busy)
			}
		})
	}
}

// A module move needs the source directory (to remove the old file) and the
// destination directory. A read-only directory stops the rename before any
// change.
func TestRenameModule_ReadOnlyDirectoryRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only directory")
	}
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	a := servers[0]
	indexFile(t, st, dir, "lib/ro/alpha.ex", strings.Replace(safetyDefA, "MyApp.Alpha", "MyApp.Ro.Alpha", 1))
	indexFile(t, st, dir, "lib/beta.ex", strings.Replace(safetyCallB, "MyApp.Alpha", "MyApp.Ro.Alpha", 1))
	roDir := filepath.Join(dir, "lib/ro")
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o755) })

	_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/beta.ex"))), 1, 25, "Gamma"))
	if err == nil || !strings.Contains(err.Error(), "alpha.ex") {
		t.Fatalf("want a refusal that names alpha.ex, got %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "lib/beta.ex")); strings.Contains(got, "Gamma") {
		t.Errorf("beta.ex changed although the rename was refused:\n%s", got)
	}
}

// A document that is not a file (VS Code's untitled: buffers) must not take
// the daemon down.
func TestUntitledDocumentDoesNotPanic(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	ctx := context.Background()
	u := protocol.DocumentURI("untitled:Untitled-1")
	text := "defmodule Scratch do\n  def foo, do: bar()\n  def bar, do: 1\nend\n"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(server.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{URI: u, Text: text}}))
	must(server.DidChange(ctx, &protocol.DidChangeTextDocumentParams{
		TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: u}, Version: 2},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{{Text: text}},
	}))
	pos := protocol.TextDocumentPositionParams{TextDocument: protocol.TextDocumentIdentifier{URI: u}, Position: protocol.Position{Line: 1, Character: 16}}
	_, _ = server.Hover(ctx, &protocol.HoverParams{TextDocumentPositionParams: pos})
	_, _ = server.Definition(ctx, &protocol.DefinitionParams{TextDocumentPositionParams: pos})
	_, _ = server.RenameEdit(ctx, &protocol.RenameParams{TextDocumentPositionParams: pos, NewName: "baz"})
	must(server.DidSave(ctx, &protocol.DidSaveTextDocumentParams{TextDocument: protocol.TextDocumentIdentifier{URI: u}}))
	must(server.DidClose(ctx, &protocol.DidCloseTextDocumentParams{TextDocument: protocol.TextDocumentIdentifier{URI: u}}))
}

// An editor rename and a headless rename (an MCP tool) at the same time,
// both changing one closed file: both changes must stay. Each reads the file
// and writes it whole, so they must run one after the other.
func TestRename_ConcurrentEditorAndHeadlessKeepBothChanges(t *testing.T) {
	const gamma = "defmodule MyApp.Gamma do\n  def baz(x), do: x\nend\n"
	const both = "defmodule MyApp.Beta do\n  def run, do: MyApp.Alpha.foo(1)\n  def go, do: MyApp.Gamma.baz(2)\nend\n"
	for i := 0; i < 30; i++ {
		dir := t.TempDir()
		servers, st := sessionsAt(t, dir, 2)
		editor, core := servers[0], servers[1]
		indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
		indexFile(t, st, dir, "lib/gamma.ex", gamma)
		indexFile(t, st, dir, "lib/beta.ex", both)
		var wg sync.WaitGroup
		var errEditor, errHeadless error
		var indexed <-chan struct{}
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errEditor = editor.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
		}()
		go func() {
			defer wg.Done()
			indexed, errHeadless = core.RenameFunctionHeadless(context.Background(), "MyApp.Gamma", "baz", "qux")
		}()
		wg.Wait()
		if errEditor != nil || errHeadless != nil {
			t.Fatalf("editor: %v, headless: %v", errEditor, errHeadless)
		}
		<-indexed
		got := readFile(t, filepath.Join(dir, "lib/beta.ex"))
		if !strings.Contains(got, "MyApp.Alpha.bar(1)") || !strings.Contains(got, "MyApp.Gamma.qux(2)") {
			t.Fatalf("run %d: one rename was lost:\n%s", i, got)
		}
	}
}
