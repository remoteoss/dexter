package store

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
)

func TestDBPath(t *testing.T) {
	got := DBPath("/project/root")
	want := filepath.Join("/project/root", ".dexter", "dexter.db")
	if got != want {
		t.Errorf("DBPath = %q, want %q", got, want)
	}
}

func TestDBDir(t *testing.T) {
	got := DBDir("/project/root")
	want := filepath.Join("/project/root", ".dexter")
	if got != want {
		t.Errorf("DBDir = %q, want %q", got, want)
	}
}

func TestLegacyDBPath(t *testing.T) {
	got := LegacyDBPath("/project/root")
	want := filepath.Join("/project/root", ".dexter.db")
	if got != want {
		t.Errorf("LegacyDBPath = %q, want %q", got, want)
	}
}

func setupTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func writeElixirFile(t *testing.T, dir, relPath, content string) string {
	t.Helper()
	path := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIndexAndLookupModule(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/foo.ex", `defmodule MyApp.Foo do
  def bar do
    :ok
  end
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	results, err := s.LookupModule("MyApp.Foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 module result, got %d", len(results))
	}
	if results[0].FilePath != path || results[0].Line != 1 {
		t.Errorf("unexpected result: %+v", results[0])
	}
}

func TestIndexAndLookupFunction(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/foo.ex", `defmodule MyApp.Foo do
  def bar(arg) do
    :ok
  end

  defp secret do
    :hidden
  end
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	results, err := s.LookupFunction("MyApp.Foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Line != 2 {
		t.Errorf("expected line 2, got %d", results[0].Line)
	}

	results, err = s.LookupFunction("MyApp.Foo", "secret")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for defp, got %d", len(results))
	}
}

func TestLookupFunctionOrdersFunctionsBeforeTypes(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/foo.ex", `defmodule MyApp.Foo do
  @type bar :: atom()

  def bar(arg) do
    :ok
  end
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	results, err := s.LookupFunction("MyApp.Foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (function + type), got %d", len(results))
	}
	if results[0].Kind != "def" {
		t.Errorf("expected first result to be 'def', got %q", results[0].Kind)
	}
	if results[1].Kind != "type" {
		t.Errorf("expected second result to be 'type', got %q", results[1].Kind)
	}
}

func TestLookupEnclosingFunction_MultiLineSig(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	// Reproduces a real bug: multi-clause function followed by a function with a
	// multi-line signature. LookupEnclosingFunction must return the latter when
	// the cursor is anywhere on its definition line or within its body, not the
	// preceding multi-clause function.
	path := writeElixirFile(t, dir, "lib/worker.ex", `defmodule MyApp.Worker do
  defp resource_type(%PayrollRun{}), do: "payroll_run"
  defp resource_type(%StatutoryRemittance{}), do: "statutory_remittance"

  @impl true
  def process(%Job{
        args: %__MODULE__{
          resource_slug: resource_slug,
          resource_type: resource_type
        }
      }) do
    :ok
  end
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc   string
		line   int // 1-based, as passed to LookupEnclosingFunction
		wantFn string
	}{
		{"on def process line", 6, "process"},
		{"inside multi-line args", 8, "process"},
		{"on closing paren line", 10, "process"},
		{"inside body", 11, "process"},
		{"on resource_type clause 1", 2, "resource_type"},
		{"on resource_type clause 2", 3, "resource_type"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			_, fn, _, _, found := s.LookupEnclosingFunction(path, tt.line)
			if !found {
				t.Fatalf("expected to find enclosing function at line %d, got none", tt.line)
			}
			if fn != tt.wantFn {
				t.Errorf("line %d: got %q, want %q", tt.line, fn, tt.wantFn)
			}
		})
	}
}

func TestReindexUpdatesDefinitions(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/foo.ex", `defmodule MyApp.Foo do
  def bar do
    :ok
  end
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	// Rewrite the file with a different function
	writeElixirFile(t, dir, "lib/foo.ex", `defmodule MyApp.Foo do
  def baz do
    :ok
  end
end
`)

	defs, _, err = parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	// Old function should be gone
	results, err := s.LookupFunction("MyApp.Foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected bar to be removed, got %d results", len(results))
	}

	// New function should exist
	results, err = s.LookupFunction("MyApp.Foo", "baz")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result for baz, got %d", len(results))
	}
}

func TestRemoveFile(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/foo.ex", `defmodule MyApp.Foo do
  def bar do
    :ok
  end
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	if err := s.RemoveFile(path); err != nil {
		t.Fatal(err)
	}

	results, err := s.LookupModule("MyApp.Foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results after remove, got %d", len(results))
	}
}

