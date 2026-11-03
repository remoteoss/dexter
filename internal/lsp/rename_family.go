package lsp

import (
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

// renameFamilyInfo is what one function rename renames together.
//
// A function that is a callback of a behaviour in the project, or that
// implements one, is renamed with its whole family: the @callback (or
// @macrocallback), every implementation in the project, their @spec lines,
// the @optional_callbacks entry, the calls to all of them, and the calls
// through a variable in the behaviour's own file (`impl.name(...)`). The
// behaviour and its implementations must keep one name, or the project
// compiles with warnings that a callback is not implemented.
type renameFamilyInfo struct {
	name      string
	modules   []string // the requested module first, then the other members
	behaviour string   // the project behaviour, or ""
	callbacks []store.CallbackResult
	arities   map[int]bool // the callback arities
	// memberArities are the arities renamed for each member: the callback
	// arities, and the lower arities that default arguments of that
	// member's own definition give. See addDefaultArities.
	memberArities map[string]map[int]bool
}

// arityFilter is the arity set of each family member, for the arity check
// of each occurrence.
type arityFilter struct {
	byModule map[string]map[int]bool
	base     map[int]bool // the callback arities, for an occurrence of no known member
}

func (a *arityFilter) forModule(m string) map[int]bool {
	if set, ok := a.byModule[m]; ok {
		return set
	}
	return a.base
}

// renameFamily finds the family of module.name. It refuses a callback of a
// behaviour outside the project (a dependency, Elixir, OTP), and a family
// with an implementation outside the project: Dexter cannot change those.
// Arities: only callbacks of an arity that module.name has count; the rename
// of each member covers all arities of its name, as every function rename
// does, because default arguments make several arities of one definition.
func (s *Server) renameFamily(module, name string) (*renameFamilyInfo, error) {
	f := &renameFamilyInfo{name: name, modules: []string{module}, arities: map[int]bool{}}
	inProject := func(path string) bool { return !s.isStdlibPath(path) && !s.isDepsFile(path) }

	defs, _ := s.store.LookupFunction(module, name)
	defArities := map[int]bool{}
	for _, d := range defs {
		defArities[d.Arity] = true
	}

	if cbs, err := s.store.LookupCallbackDef(module, name); err == nil && len(cbs) > 0 {
		f.behaviour, f.callbacks = module, cbs
	} else {
		files := map[string]bool{}
		for _, d := range defs {
			files[d.FilePath] = true
		}
		for file := range files {
			behaviours, _ := s.store.LookupBehavioursForFile(file)
			for _, b := range behaviours {
				cbs, err := s.store.LookupCallbackDef(b, name)
				if err != nil {
					continue
				}
				var match []store.CallbackResult
				for _, cb := range cbs {
					if defArities[cb.Arity] {
						match = append(match, cb)
					}
				}
				if len(match) > 0 {
					f.behaviour, f.callbacks = b, match
					break
				}
			}
			if f.behaviour != "" {
				break
			}
		}
		if f.behaviour == "" {
			for _, d := range defs {
				if p := sourceText(d.FilePath); p != "" && hasImplBefore(p, d.Line) {
					return nil, s.renameRefusal(fmt.Sprintf(
						"Dexter: the rename was not applied, because %s.%s has @impl, but Dexter cannot find the behaviour it implements; Dexter cannot rename it. No file was changed.",
						module, name))
				}
			}
			return f, nil
		}
	}

	for _, cb := range f.callbacks {
		f.arities[cb.Arity] = true
		if !inProject(cb.FilePath) {
			return nil, s.renameRefusal(fmt.Sprintf(
				"Dexter: the rename was not applied, because %s/%d implements the %s behaviour from a dependency; Dexter cannot rename it. No file was changed.",
				name, cb.Arity, f.behaviour))
		}
	}
	if f.behaviour != module {
		f.modules = append(f.modules, f.behaviour)
	}

	// The implementations: modules that declare @behaviour or `use` the
	// behaviour, and, through a __using__ block that injects the
	// @behaviour, the modules that `use` such a module.
	seen := map[string]bool{f.behaviour: true}
	queue := []string{f.behaviour}
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		impls, err := s.store.LookupBehaviourImplementors(m)
		if err != nil {
			return nil, fmt.Errorf("cannot rename %s.%s: the index query failed (%v); nothing was renamed", module, name, err)
		}
		for _, impl := range impls {
			if seen[impl.Module] {
				continue
			}
			seen[impl.Module] = true
			if using, _ := s.store.LookupFunction(impl.Module, "__using__"); len(using) > 0 {
				queue = append(queue, impl.Module)
			}
			implDefs, _ := s.store.LookupFunction(impl.Module, name)
			member := false
			for _, d := range implDefs {
				if f.arities[d.Arity] {
					member = true
				}
			}
			if !member {
				continue
			}
			if !inProject(impl.FilePath) {
				return nil, s.renameRefusal(fmt.Sprintf(
					"Dexter: the rename was not applied, because %s, which implements %s.%s, is outside the project (in a dependency); Dexter cannot rename it. No file was changed.",
					impl.Module, f.behaviour, name))
			}
			if !slices.Contains(f.modules, impl.Module) {
				f.modules = append(f.modules, impl.Module)
			}
		}
	}
	if err := f.addDefaultArities(s); err != nil {
		return nil, err
	}
	return f, nil
}

