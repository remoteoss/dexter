package store

import (
	"os"
	"path/filepath"
	"testing"

	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
)

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

	defs, err := parser.ParseFile(path)
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

	defs, err := parser.ParseFile(path)
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

	defs, err := parser.ParseFile(path)
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

func TestReindexUpdatesDefinitions(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	path := writeElixirFile(t, dir, "lib/foo.ex", `defmodule MyApp.Foo do
  def bar do
    :ok
  end
end
`)

	defs, err := parser.ParseFile(path)
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

	defs, err = parser.ParseFile(path)
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

	defs, err := parser.ParseFile(path)
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

	defs, err := parser.ParseFile(path)
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

	defs, err := parser.ParseFile(path)
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
		implDefs, err := parser.ParseFile(implPath)
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

	defs, err := parser.ParseFile(path)
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

		multiDefs, err := parser.ParseFile(multiPath)
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

	defs, err := parser.ParseFile(path)
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