func TestMtimeTracking(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/foo.ex", `defmodule MyApp.Foo do
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	mtime, found := s.GetFileMtime(path)
	if !found {
		t.Fatal("expected mtime to be tracked")
	}
	if mtime == 0 {
		t.Error("expected non-zero mtime")
	}

	_, found = s.GetFileMtime("/nonexistent/path.ex")
	if found {
		t.Error("expected false for nonexistent file")
	}
}

func TestSearchModules(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/handlers.ex", `defmodule MyApp.Handlers do
end

defmodule MyApp.Handlers.Webhooks do
end

defmodule MyApp.Handlers.Billing do
end

defmodule MyApp.Repo do
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	t.Run("prefix matches multiple modules", func(t *testing.T) {
		results, err := s.SearchModules("MyApp.Handler")
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 3 {
			t.Fatalf("expected 3 results, got %d", len(results))
		}
	})

	t.Run("exact prefix", func(t *testing.T) {
		results, err := s.SearchModules("MyApp.Repo")
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(results))
		}
		if results[0].Module != "MyApp.Repo" {
			t.Errorf("expected MyApp.Repo, got %q", results[0].Module)
		}
	})

	t.Run("no matches", func(t *testing.T) {
		results, err := s.SearchModules("NonExistent")
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 0 {
			t.Errorf("expected 0 results, got %d", len(results))
		}
	})

	t.Run("excludes defimpl", func(t *testing.T) {
		implPath := writeElixirFile(t, dir, "lib/impl.ex", `defimpl Jason.Encoder, for: MyApp.Handlers do
end
`)
		implDefs, _, err := parser.ParseFile(implPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.IndexFile(implPath, implDefs); err != nil {
			t.Fatal(err)
		}

		results, err := s.SearchModules("Jason.Encoder")
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 0 {
			t.Errorf("expected 0 results (defimpl excluded), got %d", len(results))
		}
	})
}

