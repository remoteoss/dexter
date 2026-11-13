package lsp

import (
	"maps"
	"slices"
	"strings"

	"github.com/remoteoss/dexter/internal/parser"
)

// varBinding is what a variable of a __using__ body (or of a helper that
// builds its quote) holds: the option optKey read from the options, with
// defaultMod when the options do not hold it. A variable bound to a module
// (`mod = MyApp.Mod`) has no optKey. defaultMod is unknownOpt for a value
// that Dexter cannot read.
type varBinding struct {
	optKey     string
	defaultMod string
	readEdits  []optEdit // the edits of the options where the key was read
}

// optScope is where option values are read: with aliases, and with the
// variables that hold a module (consts, `dflt = MyApp.Mod`). Macro code
// names such a variable (`Keyword.put(opts, :k, dflt)`); a quote, which is
// code for the caller, names it with `unquote(dflt)`.
type optScope struct {
	aliases map[string]string
	consts  map[string]varBinding
	lits    map[string]map[string]string // keyword literal variables, in macro code
	self    string                       // the module of the macro code, for __MODULE__; "" when not known
	// macroCode is set for the code of a macro or a helper (before its
	// quote, and in an unquote), not for the arguments of a use.
	macroCode bool
	inQuote   bool
}

// macro is the scope of the code in an unquote of a quote.
func (sc optScope) macro() optScope {
	sc.inQuote = false
	return sc
}

// literal is the keyword list that the argument arg names when it is a
// variable bound to a keyword literal in macro code.
func (sc optScope) literal(source []byte, tokens []parser.Token, arg [2]int) (map[string]string, bool) {
	if sc.inQuote || arg[1] != arg[0]+1 || tokens[arg[0]].Kind != parser.TokIdent {
		return nil, false
	}
	lit, ok := sc.lits[parser.TokenText(source, tokens[arg[0]])]
	return lit, ok
}

// constant is the module that the variable name holds, or "".
func (sc optScope) constant(name string) string {
	if v, ok := sc.consts[name]; ok && v.optKey == "" && v.defaultMod != unknownOpt {
		return v.defaultMod
	}
	return ""
}

func (b *optBinder) macroScope(aliases map[string]string) optScope {
	return optScope{aliases: aliases, consts: b.varToOpt, lits: b.lits, self: b.self, macroCode: true}
}

func (b *optBinder) quoteScope(aliases map[string]string) optScope {
	return optScope{aliases: aliases, consts: b.varToOpt, self: b.self, macroCode: true, inQuote: true}
}

// unknownBinding is a variable whose value Dexter cannot read.
var unknownBinding = varBinding{defaultMod: unknownOpt}

// optBinder reads the statements of a __using__ body, or of a helper that
// builds its quote, that bind options variables (vars) and the variables
// that read one option (varToOpt). The __using__ body and the helper use the
// same rules.
type optBinder struct {
	source   []byte
	tokens   []parser.Token
	n        int
	resolve  func(string) string // resolves an alias; keeps unknownOpt
	self     string              // the module of the macro code: __MODULE__
	vars     optVars
	varToOpt map[string]varBinding
	// lits are the variables bound to a keyword literal (`extra = [key:
	// Mod]`), read in macro scope.
	lits map[string]map[string]string
	// saved holds the variables at the start of each open block: a match
	// in a block (an if, a case, a fn) does not change them after it.
	saved []optBinderVars
}

type optBinderVars struct {
	vars     optVars
	varToOpt map[string]varBinding
	lits     map[string]map[string]string
}

// block follows a token that opens (do, fn) or closes (end) a block.
func (b *optBinder) block(kind parser.TokenKind) {
	switch kind {
	case parser.TokDo, parser.TokFn:
		b.saved = append(b.saved, optBinderVars{maps.Clone(b.vars), maps.Clone(b.varToOpt), maps.Clone(b.lits)})
	case parser.TokEnd:
		if len(b.saved) == 0 {
			return
		}
		last := b.saved[len(b.saved)-1]
		b.saved = b.saved[:len(b.saved)-1]
		// In place: the callers hold the maps.
		clear(b.vars)
		maps.Copy(b.vars, last.vars)
		clear(b.varToOpt)
		maps.Copy(b.varToOpt, last.varToOpt)
		b.lits = last.lits
	}
}

