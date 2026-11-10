package lsp

import (
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/remoteoss/dexter/internal/parser"
)

// Caller is the code that holds a reference: a function, or, for a reference
// outside every def (a `test "..." do` block, a `schema "x" do` block, the
// module body), that block or the module.
type Caller struct {
	Module   string
	Function string // empty when the reference is outside every def
	Arity    int
	Line     int    // line of the def, or of the block, or of the module
	Block    string // `test "name"` for a reference in a macro call block; empty otherwise
}

// Name renders the caller as Module.function/arity, Module test "name", or the
// module alone.
func (c Caller) Name() string {
	switch {
	case c.Function != "":
		return c.Module + "." + c.Function + "/" + strconv.Itoa(c.Arity)
	case c.Block != "":
		if c.Module == "" {
			return c.Block
		}
		return c.Module + " " + c.Block
	default:
		return c.Module
	}
}

// CallerResolver finds the caller of reference lines. It reads and tokenizes
// each file at most once, so one resolver serves one request.
type CallerResolver struct {
	s     *Server
	files map[string]*fileCallers
}

type lineSpan struct {
	start, end int // 1-based, inclusive
	label      string
}

type fileCallers struct {
	defs   []lineSpan // def bodies, outermost
	blocks []lineSpan // macro call blocks outside defs, outermost first
}

// NewCallerResolver returns a resolver for one request.
func (s *Server) NewCallerResolver() *CallerResolver {
	return &CallerResolver{s: s, files: make(map[string]*fileCallers)}
}

// CallerAt returns the caller of a reference at line (1-based) in filePath.
func (r *CallerResolver) CallerAt(filePath string, line int) Caller {
	fc := r.file(filePath)
	if fc != nil {
		for _, d := range fc.defs {
			if line >= d.start && line <= d.end {
				if mod, fn, arity, defLine, ok := r.s.store.LookupEnclosingFunction(filePath, line); ok && defLine >= d.start {
					return Caller{Module: mod, Function: fn, Arity: arity, Line: defLine}
				}
				break
			}
		}
		var block *lineSpan
		for i := range fc.blocks {
			b := &fc.blocks[i]
			if line >= b.start && line <= b.end && (block == nil || b.start >= block.start) {
				block = b
			}
		}
		if block != nil {
			return Caller{Module: r.s.store.LookupEnclosingModule(filePath, line), Line: block.start, Block: block.label}
		}
	} else if mod, fn, arity, defLine, ok := r.s.store.LookupEnclosingFunction(filePath, line); ok {
		// The file could not be read: the nearest def is the best guess.
		return Caller{Module: mod, Function: fn, Arity: arity, Line: defLine}
	}
	mod := r.s.store.LookupEnclosingModule(filePath, line)
	modLine := line
	if results, err := r.s.store.LookupModule(mod); err == nil {
		for _, m := range results {
			if m.FilePath == filePath && m.Line <= line {
				modLine = m.Line
			}
		}
	}
	return Caller{Module: mod, Line: modLine}
}

func (r *CallerResolver) file(filePath string) *fileCallers {
	if fc, ok := r.files[filePath]; ok {
		return fc
	}
	// The index lines are the lines on disk, so the spans come from the disk
	// too, not from an editor buffer with unsaved changes.
	fc := callerSpans.get(filePath)
	r.files[filePath] = fc
	return fc
}

// callerSpanCache keeps the spans of recently read files, checked by size and
// mtime, so that a repeated call hierarchy or references request does not
// tokenize the same files again.
type callerSpanCache struct {
	mu      sync.Mutex
	entries map[string]callerSpanEntry
}

type callerSpanEntry struct {
	size  int64
	mtime int64
	spans *fileCallers
}

const maxCallerSpanFiles = 2048

var callerSpans = &callerSpanCache{entries: make(map[string]callerSpanEntry)}

func (c *callerSpanCache) get(filePath string) *fileCallers {
	info, err := os.Stat(filePath)
	if err != nil {
		return nil
	}
	size, mtime := info.Size(), info.ModTime().UnixNano()
	c.mu.Lock()
	if e, ok := c.entries[filePath]; ok && e.size == size && e.mtime == mtime {
		c.mu.Unlock()
		return e.spans
	}
	c.mu.Unlock()
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	spans := scanCallerSpans(data)
	c.mu.Lock()
	if len(c.entries) >= maxCallerSpanFiles {
		c.entries = make(map[string]callerSpanEntry)
	}
	c.entries[filePath] = callerSpanEntry{size: size, mtime: mtime, spans: spans}
	c.mu.Unlock()
	return spans
}

// blockFlowKeywords open do-blocks that are control flow, not a named block.
var blockFlowKeywords = map[string]bool{
	"if": true, "unless": true, "case": true, "cond": true, "with": true, "for": true,
	"try": true, "receive": true, "quote": true,
}

