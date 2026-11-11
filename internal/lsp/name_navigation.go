package lsp

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"sync"

	"github.com/remoteoss/dexter/internal/beam"
	"github.com/remoteoss/dexter/internal/store"
)

// NameKind selects which Elixir namespace a canonical name refers to.
type NameKind uint8

const (
	NameKindAny NameKind = iota
	NameKindCallable
	NameKindType
)

// NameLocation is a protocol-neutral source location for CLI and MCP clients.
type NameLocation struct {
	FilePath      string
	Line          int
	Kind          string
	Arity         int
	IsDeclaration bool

	// Also holds more places that describe the same function, for a reader
	// rather than for navigation: the def in a __using__ quote when the
	// compiled module records a later line that declared the function.
	Also []NameLocation
}

type NameLookupOptions struct {
	Kind             NameKind
	FollowDelegates  bool
	External         bool
	FallbackToModule bool
	ExcludeStdlib    bool
	// ExactModule places a generated function only at its own module's
	// definition. Without it, a module that exists only as a BEAM, such as
	// Phoenix route helpers, resolves to the nearest lexical parent with source;
	// that is a fallback, which a strict lookup must not take.
	ExactModule bool
	// CallArity, when HasArity is set, is the arity of the call at the
	// cursor: only the definitions of that arity are kept, unless none has
	// it (an arity Dexter reads wrong must not lose the answer).
	CallArity int
	HasArity  bool
}

type NameReferenceOptions struct {
	// Context ends the search early, for example when the editor cancels
	// the request; nil never ends it.
	Context            context.Context
	Kind               NameKind
	FollowDelegates    bool
	IncludeDeclaration bool
	ExcludeStdlib      bool
	InjectorModules    []string
	GeneratedProvider  string
	// Arity, when HasArity is set, names one function of the name: only its
	// references and declarations are listed. A function with default
	// arguments has several arities (see aritySet). A reference whose arity
	// Dexter cannot read stays.
	Arity    int
	HasArity bool
	// arities is the arity set of the function, once ReferenceNames read it.
	arities map[int]bool
}

// LookupName resolves a canonical module/function name through the same
// use-chain and generated-symbol logic used by editor navigation.
func (s *Server) LookupName(module, function string, opts NameLookupOptions) ([]NameLocation, error) {
	if function == "" {
		results, err := s.store.LookupModule(module)
		if err == nil && len(results) == 0 {
			// A module that exists only as a BEAM, such as one Module.create
			// made, is defined where the compiler recorded it.
			results = s.generatedModuleLocation(module)
		}
		return s.lookupLocations(results, opts.ExcludeStdlib), err
	}

	var results []store.LookupResult
	var err error
	if opts.FollowDelegates {
		results, err = s.store.LookupFollowDelegate(module, function)
	} else {
		results, err = s.store.LookupFunction(module, function)
	}
	if err != nil {
		return nil, err
	}
	results = filterLookupKind(results, opts.Kind)
	if opts.External {
		results = filterOutPrivate(results)
	}
	results = filterArity(results, opts)
	var also []store.LookupResult
	var unknownUse store.LookupResult
	var hasUnknown bool
	if len(results) == 0 {
		var injected []store.LookupResult
		injected, unknownUse, hasUnknown = s.lookupThroughUseOfDetailed(module, function, opts.FollowDelegates)
		results = filterArity(filterLookupKind(injected, opts.Kind), opts)
		if len(results) > 0 && opts.Kind != NameKindType {
			if declared := s.declaredLineOfInjected(module, function); len(declared) > 0 {
				results, also = declared, results
			}
		}
	}
	if len(results) == 0 && opts.Kind != NameKindType {
		if generated, found := s.generatedSymbol(module, "", function); found && len(generated) > 0 {
			// A line the compiled module records for the function is its own
			// definition, so even an exact lookup takes it.
			var precise bool
			results, precise = s.generatedDefinitionResultsFor(module, "", generated)
			if !precise && opts.ExactModule {
				if results, err = s.store.LookupModule(module); err != nil {
					return nil, err
				}
			}
			if !precise && len(results) > 0 {
				results[0].Arity = generated[0].Arity
				results[0].Kind = generated[0].Kind
			}
		}
	}
	if len(results) == 0 && opts.Kind != NameKindType {
		// A use chain that reads an option Dexter cannot know: its `use`
		// line, not a default that can be wrong.
		if hasUnknown {
			results = []store.LookupResult{unknownUse}
		}
	}
	if len(results) == 0 && opts.FallbackToModule {
		results, err = s.store.LookupModule(module)
		if err != nil {
			return nil, err
		}
	}
	locations := s.lookupLocations(results, opts.ExcludeStdlib)
	if len(also) > 0 && len(locations) > 0 {
		locations[0].Also = s.lookupLocations(also, opts.ExcludeStdlib)
	}
	return locations, nil
}

