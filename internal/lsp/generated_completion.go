package lsp

import (
	"container/list"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/remoteoss/dexter/internal/beam"
	"github.com/remoteoss/dexter/internal/treesitter"
	"go.lsp.dev/protocol"
)

const maxGeneratedFunctionCacheEntries = 1024

// generatedFunctionMissTTL is the retry window for a module the index does not
// know about. A store miss can become source-backed after the index changes,
// which has no filesystem stamp of its own. When an ebin exists the same entry
// also watches it, so a newly generated module invalidates the miss immediately.
const generatedFunctionMissTTL = time.Second

type generatedFunctionCacheEntry struct {
	sourcePath  string
	sourceStamp fileStamp
	buildRoot   string
	beamPath    string
	beamStamp   fileStamp
	functions   []beam.Function

	// watchDir/watchStamp invalidate a "this module is not compiled" answer. The
	// ebin directory's mtime moves when the BEAM is created, so a developer who
	// adds a macro call and recompiles gets the new functions on the next
	// keystroke instead of after a timeout.
	watchDir   string
	watchStamp fileStamp

	// providers are the modules whose macros are in scope inside this one but
	// invisible to static analysis, read from its compiled attributes.
	providers         []string
	providersResolved bool

	// docs memoizes rendered documentation prose for functions a hover has asked
	// about, so sweeping the mouse does not re-inflate the Docs chunk each time.
	// It is a pointer because get hands out a copy of the entry: the copy has to
	// share the memo with the cache instead of forking it. put replaces it with a
	// new entry, which is what invalidates it when the BEAM changes.
	docs *docMemo

	retryAfter time.Time
}

// docMemo is the documentation cache of one generatedFunctionCacheEntry. Its own
// lock guards the map because hovers run concurrently on copies of the entry,
// outside the cache's mutex; an unguarded map there is a fatal concurrent map
// write that kills the server rather than failing one request.
type docMemo struct {
	mu   sync.Mutex
	body map[string]string
}

func (m *docMemo) get(key string) (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	body, ok := m.body[key]
	return body, ok
}

func (m *docMemo) set(key, body string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.body == nil {
		m.body = make(map[string]string)
	}
	m.body[key] = body
}

type generatedFunctionCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // front is most recently used
}

// generatedCacheItem pairs an entry with its key so the LRU list can evict
// without a reverse lookup.
type generatedCacheItem struct {
	module string
	entry  generatedFunctionCacheEntry
}

func newGeneratedFunctionCache() *generatedFunctionCache {
	return &generatedFunctionCache{
		entries: make(map[string]*list.Element),
		order:   list.New(),
	}
}

func (c *generatedFunctionCache) get(module string) (generatedFunctionCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[module]
	if !ok {
		return generatedFunctionCacheEntry{}, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*generatedCacheItem).entry, true
}

// put stores an entry, evicting the least recently used one at capacity.
//
// Evicting one entry rather than clearing the cache matters at monorepo scale:
// a full clear discards every parsed Docs chunk at once, and the next round of
// completions re-inflates them all. Entries are small unless the module really
// does generate functions, so the cap bounds memory without churning.
func (c *generatedFunctionCache) put(module string, entry generatedFunctionCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry.docs == nil && entry.beamPath != "" {
		// A freshly loaded entry gets a fresh memo, which is how the prose of an
		// older BEAM is dropped; one written back from a copy keeps the memo it
		// already shares with the cache. An entry with no BEAM never reads docs.
		entry.docs = &docMemo{}
	}
	if element, ok := c.entries[module]; ok {
		item := element.Value.(*generatedCacheItem)
		item.entry = entry
		c.order.MoveToFront(element)
		return
	}
	if c.order.Len() >= maxGeneratedFunctionCacheEntries {
		if oldest := c.order.Back(); oldest != nil {
			c.order.Remove(oldest)
			delete(c.entries, oldest.Value.(*generatedCacheItem).module)
		}
	}
	c.entries[module] = c.order.PushFront(&generatedCacheItem{module: module, entry: entry})
}

