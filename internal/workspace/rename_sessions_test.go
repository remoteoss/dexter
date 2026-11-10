package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// Two editors attached to one runtime: a rename in one never writes a file
// that the other has open. The change goes into the requesting editor's
// edit. A headless rename (MCP, CLI) writes a file that is open without
// unsaved changes, and stops when an editor has unsaved changes.
func TestRenameAcrossSessions(t *testing.T) {
	const alpha = "defmodule MyApp.Alpha do\n  def foo(x), do: x\nend\n"
	const beta = "defmodule MyApp.Beta do\n  def run, do: MyApp.Alpha.foo(1)\nend\n"
	setup := func(t *testing.T) (*Runtime, string, string) {
		t.Setenv("PATH", t.TempDir())
		t.Setenv("SHELL", "/bin/false")
		root := t.TempDir()
		aPath := filepath.Join(root, "lib", "alpha.ex")
		bPath := filepath.Join(root, "lib", "beta.ex")
		if err := os.MkdirAll(filepath.Dir(aPath), 0o755); err != nil {
			t.Fatal(err)
		}
		for path, text := range map[string]string{aPath: alpha, bPath: beta} {
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		rt, err := OpenWithOptions(root, Options{NoWatch: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rt.Close() })
		if err := rt.WaitReady(testContext(t, 30*time.Second)); err != nil {
			t.Fatal(err)
		}
		return rt, aPath, bPath
	}
	ctx := context.Background()
	open := func(t *testing.T, s interface {
		DidOpen(context.Context, *protocol.DidOpenTextDocumentParams) error
	}, path, text string) {
		t.Helper()
		if err := s.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{URI: uri.File(path), Text: text}}); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		data, _ := os.ReadFile(path)
		return string(data)
	}

	for name, bufferB := range map[string]string{"clean": beta, "unsaved": beta + "# unsaved work\n"} {
		t.Run("editor requester, "+name+" buffer in the other editor", func(t *testing.T) {
			rt, aPath, bPath := setup(t)
			_, a, releaseA := rt.AttachLSPSession()
			defer releaseA()
			_, b, releaseB := rt.AttachLSPSession()
			defer releaseB()
			open(t, b, bPath, bufferB)
			open(t, a, aPath, alpha)
			edit, err := a.RenameEdit(ctx, &protocol.RenameParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(aPath)},
					Position:     protocol.Position{Line: 1, Character: 6},
				},
				NewName: "bar",
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := read(bPath); got != beta {
				t.Fatalf("beta.ex was written under the other editor's buffer:\n%s", got)
			}
			if edits := edit.Changes[uri.File(bPath)]; len(edits) == 0 || edits[0].NewText != "bar" {
				t.Fatalf("beta.ex is not in the requesting editor's edit: %+v", edit.Changes)
			}
		})
	}

	t.Run("headless requester", func(t *testing.T) {
		rt, aPath, bPath := setup(t)
		_, b, releaseB := rt.AttachLSPSession()
		defer releaseB()
		open(t, b, bPath, beta+"# unsaved work\n")
		_, err := rt.LanguageServices().RenameFunctionHeadless(context.Background(), "MyApp.Alpha", "foo", "bar")
		if err == nil || !strings.Contains(err.Error(), filepath.Join("lib", "beta.ex")+" has unsaved changes") {
			t.Fatalf("want a refusal that names lib/beta.ex, got %v", err)
		}
		if read(aPath) != alpha || read(bPath) != beta {
			t.Fatal("a file changed although the headless rename stopped")
		}

		// The same buffer without unsaved changes: the file is written.
		if err := b.DidChange(ctx, &protocol.DidChangeTextDocumentParams{
			TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri.File(bPath)}, Version: 2},
			ContentChanges: []protocol.TextDocumentContentChangeEvent{{Text: beta}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := rt.LanguageServices().RenameFunctionHeadless(context.Background(), "MyApp.Alpha", "foo", "bar"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(read(bPath), "MyApp.Alpha.bar(1)") || !strings.Contains(read(aPath), "def bar") {
			t.Fatalf("the headless rename did not write the files:\n%s", read(bPath))
		}
	})
}

// A site file that another program moves (git mv) right before a rename: the
// rename waits for the watcher, and the call site at the new path is renamed.
func TestRenameAfterUnrecordedMoveWithWatcher(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	root := t.TempDir()
	const alpha = "defmodule MyApp.Alpha do\n  def foo(x), do: x\nend\n"
	const beta = "defmodule MyApp.Beta do\n  def run, do: MyApp.Alpha.foo(1)\nend\n"
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"alpha.ex": alpha, "beta.ex": beta} {
		if err := os.WriteFile(filepath.Join(root, "lib", name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !rt.Watching() {
		if time.Now().After(deadline) {
			t.Skip("no native watcher on this system")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Let the watcher settle after the start.
	if err := rt.FlushEvents(testContext(t, 5*time.Second)); err != nil {
		t.Fatal(err)
	}

	moved := filepath.Join(root, "lib", "beta_moved.ex")
	if err := os.Rename(filepath.Join(root, "lib", "beta.ex"), moved); err != nil {
		t.Fatal(err)
	}
	indexed, err := rt.LanguageServices().RenameFunctionHeadless(context.Background(), "MyApp.Alpha", "foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	<-indexed
	data, _ := os.ReadFile(moved)
	if !strings.Contains(string(data), "MyApp.Alpha.bar(1)") {
		t.Fatalf("the call at the new path was not renamed:\n%s", data)
	}
}
