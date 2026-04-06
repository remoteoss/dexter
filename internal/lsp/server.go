package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
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
	"gitlab.com/remote-com/employ-starbase/dexter/internal/treesitter"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/version"
)

// optBinding represents a dynamic import/use in __using__ driven by opts.
// For example: `mod = Keyword.get(opts, :mod, Mox)` followed by `import unquote(mod)`
// produces: {optKey: "mod", defaultMod: "Mox", kind: "import"}.
type optBinding struct {
	optKey     string // keyword key in opts (e.g. "mod")
	defaultMod string // default module if opt not provided (e.g. "Mox"); empty if none
	kind       string // "import" or "use"
}

// usingCacheEntry holds the full parsed result of a module's defmacro __using__
// body, keyed by module name. Storing filePath avoids a LookupModule query on
// cache hits; mtime invalidates the entry when the source file changes.
type usingCacheEntry struct {
	mtime       int64
	filePath    string
	imports     []string               // modules imported in __using__, source order
	inlineDefs  map[string][]inlineDef // function name → inline defs in quote do block
	transUses   []string               // modules used inside __using__ body (double-use chains)
	optBindings []optBinding           // dynamic imports/uses resolved from opts
}

type Server struct {
	store           *store.Store
	docs            *DocumentStore
	projectRoot     string
	explicitRoot    bool // true when projectRoot was provided via CLI, not inferred from Initialize
	stdlibRoot      string
	initialized     bool
	client          protocol.Client
	followDelegates bool
	debug           bool
	mixBin          string // resolved path to the mix binary

	formatters   map[string]*formatterProcess // formatterExs path → persistent formatter
	formattersMu sync.Mutex

	usingCache   map[string]*usingCacheEntry // module name → parsed __using__ result
	usingCacheMu sync.RWMutex

	depsCache   map[string]bool // dir → whether files in that dir are deps
	depsCacheMu sync.RWMutex

	conn                  jsonrpc2.Conn // raw connection for server-initiated requests not on the Client interface
	showDocumentSupported bool          // client supports window/showDocument (LSP 3.16+)

	reindexing sync.Mutex // serializes concurrent backgroundReindex calls
}

func (s *Server) debugf(format string, args ...interface{}) {
	if s.debug {
		log.Printf("[debug] "+format, args...)
	}
}

func (s *Server) debugNow() time.Time {
	if s.debug {
		return time.Now()
	}
	return time.Time{}
}

func NewServer(s *store.Store, projectRoot string) *Server {
	return &Server{
		store:           s,
		docs:            NewDocumentStore(),
		projectRoot:     projectRoot,
		explicitRoot:    projectRoot != "",
		followDelegates: true,
		usingCache:      make(map[string]*usingCacheEntry),
		depsCache:       make(map[string]bool),
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
	server.conn = conn

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
		if !s.reindexing.TryLock() {
			return
		}
		defer s.reindexing.Unlock()

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
		walkAndIndex := func(root string, indexRefs bool) {
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

				defs, refs, err := parser.ParseFile(path)
				if err != nil {
					return nil
				}
				if !indexRefs {
					refs = nil
				}
				if err := s.store.IndexFileWithRefs(path, defs, refs); err != nil {
					log.Printf("Warning: reindex %s: %v", path, err)
				}
				reindexed++
				return nil
			})
		}

		// Index stdlib first (definitions only).
		if s.stdlibRoot != "" {
			walkAndIndex(s.stdlibRoot, false)
		}

		walkAndIndex(s.projectRoot, true)

		// Prune store entries for files no longer on disk
		if storedPaths, err := s.store.ListFilePaths(); err == nil {
			var toRemove []string
			for _, storedPath := range storedPaths {
				if _, ok := seen[storedPath]; !ok {
					toRemove = append(toRemove, storedPath)
				}
			}
			if len(toRemove) > 0 {
				_ = s.store.RemoveFiles(toRemove)
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
	if !s.explicitRoot {
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
	}

	var explicitStdlibPath string
	if opts, ok := params.InitializationOptions.(map[string]interface{}); ok {
		if v, ok := opts["followDelegates"].(bool); ok {
			s.followDelegates = v
		}
		if v, ok := opts["stdlibPath"].(string); ok {
			explicitStdlibPath = v
		}
		if v, ok := opts["debug"].(bool); ok {
			s.debug = v
		}
	}
	if os.Getenv("DEXTER_DEBUG") == "true" {
		s.debug = true
	}

	log.Printf("Initialize: projectRoot=%s debug=%v", s.projectRoot, s.debug)

	if root, ok := stdlib.Resolve(s.store, explicitStdlibPath); ok {
		s.stdlibRoot = root
		log.Printf("Elixir stdlib at: %s", root)

		// Derive mix binary from the same Elixir install
		mixBin := filepath.Join(root, "..", "bin", "mix")
		if resolved, err := filepath.Abs(mixBin); err == nil {
			mixBin = resolved
		}
		if _, err := os.Stat(mixBin); err == nil {
			s.mixBin = mixBin
			log.Printf("Mix binary at: %s", mixBin)
		}
	} else {
		log.Printf("Could not detect Elixir stdlib (set stdlibPath in initializationOptions or DEXTER_ELIXIR_LIB_ROOT)")
		if s.client != nil {
			_ = s.client.ShowMessage(context.Background(), &protocol.ShowMessageParams{
				Type:    protocol.MessageTypeWarning,
				Message: "Dexter: could not detect Elixir stdlib — stdlib modules (Enum, String, etc.) won't resolve. Set stdlibPath in initializationOptions or DEXTER_ELIXIR_LIB_ROOT.",
			})
		}
	}

	// Fallback: find mix in PATH
	if s.mixBin == "" {
		if p, err := exec.LookPath("mix"); err == nil {
			s.mixBin = p
			log.Printf("Mix binary at: %s (PATH fallback)", p)
		} else {
			log.Printf("Could not find mix binary — formatting will not work")
		}
	}

	if !s.initialized {
		s.initialized = true
		s.backgroundReindex()
		s.watchGitHead()
		s.periodicReindex()
	}

	if params.Capabilities.Window != nil && params.Capabilities.Window.ShowDocument != nil {
		s.showDocumentSupported = params.Capabilities.Window.ShowDocument.Support
	}

	result := &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: protocol.TextDocumentSyncOptions{
				OpenClose:         true,
				Change:            protocol.TextDocumentSyncKindFull,
				WillSaveWaitUntil: true,
				Save: &protocol.SaveOptions{
					IncludeText: false,
				},
			},
			DefinitionProvider:         true,
			TypeDefinitionProvider:     true,
			DeclarationProvider:        true,
			ImplementationProvider:     true,
			ReferencesProvider:         true,
			DocumentFormattingProvider: true,
			HoverProvider:              true,
			DocumentHighlightProvider:  true,
			DocumentSymbolProvider:     true,
			WorkspaceSymbolProvider:    true,
			FoldingRangeProvider:       true,
			CodeActionProvider:         true,
			RenameProvider:             &protocol.RenameOptions{PrepareProvider: true},
			CallHierarchyProvider:      true,
			CompletionProvider: &protocol.CompletionOptions{
				TriggerCharacters: []string{"."},
				ResolveProvider:   true,
			},
			SignatureHelpProvider: &protocol.SignatureHelpOptions{
				TriggerCharacters:   []string{"(", ","},
				RetriggerCharacters: []string{")"},
			},
		},
		ServerInfo: &protocol.ServerInfo{
			Name:    "dexter",
			Version: version.Version,
		},
	}
	s.debugf("Initialize: capabilities: %+v", result.Capabilities)
	return result, nil
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
	s.closeFormatters()
	return nil
}

func (s *Server) Exit(ctx context.Context) error {
	os.Exit(0)
	return nil
}

// === Document Sync ===

func (s *Server) DidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error {
	s.docs.Set(string(params.TextDocument.URI), params.TextDocument.Text)

	// Eagerly start the persistent formatter so the first format is instant.
	// Skip deps and stdlib files — we don't format those.
	path := uriToPath(params.TextDocument.URI)
	if path != "" && isFormattableFile(path) && s.isProjectFile(path) && !s.isDepsFile(path) {
		go func() {
			if mixRoot := findMixRoot(filepath.Dir(path)); mixRoot != "" {
				formatterExs := findFormatterConfig(path, mixRoot)
				_, _ = s.getFormatter(mixRoot, formatterExs)
			}
		}()
	}

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
		defs, refs, err := parser.ParseFile(path)
		if err != nil {
			log.Printf("Error parsing %s: %v", path, err)
			return
		}

		if err := s.store.IndexFileWithRefs(path, defs, refs); err != nil {
			log.Printf("Error indexing %s: %v", path, err)
		}
	}()

	return nil
}

func (s *Server) isProjectFile(path string) bool {
	cleaned := filepath.Clean(path)
	return strings.HasPrefix(cleaned, s.projectRoot+string(os.PathSeparator))
}

func isFormattableFile(path string) bool {
	ext := filepath.Ext(path)
	return ext == ".ex" || ext == ".exs" || ext == ".heex"
}

func (s *Server) mixCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	bin := s.mixBin
	if bin == "" {
		bin = "mix"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	return cmd
}

// === Definition ===

func (s *Server) Definition(ctx context.Context, params *protocol.DefinitionParams) ([]protocol.Location, error) {
	docURI := string(params.TextDocument.URI)
	if s.debug {
		t0 := time.Now()
		s.debugf("Definition request: uri=%s line=%d col=%d", docURI, params.Position.Line, params.Position.Character)
		defer func() { s.debugf("Definition: total %s", time.Since(t0).Round(time.Microsecond)) }()
	}

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
	aliases := ExtractAliasesInScope(text, lineNum)
	s.debugf("Definition: expr=%q module=%q function=%q", expr, moduleRef, functionName)

	// Bare identifier — check variable first (cheap tree-sitter lookup), then functions
	if moduleRef == "" {
		if functionName == "" {
			return nil, nil
		}

		// Variable go-to-definition via tree-sitter.
		// The first occurrence in scope is the definition (pattern/assignment).
		if tree, src, ok := s.docs.GetTree(docURI); ok {
			if occs := treesitter.FindVariableOccurrencesWithTree(tree.RootNode(), src, uint(lineNum), uint(col)); len(occs) > 0 {
				s.debugf("Definition: returning variable definition at line %d", occs[0].Line)
				return []protocol.Location{{
					URI:   params.TextDocument.URI,
					Range: lineRange(int(occs[0].Line)),
				}}, nil
			}
		}

		currentModule := firstDefmodule(lines)
		fullModule := s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, lines, lineNum, functionName, aliases)
		s.debugf("Definition: resolved bare %q -> %q", functionName, fullModule)
		if fullModule == "" {
			s.debugf("Definition: could not resolve bare function %q", functionName)
			return nil, nil
		}

		// Current module — return buffer location directly (works before indexing)
		if fullModule == currentModule {
			if line, found := FindFunctionDefinition(text, functionName); found {
				return []protocol.Location{{
					URI:   params.TextDocument.URI,
					Range: lineRange(line - 1),
				}}, nil
			}
		}

		// Look up via store
		var results []store.LookupResult
		var err error
		if s.followDelegates {
			results, err = s.store.LookupFollowDelegate(fullModule, functionName)
		} else {
			results, err = s.store.LookupFunction(fullModule, functionName)
		}
		if err == nil && len(results) > 0 {
			s.debugf("Definition: found %d result(s) in store for %s.%s", len(results), fullModule, functionName)
			return storeResultsToLocations(filterOutTypes(results)), nil
		}

		// fullModule may not directly define the function — try its use chain
		// (e.g. `import MyApp.Factory` where MyApp.Factory uses ExMachina).
		if results := s.lookupThroughUseOf(fullModule, functionName); len(results) > 0 {
			s.debugf("Definition: found %d result(s) via use chain of %s for %s", len(results), fullModule, functionName)
			return storeResultsToLocations(filterOutTypes(results)), nil
		}

		// Fallback for use-chain inline defs (not stored as module definitions)
		if results := s.lookupThroughUse(text, functionName, aliases); len(results) > 0 {
			s.debugf("Definition: found %d result(s) via current file use chain for %s", len(results), functionName)
			return storeResultsToLocations(filterOutTypes(results)), nil
		}

		s.debugf("Definition: no result found for bare function %q in module %q", functionName, fullModule)

		return nil, nil
	}

	// Module.function call — resolve aliases (including implicit nested-module aliases)
	fullModule := s.resolveModuleWithNesting(moduleRef, aliases, uriToPath(params.TextDocument.URI), lineNum)
	s.debugf("Definition: qualified call resolved %q -> %q", moduleRef, fullModule)

	if functionName != "" {
		var results []store.LookupResult
		var err error
		if s.followDelegates {
			results, err = s.store.LookupFollowDelegate(fullModule, functionName)
		} else {
			results, err = s.store.LookupFunction(fullModule, functionName)
		}
		if err == nil && len(results) > 0 {
			s.debugf("Definition: found %d result(s) in store for %s.%s", len(results), fullModule, functionName)
			return storeResultsToLocations(filterOutTypes(results)), nil
		}
		// Not directly defined — the function may have been injected by a
		// `use` macro in fullModule's source (e.g. Oban.Worker injects `new`).
		if results := s.lookupThroughUseOf(fullModule, functionName); len(results) > 0 {
			s.debugf("Definition: found %d result(s) via use chain of %s for %s", len(results), fullModule, functionName)
			return storeResultsToLocations(results), nil
		}
		s.debugf("Definition: no result for %s.%s", fullModule, functionName)
	}

	// Fall back to module (fullModule already resolved via nesting above)
	results, err := s.store.LookupModule(fullModule)
	if err != nil || len(results) == 0 {
		return nil, nil
	}
	return storeResultsToLocations(results), nil
}

func storeResultsToLocations(results []store.LookupResult) []protocol.Location {
	type locKey struct {
		filePath string
		line     int
	}
	seen := make(map[locKey]struct{}, len(results))
	var locations []protocol.Location
	for _, r := range results {
		k := locKey{r.FilePath, r.Line}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		locations = append(locations, protocol.Location{
			URI:   uri.File(r.FilePath),
			Range: lineRange(r.Line - 1), // LSP lines are 0-based
		})
	}
	return locations
}

var typeKinds = map[string]bool{"type": true, "typep": true, "opaque": true}

func filterOutTypes(results []store.LookupResult) []store.LookupResult {
	var nonTypes []store.LookupResult
	for _, r := range results {
		if !typeKinds[r.Kind] {
			nonTypes = append(nonTypes, r)
		}
	}
	if len(nonTypes) > 0 {
		return nonTypes
	}
	return results
}

func lineRange(line int) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{Line: uint32(line), Character: 0},
		End:   protocol.Position{Line: uint32(line), Character: 0},
	}
}