// generatedFunctionsForModule is the single cached path for finding public
// callables present in a compiled module but absent from Dexter's source index.
// Completion and document prewarming both use it, so export and Docs parsing is
// never implemented or cached separately by individual LSP features.
//
// Every outcome is cached, including "the index does not know this module". The
// entry shape says how it is invalidated: no sourcePath and no beamPath is a
// store miss that only retryAfter can clear; a beamPath is checked by stamp; a
// watchDir is a module that is not compiled yet.
//
// The result is sorted by name then arity, which callers rely on to binary-search
// a prefix range.
func (s *Server) generatedFunctionsForModule(module string) []beam.Function {
	return s.generatedFunctionsFor(module, "")
}

// generatedFunctionsFor resolves a module's compiled functions, caching every
// outcome including the negative ones.
//
// knownBeam may be supplied by a caller that has already located the BEAM. That
// is how DSL entity modules work: Spark generates Ash.Resource.Dsl.CodeInterface.Define
// at compile time, so it has no source file and the index cannot resolve it. The
// only way to find one is the directory its parent extension was compiled into,
// which the caller has already determined.
func (s *Server) generatedFunctionsFor(module, knownBeam string) []beam.Function {
	var sourcePath, buildRoot string
	var sourceStamp fileStamp

	entry, cached := s.generatedCache.get(module)
	if cached {
		switch {
		case entry.sourcePath == "" && entry.beamPath == "":
			// A source-less module may have appeared in the project's ebin, while
			// a formerly unknown source module may have entered the index. The
			// directory stamp and retry deadline cover those two invalidators.
			if reuse, functions := cachedEntryStillValid(entry); reuse {
				return functions
			}
		case entry.sourcePath != "":
			if stamp := statFileStamp(entry.sourcePath); stamp.exists && stamp == entry.sourceStamp {
				// Source is unchanged, so keep the resolved path and skip
				// LookupModule even when the BEAM moved underneath us.
				sourcePath, buildRoot, sourceStamp = entry.sourcePath, entry.buildRoot, stamp
				if reuse, functions := cachedEntryStillValid(entry); reuse {
					return functions
				}
			}
		default:
			// A generated module: there is no source to stat, so its BEAM alone
			// decides whether the answer still holds.
			if reuse, functions := cachedEntryStillValid(entry); reuse {
				return functions
			}
		}
	}

	if knownBeam != "" {
		s.debugf("Generated BEAM resolve: module=%s strategy=known-beam beam=%s", module, knownBeam)
		return s.loadAndCacheGenerated(module, knownBeam, "", fileStamp{}, "")
	}

	if sourcePath == "" {
		moduleResults, err := s.store.LookupModule(module)
		if err != nil {
			s.debugf("Generated BEAM resolve: module=%s source lookup failed: %v", module, err)
			s.cacheGeneratedMiss(module)
			return nil
		}
		if len(moduleResults) == 0 {
			// The module itself may be generated. Phoenix route helpers are a
			// common example: aliases name MyApp.Router.Helpers, but no source
			// definition exists for the store to return. Probe the project build;
			// locateModuleBEAM checks the root application first and caches the
			// application listing, so this is one stat on the warm path.
			buildRoot = s.findBuildRoot(s.projectRoot)
			s.debugf("Generated BEAM resolve: module=%s source=generated build_root=%s", module, buildRoot)
		} else {
			sourcePath = moduleResults[0].FilePath
			sourceStamp = statFileStamp(sourcePath)
			if !sourceStamp.exists {
				s.debugf("Generated BEAM resolve: module=%s indexed source missing path=%s", module, sourcePath)
				s.cacheGeneratedMiss(module)
				return nil
			}
			buildRoot = s.findBuildRoot(filepath.Dir(sourcePath))
			s.debugf("Generated BEAM resolve: module=%s source=%s build_root=%s", module, sourcePath, buildRoot)
		}
	}

	loc := s.locateModuleBEAM(buildRoot, module, sourcePath)
	if loc.beamPath == "" {
		s.debugf("Generated BEAM resolve: module=%s no compiled BEAM watch=%s", module, loc.watchDir)
		negative := generatedFunctionCacheEntry{
			sourcePath:  sourcePath,
			sourceStamp: sourceStamp,
			buildRoot:   buildRoot,
			watchDir:    loc.watchDir,
			watchStamp:  loc.watchStamp,
		}
		if sourcePath == "" {
			// Recheck the store periodically as well: indexing a newly created
			// source module does not move the compiled directory's mtime.
			negative.retryAfter = time.Now().Add(generatedFunctionMissTTL)
		} else if loc.watchDir == "" {
			negative.retryAfter = time.Now().Add(generatedFunctionMissTTL)
		}
		s.generatedCache.put(module, negative)
		return nil
	}
	s.debugf("Generated BEAM resolve: module=%s beam=%s", module, loc.beamPath)
	return s.loadAndCacheGenerated(module, loc.beamPath, sourcePath, sourceStamp, buildRoot)
}

