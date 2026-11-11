package lsp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/parser"
)

// indexStaleness collects the files that show that the index does not
// describe the files as they are now. A rename plans from the index: a site
// in a file that changed since it was indexed can be on another line now, and
// a file the index does not have yet (the new path of a move) has sites the
// rename cannot see. Either way the plan would be wrong, so the rename brings
// those files into the index and plans again (see retryWithFreshIndex).
type indexStaleness struct {
	missing []string             // listed by the index, but gone or unreadable
	moved   []string             // a buffer line the index lists lacks the token
	disk    map[string]diskStamp // read from disk: compared with the stored mtime
}

func (st *indexStaleness) read(path string, r renameRead) {
	if !r.fromDisk {
		return
	}
	if st.disk == nil {
		st.disk = make(map[string]diskStamp)
	}
	st.disk[path] = r.stamp
}

// indexBehindError is a plan that the index could not support: the files
// that changed on disk since they were indexed, or are gone (disk), and the
// requesting editor's buffers whose lines do not match the index (buffers).
type indexBehindError struct {
	disk    []string
	buffers []string
}

func (e *indexBehindError) Error() string {
	return fmt.Sprintf("the index is behind %d files", len(e.disk)+len(e.buffers))
}

func (e *indexBehindError) paths() []string {
	return append(append([]string(nil), e.disk...), e.buffers...)
}

// checkIndexCurrent returns an *indexBehindError when a file the rename reads
// is not as the index has it. One query reads the stored mtimes of every
// file; when every file is current, that query is the whole cost.
func (s *Server) checkIndexCurrent(st *indexStaleness) error {
	stale := append([]string(nil), st.missing...)
	if len(st.disk) > 0 {
		paths := make([]string, 0, len(st.disk))
		for p := range st.disk {
			paths = append(paths, p)
		}
		stored, err := s.store.FileMtimes(paths)
		if err != nil {
			return fmt.Errorf("cannot rename: the index query failed (%v); nothing was renamed", err)
		}
		for _, p := range paths {
			if m, ok := stored[p]; !ok || m != st.disk[p].mtime {
				stale = append(stale, p)
			}
		}
	}
	if len(stale) == 0 && len(st.moved) == 0 {
		return nil
	}
	return &indexBehindError{disk: stale, buffers: append([]string(nil), st.moved...)}
}

// renameStaleLimit is how many files that are behind in the index a rename
// brings into the index itself. More than that is a large change that the
// watcher is still indexing (a branch switch); the rename then asks the user
// to wait for it. Tests lower it.
var renameStaleLimit = 200

// retryWithFreshIndex runs plan, and when the index is behind the files it
// reads, indexes exactly those files again and runs plan one more time. A
// user who saves a file and renames at once, before the watcher indexed the
// save, gets the rename instead of a refusal. Only a state that a reindex
// cannot fix is refused: a buffer of the requesting editor with unsaved
// lines that do not match the index, or a change too large to index here.
// The caller holds renameSerial.
func (s *Server) retryWithFreshIndex(ctx context.Context, plan func() (*WorkspaceEdit, error)) (*WorkspaceEdit, error) {
	edit, err := plan()
	var behind *indexBehindError
	if !errors.As(err, &behind) {
		return edit, err
	}
	paths := behind.paths()
	if len(paths) > renameStaleLimit {
		return nil, s.renameRefusal(fmt.Sprintf(
			"Dexter: the rename was not applied, because %d files changed since Dexter indexed them; Dexter is probably still indexing a large change, such as a branch switch. No file was changed; wait for the index, then rename again.",
			len(paths)))
	}
	unexplained, err := s.flushForMoves(ctx, behind.disk)
	if err != nil {
		return nil, err
	}
	if ierr := s.reindexForRename(paths); ierr != nil {
		return nil, ierr
	}
	if len(unexplained) > 0 {
		// The watcher had nothing about these files: they can be deleted,
		// or moved by a program whose events are still on the way. The
		// rename stops once; the index no longer has them, so the next
		// try plans from the updated index.
		return nil, s.renameRefusal(fmt.Sprintf(
			"Dexter: the rename was not applied, because %s is gone, and the file watcher has not reported it. No file was changed; if it was moved, rename again in a moment.",
			s.summarize(unexplained)))
	}
	edit, err = plan()
	if !errors.As(err, &behind) {
		return edit, err
	}
	if len(behind.buffers) > 0 {
		return nil, s.renameRefusal(fmt.Sprintf(
			"Dexter: the rename was not applied, because %s changed in this editor since it was saved, and the index has the saved lines. No file was changed; save it, then rename again.",
			s.summarize(behind.buffers)))
	}
	return nil, s.renameRefusal(fmt.Sprintf(
		"Dexter: the rename was not applied, because the index is behind the files: %s changed since Dexter indexed it, or is gone. No file was changed; try the rename again in a moment.",
		s.summarize(behind.disk)))
}