// nthLine returns the n-th line (0-based) from text without splitting the
// entire string. The bool indicates whether the line was found.
func nthLine(text string, n int) (string, bool) {
	start := 0
	for i := 0; i < n; i++ {
		idx := strings.IndexByte(text[start:], '\n')
		if idx < 0 {
			return "", false
		}
		start += idx + 1
	}
	end := strings.IndexByte(text[start:], '\n')
	if end < 0 {
		return text[start:], true
	}
	return text[start : start+end], true
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

// findMixRoot walks up from dir looking for the nearest mix.exs.
func findMixRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "mix.exs")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
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
	docURI := string(params.TextDocument.URI)
	text, ok := s.docs.Get(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Range.Start.Line)
	if lineNum >= len(lines) {
		return nil, nil
	}

	// Find the full dotted expression at the cursor so that "DocuSign.Client.request"
	// gives us the complete module reference, not just the segment under the cursor.
	col := int(params.Range.Start.Character)
	fullExpr := ExtractFullExpression(lines[lineNum], col)
	if fullExpr == "" {
		return nil, nil
	}

	moduleRef, _ := ExtractModuleAndFunction(fullExpr)
	if moduleRef == "" {
		return nil, nil
	}

	aliases := ExtractAliasesInScope(text, lineNum)

	// Check if the first segment is already aliased — if so, the reference
	// already resolves and no code action is needed.
	firstSegment := moduleRef
	if dot := strings.IndexByte(moduleRef, '.'); dot >= 0 {
		firstSegment = moduleRef[:dot]
	}
	if _, aliased := aliases[firstSegment]; aliased {
		return nil, nil
	}

	insertLine, indent := findAliasInsertPoint(lines)
	var actions []protocol.CodeAction

	// Case 1: Fully qualified module in the store (e.g. "MyApp.RandomAPI.Client").
	// Offer to alias it and replace the usage with the short form.
	if strings.Contains(moduleRef, ".") {
		if defResults, err := s.store.LookupModule(moduleRef); err == nil && len(defResults) > 0 {
			lastSegment := moduleLastSegment(moduleRef)
			aliasText := indent + "alias " + moduleRef + "\n"

			// Replace the module part of the expression on the current line
			// with just the last segment (e.g. "MyApp.RandomAPI.Client" → "Client").
			// Use the expression start column rather than strings.Index so that
			// duplicate module references on the same line are not misidentified.
			_, exprStart := extractExpressionBounds(lines[lineNum], col)
			var edits []protocol.TextEdit
			// Insert the alias line
			edits = append(edits, protocol.TextEdit{
				Range: protocol.Range{
					Start: protocol.Position{Line: uint32(insertLine), Character: 0},
					End:   protocol.Position{Line: uint32(insertLine), Character: 0},
				},
				NewText: aliasText,
			})
			// Replace the qualified module reference with the short name
			edits = append(edits, protocol.TextEdit{
				Range: protocol.Range{
					Start: protocol.Position{Line: uint32(lineNum), Character: uint32(exprStart)},
					End:   protocol.Position{Line: uint32(lineNum), Character: uint32(exprStart + len(moduleRef))},
				},
				NewText: lastSegment,
			})
			actions = append(actions, protocol.CodeAction{
				Title: "Add alias " + moduleRef,
				Kind:  protocol.QuickFix,
				Edit: &protocol.WorkspaceEdit{
					Changes: map[protocol.DocumentURI][]protocol.TextEdit{
						protocol.DocumentURI(docURI): edits,
					},
				},
			})
		}
	}

	// Case 2: Short or partially-qualified name not in the store
	// (e.g. "Client" or "DocuSign.Client"). Search for matching modules.
	if len(actions) == 0 {
		results, err := s.store.SearchModulesBySuffix(moduleRef)
		if err == nil {
			for _, r := range results {
				if r.Module == moduleRef {
					continue
				}
				suffix := "." + moduleRef
				if !strings.HasSuffix(r.Module, suffix) {
					continue
				}

				aliasTarget := r.Module
				if strings.Contains(moduleRef, ".") {
					aliasTarget = strings.TrimSuffix(r.Module, moduleRef[len(firstSegment):])
				}

				aliasText := indent + "alias " + aliasTarget + "\n"
				actions = append(actions, protocol.CodeAction{
					Title: "Add alias " + aliasTarget,
					Kind:  protocol.QuickFix,
					Edit: &protocol.WorkspaceEdit{
						Changes: map[protocol.DocumentURI][]protocol.TextEdit{
							protocol.DocumentURI(docURI): {
								{
									Range: protocol.Range{
										Start: protocol.Position{Line: uint32(insertLine), Character: 0},
										End:   protocol.Position{Line: uint32(insertLine), Character: 0},
									},
									NewText: aliasText,
								},
							},
						},
					},
				})

				if len(actions) >= 5 {
					break
				}
			}
		}
	}

	return actions, nil
}

