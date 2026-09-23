package beam

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
)

func TestReadCompiledEvidence(t *testing.T) {
	beamPath := filepath.Join(t.TempDir(), "Elixir.MyApp.Worker.beam")
	writeTestBEAMOpts(t, beamPath, testBEAMOptions{
		atomNames: defaultTestAtoms,
		exports:   defaultTestExports,
		dbgi:      buildDebugTerm(),
	})
	evidence, err := ReadCompiledEvidence(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Module != "MyApp.Worker" || evidence.Source != "lib/my_app/worker.ex" {
		t.Fatalf("identity = %s %s", evidence.Module, evidence.Source)
	}
	if len(evidence.Functions) != 4 {
		t.Fatalf("functions = %+v", evidence.Functions)
	}
	want := map[parser.CallEdge]bool{
		{
			Caller: parser.FunctionID{Module: "MyApp.Worker", Function: "run", Arity: 1},
			Callee: parser.FunctionID{Module: "SharedLib.Service", Function: "fetch", Arity: 1},
			Kind:   "call",
		}: true,
		{
			Caller: parser.FunctionID{Module: "MyApp.Worker", Function: "run", Arity: 1},
			Callee: parser.FunctionID{Module: "MyApp.Worker", Function: "helper", Arity: 1},
			Kind:   "local",
		}: true,
	}
	for _, edge := range evidence.Edges {
		delete(want, edge)
	}
	if len(want) != 0 {
		t.Fatalf("missing edges: %+v; got %+v", want, evidence.Edges)
	}
	if len(evidence.Unresolved) != 1 || evidence.Unresolved[0].Caller.Function != "dynamic" {
		t.Fatalf("unresolved = %+v", evidence.Unresolved)
	}
}

func TestCompiledWalkExpandsBoundedTargets(t *testing.T) {
	caller := parser.FunctionID{Module: "MyApp.Dispatcher", Function: "run", Arity: 1}
	variable := etfTuple{etfAtom("module"), etfListValue{}, etfAtom("Elixir")}
	state := compiledWalkState{
		caller: caller, locals: map[parser.FunctionID]struct{}{}, edges: make(map[parser.CallEdge]struct{}),
		unresolved: make(map[CompiledUnresolved]struct{}), bindings: make(map[string][]compiledValue),
	}
	key, ok := variableKey(variable)
	if !ok {
		t.Fatal("test variable has no key")
	}
	state.bindings[key] = []compiledValue{
		{kind: "atom", module: "MyApp.First", function: "Elixir.MyApp.First"},
		{kind: "atom", module: "MyApp.Second", function: "Elixir.MyApp.Second"},
	}
	call := etfTuple{
		etfTuple{etfAtom("."), etfListValue{}, etfListValue{Values: []interface{}{variable, etfAtom("perform")}}},
		etfListValue{},
		etfListValue{Values: []interface{}{1}},
	}
	state.walk(call)
	for _, module := range []string{"MyApp.First", "MyApp.Second"} {
		edge := parser.CallEdge{
			Caller: caller, Callee: parser.FunctionID{Module: module, Function: "perform", Arity: 1}, Kind: "inferred_call",
		}
		if _, ok := state.edges[edge]; !ok {
			t.Errorf("missing bounded edge %+v", edge)
		}
	}
	if len(state.unresolved) != 0 {
		t.Fatalf("bounded targets remained unresolved: %+v", state.unresolved)
	}
}

func TestCompiledWalkTraversesAnonymousCallTarget(t *testing.T) {
	caller := parser.FunctionID{Module: "MyApp.Dispatcher", Function: "run", Arity: 1}
	state := compiledWalkState{
		caller: caller, locals: map[parser.FunctionID]struct{}{}, edges: make(map[parser.CallEdge]struct{}),
		unresolved: make(map[CompiledUnresolved]struct{}), bindings: make(map[string][]compiledValue),
	}
	remote := etfTuple{
		etfTuple{etfAtom("."), etfListValue{}, etfListValue{Values: []interface{}{
			etfAtom("Elixir.SharedLib.Worker"), etfAtom("perform"),
		}}},
		etfListValue{}, etfListValue{},
	}
	arrow := etfTuple{etfAtom("->"), etfListValue{}, etfListValue{Values: []interface{}{etfListValue{}, remote}}}
	target := etfTuple{etfAtom("fn"), etfListValue{}, etfListValue{Values: []interface{}{arrow}}}
	call := etfTuple{
		etfTuple{etfAtom("."), etfListValue{}, etfListValue{Values: []interface{}{target}}},
		etfListValue{}, etfListValue{},
	}
	state.walk(call)
	want := parser.CallEdge{
		Caller: caller,
		Callee: parser.FunctionID{Module: "SharedLib.Worker", Function: "perform", Arity: 0},
		Kind:   "call",
	}
	if _, ok := state.edges[want]; !ok {
		t.Fatalf("missing target edge %+v; got %+v", want, state.edges)
	}
}

func TestCompiledResources(t *testing.T) {
	attributes := etfListValue{Values: []interface{}{
		etfTuple{etfAtom("external_resource"), "/project/priv/schema.json"},
		etfTuple{etfAtom("external_resource"), etfListValue{Values: []interface{}{"priv/template.eex"}}},
	}}
	got := compiledResources(attributes)
	want := []string{"/project/priv/schema.json", "priv/template.eex"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resources = %v, want %v", got, want)
	}
}

func TestCompiledBehavioursAndCallbacks(t *testing.T) {
	attributes := etfListValue{Values: []interface{}{
		etfTuple{etfAtom("behaviour"), etfAtom("Elixir.SharedLib.Worker")},
	}}
	if got := compiledBehaviours(attributes); !reflect.DeepEqual(got, []string{"SharedLib.Worker"}) {
		t.Fatalf("behaviours = %v", got)
	}
	forms := etfListValue{Values: []interface{}{
		etfTuple{
			etfAtom("attribute"), 1, etfAtom("callback"),
			etfTuple{etfTuple{etfAtom("perform"), 1}, etfListValue{}},
		},
	}}
	want := []parser.FunctionID{{Module: "SharedLib.Worker", Function: "perform", Arity: 1}}
	if got := compiledCallbacks(forms, "SharedLib.Worker"); !reflect.DeepEqual(got, want) {
		t.Fatalf("callbacks = %v, want %v", got, want)
	}
}

func buildDebugTerm() []byte {
	w := &etfTestWriter{}
	w.version()
	w.smallTuple(3)
	w.atom("debug_info_v1")
	w.atom("elixir_erl")
	w.smallTuple(3)
	w.atom("elixir_v1")
	w.mapHeader(3)
	w.atom("module")
	w.atom("Elixir.MyApp.Worker")
	w.atom("relative_file")
	w.binary("lib/my_app/worker.ex")
	w.atom("definitions")
	w.listHeader(3)
	writeDebugDefinition(w, "helper", 1, func(w *etfTestWriter) { writeVariable(w, "value") })
	writeDebugDefinition(w, "run", 1, func(w *etfTestWriter) {
		w.smallTuple(3)
		w.atom("__block__")
		w.nil()
		w.listHeader(2)
		writeRemoteCall(w, "Elixir.SharedLib.Service", "fetch")
		writeLocalCall(w, "helper")
		w.nil()
	})
	writeDebugDefinition(w, "dynamic", 1, func(w *etfTestWriter) {
		w.smallTuple(3)
		w.smallTuple(3)
		w.atom(".")
		w.nil()
		w.listHeader(2)
		writeVariable(w, "module")
		w.atom("fetch")
		w.nil()
		w.nil()
		w.listHeader(1)
		writeVariable(w, "value")
		w.nil()
	})
	w.nil()
	w.nil()
	return w.buf
}

func writeDebugDefinition(w *etfTestWriter, name string, arity int, body func(*etfTestWriter)) {
	w.smallTuple(4)
	w.smallTuple(2)
	w.atom(name)
	w.smallInt(arity)
	w.atom("def")
	w.nil()
	w.listHeader(1)
	w.smallTuple(4)
	w.nil()
	w.listHeader(arity)
	for i := 0; i < arity; i++ {
		writeVariable(w, "value")
	}
	w.nil()
	w.nil()
	body(w)
	w.nil()
}

func writeVariable(w *etfTestWriter, name string) {
	w.smallTuple(3)
	w.atom(name)
	w.nil()
	w.atom("Elixir")
}

func writeRemoteCall(w *etfTestWriter, module, function string) {
	w.smallTuple(3)
	w.smallTuple(3)
	w.atom(".")
	w.nil()
	w.listHeader(2)
	w.atom(module)
	w.atom(function)
	w.nil()
	w.nil()
	w.listHeader(1)
	writeVariable(w, "value")
	w.nil()
}

func writeLocalCall(w *etfTestWriter, function string) {
	w.smallTuple(3)
	w.atom(function)
	w.nil()
	w.listHeader(1)
	writeVariable(w, "value")
	w.nil()
}