// addDefaultArities adds to the family's arities the lower arities that
// default arguments give a member's definition of a callback arity:
// `def go(x, opts \\ [])` implements go/2 and also defines go/1, and its
// callers of go/1 must change with it. Another definition of the name, of
// an arity no callback has (`def go(a, b, c)`), stays out.
func (f *renameFamilyInfo) addDefaultArities(s *Server) error {
	callback := f.arities
	f.memberArities = make(map[string]map[int]bool, len(f.modules))
	for _, m := range f.modules {
		set := make(map[int]bool, len(callback))
		for a := range callback {
			set[a] = true
		}
		f.memberArities[m] = set
	}
	files := map[string]bool{}
	for _, m := range f.modules {
		defs, _ := s.store.LookupFunction(m, f.name)
		for _, d := range defs {
			files[d.FilePath] = true
		}
	}
	for file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		res := parser.TokenizeFull(data)
		toks := res.Tokens
		for i, t := range toks {
			if t.Kind != parser.TokIdent || i == 0 || parser.TokenText(data, t) != f.name {
				continue
			}
			switch toks[i-1].Kind {
			case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop:
			default:
				continue
			}
			member := extractEnclosingModuleFromTokens(data, toks, t.Line-1)
			if !slices.Contains(f.modules, member) {
				continue
			}
			lo, hi, known := tokenArity(data, toks, i)
			if !known || lo == hi {
				continue
			}
			covers := false
			for a := lo; a <= hi; a++ {
				covers = covers || callback[a]
			}
			if !covers {
				continue
			}
			for a := lo; a <= hi; a++ {
				if callback[a] {
					continue
				}
				// The added arity must not be a callback of another
				// behaviour of the same module: the definition would then
				// belong to two families, and one name cannot serve both.
				if other := s.otherCallbackOwner(file, f.behaviour, f.name, a); other != "" {
					return s.renameRefusal(fmt.Sprintf(
						"Dexter: the rename was not applied, because %s.%s has default arguments that also define %s/%d, a callback of the behaviour %s: the definition belongs to two callback families. No file was changed.",
						member, f.name, f.name, a, other))
				}
				f.memberArities[member][a] = true
			}
		}
	}
	return nil
}

// otherCallbackOwner returns a behaviour other than behaviour, declared or
// used in file, that has a callback name/arity, or "".
func (s *Server) otherCallbackOwner(file, behaviour, name string, arity int) string {
	behaviours, _ := s.store.LookupBehavioursForFile(file)
	for _, b := range behaviours {
		if b == behaviour {
			continue
		}
		cbs, _ := s.store.LookupCallbackDef(b, name)
		for _, cb := range cbs {
			if cb.Arity == arity {
				return b
			}
		}
	}
	return ""
}

// renameArities is the arity filter of the rename: nil for a function that
// is not in a callback family (every arity of its name is renamed, as
// default arguments make several), and for a family the arity set of each
// member.
func (f *renameFamilyInfo) renameArities() *arityFilter {
	if f.behaviour == "" {
		return nil
	}
	return &arityFilter{byModule: f.memberArities, base: f.arities}
}