// findAliasInsertPoint returns the 0-based line number where a new alias should
// be inserted and the indentation prefix to use. Places it after the last
// existing alias/import/use block, matching their indentation.
func findAliasInsertPoint(lines []string) (insertLine int, indent string) {
	lastDirective := -1
	lastIndent := "  " // default to two spaces
	moduleLineFound := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "defmodule ") {
			moduleLineFound = true
			if lastDirective < 0 {
				lastDirective = i
			}
			continue
		}
		if moduleLineFound {
			if strings.HasPrefix(trimmed, "alias ") || strings.HasPrefix(trimmed, "import ") ||
				strings.HasPrefix(trimmed, "use ") || strings.HasPrefix(trimmed, "require ") {
				lastDirective = i
				lastIndent = line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			}
		}
	}
	if lastDirective >= 0 {
		return lastDirective + 1, lastIndent
	}
	return 0, lastIndent
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

	prefix, afterDot, prefixStartCol := ExtractCompletionContext(lines[lineNum], col)
	if prefix == "" && !afterDot {
		return nil, nil
	}

	// Range covering the already-typed prefix through cursor — used for
	// textEdit on module items so the editor replaces rather than appends.
	prefixRange := protocol.Range{
		Start: protocol.Position{Line: uint32(lineNum), Character: uint32(prefixStartCol)},
		End:   protocol.Position{Line: uint32(lineNum), Character: uint32(col)},
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
		seenModules := make(map[string]bool)

		addModuleItem := func(label, detail string) {
			if seenModules[label] {
				return
			}
			seenModules[label] = true
			items = append(items, protocol.CompletionItem{
				Label:  label,
				Kind:   protocol.CompletionItemKindModule,
				Detail: detail,
				TextEdit: &protocol.TextEdit{
					Range:   prefixRange,
					NewText: label,
				},
			})
		}

		for shortName, fullModule := range aliases {
			if strings.HasPrefix(shortName, moduleRef) {
				addModuleItem(shortName, fullModule)
			}
		}

		if parts := strings.SplitN(moduleRef, ".", 2); len(parts) == 2 {
			if resolved, ok := aliases[parts[0]]; ok {
				resolvedPrefix := resolved + "." + parts[1]
				aliasResults, err := s.store.SearchModules(resolvedPrefix)
				if err == nil {
					for _, r := range aliasResults {
						label := parts[0] + strings.TrimPrefix(r.Module, resolved)
						addModuleItem(label, r.Module)
					}
				}
			}
		}

		if dotIdx := strings.LastIndexByte(moduleRef, '.'); dotIdx >= 0 {
			parentModule := moduleRef[:dotIdx]
			segmentPrefix := moduleRef[dotIdx+1:]
			resolved := resolveModule(parentModule, aliases)
			segments, err := s.store.SearchSubmoduleSegments(resolved, segmentPrefix)
			if err == nil {
				for _, segment := range segments {
					label := parentModule + "." + segment
					addModuleItem(label, resolved+"."+segment)
				}
			}
		}

		results, err := s.store.SearchModules(moduleRef)
		if err != nil {
			return nil, nil
		}
		for _, r := range results {
			addModuleItem(r.Module, "module")
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

		// Variables in scope via tree-sitter
		var varsInScope []string
		if tree, src, ok := s.docs.GetTree(docURI); ok {
			varsInScope = treesitter.FindVariablesInScopeWithTree(tree.RootNode(), src, uint(lineNum), uint(col))
		}
		for _, varName := range varsInScope {
			if strings.HasPrefix(varName, funcPrefix) && !seen[varName] {
				seen[varName] = true
				items = append(items, protocol.CompletionItem{
					Label:  varName,
					Kind:   protocol.CompletionItemKindVariable,
					Detail: "variable",
				})
			}
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
	return s.cachedUsingWithPath(moduleName, "")
}

// cachedUsingWithPath is like cachedUsing but accepts a known file path to
// skip the LookupModule query on a cache miss.
func (s *Server) cachedUsingWithPath(moduleName, knownPath string) *usingCacheEntry {
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

	// Cache miss — use provided path or look up from the store
	filePath := knownPath
	if filePath == "" {
		modResults, err := s.store.LookupModule(moduleName)
		if err != nil || len(modResults) == 0 {
			return nil
		}
		filePath = modResults[0].FilePath
	}
	filePath = filepath.Clean(filePath)
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
	f, err := os.Open(filePath)
	if err != nil {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil
	}
	fileData, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return nil
	}
	imported, inlineDefs, transUses, optBindings := parseUsingBody(string(fileData))
	return &usingCacheEntry{
		mtime:       info.ModTime().UnixNano(),
		filePath:    filePath,
		imports:     imported,
		inlineDefs:  inlineDefs,
		transUses:   transUses,
		optBindings: optBindings,
	}
}

// lookupThroughUseOf looks up functionName through the `use` declarations in
// fullModule's source file. This handles qualified calls like M.func() where
// func is not defined directly in M but is injected by a macro M uses.
func (s *Server) lookupThroughUseOf(fullModule, functionName string) []store.LookupResult {
	modResults, err := s.store.LookupModule(fullModule)
	if err != nil || len(modResults) == 0 {
		return nil
	}
	fileText, _, ok := s.readFileText(modResults[0].FilePath)
	if !ok {
		return nil
	}
	return s.lookupThroughUse(fileText, functionName, ExtractAliases(fileText))
}

// lookupThroughUse searches for functionName in definitions injected by `use`
// declarations. Inline defs (defined directly in the quote do block) take
// priority over imported ones. Later `use` declarations shadow earlier ones.
// Transitive use chains (use inside __using__ body) are followed recursively.
func (s *Server) lookupThroughUse(text, functionName string, aliases map[string]string) []store.LookupResult {
	useCalls := ExtractUsesWithOpts(text, aliases)
	visited := make(map[string]bool)

	for i := len(useCalls) - 1; i >= 0; i-- {
		if result := s.lookupInUsingEntry(useCalls[i].Module, functionName, useCalls[i].Opts, visited); result != nil {
			return result
		}
	}
	return nil
}

// lookupInUsingEntry resolves functionName through a single module's __using__
// body, then recurses into any transitive uses. The visited set prevents cycles.
// consumerOpts are the keyword args from the `use Module, key: Val` call and
// are used to resolve dynamic imports like `import unquote(mod)`.
func (s *Server) lookupInUsingEntry(moduleName, functionName string, consumerOpts map[string]string, visited map[string]bool) []store.LookupResult {
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

	// Static imports
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

	// Dynamic imports/uses driven by opts (e.g. `import unquote(mod)`)
	for _, b := range entry.optBindings {
		mod := consumerOpts[b.optKey]
		if mod == "" {
			mod = b.defaultMod
		}
		if mod == "" {
			continue
		}
		switch b.kind {
		case "import":
			var results []store.LookupResult
			var err error
			if s.followDelegates {
				results, err = s.store.LookupFollowDelegate(mod, functionName)
			} else {
				results, err = s.store.LookupFunction(mod, functionName)
			}
			if err == nil && len(results) > 0 {
				return results
			}
		case "use":
			if result := s.lookupInUsingEntry(mod, functionName, nil, visited); result != nil {
				return result
			}
		}
	}

	// Transitive uses: use Module inside the __using__ body (double-use chains)
	for k := len(entry.transUses) - 1; k >= 0; k-- {
		if result := s.lookupInUsingEntry(entry.transUses[k], functionName, nil, visited); result != nil {
			return result
		}
	}

	return nil
}

// resolveModuleViaUseChain returns the module name that provides functionName
// resolveModuleViaUseChainWithOpts is like resolveModuleViaUseChain but uses
// consumer-provided opts to resolve dynamic imports (e.g. `import unquote(mod)`).
func (s *Server) resolveModuleViaUseChainWithOpts(moduleName, functionName string, consumerOpts map[string]string, visited map[string]bool) string {
	if visited[moduleName] {
		return ""
	}
	visited[moduleName] = true

	entry := s.cachedUsing(moduleName)
	if entry == nil {
		return ""
	}

	for j := len(entry.imports) - 1; j >= 0; j-- {
		if results, err := s.store.LookupFunction(entry.imports[j], functionName); err == nil && len(results) > 0 {
			return entry.imports[j]
		}
	}

	for _, b := range entry.optBindings {
		mod := consumerOpts[b.optKey]
		if mod == "" {
			mod = b.defaultMod
		}
		if mod == "" {
			continue
		}
		switch b.kind {
		case "import":
			if results, err := s.store.LookupFunction(mod, functionName); err == nil && len(results) > 0 {
				return mod
			}
		case "use":
			if m := s.resolveModuleViaUseChainWithOpts(mod, functionName, nil, visited); m != "" {
				return m
			}
		}
	}

	for k := len(entry.transUses) - 1; k >= 0; k-- {
		if mod := s.resolveModuleViaUseChainWithOpts(entry.transUses[k], functionName, nil, visited); mod != "" {
			return mod
		}
	}

	return ""
}

// findModulesWhoseUsingImports returns modules whose __using__ chain
// (directly or transitively via use) imports targetModule. Follows the chain
// upward: if C.__using__ imports targetModule, and B.__using__ uses C, and
// A.__using__ uses B, then all of [C, B, A] are returned.
//
// Instead of searching all refs for the target module (expensive for common
// modules like String), this iterates over the small set of modules that
// define __using__ and checks their cached import/transUse lists.
func (s *Server) findModulesWhoseUsingImports(targetModule string) []string {
	usingModules, err := s.store.LookupUsingModules()
	if err != nil {
		return nil
	}

	// Load and validate __using__ entries concurrently, using file paths
	// from the index to skip per-module LookupModule queries on cache miss.
	// Parallelism helps because each entry requires an os.Stat call for
	// mtime validation (and possibly a file read on cache miss).
	type cached struct {
		module string
		entry  *usingCacheEntry
	}
	entries := make([]cached, len(usingModules))
	var wg sync.WaitGroup
	for i, um := range usingModules {
		wg.Add(1)
		go func(i int, mod, path string) {
			defer wg.Done()
			entry := s.cachedUsingWithPath(mod, path)
			if entry != nil {
				entries[i] = cached{mod, entry}
			}
		}(i, um.Module, um.FilePath)
	}
	wg.Wait()
	// Compact out nil entries.
	n := 0
	for _, c := range entries {
		if c.entry != nil {
			entries[n] = c
			n++
		}
	}
	entries = entries[:n]

	// Step 1: Find modules whose __using__ directly imports targetModule.
	seen := make(map[string]bool)
	var directInjectors []string
	for _, c := range entries {
		for _, imp := range c.entry.imports {
			if imp == targetModule {
				directInjectors = append(directInjectors, c.module)
				seen[c.module] = true
				break
			}
		}
	}

	if len(directInjectors) == 0 {
		return nil
	}

	// Step 2: Walk upward — find modules whose __using__ transitively uses
	// any of the direct injectors (via transUses in __using__ bodies).
	allInjectors := append([]string{}, directInjectors...)
	queue := append([]string{}, directInjectors...)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, c := range entries {
			if seen[c.module] {
				continue
			}
			for _, tu := range c.entry.transUses {
				if tu == current {
					seen[c.module] = true
					allInjectors = append(allInjectors, c.module)
					queue = append(queue, c.module)
					break
				}
			}
		}
	}

	return allInjectors
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

// firstDefmodule returns the first defmodule name found in the file, or "".
func firstDefmodule(lines []string) string {
	for _, l := range lines {
		if m := parser.DefmoduleRe.FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	return ""
}

// resolveBareFunctionModule finds the module that defines a bare function name.
// Mirrors the go-to-definition priority: current file modules → imports → use chains → Kernel.
// Callers should pass pre-computed aliases to avoid redundant ExtractAliases scans.
func (s *Server) resolveBareFunctionModule(filePath, text string, lines []string, lineNum int, functionName string, aliases map[string]string) string {
	// Check all modules in the current file with a single query, preferring
	// the one closest to the cursor line (handles sibling nested modules).
	if mod, ok := s.store.LookupFunctionInFile(filePath, functionName, lineNum+1); ok {
		return mod
	}

	// Explicit imports (direct definitions only — fast store lookup)
	imports := ExtractImports(text)
	for _, mod := range imports {
		if results, err := s.store.LookupFunction(mod, functionName); err == nil && len(results) > 0 {
			return mod
		}
	}

	// Use chains — use opts-aware resolution so `import unquote(mod)` patterns
	// resolve to the consumer-provided module rather than always using the default.
	for _, uc := range ExtractUsesWithOpts(text, aliases) {
		if mod := s.resolveModuleViaUseChainWithOpts(uc.Module, functionName, uc.Opts, map[string]bool{}); mod != "" {
			return mod
		}
	}

	// Kernel is always in scope
	if results, err := s.store.LookupFunction("Kernel", functionName); err == nil && len(results) > 0 {
		return "Kernel"
	}

	// Slow fallback: function may be injected into an imported module via its
	// own use chain (e.g. MyApp.Factory uses ExMachina, which injects `insert`).
	for _, mod := range imports {
		if results := s.lookupThroughUseOf(mod, functionName); len(results) > 0 {
			return mod
		}
	}

	return ""
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

// resolveModuleWithNesting resolves a module reference, falling back to the
// implicit alias created by nested defmodule declarations. In Elixir,
// `defmodule Inner do` inside `defmodule Outer do` creates an implicit alias
// Inner → Outer.Inner within Outer's scope.
func (s *Server) resolveModuleWithNesting(moduleRef string, aliases map[string]string, filePath string, lineNum int) string {
	resolved := resolveModule(moduleRef, aliases)

	// If the alias map resolved it, or it already looks fully qualified, use it
	if resolved != moduleRef {
		return resolved
	}
	if results, err := s.store.LookupModule(resolved); err == nil && len(results) > 0 {
		return resolved
	}

	// Try implicit alias: prepend enclosing parent module(s).
	// Walk up the nesting until we find a match.
	enclosing := s.store.LookupEnclosingModule(filePath, lineNum+1)
	for enclosing != "" {
		candidate := enclosing + "." + moduleRef
		if results, err := s.store.LookupModule(candidate); err == nil && len(results) > 0 {
			return candidate
		}
		// Move up one level: "A.B.C" → "A.B"
		if dot := strings.LastIndex(enclosing, "."); dot >= 0 {
			enclosing = enclosing[:dot]
		} else {
			break
		}
	}

	return resolved
}

func funcKey(name string, arity int) string {
	return name + "/" + strconv.Itoa(arity)
}

func applySnippet(item *protocol.CompletionItem, name string, arity int) {
	item.Label = fmt.Sprintf("%s/%d", name, arity)
	item.FilterText = name
	item.InsertTextFormat = protocol.InsertTextFormatSnippet
	if arity > 0 {
		item.InsertText = functionSnippet(name, arity)
	} else {
		item.InsertText = name + "()"
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
	case "type", "typep", "opaque":
		return protocol.CompletionItemKindTypeParameter
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
	docURI := string(params.TextDocument.URI)
	if s.debug {
		t0 := time.Now()
		s.debugf("Declaration request: uri=%s line=%d col=%d", docURI, params.Position.Line, params.Position.Character)
		defer func() { s.debugf("Declaration: total %s", time.Since(t0).Round(time.Microsecond)) }()
	}

	text, ok := s.docs.Get(docURI)
	if !ok {
		s.debugf("Declaration: document not open")
		return nil, nil
	}

	path := uriToPath(params.TextDocument.URI)
	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := int(params.Position.Character)

	if lineNum >= len(lines) {
		return nil, nil
	}

	// Use the enclosing function to get both name, arity, and module for precise matching.
	// Fall back to expression extraction if the cursor is not inside a function body.
	currentModule := ""
	functionName := ""
	arity := -1
	if mod, fn, ar, _, found := s.store.LookupEnclosingFunction(path, lineNum+1); found {
		currentModule = mod
		functionName = fn
		arity = ar
	}
	if functionName == "" {
		expr := ExtractExpression(lines[lineNum], col)
		if expr == "" {
			s.debugf("Declaration: no expression at cursor")
			return nil, nil
		}
		_, functionName = ExtractModuleAndFunction(expr)
		if functionName == "" {
			s.debugf("Declaration: no function name in expression")
			return nil, nil
		}
	}

	s.debugf("Declaration: module=%s function=%s arity=%d", currentModule, functionName, arity)

	appendCallbackLocations := func(locations []protocol.Location, behaviourModule string) []protocol.Location {
		callbacks, err := s.store.LookupCallbackDef(behaviourModule, functionName)
		if err != nil {
			return locations
		}
		s.debugf("Declaration: found %d callbacks in %s for %s", len(callbacks), behaviourModule, functionName)
		for _, cb := range callbacks {
			if arity >= 0 && cb.Arity != arity {
				continue
			}
			locations = append(locations, protocol.Location{
				URI:   uri.File(cb.FilePath),
				Range: lineRange(cb.Line - 1),
			})
		}
		return locations
	}

	var locations []protocol.Location

	// Check for callbacks defined in the current module itself (common pattern where
	// a module defines @callback and @impl def in the same file).
	if currentModule != "" {
		locations = appendCallbackLocations(locations, currentModule)
	}

	// Also check declared @behaviour and `use` modules for callbacks defined elsewhere.
	behaviours, err := s.store.LookupBehavioursForFile(path)
	s.debugf("Declaration: behaviours for file: %v (err=%v)", behaviours, err)
	if err == nil {
		for _, behaviour := range behaviours {
			if behaviour == currentModule {
				continue // already checked above
			}
			locations = appendCallbackLocations(locations, behaviour)
		}
	}

	// Fallback: walk the transitive use-chain (including modules surfaced via
	// keywordModuleRe from `Keyword.put_new/pop`) looking for @callback definitions.
	// This handles dynamic `use unquote(mod)` patterns where the concrete module
	// is specified as a keyword opt (e.g. oban_module: Oban.Pro.Worker).
	if len(locations) == 0 && functionName != "" {
		aliases := ExtractAliasesInScope(text, lineNum)
		if callbacks := s.findCallbacksViaUseChain(text, functionName, arity, aliases); len(callbacks) > 0 {
			s.debugf("Declaration: found %d callbacks via use-chain for %s/%d", len(callbacks), functionName, arity)
			for _, cb := range callbacks {
				locations = append(locations, protocol.Location{
					URI:   uri.File(cb.FilePath),
					Range: lineRange(cb.Line - 1),
				})
			}
		}
	}

	// Last resort: if an @impl annotation is present but the chain still yielded
	// nothing, do a project-wide callback search by name/arity.
	if len(locations) == 0 && functionName != "" {
		implModule := extractImplAnnotation(lines, lineNum)
		if implModule == "true" {
			s.debugf("Declaration: @impl true global fallback for %s/%d", functionName, arity)
			if callbacks, err := s.store.LookupCallbackDefGlobal(functionName, arity); err == nil {
				for _, cb := range callbacks {
					locations = append(locations, protocol.Location{
						URI:   uri.File(cb.FilePath),
						Range: lineRange(cb.Line - 1),
					})
				}
			}
		} else if implModule != "" {
			s.debugf("Declaration: @impl %s fallback for %s/%d", implModule, functionName, arity)
			locations = appendCallbackLocations(locations, implModule)
		}
	}

	s.debugf("Declaration: returning %d locations", len(locations))
	return locations, nil
}

// extractImplAnnotation scans backward from lineNum for an @impl annotation
// on the preceding non-blank lines. Returns the module name ("true" for @impl true,
// the module string for @impl SomeModule, or "" if none found).
func extractImplAnnotation(lines []string, lineNum int) string {
	for i := lineNum - 1; i >= 0 && i >= lineNum-3; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "@impl ") {
			return strings.TrimSpace(trimmed[len("@impl "):])
		}
		// Stop at the first non-blank, non-@impl line
		break
	}
	return ""
}

// findCallbacksViaUseChain walks the transitive use-chain of the file (using
// the same cached __using__ entries as go-to-definition) and collects @callback
// definitions matching functionName/arity. This resolves dynamic `use unquote(mod)`
// patterns where the concrete module appears as a keyword opt in the chain.
func (s *Server) findCallbacksViaUseChain(text, functionName string, arity int, aliases map[string]string) []store.CallbackResult {
	uses := ExtractUses(text)
	visited := make(map[string]bool)
	var results []store.CallbackResult
	for _, moduleName := range uses {
		moduleName = resolveModule(moduleName, aliases)
		s.collectCallbacksInChain(moduleName, functionName, arity, visited, &results)
	}
	return results
}

func (s *Server) collectCallbacksInChain(moduleName, functionName string, arity int, visited map[string]bool, results *[]store.CallbackResult) {
	if visited[moduleName] {
		return
	}
	visited[moduleName] = true

	if callbacks, err := s.store.LookupCallbackDef(moduleName, functionName); err == nil {
		for _, cb := range callbacks {
			if arity < 0 || cb.Arity == arity {
				*results = append(*results, cb)
			}
		}
	}

	entry := s.cachedUsing(moduleName)
	if entry == nil {
		return
	}
	for _, transModule := range entry.transUses {
		s.collectCallbacksInChain(transModule, functionName, arity, visited, results)
	}
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
				defs, refs, err := parser.ParseFile(filePath)
				if err != nil {
					log.Printf("Error parsing %s: %v", filePath, err)
					return
				}

				if err := s.store.IndexFileWithRefs(filePath, defs, refs); err != nil {
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
	docURI := string(params.TextDocument.URI)
	text, ok := s.docs.Get(docURI)
	if !ok {
		return nil, nil
	}

	lineNum := int(params.Position.Line)
	col := int(params.Position.Character)

	tree, src, hasTree := s.docs.GetTree(docURI)
	if !hasTree {
		return nil, nil
	}
	root := tree.RootNode()

	// Try scope-aware variable highlight first
	if occs := treesitter.FindVariableOccurrencesWithTree(root, src, uint(lineNum), uint(col)); len(occs) > 0 {
		var highlights []protocol.DocumentHighlight
		for _, occ := range occs {
			highlights = append(highlights, protocol.DocumentHighlight{
				Range: protocol.Range{
					Start: protocol.Position{Line: uint32(occ.Line), Character: uint32(occ.StartCol)},
					End:   protocol.Position{Line: uint32(occ.Line), Character: uint32(occ.EndCol)},
				},
				Kind: protocol.DocumentHighlightKindText,
			})
		}
		return highlights, nil
	}

	// Extract the cursor's line without splitting the entire document
	line, ok := nthLine(text, lineNum)
	if !ok || line == "" {
		return nil, nil
	}

	expr := ExtractExpression(line, col)
	if expr == "" {
		return nil, nil
	}

	moduleRef, functionName := ExtractModuleAndFunction(expr)
	token := functionName
	if token == "" {
		token = moduleLastSegment(moduleRef)
	}
	if token == "" {
		return nil, nil
	}

	// Reuse the same parsed tree for token occurrences
	occs := treesitter.FindTokenOccurrencesWithTree(root, src, token)
	if len(occs) == 0 {
		return nil, nil
	}

	var highlights []protocol.DocumentHighlight
	for _, occ := range occs {
		highlights = append(highlights, protocol.DocumentHighlight{
			Range: protocol.Range{
				Start: protocol.Position{Line: uint32(occ.Line), Character: uint32(occ.StartCol)},
				End:   protocol.Position{Line: uint32(occ.Line), Character: uint32(occ.EndCol)},
			},
			Kind: protocol.DocumentHighlightKindText,
		})
	}
	return highlights, nil
}
func (s *Server) DocumentLink(ctx context.Context, params *protocol.DocumentLinkParams) ([]protocol.DocumentLink, error) {
	return nil, nil
}
func (s *Server) DocumentLinkResolve(ctx context.Context, params *protocol.DocumentLink) (*protocol.DocumentLink, error) {
	return nil, nil
}
func (s *Server) DocumentSymbol(ctx context.Context, params *protocol.DocumentSymbolParams) ([]interface{}, error) {
	docURI := string(params.TextDocument.URI)
	text, ok := s.docs.Get(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lastLine := len(lines) - 1

	type symbolEntry struct {
		symbol    protocol.DocumentSymbol
		module    string // owning module name (empty for top-level modules)
		parentIdx int    // index of parent entry (-1 for top-level)
	}

	type blockFrame struct {
		name     string // module full name, or "" for functions
		indent   int
		entryIdx int // index into entries slice
	}

	var entries []symbolEntry
	var moduleStack []blockFrame // defmodule/defprotocol/defimpl
	var funcStack []blockFrame   // def/defp/defmacro/describe/test/etc with do...end bodies
	inHeredoc := false

	// currentParentIdx returns the index of the innermost enclosing block.
	// funcStack entries (describe blocks) take priority over moduleStack.
	currentParentIdx := func() int {
		if len(funcStack) > 0 {
			return funcStack[len(funcStack)-1].entryIdx
		}
		if len(moduleStack) > 0 {
			return moduleStack[len(moduleStack)-1].entryIdx
		}
		return -1
	}

	for lineIdx, line := range lines {
		if strings.IndexByte(line, '"') >= 0 {
			quoteCount := strings.Count(line, `"""`)
			if quoteCount > 0 {
				if quoteCount >= 2 {
					continue
				}
				inHeredoc = !inHeredoc
				continue
			}
		}
		if strings.IndexByte(line, '\'') >= 0 {
			quoteCount := strings.Count(line, `'''`)
			if quoteCount > 0 {
				if quoteCount >= 2 {
					continue
				}
				inHeredoc = !inHeredoc
				continue
			}
		}
		if inHeredoc {
			continue
		}

		trimStart := 0
		for trimStart < len(line) && (line[trimStart] == ' ' || line[trimStart] == '\t') {
			trimStart++
		}
		if trimStart >= len(line) {
			continue
		}
		first := line[trimStart]
		rest := line[trimStart:]

		// Fast first-character dispatch
		if first != 'd' && first != 'e' && first != '@' && (first < 'a' || first > 'z') {
			continue
		}

		// 'e' — pop stacks on "end"
		if first == 'e' {
			if len(rest) >= 3 && rest[0] == 'e' && rest[1] == 'n' && rest[2] == 'd' &&
				(len(rest) == 3 || rest[3] == ' ' || rest[3] == '\t' || rest[3] == '\r') {
				trimmedEnd := strings.TrimRight(rest, " \t\r")
				if trimmedEnd == "end" {
					endPos := protocol.Position{Line: uint32(lineIdx), Character: uint32(len(line))}
					if len(funcStack) > 0 && funcStack[len(funcStack)-1].indent == trimStart {
						entries[funcStack[len(funcStack)-1].entryIdx].symbol.Range.End = endPos
						funcStack = funcStack[:len(funcStack)-1]
					} else if len(moduleStack) > 0 && moduleStack[len(moduleStack)-1].indent == trimStart {
						entries[moduleStack[len(moduleStack)-1].entryIdx].symbol.Range.End = endPos
						moduleStack = moduleStack[:len(moduleStack)-1]
					}
				}
			}
			continue
		}

		currentModule := ""
		if len(moduleStack) > 0 {
			currentModule = moduleStack[len(moduleStack)-1].name
		}

		// 'd' — defmodule/defprotocol/defimpl/def*/defstruct/defexception
		if first == 'd' && strings.HasPrefix(rest, "def") {

			// Try module-level keywords first, ordered by frequency
			var matchedKeyword string
			switch {
			case strings.HasPrefix(rest, "defmodule") && len(rest) > 9 && (rest[9] == ' ' || rest[9] == '\t'):
				matchedKeyword = "defmodule"
			case strings.HasPrefix(rest, "defimpl") && len(rest) > 7 && (rest[7] == ' ' || rest[7] == '\t'):
				matchedKeyword = "defimpl"
			case strings.HasPrefix(rest, "defprotocol") && len(rest) > 11 && (rest[11] == ' ' || rest[11] == '\t'):
				matchedKeyword = "defprotocol"
			}
			if matchedKeyword != "" {
				after := strings.TrimLeft(rest[len(matchedKeyword)+1:], " \t")
				name := parser.ScanModuleName(after)
				if name != "" {
					fullName := name
					if !strings.Contains(name, ".") && currentModule != "" {
						fullName = currentModule + "." + name
					}

					kind := defKindToSymbolKind(matchedKeyword)
					if matchedKeyword == "defmodule" {
						kind = defKindToSymbolKind("module")
					}

					nameCol := strings.Index(line, name)
					if nameCol < 0 {
						nameCol = trimStart
					}

					entryIdx := len(entries)
					moduleParentIdx := -1
					if len(moduleStack) > 0 {
						moduleParentIdx = moduleStack[len(moduleStack)-1].entryIdx
					}
					entries = append(entries, symbolEntry{
						symbol: protocol.DocumentSymbol{
							Name:   name,
							Detail: matchedKeyword,
							Kind:   kind,
							Range: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
								End:   protocol.Position{Line: uint32(lastLine), Character: 0},
							},
							SelectionRange: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: uint32(nameCol)},
								End:   protocol.Position{Line: uint32(lineIdx), Character: uint32(nameCol + len(name))},
							},
						},
						module:    currentModule,
						parentIdx: moduleParentIdx,
					})
					moduleStack = append(moduleStack, blockFrame{name: fullName, indent: trimStart, entryIdx: entryIdx})
					continue
				}
			}

			// Function/macro/guard/delegate definitions
			if currentModule != "" {
				if kind, funcName, ok := parser.ScanFuncDef(rest); ok {
					arity := parser.ExtractArity(line, funcName)
					nameWithArity := fmt.Sprintf("%s/%d", funcName, arity)

					nameCol := strings.Index(line, funcName)
					if nameCol < 0 {
						nameCol = trimStart
					}

					hasDoBlock := false
					trimmedRight := strings.TrimRight(rest, " \t\r")
					if strings.HasSuffix(trimmedRight, " do") || strings.HasSuffix(trimmedRight, "\tdo") {
						hasDoBlock = true
					}

					rangeEnd := protocol.Position{Line: uint32(lineIdx), Character: uint32(len(line))}
					if hasDoBlock {
						rangeEnd = protocol.Position{Line: uint32(lastLine), Character: 0}
					}

					entryIdx := len(entries)
					entries = append(entries, symbolEntry{
						symbol: protocol.DocumentSymbol{
							Name:   nameWithArity,
							Detail: kind,
							Kind:   defKindToSymbolKind(kind),
							Range: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
								End:   rangeEnd,
							},
							SelectionRange: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: uint32(nameCol)},
								End:   protocol.Position{Line: uint32(lineIdx), Character: uint32(nameCol + len(funcName))},
							},
						},
						module:    currentModule,
						parentIdx: currentParentIdx(),
					})

					if hasDoBlock {
						funcStack = append(funcStack, blockFrame{indent: trimStart, entryIdx: entryIdx})
					}
				} else if strings.HasPrefix(rest, "defstruct ") || strings.HasPrefix(rest, "defstruct\t") {
					entries = append(entries, symbolEntry{
						symbol: protocol.DocumentSymbol{
							Name:   "defstruct",
							Detail: "defstruct",
							Kind:   defKindToSymbolKind("defstruct"),
							Range: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
								End:   protocol.Position{Line: uint32(lineIdx), Character: uint32(len(line))},
							},
							SelectionRange: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: uint32(trimStart)},
								End:   protocol.Position{Line: uint32(lineIdx), Character: uint32(trimStart + 9)},
							},
						},
						module:    currentModule,
						parentIdx: currentParentIdx(),
					})
				} else if strings.HasPrefix(rest, "defexception ") || strings.HasPrefix(rest, "defexception\t") {
					entries = append(entries, symbolEntry{
						symbol: protocol.DocumentSymbol{
							Name:   "defexception",
							Detail: "defexception",
							Kind:   defKindToSymbolKind("defexception"),
							Range: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
								End:   protocol.Position{Line: uint32(lineIdx), Character: uint32(len(line))},
							},
							SelectionRange: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: uint32(trimStart)},
								End:   protocol.Position{Line: uint32(lineIdx), Character: uint32(trimStart + 12)},
							},
						},
						module:    currentModule,
						parentIdx: currentParentIdx(),
					})
				}
			}
			continue
		}

		// '@' — type definitions and @behaviour refs
		if first == '@' {
			if currentModule == "" {
				continue
			}
			var kind string
			var afterKw string
			if strings.HasPrefix(rest, "@typep") && len(rest) > 6 && (rest[6] == ' ' || rest[6] == '\t') {
				kind = "typep"
				afterKw = strings.TrimLeft(rest[6:], " \t")
			} else if strings.HasPrefix(rest, "@type") && len(rest) > 5 && (rest[5] == ' ' || rest[5] == '\t') {
				kind = "type"
				afterKw = strings.TrimLeft(rest[5:], " \t")
			} else if strings.HasPrefix(rest, "@opaque") && len(rest) > 7 && (rest[7] == ' ' || rest[7] == '\t') {
				kind = "opaque"
				afterKw = strings.TrimLeft(rest[7:], " \t")
			} else if strings.HasPrefix(rest, "@macrocallback") && len(rest) > 14 && (rest[14] == ' ' || rest[14] == '\t') {
				kind = "macrocallback"
				afterKw = strings.TrimLeft(rest[14:], " \t")
			} else if strings.HasPrefix(rest, "@callback") && len(rest) > 9 && (rest[9] == ' ' || rest[9] == '\t') {
				kind = "callback"
				afterKw = strings.TrimLeft(rest[9:], " \t")
			}
			if kind != "" {
				name := parser.ScanFuncName(afterKw)
				if name != "" {
					arity := parser.ExtractArity(line, name)
					nameWithArity := fmt.Sprintf("%s/%d", name, arity)

					nameCol := strings.Index(line, name)
					if nameCol < 0 {
						nameCol = trimStart
					}

					entries = append(entries, symbolEntry{
						symbol: protocol.DocumentSymbol{
							Name:   nameWithArity,
							Detail: "@" + kind,
							Kind:   defKindToSymbolKind(kind),
							Range: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
								End:   protocol.Position{Line: uint32(lineIdx), Character: uint32(len(line))},
							},
							SelectionRange: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: uint32(nameCol)},
								End:   protocol.Position{Line: uint32(lineIdx), Character: uint32(nameCol + len(name))},
							},
						},
						module:    currentModule,
						parentIdx: currentParentIdx(),
					})
				}
			}
			continue
		}

		// Bare macro calls with do blocks (describe, test, setup, etc.)
		if currentModule != "" && first >= 'a' && first <= 'z' {
			trimmedRight := strings.TrimRight(rest, " \t\r")
			if strings.HasSuffix(trimmedRight, " do") || strings.HasSuffix(trimmedRight, "\tdo") {
				macroName := parser.ScanFuncName(rest)
				if macroName != "" && !parser.IsElixirKeyword(macroName) {
					afterName := rest[len(macroName):]
					doIdx := strings.LastIndex(trimmedRight, " do")
					if doIdx < 0 {
						doIdx = strings.LastIndex(trimmedRight, "\tdo")
					}
					label := macroName
					if doIdx > len(macroName) {
						arg := strings.TrimSpace(afterName[:doIdx-len(macroName)])
						if len(arg) >= 2 && arg[0] == '"' && arg[len(arg)-1] == '"' {
							arg = arg[1 : len(arg)-1]
						}
						if arg != "" {
							label = macroName + " " + arg
						}
					}

					entryIdx := len(entries)
					entries = append(entries, symbolEntry{
						symbol: protocol.DocumentSymbol{
							Name:   label,
							Detail: macroName,
							Kind:   protocol.SymbolKindFunction,
							Range: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
								End:   protocol.Position{Line: uint32(lastLine), Character: 0},
							},
							SelectionRange: protocol.Range{
								Start: protocol.Position{Line: uint32(lineIdx), Character: uint32(trimStart)},
								End:   protocol.Position{Line: uint32(lineIdx), Character: uint32(trimStart + len(macroName))},
							},
						},
						module:    currentModule,
						parentIdx: currentParentIdx(),
					})
					funcStack = append(funcStack, blockFrame{indent: trimStart, entryIdx: entryIdx})
				}
			}
		}
	}

	// Build hierarchical tree using parentIdx references.
	// Process in reverse so children are attached before their parents are read.
	type symNode struct {
		sym      protocol.DocumentSymbol
		children []int // indices of child entries
	}
	nodes := make([]symNode, len(entries))
	for i, e := range entries {
		nodes[i] = symNode{sym: e.symbol}
	}

	var rootIndices []int
	for i, e := range entries {
		if e.parentIdx >= 0 && e.parentIdx < len(nodes) {
			nodes[e.parentIdx].children = append(nodes[e.parentIdx].children, i)
		} else {
			rootIndices = append(rootIndices, i)
		}
	}

	var buildSymbol func(idx int) protocol.DocumentSymbol
	buildSymbol = func(idx int) protocol.DocumentSymbol {
		s := nodes[idx].sym
		for _, childIdx := range nodes[idx].children {
			s.Children = append(s.Children, buildSymbol(childIdx))
		}
		return s
	}

	var result []interface{}
	for _, idx := range rootIndices {
		result = append(result, buildSymbol(idx))
	}
	return result, nil
}

