package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func TestApplyTextEdits(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		edits []protocol.TextEdit
		want  string
	}{
		{
			name: "single token on one line",
			text: "def fetch_user(id) do\n  fetch_user(id)\nend\n",
			edits: []protocol.TextEdit{
				{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 4}, End: protocol.Position{Line: 0, Character: 14}}, NewText: "get_user"},
			},
			want: "def get_user(id) do\n  fetch_user(id)\nend\n",
		},
		{
			name: "two tokens on the same line applied right to left",
			text: "fetch_user(fetch_user(1))\n",
			edits: []protocol.TextEdit{
				{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 10}}, NewText: "get_user"},
				{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 11}, End: protocol.Position{Line: 0, Character: 21}}, NewText: "get_user"},
			},
			want: "get_user(get_user(1))\n",
		},
		{
			name: "out-of-range column is skipped, not a panic",
			text: "short\n",
			edits: []protocol.TextEdit{
				{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 40}, End: protocol.Position{Line: 0, Character: 50}}, NewText: "x"},
			},
			want: "short\n",
		},
		{
			name: "multi-line span replacement",
			text: "a\nold one\nold two\nb\n",
			edits: []protocol.TextEdit{
				{Range: protocol.Range{Start: protocol.Position{Line: 1, Character: 0}, End: protocol.Position{Line: 2, Character: 7}}, NewText: "new one\nnew two\nnew three"},
			},
			want: "a\nnew one\nnew two\nnew three\nb\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := applyTextEdits(tt.text, tt.edits); got != tt.want {
				t.Errorf("got:\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestReadyWaitsForInitialize(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	select {
	case <-server.Ready():
		t.Fatal("server reported ready before LSP initialization")
	default:
	}
	if _, err := server.Initialize(context.Background(), &protocol.InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.Ready():
	default:
		t.Fatal("server did not report ready after LSP initialization")
	}
}

// Without a live client, a rename requested through the exported API must
// land on disk even for files marked open (the defensive fallback path).
func TestRenameFunction_WritesOpenBuffers(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def fetch_user(id), do: id
end
`)
	openSrc := `defmodule MyApp.Caller do
  def go(id), do: MyApp.Accounts.fetch_user(id)
end
`
	indexFile(t, server.store, server.projectRoot, "lib/caller.ex", openSrc)
	openPath := filepath.Join(server.projectRoot, "lib/caller.ex")
	server.docs.Set(string(uri.File(openPath)), openSrc) // simulate didOpen

	summary, err := server.RenameFunction("MyApp.Accounts", "fetch_user", "get_user")
	if err != nil {
		t.Fatal(err)
	}
	server.backgroundWork.Wait()

	if len(summary.FilesChanged) != 2 {
		t.Errorf("FilesChanged = %v, want both files", summary.FilesChanged)
	}
	data, err := os.ReadFile(openPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "MyApp.Accounts.get_user(id)") {
		t.Errorf("open buffer's file not written to disk:\n%s", data)
	}
	results, err := server.store.LookupFunction("MyApp.Accounts", "get_user")
	if err != nil || len(results) == 0 {
		t.Errorf("index not updated after rename: %v, %v", results, err)
	}
}

// fakeConn records workspace/applyEdit requests. The edit is captured as the
// JSON that actually goes over the wire, because that is the only place the
// resource operations survive — protocol.Client.ApplyEdit's typed params
// would drop them.
type fakeConn struct {
	jsonrpc2.Conn
	applied *WorkspaceEdit
	raw     map[string]interface{}
	reject  bool
}

func (f *fakeConn) Call(_ context.Context, method string, params, result interface{}) (jsonrpc2.ID, error) {
	if method != protocol.MethodWorkspaceApplyEdit {
		return jsonrpc2.ID{}, nil
	}
	p, ok := params.(*applyWorkspaceEditParams)
	if !ok {
		return jsonrpc2.ID{}, fmt.Errorf("applyEdit params were %T", params)
	}
	f.applied = p.Edit
	data, err := json.Marshal(p)
	if err != nil {
		return jsonrpc2.ID{}, err
	}
	if err := json.Unmarshal(data, &f.raw); err != nil {
		return jsonrpc2.ID{}, err
	}
	if res, ok := result.(*protocol.ApplyWorkspaceEditResponse); ok {
		res.Applied = !f.reject
	}
	return jsonrpc2.ID{}, nil
}

// With a live client (attached mode), open-buffer edits go to the editor via
// workspace/applyEdit; dexter must not write those files behind its back.
func TestRenameFunction_ForwardsOpenBufferEditsToClient(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	fc := &fakeConn{}
	server.conn = fc

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def fetch_user(id), do: id
end
`)
	openSrc := `defmodule MyApp.Caller do
  def go(id), do: MyApp.Accounts.fetch_user(id)
end
`
	indexFile(t, server.store, server.projectRoot, "lib/caller.ex", openSrc)
	openPath := filepath.Join(server.projectRoot, "lib/caller.ex")
	server.docs.Set(string(uri.File(openPath)), openSrc)

	if _, err := server.RenameFunction("MyApp.Accounts", "fetch_user", "get_user"); err != nil {
		t.Fatal(err)
	}

	if fc.applied == nil {
		t.Fatal("no workspace/applyEdit request reached the client")
	}
	if len(fc.applied.Changes) != 2 {
		t.Errorf("ApplyEdit carried %d files, want both open and closed files", len(fc.applied.Changes))
	}
	data, err := os.ReadFile(openPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "get_user") {
		t.Error("open buffer's file was written to disk despite a live client")
	}
}

