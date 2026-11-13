package lsp

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

// ProtocolImpl is one implementation of a protocol: a defimpl block, or an
// `@derive` that makes the protocol generate one for a module.
type ProtocolImpl struct {
	Kind     string // "defimpl" or "derive"
	Module   string // for a derive: the module it derives for
	FilePath string // the defimpl line, the @derive line, or the `use` line
	Line     int
	// For a derive that a `use` brings in: the module whose __using__ holds
	// the @derive. Empty for a derive in the module's own source.
	Via string
	// Unsure is true for a derive whose module has no struct that Dexter can
	// see (no defstruct in its source or injected, and no compiled module):
	// Elixir ignores a @derive with no struct after it, so the
	// implementation exists only if a macro defines the struct.
	Unsure bool
	// Conditional is true for an Unsure derive whose @derive is in a branch
	// of a __using__ body (or reached through one), which the options of the
	// `use` decide.
	Conditional bool
}

// ProtocolImplementations returns the defimpl blocks and the derives of
// protocol, and whether protocol is a protocol the index knows. Derives come
// from `@derive` in a module's own source, and from `@derive` in the
// __using__ quote of a module that it uses, directly or through a chain of
// __using__ bodies.
func (s *Server) ProtocolImplementations(protocol string) ([]ProtocolImpl, bool) {
	mods, err := s.store.LookupModule(protocol)
	if err != nil {
		return nil, false
	}
	isProtocol := false
	var out []ProtocolImpl
	for _, r := range mods {
		switch r.Kind {
		case "defprotocol":
			isProtocol = true
		case "defimpl":
			out = append(out, ProtocolImpl{Kind: "defimpl", FilePath: r.FilePath, Line: r.Line})
		}
	}
	if !isProtocol {
		return nil, false
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].Line < out[j].Line
	})

	derived := make(map[string]bool)
	if derives, err := s.store.LookupDerives(protocol); err == nil {
		for _, d := range derives {
			if derived[d.Module] {
				continue
			}
			derived[d.Module] = true
			impl := ProtocolImpl{Kind: "derive", Module: d.Module, FilePath: d.FilePath, Line: d.Line}
			if keep, _ := s.keepDerive(protocol, &impl); keep {
				out = append(out, impl)
			}
		}
	}
	out = append(out, s.derivesThroughUse(protocol, derived)...)
	return out, true
}