func defKindToSymbolKind(kind string) protocol.SymbolKind {
	switch kind {
	case "module", "defimpl":
		return protocol.SymbolKindModule
	case "defprotocol":
		return protocol.SymbolKindInterface
	case "def", "defp", "defmacro", "defmacrop", "defguard", "defguardp", "defdelegate":
		return protocol.SymbolKindFunction
	case "type", "typep", "opaque":
		return protocol.SymbolKindTypeParameter
	case "callback", "macrocallback":
		return protocol.SymbolKindEvent
	case "defstruct", "defexception":
		return protocol.SymbolKindStruct
	default:
		return protocol.SymbolKindVariable
	}
}
func (s *Server) ExecuteCommand(ctx context.Context, params *protocol.ExecuteCommandParams) (interface{}, error) {
	return nil, nil
}
func (s *Server) FoldingRanges(ctx context.Context, params *protocol.FoldingRangeParams) ([]protocol.FoldingRange, error) {
	docURI := string(params.TextDocument.URI)
	text, ok := s.docs.Get(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	var ranges []protocol.FoldingRange
	inHeredoc := false
	heredocStart := 0

	type blockStart struct {
		line   int
		indent int
	}
	var stack []blockStart

	for i, line := range lines {
		// Track heredoc boundaries as foldable regions (""" and ''')
		isHeredocDelimiter := false
		if strings.Contains(line, `"""`) {
			if strings.Count(line, `"""`) < 2 {
				isHeredocDelimiter = true
			}
		} else if strings.Contains(line, `'''`) {
			if strings.Count(line, `'''`) < 2 {
				isHeredocDelimiter = true
			}
		}
		if isHeredocDelimiter {
			if !inHeredoc {
				inHeredoc = true
				heredocStart = i
			} else {
				inHeredoc = false
				if i > heredocStart {
					ranges = append(ranges, protocol.FoldingRange{
						StartLine: uint32(heredocStart),
						EndLine:   uint32(i),
					})
				}
			}
			continue
		}
		if inHeredoc {
			continue
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))

		// Check for do blocks: "... do" at end of line
		if strings.HasSuffix(trimmed, " do") || strings.HasSuffix(trimmed, "\tdo") || trimmed == "do" {
			stack = append(stack, blockStart{line: i, indent: indent})
			continue
		}

		// Pop on "end" at matching indent
		if trimmed == "end" && len(stack) > 0 {
			top := stack[len(stack)-1]
			if indent == top.indent {
				stack = stack[:len(stack)-1]
				if i > top.line {
					ranges = append(ranges, protocol.FoldingRange{
						StartLine: uint32(top.line),
						EndLine:   uint32(i),
					})
				}
			}
		}
	}

	return ranges, nil
}

