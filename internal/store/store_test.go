package store

import (
	"os"
	"path/filepath"
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
