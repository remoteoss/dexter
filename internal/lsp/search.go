package lsp

import (
	"strings"
	"unicode"

	"github.com/remoteoss/dexter/internal/store"
)

// SymbolSearchResult is one match of a symbol search: a function (one row for
// all its clauses in a file) or module from the index, or a member that the
// module's source does not define (injected by `use`, or generated).
type SymbolSearchResult struct {
	store.SymbolMatch
	// Member is set for an injected or generated function. Its location is
	// where definition finds it.
	Member *ModuleMember
}

// SymbolSearch is the answer to a symbol search.
type SymbolSearch struct {
	Results []SymbolSearchResult
	// More is true when there are more index rows than Results holds.
	More bool
	// Query is the query that matched: the one asked, with whitespace read as
	// a dot, or its CamelCase form when only that matched.
	Query string
}

// maxSearchMembers caps the injected and generated functions that one search
// lists. A compiled module can export hundreds (route helpers), and each one
// costs a definition lookup.
const maxSearchMembers = 100

// SearchNames finds symbols by a fuzzy name query, as the editor's workspace
// symbols and MCP's search do. Whitespace in the query works as a dot
// (`Accounts fetch` is `Accounts.fetch`), and a query that matches nothing is
// tried again in CamelCase (`employee_controller` as `EmployeeController`).
// A Module.function query also lists what `use` injects into that module and
// what its compiled module generates; the module part may be the end of the
// name (`Workers.Mailer.new`).
func (s *Server) SearchNames(query string, includeStdlib bool, limit int) (SymbolSearch, error) {
	query = strings.Join(strings.Fields(query), ".")
	var exclude []string
	if root := s.StdlibRoot(); !includeStdlib && root != "" {
		exclude = append(exclude, root)
	}
	search := func(q string) (SymbolSearch, error) {
		// The modules whose members the query names are looked up while the
		// index is searched: both scan the definitions, and neither waits
		// for the other.
		modulesCh := make(chan []string, 1)
		go func() { modulesCh <- s.memberModules(q) }()
		rows, more, err := s.store.SearchSymbolsGrouped(q, limit, exclude...)
		modules := <-modulesCh
		if err != nil {
			return SymbolSearch{}, err
		}
		out := SymbolSearch{Query: q, More: more}
		seen := make(map[string]bool, len(rows))
		for _, r := range rows {
			out.Results = append(out.Results, SymbolSearchResult{SymbolMatch: r})
			seen[r.Module+"\x00"+funcKey(r.Function, r.Arity)] = true
		}
		for _, m := range s.searchMembers(q, modules) {
			key := m.module + "\x00" + funcKey(m.member.Name, m.member.Arity)
			if seen[key] {
				continue
			}
			seen[key] = true
			member := m.member
			loc := store.SymbolMatch{CompletionResult: store.CompletionResult{
				Module: m.module, Function: member.Name, Arity: member.Arity, Kind: member.Kind,
			}, Clauses: 1}
			if member.Source == MemberInjected && member.FilePath != "" {
				// Where definition goes too: the def in the quote.
				loc.FilePath, loc.Line = member.FilePath, member.Line
			} else if locs, err := s.LookupName(m.module, member.Name, NameLookupOptions{External: true}); err == nil && len(locs) > 0 {
				best := locs[0]
				for _, l := range locs {
					if l.Arity == member.Arity {
						best = l
						break
					}
				}
				loc.FilePath, loc.Line = best.FilePath, best.Line
			} else if member.FilePath != "" {
				loc.FilePath, loc.Line = member.FilePath, member.Line
			}
			out.Results = append(out.Results, SymbolSearchResult{SymbolMatch: loc, Member: &member})
		}
		return out, nil
	}
	out, err := search(query)
	if err != nil || len(out.Results) > 0 {
		return out, err
	}
	if camel := camelizeQuery(query); camel != query {
		return search(camel)
	}
	return out, nil
}

type searchMember struct {
	module string
	member ModuleMember
}

// memberModules returns the modules that the module part of a
// Module.function query names: an exact module name or the end of one. A
// query without a module part names none: listing members for it would read
// the compiled module of every module in the workspace.
func (s *Server) memberModules(query string) []string {
	dot := strings.LastIndex(query, ".")
	if dot <= 0 {
		return nil
	}
	modulePart := query[:dot]
	if modulePart[0] < 'A' || modulePart[0] > 'Z' {
		return nil
	}
	if results, err := s.store.LookupModule(modulePart); err == nil && len(results) > 0 {
		return []string{modulePart}
	}
	const maxModules = 5
	modules, _ := s.store.ModuleNamesBySuffix(modulePart, maxModules)
	return modules
}

// searchMembers returns the injected and generated functions of modules
// (memberModules of the query) whose name starts with the function part of
// the query.
func (s *Server) searchMembers(query string, modules []string) []searchMember {
	prefix := query[strings.LastIndex(query, ".")+1:]
	var out []searchMember
	for _, module := range modules {
		for _, m := range s.ModuleMembers(module) {
			if !strings.HasPrefix(m.Name, prefix) {
				continue
			}
			if len(out) == maxSearchMembers {
				return out
			}
			out = append(out, searchMember{module: module, member: m})
		}
	}
	return out
}

// camelizeQuery turns snake_case segments into CamelCase (employee_controller
// to EmployeeController), so a query written like a file name finds the module.
// Only a segment with an underscore changes: the search ignores case, so a
// segment that only gets a capital letter would find nothing more.
func camelizeQuery(query string) string {
	segments := strings.Split(query, ".")
	changed := false
	for i, seg := range segments {
		if !strings.ContainsRune(seg, '_') {
			continue
		}
		var b strings.Builder
		upper := true
		for _, r := range seg {
			if r == '_' {
				upper = true
				continue
			}
			if upper {
				r = unicode.ToUpper(r)
				upper = false
			}
			b.WriteRune(r)
		}
		if b.String() != seg {
			segments[i] = b.String()
			changed = true
		}
	}
	if !changed {
		return query
	}
	return strings.Join(segments, ".")
}