// scanCallerSpans finds the def bodies, and the macro call blocks outside them
// (`test "x" do`, `describe "x" do`, `setup do`, `schema "x" do`).
func scanCallerSpans(source []byte) *fileCallers {
	tokens := parser.Tokenize(source)
	n := len(tokens)
	fc := &fileCallers{}
	lastLine := func(j int) int {
		if j <= 0 {
			return 1
		}
		if j > n {
			j = n
		}
		return tokens[j-1].Line
	}
	for i := 0; i < n; i++ {
		tok := tokens[i]
		switch tok.Kind {
		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop:
			end := blockEnd(tokens, i)
			fc.defs = append(fc.defs, lineSpan{start: tok.Line, end: lastLine(end)})
			if end > i {
				i = end - 1
			}
		case parser.TokIdent:
			prev := i - 1
			for prev >= 0 && tokens[prev].Kind == parser.TokComment {
				prev--
			}
			if prev >= 0 && tokens[prev].Kind != parser.TokEOL {
				continue
			}
			name := parser.TokenText(source, tok)
			if blockFlowKeywords[name] {
				continue
			}
			_, next, hasDo := parser.ScanForwardToMacroCallBlockDo(tokens, n, i+1)
			if !hasDo {
				continue
			}
			label := name
			if a := parser.NextSigToken(tokens, n, i+1); a < n {
				if tokens[a].Kind == parser.TokOpenParen {
					a = parser.NextSigToken(tokens, n, a+1)
				}
				if a < n && tokens[a].Kind == parser.TokString {
					label += " " + strings.TrimSpace(parser.TokenText(source, tokens[a]))
				}
			}
			depth := 1
			j := next
			for ; j < n && depth > 0; j++ {
				switch tokens[j].Kind {
				case parser.TokDo, parser.TokFn, parser.TokEnd:
					parser.TrackBlockDepth(tokens[j].Kind, &depth)
				}
			}
			fc.blocks = append(fc.blocks, lineSpan{start: tok.Line, end: lastLine(j), label: label})
			// Blocks nest (describe > test): keep walking inside this one.
		}
	}
	return fc
}

// bodyStart returns the index of the first token of the body of the
// definition at tokens[i]: after its `do`, or after the comma before `do:`.
func bodyStart(tokens []parser.Token, i int) int {
	brackets := 0
	for k := i + 1; k < len(tokens); k++ {
		switch tokens[k].Kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
			brackets++
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace:
			brackets--
		case parser.TokDo, parser.TokComma:
			if brackets <= 0 {
				return k + 1
			}
		case parser.TokEOL, parser.TokEOF:
			if brackets <= 0 && tokens[k-1].Kind != parser.TokWhen {
				return k
			}
		}
	}
	return len(tokens)
}

// blockEnd returns the index after the end of the definition that starts at
// tokens[i]: its do-block's `end`, the end of a `, do:` statement, or the end
// of the line for a bodiless head (`def f(a, b \\ 1)`).
func blockEnd(tokens []parser.Token, i int) int {
	n := len(tokens)
	brackets := 0
	last := tokens[i].Kind
	for k := i + 1; k < n; k++ {
		switch kind := tokens[k].Kind; kind {
		case parser.TokOpenParen, parser.TokOpenBracket, parser.TokOpenBrace:
			brackets++
		case parser.TokCloseParen, parser.TokCloseBracket, parser.TokCloseBrace:
			brackets--
		case parser.TokDo:
			if brackets <= 0 {
				depth := 1
				j := k + 1
				for ; j < n && depth > 0; j++ {
					switch tokens[j].Kind {
					case parser.TokDo, parser.TokFn, parser.TokEnd:
						parser.TrackBlockDepth(tokens[j].Kind, &depth)
					}
				}
				return j
			}
		case parser.TokComma:
			if brackets <= 0 {
				return skipToEndOfStatement(tokens, n, k)
			}
		case parser.TokEOL:
			if brackets <= 0 && last != parser.TokWhen {
				return k
			}
			continue
		case parser.TokComment:
			continue
		case parser.TokEOF:
			return k
		}
		last = tokens[k].Kind
	}
	return n
}

// Callee is a function that a function calls.
type Callee struct {
	Module   string
	Function string
	FilePath string // where the callee is defined, as definition finds it
	Line     int
	Kind     string
	Calls    []int // lines of the calls, in order
}