// renameFlushWait bounds the wait for the watcher in flushForMoves.
var renameFlushWait = 2 * time.Second

// flushForMoves waits for the watcher when a file the rename plans from is
// gone and no earlier reply asked an editor to move it. That is what a move by
// another program (git mv, git checkout, a file manager) looks like before the
// watcher has indexed the new path: removing the old path and planning on
// would miss the call sites at the new path, and the project would not
// compile. After the wait the index has the new path. The common case, with
// no gone file, does not wait.
//
// It returns the gone files that the watcher did not report: the index still
// has them after the wait.
func (s *Server) flushForMoves(ctx context.Context, paths []string) ([]string, error) {
	if s.index.flushEvents == nil {
		return nil, nil
	}
	var gone []string
	for _, p := range paths {
		if _, recorded := s.index.clientMoves[p]; recorded {
			continue
		}
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			gone = append(gone, p)
		}
	}
	if len(gone) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, renameFlushWait)
	defer cancel()
	if err := s.index.flushEvents(ctx); err != nil {
		return nil, s.renameRefusal("Dexter: the rename was not applied, because Dexter is still indexing a change. No file was changed; wait for the index, then rename again.")
	}
	var unexplained []string
	for _, p := range gone {
		if has, err := s.store.HasPath(p); err != nil || has {
			unexplained = append(unexplained, p)
		}
	}
	return unexplained, nil
}

// renameRefusal queues msg for the requesting editor and returns it as the
// error of the rename.
func (s *Server) renameRefusal(msg string) error {
	s.renameNotify(notify.Error, msg)
	return errors.New(strings.TrimPrefix(msg, "Dexter: "))
}

// summarize lists paths relative to the project, the first one and how many
// more.
func (s *Server) summarize(paths []string) string {
	shown := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			shown = append(shown, s.displayPath(p))
		}
	}
	sort.Strings(shown)
	return notify.Summarize(shown)
}

