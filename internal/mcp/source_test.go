package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// Regression: dexter_file_outline read any path the agent gave, so an
// absolute path or one with .. could read every file the user can read, and
// /dev/zero filled the daemon's memory.
func TestFileOutline_RefusesPathsOutsideRoot(t *testing.T) {
	e := setupProject(t)
	outside := filepath.Join(t.TempDir(), "secret.ex")
	if err := os.WriteFile(outside, []byte("defmodule Secret do\n  def key, do: 1\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.root, "lib", "link.ex")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(e.root, "lib", "zero.ex")); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(e.root, outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, rel, "lib/link.ex", "/dev/zero", "lib/zero.ex", "../" + filepath.Base(e.root) + "x/a.ex"} {
		errText := e.callToolExpectError("dexter_file_outline", map[string]any{"file": p})
		wantContains(t, errText, "outside the project root")
		wantNotContains(t, errText, "Secret")
	}

	// A symlink that stays inside the root is still fine.
	if err := os.Symlink(filepath.Join(e.root, "lib/my_app/worker.ex"), filepath.Join(e.root, "lib", "alias.ex")); err != nil {
		t.Fatal(err)
	}
	wantContains(t, e.callTool("dexter_file_outline", map[string]any{"file": "lib/alias.ex"}), "defmodule MyApp.Worker")
}

func TestFileOutline_RefusesLargeFilesAndDirectories(t *testing.T) {
	e := setupProject(t)
	big := filepath.Join(e.root, "lib", "big.ex")
	if err := os.WriteFile(big, make([]byte, maxSourceBytes+1), 0644); err != nil {
		t.Fatal(err)
	}
	wantContains(t, e.callToolExpectError("dexter_file_outline", map[string]any{"file": "lib/big.ex"}), "MB limit")
	wantContains(t, e.callToolExpectError("dexter_file_outline", map[string]any{"file": "lib"}), "not a regular file")
	wantContains(t, e.callTool("dexter_file_outline", map[string]any{"file": "lib/missing.ex"}), "File not found: lib/missing.ex")
}

// Regression: a function with thousands of clauses gave an answer of hundreds
// of kilobytes, and each clause read and tokenized the file again.
func TestDefinitionTool_CapsClauses(t *testing.T) {
	e := setupProject(t)
	var src strings.Builder
	src.WriteString("defmodule MyApp.Big do\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&src, "  def code(%d), do: %d\n", i, i)
	}
	src.WriteString("end\n")
	e.indexFile("lib/my_app/big.ex", src.String())

	out := e.callTool("dexter_definition", map[string]any{"module": "MyApp.Big", "function": "code"})
	if got := strings.Count(out, "MyApp.Big.code/1 (def)"); got != 20 {
		t.Errorf("answer shows %d clauses, want 20:\n%s", got, out)
	}
	wantContains(t, out, "… and 280 more clause(s) not shown.")
}

// MCP answers from what the user sees: a buffer that an attached editor holds
// open, with changes not yet saved, wins over the disk, and the answer says so.
// The index positions refer to the saved file, so they are mapped into the
// buffer.
func TestTools_SeeUnsavedEditorBuffers(t *testing.T) {
	e := setupProject(t)
	_, session, release := e.rt.AttachLSPSession()
	t.Cleanup(release)

	change := func(rel, text string) {
		t.Helper()
		path := filepath.Join(e.root, rel)
		if err := session.DidChange(context.Background(), &protocol.DidChangeTextDocumentParams{
			TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(uri.File(path))}},
			ContentChanges: []protocol.TextDocumentContentChangeEvent{{Text: text}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A new module at the end of the buffer shows in the outline.
	change("lib/my_app/worker.ex", workerSource+"\ndefmodule MyApp.Draft do\n  def draft_only(x), do: x\nend\n")
	out := e.callTool("dexter_file_outline", map[string]any{"file": "lib/my_app/worker.ex"})
	wantContains(t, out, "defmodule MyApp.Draft", "draft_only/1", "unsaved editor buffers", "lib/my_app/worker.ex")

	// Two lines inserted at the top move every reference down by two.
	change("lib/my_app/worker.ex", "# draft\n# notes\n"+workerSource)
	refs := e.callTool("dexter_references", map[string]any{"module": "MyApp.Accounts", "function": "fetch_user"})
	wantContains(t, refs, "5: MyApp.Accounts.fetch_user(1)", "10: MyApp.Accounts.fetch_user(2)", "unsaved editor buffers")

	// A reference on a line that the buffer changed shows the saved line,
	// marked as such.
	change("lib/my_app/worker.ex", strings.Replace(workerSource, "MyApp.Accounts.fetch_user(1)", "MyApp.Accounts.fetch_user(id)", 1))
	refs = e.callTool("dexter_references", map[string]any{"module": "MyApp.Accounts", "function": "fetch_user"})
	wantContains(t, refs, "3: MyApp.Accounts.fetch_user(1) (saved text; this part is changed in an unsaved editor buffer)", "8: MyApp.Accounts.fetch_user(2)\n")

	// A definition below an inserted function: the head, @spec, and @doc come
	// from the buffer at the moved line.
	change("lib/my_app/accounts.ex", strings.Replace(accountsSource, "  @doc \"\"\"\n  Fetches", "  def helper, do: :ok\n\n  @doc \"\"\"\n  Fetches a user, unsaved.\n  Fetches", 1))
	def := e.callTool("dexter_definition", map[string]any{"module": "MyApp.Accounts", "function": "fetch_user"})
	wantContains(t, def, "lib/my_app/accounts.ex:13", "@spec fetch_user(integer())", "def fetch_user(id) do", "Fetches a user, unsaved.", "unsaved editor buffers")

	// A buffer that matches the disk is not unsaved.
	change("lib/my_app/worker.ex", workerSource)
	change("lib/my_app/accounts.ex", accountsSource)
	out = e.callTool("dexter_file_outline", map[string]any{"file": "lib/my_app/worker.ex"})
	wantNotContains(t, out, "MyApp.Draft", "unsaved")
	def = e.callTool("dexter_definition", map[string]any{"module": "MyApp.Accounts", "function": "fetch_user"})
	wantContains(t, def, "lib/my_app/accounts.ex:10", "def fetch_user(id) do")
	wantNotContains(t, def, "unsaved")
}

func TestSourceViewLocate(t *testing.T) {
	disk := "a\nb\nc\nd\ne"
	v := newSourceView("a\nX\nY\nc\nd\ne", true, disk, true) // b replaced by X, Y
	for line, want := range map[int][2]int{1: {1, 1}, 2: {2, 0}, 3: {4, 1}, 5: {6, 1}} {
		got, in := v.locate(line)
		if in != (want[1] == 1) || (in && got != want[0]) {
			t.Errorf("locate(%d) = %d, %v; want %v", line, got, in, want)
		}
	}
}
