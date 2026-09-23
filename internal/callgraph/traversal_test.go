package callgraph

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

type fakeGraph struct {
	symbols   map[parser.FunctionID]store.CallSymbol
	callees   map[int64][]store.CallRelation
	callers   map[int64][]store.CallRelation
	frontiers [][]int64
}

func (f *fakeGraph) ResolveCallSymbols(functions []parser.FunctionID) ([]store.CallSymbol, error) {
	var result []store.CallSymbol
	for _, function := range functions {
		if symbol, ok := f.symbols[function]; ok {
			result = append(result, symbol)
		}
	}
	return result, nil
}

func (f *fakeGraph) LookupCalleeFrontier(ids []int64) ([]store.CallRelation, error) {
	f.frontiers = append(f.frontiers, append([]int64(nil), ids...))
	return collectRelations(ids, f.callees), nil
}

func (f *fakeGraph) LookupCallerFrontier(ids []int64) ([]store.CallRelation, error) {
	f.frontiers = append(f.frontiers, append([]int64(nil), ids...))
	return collectRelations(ids, f.callers), nil
}

func collectRelations(ids []int64, adjacent map[int64][]store.CallRelation) []store.CallRelation {
	var result []store.CallRelation
	for _, id := range ids {
		result = append(result, adjacent[id]...)
	}
	return result
}

