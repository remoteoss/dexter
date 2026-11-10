package lsp

import (
	"cmp"
	"context"
	"runtime/debug"
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
	if len(results) == 0 {
		results = filterArity(filterLookupKind(s.lookupThroughUseOfWithFollow(module, function, opts.FollowDelegates), opts.Kind), opts)
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

	refResults, err := s.store.LookupReferences(module, function)
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
			transitive, lookupErr := s.store.LookupReferences(injector, function)
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
			refResults = append(refResults, s.findBareCallRefs(module, function)...)
		}
		if function != "" && opts.FollowDelegates {
			delegated, err := s.delegateReferences(ctx, module, function, moduleRefs)
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
func (s *Server) delegateReferences(ctx context.Context, module, function string, moduleRefs []store.ReferenceResult) ([]store.ReferenceResult, error) {
	delegates, _ := s.store.LookupDelegatesTo(module, function)
	injected := s.injectedDelegatesTo(module, function)
	if len(delegates) == 0 && len(injected) == 0 {
		return nil, nil
	}
	var out []store.ReferenceResult
	facades := make(map[string][]string) // facade function → facade modules
	// A facade in a __using__ quote is in each module that uses the quote's
	// module; the index records its bare calls there as calls of that module.
	for _, d := range injected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, store.ReferenceResult{FilePath: d.FilePath, Line: d.Line, Kind: siteDelegate})
		if refs, err := s.store.LookupReferences(d.Module, d.Function); err == nil {
			out = append(out, refs...)
		}
		users, _ := s.store.LookupReferences(d.Module, "")
		for _, u := range users {
			if u.Kind != "use" {
				continue
			}
			if user := s.store.LookupEnclosingModule(u.FilePath, u.Line); user != "" && user != d.Module {
				facades[d.Function] = append(facades[d.Function], user)
			}
		}
	}
	for _, d := range delegates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out = append(out, store.ReferenceResult{FilePath: d.FilePath, Line: d.Line, Kind: siteDelegate})
		if d.Module != module {
			facades[d.Function] = append(facades[d.Function], d.Module)
			out = append(out, s.findBareCallRefs(d.Module, d.Function)...)
			continue
		}
		if d.Function != function {
			if refs, err := s.store.LookupReferences(module, d.Function); err == nil {
				out = append(out, refs...)
			}
		}
		for _, u := range moduleRefs {
			if u.Kind != "use" {
				continue
			}
			if user := s.store.LookupEnclosingModule(u.FilePath, u.Line); user != "" && user != module {
				facades[d.Function] = append(facades[d.Function], user)
			}
		}
	}
	for fn, mods := range facades {
		if refs, err := s.store.LookupReferencesInModules(mods, fn); err == nil {
			out = append(out, refs...)
		}
	}
	return out, nil
}

// nameLocations filters references by kind, adds the declarations when
// asked, and lists each line once.
func (s *Server) nameLocations(module, function string, refResults []store.ReferenceResult, opts NameReferenceOptions) ([]NameLocation, error) {
	refResults = filterReferenceKind(refResults, opts.Kind)
	locations := make([]NameLocation, 0, len(refResults)+1)
	seen := make(map[nameLocationKey]struct{}, len(refResults)+1)
	for _, result := range refResults {
		if opts.ExcludeStdlib && s.isStdlibPath(result.FilePath) {
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
			declaration.IsDeclaration = true
			appendNameLocation(&locations, seen, declaration)
		}
	}
	return locations, nil
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
		path string
		line int
	}
	seen := make(map[siteKey]bool)
	for _, e := range s.loadUsingEntries() {
		for _, body := range e.entry.bodies() {
			for name, defs := range body.inlineDefs {
				for _, d := range defs {
					if d.kind != "defdelegate" || d.delegateTo == "" {
						continue
					}
					k := siteKey{e.entry.filePath, d.line}
					if seen[k] {
						continue
					}
					seen[k] = true
					target := d.delegateTo + "\x00" + cmp.Or(d.delegateAs, name)
					byTarget[target] = append(byTarget[target], store.DelegateEntry{Module: e.module, Function: name, DelegateAs: d.delegateAs, FilePath: e.entry.filePath, Line: d.line})
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
