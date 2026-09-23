package impact

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

func TestAnalyzeSelectsTestWithShortestExplanation(t *testing.T) {
	base, baseRoot := buildIndex(t, map[string]string{
		"lib/service.ex": `defmodule MyApp.Service do
  def run(value), do: value + 1
end`,
		"test/service_test.exs": `defmodule MyApp.ServiceTest do
  def helper(value), do: MyApp.Service.run(value)
end`,
	})
	head, headRoot := buildIndex(t, map[string]string{
		"lib/service.ex": `defmodule MyApp.Service do
  def run(value), do: value + 2
end`,
		"test/service_test.exs": `defmodule MyApp.ServiceTest do
  def helper(value), do: MyApp.Service.run(value)
end`,
	})

	result, err := Analyze(base, baseRoot, head, headRoot, unlimitedOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedFunctions) != 1 || result.ChangedFunctions[0].Change != "modified" {
		t.Fatalf("changed functions = %+v", result.ChangedFunctions)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].File != "test/service_test.exs" {
		t.Fatalf("candidates = %+v", result.Candidates)
	}
	for _, explanation := range result.Candidates[0].Explanations {
		if len(explanation.Path) == 3 &&
			explanation.Path[1] == (parser.FunctionID{Module: "MyApp.ServiceTest", Function: "helper", Arity: 1}) &&
			explanation.Path[2] == (parser.FunctionID{Module: "MyApp.ServiceTest", Function: "__dexter_test_root__", Arity: 0}) {
			return
		}
	}
	t.Fatalf("no helper explanation in %+v", result.Candidates[0].Explanations)
}

func TestAnalyzeWidensSelectionForUnresolvedChangedFunction(t *testing.T) {
	base, baseRoot := buildIndex(t, map[string]string{
		"lib/service.ex": `defmodule MyApp.Service do
  def isolated, do: 1
end`,
		"test/first_test.exs": "defmodule MyApp.FirstTest do\nend",
	})
	head, headRoot := buildIndex(t, map[string]string{
		"lib/service.ex": `defmodule MyApp.Service do
  def isolated, do: 2
end`,
		"test/first_test.exs": "defmodule MyApp.FirstTest do\nend",
	})

	result, err := Analyze(base, baseRoot, head, headRoot, unlimitedOptions())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].File != "test/first_test.exs" {
		t.Fatalf("unresolved change did not select all tests: %+v", result.Candidates)
	}
	if len(result.Unresolved) != 2 {
		t.Fatalf("unresolved evidence = %v, want base and head", result.Unresolved)
	}
}

func TestAnalyzeSeparatesIndexRootFromSelectionScope(t *testing.T) {
	files := map[string]string{
		"apps/sample_app/test/app_test.exs": "defmodule MyApp.AppTest do\n  def helper, do: :ok\nend",
		"libs/shared/test/shared_test.exs":  "defmodule SharedLib.SharedTest do\n  def helper, do: :ok\nend",
	}
	base, baseRoot := buildIndex(t, files)
	head, headRoot := buildIndex(t, files)

	result, err := Analyze(
		base, filepath.Join(baseRoot, "apps/sample_app"),
		head, filepath.Join(headRoot, "apps/sample_app"),
		unlimitedOptions(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage.TestFiles != 1 || result.Coverage.RootedTestFiles != 1 {
		t.Fatalf("scoped coverage = %+v, want only apps/sample_app", result.Coverage)
	}
}

func TestAnalyzeIncludesMonorepoDependencyChangesForScopedTests(t *testing.T) {
	base, baseRoot := buildIndex(t, map[string]string{
		"libs/shared/lib/worker.ex": `defmodule SharedLib.Worker do
  def run(value), do: value + 1
end`,
		"apps/sample_app/test/worker_test.exs": `defmodule MyApp.WorkerTest do
  def helper(value), do: SharedLib.Worker.run(value)
end`,
	})
	head, headRoot := buildIndex(t, map[string]string{
		"libs/shared/lib/worker.ex": `defmodule SharedLib.Worker do
  def run(value), do: value + 2
end`,
		"apps/sample_app/test/worker_test.exs": `defmodule MyApp.WorkerTest do
  def helper(value), do: SharedLib.Worker.run(value)
end`,
	})

	result, err := Analyze(
		base, filepath.Join(baseRoot, "apps/sample_app"),
		head, filepath.Join(headRoot, "apps/sample_app"),
		unlimitedOptions(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].File != "test/worker_test.exs" {
		t.Fatalf("dependency candidates = %+v", result.Candidates)
	}
}

func TestAnalyzeSeedsFunctionsForModuleLevelFileChanges(t *testing.T) {
	base, baseRoot := buildIndex(t, map[string]string{
		"lib/service.ex": `defmodule MyApp.Service do
  @mode :first
  def run(value), do: value
end`,
		"test/service_test.exs": `defmodule MyApp.ServiceTest do
  test "runs", do: MyApp.Service.run(1)
end`,
	})
	head, headRoot := buildIndex(t, map[string]string{
		"lib/service.ex": `defmodule MyApp.Service do
  @mode :second
  def run(value), do: value
end`,
		"test/service_test.exs": `defmodule MyApp.ServiceTest do
  test "runs", do: MyApp.Service.run(1)
end`,
	})

	options := unlimitedOptions()
	options.ChangedFiles = []string{
		filepath.Join(baseRoot, "lib/service.ex"),
		filepath.Join(headRoot, "lib/service.ex"),
	}
	result, err := Analyze(base, baseRoot, head, headRoot, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedFunctions) != 1 || result.ChangedFunctions[0].Function.Function != "run" {
		t.Fatalf("module-level change did not seed file functions: %+v", result.ChangedFunctions)
	}
	if len(result.Candidates) != 1 || result.Candidates[0].File != "test/service_test.exs" {
		t.Fatalf("module-level candidates = %+v", result.Candidates)
	}
}

func unlimitedOptions() Options {
	return Options{MaxDepth: -1, MaxNodes: -1}
}

func buildIndex(t *testing.T, files map[string]string) (*store.Store, string) {
	t.Helper()
	root := t.TempDir()
	index, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	for relative, source := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		definitions, references, calls, err := parser.ParseTextWithCalls(path, source)
		if err != nil {
			t.Fatal(err)
		}
		if err := index.IndexFileWithRefsAndCalls(path, definitions, references, calls); err != nil {
			t.Fatal(err)
		}
	}
	return index, root
}