func newOptBinder(source []byte, tokens []parser.Token, self string, resolve func(string) string) *optBinder {
	return &optBinder{source: source, tokens: tokens, n: len(tokens), resolve: resolve, self: self, vars: optVars{}, varToOpt: make(map[string]varBinding), lits: make(map[string]map[string]string)}
}

// resolveValue resolves a module that macro code names: an alias, or
// __MODULE__, the module that defines the macro.
func (b *optBinder) resolveValue(name string) string {
	if mod, ok := selfModuleRef(name, b.self); ok {
		return mod
	}
	return b.resolve(name)
}

// selfModuleRef is the module that `__MODULE__` or `__MODULE__.Sub` names
// in the code of module self. ok is false for another name; mod is
// unknownOpt when self is not known.
func selfModuleRef(name, self string) (mod string, ok bool) {
	rest, found := strings.CutPrefix(name, "__MODULE__")
	if !found || (rest != "" && rest[0] != '.') {
		return "", false
	}
	if self == "" {
		return unknownOpt, true
	}
	return self + rest, true
}

func (b *optBinder) nextSig(i int) int {
	return tokNextSig(b.tokens, b.n, i)
}

func (b *optBinder) text(i int) string {
	return parser.TokenText(b.source, b.tokens[i])
}

func (b *optBinder) isKeyword(i int) bool {
	return i+2 < b.n && b.tokens[i].Kind == parser.TokModule && b.text(i) == "Keyword" && b.tokens[i+1].Kind == parser.TokDot
}

// isAssign reports whether tokens[i] is the `=` of a match.
func (b *optBinder) isAssign(i int) bool {
	return i < b.n && b.tokens[i].Kind == parser.TokOther && b.text(i) == "="
}

// firstArgVar is the variable that `Keyword.f(opts, ...)` at tokens[k]
// reads, or "".
func (b *optBinder) firstArgVar(k int) string {
	tokens, n := b.tokens, b.n
	if a := b.nextSig(k + 4); k+3 < n && tokens[k+3].Kind == parser.TokOpenParen && a < n && tokens[a].Kind == parser.TokIdent {
		if c := b.nextSig(a + 1); c < n && (tokens[c].Kind == parser.TokComma || tokens[c].Kind == parser.TokCloseParen) {
			return b.text(a)
		}
	}
	return ""
}

// orDefault reads `|| Default` at tokens[end]: the default (aliases
// resolved; unknownOpt for one that is not a module alone, such as
// `build()`) and the token after it. With no `||`, def is "".
func (b *optBinder) orDefault(end int) (def string, after int) {
	tokens, n := b.tokens, b.n
	isBar := func(i int) bool {
		return i < n && tokens[i].Kind == parser.TokOther && b.text(i) == "|"
	}
	if !isBar(end) || !isBar(end+1) || tokens[end+1].Start != tokens[end].End {
		return "", end
	}
	// `||` is two tokens.
	m := b.nextSig(end + 2)
	name, after := "", m
	if m < n && tokens[m].Kind == parser.TokModule {
		name, after = tokCollectModuleName(b.source, tokens, n, m)
	} else if m < n && tokens[m].Kind == parser.TokAtom {
		if name = atomModuleName(b.source, tokens, m); name != "" {
			after = m + 1
		}
	} else if m < n && tokens[m].Kind == parser.TokAttr {
		if name = attrModule(b.source, tokens, m, nil); name != "" {
			after = m + 1
		}
	}
	if name != "" && (after >= n || tokens[after].Kind == parser.TokEOL || tokens[after].Kind == parser.TokEOF ||
		tokens[after].Kind == parser.TokComment || tokens[after].Kind == parser.TokCloseParen) {
		return b.resolveValue(name), after
	}
	end = exprEnd(tokens, n, m)
	// A variable bound to a module before.
	if m < n && tokens[m].Kind == parser.TokIdent && end == m+1 {
		if mod := b.macroScope(nil).constant(b.text(m)); mod != "" {
			return mod, end
		}
	}
	return unknownOpt, end
}

