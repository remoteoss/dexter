package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// fakeClient records ApplyEdit requests; other client methods are never
// called by the rename path.
type fakeClient struct {
	protocol.Client
	applied *protocol.WorkspaceEdit
	reject  bool
}

func (f *fakeClient) ApplyEdit(_ context.Context, params *protocol.ApplyWorkspaceEditParams) (bool, error) {
	f.applied = &params.Edit
	return !f.reject, nil
}

// With a live client (attached mode), open-buffer edits go to the editor via
// workspace/applyEdit; dexter must not write those files behind its back.
func TestRenameFunction_ForwardsOpenBufferEditsToClient(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	fc := &fakeClient{}
	server.client = fc

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
	if len(fc.applied.Changes) != 1 {
		t.Errorf("ApplyEdit carried %d files, want 1", len(fc.applied.Changes))
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
	server.client = &fakeClient{reject: true}

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

	if _, err := server.RenameFunction("MyApp.Accounts", "fetch_user", "get_user"); err == nil {
		t.Fatal("rename reported success despite the editor rejecting the edit")
	}
}