// reindexForRename brings paths into the index at once, as the watcher does
// for one file: a file on disk is indexed again, and a file that is gone is
// removed. A file that the requesting editor moved (a rename operation in an
// earlier reply) also has its new path indexed. It waits for the workspace
// index lock up to renameIndexWait: a long pass holds it.
func (s *Server) reindexForRename(paths []string) error {
	deadline := time.Now().Add(renameIndexWait)
	for !s.index.reindexing.TryLock() {
		if time.Now().After(deadline) {
			return s.renameRefusal("Dexter: the rename was not applied, because Dexter is still indexing a change. No file was changed; wait for the index, then rename again.")
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer s.index.reindexing.Unlock()
	s.index.writes.RLock()
	defer s.index.writes.RUnlock()
	if s.index.unavailable {
		return s.renameRefusal("Dexter: the rename was not applied, because the index cannot be used. No file was changed.")
	}
	var gone []string
	reindex := func(p string) {
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			gone = append(gone, p)
			return
		}
		// A failure is recorded and reported by indexOneFileLocked; the
		// plan that follows sees the file as still behind.
		_ = s.indexOneFileLocked(p)
	}
	for _, p := range paths {
		reindex(p)
		if to, ok := s.index.takeClientMove(p); ok {
			reindex(to)
		}
	}
	if len(gone) > 0 {
		if err := s.store.RemoveFilesContext(s.index.work, gone); err != nil {
			return fmt.Errorf("cannot rename: the index could not be updated (%v); nothing was renamed", err)
		}
		for _, p := range gone {
			s.index.failures.removed(p)
		}
	}
	s.index.reportFileFailures()
	return nil
}

// siteFile is one file of a function rename, with what siteColumns needs to
// resolve a qualified call on one of its lines.
type siteFile struct {
	path    string
	lines   []string
	modules []string // the modules of the renamed function (a callback family can have several)
	name    string   // the renamed function
	tf      *TokenizedFile
	// code holds the byte offsets of the identifier tokens with the
	// renamed name, in code and in #{} interpolations; see codeColumns.
	code map[int]bool
	// call holds the offsets, of those, where the identifier is a call, a
	// capture, or a definition's name, not a variable; see isCallToken.
	call       map[int]bool
	lineStarts []int
	// arities, when set, are the only arities to rename (a callback
	// family); tokenIndex maps the offset of a name token to its index in
	// the main token stream, for arityOK.
	arities    *arityFilter
	tokenIndex map[int]int
	// typeName holds the offsets of the name tokens in a typespec that are
	// types: all but the name in a @spec or @callback head.
	typeName map[int]bool
}

// ensureTokens tokenizes the file once, on the first line that needs it.
func (sf *siteFile) ensureTokens() {
	name := sf.name
	if sf.tf != nil {
		return
	}
	src := []byte(strings.Join(sf.lines, "\n"))
	res := parser.TokenizeFull(src)
	sf.tf = NewTokenizedFileFromCache(res.Tokens, src, res.LineStarts, res.Interp)
	sf.lineStarts = res.LineStarts
	sf.code = make(map[int]bool)
	sf.call = make(map[int]bool)
	sf.tokenIndex = make(map[int]int)
	sf.typeName = make(map[int]bool)
	toks, n := res.Tokens, len(res.Tokens)
	for i := 0; i < n; i++ {
		kind := toks[i].Kind
		if kind != parser.TokAttrType && kind != parser.TokAttrSpec && kind != parser.TokAttrCallback {
			continue
		}
		end := parser.ScanTypespecEnd(src, toks, n, i)
		for j := i + 1; j <= end && j < n; j++ {
			t := toks[j]
			head := j == i+1 && kind != parser.TokAttrType
			if !head && t.Kind == parser.TokIdent && t.End-t.Start == len(name) && string(src[t.Start:t.End]) == name {
				sf.typeName[t.Start] = true
			}
		}
		i = end
	}
	for k, toks := range [][]parser.Token{res.Tokens, res.Interp} {
		for i, t := range toks {
			if t.Kind == parser.TokIdent && t.End-t.Start == len(name) && string(src[t.Start:t.End]) == name {
				sf.code[t.Start] = true
				if k == 0 {
					sf.tokenIndex[t.Start] = i
				}
				if isCallToken(src, toks, i, k == 1) {
					sf.call[t.Start] = true
				}
			}
		}
	}
}

// codeColumns keeps the columns of cols on line (1-based) where the token is
// code: not in a string, a heredoc, a sigil, a charlist, or a comment. Code in
// a #{} interpolation counts.
func (sf *siteFile) codeColumns(line int, cols []int) []int {
	sf.ensureTokens()
	if line-1 >= len(sf.lineStarts) {
		return nil
	}
	base := sf.lineStarts[line-1]
	out := cols[:0:0]
	for _, c := range cols {
		if sf.code[base+c] {
			out = append(out, c)
		}
	}
	return out
}

// siteColumns returns the columns on lineText where token is the renamed
// function: module.token, with module resolved through the aliases in scope.
// A line can also call a function of the same name in another module
// (`{MyApp.Alpha.foo(1), MyApp.Facade.foo(2)}`); those calls stay.
//
//   - A qualified call (`Mod.foo`, `&Mod.foo/1`, `|> Mod.foo()`) changes when
//     Mod resolves to the module.
//   - An unqualified one (`foo(`, `&foo/1`, `|> foo`, a def head) changes when
//     the site allows it (renameSite.bareOK): the line is in the module, or
//     the file imports it.
//   - `x.foo` with a variable or an expression before the dot is a field,
//     and stays.
//
// The common case costs nothing more than before: on a line that the index
// lists, a single occurrence is the one the index resolved.
func (s *Server) siteColumns(sf *siteFile, site renameSite, lineText, token string) []int {
	cols := s.siteColumnsAnyArity(sf, site, lineText, token)
	if len(cols) > 0 && (site.kind == siteSpec || site.kind == siteTypespec || site.kind == "typespec") {
		// A type of the function's name is not the function: in a
		// typespec, only the name of a @spec or @callback head is.
		sf.ensureTokens()
		kept := cols[:0:0]
		for _, c := range cols {
			if !sf.typeName[sf.lineStarts[site.line-1]+c] {
				kept = append(kept, c)
			}
		}
		cols = kept
	}
	if site.localArities != nil && len(cols) > 0 {
		sf.ensureTokens()
		kept := cols[:0:0]
		for _, c := range cols {
			if q, qualified := qualifierBefore(lineText, c); qualified && q != "" {
				kept = append(kept, c)
				continue
			}
			if i, ok := sf.tokenIndex[sf.lineStarts[site.line-1]+c]; ok {
				if lo, hi, known := tokenArity(sf.tf.source, sf.tf.tokens, i); known && !anyArityIn(site.localArities, lo, hi) {
					kept = append(kept, c)
				}
			}
		}
		cols = kept
	}
	if site.ownArities != nil && len(cols) > 0 {
		sf.ensureTokens()
		kept := cols[:0:0]
		for _, c := range cols {
			if _, qualified := qualifierBefore(lineText, c); !qualified {
				if i, ok := sf.tokenIndex[sf.lineStarts[site.line-1]+c]; ok {
					if lo, hi, known := tokenArity(sf.tf.source, sf.tf.tokens, i); known && !anyArityIn(site.ownArities, lo, hi) {
						continue
					}
				}
			}
			kept = append(kept, c)
		}
		cols = kept
	}
	if sf.arities == nil || len(cols) == 0 {
		return cols
	}
	sf.ensureTokens()
	out := cols[:0:0]
	for _, c := range cols {
		// The arities of the member that the occurrence names: the module
		// of its qualifier, or, unqualified, the member whose collection
		// added the line (its own module, or the module the file imports).
		module := site.module
		if q, qualified := qualifierBefore(lineText, c); qualified && q != "" {
			module = s.qualifierModule(sf, site.line, q)
		}
		if sf.arityOK(sf.lineStarts[site.line-1]+c, sf.arities.forModule(module)) {
			out = append(out, c)
		}
	}
	return out
}

// arityOK reports whether the name token at offset has one of sf.arities: a
// definition or a @spec head (with default arguments, a range of arities), a
// call (one more argument when a pipe gives the first), a capture `&f/2`, or
// a keyword key `f: 2`. An occurrence whose arity Dexter cannot read (in a
// #{} interpolation, a call without parentheses) counts.
func (sf *siteFile) arityOK(offset int, arities map[int]bool) bool {
	i, ok := sf.tokenIndex[offset]
	if !ok {
		return true
	}
	toks, src := sf.tf.tokens, sf.tf.source
	lo, hi, known := tokenArity(src, toks, i)
	if !known {
		return true
	}
	for a := range arities {
		if a >= lo && a <= hi {
			return true
		}
	}
	return false
}

// tokenArity reads the arity at the name token toks[i]; see arityOK. lo and
// hi differ for a head with default arguments.
func tokenArity(src []byte, toks []parser.Token, i int) (lo, hi int, known bool) {
	next := func(j int) int {
		for j < len(toks) && (toks[j].Kind == parser.TokEOL || toks[j].Kind == parser.TokComment) {
			j++
		}
		return j
	}
	prev := i - 1
	head := false
	if prev >= 0 {
		switch toks[prev].Kind {
		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate,
			parser.TokAttrSpec, parser.TokAttrCallback:
			head = true
		}
	}
	// A capture (`&f/2`, `&Mod.f/2`, `&(Mod.f/2)`) is read with the calls,
	// by parser.CallArity.
	j := i + 1
	if j < len(toks) && toks[j].Kind == parser.TokColon {
		// A keyword key: `f: 2` in only:, except:, @optional_callbacks,
		// defoverridable.
		k := next(j + 1)
		if k < len(toks) && toks[k].Kind == parser.TokNumber {
			if n, err := strconv.Atoi(parser.TokenText(src, toks[k])); err == nil {
				return n, n, true
			}
		}
		return 0, 0, false
	}
	if !head {
		// Counted as the parser counts it for the index (parser.CallArity),
		// so references and the index agree on which calls are which.
		if n := parser.CallArity(src, toks, i); n >= 0 {
			return n, n, true
		}
		return 0, 0, false
	}
	if j >= len(toks) || toks[j].Kind != parser.TokOpenParen {
		return 0, 0, true
	}
	// A head: its parameters, with default arguments a range, counted as
	// the index counts them.
	n, defaults, _, _ := parser.CollectParams(src, toks, len(toks), j)
	return n - defaults, n, true
}

// siteColumnsAnyArity is siteColumns without the arity check.
func (s *Server) siteColumnsAnyArity(sf *siteFile, site renameSite, lineText, token string) []int {
	if site.includeKeyword {
		return findAllTokenColumns(lineText, token)
	}
	cols := findFunctionTokenColumns(lineText, token)
	if len(cols) == 0 {
		return cols
	}
	if site.fromIndex && len(cols) == 1 {
		// The occurrence that the index resolved; it is checked without
		// tokenizing the file, because the index can attribute a call to
		// the wrong module (a call in a module that `use`s another, or a
		// call through a variable). A qualifier that is a member module, or
		// a short form of one, is accepted at once.
		q, qualified := qualifierBefore(lineText, cols[0])
		switch {
		case !qualified:
			if site.bareOK {
				return cols
			}
			return nil
		case q == "":
			if site.dynamicOK {
				return cols
			}
			return nil
		}
		for _, m := range sf.modules {
			if m == q || strings.HasSuffix(m, "."+q) {
				return cols
			}
		}
		if s.qualifierIs(sf, site.line, q) {
			return cols
		}
		return nil
	}
	cols = sf.codeColumns(site.line, cols)
	out := make([]int, 0, len(cols))
	for _, col := range cols {
		q, qualified := qualifierBefore(lineText, col)
		switch {
		case !qualified:
			// A variable can have the function's name; only a call, a
			// capture, or a definition's name is the function.
			if site.bareOK && sf.call[sf.lineStarts[site.line-1]+col] {
				out = append(out, col)
			}
		case q != "" && s.qualifierIs(sf, site.line, q):
			out = append(out, col)
		case q == "" && site.dynamicOK:
			out = append(out, col)
		}
	}
	return out
}

// qualifierBefore returns the module expression before the token at col
// (`MyApp.Alpha` in `MyApp.Alpha.foo`). qualified is false when no dot comes
// before the token. q is "" when what comes before the dot is not a module:
// a variable, a call result, or an Erlang module.
func qualifierBefore(line string, col int) (q string, qualified bool) {
	if col == 0 || line[col-1] != '.' {
		return "", false
	}
	j := col - 1
	for j > 0 {
		c := line[j-1]
		if c == '_' || c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			j--
			continue
		}
		break
	}
	q = line[j : col-1]
	if j > 0 && line[j-1] == ':' {
		return "", true // :erlang_mod.foo
	}
	if q == "" || strings.HasSuffix(q, ".") {
		return "", true
	}
	if strings.HasPrefix(q, "__MODULE__") || (q[0] >= 'A' && q[0] <= 'Z') {
		return q, true
	}
	return "", true
}