// An editor may refuse a workspace edit (applied: false); the rename must
// report failure, not success, when open-buffer edits were not applied.
func TestRenameFunction_ReportsRejectedApplyEdit(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	server.conn = &fakeConn{reject: true}

	definitionSrc := `defmodule MyApp.Accounts do
  def fetch_user(id), do: id
end
	`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", definitionSrc)
	definitionPath := filepath.Join(server.projectRoot, "lib/accounts.ex")
	openSrc := `defmodule MyApp.Caller do
  def go(id), do: MyApp.Accounts.fetch_user(id)
end
`
	indexFile(t, server.store, server.projectRoot, "lib/caller.ex", openSrc)
	openPath := filepath.Join(server.projectRoot, "lib/caller.ex")
	server.docs.Set(string(uri.File(openPath)), openSrc)

	if _, err := server.RenameFunction("MyApp.Accounts", "fetch_user", "get_user"); err == nil {
		t.Fatal("rename reported success despite the editor rejecting the edit")
	}
	data, err := os.ReadFile(definitionPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != definitionSrc {
		t.Errorf("rejected rename changed a closed file:\n%s", data)
	}
}

// An agent renaming a module whose file the editor has open: the move belongs
// to the editor, so it has to travel in the applyEdit request as a rename
// resource operation. protocol.ApplyWorkspaceEditParams cannot carry one, so a
// deliverEdits that reaches for the typed client would silently move nothing.
func TestRenameModule_ForwardsFileMoveToClient(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	fc := &fakeConn{}
	server.conn = fc
	server.renameFileOpsSupported = true

	src := `defmodule MyApp.Accounts do
  def list_users, do: []
end
`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", src)
	oldPath := filepath.Join(server.projectRoot, "lib/accounts.ex")
	newPath := filepath.Join(server.projectRoot, "lib/auth.ex")
	server.docs.Set(string(uri.File(oldPath)), src)

	summary, err := server.RenameModule("MyApp.Accounts", "MyApp.Auth")
	if err != nil {
		t.Fatal(err)
	}

	if fc.applied == nil {
		t.Fatal("no workspace/applyEdit request reached the editor")
	}
	var renamed *RenameFile
	for _, change := range fc.applied.DocumentChanges {
		if rf, ok := change.(RenameFile); ok {
			renamed = &rf
		}
	}
	if renamed == nil {
		t.Fatalf("applyEdit carried no rename operation: %+v", fc.applied.DocumentChanges)
	}
	if want := protocol.DocumentURI(uri.File(newPath)); renamed.NewURI != want {
		t.Errorf("rename target = %s, want %s", renamed.NewURI, want)
	}

	// The operation has to survive marshaling — that is what the editor reads.
	edit, _ := fc.raw["edit"].(map[string]interface{})
	changes, _ := edit["documentChanges"].([]interface{})
	foundKind := false
	for _, c := range changes {
		if m, ok := c.(map[string]interface{}); ok && m["kind"] == "rename" {
			foundKind = true
		}
	}
	if !foundKind {
		t.Errorf("no rename operation in the marshaled request: %v", fc.raw)
	}

	// The editor performs the move, so dexter must have left both paths alone.
	if _, err := os.Stat(oldPath); err != nil {
		t.Error("dexter removed the file the editor has open")
	}
	if _, err := os.Stat(newPath); err == nil {
		t.Error("dexter created the destination; the editor performs the move")
	}
	if summary.FilesMoved[oldPath] != newPath {
		t.Errorf("summary reports moves %v, want %s → %s", summary.FilesMoved, oldPath, newPath)
	}
}

func TestRenameModule_LeavesConventionalFileInPlaceWithoutClientMoveSupport(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	fc := &fakeConn{}
	server.conn = fc
	server.renameFileOpsSupported = false

	src := `defmodule MyApp.Accounts do
  def list_users, do: []
end
`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", src)
	oldPath := filepath.Join(server.projectRoot, "lib/accounts.ex")
	newPath := filepath.Join(server.projectRoot, "lib/auth.ex")

	summary, err := server.RenameModule("MyApp.Accounts", "MyApp.Auth")
	if err != nil {
		t.Fatal(err)
	}

	for _, change := range fc.applied.DocumentChanges {
		if _, ok := change.(RenameFile); ok {
			t.Fatal("applyEdit included a rename operation the client does not support")
		}
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Errorf("source file should remain at its old path: %v", err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("destination should not be created, stat error = %v", err)
	}
	if len(summary.FilesMoved) != 0 {
		t.Errorf("summary reported unsupported moves: %v", summary.FilesMoved)
	}
}

func TestRenameModule_RejectedApplyEditLeavesDiskUntouched(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	server.conn = &fakeConn{reject: true}

	src := `defmodule MyApp.Accounts do
  def list_users, do: []
end
`
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", src)
	oldPath := filepath.Join(server.projectRoot, "lib/accounts.ex")
	newPath := filepath.Join(server.projectRoot, "lib/auth.ex")

	if _, err := server.RenameModule("MyApp.Accounts", "MyApp.Auth"); err == nil {
		t.Fatal("rename reported success despite the editor rejecting the edit")
	}
	data, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("source file was moved or removed: %v", err)
	}
	if string(data) != src {
		t.Errorf("rejected rename changed the source file:\n%s", data)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Errorf("rejected rename created destination %s", newPath)
	}
}

// Headless, no editor: nothing is open, so dexter carries out the whole edit
// itself and the summary still reports the move.
func TestRenameModule_HeadlessMovesFilesItself(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def list_users, do: []
end
`)
	oldPath := filepath.Join(server.projectRoot, "lib/accounts.ex")
	newPath := filepath.Join(server.projectRoot, "lib/auth.ex")

	summary, err := server.RenameModule("MyApp.Accounts", "MyApp.Auth")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(oldPath); err == nil {
		t.Error("expected accounts.ex to be gone")
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("expected auth.ex on disk: %v", err)
	}
	if !strings.Contains(string(data), "defmodule MyApp.Auth") {
		t.Errorf("expected 'defmodule MyApp.Auth', got:\n%s", data)
	}
	if summary.FilesMoved[oldPath] != newPath {
		t.Errorf("summary reports moves %v, want %s → %s", summary.FilesMoved, oldPath, newPath)
	}
}

// StopGitHeadWatch must end the watch goroutine so a HEAD change after it can
// no longer trigger a reindex against a store the caller is about to close.
func TestStopGitHeadWatch(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	headPath := filepath.Join(server.projectRoot, ".git", "HEAD")
	if err := os.MkdirAll(filepath.Dir(headPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(headPath, []byte("ref: refs/heads/main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	server.WatchGitHead()
	done := make(chan struct{})
	go func() {
		server.StopGitHeadWatch()
		server.StopGitHeadWatch() // idempotent
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StopGitHeadWatch did not return")
	}
}