// cachedEntryStillValid reports whether a cached entry's stamps still hold, and
// which functions to serve if they do. A negative entry is valid too: its
// functions are nil.
func cachedEntryStillValid(entry generatedFunctionCacheEntry) (bool, []beam.Function) {
	switch {
	case entry.beamPath != "":
		return statFileStamp(entry.beamPath) == entry.beamStamp, entry.functions
	case entry.watchDir != "":
		// Still not compiled; the ebin mtime moves when that changes.
		stampValid := statFileStamp(entry.watchDir) == entry.watchStamp
		deadlineValid := entry.retryAfter.IsZero() || time.Now().Before(entry.retryAfter)
		return stampValid && deadlineValid, nil
	default:
		return time.Now().Before(entry.retryAfter), nil
	}
}

// loadAndCacheGenerated computes the compiled-vs-indexed delta for a BEAM and
// caches it against that BEAM's stamp.
//
// A BEAM older than its source is stale, not unusable, and Dexter must never
// depend on the developer having compiled: the BEAM describes the last known
// generated functions, and the source index still wins wherever the two disagree
// because loadCompiledFunctionDelta subtracts everything already indexed.
func (s *Server) loadAndCacheGenerated(module, beamPath, sourcePath string, sourceStamp fileStamp, buildRoot string) []beam.Function {
	started := s.debugNow()
	entry := generatedFunctionCacheEntry{
		sourcePath:  sourcePath,
		sourceStamp: sourceStamp,
		buildRoot:   buildRoot,
		beamPath:    beamPath,
		beamStamp:   statFileStamp(beamPath),
	}
	// On error the entry is cached with no functions: an unreadable or missing
	// Docs chunk is stable until this BEAM changes.
	functions, loadErr := s.loadCompiledFunctionDelta(module, beamPath)
	if loadErr == nil {
		entry.functions = functions
	}
	s.generatedCache.put(module, entry)
	if !started.IsZero() {
		if loadErr != nil {
			s.debugf("Generated BEAM: module=%s source_backed=%t error=%v total=%s", module, sourcePath != "", loadErr, time.Since(started).Round(time.Microsecond))
		} else {
			s.debugf("Generated BEAM: module=%s source_backed=%t functions=%d total=%s", module, sourcePath != "", len(entry.functions), time.Since(started).Round(time.Microsecond))
		}
	}
	return entry.functions
}