// CalleesOf lists what module.function calls, in the order of the first call.
// It reads every clause's body: the qualified calls the index holds, and the
// unqualified calls, which resolve as go-to-definition resolves them (the
// module's own functions, imports, use chains). Each callee is located with
// LookupName, so injected and generated functions count. A callee that does
// not resolve, or that is in the stdlib, is left out.
func (s *Server) CalleesOf(module, function string) ([]Callee, error) {
	defs, err := s.store.LookupFunction(module, function)
	if err != nil {
		return nil, err
	}
	if len(defs) == 0 {
		// A function that a `use` injects: its def in the quote.
		defs = s.lookupThroughUseOf(module, function)
	}
	if len(defs) == 0 {
		return nil, nil
	}
	type key struct{ module, function string }
	var order []key
	lines := make(map[key][]int)
	add := func(k key, line int) {
		if _, ok := lines[k]; !ok {
			order = append(order, k)
		}
		for _, l := range lines[k] {
			if l == line {
				return
			}
		}
		lines[k] = append(lines[k], line)
	}

	type fileInfo struct {
		text    string
		tf      *TokenizedFile
		aliases map[string]string
	}
	files := make(map[string]*fileInfo)
	seenClause := make(map[string]bool)
	for _, d := range defs {
		clauseKey := d.FilePath + "\x00" + strconv.Itoa(d.Line)
		if seenClause[clauseKey] || d.Kind == "defdelegate" {
			continue
		}
		seenClause[clauseKey] = true
		fi, ok := files[d.FilePath]
		if !ok {
			// Disk text: the index lines, which the calls are matched
			// against, are the lines on disk.
			data, readErr := os.ReadFile(d.FilePath)
			if readErr == nil {
				text := string(data)
				tf := NewTokenizedFile(text)
				fi = &fileInfo{text: text, tf: tf, aliases: tf.ExtractAliases()}
				s.mergeAliasesFromUseTokenized(tf, fi.aliases)
			}
			files[d.FilePath] = fi
		}
		if fi == nil {
			continue
		}
		tokens := fi.tf.tokens
		n := len(tokens)
		start := -1
		for i := 0; i < n; i++ {
			if tokens[i].Line == d.Line {
				switch tokens[i].Kind {
				case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop, parser.TokDefguard, parser.TokDefguardp:
					start = i
				}
				if start >= 0 {
					break
				}
			}
			if tokens[i].Line > d.Line {
				break
			}
		}
		if start < 0 {
			continue
		}
		end := blockEnd(tokens, start)
		endLine := d.Line
		if end > start && end <= n {
			endLine = tokens[end-1].Line
		}

		// Qualified calls (and the calls the parser resolved through imports)
		// are in the index.
		if refs, err := s.store.LookupRefsInRange(d.FilePath, d.Line, endLine); err == nil {
			for _, r := range refs {
				add(key{r.Module, r.Function}, r.Line)
			}
		}

		// Unqualified calls: `helper(x)`, `x |> helper`, and `helper x` at
		// the start of a statement. The head of the definition is skipped.
		bodyFrom := bodyStart(tokens, start)
		resolved := make(map[string]string)
		for k := bodyFrom; k < end && k+1 < n; k++ {
			tok := tokens[k]
			if tok.Kind != parser.TokIdent || !isBareCallAt(tokens, n, k) {
				continue
			}
			if k > 0 && tokens[k-1].Kind == parser.TokDot {
				continue
			}
			name := parser.TokenText(fi.tf.source, tok)
			if parser.IsElixirKeyword(name) {
				continue
			}
			mod, ok := resolved[name]
			if !ok {
				mod = s.resolveBareFunctionModule(d.FilePath, fi.text, fi.tf, tok.Line-1, name, fi.aliases)
				resolved[name] = mod
			}
			if mod != "" {
				add(key{mod, name}, tok.Line)
			}
		}
	}

	var out []Callee
	for _, k := range order {
		locs, err := s.LookupName(k.module, k.function, NameLookupOptions{ExcludeStdlib: true})
		if err != nil || len(locs) == 0 {
			continue
		}
		out = append(out, Callee{
			Module: k.module, Function: k.function,
			FilePath: locs[0].FilePath, Line: locs[0].Line, Kind: locs[0].Kind,
			Calls: lines[k],
		})
	}
	return out, nil
}

// isBareCallAt reports whether the identifier at k is surely a call: with
// its parentheses (`helper(x)`), after a pipe (`x |> helper`), or at the
// start of a statement with an argument after it (`helper x`). A variable
// has none of these forms.
func isBareCallAt(tokens []parser.Token, n, k int) bool {
	if k+1 < n && tokens[k+1].Kind == parser.TokOpenParen && tokens[k+1].Start == tokens[k].End {
		return true
	}
	prev := k - 1
	for prev >= 0 && (tokens[prev].Kind == parser.TokEOL || tokens[prev].Kind == parser.TokComment) {
		prev--
	}
	if prev >= 0 && tokens[prev].Kind == parser.TokPipe {
		return true
	}
	if k > 0 && tokens[k-1].Kind != parser.TokEOL && tokens[k-1].Kind != parser.TokComment {
		return false
	}
	if k+1 >= n {
		return false
	}
	switch tokens[k+1].Kind {
	case parser.TokIdent, parser.TokModule, parser.TokString, parser.TokHeredoc, parser.TokSigil,
		parser.TokCharLiteral, parser.TokNumber, parser.TokAtom, parser.TokOpenBracket, parser.TokOpenBrace,
		parser.TokPercent:
		return true
	}
	return false
}