// qualifierModule returns the member module that the module expression q on
// line (1-based) names, or "". A member's name, or the only member whose
// name ends in q, needs no alias resolution.
func (s *Server) qualifierModule(sf *siteFile, line int, q string) string {
	match := ""
	for _, m := range sf.modules {
		if m == q {
			return m
		}
		if strings.HasSuffix(m, "."+q) {
			if match != "" {
				match = ""
				break
			}
			match = m
		}
	}
	if match != "" {
		return match
	}
	sf.ensureTokens()
	var resolved string
	if rest, ok := strings.CutPrefix(q, "__MODULE__"); ok {
		resolved = extractEnclosingModuleFromTokens(sf.tf.source, sf.tf.tokens, line-1) + rest
	} else {
		aliases := sf.tf.ExtractAliasesInScope(line - 1)
		s.mergeAliasesFromUseTokenized(sf.tf, aliases)
		resolved = resolveModule(q, aliases)
	}
	if slices.Contains(sf.modules, resolved) {
		return resolved
	}
	return ""
}

// qualifierIs reports whether the module expression q on line (1-based) of
// the file names sf.module.
func (s *Server) qualifierIs(sf *siteFile, line int, q string) bool {
	in := func(m string) bool { return slices.Contains(sf.modules, m) }
	if in(q) {
		return true
	}
	sf.ensureTokens()
	if rest, ok := strings.CutPrefix(q, "__MODULE__"); ok {
		enclosing := extractEnclosingModuleFromTokens(sf.tf.source, sf.tf.tokens, line-1)
		return enclosing != "" && in(enclosing+rest)
	}
	aliases := sf.tf.ExtractAliasesInScope(line - 1)
	s.mergeAliasesFromUseTokenized(sf.tf, aliases)
	return in(resolveModule(q, aliases))
}