// derivesThroughUse returns the modules that get a derive of protocol from a
// `use`. It reads every __using__ body, so it is for an explicit question
// about a protocol's implementations, never for a hot path.
func (s *Server) derivesThroughUse(protocol string, derived map[string]bool) []ProtocolImpl {
	entries := s.loadUsingEntries()
	seen := make(map[string]bool)
	conditional := make(map[string]bool)
	// dispatchDerives holds, for a module whose __using__ dispatches on an
	// atom (`use MyAppWeb, :schema`), the atoms whose body derives the
	// protocol, and whether surely. Its `use` sites are decided by their atom.
	dispatchDerives := make(map[string]map[string]bool)
	var direct []string
	for _, e := range entries {
		found, sure := bodyDerives(&e.entry.usingBody, protocol)
		atoms := make(map[string]bool)
		for atom, body := range e.entry.dispatch {
			if f, su := bodyDerives(body, protocol); f {
				atoms[atom] = su
			}
		}
		if len(atoms) > 0 {
			dispatchDerives[e.module] = atoms
		}
		if found || len(atoms) > 0 {
			direct = append(direct, e.module)
			seen[e.module] = true
			// An atom-dispatch body is chosen by the caller too.
			if !sure || e.entry.dispatch != nil {
				conditional[e.module] = true
			}
		}
	}
	if len(direct) == 0 {
		return nil
	}
	chain := closeOverUsingChain(entries, direct, seen)
	inChain := make(map[string]bool, len(chain))
	for _, m := range chain {
		inChain[m] = true
	}
	// A link of the chain derives for sure when it uses, outside a branch,
	// a module that derives for sure. Repeat until nothing changes.
	sure := make(map[string]bool, len(chain))
	for _, m := range direct {
		sure[m] = !conditional[m]
	}
	byModule := make(map[string]*usingCacheEntry, len(entries))
	for _, e := range entries {
		if inChain[e.module] {
			byModule[e.module] = e.entry
		}
	}
	for changed := true; changed; {
		changed = false
		for _, m := range chain {
			if sure[m] || byModule[m] == nil || byModule[m].dispatch != nil {
				continue
			}
			body := &byModule[m].usingBody
			for _, used := range body.graphUses() {
				if sure[used] && !slices.Contains(body.branchUses, used) {
					sure[m], changed = true, true
					break
				}
			}
		}
	}
	var out []ProtocolImpl
	atoms := useSiteAtoms{s: s}
	for _, via := range chain {
		sites, err := s.store.LookupUseSites(via)
		if err != nil {
			continue
		}
		for _, site := range sites {
			// A `use` in another __using__ quote is a link of the chain, not
			// a module that derives.
			if inChain[site.Module] || derived[site.Module] {
				continue
			}
			siteSure := sure[via]
			if byAtom, ok := dispatchDerives[via]; ok {
				su, derives := byAtom[atoms.at(site.FilePath, site.Line)]
				if !derives {
					if entry := byModule[via]; entry == nil || !bodyHasDerive(entry, protocol) {
						// The atom of this `use` picks a body without it.
						continue
					}
				}
				siteSure = derives && su
			}
			derived[site.Module] = true
			impl := ProtocolImpl{Kind: "derive", Module: site.Module, FilePath: site.FilePath, Line: site.Line, Via: via}
			if keep, compiled := s.keepDerive(protocol, &impl); keep {
				if !siteSure && !compiled {
					impl.Unsure, impl.Conditional = true, true
				}
				out = append(out, impl)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Module < out[j].Module })
	return out
}

// ProtocolFunctionImpls returns the definitions of a protocol function in the
// protocol's defimpl blocks, and whether the protocol declares the function.
// Functions in defprotocol and defimpl blocks share the protocol's module name
// in the index, so each definition is given to the nearest preceding block in
// its file.
func (s *Server) ProtocolFunctionImpls(protocol, function string) ([]store.LookupResult, bool) {
	mods, err := s.store.LookupModule(protocol)
	if err != nil {
		return nil, false
	}
	defs, err := s.store.LookupFunction(protocol, function)
	if err != nil {
		return nil, false
	}
	scopeKind := func(filePath string, line int) string {
		kind, scopeLine := "", -1
		for _, r := range mods {
			if r.FilePath == filePath && r.Line <= line && r.Line > scopeLine {
				kind, scopeLine = r.Kind, r.Line
			}
		}
		return kind
	}
	arities := make(map[int]bool)
	for _, d := range defs {
		if scopeKind(d.FilePath, d.Line) == "defprotocol" {
			arities[d.Arity] = true
		}
	}
	if len(arities) == 0 {
		return nil, false
	}
	var out []store.LookupResult
	for _, d := range defs {
		if arities[d.Arity] && scopeKind(d.FilePath, d.Line) == "defimpl" {
			out = append(out, d)
		}
	}
	return out, true
}

// keepDerive decides whether a @derive gives an implementation, as the
// compiler decides it. A compiled module that is not older than its source
// answers it: the compiler wrote the implementation module next to it, or
// did not. Otherwise only a struct defined after the @derive reads it: a
// defstruct in the module's source, or one that a later `use` injects. With
// neither, the derive is kept and marked Unsure.
func (s *Server) keepDerive(protocol string, impl *ProtocolImpl) (keep, compiled bool) {
	functions := s.generatedFunctionsForModule(impl.Module)
	if s.hasCurrentBeam(impl.Module) {
		entry, _ := s.generatedCache.get(impl.Module)
		implBeam := filepath.Join(filepath.Dir(entry.beamPath), "Elixir."+protocol+"."+impl.Module+".beam")
		_, err := os.Stat(implBeam)
		return err == nil, true
	}
	if rows, err := s.store.LookupFunction(impl.Module, "__struct__"); err == nil {
		for _, r := range rows {
			if r.FilePath == impl.FilePath && r.Line > impl.Line {
				return true, false
			}
		}
		if len(rows) > 0 {
			// Every struct in the source is before the @derive: ignored.
			return false, false
		}
	}
	for _, m := range s.injectedMembers(impl.Module) {
		if m.Name == "__struct__" && m.UseLine >= impl.Line {
			return true, false
		}
	}
	if len(functions) > 0 {
		// A compiled module older than the source: its struct, if any.
		for _, f := range functions {
			if f.Name == "__struct__" {
				return true, false
			}
		}
		return false, false
	}
	if entry, ok := s.generatedCache.get(impl.Module); ok && entry.beamPath != "" {
		// Compiled, and it has no struct.
		return false, false
	}
	impl.Unsure = true
	return true, false
}

// bodyDerives reports whether a __using__ body derives protocol, and
// whether surely (not in a branch that the caller's options decide).
func bodyDerives(body *usingBody, protocol string) (found, sure bool) {
	for _, p := range body.derives {
		switch p {
		case protocol:
			found, sure = true, true
		case conditionalDerivePrefix + protocol:
			found = true
		}
	}
	return found, sure
}

// useSiteAtoms reads the dispatch atom of `use` lines (`use MyAppWeb,
// :schema`), reading each file once.
type useSiteAtoms struct {
	s     *Server
	lines map[string][]string
}

func (u *useSiteAtoms) at(path string, line int) string {
	if u.lines == nil {
		u.lines = make(map[string][]string)
	}
	lines, ok := u.lines[path]
	if !ok {
		if text, _, read := u.s.readFileText(path); read {
			lines = strings.Split(text, "\n")
		}
		u.lines[path] = lines
	}
	if line < 1 || line > len(lines) {
		return ""
	}
	// The statement can go on over the next lines.
	end := min(line+3, len(lines))
	source := []byte(strings.Join(lines[line-1:end], "\n"))
	tokens := parser.Tokenize(source)
	n := len(tokens)
	for i, t := range tokens {
		if t.Kind != parser.TokUse {
			continue
		}
		_, k := tokCollectModuleName(source, tokens, n, tokNextSig(tokens, n, i+1))
		if k = tokNextSig(tokens, n, k); k < n && tokens[k].Kind == parser.TokComma {
			which, _ := tokCollectDispatchAtom(source, tokens, n, k+1)
			return which
		}
		return ""
	}
	return ""
}

func bodyHasDerive(entry *usingCacheEntry, protocol string) bool {
	found, _ := bodyDerives(&entry.usingBody, protocol)
	return found
}
