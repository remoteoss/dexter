package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"

	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/stdlib"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/store"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/version"
)

// usingCacheEntry holds the full parsed result of a module's defmacro __using__
// body, keyed by module name. Storing filePath avoids a LookupModule query on
// cache hits; mtime invalidates the entry when the source file changes.
type usingCacheEntry struct {
	mtime      int64
	filePath   string
	imports    []string               // modules imported in __using__, source order
	inlineDefs map[string][]inlineDef // function name → inline defs in quote do block
	transUses  []string               // modules used inside __using__ body (double-use chains)
}

type Server struct {
	store           *store.Store
	docs            *DocumentStore
	projectRoot     string
	stdlibRoot      string
	initialized     bool
	client          protocol.Client
	followDelegates bool

	usingCache   map[string]*usingCacheEntry // module name → parsed __using__ result
	usingCacheMu sync.RWMutex
}

func NewServer(s *store.Store, projectRoot string) *Server {
	return &Server{
		store:           s,
		docs:            NewDocumentStore(),
		projectRoot:     projectRoot,
		followDelegates: true,
		usingCache:      make(map[string]*usingCacheEntry),
	}
}

type stdinoutCloser struct {
	io.Reader
	io.Writer
}

func (s stdinoutCloser) Close() error { return nil }

// Serve starts the LSP server on the given reader/writer (typically stdin/stdout).
func Serve(in io.Reader, out io.Writer, s *store.Store, projectRoot string) error {
	server := NewServer(s, projectRoot)

	logger, _ := zap.NewProduction()
	stream := jsonrpc2.NewStream(stdinoutCloser{in, out})
	conn := jsonrpc2.NewConn(stream)
	server.client = protocol.ClientDispatcher(conn, logger)

	handler := protocol.ServerHandler(server, nil)
	ctx := context.Background()

	conn.Go(ctx, handler)
	<-conn.Done()
	return conn.Err()
}

// backgroundReindex runs in the background. If the index is empty it does a
// full init, otherwise it does an incremental mtime-based update.
func (s *Server) backgroundReindex() {
	go func() {
		start := time.Now()
		reindexed := 0
		isEmpty := s.store.IsEmpty()

		if isEmpty {
			log.Printf("No index found, building from scratch...")
			if s.client != nil {
				if err := s.client.ShowMessage(context.Background(), &protocol.ShowMessageParams{
					Type:    protocol.MessageTypeInfo,
					Message: "Dexter: building index for the first time, go-to-definition will be available shortly...",
				}); err != nil {
					log.Printf("ShowMessage: %v", err)
				}
			}
		}

		seen := make(map[string]struct{})
		walkAndIndex := func(root string) {
			_ = parser.WalkElixirFiles(root, func(path string, d fs.DirEntry) error {
				seen[path] = struct{}{}

				if !isEmpty {
					info, err := d.Info()
					if err != nil {
						return nil
					}
					storedMtime, found := s.store.GetFileMtime(path)
					currentMtime := info.ModTime().UnixNano()
					if found && storedMtime == currentMtime {
						return nil
					}
				}

				defs, err := parser.ParseFile(path)
				if err != nil {
					return nil
				}
				if err := s.store.IndexFile(path, defs); err != nil {
					log.Printf("Warning: reindex %s: %v", path, err)
				}
				reindexed++
				return nil
			})
		}

		walkAndIndex(s.projectRoot)

		if s.stdlibRoot != "" {
			walkAndIndex(s.stdlibRoot)
		}

		// Prune store entries for files no longer on disk
		if storedPaths, err := s.store.ListFilePaths(); err == nil {
			for _, storedPath := range storedPaths {
				if _, ok := seen[storedPath]; !ok {
					_ = s.store.RemoveFile(storedPath)
				}
			}
		}

		elapsed := time.Since(start).Round(time.Millisecond)
		log.Printf("Background reindex: %d files updated (%s)", reindexed, elapsed)

		if isEmpty && s.client != nil {
			if err := s.client.ShowMessage(context.Background(), &protocol.ShowMessageParams{
				Type:    protocol.MessageTypeInfo,
				Message: fmt.Sprintf("Dexter: index built (%d files in %s)", reindexed, elapsed),
			}); err != nil {
				log.Printf("ShowMessage: %v", err)
			}
		}
	}()
}

// watchGitHead polls .git/HEAD mtime and triggers reindex on branch switches.
func (s *Server) watchGitHead() {
	go func() {
		headPath := filepath.Join(s.projectRoot, ".git", "HEAD")
		var lastMtime int64

		info, err := os.Stat(headPath)
		if err != nil {
			return // no .git, skip
		}
		lastMtime = info.ModTime().UnixNano()

		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			info, err := os.Stat(headPath)
			if err != nil {
				continue
			}
			currentMtime := info.ModTime().UnixNano()
			if currentMtime != lastMtime {
				lastMtime = currentMtime
				log.Printf("Git HEAD changed, reindexing...")
				s.backgroundReindex()
			}
		}
	}()
}

