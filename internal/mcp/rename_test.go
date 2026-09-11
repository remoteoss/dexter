package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRenameTool_Function(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "function": "fetch_user", "new_name": "get_user",
	})
	wantContains(t, out,
		"Renamed MyApp.Accounts.fetch_user to get_user",
		"lib/my_app/accounts.ex",
		"lib/my_app/worker.ex",
		"git diff",
	)

	accounts := readFile(t, e.root, "lib/my_app/accounts.ex")
	wantContains(t, accounts, "def get_user(id)", "@spec get_user(integer())")
	wantNotContains(t, accounts, "fetch_user")

	worker := readFile(t, e.root, "lib/my_app/worker.ex")
	wantContains(t, worker, "MyApp.Accounts.get_user(1)", "MyApp.Accounts.get_user(2)")

	// The rename reindexes what it wrote: lookups resolve the new name only.
	wantContains(t, e.callTool("dexter_definition", map[string]any{"module": "MyApp.Accounts", "function": "get_user"}), "get_user/1 (def)")
	wantContains(t, e.callTool("dexter_definition", map[string]any{"module": "MyApp.Accounts", "function": "fetch_user"}), "not in the index")
}

func TestRenameTool_Module_MovesFiles(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "new_name": "MyApp.Users",
	})
	wantContains(t, out,
		"Renamed MyApp.Accounts to MyApp.Users",
		"Files moved to follow the naming convention:",
		"lib/my_app/accounts.ex → lib/my_app/users.ex",
		"lib/my_app/accounts/creator.ex → lib/my_app/users/creator.ex",
	)

	if _, err := os.Stat(filepath.Join(e.root, "lib/my_app/accounts.ex")); !os.IsNotExist(err) {
		t.Error("old module file still exists after rename")
	}
	wantContains(t, readFile(t, e.root, "lib/my_app/users.ex"), "defmodule MyApp.Users do")
	wantContains(t, readFile(t, e.root, "lib/my_app/users/creator.ex"), "defmodule MyApp.Users.Creator do")
	wantContains(t, readFile(t, e.root, "lib/my_app/worker.ex"), "MyApp.Users.fetch_user(1)")

	wantContains(t, e.callTool("dexter_definition", map[string]any{"module": "MyApp.Users"}), "defmodule MyApp.Users")
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

	// Failed renames must not touch disk.
	if s := readFile(t, e.root, "lib/my_app/accounts.ex"); !strings.Contains(s, "def fetch_user(id)") {
		t.Error("failed rename modified files")
	}
}