func (s *Server) Formatting(ctx context.Context, params *protocol.DocumentFormattingParams) ([]protocol.TextEdit, error) {
	s.debugf("Formatting: request received for %s", params.TextDocument.URI)
	path := uriToPath(params.TextDocument.URI)
	if path == "" || !isFormattableFile(path) || !s.isProjectFile(path) {
		return nil, nil
	}

	text, ok := s.docs.Get(string(params.TextDocument.URI))
	if !ok {
		return nil, nil
	}

	mixRoot := findMixRoot(filepath.Dir(path))
	if mixRoot == "" {
		return nil, nil
	}

	formatted, err := s.formatContent(mixRoot, path, text)
	if err != nil {
		var formatErr *FormatError
		if errors.As(err, &formatErr) {
			s.publishFormatDiagnostic(params.TextDocument.URI, formatErr)
		}
		return nil, nil
	}

	s.clearFormatDiagnostics(params.TextDocument.URI)

	if formatted == text {
		return nil, nil
	}

	lines := strings.Count(text, "\n") + 1
	return []protocol.TextEdit{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 0},
				End:   protocol.Position{Line: uint32(lines), Character: 0},
			},
			NewText: formatted,
		},
	}, nil
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
	aliases := ExtractAliasesInScope(text, lineNum)

	if moduleRef == "" {
		if functionName == "" {
			return nil, nil
		}

		currentModule := firstDefmodule(lines)
		fullModule := s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, lines, lineNum, functionName, aliases)

		if fullModule != "" {
			// Current module — hover from the buffer directly
			if fullModule == currentModule {
				if line, found := FindFunctionDefinition(text, functionName); found {
					return s.hoverFromBuffer(text, line-1)
				}
			}

			// Look up via store
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

		// Fallback for use-chain inline defs (not stored as module definitions)
		if results := s.lookupThroughUse(text, functionName, aliases); len(results) > 0 {
			return s.hoverFromFile(functionName, results[0])
		}

		return nil, nil
	}

	fullModule := s.resolveModuleWithNesting(moduleRef, aliases, uriToPath(protocol.DocumentURI(docURI)), lineNum)

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
	docURI := string(params.TextDocument.URI)
	if s.debug {
		t0 := time.Now()
		s.debugf("Implementation request: uri=%s line=%d col=%d", docURI, params.Position.Line, params.Position.Character)
		defer func() { s.debugf("Implementation: total %s", time.Since(t0).Round(time.Microsecond)) }()
	}

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
	_, functionName := ExtractModuleAndFunction(expr)
	if functionName == "" {
		return nil, nil
	}

	// Find the module enclosing the cursor via the store — this correctly handles
	// files with defmodule lines inside heredocs/doc examples.
	path := uriToPath(params.TextDocument.URI)
	currentModule := s.store.LookupEnclosingModule(path, lineNum+1)
	if currentModule == "" {
		s.debugf("Implementation: no enclosing module for %s:%d", path, lineNum+1)
		return nil, nil
	}

	s.debugf("Implementation: module=%s function=%s", currentModule, functionName)

	// Only proceed if this function is a declared callback in the current module.
	callbacks, err := s.store.LookupCallbackDef(currentModule, functionName)
	if err != nil || len(callbacks) == 0 {
		return nil, nil
	}
	callbackArity := callbacks[0].Arity

	// Always include the current module as a candidate — handles the common pattern
	// where @callback and def live in the same module. LookupFunctionInModules will
	// simply return nothing for it if no matching def exists.
	modules := []string{currentModule}
	implementors, err := s.store.LookupBehaviourImplementors(currentModule)
	if err == nil {
		for _, impl := range implementors {
			if impl.Module != currentModule {
				modules = append(modules, impl.Module)
			}
		}
	}

	s.debugf("Implementation: %d implementor modules, arity=%d", len(modules), callbackArity)

	results, err := s.store.LookupFunctionInModules(modules, functionName, callbackArity)
	if err != nil {
		return nil, nil
	}
	return storeResultsToLocations(results), nil
}
func (s *Server) OnTypeFormatting(ctx context.Context, params *protocol.DocumentOnTypeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, nil
}
func (s *Server) PrepareRename(ctx context.Context, params *protocol.PrepareRenameParams) (*protocol.Range, error) {
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

	expr, exprStart := extractExpressionBounds(lines[lineNum], col)
	moduleRef, functionName := "", ""
	if expr != "" {
		moduleRef, functionName = ExtractModuleAndFunction(expr)
	}

	// For bare identifiers (no module qualifier), check tree-sitter variables
	// first — a local variable shadows a same-named function in Elixir.
	if moduleRef == "" {
		if tree, src, ok := s.docs.GetTree(docURI); ok {
			if occs := treesitter.FindVariableOccurrencesWithTree(tree.RootNode(), src, uint(lineNum), uint(col)); len(occs) > 0 {
				for _, occ := range occs {
					if occ.Line == uint(lineNum) && uint(col) >= occ.StartCol && uint(col) < occ.EndCol {
						return &protocol.Range{
							Start: protocol.Position{Line: uint32(occ.Line), Character: uint32(occ.StartCol)},
							End:   protocol.Position{Line: uint32(occ.Line), Character: uint32(occ.EndCol)},
						}, nil
					}
				}
				return &protocol.Range{
					Start: protocol.Position{Line: uint32(occs[0].Line), Character: uint32(occs[0].StartCol)},
					End:   protocol.Position{Line: uint32(occs[0].Line), Character: uint32(occs[0].EndCol)},
				}, nil
			}
		}
	}

	// Try module/function rename via the index
	if expr != "" {
		aliases := ExtractAliasesInScope(text, lineNum)

		// Detect `as:` aliases — these are file-local renames, not module renames.
		// An `as:` alias has a short name that differs from the last segment of
		// the resolved module (e.g. TransactionReceiptSchema → MyApp.Billing.TransactionReceipt).
		if moduleRef != "" && functionName == "" {
			if resolved, ok := aliases[moduleRef]; ok && moduleLastSegment(resolved) != moduleRef {
				// File-local alias rename: find all occurrences in this file
				return &protocol.Range{
					Start: protocol.Position{Line: uint32(lineNum), Character: uint32(exprStart)},
					End:   protocol.Position{Line: uint32(lineNum), Character: uint32(exprStart + len(moduleRef))},
				}, nil
			}
		}

		var tokenName string
		var fullModule string
		found := false

		if functionName != "" {
			tokenName = functionName
			if moduleRef != "" {
				fullModule = resolveModule(moduleRef, aliases)
			} else {
				fullModule = s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, lines, lineNum, functionName, aliases)
			}
			found = fullModule != ""
		} else if moduleRef != "" {
			tokenName = moduleLastSegment(moduleRef)
			fullModule = resolveModule(moduleRef, aliases)
			if defResults, err := s.store.LookupModule(fullModule); err == nil && len(defResults) > 0 {
				found = true
			}
		}

		if found {
			// Reject stdlib and dependency symbols
			var defPaths []string
			if functionName != "" {
				if results, err := s.store.LookupFunction(fullModule, functionName); err == nil {
					for _, r := range results {
						defPaths = append(defPaths, r.FilePath)
					}
				}
				// Not directly defined — check if injected via fullModule's use chain
				// (e.g. Oban.Worker injects `new`). Block rename if from a dep.
				if len(defPaths) == 0 {
					for _, r := range s.lookupThroughUseOf(fullModule, functionName) {
						defPaths = append(defPaths, r.FilePath)
					}
				}
			} else {
				if results, err := s.store.LookupModule(fullModule); err == nil {
					for _, r := range results {
						defPaths = append(defPaths, r.FilePath)
					}
				}
			}
			hasFirstPartyDef := false
			for _, p := range defPaths {
				if (s.stdlibRoot != "" && strings.HasPrefix(p, s.stdlibRoot)) || s.isDepsFile(p) {
					continue
				}
				hasFirstPartyDef = true
				break
			}
			if len(defPaths) > 0 && !hasFirstPartyDef {
				return nil, nil
			}

			exprInLine := lines[lineNum][exprStart:]
			tokenOffset := findTokenColumn(exprInLine, tokenName)
			if tokenOffset >= 0 {
				tokenStart := exprStart + tokenOffset
				return &protocol.Range{
					Start: protocol.Position{Line: uint32(lineNum), Character: uint32(tokenStart)},
					End:   protocol.Position{Line: uint32(lineNum), Character: uint32(tokenStart + len(tokenName))},
				}, nil
			}
		}
	}

	return nil, nil
}
func (s *Server) RangeFormatting(ctx context.Context, params *protocol.DocumentRangeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, nil
}
func (s *Server) References(ctx context.Context, params *protocol.ReferenceParams) ([]protocol.Location, error) {
	docURI := string(params.TextDocument.URI)
	if s.debug {
		t := time.Now()
		s.debugf("References request: uri=%s line=%d col=%d", docURI, params.Position.Line, params.Position.Character)
		defer func() { s.debugf("References: total %s", time.Since(t).Round(time.Microsecond)) }()
	}

	text, ok := s.docs.Get(docURI)
	if !ok {
		s.debugf("References: document not found in store")
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := int(params.Position.Character)

	if lineNum >= len(lines) {
		s.debugf("References: line %d out of range (total %d)", lineNum, len(lines))
		return nil, nil
	}

	expr := ExtractExpression(lines[lineNum], col)
	if expr == "" {
		s.debugf("References: no expression at cursor")
		return nil, nil
	}

	// Special case: cursor on defmacro __using__ — find all `use ModuleName` sites.
	if expr == "__using__" {
		for _, l := range lines {
			if m := parser.DefmoduleRe.FindStringSubmatch(l); m != nil {
				s.debugf("References: __using__ in module %s — looking up use sites", m[1])
				allRefs, err := s.store.LookupReferences(m[1], "")
				if err != nil {
					return nil, nil
				}
				var locations []protocol.Location
				for _, r := range allRefs {
					if r.Kind == "use" {
						locations = append(locations, protocol.Location{
							URI:   uri.File(r.FilePath),
							Range: lineRange(r.Line - 1),
						})
					}
				}
				s.debugf("References: returning %d use sites", len(locations))
				return locations, nil
			}
		}
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
	aliases := ExtractAliasesInScope(text, lineNum)
	s.debugf("References: expr=%q module=%q function=%q", expr, moduleRef, functionName)

	var fullModule string

	if moduleRef == "" {
		if functionName == "" {
			s.debugf("References: no module or function")
			return nil, nil
		}

		// Variable references via tree-sitter (scoped to the current file).
		// When a variable is defined in the enclosing scope, it shadows any
		// function with the same name, so variable references take priority.
		// Bare identifiers that aren't defined as variables fall through to
		// function reference lookup.
		if tree, src, ok := s.docs.GetTree(docURI); ok {
			if occs := treesitter.FindVariableOccurrencesWithTree(tree.RootNode(), src, uint(lineNum), uint(col)); len(occs) > 0 {
				var locations []protocol.Location
				for _, occ := range occs {
					locations = append(locations, protocol.Location{
						URI: params.TextDocument.URI,
						Range: protocol.Range{
							Start: protocol.Position{Line: uint32(occ.Line), Character: uint32(occ.StartCol)},
							End:   protocol.Position{Line: uint32(occ.Line), Character: uint32(occ.EndCol)},
						},
					})
				}
				s.debugf("References: returning %d variable occurrences", len(locations))
				return locations, nil
			}
		}

		// Bare function — resolve to its defining module
		fullModule = s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, lines, lineNum, functionName, aliases)
		s.debugf("References: resolved bare %q -> %q", functionName, fullModule)
		if fullModule == "" {
			s.debugf("References: could not resolve bare function %q", functionName)
			return nil, nil
		}
	} else {
		// When cursor is on a defmodule line, use the store's fully-qualified
		// name directly — the user is asking about the module being defined,
		// not a reference that might be shadowed by an alias with the same name.
		if m := parser.DefmoduleRe.FindStringSubmatch(lines[lineNum]); m != nil {
			if enclosing := s.store.LookupEnclosingModule(uriToPath(params.TextDocument.URI), lineNum+1); enclosing != "" {
				fullModule = enclosing
			} else {
				fullModule = resolveModule(moduleRef, aliases)
			}
		} else {
			fullModule = s.resolveModuleWithNesting(moduleRef, aliases, uriToPath(params.TextDocument.URI), lineNum)
		}
		s.debugf("References: resolved module %q -> %q", moduleRef, fullModule)
	}

	s.debugf("References: looking up refs for %s.%s", fullModule, functionName)

	// Check the cursor file's own use chains with consumer opts first — this is
	// fast (cache reads only) and catches dynamic opt-binding injectors like
	// `import unquote(mod)`. If we find injectors here we can skip the expensive
	// findModulesWhoseUsingImports scan entirely.
	var injectors []string
	if functionName != "" && moduleRef == "" {
		useCalls := ExtractUsesWithOpts(text, aliases)
		visited := make(map[string]bool)
		for _, uc := range useCalls {
			if s.lookupInUsingEntry(uc.Module, functionName, uc.Opts, visited) != nil {
				injectors = append(injectors, uc.Module)
				s.debugf("References: opt-binding injector for %s: %s", functionName, uc.Module)
			}
		}
	}

	// Run direct lookup and (if needed) the static use-chain injector scan.
	// The static scan is expensive but necessary when the function comes from
	// a static __using__ import rather than a dynamic opt binding.
	type injectorResult struct {
		injectors []string
		elapsed   time.Duration
	}
	var injectorCh chan injectorResult
	if functionName != "" && len(injectors) == 0 {
		// Only run the expensive scan if the fast opt-binding check found nothing
		injectorCh = make(chan injectorResult, 1)
		go func() {
			tInj := s.debugNow()
			inj := s.findModulesWhoseUsingImports(fullModule)
			injectorCh <- injectorResult{inj, time.Since(tInj)}
		}()
	}

	tStep := s.debugNow()
	refResults, err := s.store.LookupReferences(fullModule, functionName)
	if err != nil {
		s.debugf("References: store error: %v", err)
		return nil, nil
	}
	if s.debug {
		s.debugf("References: direct lookup: %d results (%s)", len(refResults), time.Since(tStep).Round(time.Microsecond))
	}

	if injectorCh != nil {
		ir := <-injectorCh
		if s.debug {
			s.debugf("References: use-chain injectors for %s: %v (%s)", fullModule, ir.injectors, ir.elapsed.Round(time.Microsecond))
		}
		injectors = append(injectors, ir.injectors...)
	}

	for _, mod := range injectors {
		transitive, err := s.store.LookupReferences(mod, functionName)
		if err == nil {
			refResults = append(refResults, transitive...)
			s.debugf("References: transitive via %s: +%d results", mod, len(transitive))
		}
	}

	// Scan definition files for bare intra-module calls (not indexed in store)
	if functionName != "" {
		tStep = s.debugNow()
		refResults = append(refResults, s.findBareCallRefs(fullModule, functionName)...)
		if s.debug {
			s.debugf("References: bare call scan (%s)", time.Since(tStep).Round(time.Microsecond))
		}
	}

	// Follow defdelegate in reverse: if other modules delegate this function
	// to fullModule, include refs to those delegating modules too.
	if functionName != "" && s.followDelegates {
		tStep = s.debugNow()
		delegates, err := s.store.LookupDelegatesTo(fullModule, functionName)
		if err == nil {
			for _, del := range delegates {
				// The facade function name may differ from the target if as: is used
				facadeFunc := del.Function
				delegateRefs, err := s.store.LookupReferences(del.Module, facadeFunc)
				if err == nil {
					refResults = append(refResults, delegateRefs...)
					s.debugf("References: via delegate %s.%s: +%d results", del.Module, facadeFunc, len(delegateRefs))
				}
				refResults = append(refResults, s.findBareCallRefs(del.Module, facadeFunc)...)
			}
		}
		if s.debug {
			s.debugf("References: delegate follow (%s)", time.Since(tStep).Round(time.Microsecond))
		}
	}

	// Deduplicate by file+line (multiple injector modules may attribute the same call)
	type refKey struct {
		filePath string
		line     int
	}
	seen := make(map[refKey]struct{}, len(refResults))

	// Filter out stdlib paths
	var locations []protocol.Location
	for _, r := range refResults {
		if s.stdlibRoot != "" && strings.HasPrefix(r.FilePath, s.stdlibRoot) {
			continue
		}
		k := refKey{r.FilePath, r.Line}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		locations = append(locations, protocol.Location{
			URI:   uri.File(r.FilePath),
			Range: lineRange(r.Line - 1),
		})
	}

	// Include declaration if requested
	if params.Context.IncludeDeclaration {
		defResults, err := s.store.LookupFunction(fullModule, functionName)
		if err == nil {
			for _, r := range defResults {
				if s.stdlibRoot != "" && strings.HasPrefix(r.FilePath, s.stdlibRoot) {
					continue
				}
				k := refKey{r.FilePath, r.Line}
				if _, ok := seen[k]; ok {
					continue
				}
				locations = append(locations, protocol.Location{
					URI:   uri.File(r.FilePath),
					Range: lineRange(r.Line - 1),
				})
			}
		}
	}

	s.debugf("References: returning %d locations", len(locations))
	return locations, nil
}
func (s *Server) Rename(ctx context.Context, params *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
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

	expr, _ := extractExpressionBounds(lines[lineNum], col)
	moduleRef, functionName := "", ""
	if expr != "" {
		moduleRef, functionName = ExtractModuleAndFunction(expr)
	}

	// For bare identifiers, check tree-sitter variables first — a local
	// variable shadows a same-named function in Elixir.
	if moduleRef == "" {
		if tree, src, ok := s.docs.GetTree(docURI); ok {
			if occs := treesitter.FindVariableOccurrencesWithTree(tree.RootNode(), src, uint(lineNum), uint(col)); len(occs) > 0 {
				if treesitter.NameExistsInScopeOf(tree.RootNode(), src, uint(lineNum), uint(col), params.NewName) {
					return nil, fmt.Errorf("variable %q already exists in this scope", params.NewName)
				}
				changes := make(map[protocol.DocumentURI][]protocol.TextEdit)
				for _, occ := range occs {
					changes[protocol.DocumentURI(docURI)] = append(changes[protocol.DocumentURI(docURI)], protocol.TextEdit{
						Range: protocol.Range{
							Start: protocol.Position{Line: uint32(occ.Line), Character: uint32(occ.StartCol)},
							End:   protocol.Position{Line: uint32(occ.Line), Character: uint32(occ.EndCol)},
						},
						NewText: params.NewName,
					})
				}
				return &protocol.WorkspaceEdit{Changes: changes}, nil
			}
		}
	}

	// Try module/function rename via the index
	if expr != "" {
		aliases := ExtractAliasesInScope(text, lineNum)

		// Detect `as:` aliases — file-local rename of the alias name, not
		// the underlying module.
		if moduleRef != "" && functionName == "" {
			if resolved, ok := aliases[moduleRef]; ok && moduleLastSegment(resolved) != moduleRef {
				if !isValidModuleName(params.NewName) {
					return nil, fmt.Errorf("invalid alias name %q: must start with an uppercase letter", params.NewName)
				}
				// Replace all standalone occurrences of the alias in this file
				// (skip occurrences preceded by '.' — those are part of a
				// qualified module name, not the alias).
				changes := make(map[protocol.DocumentURI][]protocol.TextEdit)
				fileURI := protocol.DocumentURI(docURI)
				for i, line := range lines {
					for _, col := range findAllTokenColumns(line, moduleRef) {
						if col > 0 && line[col-1] == '.' {
							continue
						}
						changes[fileURI] = append(changes[fileURI], protocol.TextEdit{
							Range: protocol.Range{
								Start: protocol.Position{Line: uint32(i), Character: uint32(col)},
								End:   protocol.Position{Line: uint32(i), Character: uint32(col + len(moduleRef))},
							},
							NewText: params.NewName,
						})
					}
				}
				return &protocol.WorkspaceEdit{Changes: changes}, nil
			}
		}

		if functionName != "" {
			var fullModule string
			if moduleRef != "" {
				fullModule = resolveModule(moduleRef, aliases)
			} else {
				fullModule = s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, lines, lineNum, functionName, aliases)
			}
			if fullModule != "" {
				if !isValidFunctionName(params.NewName) {
					return nil, fmt.Errorf("invalid function name %q: must match [a-z_][a-z0-9_?!]*", params.NewName)
				}
				if existing, err := s.store.LookupFunction(fullModule, params.NewName); err == nil && len(existing) > 0 {
					return nil, fmt.Errorf("function %s.%s already exists", fullModule, params.NewName)
				}
				return s.renameFunctionEdits(fullModule, functionName, params.NewName)
			}
		} else if moduleRef != "" {
			fullModule := resolveModule(moduleRef, aliases)
			if defResults, err := s.store.LookupModule(fullModule); err == nil && len(defResults) > 0 {
				// PrepareRename highlights just the last segment, so the user's
				// input replaces that segment. Prepend the parent namespace.
				newModule := params.NewName
				if dot := strings.LastIndex(fullModule, "."); dot >= 0 {
					newModule = fullModule[:dot+1] + params.NewName
				}
				if !isValidModuleName(newModule) {
					return nil, fmt.Errorf("invalid module name %q: must be CamelCase segments separated by dots", params.NewName)
				}
				return s.renameModuleEdits(ctx, fullModule, newModule, uriToPath(params.TextDocument.URI))
			}
		}
	}

	return nil, nil
}