// loadCompiledFunctionDelta returns the callables a compiled module exports that
// Dexter's source index does not already know about. The index wins every
// disagreement: anything indexed is subtracted, so the BEAM is consulted only
// for genuinely generated functions. When nothing is missing the Docs chunk is
// never inflated, which is the common case and the whole point of checking ExpT
// first.
//
// The result is sorted by name then arity, which callers rely on to binary-search
// a prefix range instead of scanning.
func (s *Server) loadCompiledFunctionDelta(module, beamPath string) ([]beam.Function, error) {
	exports, err := beam.ReadExports(beamPath)
	if err != nil {
		return nil, err
	}
	indexed, err := s.store.ModuleFunctionKeys(module)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(indexed))
	for _, function := range indexed {
		seen[funcKey(function.Name, function.Arity)] = true
	}

	missing := make([]beam.Function, 0, len(exports))
	for _, function := range exports {
		if !seen[funcKey(function.Name, function.Arity)] {
			missing = append(missing, function)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}

	// ETF decoding is the expensive part, so it only runs once ExpT has proved
	// there is something generated to describe.
	documented, docsErr := beam.ReadDocumentedFunctions(beamPath)
	var docsByKey map[string]beam.Function
	if docsErr == nil {
		docsByKey = make(map[string]beam.Function, len(documented))
		for _, function := range documented {
			docsByKey[funcKey(function.Name, function.Arity)] = function
		}
	}

	result := missing[:0]
	for _, function := range missing {
		if documentedFunction, ok := docsByKey[funcKey(function.Name, function.Arity)]; ok {
			function = documentedFunction
		}
		// Export visibility is the boundary. @doc false and __dunder__ naming do
		// not make a function private: Oban's public new constructors are hidden
		// from docs, and callers legitimately use Ecto's __schema__ introspection.
		// This matches source-indexed public functions, which completion offers
		// regardless of their documentation or name.
		result = append(result, function)
	}
	sortFunctions(result)
	return result, nil
}

// sortFunctions orders by name then arity so a prefix search can binary-search.
// The cached slice is immutable afterwards, so this runs once per module per
// compile rather than per completion.
func sortFunctions(functions []beam.Function) {
	sort.Slice(functions, func(i, j int) bool {
		if functions[i].Name != functions[j].Name {
			return functions[i].Name < functions[j].Name
		}
		return functions[i].Arity < functions[j].Arity
	})
}

// isBlockMacro reports whether a compiled macro's only argument is a do block.
//
// This is a parameter-name test, which is normally too weak to trust: scanning
// 1769 defmacro arities across the Elixir stdlib, ExUnit, Ecto, Ash and Spark
// found block arguments named body, list, clauses, contents and args — while
// "value" (969 occurrences) and "opts" (241) are overwhelmingly *not* blocks.
// So no name identifies block macros in general.
//
// "body" is the exception, and it is exact rather than merely common: every one
// of the 25 occurrences in Ash and Spark is an arity-1 macro whose sole argument
// is the block, and "body" appears nowhere else in the corpus. It is also
// structurally confined — addGeneratedFunctionCompletions only offers the delta
// between a BEAM's exports and the source index, so indexed macros such as
// Ecto.Schema.schema/2 never reach this test.
//
// Anything broader needs a signal the BEAM does not carry: whether the last
// argument accepts a keyword list containing :do is a property of the macro's
// implementation, not its signature. Everywhere this test does not fire, the
// developer types `do` and the existing elixirFormSnippets template completes it
// to a block, which is the honest fallback.
func isBlockMacro(function beam.Function) bool {
	return function.Kind == "defmacro" && function.Arity == 1 && function.Params == "body"
}

// blockSnippet renders a macro that takes a do block, dropping the cursor inside
// it. Tabs match the existing doBlockSnippets templates.
func blockSnippet(name string, useSnippets bool) string {
	if !useSnippets {
		return name + " do\nend"
	}
	return name + " do\n\t$0\nend"
}

// macroProviderAttributes are the module attributes whose values name modules
// that supply macros to the compiled module.
//
// "extensions" is what Spark persists, and Spark generates the DSL macros behind
// Ash: resources/1, attributes/1, actions/1, code_interface/1. None of them exist
// as a defmacro in any source file — Spark emits them from %Spark.Dsl.Section{}
// data at compile time — and they leave no trace in the consumer's export or
// import tables, because macros expand away. The compiled module's own attributes
// are the only place their provider is recorded, short of interpreting Spark's
// code generation statically. Supporting another framework means adding a name
// here, not a parser.
var macroProviderAttributes = []string{"extensions"}