// parsedSource is a file's text and tokens, read once for a rename plan.
type parsedSource struct {
	text        string
	src         []byte
	tokens      []parser.Token
	interp      []parser.Token
	interpSpans []parser.Span
	lineStarts  []int

	modules  []string // the module at each line; see moduleAt
	typespec []bool   // the lines in a typespec; see inTypespec
}

// bareCallArities returns, for each line, the arities of the unqualified
// occurrences of name: a call, a capture, a pipe target, or a keyword key,
// not a definition's own name. An occurrence whose arity cannot be read
// gives -1. A call inside #{} is read from its interpolation.
func (p *parsedSource) bareCallArities(name string) map[int][]int {
	out := make(map[int][]int)
	add := func(line, a int) {
		for _, have := range out[line] {
			if have == a {
				return
			}
		}
		out[line] = append(out[line], a)
	}
	for i, t := range p.tokens {
		if t.Kind != parser.TokIdent || parser.TokenText(p.src, t) != name || (i > 0 && p.tokens[i-1].Kind == parser.TokDot) {
			continue
		}
		if i > 0 && isHeadKeyword(p.tokens[i-1].Kind) {
			// A definition's own name is not a call of it. A line with only
			// a head gets an empty list: it has no call.
			if _, seen := out[t.Line]; !seen {
				out[t.Line] = []int{}
			}
			continue
		}
		lo, hi, known := tokenArity(p.src, p.tokens, i)
		if !known {
			add(t.Line, -1)
			continue
		}
		for a := lo; a <= hi; a++ {
			add(t.Line, a)
		}
	}
	interpArities := parser.InterpArities{Source: p.src, Spans: p.interpSpans}
	for i, t := range p.interp {
		if t.Kind != parser.TokIdent || parser.TokenText(p.src, t) != name || (i > 0 && p.interp[i-1].Kind == parser.TokDot) {
			continue
		}
		add(t.Line, interpArities.Arity(t))
	}
	return out
}