// exprEnd is the token after the expression that starts at tokens[i]: the
// end of the line, a comma, or a closing bracket, outside brackets and
// blocks.
func exprEnd(tokens []parser.Token, n, i int) int {
	depth := 0
	for ; i < n; i++ {
		switch tokens[i].Kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace, parser.TokDo, parser.TokFn:
			depth++
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace, parser.TokEnd:
			if depth == 0 {
				return i
			}
			depth--
		case parser.TokEOL, parser.TokEOF, parser.TokComma, parser.TokComment:
			if depth == 0 {
				return i
			}
		}
	}
	return n
}

// accessRead reads `opts[:key]` or `opts[:key] || Default` at tokens[k], up
// to the end of the line or a `)`: the key, the default (aliases resolved;
// "" for none), and the token after.
func (b *optBinder) accessRead(k int) (key, def string, end int, ok bool) {
	tokens, n := b.tokens, b.n
	if k+3 >= n || tokens[k].Kind != parser.TokIdent || tokens[k+1].Kind != parser.TokOpenBracket || tokens[k+1].Start != tokens[k].End ||
		tokens[k+2].Kind != parser.TokAtom || tokens[k+3].Kind != parser.TokCloseBracket {
		return "", "", 0, false
	}
	key = atomKeyName(b.source, tokens, k+2)
	if key == "" {
		return "", "", 0, false
	}
	def, end = b.orDefault(k + 4)
	end = skipComments(tokens, n, end)
	if end < n && tokens[end].Kind != parser.TokEOL && tokens[end].Kind != parser.TokEOF && tokens[end].Kind != parser.TokCloseParen {
		return "", "", 0, false
	}
	return key, def, end, true
}

// scanKeywordCall reads `Keyword.f(opts, :key [, Default])` at tokens[i]:
// the function name, the key, the default module (aliases not resolved;
// unknownOpt for a default that Dexter cannot read, such as a call), and the
// token after. The key is "" when the second argument is not an atom.
func (b *optBinder) scanKeywordCall(i int) (funcName, atomKey, defaultMod string, end int) {
	return b.scanKeywordCallIn(i, false)
}

// scanKeywordCallIn is scanKeywordCall; piped is set for a call that a pipe
// gives the options to (`opts |> Keyword.get(:key, Default)`).
func (b *optBinder) scanKeywordCallIn(i int, piped bool) (funcName, atomKey, defaultMod string, end int) {
	tokens, n := b.tokens, b.n
	if i+3 >= n || !b.isKeyword(i) || tokens[i+2].Kind != parser.TokIdent {
		return "", "", "", i
	}
	funcName = b.text(i + 2)
	open := b.nextSig(i + 3)
	if open >= n || tokens[open].Kind != parser.TokOpenParen {
		return "", "", "", i
	}
	args, after := splitCallArgs(tokens, n, open)
	if after < 0 {
		return funcName, "", "", skipToEndOfStatement(tokens, n, open)
	}
	if piped {
		args = append([][2]int{{}}, args...)
	}
	if len(args) < 2 || args[1][1] != args[1][0]+1 {
		return funcName, "", "", after
	}
	atomKey = atomKeyName(b.source, tokens, args[1][0])
	if len(args) < 3 {
		return funcName, atomKey, "", after
	}
	d := args[2]
	if d[1] == d[0]+1 {
		if mod := atomModuleName(b.source, tokens, d[0]); mod != "" {
			return funcName, atomKey, mod, after
		}
		if tokens[d[0]].Kind == parser.TokAttr {
			if mod := attrModule(b.source, tokens, d[0], nil); mod != "" {
				return funcName, atomKey, mod, after
			}
		}
		// A variable bound to a module before: `dflt = Mod`.
		if tokens[d[0]].Kind == parser.TokIdent {
			if v, ok := b.varToOpt[b.text(d[0])]; ok && v.optKey == "" && v.defaultMod != "" {
				return funcName, atomKey, v.defaultMod, after
			}
		}
	}
	if mod, modEnd := tokCollectModuleName(b.source, tokens, n, d[0]); mod != "" && modEnd == d[1] {
		return funcName, atomKey, mod, after
	}
	// Any other default (`Mod.pick()`, `build()`, a variable, a function):
	// Dexter cannot read it.
	return funcName, atomKey, unknownOpt, after
}

