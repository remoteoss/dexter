package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/evidence"
	"github.com/remoteoss/dexter/internal/parser"
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
	if stats.CallEdges != 1 {
		t.Errorf("CallEdges = %d, want 1", stats.CallEdges)
	}

	results, err := s.LookupFunction("MyApp.Accounts", "create_user")
	if err != nil || len(results) == 0 {
		t.Fatalf("create_user not indexed: %v", err)
	}

	refs, err := s.LookupReferences("SharedLib.Worker", "perform")
	if err != nil || len(refs) == 0 {
		t.Fatalf("call site not indexed: %v", err)
	}
	callers, err := s.LookupCallers(parser.FunctionID{Module: "SharedLib.Worker", Function: "perform", Arity: 1})
	if err != nil || len(callers) != 1 {
		t.Fatalf("caller edge not indexed: callers=%v err=%v", callers, err)
	}
}

func TestImpactBuildWritesCompactSnapshotDirectly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def fetch(id), do: SharedLib.Worker.perform(id)
end`)
	writeFile(t, dir, "test/accounts_test.exs", `defmodule MyApp.AccountsTest do
  def helper(id), do: MyApp.Accounts.fetch(id)
end`)
	writeFile(t, dir, "deps/vendor/lib/vendor.ex", `defmodule Vendor do
  def ignored, do: :ok