// isHeadKeyword reports whether a token kind starts a definition head, whose
// next name is the defined function.
func isHeadKeyword(k parser.TokenKind) bool {
	switch k {
	case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
		parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate,
		parser.TokAttrSpec, parser.TokAttrCallback:
		return true
	}
	return false
}

// moduleAt is extractEnclosingModuleFromTokens for the 1-based line, from one
// pass over the tokens for all the lines of the file.
func (p *parsedSource) moduleAt(line int) string {
	if p.modules == nil {
		p.modules = moduleLines(p.src, p.tokens)
	}
	if line < 0 || line >= len(p.modules) {
		if len(p.modules) == 0 {
			return ""
		}
		return p.modules[len(p.modules)-1]
	}
	return p.modules[line]
}

// inTypespec is TokenizedFile.InTypespec for the 1-based line, from one pass
// over the tokens for all the lines of the file.
func (p *parsedSource) inTypespec(line int) bool {
	if p.typespec == nil {
		n := len(p.tokens)
		last := 1
		if n > 0 {
			last = p.tokens[n-1].Line
		}
		p.typespec = make([]bool, last+2)
		for i := 0; i < n; i++ {
			switch p.tokens[i].Kind {
			case parser.TokAttrType, parser.TokAttrSpec, parser.TokAttrCallback:
				end := parser.ScanTypespecEnd(p.src, p.tokens, n, i)
				endLine := last + 1
				if end < n {
					endLine = p.tokens[end].Line
				}
				for l := p.tokens[i].Line; l <= endLine && l < len(p.typespec); l++ {
					p.typespec[l] = true
				}
				if end > i {
					i = end
				}
			}
		}
	}
	return line >= 0 && line < len(p.typespec) && p.typespec[line]
}

