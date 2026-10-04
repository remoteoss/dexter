package mcp

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/lsp"
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

// editorSession is an attached editor session that a test drives.
type editorSession struct {
	t    *testing.T
	root string
	srv  *lsp.Server
}

func attachEditor(t *testing.T, e *testEnv) *editorSession {
	t.Helper()
	_, session, release := e.rt.AttachLSPSession()
	t.Cleanup(release)
	return &editorSession{t: t, root: e.root, srv: session}
}

func (s *editorSession) uri(rel string) protocol.DocumentURI {
	return protocol.DocumentURI(uri.File(filepath.Join(s.root, rel)))
}

func (s *editorSession) open(rel, text string) {
	s.t.Helper()
	if err := s.srv.DidOpen(context.Background(), &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: s.uri(rel), LanguageID: "elixir", Version: 1, Text: text},
	}); err != nil {
		s.t.Fatal(err)
	}
}

func (s *editorSession) change(rel, text string) {
	s.t.Helper()
	if err := s.srv.DidChange(context.Background(), &protocol.DidChangeTextDocumentParams{
		TextDocument:   protocol.VersionedTextDocumentIdentifier{TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: s.uri(rel)}},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{{Text: text}},
	}); err != nil {
		s.t.Fatal(err)
	}
}

func (s *editorSession) save(rel string) {
	s.t.Helper()
	if err := s.srv.DidSave(context.Background(), &protocol.DidSaveTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: s.uri(rel)},
	}); err != nil {
		s.t.Fatal(err)
	}
}

// writeLater writes a file with a modification time after every editor change
// so far, as an agent's write that follows them does.
func writeLater(t *testing.T, path, text string) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(path, []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
}

const agentWorkerSource = "# agent\n# notes\n" + workerSource + "\ndefmodule MyApp.AgentAdded do\n  def added(x), do: x\nend\n"

// Regression: an editor buffer that the user never changed, but that is older
// than the disk (the editor has not reloaded the file after the agent wrote
// it), hid the agent's edits and was reported as unsaved work.
func TestTools_CleanStaleBufferReadsDisk(t *testing.T) {
	e := setupProject(t)
	ed := attachEditor(t, e)
	ed.open("lib/my_app/worker.ex", workerSource)
	path := filepath.Join(e.root, "lib/my_app/worker.ex")
	writeLater(t, path, agentWorkerSource)

	out := e.callTool("dexter_file_outline", map[string]any{"file": "lib/my_app/worker.ex"})
	wantContains(t, out, "defmodule MyApp.AgentAdded")
	wantNotContains(t, out, "unsaved")
}

// A buffer with unsaved changes, made after the disk last changed, is what the
// user sees, so it wins.
func TestTools_DirtyBufferWins(t *testing.T) {
	e := setupProject(t)
	ed := attachEditor(t, e)
	ed.open("lib/my_app/worker.ex", workerSource)
	ed.change("lib/my_app/worker.ex", workerSource+"\ndefmodule MyApp.Typed do\nend\n")
	out := e.callTool("dexter_file_outline", map[string]any{"file": "lib/my_app/worker.ex"})
	wantContains(t, out, "defmodule MyApp.Typed", "read from unsaved editor buffers")
}

// When both the buffer and the disk changed, and the disk changed later, the
// disk wins (the index follows it), and the answer warns about the editor's
// older unsaved changes.
func TestTools_DirtyBufferOlderThanDiskReadsDiskWithNote(t *testing.T) {
	e := setupProject(t)
	ed := attachEditor(t, e)
	ed.open("lib/my_app/worker.ex", workerSource)
	ed.change("lib/my_app/worker.ex", workerSource+"\ndefmodule MyApp.Typed do\nend\n")
	writeLater(t, filepath.Join(e.root, "lib/my_app/worker.ex"), agentWorkerSource)

	out := e.callTool("dexter_file_outline", map[string]any{"file": "lib/my_app/worker.ex"})
	wantContains(t, out, "defmodule MyApp.AgentAdded", "an editor also has unsaved changes", "may conflict")
	wantNotContains(t, out, "MyApp.Typed", "read from unsaved editor buffers")
}

// A save ends the buffer's unsaved changes: a later write by the agent is
// read from disk with no warning.
func TestTools_SaveClearsUnsavedState(t *testing.T) {
	e := setupProject(t)
	ed := attachEditor(t, e)
	path := filepath.Join(e.root, "lib/my_app/worker.ex")
	ed.open("lib/my_app/worker.ex", workerSource)
	edited := workerSource + "\ndefmodule MyApp.Typed do\nend\n"
	ed.change("lib/my_app/worker.ex", edited)
	if err := os.WriteFile(path, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}
	ed.save("lib/my_app/worker.ex")
	writeLater(t, path, agentWorkerSource)

	out := e.callTool("dexter_file_outline", map[string]any{"file": "lib/my_app/worker.ex"})
	wantContains(t, out, "defmodule MyApp.AgentAdded")
	wantNotContains(t, out, "unsaved")
}

