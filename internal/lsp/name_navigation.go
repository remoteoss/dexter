package lsp

import "github.com/remoteoss/dexter/internal/store"

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
}

type NameLookupOptions struct {
	Kind             NameKind
	FollowDelegates  bool
	External         bool
	FallbackToModule bool
	ExcludeStdlib    bool
}

type NameReferenceOptions struct {
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
	if len(results) == 0 {
		results = filterLookupKind(s.lookupThroughUseOfWithFollow(module, function, opts.FollowDelegates), opts.Kind)
	}
	if len(results) == 0 && opts.Kind != NameKindType {
		if generated, found := s.generatedSymbol(module, "", function); found && len(generated) > 0 {
			results = s.generatedDefinitionResults(module)
			if len(results) > 0 {
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
	return s.lookupLocations(results, opts.ExcludeStdlib), nil
}

// ReferenceNames finds references after a frontend has resolved a canonical
// module/function name. Cursor-specific alias and variable resolution stays in
// the LSP adapter.
func (s *Server) ReferenceNames(module, function string, opts NameReferenceOptions) ([]NameLocation, error) {
	injectors := append([]string(nil), opts.InjectorModules...)
	var injectorCh chan []string
	if function != "" && function != "__using__" && len(injectors) == 0 {
		injectorCh = make(chan []string, 1)
		go func() {
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
			if result.Kind == "use" {
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
			injectors = <-injectorCh
		}
		for _, injector := range injectors {
			transitive, lookupErr := s.store.LookupReferences(injector, function)
			if lookupErr != nil {
				continue
			}
			if opts.GeneratedProvider != "" {
				transitive = s.filterGeneratedProviderReferences(opts.GeneratedProvider, function, transitive)
			}
			refResults = append(refResults, transitive...)
		}
		if function != "" {
			refResults = append(refResults, s.findBareCallRefs(module, function)...)
		}
		if function != "" && opts.FollowDelegates {
			delegates, lookupErr := s.store.LookupDelegatesTo(module, function)
			if lookupErr == nil {
				for _, delegate := range delegates {
					delegateRefs, refErr := s.store.LookupReferences(delegate.Module, delegate.Function)
					if refErr == nil {
						refResults = append(refResults, delegateRefs...)
					}
					refResults = append(refResults, s.findBareCallRefs(delegate.Module, delegate.Function)...)
				}
			}
		}
	}

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