end`)

	output := filepath.Join(t.TempDir(), "impact.db")
	stats, err := ImpactBuild(output, dir, "abc123", "apps/example", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 2 {
		t.Fatalf("Files = %d, want 2", stats.Files)
	}
	if stats.References != 0 {
		t.Fatalf("References = %d, want 0 for impact-only parsing", stats.References)
	}

	snapshot, err := store.OpenImpactSnapshot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	paths, err := snapshot.ListFilePaths()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lib/accounts.ex", "test/accounts_test.exs"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if commit, err := snapshot.ImpactSnapshotCommit(); err != nil || commit != "abc123" {
		t.Fatalf("commit = %q, err = %v", commit, err)
	}
	callers, err := snapshot.LookupCallers(parser.FunctionID{Module: "SharedLib.Worker", Function: "perform", Arity: 1})
	if err != nil || len(callers) != 1 {
		t.Fatalf("callers = %+v, err = %v", callers, err)
	}
	records, err := snapshot.ListTestFunctionRecords()
	if err != nil || len(records) != 1 || records[0].FilePath != "test/accounts_test.exs" {
		t.Fatalf("test roots = %+v, err = %v", records, err)
	}

	normal, err := store.OpenTemporary(filepath.Join(t.TempDir(), "normal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = normal.Close() }()
	if _, err := FullBuild(normal, dir, Options{}); err != nil {
		t.Fatal(err)
	}
	exportedPath := filepath.Join(t.TempDir(), "exported.db")
	if err := normal.ExportImpactSnapshot(exportedPath, dir, "abc123", "apps/example"); err != nil {
		t.Fatal(err)
	}
	exported, err := store.OpenImpactSnapshot(exportedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exported.Close() }()
	exportedPaths, err := exported.ListFilePaths()
	if err != nil || !reflect.DeepEqual(exportedPaths, paths) {
		t.Fatalf("exported paths = %v, direct paths = %v, err = %v", exportedPaths, paths, err)
	}
	directFunctions, err := snapshot.ListFunctionFingerprints()
	if err != nil {
		t.Fatal(err)
	}
	exportedFunctions, err := exported.ListFunctionFingerprints()
	if err != nil || !reflect.DeepEqual(exportedFunctions, directFunctions) {
		t.Fatalf("exported functions differ from direct build: exported=%+v direct=%+v err=%v", exportedFunctions, directFunctions, err)
	}
	exportedRoots, err := exported.ListTestFunctionRecords()
	if err != nil || !reflect.DeepEqual(exportedRoots, records) {
		t.Fatalf("exported test roots = %+v, direct roots = %+v, err = %v", exportedRoots, records, err)
	}
}

func TestImpactBuildKeepsUnreadableFileInInventory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, an unreadable file is still readable")
	}
	dir := t.TempDir()
	writeFile(t, dir, "lib/readable.ex", "defmodule MyApp.Readable do\n  def run, do: :ok\nend")
	unreadable := writeFile(t, dir, "test/unreadable_test.exs", "defmodule MyApp.UnreadableTest do\nend")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	var warnings int
	output := filepath.Join(t.TempDir(), "impact.db")
	stats, err := ImpactBuild(output, dir, "abc123", ".", Options{
		Warn: func(string, ...interface{}) { warnings++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Files != 2 || warnings != 1 {
		t.Fatalf("Files = %d, warnings = %d; want 2 files and 1 warning", stats.Files, warnings)
	}
	snapshot, err := store.OpenImpactSnapshot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	paths, err := snapshot.ListFilePaths()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"lib/readable.ex", "test/unreadable_test.exs"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	missing, err := snapshot.MissingImpactEvidenceProviders()
	if err != nil || !reflect.DeepEqual(missing, []string{"source_parse"}) {
		t.Fatalf("missing providers = %v, err = %v", missing, err)
	}
}

func TestFullBuildDoesNotCertifyUnreadableSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, an unreadable file is still readable")
	}
	dir := t.TempDir()
	writeFile(t, dir, "lib/readable.ex", "defmodule MyApp.Readable do\n  def run, do: :ok\nend")
	unreadable := writeFile(t, dir, "lib/unreadable.ex", "defmodule MyApp.Unreadable do\nend")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	index, err := store.OpenTemporary(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	if _, err := FullBuild(index, dir, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := index.ValidateImpactSource(); err == nil {
		t.Fatal("incomplete source index was certified for impact export")
	}
}

func TestImpactBuildAddsRepositoryEvidenceMappings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "lib/events.ex", "defmodule MyApp.Events do\n  def dispatch(value), do: value\nend")
	config := `{
  "schema_version": 1,
  "required_providers": ["framework_runtime"],
  "mappings": [{
    "caller": {"module": "MyApp.Events", "function": "dispatch", "arity": 1},
    "callee": {"module": "SharedLib.Consumer", "function": "handle", "arity": 1},
    "kind": "hook"
  }]
}`
	writeFile(t, dir, "dexter-impact.json", config)

	output := filepath.Join(t.TempDir(), "impact.db")
	if _, err := ImpactBuild(output, dir, "abc123", ".", Options{}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.OpenImpactSnapshot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	callers, err := snapshot.LookupCallers(parser.FunctionID{Module: "SharedLib.Consumer", Function: "handle", Arity: 1})
	wantCaller := parser.FunctionID{Module: "MyApp.Events", Function: "dispatch", Arity: 1}
	if err != nil || len(callers) != 1 || callers[0].Function != wantCaller || callers[0].Kind != "repository:hook" {
		t.Fatalf("callers = %+v, err = %v", callers, err)
	}
	missing, err := snapshot.MissingImpactEvidenceProviders()
	if err != nil || !reflect.DeepEqual(missing, []string{"framework_runtime"}) {
		t.Fatalf("missing providers = %v, err = %v", missing, err)
	}
}

func TestImpactBuildMergesRevisionMatchedProviderEvidence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "test/generated_test.exs", "defmodule MyApp.GeneratedTest do\nend")
	writeFile(t, dir, "dexter-impact.json", `{"schema_version":1,"required_providers":["compiler_trace"]}`)
	evidencePath := writeFile(t, t.TempDir(), "compiled.json", `{
  "schema_version": 1,
  "provider": "compiler_trace",
  "revision": "abc123",
  "functions": [{
    "function": {"module": "MyApp.GeneratedTest", "function": "generated_setup", "arity": 1},
    "file": "test/generated_test.exs",
    "fingerprint": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  }],
  "edges": [{
    "caller": {"module": "MyApp.GeneratedTest", "function": "generated_setup", "arity": 1},
    "callee": {"module": "SharedLib.Worker", "function": "perform", "arity": 1},
    "kind": "compiled_call"
  }],
  "test_ownership": [{
    "root": {"module": "MyApp.GeneratedTest", "function": "__dexter_test_root__", "arity": 0},
    "function": {"module": "MyApp.GeneratedTest", "function": "generated_setup", "arity": 1},
    "file": "test/generated_test.exs"
  }],
  "unresolved": [{
    "caller": {"module": "MyApp.GeneratedTest", "function": "generated_setup", "arity": 1},
    "kind": "dynamic_call",
    "detail": "variable module"
  }]
}`)
	providers, err := evidence.LoadArtifacts([]string{evidencePath}, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "impact.db")
	if _, err := ImpactBuild(output, dir, "abc123", ".", Options{ProviderEvidence: providers}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.OpenImpactSnapshot(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	missing, err := snapshot.MissingImpactEvidenceProviders()
	if err != nil || len(missing) != 0 {
		t.Fatalf("missing providers = %v, err = %v", missing, err)
	}
	callers, err := snapshot.LookupCallers(parser.FunctionID{Module: "SharedLib.Worker", Function: "perform", Arity: 1})
	wantCaller := parser.FunctionID{Module: "MyApp.GeneratedTest", Function: "generated_setup", Arity: 1}
	if err != nil || len(callers) != 1 || callers[0].Function != wantCaller || callers[0].Kind != "provider:compiler_trace:compiled_call" {
		t.Fatalf("callers = %+v, err = %v", callers, err)
	}
	records, err := snapshot.ListTestFunctionRecords()
	if err != nil || len(records) != 1 || records[0].FilePath != "test/generated_test.exs" {
		t.Fatalf("test roots = %+v, err = %v", records, err)
	}
	unresolved, err := snapshot.ListImpactUnresolved()
	if err != nil || len(unresolved) != 1 || unresolved[0].Kind != "dynamic_call" {
		t.Fatalf("unresolved = %+v, err = %v", unresolved, err)
	}
}

func TestDeclaredBeamPathsExcludeOrphans(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "sample/ebin/sample.app", `{application,sample,[
  {modules,['Elixir.MyApp.Worker',sample_server]}
]}.`)
	wantFirst := writeFile(t, root, "sample/ebin/Elixir.MyApp.Worker.beam", "first")
	wantSecond := writeFile(t, root, "sample/ebin/sample_server.beam", "second")
	writeFile(t, root, "sample/ebin/Elixir.MyApp.Orphan.beam", "orphan")

	paths, err := declaredBeamPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{wantFirst, wantSecond}
	sort.Strings(want)
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestDeclaredBeamPathsRejectMissingModule(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "sample/ebin/sample.app", `{application,sample,[{modules,['Elixir.MyApp.Missing']} ]}.`)
	if _, err := declaredBeamPaths(root); err == nil || !strings.Contains(err.Error(), "Elixir.MyApp.Missing") {
		t.Fatalf("error = %v, want missing declared module", err)
	}
}

func TestBuiltinCallbackDispatchesConnectFrameworkCalls(t *testing.T) {
	want := parser.CallEdge{
		Caller: parser.FunctionID{Module: "GenServer", Function: "call", Arity: 2},
		Callee: parser.FunctionID{Module: "callback:GenServer", Function: "handle_call", Arity: 3},
		Kind:   "callback_dispatch",
	}
	for _, edge := range builtinCallbackDispatches() {
		if edge == want {
			return
		}
	}
	t.Fatalf("missing dispatch edge %+v", want)
}

func TestFullBuildRealInventory(t *testing.T) {
	root := os.Getenv("DEXTER_FULL_BUILD_PROJECT_ROOT")
	if root == "" {
		t.Skip("DEXTER_FULL_BUILD_PROJECT_ROOT is not set")
	}
	index, err := store.OpenTemporary(filepath.Join(t.TempDir(), "normal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	stats, err := FullBuild(index, root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("files=%d definitions=%d references=%d edges=%d total=%s parse=%s write=%s indexes=%s",
		stats.Files, stats.Definitions, stats.References, stats.CallEdges, stats.Total,
		stats.Parse, stats.Write, stats.CreateIndexes)
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
	if err := s.ValidateImpactSource(); err != nil {
		t.Errorf("full build did not stamp valid impact source evidence: %v", err)
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
		"idx_call_edges_caller",
		"idx_call_edges_callee",
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