// moduleLines returns, for each 1-based line, the module that
// extractEnclosingModuleFromTokens gives for it.
func moduleLines(source []byte, tokens []parser.Token) []string {
	n := len(tokens)
	last := 1
	if n > 0 {
		last = tokens[n-1].Line
	}
	out := make([]string, last+2)
	type moduleFrame struct {
		name  string
		depth int
	}
	var stack []moduleFrame
	depth := 0
	top := func() string {
		if len(stack) > 0 {
			return stack[len(stack)-1].name
		}
		return ""
	}
	cur := 0 // lines before cur are filled
	fill := func(upTo int) {
		for ; cur < upTo && cur < len(out); cur++ {
			out[cur] = top()
		}
	}
	for i := 0; i < n; i++ {
		tok := tokens[i]
		// The lines before this token have the state of the tokens before it.
		fill(tok.Line)
		switch tok.Kind {
		case parser.TokDo, parser.TokFn:
			parser.TrackBlockDepth(tok.Kind, &depth)
		case parser.TokEnd:
			prevDepth := depth
			parser.TrackBlockDepth(tok.Kind, &depth)
			if len(stack) > 0 && stack[len(stack)-1].depth == prevDepth {
				stack = stack[:len(stack)-1]
			}
		case parser.TokDefmodule, parser.TokDefprotocol, parser.TokDefimpl:
			name, nextPos, hasDo := tokParseModuleDef(source, tokens, i+1, top())
			if name == "" {
				continue
			}
			if hasDo {
				depth++
				stack = append(stack, moduleFrame{name: name, depth: depth})
			}
			i = nextPos - 1
		}
	}
	fill(len(out))
	return out
}

// selfCallLines returns the lines (1-based) with a `__MODULE__.name` call in
// code, in #{} interpolations too.
func selfCallLines(p *parsedSource, name string) []int {
	var lines []int
	var quoted map[int]bool
	for _, toks := range [][]parser.Token{p.tokens, p.interp} {
		for i := 0; i+2 < len(toks); i++ {
			if (toks[i].Kind == parser.TokModule || toks[i].Kind == parser.TokIdent) && parser.TokenText(p.src, toks[i]) == "__MODULE__" &&
				toks[i+1].Kind == parser.TokDot && toks[i+2].Kind == parser.TokIdent &&
				parser.TokenText(p.src, toks[i+2]) == name {
				if quoted == nil {
					quoted = quoteLines(p.src, p.tokens)
				}
				// In a quote, __MODULE__ is the module that calls the macro,
				// not this one.
				if !quoted[toks[i+2].Line] {
					lines = append(lines, toks[i+2].Line)
				}
			}
		}
	}
	return lines
}

// quoteLines returns the lines inside a `quote do ... end` block, and the
// line of an inline `quote do:`.
func quoteLines(src []byte, toks []parser.Token) map[int]bool {
	lines := make(map[int]bool)
	n := len(toks)
	for i := 0; i < n; i++ {
		if toks[i].Kind != parser.TokIdent || parser.TokenText(src, toks[i]) != "quote" {
			continue
		}
		if _, _, hasDo := parser.ScanForwardToBlockDo(toks, n, i+1); hasDo {
			end := blockEnd(toks, i) // the token after the quote's end
			last := toks[min(end, n)-1].Line
			for l := toks[i].Line; l <= last; l++ {
				lines[l] = true
			}
			continue
		}
		lines[toks[i].Line] = true
	}
	return lines
}

// importOptions are the only: or except: options of one import.
type importOptions struct {
	kind  string           // "", "only", or "except"
	names map[string][]int // function → the lines of its keys in the list
	all   string           // only: :functions or only: :macros
}

// imports reports whether the import brings name (a macro when macro) in.
func (o importOptions) imports(name string, macro bool) bool {
	switch o.kind {
	case "only":
		if _, ok := o.names[name]; ok {
			return true
		}
		return (o.all == ":functions" && !macro) || (o.all == ":macros" && macro)
	case "except":
		_, ok := o.names[name]
		return !ok
	}
	return true
}

