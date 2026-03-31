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
