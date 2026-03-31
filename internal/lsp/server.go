package lsp

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"

	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/store"
)

type Server struct {
	store           *store.Store
	docs            *DocumentStore
	projectRoot     string
	initialized     bool
	client          protocol.Client
	followDelegates bool
}

func NewServer(s *store.Store, projectRoot string) *Server {
	return &Server{
		store:           s,
		docs:            NewDocumentStore(),
		projectRoot:     projectRoot,
		followDelegates: true, // default
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
				s.client.ShowMessage(context.Background(), &protocol.ShowMessageParams{
					Type:    protocol.MessageTypeInfo,
					Message: "Dexter: building index for the first time, go-to-definition will be available shortly...",
				})
			}
		}

		parser.WalkElixirFiles(s.projectRoot, func(path string, info os.FileInfo) error {
			if !isEmpty {
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

		elapsed := time.Since(start).Round(time.Millisecond)
		log.Printf("Background reindex: %d files updated (%s)", reindexed, elapsed)

		if isEmpty && s.client != nil {
			s.client.ShowMessage(context.Background(), &protocol.ShowMessageParams{
				Type:    protocol.MessageTypeInfo,
				Message: fmt.Sprintf("Dexter: index built (%d files in %s)", reindexed, elapsed),
			})
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

// === LSP Lifecycle ===

func (s *Server) Initialize(ctx context.Context, params *protocol.InitializeParams) (*protocol.InitializeResult, error) {
	if params.RootURI != "" {
		root := uriToPath(params.RootURI)
		if root != "" {
			s.projectRoot = findDexterRoot(root)
		}
	}

	if opts, ok := params.InitializationOptions.(map[string]interface{}); ok {
		if v, ok := opts["followDelegates"].(bool); ok {
			s.followDelegates = v
		}
	}

	if !s.initialized {
		s.initialized = true
		s.backgroundReindex()
		s.watchGitHead()
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
		},
		ServerInfo: &protocol.ServerInfo{
			Name:    "dexter",
			Version: "0.1.4",
		},
	}, nil
}

func (s *Server) Initialized(ctx context.Context, params *protocol.InitializedParams) error {
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

	moduleRef, functionName := ExtractModuleAndFunction(expr)

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

		return nil, nil
	}

	// Module.function call — resolve aliases, then look up
	aliases := ExtractAliases(text)
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
	return nil, nil
}
func (s *Server) CompletionResolve(ctx context.Context, params *protocol.CompletionItem) (*protocol.CompletionItem, error) {
	return nil, nil
}
func (s *Server) Declaration(ctx context.Context, params *protocol.DeclarationParams) ([]protocol.Location, error) {
	return nil, nil
}
func (s *Server) DidChangeConfiguration(ctx context.Context, params *protocol.DidChangeConfigurationParams) error {
	return nil
}
func (s *Server) DidChangeWatchedFiles(ctx context.Context, params *protocol.DidChangeWatchedFilesParams) error {
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
	return nil, nil
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
