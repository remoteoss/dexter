package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func openInEditor(t *testing.T, e *testEnv, rel, text string) {
	t.Helper()
	_, session, release := e.rt.AttachLSPSession()
	t.Cleanup(release)
	if err := session.DidOpen(context.Background(), &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri.File(filepath.Join(e.root, rel)), Text: text},
	}); err != nil {
		t.Fatal(err)
	}
}

// The rename tool never writes over unsaved work in an editor: a file with
// unsaved changes stops the whole rename, and the error names the file.
func TestRenameTool_UnsavedEditorBufferStopsTheRename(t *testing.T) {
	e := setupProject(t)
	accounts := readFile(t, e.root, "lib/my_app/accounts.ex")
	worker := readFile(t, e.root, "lib/my_app/worker.ex")
	openInEditor(t, e, "lib/my_app/worker.ex", worker+"# unsaved work\n")

	errText := e.callToolExpectError("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "function": "fetch_user", "new_name": "get_user",
	})
	wantContains(t, errText, filepath.Join("lib", "my_app", "worker.ex")+" has unsaved changes in an editor", "save or discard them there")
	if got := readFile(t, e.root, "lib/my_app/worker.ex"); got != worker {
		t.Errorf("worker.ex changed:\n%s", got)
	}
	if got := readFile(t, e.root, "lib/my_app/accounts.ex"); got != accounts {
		t.Errorf("accounts.ex changed:\n%s", got)
	}
}

// A file that an editor has open without unsaved changes is written; the
// editor reloads it.
func TestRenameTool_CleanEditorBufferIsWritten(t *testing.T) {
	e := setupProject(t)
	openInEditor(t, e, "lib/my_app/worker.ex", readFile(t, e.root, "lib/my_app/worker.ex"))
	out := e.callTool("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "function": "fetch_user", "new_name": "get_user",
	})
	wantContains(t, out, "Renamed MyApp.Accounts.fetch_user to get_user", "lib/my_app/worker.ex")
	wantContains(t, readFile(t, e.root, "lib/my_app/worker.ex"), "MyApp.Accounts.get_user(1)")
}

// Two MCP renames and an editor rename at the same time, all changing one
// file: every rename stays.
func TestRenameTool_ParallelWithEditorRename(t *testing.T) {
	e := setupTestEnv(t)
	e.indexFile("mix.exs", "defmodule MyApp.MixProject do\nend\n")
	e.indexFile("lib/a.ex", "defmodule MyApp.A do\n  def f(x), do: x\nend\n")
	e.indexFile("lib/b.ex", "defmodule MyApp.B do\n  def g(x), do: x\nend\n")
	cPath := e.indexFile("lib/c.ex", "defmodule MyApp.C do\n  def h(x), do: x\nend\n")
	e.indexFile("lib/user.ex", "defmodule MyApp.User do\n  def run(x), do: {MyApp.A.f(x), MyApp.B.g(x), MyApp.C.h(x)}\nend\n")

	_, editor, release := e.rt.AttachLSPSession()
	defer release()
	ctx := context.Background()
	if err := editor.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{
		URI: uri.File(cPath), Text: readFile(t, e.root, "lib/c.ex"),
	}}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var outA, outB string
	var editErr error
	var edit interface{}
	wg.Add(3)
	go func() {
		defer wg.Done()
		outA = e.callTool("dexter_rename_symbol", map[string]any{"module": "MyApp.A", "function": "f", "new_name": "f2"})
	}()
	go func() {
		defer wg.Done()
		outB = e.callTool("dexter_rename_symbol", map[string]any{"module": "MyApp.B", "function": "g", "new_name": "g2"})
	}()
	go func() {
		defer wg.Done()
		edit, editErr = editor.RenameEdit(ctx, &protocol.RenameParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.File(cPath)},
				Position:     protocol.Position{Line: 1, Character: 6},
			},
			NewName: "h2",
		})
	}()
	wg.Wait()
	if editErr != nil || edit == nil {
		t.Fatalf("editor rename: %v", editErr)
	}
	wantContains(t, outA, "Renamed MyApp.A.f to f2")
	wantContains(t, outB, "Renamed MyApp.B.g to g2")
	user := readFile(t, e.root, "lib/user.ex")
	for _, want := range []string{"MyApp.A.f2(x)", "MyApp.B.g2(x)", "MyApp.C.h2(x)"} {
		if !strings.Contains(user, want) {
			t.Errorf("user.ex lost a rename, want %q:\n%s", want, user)
		}
	}
}
