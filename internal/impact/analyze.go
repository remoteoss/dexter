// Package impact compares independent source indexes and selects candidate tests.
package impact

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/remoteoss/dexter/internal/callgraph"
	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

type Options struct {
	MaxDepth     int
	MaxNodes     int
	ChangedFiles []string
}

type ChangedFunction struct {
	Function        parser.FunctionID `json:"function"`
	Change          string            `json:"change"`
	Evidence        string            `json:"evidence"`
	BaseFingerprint string            `json:"base_fingerprint,omitempty"`
	HeadFingerprint string            `json:"head_fingerprint,omitempty"`
}

type Explanation struct {
	ChangedFunction *parser.FunctionID  `json:"changed_function,omitempty"`
	ChangedFile     string              `json:"changed_file,omitempty"`
	Graph           string              `json:"graph"`
	Path            []parser.FunctionID `json:"path,omitempty"`
	Kinds           []string            `json:"kinds,omitempty"`
	Uncertainty     string              `json:"uncertainty,omitempty"`
}

type Candidate struct {
	File         string        `json:"file"`
	Explanations []Explanation `json:"explanations"`
}

type Coverage struct {
	TestFiles       int `json:"test_files"`
	RootedTestFiles int `json:"rooted_test_files"`
}

type TraversalStats struct {
	Runs                    int   `json:"runs"`
	Visited                 int   `json:"visited"`
	RelationsRead           int   `json:"relations_read"`
	FrontierQueries         int   `json:"frontier_queries"`
	FrontierRelationsRead   int   `json:"frontier_relations_read"`
	MaxDepth                int   `json:"max_depth"`
	TruncatedRuns           int   `json:"truncated_runs"`
	ResolveMicroseconds     int64 `json:"resolve_us"`
	QueryMicroseconds       int64 `json:"query_us"`
	SortMicroseconds        int64 `json:"sort_us"`
	ExpandMicroseconds      int64 `json:"expand_us"`
	MaterializeMicroseconds int64 `json:"materialize_us"`
}

type Result struct {
	ChangedFunctions []ChangedFunction `json:"changed_functions"`
	Candidates       []Candidate       `json:"candidates"`
	Unresolved       []string          `json:"unresolved,omitempty"`
	Coverage         Coverage          `json:"source_test_coverage"`
	Traversal        TraversalStats    `json:"traversal"`
	Timing           AnalysisTiming    `json:"timing"`
}

type AnalysisTiming struct {
	InventoryMicroseconds   int64 `json:"inventory_us"`
	FingerprintMicroseconds int64 `json:"fingerprint_diff_us"`
	TestRootsMicroseconds   int64 `json:"test_roots_us"`
	TraversalMicroseconds   int64 `json:"traversal_us"`
	FinalizeMicroseconds    int64 `json:"finalize_us"`
}

