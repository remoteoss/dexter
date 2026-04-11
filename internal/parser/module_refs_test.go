package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModuleRefExtraction(t *testing.T) {
	dir := t.TempDir()
	content := `defmodule MyApp.Worker do
  @behaviour MyApp.WorkerBehaviour
  @impl MyApp.WorkerBehaviour
  @derive [Jason.Encoder, MyApp.Protocol]

  alias MyApp.Accounts.User

  @spec process(User.t()) :: :ok
  def process(%User{} = user) do
    case user do
      %MyApp.Accounts.User{role: :admin} -> :ok
    end
  rescue
    e in MyApp.CustomError -> :error
  end
end
`
	path := filepath.Join(dir, "worker.ex")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	moduleLines := map[string][]int{}
	for _, r := range refs {
		moduleLines[r.Module] = append(moduleLines[r.Module], r.Line)
	}

	tests := []struct {
		module string
		desc   string
	}{
		{"MyApp.WorkerBehaviour", "@behaviour reference"},
		{"MyApp.WorkerBehaviour", "@impl reference"},
		{"Jason.Encoder", "@derive reference"},
		{"MyApp.Protocol", "@derive reference (second)"},
		{"MyApp.Accounts.User", "alias reference"},
		{"MyApp.Accounts.User", "User.t() type reference"},
		{"MyApp.Accounts.User", "%User{} struct literal"},
		{"MyApp.Accounts.User", "%MyApp.Accounts.User{} qualified struct"},
		{"MyApp.CustomError", "rescue in Module"},
	}
	for _, tt := range tests {
		if lines, ok := moduleLines[tt.module]; !ok || len(lines) == 0 {
			t.Errorf("missing ref for %s (%s)", tt.module, tt.desc)
			t.Logf("all refs:")
			for _, r := range refs {
				t.Logf("  module=%-30s func=%-10s kind=%-8s line=%d", r.Module, r.Function, r.Kind, r.Line)
			}
			break
		}
	}
}

func TestMultiAliasRefs(t *testing.T) {
	dir := t.TempDir()
	content := `defmodule MyApp.Web do
  alias MyApp.{Accounts, Users, Profiles}

  def test do
    Accounts.list()
    Users.get(1)
  end
end
`
	path := filepath.Join(dir, "web.ex")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	for _, r := range refs {
		found[r.Module] = true
	}

	for _, mod := range []string{"MyApp.Accounts", "MyApp.Users", "MyApp.Profiles"} {
		if !found[mod] {
			t.Errorf("missing ref for %s from multi-alias", mod)
		}
	}
}

func TestMultiLineMultiAliasRefs(t *testing.T) {
	dir := t.TempDir()
	content := `defmodule MyApp.Web do
  alias MyApp.{
    Accounts,
    Users,
    Profiles
  }

  def test do
    Accounts.list()
    Users.get(1)
  end
end
`
	path := filepath.Join(dir, "web.ex")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	for _, r := range refs {
		found[r.Module] = true
	}

	for _, mod := range []string{"MyApp.Accounts", "MyApp.Users", "MyApp.Profiles"} {
		if !found[mod] {
			t.Errorf("missing ref for %s from multi-line multi-alias", mod)
		}
	}
}

func TestMultiLineMultiAliasWithComments(t *testing.T) {
	dir := t.TempDir()
	content := `defmodule MyApp.Web do
  alias MyApp.{
    Accounts,
    # Users is deprecated
    Profiles
  }

  def test do
    Accounts.list()
  end
end
`
	path := filepath.Join(dir, "web.ex")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	for _, r := range refs {
		if r.Kind == "alias" {
			found[r.Module] = true
		}
	}

	for _, mod := range []string{"MyApp.Accounts", "MyApp.Profiles"} {
		if !found[mod] {
			t.Errorf("missing ref for %s from multi-line multi-alias with comments", mod)
		}
	}
	if len(found) != 2 {
		t.Errorf("expected 2 alias refs, got %d: %v", len(found), found)
	}
}

func TestMultiLineMultiAliasMultiplePerLine(t *testing.T) {
	dir := t.TempDir()
	content := `defmodule MyApp.Web do
  alias MyApp.Handlers.{
    Accounts, Users,
    Profiles
  }

  def test do
    Accounts.list()
  end
end
`
	path := filepath.Join(dir, "web.ex")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	found := map[string]bool{}
	for _, r := range refs {
		if r.Kind == "alias" {
			found[r.Module] = true
		}
	}

	for _, mod := range []string{"MyApp.Handlers.Accounts", "MyApp.Handlers.Users", "MyApp.Handlers.Profiles"} {
		if !found[mod] {
			t.Errorf("missing ref for %s from multi-line multi-alias", mod)
		}
	}
}

func TestMultiLineAliasAsRefs(t *testing.T) {
	dir := t.TempDir()
	content := `defmodule MyApp.Controller do
  alias MyApp.Helpers.Paginator,
    as: Pages

  def index do
    Pages.call()
  end
end
`
	path := filepath.Join(dir, "controller.ex")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	_, refs, err := ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Should have an alias ref for the full module
	aliasFound := false
	for _, r := range refs {
		if r.Kind == "alias" && r.Module == "MyApp.Helpers.Paginator" {
			aliasFound = true
		}
	}
	if !aliasFound {
		t.Errorf("missing alias ref for MyApp.Helpers.Paginator from multi-line alias as:")
	}

	// Pages.call() should resolve to MyApp.Helpers.Paginator.call
	callFound := false
	for _, r := range refs {
		if r.Kind == "call" && r.Module == "MyApp.Helpers.Paginator" && r.Function == "call" {
			callFound = true
		}
	}
	if !callFound {
		t.Errorf("Pages.call() should resolve to MyApp.Helpers.Paginator.call via multi-line as: alias")
	}
}
