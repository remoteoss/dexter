package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
)

func writeFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()
	path := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func TestFullBuild(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create_user(attrs), do: SharedLib.Worker.perform(attrs)
  defp validate(attrs), do: attrs
end`)
	writeFile(t, dir, "lib/worker.ex", `defmodule SharedLib.Worker do
  def perform(attrs), do: attrs
end`)

	s := openStore(t, dir)

	stats, err := FullBuild(s, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if stats.Files != 2 {
		t.Errorf("Files = %d, want 2", stats.Files)
	}
	if stats.Definitions != 5 {
		// 2 defmodule + create_user + validate + perform
		t.Errorf("Definitions = %d, want 5", stats.Definitions)
	}
	if stats.References == 0 {
		t.Error("References = 0, want the SharedLib.Worker.perform call")
	}

	results, err := s.LookupFunction("MyApp.Accounts", "create_user")
	if err != nil || len(results) == 0 {
		t.Fatalf("create_user not indexed: %v", err)
	}

	refs, err := s.LookupReferences("SharedLib.Worker", "perform")
	if err != nil || len(refs) == 0 {
		t.Fatalf("call site not indexed: %v", err)
	}
}

func TestFullBuild_SetsIndexVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/a.ex", "defmodule A do\n  def a, do: :ok\nend")

	s := openStore(t, dir)

	if got := s.GetIndexVersion(); got != 0 {
		t.Fatalf("fresh store reports version %d, want 0", got)
	}

	if _, err := FullBuild(s, dir, Options{}); err != nil {
		t.Fatal(err)
	}

	if got := s.GetIndexVersion(); got != version.IndexVersion {
		t.Errorf("index version = %d, want %d", got, version.IndexVersion)
	}
}

// The bulk path drops the indexes before it writes. They have to come back, or
// every later query falls back to a table scan.
func TestFullBuild_RestoresIndexes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/a.ex", "defmodule A do\n  def a, do: :ok\nend")

	s := openStore(t, dir)
	if _, err := FullBuild(s, dir, Options{}); err != nil {
		t.Fatal(err)
	}

	names, err := s.IndexNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no indexes on the database after a full build")
	}
	for _, want := range []string{"idx_definitions_module_function", "idx_refs_module_function"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("index %s missing after full build (have %v)", want, names)
		}
	}
}

func TestFullBuild_StdlibDefinitionsOnly(t *testing.T) {
	dir := t.TempDir()
	stdlibDir := t.TempDir()
	writeFile(t, dir, "lib/a.ex", "defmodule A do\n  def a, do: :ok\nend")
	writeFile(t, stdlibDir, "elixir/lib/enum.ex", `defmodule Enum do
  def map(list, fun), do: OtherMod.helper(list, fun)
end`)

	s := openStore(t, dir)

	if _, err := FullBuild(s, dir, Options{StdlibRoot: stdlibDir}); err != nil {
		t.Fatal(err)
	}

	results, err := s.LookupFunction("Enum", "map")
	if err != nil || len(results) == 0 {
		t.Fatalf("stdlib definition not indexed: %v", err)
	}

	refs, err := s.LookupReferences("OtherMod", "helper")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("stdlib refs were indexed (%d), want none", len(refs))
	}
}

func TestFullBuild_WarnsOnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/a.ex", "defmodule A do\n  def a, do: :ok\nend")
	bad := writeFile(t, dir, "lib/bad.ex", "defmodule Bad do\nend")
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })

	if os.Geteuid() == 0 {
		t.Skip("running as root, an unreadable file is still readable")
	}

	s := openStore(t, dir)

	var warnings int
	stats, err := FullBuild(s, dir, Options{
		Warn: func(format string, args ...interface{}) { warnings++ },
	})
	if err != nil {
		t.Fatalf("one unreadable file should not fail the build: %v", err)
	}
	if warnings == 0 {
		t.Error("no warning for the unreadable file")
	}
	if stats.Files != 1 {
		t.Errorf("Files = %d, want 1 (the readable file)", stats.Files)
	}
	if results, _ := s.LookupFunction("A", "a"); len(results) == 0 {
		t.Error("the readable file was not indexed")
	}
}