// Analyze compares two independent indexes and traverses each graph separately.
func Analyze(base *store.Store, baseRoot string, head *store.Store, headRoot string, options Options) (Result, error) {
	inventoryStarted := time.Now()
	type inventoryResult struct {
		functions []store.FunctionFingerprint
		tests     []store.FunctionRecord
		err       error
	}
	baseInventory := make(chan inventoryResult, 1)
	headInventory := make(chan inventoryResult, 1)
	go func() {
		functions, err := base.ListFunctionFingerprints()
		if err != nil {
			baseInventory <- inventoryResult{err: err}
			return
		}
		tests, err := base.ListTestFunctionRecords()
		baseInventory <- inventoryResult{functions: functions, tests: tests, err: err}
	}()
	go func() {
		functions, err := head.ListFunctionFingerprints()
		if err != nil {
			headInventory <- inventoryResult{err: err}
			return
		}
		tests, err := head.ListTestFunctionRecords()
		headInventory <- inventoryResult{functions: functions, tests: tests, err: err}
	}()
	baseResult, headResult := <-baseInventory, <-headInventory
	if baseResult.err != nil {
		return Result{}, baseResult.err
	}
	if headResult.err != nil {
		return Result{}, headResult.err
	}
	inventoryElapsed := time.Since(inventoryStarted)
	fingerprintStarted := time.Now()
	forcedFunctions := make(map[parser.FunctionID]struct{})
	matchedChangedFiles := make(map[string]struct{})
	changedFiles := make(map[string]struct{}, len(options.ChangedFiles))
	for _, path := range options.ChangedFiles {
		changedFiles[filepath.ToSlash(path)] = struct{}{}
	}
	changedPaths := make([]string, 0, len(changedFiles))
	for path := range changedFiles {
		changedPaths = append(changedPaths, path)
	}
	baseChanged, err := base.ListFunctionsInFiles(changedPaths)
	if err != nil {
		return Result{}, err
	}
	headChanged, err := head.ListFunctionsInFiles(changedPaths)
	if err != nil {
		return Result{}, err
	}
	for _, function := range append(baseChanged, headChanged...) {
		forcedFunctions[function] = struct{}{}
	}
	baseCallableFiles, err := base.ListCallableFiles(changedPaths)
	if err != nil {
		return Result{}, err
	}
	headCallableFiles, err := head.ListCallableFiles(changedPaths)
	if err != nil {
		return Result{}, err
	}
	for _, path := range append(baseCallableFiles, headCallableFiles...) {
		matchedChangedFiles[path] = struct{}{}
	}
	changes := diffFunctions(baseResult.functions, headResult.functions, forcedFunctions)
	fingerprintElapsed := time.Since(fingerprintStarted)

	testRootsStarted := time.Now()
	headTests, err := testInventory(head, headRoot)
	if err != nil {
		return Result{}, err
	}
	baseRoots := testRoots(baseResult.tests, baseRoot)
	headRoots := testRoots(headResult.tests, headRoot)
	rootedFiles := make(map[string]struct{})
	for _, files := range headRoots {
		for _, file := range files {
			rootedFiles[file] = struct{}{}
		}
	}

	result := Result{
		ChangedFunctions: changes,
		Candidates:       make([]Candidate, 0),
		Coverage:         Coverage{TestFiles: len(headTests), RootedTestFiles: len(rootedFiles)},
		Timing: AnalysisTiming{
			InventoryMicroseconds:   inventoryElapsed.Microseconds(),
			FingerprintMicroseconds: fingerprintElapsed.Microseconds(),
			TestRootsMicroseconds:   time.Since(testRootsStarted).Microseconds(),
		},
	}
	candidates := make(map[string][]Explanation)
	if len(changes) > 0 {
		baseMissing, err := base.MissingImpactEvidenceProviders()
		if err != nil {
			return Result{}, err
		}
		headMissing, err := head.MissingImpactEvidenceProviders()
		if err != nil {
			return Result{}, err
		}
		for _, provider := range append(baseMissing, headMissing...) {
			result.Unresolved = append(result.Unresolved, "missing required evidence provider: "+provider)
		}
		if len(baseMissing)+len(headMissing) > 0 {
			selectAllTests(candidates, headTests, Explanation{Graph: "both", Uncertainty: "missing_required_evidence"})
		}
	}
	for changedFile := range changedFiles {
		scopedPath, inScope := relativePath(headRoot, changedFile)
		if _, exists := headTests[scopedPath]; inScope && exists {
			candidates[scopedPath] = append(candidates[scopedPath], Explanation{ChangedFile: changedFile, Graph: "head", Uncertainty: "directly_changed_test"})
		}
	}
	unsupportedChangedFiles := 0
	for changedFile := range changedFiles {
		if _, matched := matchedChangedFiles[changedFile]; matched {
			continue
		}
		scopedPath, inScope := relativePath(headRoot, changedFile)
		if _, changedTest := headTests[scopedPath]; inScope && changedTest {
			continue
		}
		unsupportedChangedFiles++
	}
	if unsupportedChangedFiles > 0 {
		result.Unresolved = append(result.Unresolved, fmt.Sprintf("changed files contain no callable source definitions: %d files", unsupportedChangedFiles))
		selectAllTests(candidates, headTests, Explanation{Graph: "head", Uncertainty: "unsupported_changed_files"})
	}
	unsupportedRoots := 0
	for file := range headTests {
		if _, rooted := rootedFiles[file]; rooted {
			continue
		}
		unsupportedRoots++
		if len(changes) > 0 {
			candidates[file] = append(candidates[file], Explanation{Graph: "head", Uncertainty: "unsupported_test_root"})
		}
	}
	if unsupportedRoots > 0 && len(changes) > 0 {
		result.Unresolved = append(result.Unresolved, fmt.Sprintf("unsupported source test roots: %d files", unsupportedRoots))
	}
	var baseChanges, headChanges []ChangedFunction
	for _, change := range changes {
		if change.Change != "added" {
			baseChanges = append(baseChanges, change)
		}
		if change.Change != "removed" {
			headChanges = append(headChanges, change)
		}
	}
	traversalStarted := time.Now()
	var baseSide, headSide Result
	baseCandidates := make(map[string][]Explanation)
	headCandidates := make(map[string][]Explanation)
	var sides sync.WaitGroup
	sides.Add(2)
	go func() {
		defer sides.Done()
		analyzeSideBatch(&baseSide, baseCandidates, headTests, base, "base", baseChanges, baseRoots, options)
	}()
	go func() {
		defer sides.Done()
		analyzeSideBatch(&headSide, headCandidates, headTests, head, "head", headChanges, headRoots, options)
	}()
	sides.Wait()
	mergeSideResult(&result, candidates, baseSide, baseCandidates)
	mergeSideResult(&result, candidates, headSide, headCandidates)
	if len(changes) > 0 && len(candidates) < len(headTests) {
		analyzeUnresolvedEvidence(&result, candidates, headTests, base, "base", baseRoots, options)
		analyzeUnresolvedEvidence(&result, candidates, headTests, head, "head", headRoots, options)
	}
	result.Timing.TraversalMicroseconds = time.Since(traversalStarted).Microseconds()

	finalizeStarted := time.Now()
	files := make([]string, 0, len(candidates))
	for file := range candidates {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		explanations := candidates[file]
		sort.Slice(explanations, func(i, j int) bool {
			a, b := explanations[i].ChangedFunction, explanations[j].ChangedFunction
			if a == nil || b == nil {
				return a == nil && b != nil
			}
			if *a != *b {
				return functionLess(*a, *b)
			}
			return explanations[i].Graph < explanations[j].Graph
		})
		result.Candidates = append(result.Candidates, Candidate{File: file, Explanations: explanations})
	}
	sort.Strings(result.Unresolved)
	result.Timing.FinalizeMicroseconds = time.Since(finalizeStarted).Microseconds()
	return result, nil
}

