package lsp

import (
	"os"
	"sort"
	"strings"
	"sync"

	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/store"
)

// MemberSource says where a module member that its source does not define
// comes from.
type MemberSource int

const (
	// MemberInjected is a def inside the quote of a `use`d module's __using__.
	MemberInjected MemberSource = iota + 1
	// MemberGenerated is a public function that the compiled module exports
	// and the source index does not know (a macro made it).
	MemberGenerated
)

// ModuleMember is a public function or macro of a module that the module's
// source does not define. Definition (LookupName) and completion find these
// through the same use-chain and compiled-module paths; ModuleMembers lists
// them.
type ModuleMember struct {
	Name   string
	Arity  int
	Kind   string // def, defmacro, ...
	Params string // comma-joined parameter names, when known
	Source MemberSource

	// For MemberInjected: the module whose __using__ injects it, the line of
	// the `use` in the module's file, and where the def is in the quote.
	UsedModule string
	UseLine    int
	FilePath   string
	Line       int

	// For MemberGenerated: the compiled module, and whether that BEAM is older
	// than the source, so it can be out of date.
	BeamPath  string
	StaleBeam bool
	Doc       string // the compiled @doc, when the BEAM has one
}

// ModuleMembers returns the public functions and macros of module that its
// source does not define: those that `use` injects (followed through the use
// chain as LookupName does) and those that only the compiled module has. A
// function from both sources is listed once, as injected. The result is
// sorted by name, then arity.
func (s *Server) ModuleMembers(module string) []ModuleMember {
	defined := make(map[string]bool)
	// The complete list: ListModuleFunctions stops at 100 rows.
	if own, err := s.store.ModuleFunctionKeys(module); err == nil {
		for _, f := range own {
			defined[funcKey(f.Name, f.Arity)] = true
		}
	}
	var out []ModuleMember
	seen := make(map[string]bool)
	for _, m := range s.injectedMembers(module) {
		key := funcKey(m.Name, m.Arity)
		if defined[key] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}

	functions := s.generatedFunctionsForModule(module)
	if len(functions) > 0 {
		entry, _ := s.generatedCache.get(module)
		stale := entry.sourcePath != "" && entry.beamStamp.exists && entry.sourceStamp.exists &&
			entry.beamStamp.mtime < entry.sourceStamp.mtime
		for i, f := range functions {
			key := funcKey(f.Name, f.Arity)
			if defined[key] || seen[key] {
				continue
			}
			seen[key] = true
			doc := ""
			if f.DocLen > 0 {
				doc = generatedFunctionDoc(&entry, functions, i)
			}
			out = append(out, ModuleMember{
				Name: f.Name, Arity: f.Arity, Kind: f.Kind, Params: f.Params,
				Source: MemberGenerated, BeamPath: entry.beamPath, StaleBeam: stale, Doc: doc,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Arity < out[j].Arity
	})
	return out
}

// injectedMembers lists the public defs that the `use` calls in module's file
// inject, following transitive uses as lookupInUsingEntryForWithFollow does.
// A later `use` shadows an earlier one.
func (s *Server) injectedMembers(module string) []ModuleMember {
	path, mtime, ok := s.store.ModuleFile(module)
	if !ok {
		return nil
	}
	useCalls := s.fileUseCalls(path, mtime)
	if len(useCalls) == 0 {
		return nil
	}
	byKey := make(map[string]ModuleMember)
	for _, call := range useCalls {
		visited := make(map[string]bool)
		s.collectUsingDefs(call.Module, call.dispatchAtom(), call.Opts, visited, func(filePath string, name string, d inlineDef) {
			byKey[funcKey(name, d.arity)] = ModuleMember{
				Name: name, Arity: d.arity, Kind: d.kind, Params: d.params,
				Source: MemberInjected, UsedModule: call.Module, UseLine: call.line,
				FilePath: filePath, Line: d.line,
			}
		})
	}
	out := make([]ModuleMember, 0, len(byKey))
	for _, m := range byKey {
		out = append(out, m)
	}
	return out
}

func (s *Server) collectUsingDefs(moduleName, which string, consumerOpts map[string]string, visited map[string]bool, add func(filePath, name string, d inlineDef)) {
	visitKey := usingVisitKey(moduleName, which)
	if visited[visitKey] {
		return
	}
	visited[visitKey] = true
	entry := s.cachedUsing(moduleName)
	if entry == nil {
		return
	}
	body := entry.bodyFor(which)
	if body == nil {
		return
	}
	// Transitive uses first, so the defs of this body win, as in the lookup.
	for _, b := range body.optBindings {
		if b.kind != "use" {
			continue
		}
		mod, _ := bindingModule(b, consumerOpts)
		if mod != "" {
			s.collectUsingDefs(mod, "", bindingOpts(b, consumerOpts), visited, add)
		}
	}
	for _, call := range body.transCalls {
		s.collectUsingDefs(call.Module, call.dispatchAtom(), callOpts(call, consumerOpts), visited, add)
	}
	for _, mod := range body.transUses {
		if !body.hasTransCall(mod) {
			s.collectUsingDefs(mod, "", nil, visited, add)
		}
	}
	for name, defs := range body.inlineDefs {
		for _, d := range defs {
			if isPrivateKind(d.kind) {
				continue
			}
			add(entry.filePath, name, d)
		}
	}
}

func isPrivateKind(kind string) bool {
	switch kind {
	case "defp", "defmacrop", "defguardp":
		return true
	}
	return false
}

// addInjectedFunctionCompletions adds the public functions that the `use`
// calls in module's file inject, as ModuleMembers lists them.
func (s *Server) addInjectedFunctionCompletions(module, prefix string, seen map[string]bool, items *[]protocol.CompletionItem, inPipe bool) {
	members := s.injectedMembers(module)
	sort.Slice(members, func(i, j int) bool {
		if members[i].Name != members[j].Name {
			return members[i].Name < members[j].Name
		}
		return members[i].Arity < members[j].Arity
	})
	for _, m := range members {
		if !strings.HasPrefix(m.Name, prefix) {
			continue
		}
		key := funcKey(m.Name, m.Arity)
		if seen[key] {
			continue
		}
		seen[key] = true
		item := protocol.CompletionItem{
			Label:  m.Name,
			Kind:   kindToCompletionItemKind(m.Kind),
			Detail: m.Kind + " (injected by use " + m.UsedModule + ")",
			Data: map[string]interface{}{
				"filePath": m.FilePath,
				"line":     m.Line,
			},
		}
		applySnippet(&item, m.Name, m.Arity, m.Params, m.Kind, inPipe, s.snippetSupport)
		*items = append(*items, item)
	}
}

// hasCurrentBeam reports whether the generated-function cache holds a
// compiled module for module that is not older than its source. Its exports
// then include what `use` injects. It reads nothing.
func (s *Server) hasCurrentBeam(module string) bool {
	entry, ok := s.generatedCache.get(module)
	if !ok || entry.beamPath == "" {
		return false
	}
	stale := entry.sourcePath != "" && entry.beamStamp.exists && entry.sourceStamp.exists &&
		entry.beamStamp.mtime < entry.sourceStamp.mtime
	return !stale
}

// fileUseCalls returns the module-level `use` calls of a file whose
// indexed mtime is mtime. The calls of a file that no editor has open,
// also none, are kept while the index has the same mtime for it (the
// watcher reindexes a file that changes): completion asks on every
// keystroke.
func (s *Server) fileUseCalls(path string, mtime int64) []UseCall {
	docURI := string(pathToURI(path))
	if text, open := s.docs.GetIfOpen(docURI); open {
		// The session keeps the buffer's tokens; uses in a function body
		// (a quote) are not the module's.
		if tf := s.docs.GetTokenizedFile(docURI); tf != nil {
			return usesVisibleAt(tf.tokens, tf.ExtractUsesWithOpts(tf.ExtractAliases()), 0)
		}
		return ExtractModuleUsesWithOpts(text, ExtractAliases(text))
	}
	if mtime == store.BufferMtime {
		// Indexed from a buffer: every version has this mtime, so it does
		// not tell one from another and the calls are not kept.
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		return ExtractModuleUsesWithOpts(string(data), ExtractAliases(string(data)))
	}
	if calls, ok := s.index.useCalls.get(path, mtime); ok {
		return calls
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := string(data)
	calls := ExtractModuleUsesWithOpts(text, ExtractAliases(text))
	s.index.useCalls.put(path, mtime, calls)
	return calls
}

// useCallsCache keeps the module-level use calls of a file by its indexed
// mtime; see fileUseCalls. It is bounded: when it is full, it starts again.
type useCallsCache struct {
	mu      sync.Mutex
	entries map[string]useCallsEntry
}

type useCallsEntry struct {
	mtime int64
	calls []UseCall
}

const useCallsCacheSize = 1024

func (c *useCallsCache) get(path string, mtime int64) ([]UseCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[path]
	if !ok || e.mtime != mtime {
		return nil, false
	}
	return e.calls, true
}

func (c *useCallsCache) put(path string, mtime int64, calls []UseCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil || len(c.entries) >= useCallsCacheSize {
		c.entries = make(map[string]useCallsEntry)
	}
	c.entries[path] = useCallsEntry{mtime: mtime, calls: calls}
}
