package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/store"
)

// buildDexter builds the binary once for all integration tests
func buildDexter(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "dexter")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "./cmd/")
	cmd.Dir = filepath.Dir(mustAbs(t, "go.mod"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return binary
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// scaffoldProject creates a fake Elixir project with various module patterns
func scaffoldProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	files := map[string]string{
		// mix.exs so findProjectRoot works
		"mix.exs": `defmodule MyApp.MixProject do
  use Mix.Project
end
`,
		// Simple module with public and private functions
		"lib/my_app/repo.ex": `defmodule MyApp.Repo do
  def get(schema, id) do
    :ok
  end

  def get!(schema, id) do
    :ok
  end

  defp build_query(schema) do
    :ok
  end
end
`,
		// Module with multiple function heads (pattern matching)
		"lib/my_app/handlers/webhooks.ex": `defmodule MyApp.Handlers.Webhooks do
  def process_event("completed", payload) do
    :ok
  end

  def process_event("declined", payload) do
    :declined
  end

  def process_event(_, _) do
    :unknown
  end
end
`,
		// Module that will be aliased with as:
		"lib/my_app/serializer/date.ex": `defmodule MyApp.Serializer.Date do
  def format(date) do
    :ok
  end
end
`,
		// Module for multi-alias testing
		"lib/my_app/companies/value/company.ex": `defmodule MyApp.Companies.Value.Company do
  def build(attrs) do
    :ok
  end
end
`,
		"lib/my_app/countries/value/country.ex": `defmodule MyApp.Countries.Value.Country do
  def build(attrs) do
    :ok
  end
end
`,
		// Module with import
		"lib/my_app/helpers/formatting.ex": `defmodule MyApp.Helpers.Formatting do
  def format_currency(amount) do
    :ok
  end

  def format_date(date) do
    :ok
  end
end
`,
		// Caller that uses alias
		"lib/my_app/workers/webhook_worker.ex": `defmodule MyApp.Workers.WebhookWorker do
  alias MyApp.Handlers.Webhooks

  def run(event_type, payload) do
    Webhooks.process_event(event_type, payload)
  end
end
`,
		// Caller that uses alias with as:
		"lib/my_app/values/timesheet.ex": `defmodule MyApp.Values.Timesheet do
  alias MyApp.Serializer.Date, as: DateSerializer
  alias MyApp.Companies.Value.Company, as: CompanyValue

  def build(date, company) do
    DateSerializer.format(date)
    CompanyValue.build(company)
  end
end
`,
		// Caller that uses multi-alias
		"lib/my_app/values/report.ex": `defmodule MyApp.Values.Report do
  alias MyApp.Companies.Value.{Company, Country}

  def build(company, country) do
    Company.build(company)
  end
end
`,
		// Caller that uses import
		"lib/my_app/views/money_view.ex": `defmodule MyApp.Views.MoneyView do
  import MyApp.Helpers.Formatting

  def render(amount, date) do
    format_currency(amount)
    format_date(date)
  end
end
`,
		// Nested modules
		"lib/my_app/outer.ex": `defmodule MyApp.Outer do
  def outer_func do
    :ok
  end

  defmodule MyApp.Outer.Inner do
    def inner_func do
      :ok
    end
  end
end
`,
		// Module with macros
		"lib/my_app/macros.ex": `defmodule MyApp.Macros do
  defmacro my_macro(arg) do
    quote do: unquote(arg)
  end

  defmacrop private_macro do
    :ok
  end
end
`,
		// Module with ? and ! functions
		"lib/my_app/guards.ex": `defmodule MyApp.Guards do
  def valid?(thing) do
    true
  end

  def process!(thing) do
    :ok
  end
end
`,
		// Fully qualified call (no alias needed)
		"lib/my_app/workers/direct_worker.ex": `defmodule MyApp.Workers.DirectWorker do
  def run do
    MyApp.Repo.get(User, 1)
  end
end
`,
		// Test file (should be indexed but separate from lib)
		"test/my_app/repo_test.exs": `defmodule MyApp.RepoTest do
  def test_get do
    :ok
  end
end
`,
	}

	for relPath, content := range files {
		fullPath := filepath.Join(root, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func runDexter(t *testing.T, binary string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dexter %v failed: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestIntegration_InitAndLookup(t *testing.T) {
	binary := buildDexter(t)
	root := scaffoldProject(t)

	// Index the project
	out := runDexter(t, binary, root, "init", root)
	if !strings.Contains(out, "Indexed") {
		t.Fatalf("expected index output, got: %s", out)
	}

	tests := []struct {
		name     string
		args     []string
		expected []string // substrings that must appear in output
		notIn    []string // substrings that must NOT appear
	}{
		{
			name:     "simple module lookup",
			args:     []string{"lookup", "MyApp.Repo"},
			expected: []string{"lib/my_app/repo.ex:1"},
		},
		{
			name:     "function lookup",
			args:     []string{"lookup", "MyApp.Repo", "get"},
			expected: []string{"lib/my_app/repo.ex:2"},
		},
		{
			name:     "function with bang",
			args:     []string{"lookup", "MyApp.Repo", "get!"},
			expected: []string{"lib/my_app/repo.ex:6"},
		},
		{
			name:     "private function lookup",
			args:     []string{"lookup", "MyApp.Repo", "build_query"},
			expected: []string{"lib/my_app/repo.ex:10"},
		},
		{
			name:     "multiple function heads",
			args:     []string{"lookup", "MyApp.Handlers.Webhooks", "process_event"},
			expected: []string{"lib/my_app/handlers/webhooks.ex:2", "lib/my_app/handlers/webhooks.ex:6", "lib/my_app/handlers/webhooks.ex:10"},
		},
		{
			name:     "nested module - outer",
			args:     []string{"lookup", "MyApp.Outer"},
			expected: []string{"lib/my_app/outer.ex:1"},
		},
		{
			name:     "nested module - inner",
			args:     []string{"lookup", "MyApp.Outer.Inner"},
			expected: []string{"lib/my_app/outer.ex:6"},
		},
		{
			name:     "inner module function",
			args:     []string{"lookup", "MyApp.Outer.Inner", "inner_func"},
			expected: []string{"lib/my_app/outer.ex:7"},
		},
		{
			name:     "macro lookup",
			args:     []string{"lookup", "MyApp.Macros", "my_macro"},
			expected: []string{"lib/my_app/macros.ex:2"},
		},
		{
			name:     "private macro lookup",
			args:     []string{"lookup", "MyApp.Macros", "private_macro"},
			expected: []string{"lib/my_app/macros.ex:6"},
		},
		{
			name:     "question mark function",
			args:     []string{"lookup", "MyApp.Guards", "valid?"},
			expected: []string{"lib/my_app/guards.ex:2"},
		},
		{
			name:     "bang function",
			args:     []string{"lookup", "MyApp.Guards", "process!"},
			expected: []string{"lib/my_app/guards.ex:6"},
		},
		{
			name:     "test file module",
			args:     []string{"lookup", "MyApp.RepoTest"},
			expected: []string{"test/my_app/repo_test.exs:1"},
		},
		{
			name:     "nonexistent module",
			args:     []string{"lookup", "MyApp.DoesNotExist"},
			expected: []string{""}, // empty output
		},
		{
			name:     "nonexistent function",
			args:     []string{"lookup", "MyApp.Repo", "nonexistent"},
			expected: []string{""}, // empty output, falls back to module on the Lua side
		},
		{
			name:     "import target function",
			args:     []string{"lookup", "MyApp.Helpers.Formatting", "format_currency"},
			expected: []string{"lib/my_app/helpers/formatting.ex:2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := runDexter(t, binary, root, tt.args...)
			for _, exp := range tt.expected {
				if exp == "" && out == "" {
					continue
				}
				if exp != "" && !strings.Contains(out, exp) {
					t.Errorf("expected output to contain %q, got:\n%s", exp, out)
				}
			}
			for _, notExp := range tt.notIn {
				if strings.Contains(out, notExp) {
					t.Errorf("expected output NOT to contain %q, got:\n%s", notExp, out)
				}
			}
		})
	}
}

func TestIntegration_Reindex(t *testing.T) {
	binary := buildDexter(t)
	root := scaffoldProject(t)

	runDexter(t, binary, root, "init", root)

	// Verify initial state
	out := runDexter(t, binary, root, "lookup", "MyApp.Repo", "get")
	if !strings.Contains(out, "repo.ex:2") {
		t.Fatalf("expected get at line 2, got: %s", out)
	}

	// Modify the file — add a new function at the top
	repoPath := filepath.Join(root, "lib/my_app/repo.ex")
	newContent := `defmodule MyApp.Repo do
  def all(schema) do
    :ok
  end

  def get(schema, id) do
    :ok
  end
end
`
	if err := os.WriteFile(repoPath, []byte(newContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Reindex just that file
	runDexter(t, binary, root, "reindex", repoPath)

	// New function should exist
	out = runDexter(t, binary, root, "lookup", "MyApp.Repo", "all")
	if !strings.Contains(out, "repo.ex:2") {
		t.Errorf("expected all at line 2, got: %s", out)
	}

	// get should now be at line 6
	out = runDexter(t, binary, root, "lookup", "MyApp.Repo", "get")
	if !strings.Contains(out, "repo.ex:6") {
		t.Errorf("expected get at line 6 after reindex, got: %s", out)
	}

	// Old functions that were removed should fall back to module
	out = runDexter(t, binary, root, "lookup", "MyApp.Repo", "build_query")
	if strings.Contains(out, "build_query") {
		t.Errorf("expected build_query definition to be gone, got: %s", out)
	}
	// Should fall back to module line
	if !strings.Contains(out, "repo.ex:1") {
		t.Errorf("expected fallback to module line, got: %s", out)
	}
}

func TestIntegration_LookupFindsDbViaGitRoot(t *testing.T) {
	binary := buildDexter(t)
	root := scaffoldProject(t)

	// Create a monorepo-like structure: .git at root, subapp with mix.exs
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	subapp := filepath.Join(root, "apps", "myapp")
	if err := os.MkdirAll(subapp, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subapp, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Init explicitly at the .git root (monorepo root)
	runDexter(t, binary, root, "init", root)

	// Lookup run from the subapp should find the db at the .git root
	out := runDexter(t, binary, subapp, "lookup", "MyApp.Repo")
	if !strings.Contains(out, "repo.ex:1") {
		t.Errorf("expected lookup to find MyApp.Repo via .git root db, got: %s", out)
	}
}

func TestIntegration_LookupPrefersGitOverMixExs(t *testing.T) {
	binary := buildDexter(t)

	// Monorepo root with .git
	monorepoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(monorepoRoot, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(monorepoRoot, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Subapp with its own mix.exs
	subapp := filepath.Join(monorepoRoot, "apps", "my_app")
	if err := os.MkdirAll(subapp, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subapp, "mix.exs"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Index at the monorepo root
	scaffoldFiles := map[string]string{
		"mix.exs": `defmodule MyApp.MixProject do use Mix.Project end`,
		"apps/my_app/lib/foo.ex": `defmodule Foo do
  def bar do :ok end
end`,
	}
	for relPath, content := range scaffoldFiles {
		fullPath := filepath.Join(monorepoRoot, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runDexter(t, binary, monorepoRoot, "init", monorepoRoot)

	// Lookup from subapp should find the db at .git root, not create a new one at mix.exs
	out := runDexter(t, binary, subapp, "lookup", "Foo")
	if !strings.Contains(out, "foo.ex:1") {
		t.Errorf("expected lookup to find Foo via .git root db, got: %s", out)
	}

	if _, err := os.Stat(filepath.Join(subapp, ".dexter", "dexter.db")); err == nil {
		t.Error("should not have created a second .dexter/dexter.db in the subapp")
	}
}

func TestIntegration_InitForce(t *testing.T) {
	binary := buildDexter(t)
	root := scaffoldProject(t)

	// First init
	runDexter(t, binary, root, "init", root)

	// Second init without --force should fail
	cmd := exec.Command(binary, "init", root)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected init to fail without --force")
	}
	if !strings.Contains(string(out), "already exists") {
		t.Errorf("expected 'already exists' message, got: %s", out)
	}

	// With --force should succeed
	out2 := runDexter(t, binary, root, "init", "--force", root)
	if !strings.Contains(out2, "Indexed") {
		t.Fatalf("expected successful re-init with --force, got: %s", out2)
	}

	// Lookups should still work after force reinit
	out3 := runDexter(t, binary, root, "lookup", "MyApp.Repo")
	if !strings.Contains(out3, "repo.ex:1") {
		t.Errorf("expected repo lookup to work after force reinit, got: %s", out3)
	}
}

// TestIntegration_CorruptDBRecovery simulates the LSP startup recovery path:
// a corrupted DB is detected, deleted, and rebuilt so that lookups still work.
// This mirrors the open-with-retry loop in cmdLSP.
func TestIntegration_CorruptDBRecovery(t *testing.T) {
	binary := buildDexter(t)
	root := scaffoldProject(t)

	runDexter(t, binary, root, "init", root)

	// Corrupt the DB with garbage bytes
	dbPath := store.DBPath(root)
	if err := os.WriteFile(dbPath, []byte("not a sqlite database"), 0644); err != nil {
		t.Fatal(err)
	}

	// init --force should detect the garbage file, delete it, and rebuild cleanly
	out := runDexter(t, binary, root, "init", "--force", root)
	if !strings.Contains(out, "Indexed") {
		t.Fatalf("expected successful rebuild after corruption, got: %s", out)
	}

	// Lookups must work after recovery
	out2 := runDexter(t, binary, root, "lookup", "MyApp.Repo", "get")
	if !strings.Contains(out2, "repo.ex:2") {
		t.Errorf("expected lookup to work after corrupt DB recovery, got: %s", out2)
	}
}

// TestIntegration_LegacyMigration simulates an upgrade path: a project with
// the pre-.dexter/ folder layout (legacy .dexter.db file and its WAL
// siblings at the root) is migrated automatically on the next dexter
// invocation. The legacy files should be deleted and a fresh index built
// at .dexter/dexter.db.
func TestIntegration_LegacyMigration(t *testing.T) {
	binary := buildDexter(t)
	root := scaffoldProject(t)

	// Seed the legacy layout.
	legacy := filepath.Join(root, ".dexter.db")
	for _, f := range []string{legacy, legacy + "-shm", legacy + "-wal"} {
		if err := os.WriteFile(f, []byte("legacy placeholder"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Run init — migration runs inside store.Open.
	runDexter(t, binary, root, "init", root)

	// Legacy files should be gone.
	for _, f := range []string{legacy, legacy + "-shm", legacy + "-wal"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("legacy file %s still exists after migration", f)
		}
	}

	// New DB should exist and be functional.
	newDB := filepath.Join(root, ".dexter", "dexter.db")
	if _, err := os.Stat(newDB); err != nil {
		t.Errorf("new DB not created: %v", err)
	}

	// Lookups should work against the migrated index.
	out := runDexter(t, binary, root, "lookup", "MyApp.Repo")
	if !strings.Contains(out, "repo.ex:1") {
		t.Errorf("expected lookup to work after migration, got: %s", out)
	}
}