func TestListModuleFunctions(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def create(attrs) do
    :ok
  end

  def list(opts) do
    :ok
  end

  defp validate(attrs) do
    :ok
  end

  defmacro my_macro(expr) do
    quote do: unquote(expr)
  end

  defdelegate fetch(id), to: MyApp.Repo
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	t.Run("public only", func(t *testing.T) {
		results, err := s.ListModuleFunctions("MyApp.Accounts", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 4 {
			t.Fatalf("expected 4 public functions, got %d", len(results))
		}
		for _, r := range results {
			if r.Kind == "defp" {
				t.Error("should not include defp when publicOnly=true")
			}
		}
	})

	t.Run("all functions", func(t *testing.T) {
		results, err := s.ListModuleFunctions("MyApp.Accounts", false)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 5 {
			t.Fatalf("expected 5 total functions, got %d", len(results))
		}
	})

	t.Run("deduplicates multi-clause functions", func(t *testing.T) {
		multiPath := writeElixirFile(t, dir, "lib/webhooks.ex", `defmodule MyApp.Webhooks do
  def process("a", p) do
    :ok
  end

  def process("b", p) do
    :ok
  end

  def process("c", p) do
    :ok
  end
end
`)

		multiDefs, _, err := parser.ParseFile(multiPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.IndexFile(multiPath, multiDefs); err != nil {
			t.Fatal(err)
		}

		results, err := s.ListModuleFunctions("MyApp.Webhooks", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 {
			t.Fatalf("expected 1 deduplicated function, got %d", len(results))
		}
		if results[0].Function != "process" {
			t.Errorf("expected 'process', got %q", results[0].Function)
		}
	})

	t.Run("nonexistent module", func(t *testing.T) {
		results, err := s.ListModuleFunctions("NonExistent", true)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 0 {
			t.Errorf("expected 0 results, got %d", len(results))
		}
	})
}

func TestMultipleFunctionHeadsLookup(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/webhooks.ex", `defmodule MyApp.Webhooks do
  def process_event("completed", payload) do
    :ok
  end

  def process_event("declined", payload) do
    :declined
  end
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	results, err := s.LookupFunction("MyApp.Webhooks", "process_event")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for multiple heads, got %d", len(results))
	}
	if results[0].Line != 2 || results[1].Line != 6 {
		t.Errorf("unexpected lines: %d, %d", results[0].Line, results[1].Line)
	}
}

func TestIndexAndLookupReferences(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/accounts.ex", `defmodule MyApp.Accounts do
  alias MyApp.Repo

  def list do
    Repo.all(MyApp.User)
  end
end
`)

	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFileWithRefs(path, defs, refs); err != nil {
		t.Fatal(err)
	}

	// Look up references to MyApp.Repo
	results, err := s.LookupReferences("MyApp.Repo", "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 reference to MyApp.Repo.all, got %d", len(results))
	}
	if results[0].FilePath != path || results[0].Line != 5 {
		t.Errorf("unexpected result: %+v", results[0])
	}

	// Module-only lookup (alias reference)
	modResults, err := s.LookupReferences("MyApp.Repo", "")
	if err != nil {
		t.Fatal(err)
	}
	// Should include the alias line and the Repo.all call
	if len(modResults) < 1 {
		t.Errorf("expected at least 1 module-level reference, got %d", len(modResults))
	}
}

func TestReindexClearsOldRefs(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/foo.ex", `defmodule Foo do
  def bar, do: MyApp.Repo.all(MyApp.User)
end
`)

	defs, refs, _ := parser.ParseFile(path)
	_ = s.IndexFileWithRefs(path, defs, refs)

	results, _ := s.LookupReferences("MyApp.Repo", "all")
	if len(results) != 1 {
		t.Fatalf("expected 1 ref before reindex, got %d", len(results))
	}

	// Rewrite without the reference
	writeElixirFile(t, dir, "lib/foo.ex", `defmodule Foo do
  def bar, do: :ok
end
`)

	defs, refs, _ = parser.ParseFile(path)
	_ = s.IndexFileWithRefs(path, defs, refs)

	results, _ = s.LookupReferences("MyApp.Repo", "all")
	if len(results) != 0 {
		t.Errorf("expected 0 refs after reindex, got %d", len(results))
	}
}

func TestLookupReferencesIncludesBareMacroCalls(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	// File that defines the macro
	defPath := writeElixirFile(t, dir, "lib/schema.ex", `defmodule MyApp.EctoSchema do
  defmacro embedded_schema(do: block) do
    quote do: unquote(block)
  end
end
`)

	// File that calls the macro without module prefix (injected via use)
	callerPath := writeElixirFile(t, dir, "lib/user.ex", `defmodule MyApp.User do
  use MyApp.EctoSchema

  embedded_schema do
    field :name, :string
  end
end
`)

	defDefs, defRefs, err := parser.ParseFile(defPath)
	if err != nil {
		t.Fatal(err)
	}
	callerDefs, callerRefs, err := parser.ParseFile(callerPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.IndexFileWithRefs(defPath, defDefs, defRefs); err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFileWithRefs(callerPath, callerDefs, callerRefs); err != nil {
		t.Fatal(err)
	}

	results, err := s.LookupReferences("MyApp.EctoSchema", "embedded_schema")
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, r := range results {
		if r.FilePath == callerPath {
			found = true
		}
	}
	if !found {
		t.Errorf("expected bare call to embedded_schema in %s, got results: %v", callerPath, results)
	}
}

func TestBatchIndexMultipleFiles(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	pathA := writeElixirFile(t, dir, "lib/alpha.ex", `defmodule MyApp.Alpha do
  def run do
    :ok
  end
end
`)
	pathB := writeElixirFile(t, dir, "lib/beta.ex", `defmodule MyApp.Bravo do
  def start(arg) do
    :ok
  end

  def stop do
    :ok
  end
end
`)

	defsA, _, err := parser.ParseFile(pathA)
	if err != nil {
		t.Fatal(err)
	}
	defsB, _, err := parser.ParseFile(pathB)
	if err != nil {
		t.Fatal(err)
	}

	batch, err := s.BeginBatch()
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.IndexFile(pathA, defsA); err != nil {
		t.Fatal(err)
	}
	if err := batch.IndexFile(pathB, defsB); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	// Both modules should be queryable
	results, err := s.LookupModule("MyApp.Alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for Alpha, got %d", len(results))
	}

	results, err = s.LookupFunction("MyApp.Bravo", "start")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for Beta.start, got %d", len(results))
	}

	// Mtime should be tracked for both files
	_, found := s.GetFileMtime(pathA)
	if !found {
		t.Error("expected mtime for pathA")
	}
	_, found = s.GetFileMtime(pathB)
	if !found {
		t.Error("expected mtime for pathB")
	}
}

func TestBatchIndexFileWithMtime(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/delta.ex", `defmodule MyApp.Delta do
  def ping do
    :pong
  end
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	expectedMtime := int64(1234567890)
	batch, err := s.BeginBatch()
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.IndexFileWithMtime(path, expectedMtime, defs); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	// Definitions should be queryable
	results, err := s.LookupModule("MyApp.Delta")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for Delta, got %d", len(results))
	}

	// Mtime should be the value we passed, not from os.Stat
	mtime, found := s.GetFileMtime(path)
	if !found {
		t.Fatal("expected mtime to be tracked")
	}
	if mtime != expectedMtime {
		t.Errorf("expected mtime %d, got %d", expectedMtime, mtime)
	}
}

func TestBatchRollback(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/gamma.ex", `defmodule MyApp.Golf do
  def hello do
    :ok
  end
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}

	batch, err := s.BeginBatch()
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}
	if err := batch.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Nothing should have been persisted
	results, err := s.LookupModule("MyApp.Golf")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results after rollback, got %d", len(results))
	}
}