// periodicReindex runs backgroundReindex on a fixed interval to catch files
// created or deleted outside the editor.
func (s *Server) periodicReindex() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			s.backgroundReindex()
		}
	}()
}

// === LSP Lifecycle ===

func (s *Server) Initialize(ctx context.Context, params *protocol.InitializeParams) (*protocol.InitializeResult, error) {
	if len(params.WorkspaceFolders) > 0 {
		root := uriToPath(protocol.DocumentURI(params.WorkspaceFolders[0].URI))
		if root != "" {
			s.projectRoot = findDexterRoot(root)
		}
	} else if params.RootURI != "" { //nolint:staticcheck // RootURI is deprecated but Neovim still sends it
		root := uriToPath(params.RootURI) //nolint:staticcheck
		if root != "" {
			s.projectRoot = findDexterRoot(root)
		}
	}

	var explicitStdlibPath string
	if opts, ok := params.InitializationOptions.(map[string]interface{}); ok {
		if v, ok := opts["followDelegates"].(bool); ok {
			s.followDelegates = v
		}
		if v, ok := opts["stdlibPath"].(string); ok {
			explicitStdlibPath = v
		}
	}

	if root, ok := stdlib.Resolve(s.store, explicitStdlibPath); ok {
		s.stdlibRoot = root
		log.Printf("Elixir stdlib at: %s", root)
	} else {
		log.Printf("Could not detect Elixir stdlib (set stdlibPath in initializationOptions or DEXTER_ELIXIR_LIB_ROOT)")
		if s.client != nil {
			_ = s.client.ShowMessage(context.Background(), &protocol.ShowMessageParams{
				Type:    protocol.MessageTypeWarning,
				Message: "Dexter: could not detect Elixir stdlib — stdlib modules (Enum, String, etc.) won't resolve. Set stdlibPath in initializationOptions or DEXTER_ELIXIR_LIB_ROOT.",
			})
		}
	}

	if !s.initialized {
		s.initialized = true
		s.backgroundReindex()
		s.watchGitHead()
		s.periodicReindex()
	}

	return &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: protocol.TextDocumentSyncOptions{
				OpenClose: true,
				Change:    protocol.TextDocumentSyncKindFull,
				Save: &protocol.SaveOptions{
					IncludeText: false,
				},
			},
			DefinitionProvider: true,
			HoverProvider:      true,
			CompletionProvider: &protocol.CompletionOptions{
				TriggerCharacters: []string{"."},
				ResolveProvider:   true,
			},
		},
		ServerInfo: &protocol.ServerInfo{
			Name:    "dexter",
			Version: version.Version,
		},
	}, nil
}

func (s *Server) Initialized(ctx context.Context, params *protocol.InitializedParams) error {
	if s.client != nil {
		go func() {
			if err := s.client.RegisterCapability(context.Background(), &protocol.RegistrationParams{
				Registrations: []protocol.Registration{
					{
						ID:     "dexter-file-watcher",
						Method: "workspace/didChangeWatchedFiles",
						RegisterOptions: protocol.DidChangeWatchedFilesRegistrationOptions{
							Watchers: []protocol.FileSystemWatcher{
								{GlobPattern: "**/*.ex", Kind: protocol.WatchKindCreate + protocol.WatchKindChange + protocol.WatchKindDelete},
								{GlobPattern: "**/*.exs", Kind: protocol.WatchKindCreate + protocol.WatchKindChange + protocol.WatchKindDelete},
							},
						},
					},
				},
			}); err != nil {
				log.Printf("RegisterCapability: %v", err)
			}
		}()
	}
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return nil
}

func (s *Server) Exit(ctx context.Context) error {
	os.Exit(0)
	return nil
}

// === Document Sync ===

func (s *Server) DidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error {
	s.docs.Set(string(params.TextDocument.URI), params.TextDocument.Text)
	return nil
}

func (s *Server) DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	if len(params.ContentChanges) > 0 {
		// Full sync mode — last change contains the full text
		s.docs.Set(string(params.TextDocument.URI), params.ContentChanges[len(params.ContentChanges)-1].Text)
	}
	return nil
}

func (s *Server) DidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error {
	s.docs.Close(string(params.TextDocument.URI))
	return nil
}