// sourceText reads a file for a check that needs only its text.
func sourceText(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// addBehaviourSites adds the sites that only the behaviour has: the
// @callback lines, the @optional_callbacks entry, and the calls through a
// variable in the behaviour's own file, such as `adapter.name(...)`, of a
// callback arity.
func (f *renameFamilyInfo) addBehaviourSites(s *Server, source func(string) *parsedSource, add func(renameSite)) {
	if f.behaviour == "" {
		return
	}
	files := map[string]bool{}
	for _, cb := range f.callbacks {
		files[cb.FilePath] = true
		add(renameSite{module: f.behaviour, filePath: cb.FilePath, line: cb.Line, fromIndex: true, bareOK: true, kind: siteCallback})
	}
	for file := range files {
		p := source(file)
		if p == nil {
			continue
		}
		for _, line := range keywordListLines(p, "@optional_callbacks", f.name) {
			add(renameSite{module: f.behaviour, filePath: file, line: line, includeKeyword: true, kind: siteCallback})
		}
		for _, c := range variableCalls(p, f.name, f.arities) {
			add(renameSite{module: f.behaviour, filePath: file, line: c, dynamicOK: true, kind: siteCall})
		}
	}
}

// notifyDynamicCalls tells the requesting editor about calls through a
// variable outside the behaviour's file that can call the callback: Dexter
// cannot know which module the variable holds, so it does not rename them.
func (f *renameFamilyInfo) notifyDynamicCalls(s *Server, source func(string) *parsedSource) {
	if f.behaviour == "" {
		return
	}
	own := map[string]bool{}
	for _, cb := range f.callbacks {
		own[cb.FilePath] = true
	}
	refs, _ := s.store.LookupReferences(f.behaviour, "")
	files := map[string]bool{}
	for _, r := range refs {
		if !own[r.FilePath] && !s.isStdlibPath(r.FilePath) && !s.isDepsFile(r.FilePath) {
			files[r.FilePath] = true
		}
	}
	var found []string
	for file := range files {
		if p := source(file); p != nil {
			for _, line := range variableCalls(p, f.name, f.arities) {
				found = append(found, fmt.Sprintf("%s:%d", s.displayPath(file), line))
			}
		}
	}
	if len(found) == 0 {
		return
	}
	sort.Strings(found)
	s.renameNotify(notify.Warning, fmt.Sprintf(
		"Dexter: the rename did not change %d calls through a variable that can call the callback %s.%s (Dexter cannot know the module the variable holds): %s. Check them.",
		len(found), f.behaviour, f.name, notify.Summarize(found)))
}

// keywordListLines returns the lines of the `name:` keys in the keyword
// lists after head: `@optional_callbacks` or `defoverridable`.
func keywordListLines(p *parsedSource, head, name string) []int {
	var lines []int
	toks := p.tokens
	for i := 0; i < len(toks); i++ {
		if (toks[i].Kind != parser.TokAttr && toks[i].Kind != parser.TokIdent) || parser.TokenText(p.src, toks[i]) != head {
			continue
		}
		depth := 0
		last := parser.TokAttr
	list:
		for j := i + 1; j < len(toks); j++ {
			switch toks[j].Kind {
			case parser.TokOpenBracket:
				depth++
			case parser.TokCloseBracket:
				depth--
				if depth <= 0 {
					break list
				}
			case parser.TokIdent:
				// `@optional_callbacks name: 1, other: 2` (over several lines
				// after a comma) or a [ ] list.
				if depth <= 1 && j+1 < len(toks) && toks[j+1].Kind == parser.TokColon && parser.TokenText(p.src, toks[j]) == name {
					lines = append(lines, toks[j].Line)
				}
			case parser.TokEOL, parser.TokComment:
				if depth == 0 && last != parser.TokComma {
					break list
				}
				continue
			case parser.TokEOF:
				break list
			}
			last = toks[j].Kind
		}
	}
	return lines
}

// variableCalls returns the lines of `x.name(...)` and `@x.name(...)` calls
// with a variable or an attribute before the dot and a callback arity
// (one more argument when a pipe gives the first).
func variableCalls(p *parsedSource, name string, arities map[int]bool) []int {
	var lines []int
	toks := p.tokens
	for i := 2; i+1 < len(toks); i++ {
		t := toks[i]
		if t.Kind != parser.TokIdent || toks[i+1].Kind != parser.TokOpenParen || toks[i-1].Kind != parser.TokDot ||
			parser.TokenText(p.src, t) != name {
			continue
		}
		q := toks[i-2]
		if q.Kind != parser.TokIdent && q.Kind != parser.TokAttr {
			continue
		}
		if q.Kind == parser.TokIdent && parser.TokenText(p.src, q) == "__MODULE__" {
			continue
		}
		args := callArgs(toks, i+1)
		if i-3 >= 0 && toks[i-3].Kind == parser.TokPipe {
			args++
		}
		if arities[args] {
			lines = append(lines, t.Line)
		}
	}
	return lines
}

// callArgs counts the arguments of the call whose open parenthesis is
// toks[open].
func callArgs(toks []parser.Token, open int) int {
	depth, args, empty := 0, 0, true
	for j := open; j < len(toks); j++ {
		switch toks[j].Kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace, parser.TokOpenAngle, parser.TokDo, parser.TokFn:
			depth++
			if depth > 1 {
				empty = false
			}
			continue
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace, parser.TokCloseAngle, parser.TokEnd:
			depth--
			if depth == 0 {
				if empty {
					return 0
				}
				return args + 1
			}
		case parser.TokComma:
			if depth == 1 {
				args++
			}
		case parser.TokEOL, parser.TokComment:
			continue
		}
		empty = false
	}
	return args + 1
}
