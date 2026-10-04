package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Regression: the rename once applied editor positions (UTF-16 columns) as
// byte offsets on disk, so a non-ASCII character left of the name moved the
// edit and corrupted the line.
func TestRenameTool_NonASCIIBeforeName(t *testing.T) {
	e := setupProject(t)
	e.indexFile("lib/my_app/greeter.ex", `defmodule MyApp.Greeter do
  def greet(id), do: {"héllo wörld ✓", MyApp.Accounts.fetch_user(id)}
end
`)
	e.callTool("dexter_rename_symbol", map[string]any{
		"module": "MyApp.Accounts", "function": "fetch_user", "new_name": "get_user",
	})
	wantContains(t, readFile(t, e.root, "lib/my_app/greeter.ex"),
		`  def greet(id), do: {"héllo wörld ✓", MyApp.Accounts.get_user(id)}`)
}

// Regression: two renames at the same time each read the affected files and
// wrote them back, so the later write dropped the other rename's edits while
// both reported success.
func TestRenameTool_ConcurrentRenamesKeepBothEdits(t *testing.T) {
	e := setupProject(t)
	for i := 0; i < 40; i++ {
		e.indexFile(fmt.Sprintf("lib/my_app/caller_%d.ex", i), fmt.Sprintf(`defmodule MyApp.Caller%d do
  def run do
    MyApp.Accounts.fetch_user(1)
    MyApp.Accounts.list_users([])
  end
end
`, i))
	}
	h := NewHandler(e.rt, e.lsp)
	pairs := [][2][2]string{
		{{"fetch_user", "get_user"}, {"list_users", "all_users"}},
		{{"get_user", "fetch_user"}, {"all_users", "list_users"}},
	}
	for round := 0; round < 6; round++ {
		renames := pairs[round%2]
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, r := range renames {
			wg.Add(1)
			go func() {
				defer wg.Done()
				args, _ := json.Marshal(map[string]any{"module": "MyApp.Accounts", "function": r[0], "new_name": r[1]})
				_, errs[i] = h.Call(context.Background(), renameToolName, args, 0)
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				t.Fatalf("round %d: rename failed: %v", round, err)
			}
		}
		for i := 0; i < 40; i++ {
			text := readFile(t, e.root, fmt.Sprintf("lib/my_app/caller_%d.ex", i))
			for _, r := range renames {
				if !strings.Contains(text, "MyApp.Accounts."+r[1]+"(") || strings.Contains(text, "MyApp.Accounts."+r[0]+"(") {
					t.Fatalf("round %d: caller_%d.ex lost the rename %s → %s:\n%s", round, i, r[0], r[1], text)
				}
			}
		}
	}
}
