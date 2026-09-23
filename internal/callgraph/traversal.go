// Package callgraph traverses the persistent source call graph.
package callgraph

import (
	"errors"
	"sort"
	"time"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

// Direction selects which side of each call edge traversal follows.
type Direction string

const (
	Forward Direction = "forward"
	Reverse Direction = "reverse"
)

// Options bounds one traversal. A negative budget is unlimited; zero permits
// the resolved seeds but no edge expansion or additional nodes.
type Options struct {
	Direction Direction           `json:"direction"`
	MaxDepth  int                 `json:"max_depth,omitempty"`
	MaxNodes  int                 `json:"max_nodes,omitempty"`
	Targets   []parser.FunctionID `json:"targets,omitempty"`
}

// Path is one deterministic shortest path from a seed to a reached function.
// Kinds[i] describes the relationship between Functions[i] and Functions[i+1].
type Path struct {
	Functions []parser.FunctionID `json:"functions"`
	Kinds     []string            `json:"kinds"`
}

// Stats summarizes work done by one traversal.
type Stats struct {
	Seeds           int           `json:"seeds"`
	Visited         int           `json:"visited"`
	RelationsRead   int           `json:"relations_read"`
	FrontierQueries int           `json:"frontier_queries"`
	DepthReached    int           `json:"depth_reached"`
	ResolveTime     time.Duration `json:"-"`
	QueryTime       time.Duration `json:"-"`
	SortTime        time.Duration `json:"-"`
	ExpandTime      time.Duration `json:"-"`
	MaterializeTime time.Duration `json:"-"`
}

// Result contains stable, JSON-ready traversal output.
type Result struct {
	Paths           []Path              `json:"paths"`
	UnresolvedSeeds []parser.FunctionID `json:"unresolved_seeds,omitempty"`
	Stats           Stats               `json:"stats"`
	DepthTruncated  bool                `json:"depth_truncated,omitempty"`
	NodeTruncated   bool                `json:"node_truncated,omitempty"`
}

// BatchStats measures shared work across a multi-seed traversal batch.
type BatchStats struct {
	FrontierQueries int
	RelationsRead   int
	ResolveTime     time.Duration
	QueryTime       time.Duration
	SortTime        time.Duration
	ExpandTime      time.Duration
	MaterializeTime time.Duration
}

type source interface {
	ResolveCallSymbols([]parser.FunctionID) ([]store.CallSymbol, error)
	LookupCalleeFrontier([]int64) ([]store.CallRelation, error)
	LookupCallerFrontier([]int64) ([]store.CallRelation, error)
}

type reachedNode struct {
	symbol store.CallSymbol
	parent int
	kind   string
	depth  int
}

type parentMetadata struct {
	index int
	rank  int
}

type traversalState struct {
	result   Result
	visited  map[int64]struct{}
	nodes    []reachedNode
	frontier []int
}

// TraverseMany traverses independent seed groups while sharing each depth's
// SQLite frontier query. Result order matches seedGroups.
func TraverseMany(graph source, seedGroups [][]parser.FunctionID, options Options) ([]Result, BatchStats, error) {
	if options.Direction != Forward && options.Direction != Reverse {
		return nil, BatchStats{}, errors.New("call graph direction must be forward or reverse")
	}
	if options.MaxDepth < -1 || options.MaxNodes < -1 {
		return nil, BatchStats{}, errors.New("call graph budgets must be -1 or greater")
	}

	stats := BatchStats{}
	resolveStarted := time.Now()
	requested := make([][]parser.FunctionID, len(seedGroups))
	var allSeeds []parser.FunctionID
	for i, seeds := range seedGroups {
		requested[i] = uniqueFunctions(seeds)
		allSeeds = append(allSeeds, requested[i]...)
	}
	symbols, err := graph.ResolveCallSymbols(uniqueFunctions(allSeeds))
	stats.ResolveTime = time.Since(resolveStarted)
	if err != nil {
		return nil, stats, err
	}
	symbolByFunction := make(map[parser.FunctionID]store.CallSymbol, len(symbols))
	for _, symbol := range symbols {
		symbolByFunction[symbol.Function] = symbol
	}
	targets := make(map[parser.FunctionID]struct{}, len(options.Targets))
	for _, target := range options.Targets {
		targets[target] = struct{}{}
	}

	states := make([]traversalState, len(seedGroups))
	for stateIndex := range states {
		resolved := make([]store.CallSymbol, 0, len(requested[stateIndex]))
		for _, seed := range requested[stateIndex] {
			if symbol, ok := symbolByFunction[seed]; ok {
				resolved = append(resolved, symbol)
			} else {
				states[stateIndex].result.UnresolvedSeeds = append(states[stateIndex].result.UnresolvedSeeds, seed)
			}
		}
		if options.MaxNodes >= 0 && len(resolved) > options.MaxNodes {
			return nil, stats, errors.New("call graph node budget is smaller than a resolved seed set")
		}
		sort.Slice(resolved, func(i, j int) bool { return functionLess(resolved[i].Function, resolved[j].Function) })
		state := &states[stateIndex]
		state.result.Stats.Seeds = len(resolved)
		state.visited = make(map[int64]struct{}, len(resolved))
		state.nodes = make([]reachedNode, len(resolved))
		state.frontier = make([]int, len(resolved))
		for i, symbol := range resolved {
			state.visited[symbol.ID] = struct{}{}
			state.nodes[i] = reachedNode{symbol: symbol, parent: -1}
			state.frontier[i] = i
		}
	}

	for depth := 0; ; depth++ {
		frontierSet := make(map[int64]struct{})
		for i := range states {
			state := &states[i]
			if len(state.frontier) == 0 {
				continue
			}
			if options.MaxDepth >= 0 && depth >= options.MaxDepth {
				state.result.DepthTruncated = true
				state.frontier = nil
				continue
			}
			for _, nodeIndex := range state.frontier {
				frontierSet[state.nodes[nodeIndex].symbol.ID] = struct{}{}
			}
		}
		if len(frontierSet) == 0 {
			break
		}
		ids := make([]int64, 0, len(frontierSet))
		for id := range frontierSet {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		queryStarted := time.Now()
		var relations []store.CallRelation
		if options.Direction == Forward {
			relations, err = graph.LookupCalleeFrontier(ids)
		} else {
			relations, err = graph.LookupCallerFrontier(ids)
		}
		stats.QueryTime += time.Since(queryStarted)
		stats.FrontierQueries++
		stats.RelationsRead += len(relations)
		if err != nil {
			return nil, stats, err
		}
		byFrontier := make(map[int64][]store.CallRelation, len(ids))
		for _, relation := range relations {
			byFrontier[relation.FrontierID] = append(byFrontier[relation.FrontierID], relation)
		}

		for stateIndex := range states {
			state := &states[stateIndex]
			if len(state.frontier) == 0 {
				continue
			}
			parents := make(map[int64]parentMetadata, len(state.frontier))
			stateRelations := make([]store.CallRelation, 0)
			for rank, nodeIndex := range state.frontier {
				id := state.nodes[nodeIndex].symbol.ID
				parents[id] = parentMetadata{index: nodeIndex, rank: rank}
				stateRelations = append(stateRelations, byFrontier[id]...)
			}
			state.result.Stats.RelationsRead += len(stateRelations)
			sortStarted := time.Now()
			sortRelationsByParentRank(stateRelations, parents)
			stats.SortTime += time.Since(sortStarted)
			expandStarted := time.Now()
			next := make([]int, 0, len(stateRelations))
			for _, relation := range stateRelations {
				if _, ok := state.visited[relation.Adjacent.ID]; ok {
					continue
				}
				if options.MaxNodes >= 0 && len(state.visited) >= options.MaxNodes {
					state.result.NodeTruncated = true
					break
				}
				state.visited[relation.Adjacent.ID] = struct{}{}
				state.nodes = append(state.nodes, reachedNode{
					symbol: relation.Adjacent,
					parent: parents[relation.FrontierID].index,
					kind:   relation.Kind,
					depth:  depth + 1,
				})
				next = append(next, len(state.nodes)-1)
				state.result.Stats.DepthReached = depth + 1
			}
			state.frontier = next
			stats.ExpandTime += time.Since(expandStarted)
		}
	}

	results := make([]Result, len(states))
	materializeStarted := time.Now()
	for stateIndex := range states {
		state := &states[stateIndex]
		state.result.Paths = make([]Path, 0, len(targets))
		for i := range state.nodes {
			if _, ok := targets[state.nodes[i].symbol.Function]; ok {
				state.result.Paths = append(state.result.Paths, materializePath(state.nodes, i))
			}
		}
		sort.Slice(state.result.Paths, func(i, j int) bool {
			a := state.result.Paths[i].Functions[len(state.result.Paths[i].Functions)-1]
			b := state.result.Paths[j].Functions[len(state.result.Paths[j].Functions)-1]
			return functionLess(a, b)
		})
		state.result.Stats.Visited = len(state.visited)
		results[stateIndex] = state.result
	}
	stats.MaterializeTime = time.Since(materializeStarted)
	return results, stats, nil
}

// Traverse walks complete breadth-first frontiers. Exact and unknown arities
// remain separate because each is a distinct interned function identity.
func Traverse(graph source, seeds []parser.FunctionID, options Options) (Result, error) {
	if options.Direction != Forward && options.Direction != Reverse {
		return Result{}, errors.New("call graph direction must be forward or reverse")
	}
	if options.MaxDepth < -1 || options.MaxNodes < -1 {
		return Result{}, errors.New("call graph budgets must be -1 or greater")
	}

	resolveStarted := time.Now()
	requestedSeeds := uniqueFunctions(seeds)
	symbols, err := graph.ResolveCallSymbols(requestedSeeds)
	if err != nil {
		return Result{}, err
	}
	if options.MaxNodes >= 0 && len(symbols) > options.MaxNodes {
		return Result{}, errors.New("call graph node budget is smaller than the resolved seed set")
	}
	sort.Slice(symbols, func(i, j int) bool {
		return functionLess(symbols[i].Function, symbols[j].Function)
	})

	result := Result{Stats: Stats{Seeds: len(symbols), Visited: len(symbols), ResolveTime: time.Since(resolveStarted)}}
	resolvedSeeds := make(map[parser.FunctionID]struct{}, len(symbols))
	for _, symbol := range symbols {
		resolvedSeeds[symbol.Function] = struct{}{}
	}
	for _, seed := range requestedSeeds {
		if _, ok := resolvedSeeds[seed]; !ok {
			result.UnresolvedSeeds = append(result.UnresolvedSeeds, seed)
		}
	}
	targets := make(map[parser.FunctionID]struct{}, len(options.Targets))
	for _, target := range options.Targets {
		targets[target] = struct{}{}
	}
	visited := make(map[int64]struct{}, len(symbols))
	nodes := make([]reachedNode, len(symbols))
	frontier := make([]int, len(symbols))
	for i, symbol := range symbols {
		visited[symbol.ID] = struct{}{}
		nodes[i] = reachedNode{symbol: symbol, parent: -1}
		frontier[i] = i
	}

	for depth := 0; len(frontier) > 0; depth++ {
		if options.MaxDepth >= 0 && depth >= options.MaxDepth {
			result.DepthTruncated = true
			break
		}
		ids := make([]int64, len(frontier))
		parents := make(map[int64]parentMetadata, len(frontier))
		for rank, nodeIndex := range frontier {
			ids[rank] = nodes[nodeIndex].symbol.ID
			parents[ids[rank]] = parentMetadata{index: nodeIndex, rank: rank}
		}
		var relations []store.CallRelation
		queryStarted := time.Now()
		if options.Direction == Forward {
			relations, err = graph.LookupCalleeFrontier(ids)
		} else {
			relations, err = graph.LookupCallerFrontier(ids)
		}
		if err != nil {
			return Result{}, err
		}
		result.Stats.QueryTime += time.Since(queryStarted)
		result.Stats.FrontierQueries++
		result.Stats.RelationsRead += len(relations)
		sortStarted := time.Now()
		sortRelationsByParentRank(relations, parents)
		result.Stats.SortTime += time.Since(sortStarted)

		expandStarted := time.Now()
		next := make([]int, 0, len(relations))
		for _, relation := range relations {
			if _, ok := visited[relation.Adjacent.ID]; ok {
				continue
			}
			if options.MaxNodes >= 0 && len(visited) >= options.MaxNodes {
				result.NodeTruncated = true
				break
			}
			visited[relation.Adjacent.ID] = struct{}{}
			nodes = append(nodes, reachedNode{
				symbol: relation.Adjacent,
				parent: parents[relation.FrontierID].index,
				kind:   relation.Kind,
				depth:  depth + 1,
			})
			next = append(next, len(nodes)-1)
			result.Stats.DepthReached = depth + 1
		}
		frontier = next
		result.Stats.ExpandTime += time.Since(expandStarted)
	}

	materializeStarted := time.Now()
	result.Paths = make([]Path, 0, len(targets))
	for i := range nodes {
		if _, ok := targets[nodes[i].symbol.Function]; ok {
			result.Paths = append(result.Paths, materializePath(nodes, i))
		}
	}
	sort.Slice(result.Paths, func(i, j int) bool {
		a := result.Paths[i].Functions[len(result.Paths[i].Functions)-1]
		b := result.Paths[j].Functions[len(result.Paths[j].Functions)-1]
		return functionLess(a, b)
	})
	result.Stats.MaterializeTime = time.Since(materializeStarted)
	result.Stats.Visited = len(visited)
	return result, nil
}

func uniqueFunctions(functions []parser.FunctionID) []parser.FunctionID {
	seen := make(map[parser.FunctionID]struct{}, len(functions))
	result := make([]parser.FunctionID, 0, len(functions))
	for _, function := range functions {
		if _, ok := seen[function]; !ok {
			seen[function] = struct{}{}
			result = append(result, function)
		}
	}
	sort.Slice(result, func(i, j int) bool { return functionLess(result[i], result[j]) })
	return result
}

func sortRelationsByParentRank(relations []store.CallRelation, parents map[int64]parentMetadata) {
	sort.Slice(relations, func(i, j int) bool {
		a, b := relations[i], relations[j]
		aParent, bParent := parents[a.FrontierID], parents[b.FrontierID]
		if aParent.rank != bParent.rank {
			return aParent.rank < bParent.rank
		}
		if a.Adjacent.Function != b.Adjacent.Function {
			return functionLess(a.Adjacent.Function, b.Adjacent.Function)
		}
		return a.Kind < b.Kind
	})
}

func materializePath(nodes []reachedNode, index int) Path {
	path := Path{
		Functions: make([]parser.FunctionID, nodes[index].depth+1),
		Kinds:     make([]string, nodes[index].depth),
	}
	for position := nodes[index].depth; index >= 0; position-- {
		path.Functions[position] = nodes[index].symbol.Function
		if position > 0 {
			path.Kinds[position-1] = nodes[index].kind
		}
		index = nodes[index].parent
	}
	return path
}

func functionLess(a, b parser.FunctionID) bool {
	if a.Module != b.Module {
		return a.Module < b.Module
	}
	if a.Function != b.Function {
		return a.Function < b.Function
	}
	return a.Arity < b.Arity
}