func analyzeUnresolvedEvidence(result *Result, candidates map[string][]Explanation, headTests map[string]struct{}, graph *store.Store, side string, roots map[parser.FunctionID][]string, options Options) {
	unresolved, err := graph.ListImpactUnresolved()
	if err != nil {
		result.Unresolved = append(result.Unresolved, side+" unresolved evidence: "+err.Error())
		selectAllTests(candidates, headTests, Explanation{Graph: side, Uncertainty: "unresolved_evidence_error"})
		return
	}
	if len(unresolved) == 0 {
		return
	}
	seeds := make([]parser.FunctionID, 0, len(unresolved))
	kinds := make(map[parser.FunctionID]string, len(unresolved))
	for _, record := range unresolved {
		seeds = append(seeds, record.Caller)
		uncertainty := "provider:" + record.Provider + ":" + record.Kind
		if previous, exists := kinds[record.Caller]; !exists || uncertainty < previous {
			kinds[record.Caller] = uncertainty
		}
		result.Unresolved = append(result.Unresolved, fmt.Sprintf("%s %s %s.%s/%d: %s", side, record.Provider,
			record.Caller.Module, record.Caller.Function, record.Caller.Arity, record.Kind))
	}
	targets := make([]parser.FunctionID, 0, len(roots))
	for target := range roots {
		targets = append(targets, target)
	}
	traversal, err := callgraph.Traverse(graph, seeds, callgraph.Options{
		Direction: callgraph.Reverse,
		MaxDepth:  options.MaxDepth,
		MaxNodes:  options.MaxNodes,
		Targets:   targets,
	})
	if err != nil || len(traversal.Paths) == 0 || len(traversal.UnresolvedSeeds) > 0 || traversal.DepthTruncated || traversal.NodeTruncated {
		selectAllTests(candidates, headTests, Explanation{Graph: side, Uncertainty: "unresolved_evidence_scope"})
		return
	}
	for _, path := range traversal.Paths {
		target := path.Functions[len(path.Functions)-1]
		for _, file := range roots[target] {
			if _, exists := headTests[file]; !exists {
				continue
			}
			candidates[file] = append(candidates[file], Explanation{
				Graph: side, Path: path.Functions, Kinds: path.Kinds, Uncertainty: kinds[path.Functions[0]],
			})
		}
	}
}