// declaredLineOfInjected is where the compiled module says it declared a
// function that a `use` also injects, when that is a later line of the
// module's own file and not a `use` line: a macro call such as
// `schema "things" do` that generates the function after a `use` brought the
// macro in. A def that a `use` injects is expanded at the `use` line, so for
// it this returns nothing and the def in the quote stays the answer.
//
// The first lookup of a module reads its compiled module once (as completion
// does), and the cache keeps the outcome; so the answer does not depend on
// which request came first.
func (s *Server) declaredLineOfInjected(module, function string) []store.LookupResult {
	compiled := s.generatedFunctionsForModule(module)
	entry, ok := s.generatedCache.get(module)
	if !ok || entry.beamPath == "" || entry.sourcePath == "" {
		return nil
	}
	var functions []beam.Function
	for _, f := range compiled {
		if f.Name == function {
			functions = append(functions, f)
		}
	}
	if len(functions) == 0 {
		return nil
	}
	sources, ok := s.generatedDefinitionSourcesFor(module, entry.beamPath)
	if !ok || len(sources.debugInfo.Lines) == 0 {
		return nil
	}
	// The use lines come from the index, so the common case (the def is
	// expanded at a `use` line) reads no file. A `use` inside a quote counts
	// as one too, which can only keep the quote's def.
	useLines, err := s.store.UseLines(entry.sourcePath)
	if err != nil {
		return nil
	}
	lines := generatedFunctionLines(module, entry.sourcePath, sources, functions)
	for _, line := range lines {
		if line.Line <= sources.debugInfo.ModuleLine || useLines[line.Line] {
			return nil
		}
	}
	// An edit since the compile can have removed the line.
	src := s.currentSourceOf(entry.sourcePath)
	if src == nil {
		return nil
	}
	for _, line := range lines {
		if line.Line > len(src.lines) {
			return nil
		}
	}
	return lines
}

// testHookReferences, when set by a test, runs at the start of ReferenceNames.
var testHookReferences func(ctx context.Context)