func (s *Server) DidSave(ctx context.Context, params *protocol.DidSaveTextDocumentParams) error {
	path := uriToPath(params.TextDocument.URI)
	if path == "" || !parser.IsElixirFile(path) {
		return nil
	}

	go func() {
		defs, err := parser.ParseFile(path)
		if err != nil {
			log.Printf("Error parsing %s: %v", path, err)
			return
		}
		if err := s.store.IndexFile(path, defs); err != nil {
			log.Printf("Error indexing %s: %v", path, err)
		}
	}()

	return nil
}

// === Definition ===

func (s *Server) Definition(ctx context.Context, params *protocol.DefinitionParams) ([]protocol.Location, error) {
	docURI := string(params.TextDocument.URI)

	text, ok := s.docs.Get(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := int(params.Position.Character)

	if lineNum >= len(lines) {
		return nil, nil
	}

	// Check for @module_attribute reference first
	if attrName := ExtractModuleAttribute(lines[lineNum], col); attrName != "" {
		if line, found := FindModuleAttributeDefinition(text, attrName); found {
			return []protocol.Location{{
				URI:   params.TextDocument.URI,
				Range: lineRange(line - 1),
			}}, nil
		}
		return nil, nil
	}

	expr := ExtractExpression(lines[lineNum], col)
	if expr == "" {
		return nil, nil
	}

	// Substitute __MODULE__ with the actual module name so that expressions
	// like __MODULE__.User resolve correctly through normal alias/module paths.
	if strings.Contains(expr, "__MODULE__") {
		for _, l := range lines {
			if m := parser.DefmoduleRe.FindStringSubmatch(l); m != nil {
				expr = strings.ReplaceAll(expr, "__MODULE__", m[1])
				break
			}
		}
	}

	moduleRef, functionName := ExtractModuleAndFunction(expr)
	aliases := ExtractAliases(text)

	// Bare function call — check local buffer, then imports
	if moduleRef == "" {
		if functionName == "" {
			return nil, nil
		}

		// Check current buffer first
		if line, found := FindFunctionDefinition(text, functionName); found {
			return []protocol.Location{{
				URI:   params.TextDocument.URI,
				Range: lineRange(line - 1),
			}}, nil
		}

		// Check imports
		imports := ExtractImports(text)
		for _, mod := range imports {
			results, err := s.store.LookupFollowDelegate(mod, functionName)
			if err != nil {
				continue
			}
			if len(results) > 0 {
				return storeResultsToLocations(results), nil
			}
		}

		// Check use-injected imports and inline defs
		if results := s.lookupThroughUse(text, functionName, aliases); len(results) > 0 {
			return storeResultsToLocations(results), nil
		}

		// Kernel is always imported — fall back to it last
		if results, err := s.store.LookupFollowDelegate("Kernel", functionName); err == nil && len(results) > 0 {
			return storeResultsToLocations(results), nil
		}

		return nil, nil
	}

	// Module.function call — resolve aliases, then look up
	fullModule := moduleRef
	if resolved, ok := aliases[moduleRef]; ok {
		// Exact alias: "Foo" -> "MyApp.Handlers.Foo"
		fullModule = resolved
	} else if parts := strings.SplitN(moduleRef, ".", 2); len(parts) == 2 {
		// Partial alias: "Services.AssociateWithTeamV2" where the file has
		// "alias __MODULE__.Services". Only resolves if the first segment is
		// explicitly aliased — otherwise falls through to a direct lookup.
		if resolved, ok := aliases[parts[0]]; ok {
			fullModule = resolved + "." + parts[1]
		}
	}

	if functionName != "" {
		var results []store.LookupResult
		var err error
		if s.followDelegates {
			results, err = s.store.LookupFollowDelegate(fullModule, functionName)
		} else {
			results, err = s.store.LookupFunction(fullModule, functionName)
		}
		if err == nil && len(results) > 0 {
			return storeResultsToLocations(results), nil
		}
	}

	// Fall back to module
	results, err := s.store.LookupModule(fullModule)
	if err != nil {
		return nil, nil
	}
	return storeResultsToLocations(results), nil
}

func storeResultsToLocations(results []store.LookupResult) []protocol.Location {
	var locations []protocol.Location
	for _, r := range results {
		locations = append(locations, protocol.Location{
			URI:   uri.File(r.FilePath),
			Range: lineRange(r.Line - 1), // LSP lines are 0-based
		})
	}
	return locations
}

func lineRange(line int) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{Line: uint32(line), Character: 0},
		End:   protocol.Position{Line: uint32(line), Character: 0},
	}
}

