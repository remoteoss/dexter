package parser

import "testing"

func TestFunctionFingerprintIsDeterministic(t *testing.T) {
	first := fingerprintFor(t, `defmodule MyApp.Worker do
  def run(value) do
    value + 1 # explanation
  end
end`, "run", 1)
	second := fingerprintFor(t, `defmodule MyApp.Worker do
  def run(value) do
    value + 1 # explanation
  end
end`, "run", 1)
	if first != second {
		t.Fatalf("identical source changed fingerprint: %x != %x", first, second)
	}
}

func TestFunctionFingerprintPreservesSemanticSyntax(t *testing.T) {
	base := fingerprintFor(t, `defmodule MyApp.Worker do
  def run(value) when is_integer(value), do: value + 1
end`, "run", 1)
	for name, source := range map[string]string{
		"literal": `defmodule MyApp.Worker do
  def run(value) when is_integer(value), do: value + 2
end`,
		"operator": `defmodule MyApp.Worker do
  def run(value) when is_integer(value), do: value - 1
end`,
		"guard": `defmodule MyApp.Worker do
  def run(value) when is_float(value), do: value + 1
end`,
	} {
		t.Run(name, func(t *testing.T) {
			if changed := fingerprintFor(t, source, "run", 1); changed == base {
				t.Fatalf("semantic %s change did not change fingerprint %x", name, base)
			}
		})
	}
}

func TestFunctionFingerprintCoversDefaultsAndClauseOrder(t *testing.T) {
	definitions, _, err := ParseText("worker.ex", `defmodule MyApp.Worker do
  def run(value, option \\ :default)
  def run(:first, option), do: {:first, option}
  def run(:second, option), do: {:second, option}
end`)
	if err != nil {
		t.Fatal(err)
	}
	var arityOne, arityTwo [32]byte
	for _, definition := range definitions {
		if definition.Function != "run" {
			continue
		}
		switch definition.Arity {
		case 1:
			arityOne = definition.Fingerprint
		case 2:
			arityTwo = definition.Fingerprint
		}
	}
	if arityOne == ([32]byte{}) || arityTwo == ([32]byte{}) {
		t.Fatalf("default arity fingerprints are empty: /1=%x /2=%x", arityOne, arityTwo)
	}

	firstOrder := fingerprintFor(t, `defmodule MyApp.Worker do
  def run(:first), do: 1
  def run(:second), do: 2
end`, "run", 1)
	secondOrder := fingerprintFor(t, `defmodule MyApp.Worker do
  def run(:second), do: 2
  def run(:first), do: 1
end`, "run", 1)
	if firstOrder == secondOrder {
		t.Fatalf("clause reordering did not change fingerprint %x", firstOrder)
	}
}

func TestGeneratedDefinitionFingerprint(t *testing.T) {
	first := fingerprintFor(t, `defmodule MyApp.Record do
  defstruct value: 1
end`, "__struct__", 0)
	second := fingerprintFor(t, `defmodule MyApp.Record do
  defstruct value: 2
end`, "__struct__", 0)
	if first == second {
		t.Fatalf("defstruct field change did not change fingerprint %x", first)
	}
}

func TestSourceTestRootOwnsCallsInExUnitBodies(t *testing.T) {
	definitions, _, calls, err := ParseTextWithCalls("test/worker_test.exs", `defmodule MyApp.WorkerTest do
  test "runs the worker" do
    SharedLib.Worker.run(1)
  end
end`)
	if err != nil {
		t.Fatal(err)
	}
	root := FunctionID{Module: "MyApp.WorkerTest", Function: "__dexter_test_root__", Arity: 0}
	foundDefinition := false
	for _, definition := range definitions {
		if definition.Module == root.Module && definition.Function == root.Function && definition.Arity == 0 {
			foundDefinition = definition.Fingerprint != ([32]byte{})
		}
	}
	if !foundDefinition {
		t.Fatal("source test root definition is missing or has no fingerprint")
	}
	wantCallee := FunctionID{Module: "SharedLib.Worker", Function: "run", Arity: 1}
	for _, call := range calls {
		if call.Caller == root && call.Callee == wantCallee {
			return
		}
	}
	t.Fatalf("test root call edge not found in %+v", calls)
}

func fingerprintFor(t *testing.T, source, function string, arity int) [32]byte {
	t.Helper()
	definitions, _, err := ParseText("worker.ex", source)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range definitions {
		if definition.Function == function && definition.Arity == arity {
			if definition.Fingerprint == ([32]byte{}) {
				t.Fatalf("%s/%d has an empty fingerprint", function, arity)
			}
			return definition.Fingerprint
		}
	}
	t.Fatalf("definition %s/%d not found", function, arity)
	return [32]byte{}
}