// keywordRead is the binding of a variable that a Keyword call reads from the
// options variable src (see scanKeywordCall); known is false for a call that
// is not a read of one key.
func (b *optBinder) keywordRead(funcName, atomKey, defaultMod, src string) (varBinding, bool) {
	if atomKey == "" || src == "" {
		return varBinding{}, false
	}
	v := varBinding{optKey: atomKey, readEdits: slices.Clone(b.vars[src])}
	switch funcName {
	case "get", "pop", "pop!":
		v.defaultMod = b.resolveValue(defaultMod)
	case "get_lazy", "pop_lazy":
		// The function gives the default.
		v.defaultMod = unknownOpt
	case "fetch!":
	default:
		return varBinding{}, false
	}
	return v, true
}

// unquotedBinding is the binding of the module in `unquote(...)`, whose open
// parenthesis is tokens[open]: a variable bound before, or a read of an
// option (`Keyword.get(opts, :key, Mod)`, `opts[:key] || Mod`). Anything
// else, such as a parameter or a call, is a module that Dexter cannot read.
// ok is false when the parenthesis does not close.
func (b *optBinder) unquotedBinding(open int) (varBinding, bool) {
	tokens, n := b.tokens, b.n
	k := b.nextSig(open + 1)
	closing := matchingBracket(tokens, n, open)
	if k >= n || closing < 0 {
		return varBinding{}, false
	}
	if tokens[k].Kind == parser.TokIdent && b.nextSig(k+1) == closing {
		if v, ok := b.varToOpt[b.text(k)]; ok {
			return v, true
		}
		return unknownBinding, true
	}
	if b.isKeyword(k) {
		funcName, atomKey, defaultMod, end := b.scanKeywordCall(k)
		if v, known := b.keywordRead(funcName, atomKey, defaultMod, b.firstArgVar(k)); known && funcName != "pop" && funcName != "pop!" && funcName != "pop_lazy" {
			if v.defaultMod == "" {
				v.defaultMod, end = b.orDefault(end)
			}
			if b.nextSig(end) == closing {
				return v, true
			}
		}
	}
	if key, def, end, ok := b.accessRead(k); ok && end == closing {
		return varBinding{optKey: key, defaultMod: def, readEdits: slices.Clone(b.vars[b.text(k)])}, true
	}
	return unknownBinding, true
}