func TestTraverseBatchesFrontiersAndChoosesDeterministicShortestPath(t *testing.T) {
	a := testSymbol(1, "A", "start", 0)
	b := testSymbol(2, "B", "left", 0)
	c := testSymbol(3, "C", "right", 0)
	d := testSymbol(4, "D", "root", 0)
	graph := &fakeGraph{
		symbols: map[parser.FunctionID]store.CallSymbol{a.Function: a},
		callees: map[int64][]store.CallRelation{
			a.ID: {{FrontierID: a.ID, Adjacent: c, Kind: "call"}, {FrontierID: a.ID, Adjacent: b, Kind: "call"}},
			b.ID: {{FrontierID: b.ID, Adjacent: d, Kind: "call"}},
			c.ID: {{FrontierID: c.ID, Adjacent: d, Kind: "call"}},
			d.ID: {{FrontierID: d.ID, Adjacent: a, Kind: "call"}},
		},
	}

	result, err := Traverse(graph, []parser.FunctionID{a.Function}, Options{Direction: Forward, MaxDepth: -1, MaxNodes: -1, Targets: []parser.FunctionID{d.Function}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(graph.frontiers, [][]int64{{1}, {2, 3}, {4}}) {
		t.Fatalf("frontiers = %v, want one batched query per depth", graph.frontiers)
	}
	want := []parser.FunctionID{a.Function, b.Function, d.Function}
	if len(result.Paths) != 1 || !reflect.DeepEqual(result.Paths[0].Functions, want) {
		t.Errorf("path to D = %v, want %v", result.Paths, want)
	}
	if result.Stats.Visited != 4 || result.Stats.DepthReached != 2 {
		t.Errorf("stats = %+v", result.Stats)
	}
}

func TestTraverseKeepsUnknownAritySeparateAndAppliesBudgets(t *testing.T) {
	exact := testSymbol(1, "A", "run", 1)
	unknown := testSymbol(2, "A", "run", parser.UnknownArity)
	caller := testSymbol(3, "B", "call", 0)
	graph := &fakeGraph{
		symbols: map[parser.FunctionID]store.CallSymbol{exact.Function: exact, unknown.Function: unknown},
		callers: map[int64][]store.CallRelation{
			unknown.ID: {{FrontierID: unknown.ID, Adjacent: caller, Kind: "call"}},
		},
	}

	result, err := Traverse(graph, []parser.FunctionID{exact.Function}, Options{Direction: Reverse, MaxDepth: 1, MaxNodes: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Paths) != 0 {
		t.Fatalf("exact traversal reached unknown-arity callers: %+v", result.Paths)
	}

	result, err = Traverse(graph, []parser.FunctionID{unknown.Function}, Options{Direction: Reverse, MaxDepth: -1, MaxNodes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !result.NodeTruncated || result.Stats.Visited != 1 {
		t.Fatalf("node budget result = %+v", result)
	}
}

func TestTraverseReportsUnresolvedSeeds(t *testing.T) {
	known := testSymbol(1, "A", "run", 0)
	missing := parser.FunctionID{Module: "Missing", Function: "run", Arity: 0}
	graph := &fakeGraph{symbols: map[parser.FunctionID]store.CallSymbol{known.Function: known}}

	result, err := Traverse(graph, []parser.FunctionID{missing, known.Function}, Options{Direction: Reverse, MaxDepth: -1, MaxNodes: -1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.UnresolvedSeeds, []parser.FunctionID{missing}) {
		t.Fatalf("unresolved seeds = %v, want %v", result.UnresolvedSeeds, missing)
	}
}

func TestTraverseManySharesFrontierQueriesAndKeepsSeedPaths(t *testing.T) {
	a := testSymbol(1, "A", "start", 0)
	b := testSymbol(2, "B", "middle", 0)
	c := testSymbol(3, "C", "middle", 0)
	d := testSymbol(4, "D", "target", 0)
	e := testSymbol(5, "E", "start", 0)
	graph := &fakeGraph{
		symbols: map[parser.FunctionID]store.CallSymbol{a.Function: a, e.Function: e},
		callees: map[int64][]store.CallRelation{
			a.ID: {{FrontierID: a.ID, Adjacent: b, Kind: "call"}},
			e.ID: {{FrontierID: e.ID, Adjacent: c, Kind: "call"}},
			b.ID: {{FrontierID: b.ID, Adjacent: d, Kind: "call"}},
			c.ID: {{FrontierID: c.ID, Adjacent: d, Kind: "call"}},
		},
	}
	results, stats, err := TraverseMany(graph, [][]parser.FunctionID{{a.Function}, {e.Function}}, Options{
		Direction: Forward,
		MaxDepth:  -1,
		MaxNodes:  -1,
		Targets:   []parser.FunctionID{d.Function},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FrontierQueries != 3 || len(graph.frontiers) != 3 {
		t.Fatalf("frontier queries = %d (%v), want 3 shared depth queries", stats.FrontierQueries, graph.frontiers)
	}
	if len(results) != 2 || len(results[0].Paths) != 1 || len(results[1].Paths) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Paths[0].Functions[0] != a.Function || results[1].Paths[0].Functions[0] != e.Function {
		t.Fatalf("seed provenance lost: %+v", results)
	}
}

func testSymbol(id int64, module, function string, arity int) store.CallSymbol {
	return store.CallSymbol{ID: id, Function: parser.FunctionID{Module: module, Function: function, Arity: arity}}
}

func BenchmarkTraverseLayeredGraph(b *testing.B) {
	const (
		width = 100
		depth = 100
	)
	seed := testSymbol(1, "Layer0", "run", 0)
	graph := &fakeGraph{
		symbols: map[parser.FunctionID]store.CallSymbol{seed.Function: seed},
		callees: make(map[int64][]store.CallRelation, width*depth),
	}
	previous := []store.CallSymbol{seed}
	var target store.CallSymbol
	for layer := 1; layer <= depth; layer++ {
		current := make([]store.CallSymbol, width)
		for i := range current {
			id := int64((layer-1)*width + i + 2)
			current[i] = testSymbol(id, "Layer"+strconv.Itoa(layer), "run"+strconv.Itoa(i), 0)
		}
		for i, parent := range previous {
			first := current[i%width]
			second := current[(i+1)%width]
			graph.callees[parent.ID] = []store.CallRelation{
				{FrontierID: parent.ID, Adjacent: first, Kind: "call"},
				{FrontierID: parent.ID, Adjacent: second, Kind: "call"},
			}
		}
		previous = current
		target = current[width-1]
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		graph.frontiers = graph.frontiers[:0]
		if _, err := Traverse(graph, []parser.FunctionID{seed.Function}, Options{
			Direction: Forward,
			MaxDepth:  -1,
			MaxNodes:  -1,
			Targets:   []parser.FunctionID{target.Function},
		}); err != nil {
			b.Fatal(err)
		}
	}
}
