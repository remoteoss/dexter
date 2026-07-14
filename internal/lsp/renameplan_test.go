package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests are the regression guard for the rename plan/apply split: a plan
// must predict exactly what the LSP apply path writes to disk. If the two ever
// diverge, the MCP rename tool would hand agents diffs that don't match what
// an editor rename produces.

func TestFunctionRenamePlan_MatchesApply(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	fixtures := map[string]string{
		"lib/accounts.ex": `defmodule MyApp.Accounts do
  @doc """
  Fetches a user.
  """
  @spec fetch_user(integer()) :: map()
  def fetch_user(id) do
    validate(id)
    do_fetch(id)
  end

  def other_fun do
    fetch_user(1)
  end

  defp validate(id), do: id
  defp do_fetch(id), do: %{id: id}
end
`,
		"lib/caller.ex": `defmodule MyApp.Caller do
  def call do
    MyApp.Accounts.fetch_user(42)
  end
end
`,
		"lib/importer.ex": `defmodule MyApp.Importer do
  import MyApp.Accounts, only: [fetch_user: 1]

  def go do
    fetch_user(7)
  end
end
`,
		"lib/facade.ex": `defmodule MyApp.Facade do
  defdelegate fetch_user(id), to: MyApp.Accounts
end
`,
		"lib/unrelated.ex": `defmodule MyApp.Unrelated do
  def nothing_here, do: :ok
end
`,
	}
	for rel, content := range fixtures {
		indexFile(t, server.store, server.projectRoot, rel, content)
	}

	plan, err := server.FunctionRenamePlan("MyApp.Accounts", "fetch_user", "get_user")
	if err != nil {
		t.Fatalf("FunctionRenamePlan: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("plan is empty")
	}

	// The plan must not have touched disk.
	for rel, content := range fixtures {
		data, err := os.ReadFile(filepath.Join(server.projectRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != content {
			t.Fatalf("FunctionRenamePlan modified %s on disk", rel)
		}
	}

	// Now run the LSP apply path (all files closed, so everything is written
	// directly to disk).
	if _, err := server.renameFunctionEdits("MyApp.Accounts", "fetch_user", "get_user"); err != nil {
		t.Fatalf("renameFunctionEdits: %v", err)
	}
	server.backgroundWork.Wait()

	// Every planned change must match the bytes the apply path wrote.
	planned := make(map[string]FileChange, len(plan.Changes))
	for _, fc := range plan.Changes {
		if fc.NewPath != "" {
			t.Errorf("function rename plan should not move files, got %s -> %s", fc.Path, fc.NewPath)
		}
		planned[fc.Path] = fc
		data, err := os.ReadFile(fc.Path)
		if err != nil {
			t.Fatalf("reading %s after apply: %v", fc.Path, err)
		}
		if string(data) != fc.NewText {
			t.Errorf("plan NewText for %s does not match applied content.\nplan:\n%s\napplied:\n%s", fc.Path, fc.NewText, data)
		}
	}

	// And every file the apply path changed must be in the plan.
	for rel, original := range fixtures {
		path := filepath.Join(server.projectRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != original {
			if _, ok := planned[path]; !ok {
				t.Errorf("apply changed %s but the plan did not include it", rel)
			}
		}
	}

	// Sanity: the interesting sites were actually renamed.
	mustContain := map[string][]string{
		"lib/accounts.ex": {"def get_user(id)", "@spec get_user(integer())", "get_user(1)"},
		"lib/caller.ex":   {"MyApp.Accounts.get_user(42)"},
		"lib/importer.ex": {"only: [get_user: 1]", "get_user(7)"},
		"lib/facade.ex":   {"as: :get_user"}, // delegate facade keeps its public name via as:
	}
	for rel, wants := range mustContain {
		data, _ := os.ReadFile(filepath.Join(server.projectRoot, rel))
		for _, w := range wants {
			if !strings.Contains(string(data), w) {
				t.Errorf("%s missing %q after rename:\n%s", rel, w, data)
			}
		}
	}
}

func TestModuleRenamePlan_MatchesApply(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	fixtures := map[string]string{
		"lib/my_app/companies.ex": `defmodule MyApp.Companies do
  def list, do: []
end
`,
		"lib/my_app/companies/finder.ex": `defmodule MyApp.Companies.Finder do
  def find(id), do: id
end
`,
		"lib/my_app/consumer.ex": `defmodule MyApp.Consumer do
  alias MyApp.Companies
  alias MyApp.Companies.Finder

  def go do
    Companies.list()
    Finder.find(1)
  end
end
`,
	}
	for rel, content := range fixtures {
		indexFile(t, server.store, server.projectRoot, rel, content)
	}

	plan, err := server.ModuleRenamePlan("MyApp.Companies", "MyApp.Clients")
	if err != nil {
		t.Fatalf("ModuleRenamePlan: %v", err)
	}

	// The plan must not have touched disk.
	for rel, content := range fixtures {
		data, err := os.ReadFile(filepath.Join(server.projectRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != content {
			t.Fatalf("ModuleRenamePlan modified %s on disk", rel)
		}
	}

	// The conventional files must be planned as moves.
	moves := make(map[string]string)
	for _, fc := range plan.Changes {
		if fc.NewPath != "" {
			moves[fc.Path] = fc.NewPath
		}
	}
	wantMoves := map[string]string{
		filepath.Join(server.projectRoot, "lib/my_app/companies.ex"):        filepath.Join(server.projectRoot, "lib/my_app/clients.ex"),
		filepath.Join(server.projectRoot, "lib/my_app/companies/finder.ex"): filepath.Join(server.projectRoot, "lib/my_app/clients/finder.ex"),
	}
	for from, to := range wantMoves {
		if moves[from] != to {
			t.Errorf("plan move for %s = %q, want %q", from, moves[from], to)
		}
	}

	// Apply via the LSP path.
	if _, err := server.renameModuleEdits(context.Background(), "MyApp.Companies", "MyApp.Clients", ""); err != nil {
		t.Fatalf("renameModuleEdits: %v", err)
	}
	server.backgroundWork.Wait()

	for _, fc := range plan.Changes {
		finalPath := fc.Path
		if fc.NewPath != "" {
			finalPath = fc.NewPath
			if _, err := os.Stat(fc.Path); !os.IsNotExist(err) {
				t.Errorf("old path %s still exists after apply", fc.Path)
			}
		}
		data, err := os.ReadFile(finalPath)
		if err != nil {
			t.Fatalf("reading %s after apply: %v", finalPath, err)
		}
		if string(data) != fc.NewText {
			t.Errorf("plan NewText for %s does not match applied content.\nplan:\n%s\napplied:\n%s", finalPath, fc.NewText, data)
		}
	}

	// Consumer aliases must have been rewritten.
	data, _ := os.ReadFile(filepath.Join(server.projectRoot, "lib/my_app/consumer.ex"))
	for _, w := range []string{"alias MyApp.Clients", "alias MyApp.Clients.Finder", "Clients.list()"} {
		if !strings.Contains(string(data), w) {
			t.Errorf("consumer missing %q after rename:\n%s", w, data)
		}
	}
}

func TestFunctionRenamePlan_Validation(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def fetch_user(id), do: id
  def get_user(id), do: id
end
`)

	if _, err := server.FunctionRenamePlan("MyApp.Accounts", "fetch_user", "NotAFunction"); err == nil {
		t.Error("invalid new name accepted")
	}
	if _, err := server.FunctionRenamePlan("MyApp.Accounts", "fetch_user", "get_user"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("collision not detected: %v", err)
	}
	if _, err := server.FunctionRenamePlan("MyApp.Accounts", "no_such_fun", "whatever"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing function not detected: %v", err)
	}
}

func TestModuleRenamePlan_Validation(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/a.ex", "defmodule MyApp.A do\nend\n")
	indexFile(t, server.store, server.projectRoot, "lib/b.ex", "defmodule MyApp.B do\nend\n")

	if _, err := server.ModuleRenamePlan("MyApp.A", "not_a_module"); err == nil {
		t.Error("invalid module name accepted")
	}
	if _, err := server.ModuleRenamePlan("MyApp.A", "MyApp.B"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("collision not detected: %v", err)
	}
	if _, err := server.ModuleRenamePlan("MyApp.Missing", "MyApp.New"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing module not detected: %v", err)
	}
}