// findDexterRoot walks up from the given path looking for .dexter.db first,
// then .git (monorepo root), falling back to the original path.
func findDexterRoot(path string) string {
	for _, marker := range []string{".dexter.db", ".git"} {
		dir := path
		for {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return path
}

func uriToPath(u protocol.DocumentURI) string {
	parsed := uri.URI(u)
	return parsed.Filename()
}

// === No-op implementations for unused Server interface methods ===

func (s *Server) WorkDoneProgressCancel(ctx context.Context, params *protocol.WorkDoneProgressCancelParams) error {
	return nil
}
func (s *Server) LogTrace(ctx context.Context, params *protocol.LogTraceParams) error { return nil }
func (s *Server) SetTrace(ctx context.Context, params *protocol.SetTraceParams) error { return nil }
func (s *Server) CodeAction(ctx context.Context, params *protocol.CodeActionParams) ([]protocol.CodeAction, error) {
	return nil, nil
}
func (s *Server) CodeLens(ctx context.Context, params *protocol.CodeLensParams) ([]protocol.CodeLens, error) {
	return nil, nil
}
func (s *Server) CodeLensResolve(ctx context.Context, params *protocol.CodeLens) (*protocol.CodeLens, error) {
	return nil, nil
}
func (s *Server) ColorPresentation(ctx context.Context, params *protocol.ColorPresentationParams) ([]protocol.ColorPresentation, error) {
	return nil, nil
}
func (s *Server) Completion(ctx context.Context, params *protocol.CompletionParams) (*protocol.CompletionList, error) {
	docURI := string(params.TextDocument.URI)

	text, ok := s.docs.Get(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := int(params.Position.Character)

	if lineNum >= len(lines) {
		return nil, nil
	}

	prefix, afterDot := ExtractCompletionContext(lines[lineNum], col)
	if prefix == "" && !afterDot {
		return nil, nil
	}

	moduleRef, funcPrefix := ExtractModuleAndFunction(prefix)

	var items []protocol.CompletionItem

	if moduleRef != "" && (afterDot || funcPrefix != "") {
		aliases := ExtractAliases(text)
		resolved := resolveModule(moduleRef, aliases)
		results, err := s.store.ListModuleFunctions(resolved, true)
		if err != nil {
			return nil, nil
		}
		for _, r := range results {
			if funcPrefix != "" && !strings.HasPrefix(r.Function, funcPrefix) {
				continue
			}
			item := protocol.CompletionItem{
				Label:  r.Function,
				Kind:   kindToCompletionItemKind(r.Kind),
				Detail: r.Kind,
				Data: map[string]interface{}{
					"filePath": r.FilePath,
					"line":     r.Line,
				},
			}
			applySnippet(&item, r.Function, r.Arity)
			items = append(items, item)
		}

		if afterDot {
			segments, err := s.store.SearchSubmoduleSegments(resolved, funcPrefix)
			if err == nil {
				for _, segment := range segments {
					items = append(items, protocol.CompletionItem{
						Label:  segment,
						Kind:   protocol.CompletionItemKindModule,
						Detail: resolved + "." + segment,
					})
				}
			}
		}
	} else if moduleRef != "" {
		aliases := ExtractAliases(text)

		for shortName, fullModule := range aliases {
			if strings.HasPrefix(shortName, moduleRef) {
				items = append(items, protocol.CompletionItem{
					Label:  shortName,
					Kind:   protocol.CompletionItemKindModule,
					Detail: fullModule,
				})
			}
		}

		if parts := strings.SplitN(moduleRef, ".", 2); len(parts) == 2 {
			if resolved, ok := aliases[parts[0]]; ok {
				resolvedPrefix := resolved + "." + parts[1]
				aliasResults, err := s.store.SearchModules(resolvedPrefix)
				if err == nil {
					for _, r := range aliasResults {
						label := parts[0] + strings.TrimPrefix(r.Module, resolved)
						items = append(items, protocol.CompletionItem{
							Label:  label,
							Kind:   protocol.CompletionItemKindModule,
							Detail: r.Module,
						})
					}
				}
			}
		}

		// When moduleRef has dots (e.g. "MyApp.Ser"), also search for
		// sub-module segments under the parent with the last part as prefix.
		if dotIdx := strings.LastIndexByte(moduleRef, '.'); dotIdx >= 0 {
			parentModule := moduleRef[:dotIdx]
			segmentPrefix := moduleRef[dotIdx+1:]
			resolved := resolveModule(parentModule, aliases)
			segments, err := s.store.SearchSubmoduleSegments(resolved, segmentPrefix)
			if err == nil {
				for _, segment := range segments {
					label := parentModule + "." + segment
					items = append(items, protocol.CompletionItem{
						Label:  label,
						Kind:   protocol.CompletionItemKindModule,
						Detail: resolved + "." + segment,
					})
				}
			}
		}

		results, err := s.store.SearchModules(moduleRef)
		if err != nil {
			return nil, nil
		}
		for _, r := range results {
			items = append(items, protocol.CompletionItem{
				Label:  r.Module,
				Kind:   protocol.CompletionItemKindModule,
				Detail: "module",
			})
		}
	} else if funcPrefix != "" {
		seen := make(map[string]bool)

		for _, bf := range FindBufferFunctions(text) {
			key := funcKey(bf.Name, bf.Arity)
			if strings.HasPrefix(bf.Name, funcPrefix) && !seen[key] {
				seen[key] = true
				item := protocol.CompletionItem{
					Label:  bf.Name,
					Kind:   kindToCompletionItemKind(bf.Kind),
					Detail: bf.Kind,
				}
				applySnippet(&item, bf.Name, bf.Arity)
				items = append(items, item)
			}
		}

		for _, mod := range ExtractImports(text) {
			results, err := s.store.ListModuleFunctions(mod, true)
			if err != nil {
				continue
			}
			for _, r := range results {
				key := funcKey(r.Function, r.Arity)
				if strings.HasPrefix(r.Function, funcPrefix) && !seen[key] {
					seen[key] = true
					item := protocol.CompletionItem{
						Label:  r.Function,
						Kind:   kindToCompletionItemKind(r.Kind),
						Detail: r.Module + " (" + r.Kind + ")",
						Data: map[string]interface{}{
							"filePath": r.FilePath,
							"line":     r.Line,
						},
					}
					applySnippet(&item, r.Function, r.Arity)
					items = append(items, item)
				}
			}
		}

		// Check use-injected imports and inline defs (including transitive use chains)
		aliases := ExtractAliases(text)
		visitedCompletion := make(map[string]bool)
		for _, usedModule := range ExtractUses(text) {
			s.addCompletionsFromUsing(resolveModule(usedModule, aliases), funcPrefix, seen, &items, visitedCompletion)
		}
	}

	if len(items) == 0 {
		return nil, nil
	}

	return &protocol.CompletionList{
		IsIncomplete: len(items) >= 100,
		Items:        items,
	}, nil
}

// cachedUsing returns the parsed __using__ body for the given module name.
// The result is cached by module name; filePath is stored in the entry so
// LookupModule is only called on the first access. The cache is invalidated
// when the source file's mtime changes.
func (s *Server) cachedUsing(moduleName string) *usingCacheEntry {
	s.usingCacheMu.RLock()
	entry, ok := s.usingCache[moduleName]
	s.usingCacheMu.RUnlock()

	if ok {
		info, err := os.Stat(entry.filePath)
		if err == nil && info.ModTime().UnixNano() == entry.mtime {
			return entry
		}
		// File changed — re-parse using the cached path (no LookupModule needed)
		if newEntry := s.parseUsingFile(entry.filePath); newEntry != nil {
			s.usingCacheMu.Lock()
			s.usingCache[moduleName] = newEntry
			s.usingCacheMu.Unlock()
			return newEntry
		}
		return nil
	}

	// Cache miss — look up file path from the store (only on first access)
	modResults, err := s.store.LookupModule(moduleName)
	if err != nil || len(modResults) == 0 {
		return nil
	}
	filePath := filepath.Clean(modResults[0].FilePath)
	newEntry := s.parseUsingFile(filePath)
	if newEntry == nil {
		return nil
	}
	s.usingCacheMu.Lock()
	s.usingCache[moduleName] = newEntry
	s.usingCacheMu.Unlock()
	return newEntry
}

func (s *Server) parseUsingFile(filePath string) *usingCacheEntry {
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return nil
	}
	imported, inlineDefs, transUses := parseUsingBody(string(fileData))
	return &usingCacheEntry{
		mtime:      info.ModTime().UnixNano(),
		filePath:   filePath,
		imports:    imported,
		inlineDefs: inlineDefs,
		transUses:  transUses,
	}
}

// lookupThroughUse searches for functionName in definitions injected by `use`
// declarations. Inline defs (defined directly in the quote do block) take
// priority over imported ones. Later `use` declarations shadow earlier ones.
// Transitive use chains (use inside __using__ body) are followed recursively.
func (s *Server) lookupThroughUse(text, functionName string, aliases map[string]string) []store.LookupResult {
	uses := ExtractUses(text)
	visited := make(map[string]bool)

	for i := len(uses) - 1; i >= 0; i-- {
		moduleName := resolveModule(uses[i], aliases)
		if result := s.lookupInUsingEntry(moduleName, functionName, visited); result != nil {
			return result
		}
	}
	return nil
}

// lookupInUsingEntry resolves functionName through a single module's __using__
// body, then recurses into any transitive uses. The visited set prevents cycles.
func (s *Server) lookupInUsingEntry(moduleName, functionName string, visited map[string]bool) []store.LookupResult {
	if visited[moduleName] {
		return nil
	}
	visited[moduleName] = true

	entry := s.cachedUsing(moduleName)
	if entry == nil {
		return nil
	}

	// Inline defs take priority: directly injected by the quote do block
	if defs, ok := entry.inlineDefs[functionName]; ok {
		var results []store.LookupResult
		for _, d := range defs {
			results = append(results, store.LookupResult{FilePath: entry.filePath, Line: d.line})
		}
		return results
	}

	// Imported modules (last import in __using__ wins → iterate in reverse)
	for j := len(entry.imports) - 1; j >= 0; j-- {
		var results []store.LookupResult
		var err error
		if s.followDelegates {
			results, err = s.store.LookupFollowDelegate(entry.imports[j], functionName)
		} else {
			results, err = s.store.LookupFunction(entry.imports[j], functionName)
		}
		if err != nil || len(results) == 0 {
			continue
		}
		return results
	}

	// Transitive uses: use Module inside the __using__ body (double-use chains)
	for k := len(entry.transUses) - 1; k >= 0; k-- {
		if result := s.lookupInUsingEntry(entry.transUses[k], functionName, visited); result != nil {
			return result
		}
	}

	return nil
}

// addCompletionsFromUsing adds completion items injected by a module's __using__
// body — inline defs, imported functions, and transitive uses — into items.
func (s *Server) addCompletionsFromUsing(moduleName, funcPrefix string, seen map[string]bool, items *[]protocol.CompletionItem, visited map[string]bool) {
	if visited[moduleName] {
		return
	}
	visited[moduleName] = true

	entry := s.cachedUsing(moduleName)
	if entry == nil {
		return
	}

	for funcName, defs := range entry.inlineDefs {
		if !strings.HasPrefix(funcName, funcPrefix) {
			continue
		}
		for _, d := range defs {
			key := funcKey(funcName, d.arity)
			if !seen[key] {
				seen[key] = true
				item := protocol.CompletionItem{
					Label:  funcName,
					Kind:   kindToCompletionItemKind(d.kind),
					Detail: d.kind,
					Data: map[string]interface{}{
						"filePath": entry.filePath,
						"line":     d.line,
					},
				}
				applySnippet(&item, funcName, d.arity)
				*items = append(*items, item)
			}
		}
	}

	for _, mod := range entry.imports {
		results, err := s.store.ListModuleFunctions(mod, true)
		if err != nil {
			continue
		}
		for _, r := range results {
			key := funcKey(r.Function, r.Arity)
			if strings.HasPrefix(r.Function, funcPrefix) && !seen[key] {
				seen[key] = true
				item := protocol.CompletionItem{
					Label:  r.Function,
					Kind:   kindToCompletionItemKind(r.Kind),
					Detail: r.Module + " (" + r.Kind + ")",
					Data: map[string]interface{}{
						"filePath": r.FilePath,
						"line":     r.Line,
					},
				}
				applySnippet(&item, r.Function, r.Arity)
				*items = append(*items, item)
			}
		}
	}

	for _, transModule := range entry.transUses {
		s.addCompletionsFromUsing(transModule, funcPrefix, seen, items, visited)
	}
}

func resolveModule(moduleRef string, aliases map[string]string) string {
	if resolved, ok := aliases[moduleRef]; ok {
		return resolved
	}
	if parts := strings.SplitN(moduleRef, ".", 2); len(parts) == 2 {
		if resolved, ok := aliases[parts[0]]; ok {
			return resolved + "." + parts[1]
		}
	}
	return moduleRef
}

func funcKey(name string, arity int) string {
	return name + "/" + strconv.Itoa(arity)
}

func applySnippet(item *protocol.CompletionItem, name string, arity int) {
	if arity > 0 {
		item.InsertText = functionSnippet(name, arity)
		item.InsertTextFormat = protocol.InsertTextFormatSnippet
	}
}

func functionSnippet(name string, arity int) string {
	var args []string
	for i := 1; i <= arity; i++ {
		args = append(args, fmt.Sprintf("${%d:arg%d}", i, i))
	}
	return name + "(" + strings.Join(args, ", ") + ")"
}

func kindToCompletionItemKind(kind string) protocol.CompletionItemKind {
	switch kind {
	case "module", "defprotocol":
		return protocol.CompletionItemKindModule
	default:
		return protocol.CompletionItemKindFunction
	}
}
func (s *Server) CompletionResolve(ctx context.Context, params *protocol.CompletionItem) (*protocol.CompletionItem, error) {
	if params.Data == nil {
		return params, nil
	}

	raw, err := json.Marshal(params.Data)
	if err != nil {
		return params, nil
	}

	var data struct {
		FilePath string `json:"filePath"`
		Line     int    `json:"line"`
	}
	if err := json.Unmarshal(raw, &data); err != nil || data.FilePath == "" {
		return params, nil
	}

	cleaned := filepath.Clean(data.FilePath)
	inProject := strings.HasPrefix(cleaned, s.projectRoot+string(os.PathSeparator))
	inStdlib := s.stdlibRoot != "" && strings.HasPrefix(cleaned, s.stdlibRoot+string(os.PathSeparator))
	if !inProject && !inStdlib {
		return params, nil
	}

	fileData, err := os.ReadFile(cleaned)
	if err != nil {
		return params, nil
	}

	lines := strings.Split(string(fileData), "\n")
	defIdx := data.Line - 1
	if defIdx < 0 || defIdx >= len(lines) {
		return params, nil
	}

	doc, spec := extractDocAbove(lines, defIdx)
	signature := extractSignature(lines, defIdx)
	content := formatHoverContent(doc, spec, signature)

	if content != "" {
		params.Documentation = protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: content,
		}
	}

	return params, nil
}
func (s *Server) Declaration(ctx context.Context, params *protocol.DeclarationParams) ([]protocol.Location, error) {
	return nil, nil
}
func (s *Server) DidChangeConfiguration(ctx context.Context, params *protocol.DidChangeConfigurationParams) error {
	return nil
}
func (s *Server) DidChangeWatchedFiles(ctx context.Context, params *protocol.DidChangeWatchedFilesParams) error {
	for _, change := range params.Changes {
		path := uriToPath(change.URI)
		if path == "" {
			continue
		}
		switch change.Type {
		case protocol.FileChangeTypeCreated, protocol.FileChangeTypeChanged:
			go func(filePath string) {
				defs, err := parser.ParseFile(filePath)
				if err != nil {
					log.Printf("Error parsing %s: %v", filePath, err)
					return
				}
				if err := s.store.IndexFile(filePath, defs); err != nil {
					log.Printf("Error indexing %s: %v", filePath, err)
				}
			}(path)
		case protocol.FileChangeTypeDeleted:
			go func(filePath string) {
				if err := s.store.RemoveFile(filePath); err != nil {
					log.Printf("Error removing %s from index: %v", filePath, err)
				}
			}(path)
		}
	}
	return nil
}
func (s *Server) DidChangeWorkspaceFolders(ctx context.Context, params *protocol.DidChangeWorkspaceFoldersParams) error {
	return nil
}
func (s *Server) DocumentColor(ctx context.Context, params *protocol.DocumentColorParams) ([]protocol.ColorInformation, error) {
	return nil, nil
}
func (s *Server) DocumentHighlight(ctx context.Context, params *protocol.DocumentHighlightParams) ([]protocol.DocumentHighlight, error) {
	return nil, nil
}
func (s *Server) DocumentLink(ctx context.Context, params *protocol.DocumentLinkParams) ([]protocol.DocumentLink, error) {
	return nil, nil
}
func (s *Server) DocumentLinkResolve(ctx context.Context, params *protocol.DocumentLink) (*protocol.DocumentLink, error) {
	return nil, nil
}
func (s *Server) DocumentSymbol(ctx context.Context, params *protocol.DocumentSymbolParams) ([]interface{}, error) {
	return nil, nil
}
func (s *Server) ExecuteCommand(ctx context.Context, params *protocol.ExecuteCommandParams) (interface{}, error) {
	return nil, nil
}
func (s *Server) FoldingRanges(ctx context.Context, params *protocol.FoldingRangeParams) ([]protocol.FoldingRange, error) {
	return nil, nil
}
func (s *Server) Formatting(ctx context.Context, params *protocol.DocumentFormattingParams) ([]protocol.TextEdit, error) {
	return nil, nil
}
func (s *Server) Hover(ctx context.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
	docURI := string(params.TextDocument.URI)

	text, ok := s.docs.Get(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := int(params.Position.Character)

	if lineNum >= len(lines) {
		return nil, nil
	}

	expr := ExtractExpression(lines[lineNum], col)
	if expr == "" {
		return nil, nil
	}

	if strings.Contains(expr, "__MODULE__") {
		for _, l := range lines {
			if m := parser.DefmoduleRe.FindStringSubmatch(l); m != nil {
				expr = strings.ReplaceAll(expr, "__MODULE__", m[1])
				break
			}
		}
	}

	moduleRef, functionName := ExtractModuleAndFunction(expr)
	aliases := ExtractAliases(text)

	if moduleRef == "" {
		if functionName == "" {
			return nil, nil
		}

		if line, found := FindFunctionDefinition(text, functionName); found {
			return s.hoverFromBuffer(text, line-1)
		}

		for _, mod := range ExtractImports(text) {
			var results []store.LookupResult
			var err error
			if s.followDelegates {
				results, err = s.store.LookupFollowDelegate(mod, functionName)
			} else {
				results, err = s.store.LookupFunction(mod, functionName)
			}
			if err != nil || len(results) == 0 {
				continue
			}
			return s.hoverFromFile(functionName, results[0])
		}

		// Check use-injected imports and inline defs
		if results := s.lookupThroughUse(text, functionName, aliases); len(results) > 0 {
			return s.hoverFromFile(functionName, results[0])
		}

		// Kernel is always imported — fall back to it last
		if results, err := s.store.LookupFollowDelegate("Kernel", functionName); err == nil && len(results) > 0 {
			return s.hoverFromFile(functionName, results[0])
		}

		return nil, nil
	}

	fullModule := resolveModule(moduleRef, aliases)

	if functionName != "" {
		var results []store.LookupResult
		var err error
		if s.followDelegates {
			results, err = s.store.LookupFollowDelegate(fullModule, functionName)
		} else {
			results, err = s.store.LookupFunction(fullModule, functionName)
		}
		if err == nil && len(results) > 0 {
			return s.hoverFromFile(functionName, results[0])
		}
	}

	results, err := s.store.LookupModule(fullModule)
	if err != nil || len(results) == 0 {
		return nil, nil
	}
	return s.hoverFromFile("", results[0])
}
func (s *Server) Implementation(ctx context.Context, params *protocol.ImplementationParams) ([]protocol.Location, error) {
	return nil, nil
}
func (s *Server) OnTypeFormatting(ctx context.Context, params *protocol.DocumentOnTypeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, nil
}
func (s *Server) PrepareRename(ctx context.Context, params *protocol.PrepareRenameParams) (*protocol.Range, error) {
	return nil, nil
}
func (s *Server) RangeFormatting(ctx context.Context, params *protocol.DocumentRangeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, nil
}
func (s *Server) References(ctx context.Context, params *protocol.ReferenceParams) ([]protocol.Location, error) {
	return nil, nil
}
func (s *Server) Rename(ctx context.Context, params *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	return nil, nil
}
func (s *Server) SignatureHelp(ctx context.Context, params *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	return nil, nil
}
func (s *Server) Symbols(ctx context.Context, params *protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	return nil, nil
}
func (s *Server) TypeDefinition(ctx context.Context, params *protocol.TypeDefinitionParams) ([]protocol.Location, error) {
	return nil, nil
}
func (s *Server) WillSave(ctx context.Context, params *protocol.WillSaveTextDocumentParams) error {
	return nil
}
func (s *Server) WillSaveWaitUntil(ctx context.Context, params *protocol.WillSaveTextDocumentParams) ([]protocol.TextEdit, error) {
	return nil, nil
}
func (s *Server) ShowDocument(ctx context.Context, params *protocol.ShowDocumentParams) (*protocol.ShowDocumentResult, error) {
	return nil, nil
}
func (s *Server) WillCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, nil
}
func (s *Server) DidCreateFiles(ctx context.Context, params *protocol.CreateFilesParams) error {
	return nil
}
func (s *Server) WillRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, nil
}
func (s *Server) DidRenameFiles(ctx context.Context, params *protocol.RenameFilesParams) error {
	return nil
}
func (s *Server) WillDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) (*protocol.WorkspaceEdit, error) {
	return nil, nil
}
func (s *Server) DidDeleteFiles(ctx context.Context, params *protocol.DeleteFilesParams) error {
	return nil
}
func (s *Server) CodeLensRefresh(ctx context.Context) error { return nil }
func (s *Server) PrepareCallHierarchy(ctx context.Context, params *protocol.CallHierarchyPrepareParams) ([]protocol.CallHierarchyItem, error) {
	return nil, nil
}
func (s *Server) IncomingCalls(ctx context.Context, params *protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	return nil, nil
}
func (s *Server) OutgoingCalls(ctx context.Context, params *protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	return nil, nil
}
func (s *Server) SemanticTokensFull(ctx context.Context, params *protocol.SemanticTokensParams) (*protocol.SemanticTokens, error) {
	return nil, nil
}
func (s *Server) SemanticTokensFullDelta(ctx context.Context, params *protocol.SemanticTokensDeltaParams) (interface{}, error) {
	return nil, nil
}
func (s *Server) SemanticTokensRange(ctx context.Context, params *protocol.SemanticTokensRangeParams) (*protocol.SemanticTokens, error) {
	return nil, nil
}
func (s *Server) SemanticTokensRefresh(ctx context.Context) error { return nil }
func (s *Server) LinkedEditingRange(ctx context.Context, params *protocol.LinkedEditingRangeParams) (*protocol.LinkedEditingRanges, error) {
	return nil, nil
}
func (s *Server) Moniker(ctx context.Context, params *protocol.MonikerParams) ([]protocol.Moniker, error) {
	return nil, nil
}
func (s *Server) Request(ctx context.Context, method string, params interface{}) (interface{}, error) {
	return nil, nil
}
