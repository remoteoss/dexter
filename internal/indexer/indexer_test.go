package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
)

func TestRestoreIndexesFailureIsUnindexed(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	err = restoreIndexes(s, errors.New("bulk write failed"))
	if !errors.Is(err, ErrUnindexed) {
		t.Fatalf("restoreIndexes error = %v, want ErrUnindexed", err)
	}
}

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
	// Every index createIndexes builds, not a sample of them. Silently losing
	// idx_definitions_using, for instance, turns LookupUsingModules — and so
	// the server's warmUsingCache — back into a scan of every definition.
	//
	// DropIndexes names eleven; the other five were retired earlier and it
	// clears them out on purpose, so createIndexes does not rebuild them.
	for _, want := range []string{
		"idx_definitions_module_function",
		"idx_definitions_file_id_line",
		"idx_definitions_delegate_to",
		"idx_definitions_using",
		"idx_refs_module_function",
		"idx_refs_file_id",
	} {
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

// A write failure inside the bulk transaction has to end the build. Continuing
// would commit a files row carrying a current mtime with no symbols behind it,
// which the mtime sweep then skips forever, and would stamp an index version
// that stops the next start from rebuilding.
//
// An already-indexed path is the cheapest deterministic write failure: the
// insert-only batch cannot upsert, so it fails on files.path UNIQUE at the
// first file. FullBuild is only ever called on an empty index in production —
// this reaches the same error path without needing to break the database.
func TestFullBuild_WriteFailureFailsTheBuild(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/a.ex", "defmodule A do\n  def a, do: :ok\nend")

	s := openStore(t, dir)
	if err := s.IndexFileWithRefs(filepath.Join(dir, "lib", "a.ex"), nil, nil); err != nil {
		t.Fatal(err)
	}

	var warnings int
	if _, err := FullBuild(s, dir, Options{
		Warn: func(format string, args ...interface{}) { warnings++ },
	}); err == nil {
		t.Fatal("FullBuild succeeded despite a failed write")
	}
	if got := s.GetIndexVersion(); got == version.IndexVersion {
		t.Errorf("index version = %d after a failed build; the next start will not rebuild", got)
	}
	if warnings != 0 {
		t.Errorf("write failure produced %d warnings; it must not be demoted to one", warnings)
	}

	// A failed bulk load has to leave the indexes behind, or every query on the
	// database it left is a table scan.
	names, err := s.IndexNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Error("no indexes on the database after a failed build")
	}
}

// The stdlib root can resolve inside the project root — an explicit
// stdlibPath, DEXTER_ELIXIR_LIB_ROOT, or a vendored checkout under deps/. The
// insert-only batch cannot upsert, so an overlapping path would fail on
// files.path UNIQUE. The overlap is dropped from the stdlib pass instead, so
// the project pass wins and keeps its refs.
func TestFullBuild_StdlibInsideProjectRootKeepsRefs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/a.ex", "defmodule A do\n  def a, do: :ok\nend")
	writeFile(t, dir, "deps/elixir/lib/enum.ex", `defmodule Enum do
  def map(list, fun), do: OtherMod.helper(list, fun)
end`)

	s := openStore(t, dir)

	stats, err := FullBuild(s, dir, Options{StdlibRoot: filepath.Join(dir, "deps", "elixir")})
	if err != nil {
		t.Fatalf("stdlib root inside the project root failed the build: %v", err)
	}
	if stats.Files != 2 {
		t.Errorf("Files = %d, want 2 — the overlapping file was counted twice", stats.Files)
	}

	if results, _ := s.LookupFunction("Enum", "map"); len(results) != 1 {
		t.Errorf("Enum.map has %d definitions, want 1", len(results))
	}
	// The project pass indexes refs; the stdlib pass does not. Losing these is
	// how the overlap used to fail — silently, behind one warning.
	refs, err := s.LookupReferences("OtherMod", "helper")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 {
		t.Error("refs from the overlapping file were lost")
	}
}

// Two unreadable files, not one: Warn is called from every parse worker, so a
// single bad file cannot show whether the callback is serialised. Under -race,
// which CI runs, one file passes an unsynchronised callback and two do not.
func TestFullBuild_WarnsOnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/a.ex", "defmodule A do\n  def a, do: :ok\nend")
	for _, name := range []string{"lib/bad1.ex", "lib/bad2.ex"} {
		bad := writeFile(t, dir, name, "defmodule Bad do\nend")
		if err := os.Chmod(bad, 0o000); err != nil {
			t.Skipf("cannot chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })
	}

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
	if warnings != 2 {
		t.Errorf("warnings = %d, want 2 (one per unreadable file)", warnings)
	}
	if stats.Files != 1 {
		t.Errorf("Files = %d, want 1 (the readable file)", stats.Files)
	}
	if results, _ := s.LookupFunction("A", "a"); len(results) == 0 {
		t.Error("the readable file was not indexed")
	}
}