func mergeSideResult(result *Result, candidates map[string][]Explanation, side Result, sideCandidates map[string][]Explanation) {
	result.Unresolved = append(result.Unresolved, side.Unresolved...)
	result.Traversal.Runs += side.Traversal.Runs
	result.Traversal.Visited += side.Traversal.Visited
	result.Traversal.RelationsRead += side.Traversal.RelationsRead
	result.Traversal.FrontierQueries += side.Traversal.FrontierQueries
	result.Traversal.FrontierRelationsRead += side.Traversal.FrontierRelationsRead
	result.Traversal.MaxDepth = max(result.Traversal.MaxDepth, side.Traversal.MaxDepth)
	result.Traversal.TruncatedRuns += side.Traversal.TruncatedRuns
	result.Traversal.ResolveMicroseconds += side.Traversal.ResolveMicroseconds
	result.Traversal.QueryMicroseconds += side.Traversal.QueryMicroseconds
	result.Traversal.SortMicroseconds += side.Traversal.SortMicroseconds
	result.Traversal.ExpandMicroseconds += side.Traversal.ExpandMicroseconds
	result.Traversal.MaterializeMicroseconds += side.Traversal.MaterializeMicroseconds
	for file, explanations := range sideCandidates {
		candidates[file] = append(candidates[file], explanations...)
	}
}

func analyzeSideBatch(result *Result, candidates map[string][]Explanation, headTests map[string]struct{}, graph *store.Store, side string, changes []ChangedFunction, roots map[parser.FunctionID][]string, options Options) {
	if len(changes) == 0 {
		return
	}
	targets := make([]parser.FunctionID, 0, len(roots))
	for target := range roots {
		targets = append(targets, target)
	}
	seedOwners := make(map[parser.FunctionID]parser.FunctionID, len(changes)*2)
	seeds := make([]parser.FunctionID, 0, len(changes)*2)
	for _, change := range changes {
		seeds = append(seeds, change.Function)
		seedOwners[change.Function] = change.Function
		if change.Function.Arity != parser.UnknownArity {
			unknown := parser.FunctionID{Module: change.Function.Module, Function: change.Function.Function, Arity: parser.UnknownArity}
			seeds = append(seeds, unknown)
			if owner, exists := seedOwners[unknown]; !exists || functionLess(change.Function, owner) {
				seedOwners[unknown] = change.Function
			}
		}
	}
	traversal, err := callgraph.Traverse(graph, seeds, callgraph.Options{
		Direction: callgraph.Reverse,
		MaxDepth:  options.MaxDepth,
		MaxNodes:  options.MaxNodes,
		Targets:   targets,
	})
	if err != nil {
		for _, change := range changes {
			changed := change.Function
			result.Unresolved = append(result.Unresolved, fmt.Sprintf("%s %s.%s/%d: %v", side, changed.Module, changed.Function, changed.Arity, err))
		}
		selectAllTests(candidates, headTests, Explanation{Graph: side, Uncertainty: "traversal_error"})
		return
	}
	result.Traversal.Runs++
	result.Traversal.Visited += traversal.Stats.Visited
	result.Traversal.RelationsRead += traversal.Stats.RelationsRead
	result.Traversal.FrontierQueries += traversal.Stats.FrontierQueries
	result.Traversal.FrontierRelationsRead += traversal.Stats.RelationsRead
	result.Traversal.ResolveMicroseconds += traversal.Stats.ResolveTime.Microseconds()
	result.Traversal.QueryMicroseconds += traversal.Stats.QueryTime.Microseconds()
	result.Traversal.SortMicroseconds += traversal.Stats.SortTime.Microseconds()
	result.Traversal.ExpandMicroseconds += traversal.Stats.ExpandTime.Microseconds()
	result.Traversal.MaterializeMicroseconds += traversal.Stats.MaterializeTime.Microseconds()
	result.Traversal.MaxDepth = max(result.Traversal.MaxDepth, traversal.Stats.DepthReached)
	if traversal.DepthTruncated || traversal.NodeTruncated {
		result.Traversal.TruncatedRuns++
	}
	unresolvedExact := make(map[parser.FunctionID]struct{})
	for _, unresolved := range traversal.UnresolvedSeeds {
		if owner, ok := seedOwners[unresolved]; ok && unresolved == owner {
			unresolvedExact[owner] = struct{}{}
		}
	}
	for changed := range unresolvedExact {
		result.Unresolved = append(result.Unresolved, fmt.Sprintf("%s %s.%s/%d: unresolved_seed", side, changed.Module, changed.Function, changed.Arity))
	}
	if len(unresolvedExact) > 0 {
		selectAllTests(candidates, headTests, Explanation{Graph: side, Uncertainty: "unresolved_seed"})
	}
	if traversal.DepthTruncated || traversal.NodeTruncated {
		for _, change := range changes {
			changed := change.Function
			result.Unresolved = append(result.Unresolved, fmt.Sprintf("%s %s.%s/%d: traversal_budget", side, changed.Module, changed.Function, changed.Arity))
		}
		selectAllTests(candidates, headTests, Explanation{Graph: side, Uncertainty: "traversal_budget"})
	}
	for _, path := range traversal.Paths {
		changed := seedOwners[path.Functions[0]]
		target := path.Functions[len(path.Functions)-1]
		uncertainty := ""
		if path.Functions[0].Arity == parser.UnknownArity {
			uncertainty = "unknown_arity"
		}
		for _, file := range roots[target] {
			if _, exists := headTests[file]; !exists {
				continue
			}
			candidates[file] = append(candidates[file], Explanation{
				ChangedFunction: functionPointer(changed),
				Graph:           side,
				Path:            path.Functions,
				Kinds:           path.Kinds,
				Uncertainty:     uncertainty,
			})
		}
	}
}

