package mcp

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// hashTree fingerprints every regular file under root except dexter's own
// index database (the store legitimately writes there).
func hashTree(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	hashes := make(map[string][32]byte)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".dexter" {
				return fs.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hashes[path] = sha256.Sum256(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hashes
}

func TestRenameTool_FunctionDiff(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "function": "fetch_user", "new_name": "get_user",
	})
	wantContains(t, out,
		"Rename function MyApp.Accounts.fetch_user to get_user",
		"Nothing has been written",
		"--- a/lib/my_app/accounts.ex",
		"+++ b/lib/my_app/accounts.ex",
		"-  def fetch_user(id) do",
		"+  def get_user(id) do",
		"-  @spec fetch_user(integer())",
		"+  @spec get_user(integer())",
		"--- a/lib/my_app/worker.ex",
		"-    MyApp.Accounts.fetch_user(1)",
		"+    MyApp.Accounts.get_user(1)",
		"dexter_reindex",
	)
}

func TestRenameTool_ModuleDiff_WithFileRename(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "new_name": "MyApp.Users",
	})
	wantContains(t, out,
		"Rename module MyApp.Accounts to MyApp.Users",
		"RENAME lib/my_app/accounts.ex → lib/my_app/users.ex",
		"RENAME lib/my_app/accounts/creator.ex → lib/my_app/users/creator.ex",
		"-defmodule MyApp.Accounts do",
		"+defmodule MyApp.Users do",
		"-defmodule MyApp.Accounts.Creator do",
		"+defmodule MyApp.Users.Creator do",
		// Diffs for moved files are addressed at the new path.
		"--- a/lib/my_app/users.ex",
	)
}

func TestRenameTool_DoesNotModifyDisk(t *testing.T) {
	e := setupProject(t)
	before := hashTree(t, e.root)

	e.callTool("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "function": "fetch_user", "new_name": "get_user",
	})
	e.callTool("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "new_name": "MyApp.Users",
	})

	after := hashTree(t, e.root)
	if len(before) != len(after) {
		t.Fatalf("file count changed: %d -> %d", len(before), len(after))
	}
	for path, h := range before {
		if after[path] != h {
			t.Errorf("file modified by rename tool: %s", path)
		}
	}
}

func TestRenameTool_Errors(t *testing.T) {
	e := setupProject(t)

	errText := e.callToolExpectError("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "function": "fetch_user", "new_name": "NotValid",
	})
	wantContains(t, errText, "invalid function name")

	errText = e.callToolExpectError("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "function": "fetch_user", "new_name": "list_users",
	})
	wantContains(t, errText, "already exists")

	errText = e.callToolExpectError("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Missing", "new_name": "MyApp.New",
	})
	wantContains(t, errText, "not found")
}