// assignment reads `name = value` at tokens[i] (the name). top is false for
// a statement in a block (an if, a fn): a match there does not change the
// variable after the block, so a value that Dexter cannot read changes
// nothing. It returns the token to go on from, the modules that the edits put
// (edges of the use-chain graph), and handled false when the walk must go on
// into the value, which can hold a quote.
func (b *optBinder) assignment(i int, top bool, fileAliases map[string]string) (end int, mods []string, handled bool) {
	tokens, n := b.tokens, b.n
	name := b.text(i)
	j := b.nextSig(i + 1)
	if !b.isAssign(j) {
		return i, nil, false
	}
	k := b.nextSig(j + 1)
	delete(b.lits, name)
	// A keyword literal: `extra = [key: Mod]`.
	if k < n && tokens[k].Kind == parser.TokOpenBracket {
		if closing := matchingBracket(tokens, n, k); closing > 0 {
			if e := skipComments(tokens, n, closing+1); (e >= n || tokens[e].Kind == parser.TokEOL || tokens[e].Kind == parser.TokEOF) &&
				keywordLiteral(b.source, tokens, n, [2]int{k, closing + 1}) {
				b.lits[name] = collectKeywordOpts(b.source, tokens, closing, k+1, b.macroScope(fileAliases))
				b.varToOpt[name] = unknownBinding
				b.vars[name] = []optEdit{{op: "unknown"}}
				return closing + 1, nil, true
			}
		}
	}
	// An edit of an options variable: Keyword.put/put_new/delete/drop/take/
	// merge, also piped, or `opts ++ [k: Mod]`.
	if src, edits, mods, end, ok := readKeywordEdits(b.source, tokens, n, k, b.macroScope(fileAliases)); ok {
		b.vars[name] = b.vars.derive(src, edits)
		delete(b.varToOpt, name)
		return end, mods, true
	}
	// A read: Keyword.{get,pop,fetch!,...}(opts, :key, Default), with the
	// edits the options have here.
	if b.isKeyword(k) {
		funcName, atomKey, defaultMod, end := b.scanKeywordCall(k)
		v, known := b.keywordRead(funcName, atomKey, defaultMod, b.firstArgVar(k))
		if known && v.defaultMod == "" {
			// Keyword.get(opts, :key) || Default
			v.defaultMod, end = b.orDefault(end)
		}
		if e := skipComments(tokens, n, end); !known || (e < n && tokens[e].Kind != parser.TokEOL && tokens[e].Kind != parser.TokEOF) {
			// Not a read that Dexter knows (`Keyword.keys(opts)`, or one
			// that a pipe goes on from).
			v = unknownBinding
		}
		b.varToOpt[name] = v
		b.vars[name] = []optEdit{{op: "unknown"}}
		return end, nil, true
	}
	// opts |> Keyword.get(:key, Default): a read too.
	if k < n && tokens[k].Kind == parser.TokIdent {
		if call := nextPipe(tokens, n, k+1); call > 0 && b.isKeyword(call) {
			funcName, atomKey, defaultMod, end := b.scanKeywordCallIn(call, true)
			v, known := b.keywordRead(funcName, atomKey, defaultMod, b.text(k))
			if e := skipComments(tokens, n, end); !known || funcName == "pop" || funcName == "pop!" || funcName == "pop_lazy" ||
				(e < n && tokens[e].Kind != parser.TokEOL && tokens[e].Kind != parser.TokEOF) {
				v = unknownBinding
			}
			b.varToOpt[name] = v
			b.vars[name] = []optEdit{{op: "unknown"}}
			return end, nil, true
		}
	}
	// opts[:key], or opts[:key] || Default: a read, as with Keyword.get.
	if key, def, end, ok := b.accessRead(k); ok {
		b.varToOpt[name] = varBinding{optKey: key, defaultMod: def, readEdits: slices.Clone(b.vars[b.text(k)])}
		b.vars[name] = []optEdit{{op: "unknown"}}
		return end, nil, true
	}
	// A module alone.
	if k < n && tokens[k].Kind == parser.TokModule {
		if mod, endK := tokCollectModuleName(b.source, tokens, n, k); mod != "" {
			if peek := skipComments(tokens, n, endK); peek >= n || tokens[peek].Kind == parser.TokEOL || tokens[peek].Kind == parser.TokEOF {
				b.varToOpt[name] = varBinding{defaultMod: b.resolveValue(mod)}
				b.vars[name] = []optEdit{{op: "unknown"}}
				return endK, nil, true
			}
		}
	}
	// Any other value (`if ...`, `case ...`, a call): Dexter cannot read it,
	// as options or as a module.
	if top {
		b.varToOpt[name] = unknownBinding
		b.vars[name] = []optEdit{{op: "unknown"}}
	}
	return i, nil, false
}