// renameFunctionEdits builds a WorkspaceEdit renaming all occurrences of
// module.functionName to newName across the codebase.
func (s *Server) renameFunctionEdits(module, functionName, newName string) (*protocol.WorkspaceEdit, error) {
	// Collect all (filePath, lineNumber) pairs — definitions + references
	type siteKey struct {
		filePath string
		line     int
	}
	seen := make(map[siteKey]bool)
	var sites []renameSite

	addSiteOpts := func(filePath string, line int, includeKeyword bool) {
		if s.stdlibRoot != "" && strings.HasPrefix(filePath, s.stdlibRoot) {
			return
		}
		if s.isDepsFile(filePath) {
			return
		}
		k := siteKey{filePath, line}
		if !seen[k] {
			seen[k] = true
			sites = append(sites, renameSite{filePath, line, includeKeyword})
		}
	}
	addSite := func(filePath string, line int) {
		addSiteOpts(filePath, line, false)
	}

	// Definition sites
	defResults, err := s.store.LookupFunction(module, functionName)
	if err != nil {
		return nil, nil
	}
	for _, r := range defResults {
		addSite(r.FilePath, r.Line)
	}

	// Direct reference sites (calls, imports — skip alias/use which are module-level)
	refResults, err := s.store.LookupReferences(module, functionName)
	if err != nil {
		return nil, nil
	}
	for _, r := range refResults {
		if r.Kind == "alias" || r.Kind == "use" {
			continue
		}
		addSite(r.FilePath, r.Line)
	}

	// Transitive refs via __using__ chains
	for _, mod := range s.findModulesWhoseUsingImports(module) {
		transitive, err := s.store.LookupReferences(mod, functionName)
		if err == nil {
			for _, r := range transitive {
				if r.Kind == "alias" || r.Kind == "use" {
					continue
				}
				addSite(r.FilePath, r.Line)
			}
		}
	}

	// Collect definition file paths for file-scanning passes below
	defFilePaths := make(map[string]bool)
	for _, r := range defResults {
		defFilePaths[r.FilePath] = true
	}

	// Scan definition files for @spec/@callback lines and bare intra-module calls
	// (none of these are indexed in the store).
	specPrefix := "@spec " + functionName
	callbackPrefix := "@callback " + functionName
	for filePath := range defFilePaths {
		fileText, _, ok := s.readFileText(filePath)
		if !ok {
			continue
		}
		// @spec and @callback lines
		for i, line := range strings.Split(fileText, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, specPrefix) {
				rest := trimmed[len(specPrefix):]
				if len(rest) == 0 || rest[0] == '(' || rest[0] == ' ' || rest[0] == '\t' {
					addSite(filePath, i+1)
				}
			}
			if strings.HasPrefix(trimmed, callbackPrefix) {
				rest := trimmed[len(callbackPrefix):]
				if len(rest) == 0 || rest[0] == '(' || rest[0] == ' ' || rest[0] == '\t' {
					addSite(filePath, i+1)
				}
			}
		}
		// Bare calls: functionName(...) and |> functionName
		for _, lineNum := range FindBareFunctionCalls(fileText, functionName) {
			addSite(filePath, lineNum)
		}
	}

	// Scan all files that import the module for `import Module, only: [functionName: N]` lines,
	// then also scan those files for bare calls (which aren't indexed as references).
	importRefs, _ := s.store.LookupReferences(module, "")
	importFilePaths := make(map[string]bool)
	for _, r := range importRefs {
		if r.Kind != "import" {
			continue
		}
		lineText, ok := s.getFileLine(r.FilePath, r.Line)
		if !ok {
			continue
		}
		if findTokenColumn(lineText, functionName) >= 0 {
			addSiteOpts(r.FilePath, r.Line, true)
			importFilePaths[r.FilePath] = true
		}
	}
	for filePath := range importFilePaths {
		fileText, _, ok := s.readFileText(filePath)
		if !ok {
			continue
		}
		for _, lineNum := range FindBareFunctionCalls(fileText, functionName) {
			addSite(filePath, lineNum)
		}
	}

	edit := s.buildTextEdits(sites, functionName, newName)

	// Update defdelegate lines that forward to this function: add or update
	// the `as:` option so the facade keeps working after the rename.
	if s.followDelegates {
		delegates, err := s.store.LookupDelegatesTo(module, functionName)
		if err == nil {
			for _, del := range delegates {
				if s.stdlibRoot != "" && strings.HasPrefix(del.FilePath, s.stdlibRoot) {
					continue
				}
				if s.isDepsFile(del.FilePath) {
					continue
				}
				fileText, open, ok := s.readFileText(del.FilePath)
				if !ok {
					continue
				}
				fileLines := strings.Split(fileText, "\n")
				startLine := del.Line - 1
				if startLine >= len(fileLines) {
					continue
				}

				updatedSpan, spanStart, spanEnd := updateDelegateAs(fileLines, startLine, del.Function, newName)
				// Check if anything actually changed
				changed := len(updatedSpan) != spanEnd-spanStart
				if !changed {
					for i, line := range updatedSpan {
						if line != fileLines[spanStart+i] {
							changed = true
							break
						}
					}
				}
				if !changed {
					continue
				}

				fileURI := protocol.DocumentURI(uri.File(del.FilePath))
				if open {
					if edit.Changes == nil {
						edit.Changes = make(map[protocol.DocumentURI][]protocol.TextEdit)
					}
					edit.Changes[fileURI] = append(edit.Changes[fileURI], protocol.TextEdit{
						Range: protocol.Range{
							Start: protocol.Position{Line: uint32(spanStart), Character: 0},
							End:   protocol.Position{Line: uint32(spanEnd - 1), Character: uint32(len(fileLines[spanEnd-1]))},
						},
						NewText: strings.Join(updatedSpan, "\n"),
					})
				} else {
					// Closed file: splice updated span into file lines and write back
					newFileLines := make([]string, 0, len(fileLines)-spanEnd+spanStart+len(updatedSpan))
					newFileLines = append(newFileLines, fileLines[:spanStart]...)
					newFileLines = append(newFileLines, updatedSpan...)
					newFileLines = append(newFileLines, fileLines[spanEnd:]...)
					_ = os.WriteFile(del.FilePath, []byte(strings.Join(newFileLines, "\n")), 0644)
				}
			}
		}
	}

	return edit, nil
}

// renameModuleEdits builds a WorkspaceEdit renaming oldModule to newModule,
// including all submodules and their references.
//
// Files not currently open in the editor are written directly to disk in
// parallel goroutines. Only open buffers are included in the returned
// WorkspaceEdit, keeping the response small and avoiding editor freezes.
// Files following the naming convention are also renamed/moved.
func (s *Server) renameModuleEdits(ctx context.Context, oldModule, newModule, triggerFilePath string) (*protocol.WorkspaceEdit, error) {
	mr := s.buildModuleRename(oldModule, newModule)

	// Check for collisions: verify that none of the target module names
	// (including submodules) already exist, and that no destination file
	// paths are occupied.
	if err := mr.checkCollisions(); err != nil {
		return nil, err
	}

	mr.collectSites()

	fileCache := mr.readFiles()

	movedFiles, openMovedFiles, showDocumentPath := mr.moveConventionalFiles(fileCache, triggerFilePath)
	openChanges := mr.applyEdits(fileCache, movedFiles)
	mr.reindex(fileCache, movedFiles, openMovedFiles)

	// For open files that were moved: send showDocument so the editor opens
	// the new path, then delete the old file in the background.
	if s.showDocumentSupported && s.conn != nil {
		for oldPath, newPath := range openMovedFiles {
			showURI := protocol.URI(string(uri.File(newPath)))
			takeFocus := newPath == showDocumentPath
			go func() {
				var result protocol.ShowDocumentResult
				_ = protocol.Call(context.Background(), s.conn, "window/showDocument", &protocol.ShowDocumentParams{
					URI:       showURI,
					TakeFocus: takeFocus,
				}, &result)
				// Delete old file after the editor has been redirected
				_ = os.Remove(oldPath)
				_ = s.store.RemoveFile(oldPath)
			}()
		}
	} else if len(openMovedFiles) > 0 {
		// Client doesn't support showDocument — still clean up old files
		for oldPath := range openMovedFiles {
			_ = os.Remove(oldPath)
			_ = s.store.RemoveFile(oldPath)
		}
	}

	return &protocol.WorkspaceEdit{Changes: openChanges}, nil
}

// moduleRename holds the state for a module rename operation.
type moduleRename struct {
	server            *Server
	oldModule         string
	newModule         string
	moduleRenames     map[string]string // old module → new module
	tokenReplacements map[string]string // old token → new token
	allModuleDefs     []store.LookupResult
	sitesByFile       map[string][]moduleEditSite
}

type moduleEditSite struct {
	filePath string
	line     int
	token    string
}

type moduleFileInfo struct {
	lines []string
	open  bool
}

func (s *Server) buildModuleRename(oldModule, newModule string) *moduleRename {
	moduleRenames := map[string]string{oldModule: newModule}
	submodules, _ := s.store.ListSubmodules(oldModule)
	for _, sub := range submodules {
		moduleRenames[sub] = newModule + sub[len(oldModule):]
	}

	tokenReplacements := make(map[string]string, len(moduleRenames)*2)
	for old, newName := range moduleRenames {
		tokenReplacements[old] = newName
		oldSeg := moduleLastSegment(old)
		newSeg := moduleLastSegment(newName)
		if _, exists := tokenReplacements[oldSeg]; !exists {
			tokenReplacements[oldSeg] = newSeg
		}
	}

	return &moduleRename{
		server:            s,
		oldModule:         oldModule,
		newModule:         newModule,
		moduleRenames:     moduleRenames,
		tokenReplacements: tokenReplacements,
		sitesByFile:       make(map[string][]moduleEditSite),
	}
}

// checkCollisions verifies that none of the target module names (including
// submodules) already exist at unrelated file paths, and that no conventional
// destination file paths are already occupied on disk.
func (mr *moduleRename) checkCollisions() error {
	// Build the set of source file paths involved in this rename so we can
	// ignore stale index entries at those paths.
	oldDefs, _ := mr.server.store.LookupModulesByPrefix(mr.oldModule)
	ignorePaths := make(map[string]bool, len(oldDefs)*2)
	for _, r := range oldDefs {
		ignorePaths[r.FilePath] = true
	}

	for oldMod, newMod := range mr.moduleRenames {
		// Module name collision: check if newMod already exists outside our rename scope
		if existing, err := mr.server.store.LookupModule(newMod); err == nil && len(existing) > 0 {
			for _, r := range existing {
				if !ignorePaths[r.FilePath] {
					return fmt.Errorf("module %s already exists", newMod)
				}
			}
		}

		// File path collision: if the old module follows naming convention,
		// check that the destination path isn't already taken.
		oldDefs, _ := mr.server.store.LookupModule(oldMod)
		for _, r := range oldDefs {
			if fileMatchesModuleConvention(r.FilePath, oldMod) {
				newPath := conventionalNewPath(r.FilePath, oldMod, newMod)
				if _, err := os.Stat(newPath); err == nil {
					// File exists — only a collision if it's not a source file
					// we're moving as part of this rename
					if !ignorePaths[newPath] {
						return fmt.Errorf("file %s already exists", newPath)
					}
				}
			}
		}
	}
	return nil
}

func (mr *moduleRename) isExcluded(filePath string) bool {
	return (mr.server.stdlibRoot != "" && strings.HasPrefix(filePath, mr.server.stdlibRoot)) || mr.server.isDepsFile(filePath)
}

func (mr *moduleRename) collectSites() {
	seen := make(map[string]bool)
	addSite := func(filePath string, line int, token string) {
		if mr.isExcluded(filePath) {
			return
		}
		k := filePath + "\x00" + strconv.Itoa(line) + "\x00" + token
		if !seen[k] {
			seen[k] = true
			mr.sitesByFile[filePath] = append(mr.sitesByFile[filePath], moduleEditSite{filePath, line, token})
		}
	}

	// Definition sites
	allModuleDefs, err := mr.server.store.LookupModulesByPrefix(mr.oldModule)
	if err == nil {
		for _, r := range allModuleDefs {
			if _, ok := mr.moduleRenames[r.Module]; ok {
				addSite(r.FilePath, r.Line, r.Module)
			}
		}
	}
	mr.allModuleDefs = allModuleDefs

	// Reference sites
	refs, err := mr.server.store.LookupReferencesByPrefix(mr.oldModule)
	if err == nil {
		for _, r := range refs {
			if _, ok := mr.moduleRenames[r.Module]; !ok {
				newMod := mr.newModule + r.Module[len(mr.oldModule):]
				mr.moduleRenames[r.Module] = newMod
				mr.tokenReplacements[r.Module] = newMod
			}
			addSite(r.FilePath, r.Line, r.Module)
		}
	}
}