// Regression: lines were mapped through the lines that both texts share at
// the start and at the end, so an edit at the top and one at the bottom
// marked every line between them as changed, and a buffer with CRLF line
// endings marked every line.
func TestLineMap(t *testing.T) {
	disk := "a\nb\nc\nd\ne\nf"
	m := newLineMap(disk, "TOP\na\nb\nc\nd\ne\nBOTTOM") // f changed, line added at top
	for line, want := range map[int]int{1: 2, 2: 3, 3: 4, 4: 5, 5: 6} {
		if got, ok := m.locate(line); !ok || got != want {
			t.Errorf("locate(%d) = %d, %v; want %d", line, got, ok, want)
		}
	}
	if _, ok := m.locate(6); ok {
		t.Error("the changed last line is mapped")
	}

	crlf := newLineMap(disk, strings.ReplaceAll(disk, "\n", "\r\n"))
	for line := 1; line <= 6; line++ {
		if got, ok := crlf.locate(line); !ok || got != line {
			t.Errorf("CRLF: locate(%d) = %d, %v; want %d", line, got, ok, line)
		}
	}

	// A deleted line in the middle, and a changed one.
	mid := newLineMap(disk, "a\nc\nD\ne\nf")
	for line, want := range map[int]int{1: 1, 3: 2, 5: 4, 6: 5} {
		if got, ok := mid.locate(line); !ok || got != want {
			t.Errorf("mid: locate(%d) = %d, %v; want %d", line, got, ok, want)
		}
	}
	for _, line := range []int{2, 4} {
		if _, ok := mid.locate(line); ok {
			t.Errorf("mid: changed line %d is mapped", line)
		}
	}
}

// The diff is exact on random edits: every mapped pair is equal, mapped lines
// keep their order, and no more lines are left unmapped than a shortest edit
// script deletes.
func TestLineMapRandomEdits(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for round := 0; round < 200; round++ {
		var a []string
		for i := 0; i < 1+rng.IntN(60); i++ {
			a = append(a, fmt.Sprintf("l%d", rng.IntN(8)))
		}
		var b []string
		for _, l := range a {
			switch rng.IntN(6) {
			case 0: // delete
			case 1:
				b = append(b, "new", l)
			case 2:
				b = append(b, "changed")
			default:
				b = append(b, l)
			}
		}
		m := newLineMap(strings.Join(a, "\n"), strings.Join(b, "\n"))
		last := 0
		mapped := 0
		for i := 1; i <= len(a); i++ {
			j, ok := m.locate(i)
			if !ok {
				continue
			}
			mapped++
			if j <= last || a[i-1] != b[j-1] {
				t.Fatalf("round %d: line %d mapped to %d (last %d): %q vs %q", round, i, j, last, a[i-1], b[j-1])
			}
			last = j
		}
		if want := lcsLen(a, b); mapped != want {
			t.Fatalf("round %d: %d lines mapped, want the LCS length %d", round, mapped, want)
		}
	}
}

func lcsLen(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else {
				dp[i][j] = max(dp[i-1][j], dp[i][j-1])
			}
		}
	}
	return dp[len(a)][len(b)]
}

// BenchmarkLineMap10k measures a 10,000-line file with edits at the top, in
// the middle, and at the bottom.
func BenchmarkLineMap10k(b *testing.B) {
	var lines []string
	for i := 0; i < 10000; i++ {
		lines = append(lines, fmt.Sprintf("  def f%d(x), do: x + %d", i, i))
	}
	disk := strings.Join(lines, "\n")
	edited := append([]string{"# top"}, lines...)
	edited[5000] = "  # changed"
	edited = append(edited[:7000], append([]string{"  def added(x), do: x"}, edited[7000:]...)...)
	edited[len(edited)-1] = "# bottom"
	text := strings.Join(edited, "\n")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		newLineMap(disk, text)
	}
}

// BenchmarkLineMap10kManyEdits is a 10,000-line file with edits near the
// bound (about 900 inserted and deleted lines).
func BenchmarkLineMap10kManyEdits(b *testing.B) {
	var lines []string
	for i := 0; i < 10000; i++ {
		lines = append(lines, fmt.Sprintf("  def f%d(x), do: x + %d", i, i))
	}
	disk := strings.Join(lines, "\n")
	edited := append([]string(nil), lines...)
	for i := 0; i < len(edited); i += 22 {
		edited[i] = "  # changed"
	}
	text := strings.Join(edited, "\n")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		newLineMap(disk, text)
	}
}