func TestSearchSubmoduleSegments(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/app.ex", `defmodule MyApp do
end

defmodule MyApp.Accounts do
end

defmodule MyApp.Accounts.User do
end

defmodule MyApp.Accounts.Team do
end

defmodule MyApp.Services do
end

defmodule MyApp.Services.Auth do
end

defmodule MyApp.Schema do
end
`)

	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	t.Run("all immediate children", func(t *testing.T) {
		segments, err := s.SearchSubmoduleSegments("MyApp", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(segments) != 3 {
			t.Fatalf("expected 3 segments, got %d: %v", len(segments), segments)
		}
		expected := map[string]bool{"Accounts": true, "Schema": true, "Services": true}
		for _, seg := range segments {
			if !expected[seg] {
				t.Errorf("unexpected segment: %q", seg)
			}
		}
	})

	t.Run("with prefix filter", func(t *testing.T) {
		segments, err := s.SearchSubmoduleSegments("MyApp", "S")
		if err != nil {
			t.Fatal(err)
		}
		if len(segments) != 2 {
			t.Fatalf("expected 2 segments, got %d: %v", len(segments), segments)
		}
		for _, seg := range segments {
			if seg != "Schema" && seg != "Services" {
				t.Errorf("unexpected segment: %q", seg)
			}
		}
	})

	t.Run("with specific prefix", func(t *testing.T) {
		segments, err := s.SearchSubmoduleSegments("MyApp", "Ser")
		if err != nil {
			t.Fatal(err)
		}
		if len(segments) != 1 || segments[0] != "Services" {
			t.Errorf("expected [Services], got %v", segments)
		}
	})

	t.Run("nested parent", func(t *testing.T) {
		segments, err := s.SearchSubmoduleSegments("MyApp.Accounts", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(segments) != 2 {
			t.Fatalf("expected 2 segments, got %d: %v", len(segments), segments)
		}
		expected := map[string]bool{"Team": true, "User": true}
		for _, seg := range segments {
			if !expected[seg] {
				t.Errorf("unexpected segment: %q", seg)
			}
		}
	})

	t.Run("no matches", func(t *testing.T) {
		segments, err := s.SearchSubmoduleSegments("MyApp", "Z")
		if err != nil {
			t.Fatal(err)
		}
		if len(segments) != 0 {
			t.Errorf("expected 0 segments, got %v", segments)
		}
	})
}

func TestStdlibRoot(t *testing.T) {
	s, _ := setupTestStore(t)
	defer func() { _ = s.Close() }()

	// Empty store returns nothing.
	if _, ok := s.GetStdlibRoot(); ok {
		t.Error("expected no stdlib root on fresh store")
	}

	if err := s.SetStdlibRoot("/path/to/elixir/lib"); err != nil {
		t.Fatal(err)
	}

	root, ok := s.GetStdlibRoot()
	if !ok {
		t.Fatal("expected stdlib root after set")
	}
	if root != "/path/to/elixir/lib" {
		t.Errorf("got %q, want %q", root, "/path/to/elixir/lib")
	}

	// Overwrite with a new value.
	if err := s.SetStdlibRoot("/new/path"); err != nil {
		t.Fatal(err)
	}
	root, _ = s.GetStdlibRoot()
	if root != "/new/path" {
		t.Errorf("got %q after overwrite, want %q", root, "/new/path")
	}
}

func TestOpenCorruptedDB(t *testing.T) {
	dir := t.TempDir()

	// Pre-create the .dexter/ folder and plant a garbage DB file in the
	// new location so Open's migration path doesn't touch it.
	if err := os.MkdirAll(DBDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := DBPath(dir)

	if err := os.WriteFile(dbPath, []byte("this is not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Open(dir)
	if err == nil {
		t.Fatal("expected Open to fail on a corrupted DB file, got nil")
	}
}

func TestOpen_CreatesDexterFolder(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if info, err := os.Stat(filepath.Join(dir, ".dexter")); err != nil || !info.IsDir() {
		t.Errorf(".dexter/ directory was not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".dexter", "dexter.db")); err != nil {
		t.Errorf(".dexter/dexter.db was not created: %v", err)
	}
	gitignore := filepath.Join(dir, ".dexter", ".gitignore")
	content, err := os.ReadFile(gitignore)
	if err != nil {
		t.Errorf(".dexter/.gitignore was not created: %v", err)
	} else if string(content) != "*\n" {
		t.Errorf(".dexter/.gitignore content = %q, want %q", string(content), "*\n")
	}
}

func TestOpen_MigratesLegacyLayout(t *testing.T) {
	dir := t.TempDir()

	// Seed a fake legacy database and its WAL siblings.
	legacy := filepath.Join(dir, ".dexter.db")
	legacyShm := legacy + "-shm"
	legacyWal := legacy + "-wal"
	for _, f := range []string{legacy, legacyShm, legacyWal} {
		if err := os.WriteFile(f, []byte("legacy placeholder"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	for _, f := range []string{legacy, legacyShm, legacyWal} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("legacy file %s still exists (err=%v)", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".dexter", "dexter.db")); err != nil {
		t.Errorf("new DB was not created: %v", err)
	}

	// Smoke test: the store should be functional after migration.
	if _, err := s.db.Exec("INSERT INTO files (path, mtime) VALUES (?, ?)", "/fake.ex", 1); err != nil {
		t.Errorf("store not functional after migration: %v", err)
	}
}

func TestOpen_MigrationWithPartialLegacyFiles(t *testing.T) {
	// Legacy DB present but no WAL siblings — should still migrate cleanly.
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".dexter.db")
	if err := os.WriteFile(legacy, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy .dexter.db still exists: %v", err)
	}
}

func TestSearchSymbols(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def list_users, do: []
  def create_user(attrs), do: attrs
  defp validate(attrs), do: attrs
end
`)
	defs, _, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path, defs); err != nil {
		t.Fatal(err)
	}

	path2 := writeElixirFile(t, dir, "lib/users.ex", `defmodule MyApp.Users do
  def get_user(id), do: nil
end
`)
	defs2, _, err := parser.ParseFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFile(path2, defs2); err != nil {
		t.Fatal(err)
	}

	// Search by module name
	results, err := s.SearchSymbols("Accounts")
	if err != nil {
		t.Fatal(err)
	}
	foundModule := false
	for _, r := range results {
		if r.Module == "MyApp.Accounts" && r.Function == "" {
			foundModule = true
		}
	}
	if !foundModule {
		t.Error("expected to find MyApp.Accounts module")
	}

	// Search by function name
	results, err = s.SearchSymbols("list_users")
	if err != nil {
		t.Fatal(err)
	}
	foundFunc := false
	for _, r := range results {
		if r.Function == "list_users" {
			foundFunc = true
			if r.Module != "MyApp.Accounts" {
				t.Errorf("expected module MyApp.Accounts, got %q", r.Module)
			}
		}
	}
	if !foundFunc {
		t.Error("expected to find list_users function")
	}

	// Search matching both modules and functions
	results, err = s.SearchSymbols("user")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected results for 'user' query")
	}

	// Verify private functions are included
	results, err = s.SearchSymbols("validate")
	if err != nil {
		t.Fatal(err)
	}
	foundPrivate := false
	for _, r := range results {
		if r.Function == "validate" {
			foundPrivate = true
		}
	}
	if !foundPrivate {
		t.Error("expected to find private function 'validate'")
	}

	// Search by partial qualified name: "Accounts.list_users" should match MyApp.Accounts.list_users
	results, err = s.SearchSymbols("Accounts.list_users")
	if err != nil {
		t.Fatal(err)
	}
	foundQualified := false
	for _, r := range results {
		if r.Module == "MyApp.Accounts" && r.Function == "list_users" {
			foundQualified = true
		}
	}
	if !foundQualified {
		t.Error("expected to find list_users via compound query 'Accounts.list_users'")
	}

	// Case-insensitive: "accounts.list_users" should still find the function
	results, err = s.SearchSymbols("accounts.list_users")
	if err != nil {
		t.Fatal(err)
	}
	foundCaseInsensitive := false
	for _, r := range results {
		if r.Module == "MyApp.Accounts" && r.Function == "list_users" {
			foundCaseInsensitive = true
		}
	}
	if !foundCaseInsensitive {
		t.Error("expected case-insensitive match for 'accounts.list_users'")
	}

	// Dotted module-only query "MyApp.Accounts" should still find the module
	results, err = s.SearchSymbols("MyApp.Accounts")
	if err != nil {
		t.Fatal(err)
	}
	foundModuleOnly := false
	for _, r := range results {
		if r.Module == "MyApp.Accounts" && r.Function == "" {
			foundModuleOnly = true
		}
	}
	if !foundModuleOnly {
		t.Error("expected to find MyApp.Accounts module via dotted module query")
	}

	// Exact match ranking: "MyApp.Accounts" should be the first result
	// when searching for that exact module name.
	results, err = s.SearchSymbols("MyApp.Accounts")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for exact module query")
	}
	if results[0].Module != "MyApp.Accounts" || results[0].Function != "" {
		t.Errorf("expected exact module match first, got %s.%s", results[0].Module, results[0].Function)
	}

	// Case-sensitive ranking: "Users" should rank MyApp.Users (exact case)
	// before MyApp.Accounts.list_users/create_user (case-insensitive "users" substring).
	results, err = s.SearchSymbols("Users")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results for 'Users' query")
	}
	if results[0].Module != "MyApp.Users" {
		t.Errorf("expected case-sensitive match MyApp.Users first, got %s.%s", results[0].Module, results[0].Function)
	}
}