// tupleAssignment reads a match of a tuple at tokens[i] (the `{`):
// `{mod, opts} = Keyword.pop(opts, :key, Default)` and
// `{:ok, mod} = Keyword.fetch(opts, :key)`. top is as for assignment. It
// returns the token to go on from, and handled false when the walk must go
// on from i+1.
func (b *optBinder) tupleAssignment(i int, top bool) (end int, handled bool) {
	tokens, n := b.tokens, b.n
	closing := matchingBracket(tokens, n, i)
	if closing < 0 || !b.isAssign(b.nextSig(closing+1)) {
		return i, false
	}
	// The variables of a two-element tuple; "" for one that is not a
	// variable.
	elems, _ := splitBracketElems(tokens, n, i)
	varAt := func(e [2]int) string {
		if e[1] == e[0]+1 && tokens[e[0]].Kind == parser.TokIdent {
			return b.text(e[0])
		}
		return ""
	}
	var first, rest string
	okTuple := false
	if len(elems) == 2 {
		first, rest = varAt(elems[0]), varAt(elems[1])
		if elems[0][1] == elems[0][0]+1 && tokens[elems[0][0]].Kind == parser.TokAtom && b.text(elems[0][0]) == ":ok" {
			first, rest, okTuple = rest, "", true
		}
	}
	kw := b.nextSig(b.nextSig(closing+1) + 1)
	if first != "" && b.isKeyword(kw) {
		funcName, atomKey, defaultMod, endJ := b.scanKeywordCall(kw)
		src := b.firstArgVar(kw)
		switch {
		case okTuple && funcName == "fetch":
			funcName = "fetch!"
		case okTuple || funcName == "get" || funcName == "get_lazy" || funcName == "fetch!":
			// Not a tuple of a read and the options left.
			funcName = ""
		}
		if e := skipComments(tokens, n, endJ); e < n && tokens[e].Kind != parser.TokEOL && tokens[e].Kind != parser.TokEOF {
			funcName = ""
		}
		if v, ok := b.keywordRead(funcName, atomKey, defaultMod, src); ok {
			b.varToOpt[first] = v
			b.vars[first] = []optEdit{{op: "unknown"}}
			if rest != "" {
				// {mod, opts} = Keyword.pop(opts, :key): the options in the
				// second element no longer have the key.
				b.vars[rest] = b.vars.derive(src, []optEdit{{op: "delete", key: atomKey}})
				delete(b.varToOpt, rest)
			}
			return endJ, true
		}
	}
	if top {
		// A tuple that Dexter cannot read.
		for _, e := range elems {
			if v := varAt(e); v != "" {
				b.varToOpt[v] = unknownBinding
				b.vars[v] = []optEdit{{op: "unknown"}}
			}
		}
	}
	return i, false
}

// splitBracketElems returns the token ranges [start, end) of the elements of
// the bracket (a tuple or a list) that opens at tokens[open], and the token
// after its close (-1 when it does not close).
func splitBracketElems(tokens []parser.Token, n, open int) ([][2]int, int) {
	var elems [][2]int
	depth := 0
	start := -1
	for j := open + 1; j < n; j++ {
		k := tokens[j].Kind
		if k == parser.TokEOL || k == parser.TokComment {
			continue
		}
		switch k {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace, parser.TokDo, parser.TokFn:
			if start < 0 {
				start = j
			}
			depth++
			continue
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace, parser.TokEnd:
			if depth == 0 {
				if start >= 0 {
					elems = append(elems, [2]int{start, lastSigBefore(tokens, j) + 1})
				}
				return elems, j + 1
			}
			depth--
			continue
		case parser.TokComma:
			if depth == 0 {
				if start >= 0 {
					elems = append(elems, [2]int{start, lastSigBefore(tokens, j) + 1})
				}
				start = -1
				continue
			}
		}
		if start < 0 {
			start = j
		}
	}
	return nil, -1
}