// parseImportOptions reads the options of the import on line (1-based) from
// the file's tokens, so a keyword list over several lines counts: `only: [`
// on one line and `foo: 1` on the next.
func parseImportOptions(p *parsedSource, line int) importOptions {
	opts := importOptions{names: map[string][]int{}}
	toks := p.tokens
	i := 0
	for i < len(toks) && (toks[i].Kind != parser.TokImport || toks[i].Line != line) {
		i++
	}
	if i == len(toks) {
		return opts
	}
	// The module name, then a comma before any option.
	i++
	for i < len(toks) && (toks[i].Kind == parser.TokModule || toks[i].Kind == parser.TokDot || toks[i].Kind == parser.TokIdent) {
		i++
	}
	if i >= len(toks) || toks[i].Kind != parser.TokComma {
		return opts
	}
	depth := 0
	key := ""
	lastSig := parser.TokComma
	for i++; i < len(toks); i++ {
		t := toks[i]
		switch t.Kind {
		case parser.TokEOF:
			return opts
		case parser.TokEOL, parser.TokComment:
			// A statement goes on after a comma, a colon, or an open bracket.
			if depth == 0 && lastSig != parser.TokComma && lastSig != parser.TokColon {
				return opts
			}
			continue
		case parser.TokOpenBracket, parser.TokOpenParen, parser.TokOpenBrace:
			depth++
		case parser.TokCloseBracket, parser.TokCloseParen, parser.TokCloseBrace:
			depth--
			if depth < 0 {
				return opts
			}
		case parser.TokDo, parser.TokEnd:
			return opts
		case parser.TokIdent:
			if i+1 < len(toks) && toks[i+1].Kind == parser.TokColon {
				name := parser.TokenText(p.src, t)
				switch {
				case depth == 0 && (name == "only" || name == "except"):
					key = name
					opts.kind = name
				case depth == 1 && key != "":
					opts.names[name] = append(opts.names[name], t.Line)
				}
			}
		case parser.TokAtom:
			if depth == 0 && key == "only" {
				opts.all = parser.TokenText(p.src, t)
			}
		}
		lastSig = t.Kind
	}
	return opts
}

// hasImplBefore reports whether the attributes right above line (1-based)
// hold an @impl. Blank lines, comments, other attributes, and the text of a
// heredoc (a @doc) between them are passed over.
func hasImplBefore(text string, line int) bool {
	lines := strings.Split(text, "\n")
	for i := line - 2; i >= 0 && i < len(lines); i-- {
		t := strings.TrimSpace(lines[i])
		switch {
		case t == "" || strings.HasPrefix(t, "#"):
		case strings.HasPrefix(t, "@impl"):
			return true
		case t == `"""` || t == `'''`:
			// The end of a heredoc: go up to the line that opens it.
			for i--; i >= 0 && !strings.Contains(lines[i], t); i-- {
			}
			if i >= 0 && strings.Contains(lines[i], "@impl") {
				return true
			}
		case strings.HasPrefix(t, "@"):
		default:
			return false
		}
	}
	return false
}

// isCallToken reports whether the identifier toks[i] is a call (`foo(`,
// `foo arg`, `|> foo`), a capture (`&foo/1`), or the name in a definition or
// a @spec, rather than a variable of the same name. interp is true for the
// #{} stream, which has no parentheses: there the source decides.
func isCallToken(src []byte, toks []parser.Token, i int, interp bool) bool {
	t := toks[i]
	if interp {
		j := t.End
		for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
			j++
		}
		return j < len(src) && src[j] == '('
	}
	if i > 0 {
		switch p := toks[i-1]; p.Kind {
		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate,
			parser.TokAttrSpec, parser.TokAttrCallback, parser.TokPipe:
			return true
		case parser.TokOther:
			if parser.TokenText(src, p) == "&" {
				return true
			}
		case parser.TokEOL:
			// `|>` at the end of the line before.
			for k := i - 1; k >= 0; k-- {
				if toks[k].Kind == parser.TokPipe {
					return true
				}
				if toks[k].Kind != parser.TokEOL && toks[k].Kind != parser.TokComment {
					break
				}
			}
		}
	}
	if i+1 < len(toks) && toks[i+1].Kind == parser.TokOpenParen {
		return true
	}
	// A call without parentheses, `foo arg`, by the rule that the parser
	// and findBareCallLines use, so that the line and the column agree.
	return parser.NoParenArgCount(src, toks, i) >= 0
}

// anyArityIn reports whether arities has an arity from lo to hi.
func anyArityIn(arities map[int]bool, lo, hi int) bool {
	for a := range arities {
		if a >= lo && a <= hi {
			return true
		}
	}
	return false
}