func functionPointer(function parser.FunctionID) *parser.FunctionID {
	return &function
}

func selectAllTests(candidates map[string][]Explanation, tests map[string]struct{}, explanation Explanation) {
	for file := range tests {
		candidates[file] = append(candidates[file], explanation)
	}
}

func diffFunctions(base, head []store.FunctionFingerprint, forced map[parser.FunctionID]struct{}) []ChangedFunction {
	changes := make([]ChangedFunction, 0)
	for baseIndex, headIndex := 0, 0; baseIndex < len(base) || headIndex < len(head); {
		inBase := baseIndex < len(base)
		inHead := headIndex < len(head)
		var baseFunction, headFunction store.FunctionFingerprint
		if inBase {
			baseFunction = base[baseIndex]
		}
		if inHead {
			headFunction = head[headIndex]
		}
		if inBase && inHead {
			switch {
			case functionLess(baseFunction.Function, headFunction.Function):
				inHead = false
			case functionLess(headFunction.Function, baseFunction.Function):
				inBase = false
			}
		}
		function := headFunction.Function
		if inBase {
			function = baseFunction.Function
		}
		_, forceChanged := forced[function]
		if inBase && inHead && baseFunction.Fingerprint == headFunction.Fingerprint && !forceChanged {
			baseIndex++
			headIndex++
			continue
		}
		change := ChangedFunction{Function: function, Evidence: "fingerprint"}
		switch {
		case !inBase:
			change.Change = "added"
		case !inHead:
			change.Change = "removed"
		default:
			change.Change = "modified"
			if baseFunction.Fingerprint == headFunction.Fingerprint && forceChanged {
				change.Evidence = "changed_file"
			}
		}
		if inBase {
			change.BaseFingerprint = hex.EncodeToString(baseFunction.Fingerprint[:])
			baseIndex++
		}
		if inHead {
			change.HeadFingerprint = hex.EncodeToString(headFunction.Fingerprint[:])
			headIndex++
		}
		changes = append(changes, change)
	}
	return changes
}

func testRoots(records []store.FunctionRecord, root string) map[parser.FunctionID][]string {
	result := make(map[parser.FunctionID][]string)
	for _, record := range records {
		path, ok := relativePath(root, record.FilePath)
		if !ok || !strings.HasSuffix(path, "_test.exs") {
			continue
		}
		files := result[record.Function]
		if len(files) == 0 || files[len(files)-1] != path {
			result[record.Function] = append(files, path)
		}
	}
	return result
}

func testInventory(index *store.Store, root string) (map[string]struct{}, error) {
	paths, err := index.ListFilePaths()
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{})
	for _, path := range paths {
		relative, ok := relativePath(root, path)
		if ok && strings.HasSuffix(relative, "_test.exs") {
			result[relative] = struct{}{}
		}
	}
	return result, nil
}

func relativePath(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(relative), true
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