func (mr *moduleRename) readFiles() map[string]moduleFileInfo {
	type fileResult struct {
		path  string
		lines []string
		open  bool
	}
	resultsCh := make(chan fileResult, len(mr.sitesByFile))
	for fp := range mr.sitesByFile {
		go func() {
			text, open, ok := mr.server.readFileText(fp)
			if ok {
				resultsCh <- fileResult{fp, strings.Split(text, "\n"), open}
			} else {
				resultsCh <- fileResult{fp, nil, false}
			}
		}()
	}
	fileCache := make(map[string]moduleFileInfo, len(mr.sitesByFile))
	for range mr.sitesByFile {
		r := <-resultsCh
		if r.lines != nil {
			fileCache[r.path] = moduleFileInfo{r.lines, r.open}
		}
	}
	return fileCache
}

// findModuleEdits finds token replacements on a single line, trying the full
// qualified name first and falling back to progressively shorter dot-suffixes
// to handle aliased forms.
func (mr *moduleRename) findModuleEdits(lineText string, token string) []moduleEditResult {
	newToken, ok := mr.tokenReplacements[token]
	if !ok {
		return nil
	}
	if cols := findAllTokenColumns(lineText, token); len(cols) > 0 {
		var results []moduleEditResult
		for _, col := range cols {
			results = append(results, moduleEditResult{col, len(token), newToken})
		}
		return results
	}
	oldSuffix := token
	newSuffix := newToken
	for {
		dotIdx := strings.IndexByte(oldSuffix, '.')
		if dotIdx < 0 {
			break
		}
		oldSuffix = oldSuffix[dotIdx+1:]
		newDot := strings.IndexByte(newSuffix, '.')
		if newDot < 0 {
			break
		}
		newSuffix = newSuffix[newDot+1:]
		if oldSuffix == newSuffix {
			continue
		}
		if cols := findAllTokenColumns(lineText, oldSuffix); len(cols) > 0 {
			var results []moduleEditResult
			for _, col := range cols {
				results = append(results, moduleEditResult{col, len(oldSuffix), newSuffix})
			}
			return results
		}
	}
	return nil
}

type moduleEditResult struct {
	col      int
	length   int
	newToken string
}

func (mr *moduleRename) applyEditsToLines(lines []string, sites []moduleEditSite) []string {
	result := make([]string, len(lines))
	copy(result, lines)
	for _, es := range sites {
		if es.line-1 >= len(result) {
			continue
		}
		lineText := result[es.line-1]
		edits := mr.findModuleEdits(lineText, es.token)
		for i := len(edits) - 1; i >= 0; i-- {
			e := edits[i]
			lineText = lineText[:e.col] + e.newToken + lineText[e.col+e.length:]
		}
		result[es.line-1] = lineText
	}
	return result
}

func (mr *moduleRename) conventionalNewPath(r store.LookupResult) (string, bool) {
	oldDirSeg := camelToSnake(moduleLastSegment(mr.oldModule))
	newDirSeg := camelToSnake(moduleLastSegment(mr.newModule))

	if r.Module == mr.oldModule {
		if !fileMatchesModuleConvention(r.FilePath, mr.oldModule) {
			return "", false
		}
		return conventionalNewPath(r.FilePath, mr.oldModule, mr.newModule), true
	}
	// Submodule: compute expected path suffix and check it matches
	suffix := r.Module[len(mr.oldModule):]
	segments := strings.Split(strings.TrimPrefix(suffix, "."), ".")
	parts := make([]string, 0, len(segments)+1)
	parts = append(parts, oldDirSeg)
	for i, seg := range segments {
		if i == len(segments)-1 {
			parts = append(parts, camelToSnake(seg)+".ex")
		} else {
			parts = append(parts, camelToSnake(seg))
		}
	}
	expectedSuffix := filepath.Join(parts...)
	slashSuffix := string(os.PathSeparator) + filepath.FromSlash(expectedSuffix)
	if !strings.HasSuffix(r.FilePath, slashSuffix) {
		return "", false
	}
	newSuffix := newDirSeg + expectedSuffix[len(oldDirSeg):]
	prefix := r.FilePath[:len(r.FilePath)-len(slashSuffix)+1]
	return filepath.Join(prefix, filepath.FromSlash(newSuffix)), true
}

// moveConventionalFiles moves files that follow the naming convention to their
// new paths, applying edits in the process. Open files are NOT moved on disk —
// they are left for applyEdits to handle via TextEdits so the editor buffer
// stays in sync. Returns moved files, paths that need showDocument calls
// (open files that were moved), and the path to show for the trigger file.
func (mr *moduleRename) moveConventionalFiles(fileCache map[string]moduleFileInfo, triggerFilePath string) (movedFiles map[string]string, openMovedFiles map[string]string, showDocumentPath string) {
	movedFiles = make(map[string]string)
	openMovedFiles = make(map[string]string)
	for _, r := range mr.allModuleDefs {
		if _, ok := mr.moduleRenames[r.Module]; !ok {
			continue
		}
		newPath, follows := mr.conventionalNewPath(r)
		if !follows {
			continue
		}
		fi, hasContent := fileCache[r.FilePath]
		if !hasContent {
			continue
		}

		// Open files: write the new file to disk but DON'T delete the old one
		// or mark it in movedFiles. Instead track it in openMovedFiles so that
		// applyEdits still produces TextEdits for the editor buffer, and we
		// send showDocument to redirect the editor to the new path.
		if fi.open {
			updatedLines := mr.applyEditsToLines(fi.lines, mr.sitesByFile[r.FilePath])
			content := strings.Join(updatedLines, "\n")
			if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
				log.Printf("Rename: cannot create dir for %s: %v", newPath, err)
				continue
			}
			if err := os.WriteFile(newPath, []byte(content), 0644); err != nil {
				log.Printf("Rename: cannot write %s: %v", newPath, err)
				continue
			}
			mr.server.debugf("Rename: %s → %s (open, deferred delete)", r.FilePath, newPath)
			openMovedFiles[r.FilePath] = newPath
			if r.FilePath == triggerFilePath && showDocumentPath == "" {
				showDocumentPath = newPath
			}
			continue
		}

		updatedLines := mr.applyEditsToLines(fi.lines, mr.sitesByFile[r.FilePath])
		content := strings.Join(updatedLines, "\n")

		if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
			log.Printf("Rename: cannot create dir for %s: %v", newPath, err)
			continue
		}
		if err := os.WriteFile(newPath, []byte(content), 0644); err != nil {
			log.Printf("Rename: cannot write %s: %v", newPath, err)
			continue
		}
		if err := os.Remove(r.FilePath); err != nil {
			log.Printf("Rename: cannot remove %s: %v", r.FilePath, err)
		}
		mr.server.debugf("Rename: %s → %s", r.FilePath, newPath)
		movedFiles[r.FilePath] = newPath
	}
	return movedFiles, openMovedFiles, showDocumentPath
}

// applyEdits applies text edits to all non-moved files: open buffers get
// TextEdits in the WorkspaceEdit, closed files are written directly to disk.
func (mr *moduleRename) applyEdits(fileCache map[string]moduleFileInfo, movedFiles map[string]string) map[protocol.DocumentURI][]protocol.TextEdit {
	openChanges := make(map[protocol.DocumentURI][]protocol.TextEdit)
	var wg sync.WaitGroup

	for fp, sites := range mr.sitesByFile {
		if _, moved := movedFiles[fp]; moved {
			continue
		}
		fi, ok := fileCache[fp]
		if !ok {
			continue
		}
		if fi.open {
			fileURI := protocol.DocumentURI(uri.File(fp))
			for _, es := range sites {
				if es.line-1 >= len(fi.lines) {
					continue
				}
				lineText := fi.lines[es.line-1]
				for _, e := range mr.findModuleEdits(lineText, es.token) {
					openChanges[fileURI] = append(openChanges[fileURI], protocol.TextEdit{
						Range: protocol.Range{
							Start: protocol.Position{Line: uint32(es.line - 1), Character: uint32(e.col)},
							End:   protocol.Position{Line: uint32(es.line - 1), Character: uint32(e.col + e.length)},
						},
						NewText: e.newToken,
					})
				}
			}
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				updatedLines := mr.applyEditsToLines(fi.lines, sites)
				if err := os.WriteFile(fp, []byte(strings.Join(updatedLines, "\n")), 0644); err != nil {
					log.Printf("Rename: cannot write %s: %v", fp, err)
				}
			}()
		}
	}
	wg.Wait()
	return openChanges
}

// reindex re-parses all touched files asynchronously after the rename.
func (mr *moduleRename) reindex(fileCache map[string]moduleFileInfo, movedFiles map[string]string, openMovedFiles map[string]string) {
	for oldPath := range movedFiles {
		_ = mr.server.store.RemoveFile(oldPath)
	}

	var reindexPaths []string
	for _, newPath := range movedFiles {
		reindexPaths = append(reindexPaths, newPath)
	}

	type textReindex struct {
		path string
		text string
	}
	var openReindexes []textReindex
	for fp := range mr.sitesByFile {
		if _, moved := movedFiles[fp]; moved {
			continue
		}
		fi, ok := fileCache[fp]
		if !ok {
			continue
		}
		updatedLines := mr.applyEditsToLines(fi.lines, mr.sitesByFile[fp])
		updatedText := strings.Join(updatedLines, "\n")
		if newPath, moved := openMovedFiles[fp]; moved {
			// Open file that was moved: reindex at the new path
			openReindexes = append(openReindexes, textReindex{newPath, updatedText})
		} else if fi.open {
			openReindexes = append(openReindexes, textReindex{fp, updatedText})
		} else {
			reindexPaths = append(reindexPaths, fp)
		}
	}

	// Also reindex open moved files that had no edit sites (e.g. the file
	// only contained the defmodule line which is already in allModuleDefs)
	for oldPath, newPath := range openMovedFiles {
		if _, hasSites := mr.sitesByFile[oldPath]; !hasSites {
			reindexPaths = append(reindexPaths, newPath)
		}
	}

	go func() {
		mr.server.reindexPaths(reindexPaths)
		for _, r := range openReindexes {
			defs, refs, err := parser.ParseText(r.path, r.text)
			if err != nil {
				continue
			}
			_ = mr.server.store.IndexFileWithRefs(r.path, defs, refs)
		}
	}()
}

type renameSite struct {
	filePath       string
	line           int
	includeKeyword bool // true for import-only lines where keyword keys ARE function names
}

// buildTextEdits creates a WorkspaceEdit replacing all whole-token occurrences
// of oldToken with newToken. Open buffers are returned in the WorkspaceEdit;
// closed files are written directly to disk in parallel goroutines.
func (s *Server) buildTextEdits(sites []renameSite, oldToken, newToken string) *protocol.WorkspaceEdit {
	// Group sites by file
	sitesByFile := make(map[string][]renameSite, len(sites))
	for _, site := range sites {
		sitesByFile[site.filePath] = append(sitesByFile[site.filePath], site)
	}

	// Read all files in parallel, tracking whether each is open in the editor
	type fileResult struct {
		path  string
		lines []string
		open  bool
	}
	resultsCh := make(chan fileResult, len(sitesByFile))
	for fp := range sitesByFile {
		go func() {
			text, open, ok := s.readFileText(fp)
			if ok {
				resultsCh <- fileResult{fp, strings.Split(text, "\n"), open}
			} else {
				resultsCh <- fileResult{fp, nil, false}
			}
		}()
	}
	type fileInfo struct {
		lines []string
		open  bool
	}
	fileCache := make(map[string]fileInfo, len(sitesByFile))
	for range sitesByFile {
		r := <-resultsCh
		if r.lines != nil {
			fileCache[r.path] = fileInfo{r.lines, r.open}
		}
	}

	// applyTokenEdits applies right-to-left token replacements for the given
	// sites, returning the updated lines. Shared by open and closed file paths.
	applyTokenEdits := func(origLines []string, fileSites []renameSite) []string {
		lines := make([]string, len(origLines))
		copy(lines, origLines)
		for _, site := range fileSites {
			if site.line-1 >= len(lines) {
				continue
			}
			lineText := lines[site.line-1]
			var cols []int
			if site.includeKeyword {
				cols = findAllTokenColumns(lineText, oldToken)
			} else {
				cols = findFunctionTokenColumns(lineText, oldToken)
			}
			for i := len(cols) - 1; i >= 0; i-- {
				lineText = lineText[:cols[i]] + newToken + lineText[cols[i]+len(oldToken):]
			}
			lines[site.line-1] = lineText
		}
		return lines
	}

	openChanges := make(map[protocol.DocumentURI][]protocol.TextEdit)
	var wg sync.WaitGroup
	var reindexPaths []string
	type textReindex struct {
		path string
		text string
	}
	var openReindexes []textReindex

	for fp, fileSites := range sitesByFile {
		fi, ok := fileCache[fp]
		if !ok {
			continue
		}

		// Compute edits once for both TextEdits and reindexing
		updatedLines := applyTokenEdits(fi.lines, fileSites)

		if fi.open {
			// Open buffer: build TextEdits for the editor AND capture updated
			// text for reindexing (computed once, used for both purposes).
			fileURI := protocol.DocumentURI(uri.File(fp))
			for _, site := range fileSites {
				if site.line-1 >= len(fi.lines) {
					continue
				}
				lineText := fi.lines[site.line-1]
				var cols []int
				if site.includeKeyword {
					cols = findAllTokenColumns(lineText, oldToken)
				} else {
					cols = findFunctionTokenColumns(lineText, oldToken)
				}
				for _, col := range cols {
					openChanges[fileURI] = append(openChanges[fileURI], protocol.TextEdit{
						Range: protocol.Range{
							Start: protocol.Position{Line: uint32(site.line - 1), Character: uint32(col)},
							End:   protocol.Position{Line: uint32(site.line - 1), Character: uint32(col + len(oldToken))},
						},
						NewText: newToken,
					})
				}
			}
			openReindexes = append(openReindexes, textReindex{fp, strings.Join(updatedLines, "\n")})
		} else {
			// Closed file: write to disk in parallel
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := os.WriteFile(fp, []byte(strings.Join(updatedLines, "\n")), 0644); err != nil {
					log.Printf("Rename: cannot write %s: %v", fp, err)
				}
			}()
			reindexPaths = append(reindexPaths, fp)
		}
	}
	wg.Wait()

	go func() {
		s.reindexPaths(reindexPaths)
		for _, r := range openReindexes {
			defs, refs, err := parser.ParseText(r.path, r.text)
			if err != nil {
				continue
			}
			_ = s.store.IndexFileWithRefs(r.path, defs, refs)
		}
	}()

	return &protocol.WorkspaceEdit{Changes: openChanges}
}

// reindexPaths re-parses and reindexes a specific set of files sequentially.
// Used after rename to avoid a full project walk.
func (s *Server) reindexPaths(paths []string) {
	for _, fp := range paths {
		defs, refs, err := parser.ParseFile(fp)
		if err != nil {
			continue
		}

		_ = s.store.IndexFileWithRefs(fp, defs, refs)
	}
	if len(paths) > 0 {
		log.Printf("Rename: reindexed %d files", len(paths)) // intentionally always logged — useful for user feedback
	}
}

// isDepsFile returns true if filePath lives under the deps/ directory of some
// Mix project root (a directory containing mix.exs). It walks up from the
// file, and for each mix.exs found checks whether the file falls under that
// directory's deps/ subdirectory. Results are cached by directory so repeated
// calls for files in the same folder are O(1).
func (s *Server) isDepsFile(filePath string) bool {
	dir := filepath.Dir(filePath)

	if s.depsCache != nil {
		s.depsCacheMu.RLock()
		result, ok := s.depsCache[dir]
		s.depsCacheMu.RUnlock()
		if ok {
			return result
		}
	}

	result := isDepsFileUncached(filePath)

	if s.depsCache != nil {
		s.depsCacheMu.Lock()
		s.depsCache[dir] = result
		s.depsCacheMu.Unlock()
	}
	return result
}