// macroProvidersForModule returns the modules whose macros are in scope inside
// module but cannot be found by reading source. The answer is memoized on the
// same cache entry as the module's generated functions, so the BEAM stamp that
// invalidates one invalidates the other.
func (s *Server) macroProvidersForModule(module string) []string {
	// Resolving first validates the entry and yields the BEAM path; the functions
	// themselves are not wanted here.
	_ = s.generatedFunctionsForModule(module)

	entry, ok := s.generatedCache.get(module)
	if !ok || entry.beamPath == "" {
		return nil
	}
	if entry.providersResolved {
		return entry.providers
	}

	var providers []string
	// A module with no Attr chunk has no providers, and that is stable until the
	// BEAM changes, so record the miss too rather than re-reading on every call.
	if attrs, err := beam.ReadModuleAttributes(entry.beamPath); err == nil {
		for _, name := range macroProviderAttributes {
			for _, value := range attrs[name] {
				provider := beam.StripElixirPrefix(value)
				if provider != "" && provider != module {
					providers = append(providers, provider)
				}
			}
		}
	}
	entry.providers = providers
	entry.providersResolved = true
	s.generatedCache.put(module, entry)
	return providers
}

// hoverFromGenerated renders documentation for a function that exists only in a
// compiled module: generated by a macro, so there is no source definition to
// hover. Callers reach it only after the store lookup came back empty, which is
// what keeps the index authoritative over the BEAM.
func (s *Server) hoverFromGenerated(module, beamPath, functionName string) *protocol.Hover {
	if module == "" || functionName == "" {
		return nil
	}
	functions := s.generatedFunctionsFor(module, beamPath)
	if len(functions) == 0 {
		return nil
	}

	// functions is sorted by name, so every arity of one function is contiguous.
	start := sort.Search(len(functions), func(i int) bool { return functions[i].Name >= functionName })
	var signatures []string
	docIndex := -1
	for i := start; i < len(functions) && functions[i].Name == functionName; i++ {
		function := functions[i]
		keyword := "def"
		if function.Kind == "defmacro" {
			keyword = "defmacro"
		}
		// Params is stored comma-joined without spaces because completion snippets
		// want it that way; prose reads better with them.
		params := strings.ReplaceAll(function.Params, ",", ", ")
		signatures = append(signatures, keyword+" "+function.Name+"("+params+")")
		if docIndex < 0 && function.DocLen > 0 {
			docIndex = i
		}
	}
	if len(signatures) == 0 {
		return nil
	}

	entry, _ := s.generatedCache.get(module)
	doc := generatedFunctionDoc(&entry, functions, docIndex)
	s.debugf("Hover: generated function module=%s function=%s signatures=%d beam=%s docs=%t", module, functionName, len(signatures), entry.beamPath, doc != "")

	content := formatHoverContent(doc, "", strings.Join(signatures, "\n"))
	if content == "" {
		return nil
	}
	return &protocol.Hover{
		Contents: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: content,
		},
	}
}

// enclosingBlockPath returns the do blocks surrounding the cursor, from the
// document's cached tree. It returns nil when no tree is available, which narrows
// DSL completion to the module's top-level sections rather than failing.
func (s *Server) enclosingBlockPath(docURI string, lineNum, col int) []string {
	tree, src, release, ok := s.docs.GetTree(docURI)
	if !ok {
		return nil
	}
	defer release()
	return treesitter.EnclosingBlockPathWithTree(tree.RootNode(), src, uint(lineNum), uint(col))
}

// hoverFromGeneratedModules hovers a bare function name against a module and the
// modules whose macros it pulls in at this block depth. DSL macros such as Ash's
// resources/1 are injected by a provider that only the compiled attributes name,
// so the enclosing module itself will not have them.
func (s *Server) hoverFromGeneratedModules(module string, blockPath func() []string, functionName string) *protocol.Hover {
	if module == "" {
		return nil
	}
	if hover := s.hoverFromGenerated(module, "", functionName); hover != nil {
		return hover
	}
	for _, provider := range s.dslProvidersInScope(module, blockPath) {
		if hover := s.hoverFromGenerated(provider.module, provider.beamPath, functionName); hover != nil {
			return hover
		}
	}
	return nil
}