// ReferenceNames finds references after a frontend has resolved a canonical
// module/function name. Cursor-specific alias and variable resolution stays in
// the LSP adapter.
//
// The answer comes from the index, which records the unqualified calls of a
// function that a file imports (also in scripts and on def lines) and the
// keys of an import's only: list, so it lists the sites that a rename changes
// (siteCollector reads the files instead, because a rename must see them as
// they are now). The declarations come only with IncludeDeclaration. With
// FollowDelegates, a defdelegate that forwards to the function, and the calls
// of that facade, are references too; a facade that a __using__ quote injects
// is in each module that uses the quote.
func (s *Server) ReferenceNames(module, function string, opts NameReferenceOptions) ([]NameLocation, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if testHookReferences != nil {
		testHookReferences(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	injectors := append([]string(nil), opts.InjectorModules...)
	var injectorCh chan []string
	if function != "" && function != "__using__" && len(injectors) == 0 {
		injectorCh = make(chan []string, 1)
		go func() {
			defer func() {
				if value := recover(); value != nil {
					s.index.reporter.Panicked("request", "the search for __using__ injectors", value, debug.Stack())
					injectorCh <- nil
				}
			}()
			injectorCh <- s.findModulesWhoseUsingImports(module)
		}()
	}

	if opts.HasArity && function != "" {
		// When every definition of the name is the requested function,
		// there is nothing to filter, and the request is the name's.
		var whole bool
		if opts.arities, whole = s.aritySet(module, function, opts.Arity); whole {
			opts.HasArity, opts.arities = false, nil
		}
	}
	// The arity of each reference is read only for a filter.
	lookupRefs := s.referenceLookup(opts)
	refResults, err := lookupRefs(module, function)
	if err != nil {
		return nil, err
	}
	if function == "__using__" {
		moduleRefs, lookupErr := s.store.LookupReferences(module, "")
		if lookupErr != nil {
			return nil, lookupErr
		}
		refResults = refResults[:0]
		for _, result := range moduleRefs {
			if result.Kind == "use" || result.Kind == "quoted_use" {
				refResults = append(refResults, result)
			}
		}
	} else {
		moduleRefs := refResults
		if function != "" {
			moduleRefs, _ = s.store.LookupReferences(module, "")
		}
		refResults = append(refResults, s.injectedAliasReferences(module, function, moduleRefs)...)

		if injectorCh != nil {
			select {
			case injectors = <-injectorCh:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		for _, injector := range injectors {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			transitive, lookupErr := lookupRefs(injector, function)
			if lookupErr != nil {
				continue
			}
			if opts.GeneratedProvider != "" {
				transitive = s.filterGeneratedProviderReferences(opts.GeneratedProvider, function, transitive)
			}
			refResults = append(refResults, transitive...)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if function != "" {
			refResults = append(refResults, s.findBareCallRefs(module, function, opts.HasArity)...)
		}
		if function != "" && opts.FollowDelegates {
			delegated, err := s.delegateReferences(ctx, module, function, moduleRefs, opts)
			if err != nil {
				return nil, err
			}
			refResults = append(refResults, delegated...)
		}
	}
	return s.nameLocations(module, function, refResults, opts)
}

// delegateReferences returns the defdelegate lines that forward to
// module.function and the calls of those facades: qualified calls (one query
// for all the facade modules of a name) and bare calls in the facade module.
// A defdelegate that forwards to module itself is a facade in a __using__
// quote of module (see siteCollector.collect): the facade is in every module
// that uses module (moduleRefs has the use lines), and the index records its
// bare calls there as calls of module.
func (s *Server) delegateReferences(ctx context.Context, module, function string, moduleRefs []store.ReferenceResult, opts NameReferenceOptions) ([]store.ReferenceResult, error) {
	injected := delegateHeads(s.injectedDelegatesTo(module, function))
	delegates, _ := s.store.LookupDelegatesTo(module, function)
	heads := delegateHeads(delegates)
	if len(heads) == 0 && len(injected) == 0 {
		return nil, nil
	}
	filtered := opts.HasArity && opts.arities != nil
	// A facade forwards each of its arities with the full arity of its head
	// (`defdelegate f(a, b \\ 1)` calls the target's f/2 from f/1 too), so a
	// head is a reference when its full arity is requested, and then the
	// calls of all its arities are.
	forwards := func(h delegateHead) bool { return !filtered || opts.arities[h.Arity] }
	setOf := func(h delegateHead) map[int]bool {
		if !filtered {
			return nil
		}
		return h.arities
	}
	var out []store.ReferenceResult
	// The facade modules of each facade function and arity set: one query for
	// each group, so that a module's calls are filtered by its own head's
	// arities, not by those of another facade of the same name.
	type facadeKey struct{ fn, arities string }
	facades := make(map[facadeKey][]string)
	facadeArities := make(map[facadeKey]map[int]bool) // nil: all
	addFacade := func(fn string, set map[int]bool, mods ...string) {
		if len(mods) == 0 {
			return
		}
		k := facadeKey{fn, "*"}
		if set != nil {
			k.arities = fmt.Sprint(slices.Sorted(maps.Keys(set)))
		}
		facades[k] = append(facades[k], mods...)
		facadeArities[k] = set
	}
	// A facade in a __using__ quote is in each module that uses the quote's
	// module; the index records its bare calls there as calls of that module.
	for _, d := range injected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !forwards(d) {
			continue
		}
		out = append(out, store.ReferenceResult{FilePath: d.FilePath, Line: d.Line, Kind: siteDelegate, Arity: -1})
		out = append(out, s.facadeReferences(d.Module, d.Function, setOf(d))...)
		var users []string
		refs, _ := s.store.LookupReferences(d.Module, "")
		for _, u := range refs {
			if u.Kind != "use" {
				continue
			}
			if user := s.store.LookupEnclosingModule(u.FilePath, u.Line); user != "" && user != d.Module {
				users = append(users, user)
			}
		}
		addFacade(d.Function, setOf(d), users...)
	}
	for _, d := range heads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !forwards(d) {
			continue
		}
		out = append(out, store.ReferenceResult{FilePath: d.FilePath, Line: d.Line, Kind: siteDelegate, Arity: -1})
		if d.Module != module {
			addFacade(d.Function, setOf(d), d.Module)
			out = append(out, withAritiesIn(s.findBareCallRefs(d.Module, d.Function, filtered), setOf(d))...)
			continue
		}
		if d.Function != function {
			out = append(out, s.facadeReferences(module, d.Function, setOf(d))...)
		}
		var users []string
		for _, u := range moduleRefs {
			if u.Kind != "use" {
				continue
			}
			if user := s.store.LookupEnclosingModule(u.FilePath, u.Line); user != "" && user != module {
				users = append(users, user)
			}
		}
		addFacade(d.Function, setOf(d), users...)
	}
	for k, mods := range facades {
		if refs, err := s.store.LookupReferencesInModules(mods, k.fn); err == nil {
			out = append(out, withAritiesIn(refs, facadeArities[k])...)
		}
	}
	return out, nil
}

// delegateHead is one defdelegate line, with the arities that its head
// defines; Arity is the full one.
type delegateHead struct {
	store.DelegateEntry
	arities map[int]bool
}

// delegateHeads groups the entries of a defdelegate with default arguments,
// one for each arity, into one head.
func delegateHeads(entries []store.DelegateEntry) []delegateHead {
	type key struct {
		path string
		line int
	}
	index := make(map[key]int, len(entries))
	var heads []delegateHead
	for _, e := range entries {
		k := key{e.FilePath, e.Line}
		i, ok := index[k]
		if !ok {
			index[k] = len(heads)
			heads = append(heads, delegateHead{DelegateEntry: e, arities: map[int]bool{e.Arity: true}})
			continue
		}
		heads[i].arities[e.Arity] = true
		if e.Arity > heads[i].Arity {
			heads[i].Arity = e.Arity
		}
	}
	return heads
}

// facadeReferences are the calls of a facade function with an arity of set
// (all calls when set is nil).
func (s *Server) facadeReferences(module, function string, set map[int]bool) []store.ReferenceResult {
	var refs []store.ReferenceResult
	var err error
	if set == nil {
		refs, err = s.store.LookupReferences(module, function)
	} else {
		refs, err = s.store.LookupReferencesWithArity(module, function, slices.Collect(maps.Keys(set)))
	}
	if err != nil {
		return nil
	}
	return withAritiesIn(refs, set)
}

// withAritiesIn keeps the references with an arity of set or an unknown one
// (all of them when set is nil), and marks each as unknown: a facade call is
// a call of the target with the head's full arity, whatever its own.
func withAritiesIn(refs []store.ReferenceResult, set map[int]bool) []store.ReferenceResult {
	out := refs[:0]
	for _, r := range refs {
		if set != nil && r.Arity >= 0 && !set[r.Arity] {
			continue
		}
		r.Arity = -1
		out = append(out, r)
	}
	return out
}

// referenceLookup returns the query of a function's references for opts:
// with the arity of each, and only those of opts' arities, when the request
// filters by arity.
func (s *Server) referenceLookup(opts NameReferenceOptions) func(module, function string) ([]store.ReferenceResult, error) {
	if !opts.HasArity || opts.arities == nil {
		return s.store.LookupReferences
	}
	arities := make([]int, 0, len(opts.arities))
	for a := range opts.arities {
		arities = append(arities, a)
	}
	return func(module, function string) ([]store.ReferenceResult, error) {
		return s.store.LookupReferencesWithArity(module, function, arities)
	}
}

// nameLocations filters references by kind, adds the declarations when
// asked, and lists each line once.
func (s *Server) nameLocations(module, function string, refResults []store.ReferenceResult, opts NameReferenceOptions) ([]NameLocation, error) {
	refResults = filterReferenceKind(refResults, opts.Kind)
	arities := opts.arities
	if opts.HasArity && function != "" && arities == nil {
		arities, _ = s.aritySet(module, function, opts.Arity)
	}
	if !opts.HasArity {
		arities = nil
	}
	locations := make([]NameLocation, 0, len(refResults)+1)
	seen := make(map[nameLocationKey]struct{}, len(refResults)+1)
	for _, result := range refResults {
		if opts.ExcludeStdlib && s.isStdlibPath(result.FilePath) {
			continue
		}
		if arities != nil && result.Arity >= 0 && !arities[result.Arity] {
			continue
		}
		appendNameLocation(&locations, seen, NameLocation{FilePath: result.FilePath, Line: result.Line, Kind: result.Kind})
	}
	if opts.IncludeDeclaration {
		lookupOpts := NameLookupOptions{Kind: opts.Kind, ExcludeStdlib: opts.ExcludeStdlib}
		declarations, lookupErr := s.LookupName(module, function, lookupOpts)
		if lookupErr != nil {
			return nil, lookupErr
		}
		for _, declaration := range declarations {
			if arities != nil && isFunctionKind(declaration.Kind) && !arities[declaration.Arity] {
				continue
			}
			declaration.IsDeclaration = true
			appendNameLocation(&locations, seen, declaration)
		}
	}
	return locations, nil
}

// aritySet returns the arities of the function module.function/arity: those
// of each definition line that has the arity, so that a def with default
// arguments (`def f(a, b \\ 1)`, f/1 and f/2) is one function. A name with
// no definition of the arity in the index (a generated or injected one) is
// that arity alone.
// whole is true when every definition of the name is in the set: the name
// is one function, so no reference can be of another arity.
func (s *Server) aritySet(module, function string, arity int) (set map[int]bool, whole bool) {
	set = map[int]bool{arity: true}
	defs, err := s.store.LookupFunction(module, function)
	if err != nil || len(defs) == 0 {
		defs = s.lookupThroughUseOfWithFollow(module, function, false)
	}
	type key struct {
		path string
		line int
	}
	lines := map[key]bool{}
	for _, d := range defs {
		if d.Arity == arity {
			lines[key{d.FilePath, d.Line}] = true
		}
	}
	whole = len(lines) > 0
	for _, d := range defs {
		if lines[key{d.FilePath, d.Line}] {
			set[d.Arity] = true
		} else if isFunctionKind(d.Kind) {
			whole = false
		}
	}
	return set, whole
}

// isFunctionKind reports whether a definition kind is a function or macro
// (with an arity), not a module or a type.
func isFunctionKind(kind string) bool {
	switch kind {
	case "def", "defp", "defmacro", "defmacrop", "defguard", "defguardp", "defdelegate":
		return true
	}
	return false
}

type nameLocationKey struct {
	filePath string
	line     int
}

func appendNameLocation(locations *[]NameLocation, seen map[nameLocationKey]struct{}, location NameLocation) {
	key := nameLocationKey{location.FilePath, location.Line}
	if _, ok := seen[key]; ok {
		return
	}
	seen[key] = struct{}{}
	*locations = append(*locations, location)
}

func (s *Server) lookupLocations(results []store.LookupResult, excludeStdlib bool) []NameLocation {
	locations := make([]NameLocation, 0, len(results))
	seen := make(map[nameLocationKey]struct{}, len(results))
	for _, result := range results {
		if excludeStdlib && s.isStdlibPath(result.FilePath) {
			continue
		}
		appendNameLocation(&locations, seen, NameLocation{
			FilePath: result.FilePath,
			Line:     result.Line,
			Kind:     result.Kind,
			Arity:    result.Arity,
		})
	}
	return locations
}

// filterArity keeps the results of opts.CallArity, or all of them when none
// has it. A function with default arguments has a row for each arity.
func filterArity(results []store.LookupResult, opts NameLookupOptions) []store.LookupResult {
	if !opts.HasArity || len(results) < 2 {
		return results
	}
	var kept []store.LookupResult
	for _, r := range results {
		if r.Arity == opts.CallArity {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		return results
	}
	return kept
}

func filterLookupKind(results []store.LookupResult, kind NameKind) []store.LookupResult {
	switch kind {
	case NameKindCallable:
		return filterOutTypes(results)
	case NameKindType:
		return filterToTypes(results)
	default:
		return results
	}
}

func filterReferenceKind(results []store.ReferenceResult, kind NameKind) []store.ReferenceResult {
	if kind == NameKindAny {
		return results
	}
	filtered := results[:0]
	for _, result := range results {
		isType := result.Kind == "typespec"
		if (kind == NameKindType) == isType {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

// injectedDelegatesTo returns the defdelegate facades in a __using__ quote (or
// in a helper body it dispatches to) that forward to module.function. The
// index does not list them: a def in a quote is the calling module's, not
// the quote's (see the parser). Module is the module whose __using__ holds
// the quote; the facade is in each module that uses it.
//
// The facades of every target are found in one pass over the __using__
// bodies, kept until the next write to the index (references ask on each
// request; a pass costs about as much as loadUsingEntries).
func (s *Server) injectedDelegatesTo(module, function string) []store.DelegateEntry {
	c := &s.index.injectedDelegates
	gen := s.store.WriteGeneration()
	c.mu.Lock()
	if c.byTarget != nil && c.gen == gen {
		found := c.byTarget[module+"\x00"+function]
		c.mu.Unlock()
		return found
	}
	c.mu.Unlock()

	byTarget := make(map[string][]store.DelegateEntry)
	type siteKey struct {
		path        string
		line, arity int
	}
	seen := make(map[siteKey]bool)
	for _, e := range s.loadUsingEntries() {
		for _, body := range e.entry.bodies() {
			for name, defs := range body.inlineDefs {
				for _, d := range defs {
					if d.kind != "defdelegate" || d.delegateTo == "" {
						continue
					}
					k := siteKey{e.entry.filePath, d.line, d.arity}
					if seen[k] {
						continue
					}
					seen[k] = true
					target := d.delegateTo + "\x00" + cmp.Or(d.delegateAs, name)
					byTarget[target] = append(byTarget[target], store.DelegateEntry{Module: e.module, Function: name, DelegateAs: d.delegateAs, FilePath: e.entry.filePath, Line: d.line, Arity: d.arity})
				}
			}
		}
	}
	c.mu.Lock()
	c.byTarget, c.gen = byTarget, gen
	c.mu.Unlock()
	return byTarget[module+"\x00"+function]
}

// injectedDelegateCache holds injectedDelegatesTo's map for one write
// generation of the index.
type injectedDelegateCache struct {
	mu       sync.Mutex
	gen      uint64
	byTarget map[string][]store.DelegateEntry
}