func isDepsFileUncached(filePath string) bool {
	sep := string(os.PathSeparator)
	current := filepath.Dir(filePath)
	for {
		parent := filepath.Dir(current)
		if _, err := os.Stat(filepath.Join(current, "mix.exs")); err == nil {
			if strings.HasPrefix(filePath, filepath.Join(current, "deps")+sep) {
				return true
			}
		}
		if parent == current {
			return false
		}
		current = parent
	}
}

// readFileText returns the contents of filePath, preferring the in-memory
// document store for open buffers. The second return indicates whether the
// file is currently open in the editor.
func (s *Server) readFileText(filePath string) (text string, open bool, ok bool) {
	if t, found := s.docs.Get(string(uri.File(filePath))); found {
		return t, true, true
	}
	if data, err := os.ReadFile(filePath); err == nil {
		return string(data), false, true
	}
	return "", false, false
}

// getFileLine returns the text of line lineNum (1-based) from the file at
// filePath, preferring the in-memory document store for open buffers.
// For closed files, only reads up to the target line instead of the whole file.
func (s *Server) getFileLine(filePath string, lineNum int) (string, bool) {
	// Open buffer: extract the single line without splitting the entire text
	if text, ok := s.docs.Get(string(uri.File(filePath))); ok {
		line, found := nthLine(text, lineNum-1)
		if found {
			return line, true
		}
		return "", false
	}
	// Closed file: scan only up to the target line
	f, err := os.Open(filePath)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	current := 0
	for scanner.Scan() {
		current++
		if current == lineNum {
			return scanner.Text(), true
		}
	}
	return "", false
}

// findBareCallRefs scans definition files for bare intra-module calls to
// functionName (not indexed in the store) and returns them as ReferenceResults.
func (s *Server) findBareCallRefs(module, functionName string) []store.ReferenceResult {
	defResults, err := s.store.LookupFunction(module, functionName)
	if err != nil {
		return nil
	}
	defFilePaths := make(map[string]bool, len(defResults))
	for _, r := range defResults {
		defFilePaths[r.FilePath] = true
	}
	var refs []store.ReferenceResult
	for filePath := range defFilePaths {
		fileText, _, ok := s.readFileText(filePath)
		if !ok {
			continue
		}
		for _, lineNum := range FindBareFunctionCalls(fileText, functionName) {
			refs = append(refs, store.ReferenceResult{
				FilePath: filePath,
				Line:     lineNum,
				Kind:     "call",
			})
		}
	}
	return refs
}
func (s *Server) SignatureHelp(ctx context.Context, params *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	docURI := string(params.TextDocument.URI)
	text, ok := s.docs.Get(docURI)
	if !ok {
		return nil, nil
	}

	lineNum := int(params.Position.Line)
	col := int(params.Position.Character)

	funcExpr, argIndex, found := ExtractCallContext(text, lineNum, col)
	if !found {
		return nil, nil
	}

	moduleRef, functionName := ExtractModuleAndFunction(funcExpr)
	if functionName == "" {
		return nil, nil
	}

	aliases := ExtractAliasesInScope(text, lineNum)
	lines := strings.Split(text, "\n")

	// Resolve the function to a store lookup result
	var result *store.LookupResult
	if moduleRef != "" {
		fullModule := resolveModule(moduleRef, aliases)
		if results, err := s.store.LookupFunction(fullModule, functionName); err == nil && len(results) > 0 {
			result = &results[0]
		}
	} else {
		// Bare function — check buffer, imports, use chains, Kernel
		if defLine, found := FindFunctionDefinition(text, functionName); found {
			// Build signature from buffer
			paramNames := extractParamNames(lines, defLine-1)
			if paramNames == nil {
				return nil, nil
			}
			sig := buildSignature(functionName, paramNames, lines, defLine-1)
			return &protocol.SignatureHelp{
				Signatures:      []protocol.SignatureInformation{sig},
				ActiveSignature: 0,
				ActiveParameter: uint32(argIndex),
			}, nil
		}

		for _, mod := range ExtractImports(text) {
			if results, err := s.store.LookupFunction(mod, functionName); err == nil && len(results) > 0 {
				result = &results[0]
				break
			}
		}

		if result == nil {
			if results := s.lookupThroughUse(text, functionName, aliases); len(results) > 0 {
				result = &results[0]
			}
		}

		if result == nil {
			if results, err := s.store.LookupFollowDelegate("Kernel", functionName); err == nil && len(results) > 0 {
				result = &results[0]
			}
		}
	}

	if result == nil {
		return nil, nil
	}

	// Read the definition file, preferring the in-memory doc store
	fileText, _, ok2 := s.readFileText(result.FilePath)
	if !ok2 {
		return nil, nil
	}
	fileLines := strings.Split(fileText, "\n")
	defIdx := result.Line - 1
	if defIdx < 0 || defIdx >= len(fileLines) {
		return nil, nil
	}

	paramNames := extractParamNames(fileLines, defIdx)
	if paramNames == nil {
		return nil, nil
	}

	sig := buildSignature(functionName, paramNames, fileLines, defIdx)
	return &protocol.SignatureHelp{
		Signatures:      []protocol.SignatureInformation{sig},
		ActiveSignature: 0,
		ActiveParameter: uint32(argIndex),
	}, nil
}

func buildSignature(functionName string, paramNames []string, lines []string, defIdx int) protocol.SignatureInformation {
	label := functionName + "(" + strings.Join(paramNames, ", ") + ")"

	var params []protocol.ParameterInformation
	for _, name := range paramNames {
		params = append(params, protocol.ParameterInformation{
			Label: name,
		})
	}

	sig := protocol.SignatureInformation{
		Label:      label,
		Parameters: params,
	}

	// Add @spec and @doc as documentation if present
	doc, spec := extractDocAbove(lines, defIdx)
	var docParts []string
	if spec != "" {
		docParts = append(docParts, "```elixir\n"+spec+"\n```")
	}
	if doc != "" {
		docParts = append(docParts, doc)
	}
	if len(docParts) > 0 {
		sig.Documentation = protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: strings.Join(docParts, "\n\n"),
		}
	}

	return sig
}
func (s *Server) Symbols(ctx context.Context, params *protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	query := params.Query
	if query == "" {
		return nil, nil
	}

	results, err := s.store.SearchSymbols(query, s.stdlibRoot)
	if err != nil {
		return nil, err
	}

	var symbols []protocol.SymbolInformation
	for _, r := range results {
		name := r.Module
		containerName := ""
		if r.Function != "" {
			name = fmt.Sprintf("%s.%s/%d", r.Module, r.Function, r.Arity)
			containerName = r.Module
		}

		symbols = append(symbols, protocol.SymbolInformation{
			Name: name,
			Kind: defKindToSymbolKind(r.Kind),
			Location: protocol.Location{
				URI:   protocol.DocumentURI(uri.File(r.FilePath)),
				Range: lineRange(r.Line - 1),
			},
			ContainerName: containerName,
		})
	}
	return symbols, nil
}
func (s *Server) TypeDefinition(ctx context.Context, params *protocol.TypeDefinitionParams) ([]protocol.Location, error) {
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

	moduleRef, typeName := ExtractModuleAndFunction(expr)
	if typeName == "" {
		return nil, nil
	}

	aliases := ExtractAliasesInScope(text, lineNum)
	fullModule := s.resolveModuleWithNesting(moduleRef, aliases, uriToPath(protocol.DocumentURI(docURI)), lineNum)

	results, err := s.store.LookupFunction(fullModule, typeName)
	if err != nil {
		return nil, nil
	}

	// Filter to only type definitions
	var locations []protocol.Location
	for _, r := range results {
		if r.Kind == "type" || r.Kind == "typep" || r.Kind == "opaque" {
			locations = append(locations, protocol.Location{
				URI:   uri.File(r.FilePath),
				Range: lineRange(r.Line - 1),
			})
		}
	}
	return locations, nil
}
func (s *Server) WillSave(ctx context.Context, params *protocol.WillSaveTextDocumentParams) error {
	return nil
}
func (s *Server) WillSaveWaitUntil(ctx context.Context, params *protocol.WillSaveTextDocumentParams) ([]protocol.TextEdit, error) {
	return s.Formatting(ctx, &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: params.TextDocument.URI},
	})
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

	moduleRef, functionName := ExtractModuleAndFunction(expr)
	if functionName == "" {
		return nil, nil
	}

	aliases := ExtractAliasesInScope(text, lineNum)
	var fullModule string
	if moduleRef != "" {
		fullModule = resolveModule(moduleRef, aliases)
	} else {
		fullModule = s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, lines, lineNum, functionName, aliases)
	}
	if fullModule == "" {
		return nil, nil
	}

	defResults, err := s.store.LookupFunction(fullModule, functionName)
	if err != nil || len(defResults) == 0 {
		return nil, nil
	}

	r := defResults[0]
	nameCol := 0
	if defLine, ok := s.getFileLine(r.FilePath, r.Line); ok {
		if col := findTokenColumn(defLine, functionName); col >= 0 {
			nameCol = col
		}
	}

	item := protocol.CallHierarchyItem{
		Name:   fmt.Sprintf("%s.%s/%d", fullModule, functionName, parser.ExtractArity(lines[lineNum], functionName)),
		Kind:   protocol.SymbolKindFunction,
		Detail: r.Kind,
		URI:    protocol.DocumentURI(uri.File(r.FilePath)),
		Range:  lineRange(r.Line - 1),
		SelectionRange: protocol.Range{
			Start: protocol.Position{Line: uint32(r.Line - 1), Character: uint32(nameCol)},
			End:   protocol.Position{Line: uint32(r.Line - 1), Character: uint32(nameCol + len(functionName))},
		},
		Data: map[string]string{"module": fullModule, "function": functionName},
	}
	return []protocol.CallHierarchyItem{item}, nil
}

// extractCallHierarchyData extracts module and function from a CallHierarchyItem's
// Data field. Handles both map[string]string (direct Go calls) and
// map[string]interface{} (after JSON round-trip).
func extractCallHierarchyData(data interface{}) (module, function string) {
	if m, ok := data.(map[string]interface{}); ok {
		module, _ = m["module"].(string)
		function, _ = m["function"].(string)
	} else if m, ok := data.(map[string]string); ok {
		module = m["module"]
		function = m["function"]
	}
	return
}

func (s *Server) IncomingCalls(ctx context.Context, params *protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	module, functionName := extractCallHierarchyData(params.Item.Data)
	if module == "" || functionName == "" {
		return nil, nil
	}

	// Reuse the same ref collection as References
	refResults, err := s.store.LookupReferences(module, functionName)
	if err != nil {
		return nil, nil
	}

	// Transitive refs via __using__ chains
	for _, mod := range s.findModulesWhoseUsingImports(module) {
		if transitive, err := s.store.LookupReferences(mod, functionName); err == nil {
			refResults = append(refResults, transitive...)
		}
	}

	// Scan definition files for bare intra-module calls (not indexed in store)
	refResults = append(refResults, s.findBareCallRefs(module, functionName)...)

	// Dedup and build incoming calls
	type refKey struct {
		filePath string
		line     int
	}
	seen := make(map[refKey]bool)
	var calls []protocol.CallHierarchyIncomingCall

	for _, r := range refResults {
		if s.stdlibRoot != "" && strings.HasPrefix(r.FilePath, s.stdlibRoot) {
			continue
		}
		k := refKey{r.FilePath, r.Line}
		if seen[k] {
			continue
		}
		seen[k] = true

		// Find the enclosing function for this call site
		callerMod, callerFunc, callerArity, callerLine, found := s.store.LookupEnclosingFunction(r.FilePath, r.Line)
		if !found {
			continue
		}

		nameCol := 0
		if defLine, ok := s.getFileLine(r.FilePath, callerLine); ok {
			if col := findTokenColumn(defLine, callerFunc); col >= 0 {
				nameCol = col
			}
		}

		fromItem := protocol.CallHierarchyItem{
			Name:  fmt.Sprintf("%s.%s/%d", callerMod, callerFunc, callerArity),
			Kind:  protocol.SymbolKindFunction,
			URI:   protocol.DocumentURI(uri.File(r.FilePath)),
			Range: lineRange(callerLine - 1),
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(callerLine - 1), Character: uint32(nameCol)},
				End:   protocol.Position{Line: uint32(callerLine - 1), Character: uint32(nameCol + len(callerFunc))},
			},
			Data: map[string]string{"module": callerMod, "function": callerFunc},
		}

		// The call range is the ref line itself
		callRange := lineRange(r.Line - 1)

		calls = append(calls, protocol.CallHierarchyIncomingCall{
			From:       fromItem,
			FromRanges: []protocol.Range{callRange},
		})
	}

	return calls, nil
}

func (s *Server) OutgoingCalls(ctx context.Context, params *protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	module, functionName := extractCallHierarchyData(params.Item.Data)
	if module == "" || functionName == "" {
		return nil, nil
	}

	// Find the definition location
	defResults, err := s.store.LookupFunction(module, functionName)
	if err != nil || len(defResults) == 0 {
		return nil, nil
	}
	def := defResults[0]

	// Determine the line range of the function body: from the def line to
	// the next function definition (or a generous window if none found).
	endLine := s.store.NextFunctionLine(def.FilePath, def.Line)
	if endLine == 0 {
		endLine = def.Line + 500
	}

	// Query indexed refs within that range
	outRefs, err := s.store.LookupRefsInRange(def.FilePath, def.Line, endLine-1)
	if err != nil {
		return nil, nil
	}

	// Deduplicate by target (module, function) and collect call ranges
	type callTarget struct {
		module   string
		function string
	}
	type targetInfo struct {
		callRanges []protocol.Range
	}
	targets := make(map[callTarget]*targetInfo)
	var targetOrder []callTarget
	for _, ref := range outRefs {
		key := callTarget{ref.Module, ref.Function}
		if _, ok := targets[key]; !ok {
			targets[key] = &targetInfo{}
			targetOrder = append(targetOrder, key)
		}
		targets[key].callRanges = append(targets[key].callRanges, lineRange(ref.Line-1))
	}

	var calls []protocol.CallHierarchyOutgoingCall
	for _, key := range targetOrder {
		info := targets[key]

		// Look up the target function's definition
		targetDefs, err := s.store.LookupFunction(key.module, key.function)
		if err != nil || len(targetDefs) == 0 {
			continue
		}
		td := targetDefs[0]
		if s.stdlibRoot != "" && strings.HasPrefix(td.FilePath, s.stdlibRoot) {
			continue
		}

		nameCol := 0
		if defLine, ok := s.getFileLine(td.FilePath, td.Line); ok {
			if col := findTokenColumn(defLine, key.function); col >= 0 {
				nameCol = col
			}
		}

		toItem := protocol.CallHierarchyItem{
			Name:  fmt.Sprintf("%s.%s", key.module, key.function),
			Kind:  protocol.SymbolKindFunction,
			URI:   protocol.DocumentURI(uri.File(td.FilePath)),
			Range: lineRange(td.Line - 1),
			SelectionRange: protocol.Range{
				Start: protocol.Position{Line: uint32(td.Line - 1), Character: uint32(nameCol)},
				End:   protocol.Position{Line: uint32(td.Line - 1), Character: uint32(nameCol + len(key.function))},
			},
			Data: map[string]string{"module": key.module, "function": key.function},
		}

		calls = append(calls, protocol.CallHierarchyOutgoingCall{
			To:         toItem,
			FromRanges: info.callRanges,
		})
	}

	return calls, nil
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