// generatedFunctionDoc returns the documentation prose for one generated
// function, re-inflating the Docs chunk only on the first hover for it. docIndex
// is -1 when no arity carried prose.
//
// The memo is written through the pointer the entry shares with the cache instead
// of put back with the whole entry. Concurrent hovers are then safe, and a hover
// holding an entry that a newer BEAM has since replaced leaves the cache alone.
func generatedFunctionDoc(entry *generatedFunctionCacheEntry, functions []beam.Function, docIndex int) string {
	if docIndex < 0 || entry.beamPath == "" {
		return ""
	}
	function := functions[docIndex]
	key := funcKey(function.Name, function.Arity)
	if body, cached := entry.docs.get(key); cached {
		return body
	}
	body := ""
	if text, err := beam.ReadDocBody(entry.beamPath, function.DocOffset, function.DocLen); err == nil {
		body = text
	}
	// A failed read is cached as empty too: this BEAM is not going to grow prose
	// for this function, and re-reading it on every hover sweep is pure waste.
	entry.docs.set(key, body)
	return body
}

// cacheGeneratedMiss records that the index has no source for a module. The
// entry carries no sourcePath, so it is the negative case callers branch on.
func (s *Server) cacheGeneratedMiss(module string) {
	s.generatedCache.put(module, generatedFunctionCacheEntry{
		retryAfter: time.Now().Add(generatedFunctionMissTTL),
	})
}

// addGeneratedFunctionCompletions appends the generated callables of a module
// that match prefix. beamPath may be empty to resolve through the index, or set
// for a generated module that has no source file. It relies on the function list
// being sorted by name, so the matches form one contiguous range found by binary
// search rather than a full scan.
func (s *Server) addGeneratedFunctionCompletions(module, beamPath, prefix string, seen map[string]bool, items *[]protocol.CompletionItem, inPipe, useSnippets bool) {
	functions := s.generatedFunctionsFor(module, beamPath)
	if len(functions) == 0 {
		return
	}
	before := len(*items)
	start := sort.Search(len(functions), func(i int) bool { return functions[i].Name >= prefix })
	for _, function := range functions[start:] {
		if !strings.HasPrefix(function.Name, prefix) {
			break
		}
		key := funcKey(function.Name, function.Arity)
		if seen[key] {
			continue
		}
		seen[key] = true
		item := protocol.CompletionItem{
			Label:  function.Name,
			Kind:   kindToCompletionItemKind(function.Kind),
			Detail: module + " (compiled function)",
		}
		applySnippet(&item, function.Name, function.Arity, function.Params, function.Kind, inPipe, useSnippets)
		// A curated template still wins; it may carry argument placeholders we
		// cannot infer from the compiled form.
		if isBlockMacro(function) && doBlockSnippets[function.Name] == "" {
			item.InsertText = blockSnippet(function.Name, useSnippets)
			if useSnippets {
				item.InsertTextFormat = protocol.InsertTextFormatSnippet
			}
		}
		*items = append(*items, item)
	}
	if added := len(*items) - before; added > 0 {
		entry, _ := s.generatedCache.get(module)
		s.debugf("Completion: generated functions module=%s prefix=%q matches=%d beam=%s", module, prefix, added, entry.beamPath)
	}
}

// prewarmGeneratedFunctions follows the same tokenized alias/import/use flow
// as completion and feeds every discovered module through the shared compiled
// function cache. It runs in DidOpen's background worker and never participates
// in indexing.
func (s *Server) prewarmGeneratedFunctions(path, text string) {
	tf := NewTokenizedFile(text)
	aliases := tf.ExtractAliases()
	// This uses the existing usingCache-backed chain resolver. Besides aliases
	// injected by `use`, it ensures opening a document and later lookup/completion
	// do not independently walk the same __using__ chain.
	s.mergeAliasesFromUseTokenized(tf, aliases)

	modules := make(map[string]bool, len(aliases)+8)
	for _, module := range aliases {
		modules[module] = true
	}
	for _, module := range tf.ExtractImports() {
		modules[resolveModule(module, aliases)] = true
	}
	for _, useCall := range tf.ExtractUsesWithOpts(aliases) {
		modules[useCall.Module] = true
	}
	if currentModules, err := s.store.LookupModulesInFile(path); err == nil {
		for _, module := range currentModules {
			modules[module] = true
		}
	}

	for module := range modules {
		if module != "" && module != "Kernel" {
			s.generatedFunctionsForModule(module)
		}
	}
}