func TestFindProjectRoot(t *testing.T) {
	// Helper: create a directory tree inside t.TempDir() and return the root.
	mktree := func(t *testing.T, files []string) string {
		t.Helper()
		root := t.TempDir()
		for _, rel := range files {
			full := filepath.Join(root, rel)
			if strings.HasSuffix(rel, "/") {
				if err := os.MkdirAll(full, 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(""), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}

	t.Run("new layout at root", func(t *testing.T) {
		root := mktree(t, []string{".dexter/dexter.db", "apps/app/lib/foo.ex"})
		got := FindProjectRoot(filepath.Join(root, "apps", "app"))
		if got != root {
			t.Errorf("got %q, want %q", got, root)
		}
	})

	t.Run("legacy file at root", func(t *testing.T) {
		root := mktree(t, []string{".dexter.db", "apps/app/lib/foo.ex"})
		got := FindProjectRoot(filepath.Join(root, "apps", "app"))
		if got != root {
			t.Errorf("got %q, want %q", got, root)
		}
	})

	t.Run("git fallback", func(t *testing.T) {
		root := mktree(t, []string{".git/", "apps/app/lib/foo.ex"})
		got := FindProjectRoot(filepath.Join(root, "apps", "app"))
		if got != root {
			t.Errorf("got %q, want %q", got, root)
		}
	})

	t.Run("mix.exs extra marker", func(t *testing.T) {
		root := mktree(t, []string{"apps/app/mix.exs", "apps/app/lib/foo.ex"})
		start := filepath.Join(root, "apps", "app", "lib")
		got := FindProjectRoot(start, "mix.exs")
		want := filepath.Join(root, "apps", "app")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("no marker returns input", func(t *testing.T) {
		root := mktree(t, []string{"lib/foo.ex"})
		start := filepath.Join(root, "lib")
		got := FindProjectRoot(start)
		if got != start {
			t.Errorf("got %q, want %q", got, start)
		}
	})

	t.Run("new layout preferred over legacy", func(t *testing.T) {
		// New layout at the repo root, legacy file inside a nested subdir.
		// Walking up from the subdir must return the repo root (matching
		// .dexter/dexter.db first), not the subdir — a reversed priority
		// would incorrectly match the closer .dexter.db.
		root := mktree(t, []string{".dexter/dexter.db", "apps/app/.dexter.db"})
		cwd := filepath.Join(root, "apps", "app")
		got := FindProjectRoot(cwd)
		if got != root {
			t.Errorf("got %q, want %q", got, root)
		}
	})
}

// makeFile creates a real file so IndexFile's os.Stat succeeds, and returns its path.
func makeFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("# generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// genRows builds enough definitions and references to cross the multi-row
// INSERT chunk boundaries (defChunkRows=100, refChunkRows=180) and leave a
// partial chunk behind, so both the chunked path and flushPending are exercised.
func genRows(path string, defCount, refCount int) ([]parser.Definition, []parser.Reference) {
	defs := make([]parser.Definition, 0, defCount)
	for i := 0; i < defCount; i++ {
		defs = append(defs, parser.Definition{
			Module: "MyApp.Gen", Function: "fn" + strconv.Itoa(i), Arity: i % 4,
			Kind: "def", Line: i + 1, FilePath: path,
		})
	}
	refs := make([]parser.Reference, 0, refCount)
	for i := 0; i < refCount; i++ {
		refs = append(refs, parser.Reference{
			Module: "SharedLib.Worker", Function: "call" + strconv.Itoa(i),
			Line: i + 1, FilePath: path, Kind: "call",
		})
	}
	return defs, refs
}

// TestBulkInsertMatchesRowAtATime pins the multi-row INSERT path in
// BeginBulkInsert to the row-at-a-time path in BeginBatch. The bulk path
// buffers rows and flushes them in chunks, so a boundary or flush bug would
// silently drop or duplicate rows.
func TestBulkInsertMatchesRowAtATime(t *testing.T) {
	// 250 defs = 2 full chunks of 100 + 50 pending; 425 refs = 2 full chunks of
	// 180 + 65 pending. Both remainders exercise flushPending.
	const defCount, refCount = 250, 425

	read := func(s *Store, path string) (int, []ReferenceResult) {
		t.Helper()
		var defs int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM definitions WHERE file_id = (SELECT id FROM files WHERE path = ?)", path).Scan(&defs); err != nil {
			t.Fatal(err)
		}
		refs, err := s.LookupReferences("SharedLib.Worker", "call7")
		if err != nil {
			t.Fatal(err)
		}
		return defs, refs
	}

	bulkStore, bulkDir := setupTestStore(t)
	bulkPath := makeFile(t, bulkDir, "gen.ex")
	bd, br := genRows(bulkPath, defCount, refCount)
	batch, err := bulkStore.BeginBulkInsert()
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.IndexFileWithMtimeAndRefs(bulkPath, 1, bd, br); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	rowStore, rowDir := setupTestStore(t)
	rowPath := makeFile(t, rowDir, "gen.ex")
	rd, rr := genRows(rowPath, defCount, refCount)
	rowBatch, err := rowStore.BeginBatch()
	if err != nil {
		t.Fatal(err)
	}
	if err := rowBatch.IndexFileWithMtimeAndRefs(rowPath, 1, rd, rr); err != nil {
		t.Fatal(err)
	}
	if err := rowBatch.Commit(); err != nil {
		t.Fatal(err)
	}

	bulkDefs, bulkRefs := read(bulkStore, bulkPath)
	rowDefs, rowRefs := read(rowStore, rowPath)

	if bulkDefs != defCount {
		t.Errorf("bulk definitions = %d, want %d", bulkDefs, defCount)
	}
	if bulkDefs != rowDefs {
		t.Errorf("definition count: bulk %d, row-at-a-time %d", bulkDefs, rowDefs)
	}
	if len(bulkRefs) != 1 {
		t.Errorf("bulk refs for call7 = %d, want 1", len(bulkRefs))
	}
	if len(bulkRefs) != len(rowRefs) {
		t.Errorf("ref count: bulk %d, row-at-a-time %d", len(bulkRefs), len(rowRefs))
	}

	var total int
	if err := bulkStore.db.QueryRow("SELECT COUNT(*) FROM refs").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != refCount {
		t.Errorf("bulk refs total = %d, want %d", total, refCount)
	}
}

// TestLookupByPrefixRange covers the range predicate that replaced `LIKE
// 'Prefix.%'`. The range must include the prefix itself and everything under
// it, and must exclude a module that merely starts with the same letters.
func TestLookupByPrefixRange(t *testing.T) {
	s, dir := setupTestStore(t)
	path := makeFile(t, dir, "mods.ex")

	defs := []parser.Definition{
		{Module: "MyApp.Accounts", Kind: "module", Line: 1, FilePath: path},
		{Module: "MyApp.Accounts.User", Kind: "module", Line: 2, FilePath: path},
		{Module: "MyApp.AccountsExtra", Kind: "module", Line: 3, FilePath: path},
		{Module: "MyApp.Billing", Kind: "module", Line: 4, FilePath: path},
	}
	refs := []parser.Reference{
		{Module: "MyApp.Accounts", Line: 10, FilePath: path, Kind: "alias"},
		{Module: "MyApp.Accounts.User", Line: 11, FilePath: path, Kind: "alias"},
		{Module: "MyApp.AccountsExtra", Line: 12, FilePath: path, Kind: "alias"},
		{Module: "MyApp.Billing", Line: 13, FilePath: path, Kind: "alias"},
	}
	batch, err := s.BeginBatch()
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.IndexFileWithMtimeAndRefs(path, 1, defs, refs); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	gotMods, err := s.LookupModulesByPrefix("MyApp.Accounts")
	if err != nil {
		t.Fatal(err)
	}
	var modNames []string
	for _, m := range gotMods {
		modNames = append(modNames, m.Module)
	}
	wantMods := "MyApp.Accounts,MyApp.Accounts.User"
	if strings.Join(modNames, ",") != wantMods {
		t.Errorf("LookupModulesByPrefix = %v, want %s", modNames, wantMods)
	}

	gotRefs, err := s.LookupReferencesByPrefix("MyApp.Accounts")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range gotRefs {
		seen[r.Module] = true
	}
	if !seen["MyApp.Accounts"] || !seen["MyApp.Accounts.User"] {
		t.Errorf("LookupReferencesByPrefix missing prefix members: %v", seen)
	}
	if seen["MyApp.AccountsExtra"] {
		t.Error("LookupReferencesByPrefix matched MyApp.AccountsExtra, which is not under the prefix")
	}
	if seen["MyApp.Billing"] {
		t.Error("LookupReferencesByPrefix matched an unrelated module")
	}
}

// TestReferencesUsesCoveringIndex pins the plan for the References hot path.
// idx_refs_module_function spans (module, function, file_id, line, kind) so the
// query is answered from the index alone. Before that, every hit cost a random
// read into the refs table — thousands of them for a widely-called function.
// If the index or the query drifts apart, this fails.
func TestReferencesUsesCoveringIndex(t *testing.T) {
	s, _ := setupTestStore(t)

	rows, err := s.db.Query(
		"EXPLAIN QUERY PLAN SELECT f.path, r.line, r.kind FROM refs r JOIN files f ON f.id = r.file_id WHERE r.module = ? AND r.function = ?",
		"MyApp.Accounts", "get_user")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	got := plan.String()

	if !strings.Contains(got, "COVERING INDEX idx_refs_module_function") {
		t.Errorf("references query no longer reads from a covering index:\n%s", got)
	}
	if strings.Contains(got, "SCAN refs") {
		t.Errorf("references query scans the refs table:\n%s", got)
	}
	if strings.Contains(got, "TEMP B-TREE") {
		t.Errorf("references query sorts in SQLite; ordering belongs in Go:\n%s", got)
	}
}

// TestReindexKeepsFileID guards the id that definitions and refs point at. The
// files row is upserted rather than replaced: INSERT OR REPLACE would delete the
// old row and allocate a new id, silently detaching every row for that file.
func TestReindexKeepsFileID(t *testing.T) {
	s, dir := setupTestStore(t)
	path := writeElixirFile(t, dir, "accounts.ex", `defmodule MyApp.Accounts do
  def get_user(id), do: SharedLib.Worker.run(id)
end
`)

	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFileWithRefs(path, defs, refs); err != nil {
		t.Fatal(err)
	}

	fileID := func() int64 {
		t.Helper()
		var id int64
		if err := s.db.QueryRow("SELECT id FROM files WHERE path = ?", path).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	orphans := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(
			"SELECT (SELECT COUNT(*) FROM definitions WHERE file_id NOT IN (SELECT id FROM files)) + (SELECT COUNT(*) FROM refs WHERE file_id NOT IN (SELECT id FROM files))",
		).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	first := fileID()

	// Reindex the same path, as a save would.
	if err := s.IndexFileWithRefs(path, defs, refs); err != nil {
		t.Fatal(err)
	}
	if second := fileID(); second != first {
		t.Errorf("file id changed across reindex: %d -> %d", first, second)
	}
	if n := orphans(); n != 0 {
		t.Errorf("reindex orphaned %d rows", n)
	}

	// The definitions must still resolve back to the path.
	results, err := s.LookupFunction("MyApp.Accounts", "get_user")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].FilePath != path {
		t.Errorf("lookup after reindex = %+v, want one result at %s", results, path)
	}

	// The same through the Batch path, which upserts through its own statement.
	batch, err := s.BeginBatch()
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.IndexFileWithMtimeAndRefs(path, 42, defs, refs); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}
	if third := fileID(); third != first {
		t.Errorf("file id changed across batch reindex: %d -> %d", first, third)
	}
	if n := orphans(); n != 0 {
		t.Errorf("batch reindex orphaned %d rows", n)
	}
}

// TestRemoveFileClearsRows checks that deleting a file takes its definitions and
// refs with it. refs carries no foreign key (the parent-key check was costing
// the cold index millions of lookups), so the delete must be explicit.
func TestRemoveFileClearsRows(t *testing.T) {
	s, dir := setupTestStore(t)
	path := writeElixirFile(t, dir, "worker.ex", `defmodule SharedLib.Worker do
  def run(id), do: MyApp.Accounts.get_user(id)
end
`)
	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFileWithRefs(path, defs, refs); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFile(path); err != nil {
		t.Fatal(err)
	}

	var remaining int
	if err := s.db.QueryRow(
		"SELECT (SELECT COUNT(*) FROM definitions) + (SELECT COUNT(*) FROM refs) + (SELECT COUNT(*) FROM files)",
	).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Errorf("removing the only file left %d rows behind", remaining)
	}
}

// Leaving WAL needs exclusive access, so journal_mode fails whenever another
// connection has the database open. It is applied first so that failure leaves
// the connection exactly as it was — the ordering used to be the other way
// round, and a locked database was left with fsync disabled for the rest of the
// process.
func TestSetBulkPragmas_LockedDatabaseChangesNothing(t *testing.T) {
	dir := t.TempDir()

	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s1.Close() }()

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()

	synchronous := func(s *Store) int {
		t.Helper()
		var v int
		if err := s.db.QueryRow("PRAGMA synchronous").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	before := synchronous(s1)

	// Hold a read transaction open on the second connection, the way an LSP
	// handler serving a query would.
	tx, err := s2.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM files").Scan(&n); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s1.SetBulkPragmas(); err == nil {
		t.Fatal("SetBulkPragmas should fail while another connection holds the database")
	}

	if got := synchronous(s1); got != before {
		t.Errorf("synchronous = %d after a failed SetBulkPragmas, want %d unchanged", got, before)
	}
}

func TestSetBulkPragmas_AppliesWhenExclusive(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	if err := s.SetBulkPragmas(); err != nil {
		t.Fatalf("SetBulkPragmas on an exclusive database: %v", err)
	}

	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "memory" {
		t.Errorf("journal_mode = %q, want memory", mode)
	}
}
