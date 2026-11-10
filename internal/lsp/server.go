package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"go.uber.org/zap"

	"github.com/remoteoss/dexter/internal/beam"
	"github.com/remoteoss/dexter/internal/indexer"
	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/pathcase"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/treesitter"
	"github.com/remoteoss/dexter/internal/version"
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
	mtime    int64
	filePath string
	usingBody

	// dispatch holds one body per target of an atom-dispatch __using__, keyed by
	// the atom the consumer passes (`use MyAppWeb, :controller` → "controller").
	// Nil for ordinary __using__ macros. Selection happens at lookup time from
	// the literal atom, so targets never merge.
	dispatch map[string]*usingBody

	// checkedGen is the store's write generation when filePath was last
	// known to be the file that the index gives for the module. A hit with
	// another generation asks the index again, once.
	checkedGen atomic.Uint64
}

type erlangBuildRootState struct {
	runtimeKey string
	loading    bool
}

type erlangRuntimeCache struct {
	otpRelease  string
	codeRootDir string
	moduleNames map[string]bool
	exports     map[string][]ErlangExport
	loading     bool
	readyCh     chan struct{}
}

// WorkspaceEvents receives disk-backed workspace changes from an LSP session.
// A daemon-backed session uses this to feed the workspace's single mutation
// coordinator instead of writing the index independently; an embedded Server
// that owns its store leaves it nil and writes directly.
type WorkspaceEvents interface {
	ReconcileFile(path string)
	// ReconcileSavedFile is ReconcileFile for a file that an editor saved:
	// its spelling is checked whatever its age (a save after a rename that
	// changed only the case can use the old spelling).
	ReconcileSavedFile(path string)
	RemoveFile(path string)
	SetStdlibRoot(ctx context.Context, path string) error
}

// IndexCoordinator contains the write-side state that must be shared by every
// language-service session attached to one store. Session-local state (open
// documents, client capabilities, and connections) deliberately remains on
// Server.
type IndexCoordinator struct {
	reindexing sync.Mutex
	stdlibMu   sync.RWMutex
	stdlibRoot string
	// explicitStdlib is the first stdlibPath that an editor sent; see
	// claimExplicitStdlib. Guarded by stdlibMu.
	explicitStdlib string
	toolchain      toolchainCache // see DetectStdlib and searchMix

	// writes is held for writing by a cold full build and for reading by every
	// single-file write. The bulk path is insert-only and cannot overlap any
	// incremental mutation.
	writes      sync.RWMutex
	unavailable bool // guarded by writes

	backgroundWork sync.WaitGroup

	// reporter tells every attached editor about failures, degraded states,
	// and long work. It is shared like the store: one workspace, one set of
	// conditions.
	reporter *notify.Reporter
	failures fileFailures
	// firstBuildReported makes the first-build report once per workspace.
	firstBuildReported atomic.Bool
	otp                otpOutcomes  // see reportBeamOTP
	beamFailures       beamFailures // see rememberBeamFailure

	// caches are shared by every session; see sharedCaches.
	caches sharedCaches
	// injectedDelegates keeps the defdelegate facades of __using__ quotes;
	// see injectedDelegatesTo.
	injectedDelegates injectedDelegateCache
	// useCalls keeps the use calls of module files for completion; see
	// fileUseCalls.
	useCalls useCallsCache

	// rootPath and rootInfo are the project root and its identity when
	// checkProjectRoot first saw it. See ErrRootGone.
	rootMu   sync.Mutex
	rootPath string
	rootInfo os.FileInfo

	// worktrees caches the nested worktrees that git records for the project
	// root, for single-file writes; see Indexable.
	worktrees worktreeRecords

	// staleFailures counts passes and batches in a row that could not walk
	// or prune; see reportStale.
	staleFailures atomic.Int32

	// work is canceled by CancelWork when the workspace shuts down. A
	// reconciliation pass, a prune, and a cold build check it and stop.
	work       context.Context
	cancelWork context.CancelFunc

	// openDocs is every session's set of open documents. A rename uses it to
	// keep its disk writes away from buffers that another editor has open.
	openDocs *openDocRegistry

	// renameSerial keeps renames one at a time across every session of the
	// workspace, editor and headless (MCP) alike. It is held by a rename from
	// the reads that plan it to the end of its disk writes. Two renames in different sessions therefore
	// cannot read the same file and each write it back with only its own
	// change. renameNotices are the messages of the running rename; they are
	// sent after renameSerial is released, so a slow editor does not hold up
	// the next rename. renameSerial guards them.
	renameSerial  sync.Mutex
	renameNotices []renameNotice
	// lastRenameIndexed closes when the index has the last rename; see
	// beginRename. renameSerial guards it.
	lastRenameIndexed <-chan struct{}
	// renameReport receives what the running rename changed on disk, for a
	// caller without an editor (see RenameSummary), or is nil. renameSerial
	// guards it.
	renameReport *RenameSummary
	// renameCtx is the request of the running rename: a rename whose
	// request ends before its writes changes nothing (see commitDisk).
	renameCtx context.Context
	// flushEvents, when set, waits until the index has every file change
	// the watcher has seen or will see; see SetEventFlusher.
	flushEvents func(context.Context) error
	// clientMoves are the moves that earlier replies asked editors to make,
	// old path → new path, until a rename needs them; see reindexForRename.
	// renameSerial guards it.
	clientMoves map[string]string

	// renameMu is held for writing by a rename only for a short time: while
	// it checks again that no editor opened or changed a file it will write,
	// and puts the staged files in place. didOpen, didChange, and didClose
	// hold it for reading, so no document opens or changes between that
	// check and the writes, and an editor never waits for the slow part of
	// a rename (reading and staging the files).
	renameMu sync.RWMutex
	// onFault is set by the workspace; see OnFault.
	onFault atomic.Pointer[func(what string)]
}

type renameNotice struct {
	sev notify.Severity
	msg string
}

// renameIndexWait bounds how long a rename waits for the index update of the
// rename before it. Tests lower it.
var renameIndexWait = 10 * time.Second

// beginRename starts a rename: it waits for the rename that runs, if any, and
// then for the index update of the last rename, because the rename plans from
// the index: a file that the last rename moved or changed is not where the
// index says until that update is done. The wait costs one channel check when
// the update is done. It stops at renameIndexWait or when ctx ends; the
// rename is then refused. The returned function ends the rename and then
// sends its messages.
func (s *Server) beginRename(ctx context.Context) (end func(), err error) {
	s.index.renameSerial.Lock()
	if last := s.index.lastRenameIndexed; last != nil {
		select {
		case <-last:
		default:
			timer := time.NewTimer(renameIndexWait)
			select {
			case <-last:
				timer.Stop()
			case <-timer.C:
				err = errors.New("the rename was not applied, because the index is still being updated after the last rename. No file was changed; try the rename again in a moment")
			case <-ctx.Done():
				timer.Stop()
				err = ctx.Err()
			}
		}
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		s.index.renameSerial.Unlock()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("the rename was canceled before it changed any file: %w", err)
		}
		s.notifySession(notify.Error, "Dexter: "+err.Error()+".")
		return nil, err
	}
	return func() {
		notices := s.index.renameNotices
		s.index.renameNotices = nil
		s.index.renameSerial.Unlock()
		for _, n := range notices {
			s.notifySession(n.sev, n.msg)
		}
	}, nil
}

// SetEventFlusher gives the coordinator the workspace's event barrier (the
// runtime's FlushEvents). A rename calls it when a file it plans from is gone
// and no earlier reply moved it: a move that the watcher has not indexed yet.
// Without one (an embedded server, a test), the rename removes the gone file
// from the index and goes on. Call it before any session renames.
func (c *IndexCoordinator) SetEventFlusher(flush func(context.Context) error) {
	c.flushEvents = flush
}

// recordClientMoves remembers the file moves a reply asks the editor to make.
// The caller holds renameSerial.
func (c *IndexCoordinator) recordClientMoves(moves map[string]string) {
	if len(moves) == 0 {
		return
	}
	if c.clientMoves == nil || len(c.clientMoves) > 1000 {
		c.clientMoves = make(map[string]string, len(moves))
	}
	for from, to := range moves {
		c.clientMoves[from] = to
	}
}

// takeClientMove returns, and forgets, the new path of a file that an editor
// was asked to move. The caller holds renameSerial.
func (c *IndexCoordinator) takeClientMove(from string) (string, bool) {
	to, ok := c.clientMoves[from]
	if ok {
		delete(c.clientMoves, from)
	}
	return to, ok
}

// renameNotify queues a message of the running rename for the requesting
// editor. The caller holds renameSerial (see beginRename).
func (s *Server) renameNotify(sev notify.Severity, msg string) {
	s.index.renameNotices = append(s.index.renameNotices, renameNotice{sev, msg})
}

// CancelWork stops the reconciliation in flight and makes every later one
// return at once. The workspace calls it first when it shuts down, so a pass
// over a large change set cannot keep the process (and its workspace lock)
// alive for minutes. A stopped pass leaves no file half written: each write
// transaction holds whole files, and a canceled one rolls back. The files it
// did not reach keep their old mtime, so the next start picks them up.
func (c *IndexCoordinator) CancelWork() { c.cancelWork() }

func (c *IndexCoordinator) setStdlibRoot(root string) (string, bool) {
	c.stdlibMu.Lock()
	defer c.stdlibMu.Unlock()
	if c.stdlibRoot == root {
		return c.stdlibRoot, false
	}
	old := c.stdlibRoot
	c.stdlibRoot = root
	return old, true
}

func (c *IndexCoordinator) getStdlibRoot() string {
	c.stdlibMu.RLock()
	defer c.stdlibMu.RUnlock()
	return c.stdlibRoot
}

// NewIndexCoordinator returns write coordination for one workspace store.
func NewIndexCoordinator() *IndexCoordinator {
	work, cancel := context.WithCancel(context.Background())
	return &IndexCoordinator{reporter: notify.New(), work: work, cancelWork: cancel, caches: newSharedCaches(), openDocs: newOpenDocRegistry()}
}

// ServerOptions configures a Server attached to a daemon-owned workspace.
// Embedded callers that own their store and workspace lifecycle — the
// package's tests, for example — use NewServer instead.
type ServerOptions struct {
	Index             *IndexCoordinator
	Events            WorkspaceEvents
	ManageWorkspace   bool
	InitialStdlibRoot string
}

type Server struct {
	store           *store.Store
	docs            *DocumentStore
	projectRoot     string
	explicitRoot    bool // true when projectRoot was provided via CLI, not inferred from Initialize
	initialized     bool
	client          protocol.Client
	clientLog       *clientLog // forwards this session's log lines to its editor
	followDelegates bool
	debug           bool
	mixBin          string // resolved path to the mix binary

	beams  map[string]*beamProcess // build root → persistent BEAM process
	beamMu sync.Mutex
	// beamsClosed is set by closeBeams under beamMu; getBeamProcess then
	// starts nothing.
	beamsClosed bool

	erlangBuildRoots   map[string]*erlangBuildRootState // build root → runtime resolution state
	erlangRuntimeCache map[string]*erlangRuntimeCache   // runtime key → cached OTP modules/exports
	erlangRuntimeMu    sync.Mutex

	// The caches below are the IndexCoordinator's, shared by every session of
	// the workspace; see sharedCaches.
	usingCache     *usingCache // module name → parsed __using__ result
	generatedCache *generatedFunctionCache
	beamLibs       *beamLibIndexCache // build root → compiled application directories
	buildRoots     *buildRootCache    // compiled Mix projects in the workspace
	mixApps        *mixAppCache       // mix.exs → application name
	ebinIndexes    *ebinIndexCache    // ebin dir → modules compiled into it
	depsCache      *depsCache         // dir → whether files in that dir are deps

	conn                   jsonrpc2.Conn // raw connection for server-initiated requests not on the Client interface
	showDocumentSupported  bool          // client supports window/showDocument (LSP 3.16+)
	renameFileOpsSupported bool          // client applies rename resource operations in a WorkspaceEdit
	snippetSupport         bool          // client supports snippet insert text in completions
	// positionEncoding is the unit the client counts Position.Character in.
	// Dexter never negotiates, so this is always UTF-16 in practice — see the
	// type's doc comment for why — and columns are converted at the edges by
	// inCol/outCol. It stays a field rather than a constant so a future
	// negotiation can set it without touching every call site.
	positionEncoding PositionEncoding

	index           *IndexCoordinator
	workspaceEvents WorkspaceEvents
	manageWorkspace bool
	closeOnce       sync.Once
	sessionClosed   atomic.Bool

	workDoneProgress bool         // client supports window/workDoneProgress/create
	mixMissing       bool         // Initialize found no mix binary for this session
	stdlibConflict   string       // a message for this editor: its stdlibPath is not used
	detachReporter   atomic.Value // func(); set when the client sent initialized
}

func (s *Server) debugf(format string, args ...interface{}) {
	if s.debug {
		line := "[debug] " + fmt.Sprintf(format, args...)
		log.Print(line)
		s.clientLog.send(line)
	}
}

func (s *Server) debugNow() time.Time {
	if s.debug {
		return time.Now()
	}
	return time.Time{}
}

func NewServer(s *store.Store, projectRoot string) *Server {
	return NewServerWithOptions(s, projectRoot, ServerOptions{ManageWorkspace: true})
}

// NewServerWithOptions constructs an LSP session over shared workspace state.
// Daemon sessions pass one IndexCoordinator and set ManageWorkspace false so
// only the daemon performs startup reconciliation and owns watchers.
func NewServerWithOptions(s *store.Store, projectRoot string, opts ServerOptions) *Server {
	index := opts.Index
	if index == nil {
		index = NewIndexCoordinator()
	}
	if opts.InitialStdlibRoot != "" {
		_, _ = index.setStdlibRoot(opts.InitialStdlibRoot)
	}
	docs := NewDocumentStore()
	docs.registry = index.openDocs
	return &Server{
		store:           s,
		docs:            docs,
		projectRoot:     projectRoot,
		explicitRoot:    projectRoot != "",
		followDelegates: true,
		// Read here as well as in Initialize: the daemon's headless service
		// answers CLI and MCP calls and never receives an initialize request.
		debug:              os.Getenv("DEXTER_DEBUG") == "true",
		erlangBuildRoots:   make(map[string]*erlangBuildRootState),
		erlangRuntimeCache: make(map[string]*erlangRuntimeCache),
		usingCache:         index.caches.using,
		generatedCache:     index.caches.generated,
		beamLibs:           index.caches.beamLibs,
		buildRoots:         index.caches.buildRoots,
		mixApps:            index.caches.mixApps,
		ebinIndexes:        index.caches.ebinIndexes,
		depsCache:          index.caches.deps,
		index:              index,
		workspaceEvents:    opts.Events,
		manageWorkspace:    opts.ManageWorkspace,
		// What every client sends when the server declares no encoding, which
		// Dexter does not.
		positionEncoding: EncodingUTF16,
	}
}

// StdlibRoot returns the workspace-wide stdlib root shared by all sessions.
func (s *Server) StdlibRoot() string { return s.index.getStdlibRoot() }

// SetStdlibRoot changes the workspace-wide stdlib root and returns its previous
// value. Workspace runtimes use both values to reconcile the shared index.
func (s *Server) SetStdlibRoot(root string) (string, bool) { return s.index.setStdlibRoot(root) }

func (s *Server) isStdlibPath(path string) bool {
	root := s.StdlibRoot()
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// ServeStream runs one preconstructed LSP session over a stream the session is
// allowed to close. Workspace daemons use this so an editor's `exit`
// notification ends that session — releasing its registration and its lease on
// the daemon — instead of waiting for the peer to hang up. Sessions share index
// coordination while keeping independent document overlays and client
// capabilities.
//
// A closed stream is what a normal shutdown looks like, so it is reported as nil.
func ServeStream(server *Server, rwc io.ReadWriteCloser) error {
	logger, _ := zap.NewProduction()
	stream := jsonrpc2.NewStream(rwc)
	conn := jsonrpc2.NewConn(stream)
	server.client = protocol.ClientDispatcher(conn, logger)
	server.conn = conn
	server.clientLog = startClientLog(server.client, conn.Done())

	// Messages run one after the other. A slow request runs off the reader's
	// goroutine, so that a $/cancelRequest reaches it (see serialDispatcher).
	// The recover is inside, on the goroutine that runs the handler.
	dispatcher := newSerialDispatcher(server.recoverHandler(hookHandler(server.renameHandler(protocol.ServerHandler(server, nil)))))
	handler := cancelHandler(dispatcher.handle)
	// Every handler's context comes from this one, which ends with the
	// connection: work for a client that is gone stops.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn.Go(ctx, handler)
	<-conn.Done()
	cancel()
	// The caller releases the session when this returns. A queued message
	// must not run on a released session (it could start a BEAM that nobody
	// stops), so the queue is dropped, and the message that runs now gets a
	// moment to see its canceled context.
	if !dispatcher.close(sessionDrainWait) {
		log.Printf("Warning: an LSP request still runs %s after its connection closed", sessionDrainWait)
	}
	server.detachFromReporter()
	if err := conn.Err(); err != nil && !isStreamClosed(err) {
		return err
	}
	return nil
}

// sessionDrainWait bounds how long a closed connection waits for the request
// that runs on it. A request that does not check its context outlives it; the
// session then refuses to start anything new (see CloseSession).
var sessionDrainWait = 5 * time.Second

// isStreamClosed reports whether an error is just the stream ending: the expected
// result of an `exit` notification or a disconnecting client, not a failure worth
// surfacing to an editor's log.
func isStreamClosed(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE)
}

// WaitForIndexWork waits for background index writes already accepted by this
// workspace. Callers must prevent new sessions from adding work first.
func (s *Server) WaitForIndexWork() {
	s.index.backgroundWork.Wait()
}

// OpenDocuments reports how many buffers this session holds. Daemon status
// uses it to describe attached editor sessions; it is diagnostic only.
func (s *Server) OpenDocuments() int { return s.docs.Count() }

// ProjectRoot returns the workspace root this session answers for.
func (s *Server) ProjectRoot() string { return s.projectRoot }

// warmUsingCache parses every module's defmacro __using__ body ahead of the
// first request that needs one.
//
// findModulesWhoseUsingImports, on the References slow path, has to consult
// every __using__ module in the codebase. Those bodies are cached, but nothing
// fills the cache, so the first such request pays for all of them at once: on a
// 70k-file monorepo that is 563 file reads and parses, about 440ms, in front of
// a user waiting on a reference lookup. Every later request costs 1-2ms.
//
// The work is the same either way; this only moves it off the request path. It
// runs at the end of the background reindex, where the index is known to exist
// and no request is waiting, and it is bounded to NumCPU readers so that a
// speculative warm-up cannot storm the filesystem while the editor is starting.
//
// Nothing here is required for correctness. Entries are validated against file
// mtime when they are read, so a warmed entry that goes stale is simply
// re-parsed, and a file that cannot be read is skipped exactly as it would be
// on the request path.
func (s *Server) warmUsingCache() {
	usingModules, err := s.store.LookupUsingModules()
	if err != nil || len(usingModules) == 0 {
		return
	}

	usingModules = uniqueUsingModules(usingModules)

	start := time.Now()
	workers := runtime.NumCPU()
	if workers > len(usingModules) {
		workers = len(usingModules)
	}

	var (
		next atomic.Int64
		wg   sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.index.reporter.Recover("cache", "the warm-up of the __using__ cache")
			for {
				i := int(next.Add(1)) - 1
				if i >= len(usingModules) {
					return
				}
				s.cachedUsingWithPath(usingModules[i].Module, usingModules[i].FilePath)
			}
		}()
	}
	wg.Wait()

	s.debugf("warmUsingCache: %d __using__ modules in %s", len(usingModules), time.Since(start).Round(time.Millisecond))
}

// uniqueUsingModules keeps one file for each module, with the rule of
// usingSourceFile: the first path in byte order. Each row of
// LookupUsingModules is one (module, file) pair, so a module defined in two
// files has two rows; a caller that passed both to cachedUsingWithPath would
// make each replace the entry of the other and parse it again.
func uniqueUsingModules(usingModules []store.UsingModule) []store.UsingModule {
	chosen := make(map[string]int, len(usingModules))
	unique := make([]store.UsingModule, 0, len(usingModules))
	for _, m := range usingModules {
		m.FilePath = filepath.Clean(m.FilePath)
		if i, ok := chosen[m.Module]; ok {
			if m.FilePath < unique[i].FilePath {
				unique[i].FilePath = m.FilePath
			}
			continue
		}
		chosen[m.Module] = len(unique)
		unique = append(unique, m)
	}
	return unique
}

// pruneMissingFiles removes stored files that the sweep did not see on disk, and
// stored files inside a nested worktree or below a symlinked directory, which
// the sweep skips.
//
// It holds indexWrites for writing, so no single-file write can land between
// the decision and the delete, and it re-checks each candidate against the
// filesystem first. The re-check is what makes the prune safe rather than
// merely unlikely to be wrong: seen is one traversal's answer, and a file
// created and saved after that traversal passed its directory is absent from
// seen but present on disk and in the index.
//
// It is also the backstop for the two walkers ever disagreeing about the file
// set again. A walk that yields nothing then deletes nothing, because every
// stored path still stats — while a project whose last file was genuinely
// removed is still pruned, which is why the check is per candidate rather than
// a test on seen being empty.
//
// The per-candidate check cannot see a project root that went away: then every
// stored path is missing. So the prune checks the root, under the lock, before
// it deletes anything, and returns an ErrRootGone error instead. It returns an
// ErrIncomplete error when the delete fails.
func (s *Server) pruneMissingFiles(seen map[string]struct{}) error {
	ctx := s.index.work
	if ctx.Err() != nil {
		return nil
	}
	s.index.writes.Lock()
	defer s.index.writes.Unlock()
	if s.index.unavailable {
		return nil
	}

	storedPaths, err := s.store.ListFilePaths()
	if err != nil {
		return fmt.Errorf("%w: listing stored files: %v", ErrIncomplete, err)
	}

	var toRemove []string
	// Files in a nested worktree exist, but no walk yields them, and indexes
	// built before worktrees were skipped still hold them. The answer is the
	// same for every file in a directory, so it is looked up once per directory,
	// and only for the few paths the sweep did not see. The walk also skips
	// worktrees that git records after their .git file is gone.
	//
	// A file below a symlinked directory exists too, through the link, but the
	// walk does not follow a symlinked directory, so it never yields that path.
	// A directory that became a symlink (a checkout can do that) left such
	// rows, which duplicated the files the walk indexes at their real path.
	inWorktree := make(map[string]bool)
	viaLink := make(map[string]bool)
	var recorded map[string]struct{}
	recordedRead := false
	for i, storedPath := range storedPaths {
		if _, ok := seen[storedPath]; ok {
			continue
		}
		if i&1023 == 0 && ctx.Err() != nil {
			return nil
		}
		// No walk yields a path below neither root, or a path in a
		// directory that the walk skips, so the index does not keep one
		// (see Indexable). Both are string work.
		root := s.walkRootOf(storedPath)
		if root == "" || hasSkippedDir(root, storedPath) {
			toRemove = append(toRemove, storedPath)
			continue
		}
		_, err := os.Lstat(storedPath)
		if err != nil && !IsMissing(err) {
			continue
		}
		if err == nil {
			dir := filepath.Dir(storedPath)
			in, ok := inWorktree[dir]
			if !ok {
				if !recordedRead {
					recorded, recordedRead = s.index.worktrees.refresh(s.projectRoot), true
				}
				in = underTop(s.projectRoot, dir, recorded) || parser.InLinkedWorktree(s.projectRoot, storedPath) ||
					s.underSymlinkedDir(dir, viaLink)
				inWorktree[dir] = in
			}
			// A path in the old spelling after a rename that changed only
			// the case: a stat finds the new spelling, but the walk saw only
			// that one. One open and one fcntl, only for unseen paths.
			if !in && pathcase.SpelledBelow(root, storedPath) {
				continue
			}
		}
		toRemove = append(toRemove, storedPath)
	}
	if len(toRemove) == 0 {
		return nil
	}
	if err := s.checkProjectRoot(); err != nil {
		return err
	}
	start := time.Now()
	err = s.testRemoveFault(toRemove)
	if err == nil {
		err = s.store.RemoveFilesContext(ctx, toRemove)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		log.Printf("Warning: removing %d files from the index: %v", len(toRemove), err)
		return fmt.Errorf("%w: removing %d files: %v", ErrIncomplete, len(toRemove), err)
	}
	log.Printf("Removed %d files from the index (%s)", len(toRemove), time.Since(start).Round(time.Millisecond))
	return nil
}

// underSymlinkedDir reports whether dir, or a directory between dir and the
// walk root that holds it (the project root or the stdlib root), is a symlink.
// The root itself is not checked: a root spelled through a symlink is followed
// by the walk. Each component is checked with Lstat, not by a compare with
// filepath.EvalSymlinks, which would also resolve the root. cache holds the
// answer per directory; a cached ancestor gives the answer for the components
// above it. Only paths the walk did not see get here, so it costs nothing on a
// normal pass.
func (s *Server) underSymlinkedDir(dir string, cache map[string]bool) bool {
	root := s.walkRootOf(dir)
	if root == "" {
		return false
	}
	result := false
	for d := dir; len(d) > len(root); d = filepath.Dir(d) {
		if v, ok := cache[d]; ok {
			result = v
			break
		}
		if info, err := os.Lstat(d); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			result = true
			break
		}
	}
	if cache != nil {
		cache[dir] = result
	}
	return result
}

// walkRootOf returns the walk root (the project root or the stdlib root) that
// holds path below it, or "" when path is below neither. path must be clean.
func (s *Server) walkRootOf(path string) string {
	for _, r := range []string{s.projectRoot, s.StdlibRoot()} {
		if r != "" && len(path) > len(r) && strings.HasPrefix(path, r) && path[len(r)] == filepath.Separator {
			return r
		}
	}
	return ""
}

// InWalkRoot reports whether path is below the project root or the stdlib
// root. It is string work only.
func (s *Server) InWalkRoot(path string) bool { return s.walkRootOf(filepath.Clean(path)) != "" }

// Indexable reports whether the full walk can yield path: it is below the
// project root or the stdlib root, spelled as the walk spells it, and not
// below a symlinked directory or in a nested worktree. The index holds only
// such paths. A path dependency outside the project root is not indexed, and
// neither is another spelling of the root through a symlink: the prune could
// not tell such a row from a stale one, and it duplicated the walk's rows.
// The check is string work plus one Lstat per directory level below the root,
// and a read of a .git file only where one exists.
func (s *Server) Indexable(path string) bool {
	root := s.walkRootOf(path)
	if root == "" || hasSkippedDir(root, path) {
		return false
	}
	dir := filepath.Dir(path)
	// A worktree that git still records after its .git file is gone, as
	// during git worktree remove; the walk skips it too. The cached records
	// can be stale: git worktree move rewrites only a file inside a record,
	// and NFS can cache the directory's mtime for up to a minute. So a
	// rejection, which is rare, reads the records again first.
	if root == s.projectRoot && underTop(root, dir, s.index.worktrees.get(root)) &&
		underTop(root, dir, s.index.worktrees.refresh(root)) {
		return false
	}
	return !s.underSymlinkedDir(dir, nil) && !parser.InLinkedWorktree(s.projectRoot, path)
}

// hasSkippedDir reports whether a directory between root and path is one that
// the walk skips (_build, .git, node_modules). path must be below root.
func hasSkippedDir(root, path string) bool {
	rel := path[len(root)+1:]
	for {
		i := strings.IndexByte(rel, filepath.Separator)
		if i < 0 {
			return false
		}
		if parser.SkipDir(rel[:i]) {
			return true
		}
		rel = rel[i+1:]
	}
}

// worktreeRecords caches parser.NestedWorktreeSet for one root. The set
// changes when git adds or drops a worktree record, which changes the mtime
// of the records directory, so a cached set costs one stat to check.
//
// A file system can store mtimes coarsely (one second, or more on some
// network and VM mounts), so a change soon after a read can leave the same
// mtime. As git does for its index, a set read less than racyWindow after
// the directory's mtime is not trusted, and is read again.
type worktreeRecords struct {
	mu     sync.Mutex
	root   string
	dir    string // parser.WorktreeRecordsDir; "" when root has no .git
	stamp  int64  // mtime of dir when set was read; -1 when dir is missing
	readAt int64  // when set was read
	set    map[string]struct{}
	valid  bool
}

// racyWindow bounds the mtime granularity that worktreeRecords allows for.
const racyWindow = int64(2 * time.Second)

// get returns the recorded worktrees below root, read again when the records
// changed.
func (w *worktreeRecords) get(root string) map[string]struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.load(root, false)
}

// refresh reads the records again. A full pass uses it, so a change that
// leaves the directory's mtime alone (git worktree move rewrites a file in a
// record) is seen at the next pass.
func (w *worktreeRecords) refresh(root string) map[string]struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.load(root, true)
}

func (w *worktreeRecords) load(root string, force bool) map[string]struct{} {
	if w.root != root || w.dir == "" {
		w.root, w.dir, w.set, w.valid = root, "", nil, false
		var ok bool
		if w.dir, ok = parser.WorktreeRecordsDir(root); !ok {
			return nil
		}
	}
	stamp := int64(-1)
	if info, err := os.Stat(w.dir); err == nil {
		stamp = info.ModTime().UnixNano()
	}
	if force || !w.valid || stamp != w.stamp || w.readAt-w.stamp < racyWindow {
		w.readAt = time.Now().UnixNano()
		w.set, w.stamp, w.valid = parser.NestedWorktreeSet(root), stamp, true
	}
	return w.set
}

// underTop reports whether dir is one of tops or lies below one, not counting
// root itself.
func underTop(root, dir string, tops map[string]struct{}) bool {
	if len(tops) == 0 {
		return false
	}
	for ; len(dir) > len(root); dir = filepath.Dir(dir) {
		if _, ok := tops[dir]; ok {
			return true
		}
	}
	return false
}

// notifySession logs a report about this editor's own request and shows it
// to this editor only. Workspace states go through the shared reporter.
func (s *Server) notifySession(sev notify.Severity, message string) {
	prefix := map[notify.Severity]string{notify.Error: "Error: ", notify.Warning: "Warning: "}[sev]
	log.Printf("%s%s", prefix, strings.TrimPrefix(message, "Dexter: "))
	if s.client != nil {
		notify.Show(context.Background(), s.client, sev, message)
	}
}

// detachFromReporter stops sending workspace reports to this editor.
func (s *Server) detachFromReporter() {
	if detach, ok := s.detachReporter.Load().(func()); ok {
		detach()
	}
}

// fullBuild rebuilds the whole index in-process, through the same pipeline
// `dexter init` uses: parse on every core, then one bulk transaction with the
// indexes dropped. It is only correct on an empty index, so backgroundReindex
// is the only caller.
//
// The emptiness test is made here, under the same lock that holds for the whole
// build, and reported through the ran return value. A caller cannot sample it
// beforehand and act on the sample: a save or a watched-file event arriving in
// between takes indexWrites for reading unopposed and writes a row, and the
// build must then be skipped rather than attempted. ran is false with a nil
// error for exactly that case, which is not a failure — the incremental path
// handles what is already there.
//
// The whole build is one WAL transaction, so the -wal file grows to roughly the
// size of the index before backgroundReindex's checkpoint reclaims it;
// wal_autocheckpoint cannot touch frames belonging to an open transaction. On a
// very large repository that is a few hundred MiB of transient disk.
//
// InProcess suppresses the connection-wide pragmas. They cannot be applied
// reliably or undone on a live pool — leaving WAL needs exclusive access, and
// the per-connection ones land on whichever pooled connection happens to serve
// them. They were also the smallest part of the win.
// testHookFullBuild, when set by a test, makes the fast full build fail with
// its error, so that the incremental fallback runs.
var testHookFullBuild func() error

func (s *Server) fullBuild() (stats indexer.Stats, ran bool, err error) {
	s.index.writes.Lock()
	defer s.index.writes.Unlock()
	if !s.store.IsEmpty() {
		return indexer.Stats{}, false, nil
	}
	if testHookFullBuild != nil {
		if err := testHookFullBuild(); err != nil {
			return indexer.Stats{}, true, err
		}
	}

	var failed buildFailures
	stats, err = indexer.FullBuild(s.store, s.projectRoot, indexer.Options{
		StdlibRoot: s.StdlibRoot(),
		InProcess:  true,
		Context:    s.index.work,
		Warn: func(format string, args ...interface{}) {
			log.Printf("Warning: "+format, args...)
		},
		FileError: failed.add,
	})
	if errors.Is(err, indexer.ErrUnindexed) {
		s.index.unavailable = true
	}
	if err == nil {
		// The build saw every file, so its failures are the whole set.
		s.index.failures.replace(failed.paths)
	}
	return stats, true, err
}

// indexOneFile parses and indexes a single file, the incremental path. It
// returns an ErrIncomplete error when the write fails.
func (s *Server) indexOneFile(path string) error {
	s.index.writes.RLock()
	defer s.index.writes.RUnlock()
	if s.index.unavailable {
		return nil
	}
	err := s.indexOneFileLocked(path)
	s.index.reportFileFailures()
	return err
}

// indexOneFileLocked is indexOneFile for callers already holding indexWrites.
// Go's RWMutex is not reentrant, so the two must stay separate. A file that
// does not parse is not a write failure: no retry can change it, and the next
// save or pass reads it again.
// testHookAfterParse, when set by a test, runs in indexOneFileLocked after
// the parse and before the write.
var testHookAfterParse func(path string)

func (s *Server) indexOneFileLocked(path string) error {
	// Watchers and editors report changes in nested worktrees, below
	// symlinked directories, and outside the roots too. The full walk never
	// yields such a path, so a single-file write must skip it as well, or the
	// next prune removes it again.
	path = filepath.Clean(path)
	if !s.Indexable(path) {
		return nil
	}
	// The stat comes first; see store.IndexFileWithRefsAt. A file that cannot
	// be read is what the parse reported before: no retry can change it.
	info, err := os.Stat(path)
	if err != nil {
		log.Printf("Error parsing %s: %v", path, err)
		s.index.failures.fail(path, err)
		return nil
	}
	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		log.Printf("Error parsing %s: %v", path, err)
		s.index.failures.fail(path, err)
		return nil
	}
	if testHookAfterParse != nil {
		testHookAfterParse(path)
	}
	err = testWriteFault(path)
	if err == nil {
		err = s.store.IndexFileWithRefsAt(path, info.ModTime().UnixNano(), defs, refs)
	}
	if err != nil {
		log.Printf("Error indexing %s: %v", path, err)
		s.index.failures.fail(path, err)
		return fmt.Errorf("%w: %s: %v", ErrIncomplete, path, err)
	}
	s.index.failures.ok(path)
	return nil
}

// backgroundReindex runs in the background. If the index is empty it does a
// full build, otherwise it does an incremental mtime-based update.
func (s *Server) backgroundReindex() {
	s.startBackgroundReindex()
}

// startBackgroundReindex starts one pass. The channel receives its result and
// then closes: nil for a complete or canceled pass, an ErrRootGone error when
// the project root is gone, and an ErrIncomplete error when some files could
// not be written and a later pass must write them.
func (s *Server) startBackgroundReindex() <-chan error {
	done := make(chan error, 1)
	s.index.backgroundWork.Add(1)
	go func() {
		var result error
		defer func() {
			done <- result
			close(done)
		}()
		defer s.index.backgroundWork.Done()
		s.index.reindexing.Lock()
		defer s.index.reindexing.Unlock()
		// Deferred last so it runs first: the locks above are released after
		// it, and a write transaction that the panic left open is the reason
		// the workspace is told to restart its index.
		defer s.index.reporter.Recover("index", "the background index pass", s.index.faulted("the background index pass"))
		if s.index.work.Err() != nil {
			return // the workspace is shutting down
		}
		// The first check records the root's identity, so a later pass can
		// find a root that was replaced, even when this pass is a cold build.
		if err := s.checkProjectRoot(); err != nil {
			log.Printf("Error: background reindex stopped, the index is not changed: %v", err)
			if errors.Is(err, ErrIncomplete) {
				s.reportStale(err)
			}
			result = err
			return
		}
		// A full pass follows a mix.exs change, lost events and branch
		// switches: each can move the Mix projects.
		s.index.manifestChanged()

		start := time.Now()
		reindexed, failed := 0, 0
		coldStart := s.store.IsEmpty()
		fullBuilt := false
		var buildTask *notify.Task
		progress := reconcileProgress{reporter: s.index.reporter}

		if coldStart {
			buildTask = s.beginIndexBuild()

			stats, ran, err := s.fullBuild()
			switch {
			case errors.Is(err, indexer.ErrUnindexed):
				// The SQL indexes did not come back after the bulk load committed
				// or rolled back, so every query is a full table scan. Falling back
				// to the incremental walk would be far worse than doing nothing:
				// each per-file write issues a DELETE by file_id against definitions
				// and refs tables, once per file on disk. FullBuild leaves the
				// index version unset, so the next editor start rebuilds from
				// scratch through cmdInit — in a process with no live readers,
				// where deleting the database is safe.
				s.indexBuildFailedUnindexed(buildTask, err)
				// Collapse any committed data in the WAL rather than leaving it at
				// its high-water mark for the rest of the process.
				if err := s.store.Checkpoint(); err != nil {
					log.Printf("Warning: WAL checkpoint: %v", err)
				}
				return
			case s.index.work.Err() != nil:
				log.Printf("Index build canceled")
				return
			case err != nil:
				// The incremental walk below needs nothing to be true of the
				// database, so it is the safe thing to fall back to. It is
				// slower, not wrong.
				s.indexBuildFellBack(err)
			case !ran:
				// Something wrote to the index between the check above and the
				// build's lock. Nothing was built, and the incremental path
				// below covers whatever is there.
				log.Printf("Index was no longer empty at build time, using incremental reindex")
			default:
				fullBuilt = true
				reindexed = stats.Files
			}
		}

		// A full build already indexed every file on disk from the traversal
		// this walk would repeat, so skipping it saves a second traversal. The
		// prune lives in the same branch and so cannot run without the walk
		// that fills `seen`.
		//
		// A pass that could not walk the project root stops before the prune
		// and before anything else: a root that was moved, deleted, or
		// unmounted walks as an empty project, and the prune would then delete
		// every stored file.
		if !fullBuilt {
			// A cold build that fell back to this pass already shows its
			// own progress; the pass counts its files only when it is warm.
			var passProgress *reconcileProgress
			if !coldStart {
				passProgress = &progress
			}
			seen, n, f, err := s.reconcileChangedFiles(passProgress)
			reindexed += n
			failed += f
			var pruneErr error
			if err == nil {
				pruneErr = s.pruneMissingFiles(seen)
				if errors.Is(pruneErr, ErrRootGone) {
					err = pruneErr
				} else if pruneErr == nil {
					// Failures of files that the walk no longer finds are
					// gone. Not after a failed prune: the files whose
					// delete failed are still in the index, and they
					// stay listed as failures.
					s.index.failures.retain(seen)
				}
			}
			if s.index.work.Err() != nil {
				log.Printf("Background reindex canceled after %d files (%s)", reindexed, time.Since(start).Round(time.Millisecond))
				buildTask.End("")
				return
			}
			if err != nil {
				buildTask.End("")
			}
			switch {
			case errors.Is(err, ErrRootGone):
				log.Printf("Error: background reindex stopped, nothing was pruned: %v", err)
				result = err
				return
			case errors.Is(err, errIndexUnavailable):
				return
			case errors.Is(err, ErrIncomplete):
				log.Printf("Error: background reindex stopped, nothing was pruned: %v", err)
				s.reportStale(err)
				result = err
				return
			case err != nil:
				log.Printf("Error: background reindex stopped, nothing was pruned: %v", err)
				result = fmt.Errorf("%w: %v", ErrIncomplete, err)
				s.reportStale(result)
				return
			}
			result = pruneErr
			if coldStart && pruneErr == nil {
				// The cold build fell back to this pass, which wrote every
				// file in the current format. Without the version, the next
				// start would take the index for an unfinished build and
				// delete it.
				if err := s.store.SetIndexVersion(version.IndexVersion); err != nil {
					log.Printf("Warning: failed to store index version: %v", err)
				}
			}
		}
		if result != nil {
			s.reportStale(result)
		} else {
			s.clearStale()
		}

		// Collapse the WAL back to disk now that the (potentially large) reindex
		// is complete, so the -wal file does not stay parked at its high-water
		// mark for the lifetime of the LSP process.
		if err := s.store.Checkpoint(); err != nil {
			log.Printf("Warning: WAL checkpoint after reindex: %v", err)
		}

		// The index is in place now, so fill the __using__ cache before a user
		// asks for it. See warmUsingCache.
		s.warmUsingCache()

		elapsed := time.Since(start).Round(time.Millisecond)
		if failed > 0 {
			// The failed files keep their old stored mtime, so the next pass
			// writes them again. The caller schedules that pass, and
			// finishReindex shows them to the editor.
			log.Printf("Background reindex: %d files updated, %d failed (%s)", reindexed, failed, elapsed)
			result = errors.Join(result, fmt.Errorf("%w: %d files", ErrIncomplete, failed))
		} else {
			log.Printf("Background reindex: %d files updated (%s)", reindexed, elapsed)
		}
		progress.end(elapsed)
		s.finishReindex(buildTask, reindexed, failed, elapsed)
	}()
	return done
}

// ReindexWorkspace schedules the standard full-or-incremental reconciliation
// and waits for all accepted index work. Daemon mutation coordinators call it
// serially, so a concurrent pass can safely satisfy the request. It returns
// the pass's result; see startBackgroundReindex.
func (s *Server) ReindexWorkspace() error {
	return <-s.startBackgroundReindex()
}

// ReconcileFile applies one disk-backed file change under the workspace-wide
// mutation lock shared by every attached LSP session. It returns an
// ErrIncomplete error when the write fails.
func (s *Server) ReconcileFile(path string) error {
	s.index.reindexing.Lock()
	defer s.index.reindexing.Unlock()
	return s.indexOneFile(path)
}

// RemoveFile removes one deleted path under the workspace-wide mutation lock.
// A failed delete is logged, and the next pass's prune removes the path.
func (s *Server) RemoveFile(path string) {
	_ = s.RemoveFiles([]string{path})
}

// RemoveFiles removes disk paths as one workspace mutation.
//
// It returns an ErrIncomplete error when the delete fails (a full disk can
// fail a delete too), so that the caller schedules a full pass, whose prune
// removes the files.
func (s *Server) RemoveFiles(paths []string) error {
	s.index.reindexing.Lock()
	defer s.index.reindexing.Unlock()
	s.index.writes.RLock()
	defer s.index.writes.RUnlock()
	if s.index.unavailable {
		return nil
	}
	// A large removal can rebuild the symbol tables, which takes seconds on a
	// large index, so shutdown must be able to stop it. A canceled removal
	// leaves the files in the index, and the next start's prune removes them.
	err := s.testRemoveFault(paths)
	if err == nil {
		err = s.store.RemoveFilesContext(s.index.work, paths)
	}
	if err != nil && s.index.work.Err() == nil {
		log.Printf("Error removing %d files from index: %v", len(paths), err)
		for _, path := range paths {
			s.index.failures.fail(path, err)
		}
		s.index.reportFileFailures()
		return fmt.Errorf("%w: removing %d files: %v", ErrIncomplete, len(paths), err)
	}
	for _, path := range paths {
		s.index.failures.removed(path)
	}
	s.index.reportFileFailures()
	return nil
}

// testRemoveFault applies SetTestWriteFault to a removal.
func (s *Server) testRemoveFault(paths []string) error {
	if testHookWrite.Load() == nil {
		return nil
	}
	for _, path := range paths {
		if err := testWriteFault(path); err != nil {
			return err
		}
	}
	return nil
}

// RemoveFilesUnderRoot removes indexed files below root as one workspace
// mutation. The runtime uses it before reconciling a changed stdlib root.
func (s *Server) RemoveFilesUnderRoot(root string) error {
	if root == "" {
		return nil
	}
	stored, err := s.store.ListFilePaths()
	if err != nil {
		return fmt.Errorf("%w: listing stored files: %v", ErrIncomplete, err)
	}
	prefix := filepath.Clean(root) + string(os.PathSeparator)
	paths := make([]string, 0)
	for _, path := range stored {
		if strings.HasPrefix(filepath.Clean(path), prefix) {
			paths = append(paths, path)
		}
	}
	return s.RemoveFiles(paths)
}

// watchGitHead polls .git/HEAD mtime and triggers reindex on branch switches.
func (s *Server) watchGitHead() {
	go func() {
		defer s.index.reporter.Recover("watch", "the Git HEAD poll")
		// In a linked worktree or a submodule, .git is a file that names the
		// git directory, and HEAD is there.
		gitDir, ok := parser.GitDir(s.projectRoot)
		if !ok {
			return
		}
		headPath := filepath.Join(gitDir, "HEAD")
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

// isOTPMismatch reports whether BEAM or mix output says that the Elixir install
// was compiled for a newer OTP than the one that runs.
func isOTPMismatch(stderr string) bool {
	return strings.Contains(stderr, "requires a more recent Erlang/OTP")
}

// === LSP Lifecycle ===

// coerceBool accepts a JSON bool or a JSON string ("true"/"false"/"1"/"0"…).
// Claude Code plugin template substitution (e.g. "${user_config.debug}") produces
// a string, not a bool, so we accept both forms.
func coerceBool(v interface{}) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		if x == "" {
			return false, false
		}
		b, err := strconv.ParseBool(x)
		if err != nil {
			return false, false
		}
		return b, true
	}
	return false, false
}

func (s *Server) Initialize(ctx context.Context, params *protocol.InitializeParams) (*protocol.InitializeResult, error) {
	initStarted := time.Now()
	// Note: unlike cmd/main.go, the LSP deliberately does NOT pass "mix.exs"
	// to store.FindProjectRoot. In a monorepo we want to anchor on
	// .dexter/dexter.db or .git so the whole repo is indexed, not the first
	// nested Mix app we encounter.
	var clientRoot string
	if len(params.WorkspaceFolders) > 0 {
		clientRoot = uriToPath(protocol.DocumentURI(params.WorkspaceFolders[0].URI))
	} else if params.RootURI != "" { //nolint:staticcheck // RootURI is deprecated but Neovim still sends it
		clientRoot = uriToPath(params.RootURI) //nolint:staticcheck
	}
	if !s.explicitRoot {
		if clientRoot != "" {
			s.projectRoot = store.FindProjectRoot(clientRoot)
		}
	} else if clientRoot != "" && s.workspaceEvents != nil {
		rel, err := filepath.Rel(s.projectRoot, clientRoot)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("editor workspace root %s does not use daemon root spelling %s", clientRoot, s.projectRoot)
		}
		resolvedProject, projectErr := filepath.EvalSymlinks(s.projectRoot)
		resolvedClient, clientErr := filepath.EvalSymlinks(clientRoot)
		if projectErr == nil && clientErr == nil {
			resolvedRel, relErr := filepath.Rel(resolvedProject, resolvedClient)
			if relErr != nil || filepath.Clean(resolvedRel) != filepath.Clean(rel) {
				return nil, fmt.Errorf("editor workspace root %s uses a symlink spelling that differs from daemon root %s", clientRoot, s.projectRoot)
			}
		}
	}

	var explicitStdlibPath string
	if opts, ok := params.InitializationOptions.(map[string]interface{}); ok {
		if v, ok := coerceBool(opts["followDelegates"]); ok {
			s.followDelegates = v
		}
		if v, ok := opts["stdlibPath"].(string); ok {
			explicitStdlibPath = v
		}
		if v, ok := coerceBool(opts["debug"]); ok {
			s.debug = v
		}
		if v, ok := opts["maxTransientDocuments"].(float64); ok {
			s.docs.SetMaxTransient(int(v))
		}
	}
	if os.Getenv("DEXTER_DEBUG") == "true" {
		s.debug = true
	}

	log.Printf("Initialize: projectRoot=%s debug=%v", s.projectRoot, s.debug)
	// An editor that connects is often one that the user restarted after a
	// fix, so BEAMs that failed to start are tried again.
	s.index.beamFailures.forget()

	// Detection runs version managers and login shells. Its result is kept by
	// the coordinator, so the editors after the first of a workspace, and the
	// daemon core that detected first, do not pay for it again.
	var root, mixFrom string
	if explicitStdlibPath != "" {
		inUse, granted := s.index.claimExplicitStdlib(explicitStdlibPath)
		if !granted {
			s.stdlibConflict = stdlibConflictMessage(explicitStdlibPath, inUse)
			log.Printf("Warning: %s", strings.TrimPrefix(s.stdlibConflict, "Dexter: "))
		}
		root = inUse
		if s.workspaceEvents != nil {
			if err := s.workspaceEvents.SetStdlibRoot(ctx, root); err != nil {
				return nil, err
			}
		} else {
			s.SetStdlibRoot(root)
		}
		// mix of the Elixir install that this editor asked for: formatting
		// is local to the session.
		mixFrom = explicitStdlibPath
	} else {
		root = s.index.DetectStdlib(s.store, s.projectRoot)
		switch {
		case root == "":
		case s.workspaceEvents == nil:
			s.SetStdlibRoot(root)
		case s.StdlibRoot() == "":
			// The workspace found no standard library when it started, and
			// this detection, made later, found one: the user installed it.
			if err := s.workspaceEvents.SetStdlibRoot(ctx, root); err != nil {
				return nil, err
			}
		}
		mixFrom = root
	}
	if root != "" {
		log.Printf("Elixir stdlib at: %s", root)
	}
	s.ReportStdlib()
	s.mixBin = s.index.findMix(mixFrom, initStarted)
	if s.mixBin != "" {
		log.Printf("Mix binary at: %s", s.mixBin)
	}
	s.mixMissing = s.mixBin == ""
	if s.mixMissing {
		log.Printf("Warning: %s", strings.TrimPrefix(mixMissingMessage, "Dexter: "))
	}

	if !s.initialized && s.manageWorkspace {
		s.initialized = true
		s.backgroundReindex()
		s.watchGitHead()
	}

	if params.Capabilities.Window != nil && params.Capabilities.Window.ShowDocument != nil {
		s.showDocumentSupported = params.Capabilities.Window.ShowDocument.Support
	}
	s.workDoneProgress = params.Capabilities.Window != nil && params.Capabilities.Window.WorkDoneProgress
	// Resource operations only exist inside documentChanges, so advertising
	// them implies documentChanges support. Neovim, for one, lists
	// resourceOperations without setting the separate documentChanges flag.
	if ws := params.Capabilities.Workspace; ws != nil && ws.WorkspaceEdit != nil {
		for _, op := range ws.WorkspaceEdit.ResourceOperations {
			if op == "rename" {
				s.renameFileOpsSupported = true
				break
			}
		}
	}
	if params.Capabilities.TextDocument != nil && params.Capabilities.TextDocument.Completion != nil &&
		params.Capabilities.TextDocument.Completion.CompletionItem != nil {
		s.snippetSupport = params.Capabilities.TextDocument.Completion.CompletionItem.SnippetSupport
	}

	result := &protocol.InitializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: protocol.TextDocumentSyncOptions{
				OpenClose:         true,
				Change:            protocol.TextDocumentSyncKindFull,
				WillSaveWaitUntil: false,
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
	// Workspace reports start now, not in Initialize: before the initialize
	// response, a server must not send requests such as
	// window/workDoneProgress/create. The reporter replays what started
	// earlier, so nothing is lost.
	if s.client != nil {
		detach := s.index.reporter.Attach(s.client, s.workDoneProgress)
		if previous, ok := s.detachReporter.Swap(detach).(func()); ok {
			previous()
		}
		if s.mixMissing {
			notify.Show(ctx, s.client, notify.Warning, mixMissingMessage)
		}
		if s.stdlibConflict != "" {
			notify.Show(ctx, s.client, notify.Warning, s.stdlibConflict)
		}
	}

	// Registered for daemon-backed sessions too. The workspace's native watcher
	// deliberately skips deps/ — one watch per dependency directory would cost
	// thousands of descriptors — while the editor's glob covers it, along with
	// path dependencies. Events from both sources coalesce by path in the one
	// mutation queue, so the overlap costs a map insert, not a second reindex.
	if s.client != nil {
		go func() {
			defer s.index.reporter.Recover("request", "the file watcher registration")
			// An editor that closes the session before it answers must not
			// keep this goroutine, and the session it holds, for ever.
			ctx, cancel := s.clientCallContext()
			defer cancel()
			if err := s.client.RegisterCapability(ctx, &protocol.RegistrationParams{
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
	s.CloseSession()
	return nil
}

// CloseSession releases resources owned by one LSP client. Daemon transports
// call it even when a client disconnects without the protocol shutdown request.
func (s *Server) CloseSession() {
	s.closeOnce.Do(func() {
		// Lazy starts check this, so that work that outlives the session
		// (a request that ignored its context, a warm-up goroutine) does not
		// start a BEAM or other work for a session that is gone.
		s.sessionClosed.Store(true)
		s.detachFromReporter()
		s.closeBeams()
		s.docs.CloseAll()
	})
}

func (s *Server) Exit(ctx context.Context) error {
	if !s.manageWorkspace {
		if s.conn != nil {
			return s.conn.Close()
		}
		return nil
	}
	os.Exit(0)
	return nil
}

// === Document Sync ===

func (s *Server) DidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error {
	docURI := string(params.TextDocument.URI)
	s.index.renameMu.RLock() // see IndexCoordinator.renameMu
	s.docs.Set(docURI, params.TextDocument.Text)
	s.index.renameMu.RUnlock()
	path := uriToPath(params.TextDocument.URI)

	// Eagerly start the persistent BEAM process so the first format is instant.
	// Skip deps and stdlib files — we don't format those.
	if path != "" && isFormattableFile(path) && s.isProjectFile(path) && !s.isDepsFile(path) {
		buildRoot := s.findBuildRoot(filepath.Dir(path))
		text := params.TextDocument.Text
		go func(path, buildRoot, text string) {
			defer s.index.reporter.Recover("request", "the warm-up for an opened document")
			if s.sessionClosed.Load() {
				return // the session closed before the warm-up started
			}
			s.prewarmGeneratedFunctions(path, text)
			_ = s.getBeamProcess(context.Background(), buildRoot)
			s.startErlangModuleLoad(path)
		}(path, buildRoot, text)
	}

	return nil
}

func (s *Server) DidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	if len(params.ContentChanges) > 0 {
		// Full sync mode — last change contains the full text
		text := params.ContentChanges[len(params.ContentChanges)-1].Text
		s.index.renameMu.RLock() // see IndexCoordinator.renameMu
		s.docs.SetChanged(string(params.TextDocument.URI), text)
		s.index.renameMu.RUnlock()
	}
	return nil
}

func (s *Server) DidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error {
	s.index.renameMu.RLock() // see IndexCoordinator.renameMu
	s.docs.Close(string(params.TextDocument.URI))
	s.index.renameMu.RUnlock()
	// A rename indexes the text of open buffers (see reindexAfterRename). If
	// the editor closes such a buffer without saving it, the disk still has
	// the old text, and the index must follow the disk again. One indexed
	// query on close; the reconcile runs only for such a file.
	if path := uriToPath(params.TextDocument.URI); path != "" && parser.IsElixirFile(path) {
		if mtime, ok := s.store.GetFileMtime(path); ok && mtime == store.BufferMtime {
			s.reconcileFromDisk(path)
		}
	}
	return nil
}

// reconcileFromDisk indexes path again from the disk, or removes it from the
// index when it is gone.
func (s *Server) reconcileFromDisk(path string) {
	if s.workspaceEvents != nil {
		// The runtime stats the path and removes a file that is gone.
		s.workspaceEvents.ReconcileFile(path)
		return
	}
	go func() {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			s.RemoveFile(path)
			return
		}
		if err := s.ReconcileFile(path); err != nil {
			log.Printf("Reconcile %s after close: %v", path, err)
		}
	}()
}

func (s *Server) restartBeamForFormatterConfig(path string) {
	buildRoot := s.findBuildRoot(filepath.Dir(path))
	var bp *beamProcess
	s.beamMu.Lock()
	if existing, ok := s.beams[buildRoot]; ok {
		delete(s.beams, buildRoot)
		bp = existing
	}
	s.beamMu.Unlock()
	if bp != nil {
		bp.Close()
		log.Printf("BEAM: restarting for %s (.formatter.exs changed)", buildRoot)
	}
}

func (s *Server) DidSave(ctx context.Context, params *protocol.DidSaveTextDocumentParams) error {
	s.docs.MarkSaved(string(params.TextDocument.URI))
	path := uriToPath(params.TextDocument.URI)
	if path == "" {
		return nil
	}

	// Restart the BEAM process when .formatter.exs changes so the new
	// config is picked up on the next format request.
	if filepath.Base(path) == ".formatter.exs" {
		s.restartBeamForFormatterConfig(path)
		return nil
	}

	if !parser.IsElixirFile(path) {
		return nil
	}
	// After a rename that changed only the case (`mv Foo.ex foo.ex`), an
	// editor can still save its buffer under the old spelling, which a stat
	// finds. Indexed under that spelling, the file would have two rows: the
	// old spelling is removed, and the file is indexed under the one on disk.
	// A save is rare, so it pays the open and fcntl that a watcher event
	// skips.
	if s.workspaceEvents != nil {
		s.workspaceEvents.ReconcileSavedFile(path)
		return nil
	}
	// ReconcileFile waits behind a reconciliation pass in flight. With only
	// the write coordinator's read lock, the save would race the pass's
	// batches for SQLite's write lock and could fail after busy_timeout, and
	// then stay unindexed until the next pass. A failed write is logged, and
	// the file keeps its old mtime for the next pass.
	go func() {
		defer s.index.reporter.Recover("index", "a single-file index update")
		if disk, renamed := pathcase.SpellingBelow(s.projectRoot, path); renamed {
			s.RemoveFile(path)
			path = disk
		}
		_ = s.ReconcileFile(path)
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

	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)

	if lineNum >= len(lines) {
		return nil, nil
	}

	// Get cached tokens for efficient multi-query operations
	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}

	// Check for @module_attribute reference first
	if attrName := tf.ModuleAttributeAtCursor(lineNum, col); attrName != "" {
		if line, found := FindModuleAttributeDefinition(text, attrName); found {
			return []protocol.Location{{
				URI:   params.TextDocument.URI,
				Range: lineRange(line - 1),
			}}, nil
		}
		return nil, nil
	}

	exprCtx := tf.ExpressionAtCursor(lineNum, col)
	if exprCtx.Empty() {
		return nil, nil
	}

	// Erlang module atom (e.g. :code.all_loaded) — resolve via BEAM process.
	// Check before ExtractModuleAndFunction which doesn't handle atom-prefixed modules.
	if strings.HasPrefix(exprCtx.ModuleRef, ":") {
		erlModule := exprCtx.ModuleRef[1:] // strip the : prefix
		s.debugf("Definition: Erlang module %q function=%q", erlModule, exprCtx.FunctionName)
		return s.erlangDefinition(ctx, uriToPath(params.TextDocument.URI), erlModule, exprCtx.FunctionName)
	}

	expr := tf.ResolveModuleExpr(exprCtx.Expr(), lineNum)
	moduleRef, functionName := ExtractModuleAndFunction(expr)

	if moduleRef != "" {
		if aliasParent, inBlock := tf.ExtractAliasBlockParent(lineNum); inBlock {
			moduleRef = aliasParent + "." + moduleRef
		}
	}

	aliases := tf.ExtractAliasesInScope(lineNum)
	s.mergeAliasesFromUseTokenized(tf, aliases)
	s.debugf("Definition: expr=%q module=%q function=%q", expr, moduleRef, functionName)

	// Bare identifier — check variable first (cheap tree-sitter lookup), then functions
	if moduleRef == "" {
		if functionName == "" {
			return nil, nil
		}

		// Variable go-to-definition via tree-sitter.
		// The first occurrence in scope is the definition (pattern/assignment).
		if tree, src, release, ok := s.docs.GetTree(docURI); ok {
			defer release()
			if occs := treesitter.FindVariableOccurrencesWithTree(tree.RootNode(), src, uint(lineNum), uint(col)); len(occs) > 0 {
				s.debugf("Definition: returning variable definition at line %d", occs[0].Line)
				return []protocol.Location{{
					URI:   params.TextDocument.URI,
					Range: lineRange(int(occs[0].Line)),
				}}, nil
			}
		}

		currentModule := tf.FirstDefmodule()
		fullModule := s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, tf, lineNum, functionName, aliases)
		s.debugf("Definition: resolved bare %q -> %q", functionName, fullModule)
		if fullModule == "" {
			currentModule := s.store.LookupEnclosingModule(uriToPath(protocol.DocumentURI(docURI)), lineNum+1)
			if provider, functions, found := s.generatedSymbolInScope(currentModule, func() []string {
				return s.enclosingBlockPath(docURI, lineNum, col)
			}, functionName); found {
				if results, precise := s.generatedDefinitionResultsFor(provider.module, provider.beamPath, functions); len(results) > 0 {
					s.debugf("Definition: generated bare %q provider=%s precise=%t", functionName, provider.module, precise)
					return storeResultsToLocations(results), nil
				}
			}
			s.debugf("Definition: could not resolve bare function %q", functionName)
			return nil, nil
		}

		// Current module — return buffer location directly (works before indexing).
		// In a typespec the bare name is the type, everywhere else the function.
		if fullModule == currentModule {
			find := tf.FindFunctionDefinition
			if tf.InTypespec(lineNum) {
				find = tf.FindTypeDefinition
			}
			if line, found := find(functionName); found {
				return []protocol.Location{{
					URI:   params.TextDocument.URI,
					Range: lineRange(line - 1),
				}}, nil
			}
		}

		kind := NameKindCallable
		if tf.InTypespec(lineNum) {
			kind = NameKindType
		}
		results, err := s.LookupName(fullModule, functionName, NameLookupOptions{
			Kind:            kind,
			FollowDelegates: s.followDelegates,
			External:        fullModule != extractEnclosingModuleFromTokens(tf.source, tf.tokens, lineNum),
		})
		if err == nil && len(results) > 0 {
			s.debugf("Definition: found %d semantic result(s) for %s.%s", len(results), fullModule, functionName)
			return nameLocationsToProtocol(results), nil
		}

		// Fallback for use-chain inline defs (not stored as module definitions)
		if results := s.lookupThroughUse(text, functionName, aliases); len(results) > 0 {
			s.debugf("Definition: found %d result(s) via current file use chain for %s", len(results), functionName)
			return storeResultsToLocations(byKindForContext(tf, lineNum, results)), nil
		}

		currentModule = s.store.LookupEnclosingModule(uriToPath(protocol.DocumentURI(docURI)), lineNum+1)
		if provider, functions, found := s.generatedSymbolInScope(currentModule, func() []string {
			return s.enclosingBlockPath(docURI, lineNum, col)
		}, functionName); found {
			if results, precise := s.generatedDefinitionResultsFor(provider.module, provider.beamPath, functions); len(results) > 0 {
				s.debugf("Definition: generated fallback for bare %q provider=%s precise=%t", functionName, provider.module, precise)
				return storeResultsToLocations(results), nil
			}
		}

		s.debugf("Definition: no result found for bare function %q in module %q", functionName, fullModule)

		return nil, nil
	}

	// Module.function call — resolve aliases (including implicit nested-module aliases)
	fullModule := s.resolveModuleWithNesting(moduleRef, aliases, uriToPath(params.TextDocument.URI), lineNum)
	s.debugf("Definition: qualified call resolved %q -> %q", moduleRef, fullModule)

	if functionName != "" {
		kind := NameKindCallable
		if tf.InTypespec(lineNum) {
			kind = NameKindType
		}
		results, err := s.LookupName(fullModule, functionName, NameLookupOptions{
			Kind:             kind,
			FollowDelegates:  s.followDelegates,
			External:         fullModule != extractEnclosingModuleFromTokens(tf.source, tf.tokens, lineNum),
			FallbackToModule: true,
		})
		if err != nil {
			return nil, nil
		}
		return nameLocationsToProtocol(results), nil
	}

	results, err := s.LookupName(fullModule, "", NameLookupOptions{})
	if err != nil {
		s.debugf("Definition: module fallback lookup failed for %s: %v", fullModule, err)
		return nil, nil
	}
	if len(results) == 0 {
		s.debugf("Definition: module fallback found no source for %s", fullModule)
		return nil, nil
	}
	s.debugf("Definition: module fallback for %s.%s -> %s:%d (%d result(s))", fullModule, functionName, results[0].FilePath, results[0].Line, len(results))
	return nameLocationsToProtocol(results), nil
}

func nameLocationsToProtocol(results []NameLocation) []protocol.Location {
	locations := make([]protocol.Location, 0, len(results))
	for _, result := range results {
		locations = append(locations, protocol.Location{
			URI:   uri.File(result.FilePath),
			Range: lineRange(result.Line - 1),
		})
	}
	return locations
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

// privateKinds are definitions that only the defining module can call.
var privateKinds = map[string]bool{"defp": true, "defmacrop": true, "defguardp": true}

// publicOnly applies the same strict visibility rule as
// store.LookupPublicFunction after a delegate lookup has followed the public
// defdelegate to its final definition.
func publicOnly(results []store.LookupResult) []store.LookupResult {
	public := results[:0]
	for _, r := range results {
		switch r.Kind {
		case "def", "defmacro", "defguard", "defdelegate", "defstruct", "defexception", "type", "opaque":
			public = append(public, r)
		}
	}
	return public
}

// filterOutPrivate drops private definitions from a lookup whose call site is
// outside the defining module. A module can export a public def and keep a
// private helper of the same name — Ecto.Schema does exactly this with
// `defmacro field/3` and `defp field/4` — and only the public one is reachable
// from a remote or imported call.
//
// If every result is private the results are returned unchanged: the call does
// not compile in Elixir, but a jump to the private helper is a better answer
// than none. Callers must apply this only when the call site's enclosing module
// differs from the module that was looked up.
func filterOutPrivate(results []store.LookupResult) []store.LookupResult {
	var public []store.LookupResult
	for _, r := range results {
		if !privateKinds[r.Kind] {
			public = append(public, r)
		}
	}
	if len(public) > 0 {
		return public
	}
	return results
}

// byKindForContext picks the definitions that match how the name is written:
// types inside a typespec, everything else outside one.
func byKindForContext(tf *TokenizedFile, lineNum int, results []store.LookupResult) []store.LookupResult {
	if tf.InTypespec(lineNum) {
		return filterToTypes(results)
	}
	return filterOutTypes(results)
}

// filterToTypes is the mirror of filterOutTypes, for a name written inside a
// typespec: there `Mod.schema()` names the type, so the type definitions are
// the answer and a function of the same name is not. Results are returned
// unchanged when the module declares no such type.
func filterToTypes(results []store.LookupResult) []store.LookupResult {
	var types []store.LookupResult
	for _, r := range results {
		if typeKinds[r.Kind] {
			types = append(types, r)
		}
	}
	if len(types) > 0 {
		return types
	}
	return results
}

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

func (s *Server) erlangBuildRoot(filePath string) string {
	if filePath != "" {
		return s.findBuildRoot(filepath.Dir(filePath))
	}
	return s.findBuildRoot(s.projectRoot)
}

func erlangRuntimeKey(info *ErlangRuntimeInfo) string {
	if info == nil {
		return ""
	}
	return info.OTPRelease + "\x00" + info.CodeRootDir
}

func decodeErlangModuleNames(payload []byte) ([]string, error) {
	reader := bytes.NewReader(payload)
	var count uint16
	if err := binary.Read(reader, binary.BigEndian, &count); err != nil {
		return nil, fmt.Errorf("read module count: %w", err)
	}

	names := make([]string, 0, count)
	for i := 0; i < int(count); i++ {
		var nameLen uint16
		if err := binary.Read(reader, binary.BigEndian, &nameLen); err != nil {
			return nil, fmt.Errorf("read module name length: %w", err)
		}
		nameBuf := make([]byte, nameLen)
		if _, err := io.ReadFull(reader, nameBuf); err != nil {
			return nil, fmt.Errorf("read module name: %w", err)
		}
		names = append(names, string(nameBuf))
	}

	return names, nil
}

func (s *Server) clearErlangWarmup(runtimeKey string, readyCh chan struct{}) {
	var notifyCh chan struct{}

	s.erlangRuntimeMu.Lock()
	cache := s.erlangRuntimeCache[runtimeKey]
	if cache != nil && cache.readyCh == readyCh {
		cache.loading = false
		notifyCh = cache.readyCh
		cache.readyCh = nil
	}
	s.erlangRuntimeMu.Unlock()

	if notifyCh != nil {
		close(notifyCh)
	}
}

func (s *Server) completeErlangWarmup(buildRoot string, names []string) {
	moduleNames := make(map[string]bool, len(names))
	for _, name := range names {
		moduleNames[name] = true
	}

	var notifyCh chan struct{}
	s.erlangRuntimeMu.Lock()
	state := s.erlangBuildRoots[buildRoot]
	if state == nil || state.runtimeKey == "" {
		s.erlangRuntimeMu.Unlock()
		return
	}

	cache := s.erlangRuntimeCache[state.runtimeKey]
	if cache == nil {
		cache = &erlangRuntimeCache{exports: make(map[string][]ErlangExport)}
		s.erlangRuntimeCache[state.runtimeKey] = cache
	}
	if cache.exports == nil {
		cache.exports = make(map[string][]ErlangExport)
	}
	cache.moduleNames = moduleNames
	cache.loading = false
	notifyCh = cache.readyCh
	cache.readyCh = nil
	s.erlangRuntimeMu.Unlock()

	if notifyCh != nil {
		close(notifyCh)
	}
}

func (s *Server) failErlangWarmup(buildRoot string) {
	var notifyCh chan struct{}

	s.erlangRuntimeMu.Lock()
	state := s.erlangBuildRoots[buildRoot]
	if state != nil && state.runtimeKey != "" {
		if cache := s.erlangRuntimeCache[state.runtimeKey]; cache != nil {
			cache.loading = false
			notifyCh = cache.readyCh
			cache.readyCh = nil
		}
	}
	s.erlangRuntimeMu.Unlock()

	if notifyCh != nil {
		close(notifyCh)
	}
}

func (s *Server) handleBeamNotification(buildRoot string, notification beamNotification) {
	switch notification.op {
	case beamNotificationOTPModulesReady:
		names, err := decodeErlangModuleNames(notification.payload)
		if err != nil {
			log.Printf("failed to decode Erlang module notification: %v", err)
			s.failErlangWarmup(buildRoot)
			return
		}
		s.completeErlangWarmup(buildRoot, names)

	case beamNotificationOTPModulesFailed:
		if len(notification.payload) > 0 {
			log.Printf("failed to warm Erlang modules: %s", strings.TrimSpace(string(notification.payload)))
		} else {
			log.Printf("failed to warm Erlang modules")
		}
		s.failErlangWarmup(buildRoot)
	}
}

// erlangHover fetches documentation for an Erlang module/function via the
// BEAM process's CodeIntel service.
func (s *Server) erlangHover(ctx context.Context, filePath, module, function string) (*protocol.Hover, error) {
	buildRoot := s.erlangBuildRoot(filePath)
	bp := s.getBeamProcess(ctx, buildRoot)
	if bp == nil {
		return nil, nil
	}
	if err := bp.Ready(ctx); err != nil {
		return nil, nil
	}

	doc, err := bp.ErlangDocs(ctx, module, function, -1)
	if err != nil || doc == "" {
		return nil, nil
	}

	return &protocol.Hover{
		Contents: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: doc,
		},
	}, nil
}

// erlangDefinition resolves an Erlang module/function to its .erl source via
// the BEAM process's CodeIntel service.
func (s *Server) erlangDefinition(ctx context.Context, filePath, module, function string) ([]protocol.Location, error) {
	buildRoot := s.erlangBuildRoot(filePath)
	bp := s.getBeamProcess(ctx, buildRoot)
	if bp == nil {
		s.debugf("Definition: no BEAM process available for Erlang resolution")
		return nil, nil
	}
	if err := bp.Ready(ctx); err != nil {
		s.debugf("Definition: BEAM process not ready: %v", err)
		return nil, nil
	}

	result, err := bp.ErlangSource(ctx, module, function, -1)
	if err != nil {
		s.debugf("Definition: Erlang source lookup failed: %v", err)
		return nil, nil
	}

	line := result.Line
	if line > 0 {
		line-- // convert 1-based to 0-based for LSP
	}

	return []protocol.Location{{
		URI:   uri.File(result.File),
		Range: lineRange(line),
	}}, nil
}

// startErlangModuleLoad kicks off a background warmup for the current file's
// runtime. Build roots first resolve to a runtime fingerprint, then runtime
// caches (OTP module names + exports) are loaded once per runtime.
func (s *Server) startErlangModuleLoad(filePath string) {
	if s.sessionClosed.Load() {
		return
	}
	buildRoot := s.erlangBuildRoot(filePath)

	s.erlangRuntimeMu.Lock()
	state, ok := s.erlangBuildRoots[buildRoot]
	if !ok {
		state = &erlangBuildRootState{}
		s.erlangBuildRoots[buildRoot] = state
	}

	if state.runtimeKey != "" {
		runtimeKey := state.runtimeKey
		cache := s.erlangRuntimeCache[runtimeKey]
		switch {
		case cache != nil && cache.moduleNames != nil:
			s.erlangRuntimeMu.Unlock()
			return
		case cache != nil && cache.loading:
			s.erlangRuntimeMu.Unlock()
			return
		default:
			if cache == nil {
				cache = &erlangRuntimeCache{exports: make(map[string][]ErlangExport)}
				s.erlangRuntimeCache[runtimeKey] = cache
			}
			if cache.exports == nil {
				cache.exports = make(map[string][]ErlangExport)
			}
			cache.loading = true
			cache.readyCh = make(chan struct{})
			readyCh := cache.readyCh
			s.erlangRuntimeMu.Unlock()
			go func() {
				defer s.index.reporter.Recover("beam", "the Erlang module warm-up", func(any) { s.clearErlangWarmup(runtimeKey, readyCh) })
				s.loadErlangRuntimeCache(buildRoot, runtimeKey, readyCh)
			}()
			return
		}
	}

	if state.loading {
		s.erlangRuntimeMu.Unlock()
		return
	}

	state.loading = true
	s.erlangRuntimeMu.Unlock()
	go func() {
		defer s.index.reporter.Recover("beam", "the Erlang runtime detection", func(any) {
			s.erlangRuntimeMu.Lock()
			if state := s.erlangBuildRoots[buildRoot]; state != nil {
				state.loading = false
			}
			s.erlangRuntimeMu.Unlock()
		})
		s.resolveErlangRuntime(buildRoot)
	}()
}

func (s *Server) resolveErlangRuntime(buildRoot string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bp := s.getBeamProcess(ctx, buildRoot)
	if bp == nil {
		s.erlangRuntimeMu.Lock()
		if state := s.erlangBuildRoots[buildRoot]; state != nil {
			state.loading = false
		}
		s.erlangRuntimeMu.Unlock()
		return
	}
	if err := bp.Ready(ctx); err != nil {
		s.erlangRuntimeMu.Lock()
		if state := s.erlangBuildRoots[buildRoot]; state != nil {
			state.loading = false
		}
		s.erlangRuntimeMu.Unlock()
		return
	}

	info, err := bp.ErlangRuntimeInfo(ctx)
	if err != nil {
		s.erlangRuntimeMu.Lock()
		if state := s.erlangBuildRoots[buildRoot]; state != nil {
			state.loading = false
		}
		s.erlangRuntimeMu.Unlock()
		return
	}

	runtimeKey := erlangRuntimeKey(info)
	if runtimeKey == "" {
		s.erlangRuntimeMu.Lock()
		if state := s.erlangBuildRoots[buildRoot]; state != nil {
			state.loading = false
		}
		s.erlangRuntimeMu.Unlock()
		return
	}

	startLoad := false
	var readyCh chan struct{}
	s.erlangRuntimeMu.Lock()
	state := s.erlangBuildRoots[buildRoot]
	if state == nil {
		state = &erlangBuildRootState{}
		s.erlangBuildRoots[buildRoot] = state
	}
	state.runtimeKey = runtimeKey
	state.loading = false

	cache := s.erlangRuntimeCache[runtimeKey]
	if cache == nil {
		cache = &erlangRuntimeCache{
			otpRelease:  info.OTPRelease,
			codeRootDir: info.CodeRootDir,
			exports:     make(map[string][]ErlangExport),
		}
		s.erlangRuntimeCache[runtimeKey] = cache
	}
	if cache.exports == nil {
		cache.exports = make(map[string][]ErlangExport)
	}
	if cache.otpRelease == "" {
		cache.otpRelease = info.OTPRelease
	}
	if cache.codeRootDir == "" {
		cache.codeRootDir = info.CodeRootDir
	}
	if cache.moduleNames == nil && !cache.loading {
		cache.loading = true
		cache.readyCh = make(chan struct{})
		readyCh = cache.readyCh
		startLoad = true
	}
	s.erlangRuntimeMu.Unlock()

	if startLoad {
		go func() {
			defer s.index.reporter.Recover("beam", "the Erlang module warm-up", func(any) { s.clearErlangWarmup(runtimeKey, readyCh) })
			s.loadErlangRuntimeCache(buildRoot, runtimeKey, readyCh)
		}()
	}
}

func (s *Server) loadErlangRuntimeCache(buildRoot, runtimeKey string, readyCh chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bp := s.getBeamProcess(ctx, buildRoot)
	if bp == nil {
		s.clearErlangWarmup(runtimeKey, readyCh)
		return
	}
	if err := bp.Ready(ctx); err != nil {
		s.clearErlangWarmup(runtimeKey, readyCh)
		return
	}

	if err := bp.WarmOTPModuleNames(ctx); err != nil {
		log.Printf("failed to start OTP module warmup: %v", err)
		s.clearErlangWarmup(runtimeKey, readyCh)
		return
	}

	select {
	case <-readyCh:
		return
	case <-ctx.Done():
		s.clearErlangWarmup(runtimeKey, readyCh)
		return
	}
}

func (s *Server) erlangModuleNamesForFile(filePath string) map[string]bool {
	buildRoot := s.erlangBuildRoot(filePath)

	s.erlangRuntimeMu.Lock()
	defer s.erlangRuntimeMu.Unlock()

	state := s.erlangBuildRoots[buildRoot]
	if state == nil || state.runtimeKey == "" {
		return nil
	}
	cache := s.erlangRuntimeCache[state.runtimeKey]
	if cache == nil {
		return nil
	}
	return cache.moduleNames
}

func (s *Server) erlangModulesAvailable(filePath string) bool {
	moduleNames := s.erlangModuleNamesForFile(filePath)
	return moduleNames != nil
}

// getErlangExports returns the cached exports for an Erlang module, fetching
// from the BEAM on first access. Export caches are shared across build roots
// that resolve to the same runtime fingerprint.
func (s *Server) getErlangExports(ctx context.Context, filePath, module string) []ErlangExport {
	buildRoot := s.erlangBuildRoot(filePath)

	s.erlangRuntimeMu.Lock()
	state := s.erlangBuildRoots[buildRoot]
	if state == nil || state.runtimeKey == "" {
		s.erlangRuntimeMu.Unlock()
		s.startErlangModuleLoad(filePath)
		return nil
	}

	runtimeKey := state.runtimeKey
	cache := s.erlangRuntimeCache[runtimeKey]
	if cache == nil || cache.moduleNames == nil {
		s.erlangRuntimeMu.Unlock()
		s.startErlangModuleLoad(filePath)
		return nil
	}
	if exports, ok := cache.exports[module]; ok {
		s.erlangRuntimeMu.Unlock()
		return exports
	}
	s.erlangRuntimeMu.Unlock()

	bp := s.getBeamProcess(ctx, buildRoot)
	if bp == nil {
		return nil
	}
	if err := bp.Ready(ctx); err != nil {
		return nil
	}

	exports, err := bp.ErlangExports(ctx, module)
	if err != nil {
		return nil
	}

	s.erlangRuntimeMu.Lock()
	cache = s.erlangRuntimeCache[runtimeKey]
	if cache != nil {
		if cache.exports == nil {
			cache.exports = make(map[string][]ErlangExport)
		}
		cache.exports[module] = exports
	}
	s.erlangRuntimeMu.Unlock()
	return exports
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

// uriToPath returns the clean file path that a file URI names, or "" for a
// URI that is not a file URI. An editor sends such URIs too: VS Code sends
// `untitled:` buffers to a server whose document selector has no scheme. The
// handlers treat "" as a document with no file: it has no index rows and no
// rename, and buffer-only features still work. uri.URI.Filename panics on
// such a URI, and a panic in a handler goroutine ends the daemon and every
// editor's session with it, so nothing may call it on an editor's URI.
func uriToPath(u protocol.DocumentURI) string {
	_, path := docKeyPath(string(u))
	return path
}

// === No-op implementations for unused Server interface methods ===

func (s *Server) WorkDoneProgressCancel(ctx context.Context, params *protocol.WorkDoneProgressCancelParams) error {
	return nil
}
func (s *Server) LogTrace(ctx context.Context, params *protocol.LogTraceParams) error { return nil }
func (s *Server) SetTrace(ctx context.Context, params *protocol.SetTraceParams) error { return nil }
func (s *Server) CodeAction(ctx context.Context, params *protocol.CodeActionParams) ([]protocol.CodeAction, error) {
	docURI := string(params.TextDocument.URI)
	text, ok := s.docs.GetOrLoad(docURI)
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
	col := s.inCol(lines, lineNum, params.Range.Start.Character)
	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}

	exprCtx := tf.FullExpressionAtCursor(lineNum, col)
	if exprCtx.Empty() {
		return nil, nil
	}

	moduleRef := exprCtx.ModuleRef
	if moduleRef == "" {
		return nil, nil
	}

	aliases := tf.ExtractAliasesInScope(lineNum)
	s.mergeAliasesFromUseTokenized(tf, aliases)

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

			exprStart := exprCtx.ExprStart
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
					Start: s.outPos(lines, lineNum, exprStart),
					End:   s.outPos(lines, lineNum, exprStart+len(moduleRef)),
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
func (s *Server) Completion(ctx context.Context, params *protocol.CompletionParams) (result *protocol.CompletionList, err error) {
	docURI := string(params.TextDocument.URI)
	filePath := uriToPath(params.TextDocument.URI)
	if s.debug {
		started := time.Now()
		s.debugf("Completion request: uri=%s line=%d col=%d", docURI, params.Position.Line, params.Position.Character)
		defer func() {
			items := 0
			if result != nil {
				items = len(result.Items)
			}
			s.debugf("Completion: total %s (%d items)", time.Since(started).Round(time.Microsecond), items)
		}()
	}

	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)

	if lineNum >= len(lines) {
		return nil, nil
	}

	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}

	completionCtx := tf.CompletionContextAtCursor(lineNum, col)
	prefix, afterDot, prefixStartCol := completionCtx.Prefix, completionCtx.AfterDot, completionCtx.StartCol

	// Inside a multi-line alias block: complete child module segments under the parent.
	if aliasParent, inBlock := tf.ExtractAliasBlockParent(lineNum); inBlock {
		searchParent := aliasParent
		segmentPrefix := prefix
		labelPrefix := ""

		if afterDot && prefix != "" {
			searchParent = aliasParent + "." + prefix
			segmentPrefix = ""
			labelPrefix = prefix + "."
		} else if prefix != "" {
			if dotIdx := strings.LastIndexByte(prefix, '.'); dotIdx >= 0 {
				searchParent = aliasParent + "." + prefix[:dotIdx]
				segmentPrefix = prefix[dotIdx+1:]
				labelPrefix = prefix[:dotIdx+1]
			}
		}

		segments, err := s.store.SearchSubmoduleSegments(searchParent, segmentPrefix)
		if err != nil {
			return nil, nil
		}
		var items []protocol.CompletionItem
		for _, segment := range segments {
			items = append(items, protocol.CompletionItem{
				Label:  labelPrefix + segment,
				Kind:   protocol.CompletionItemKindModule,
				Detail: searchParent + "." + segment,
			})
		}
		if len(items) == 0 {
			return nil, nil
		}
		return &protocol.CompletionList{
			IsIncomplete: len(items) >= 100,
			Items:        items,
		}, nil
	}

	if prefix == "" && !afterDot {
		return nil, nil
	}

	// Range covering the already-typed prefix through cursor — used for
	// textEdit on module items so the editor replaces rather than appends.
	prefixRange := protocol.Range{
		Start: s.outPos(lines, lineNum, prefixStartCol),
		End:   s.outPos(lines, lineNum, col),
	}
	inPipe := IsPipeContext(lines[lineNum], prefixStartCol)

	// Erlang module/function completions (prefix starts with ":")
	if strings.HasPrefix(prefix, ":") {
		s.startErlangModuleLoad(filePath)
		if !s.erlangModulesAvailable(filePath) {
			return nil, nil
		}
		moduleNames := s.erlangModuleNamesForFile(filePath)
		if moduleNames == nil {
			return nil, nil
		}
		erlPrefix := prefix[1:] // strip leading colon
		var items []protocol.CompletionItem

		if afterDot || strings.Contains(erlPrefix, ".") {
			// :module.func — complete exported functions
			var erlModule, erlFuncPrefix string
			if dotIdx := strings.IndexByte(erlPrefix, '.'); dotIdx >= 0 {
				erlModule = erlPrefix[:dotIdx]
				erlFuncPrefix = erlPrefix[dotIdx+1:]
			} else {
				erlModule = erlPrefix
			}
			if moduleNames[erlModule] {
				for _, e := range s.getErlangExports(ctx, filePath, erlModule) {
					if strings.HasPrefix(e.Function, erlFuncPrefix) {
						item := protocol.CompletionItem{
							Label:  e.Function,
							Kind:   protocol.CompletionItemKindFunction,
							Detail: fmt.Sprintf(":%s.%s/%d", erlModule, e.Function, e.Arity),
						}
						applySnippet(&item, e.Function, e.Arity, e.Params, "", inPipe, s.snippetSupport)
						items = append(items, item)
					}
				}
			}
		} else {
			// :mod — complete module names
			for name := range moduleNames {
				if strings.HasPrefix(name, erlPrefix) {
					items = append(items, protocol.CompletionItem{
						Label:  ":" + name,
						Kind:   protocol.CompletionItemKindModule,
						Detail: "Erlang module",
						TextEdit: &protocol.TextEdit{
							Range:   prefixRange,
							NewText: ":" + name,
						},
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

	moduleRef, funcPrefix := ExtractModuleAndFunction(prefix)

	// "Module.func." or "variable." — dot after a function call result or
	// map/struct field access. We have no type info to complete the result.
	if afterDot && (funcPrefix != "" || moduleRef == "") {
		return nil, nil
	}

	var items []protocol.CompletionItem

	if moduleRef != "" && (afterDot || funcPrefix != "") {
		aliases := tf.ExtractAliases()
		s.mergeAliasesFromUseTokenized(tf, aliases)
		resolved := resolveModule(moduleRef, aliases)
		results, err := s.store.ListModuleFunctions(resolved, true)
		if err != nil {
			return nil, nil
		}
		seen := make(map[string]bool, len(results))
		for _, r := range results {
			if funcPrefix != "" && !strings.HasPrefix(r.Function, funcPrefix) {
				continue
			}
			seen[funcKey(r.Function, r.Arity)] = true
			item := protocol.CompletionItem{
				Label:  r.Function,
				Kind:   kindToCompletionItemKind(r.Kind),
				Detail: r.Kind,
				Data: map[string]interface{}{
					"filePath": r.FilePath,
					"line":     r.Line,
				},
			}
			applySnippet(&item, r.Function, r.Arity, r.Params, r.Kind, inPipe, s.snippetSupport)
			items = append(items, item)
		}

		s.addGeneratedFunctionCompletions(resolved, "", funcPrefix, seen, &items, inPipe, s.snippetSupport)
		// What `use` injects, from source, when no current compiled module
		// lists it: before the first compile, or after an edit since.
		if !s.hasCurrentBeam(resolved) {
			s.addInjectedFunctionCompletions(resolved, funcPrefix, seen, &items, inPipe)
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
		aliases := tf.ExtractAliases()
		s.mergeAliasesFromUseTokenized(tf, aliases)
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
		aliases := tf.ExtractAliases()
		s.mergeAliasesFromUseTokenized(tf, aliases)

		for _, bf := range tf.FindBufferFunctions() {
			key := funcKey(bf.Name, bf.Arity)
			if strings.HasPrefix(bf.Name, funcPrefix) && !seen[key] {
				seen[key] = true
				item := protocol.CompletionItem{
					Label:  bf.Name,
					Kind:   kindToCompletionItemKind(bf.Kind),
					Detail: bf.Kind,
				}
				applySnippet(&item, bf.Name, bf.Arity, bf.Params, bf.Kind, inPipe, s.snippetSupport)
				items = append(items, item)
			}
		}

		imports := tf.ExtractImports()
		imports = append(imports, "Kernel")
		for _, mod := range imports {
			mod = resolveModule(mod, aliases)
			results, err := s.store.ListModuleFunctions(mod, true)
			if err != nil {
				continue
			}
			for _, r := range results {
				if s.snippetSupport && elixirFormSnippets[r.Function] != "" {
					continue
				}
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
					applySnippet(&item, r.Function, r.Arity, r.Params, r.Kind, inPipe, s.snippetSupport)
					items = append(items, item)
				}
			}
			s.addGeneratedFunctionCompletions(mod, "", funcPrefix, seen, &items, inPipe, s.snippetSupport)
		}
		if currentModule := s.store.LookupEnclosingModule(filePath, lineNum+1); currentModule != "" {
			s.addGeneratedFunctionCompletions(currentModule, "", funcPrefix, seen, &items, inPipe, s.snippetSupport)
			// DSL macros are injected by provider modules that only the compiled
			// attributes name, so static use-chain resolution never reaches them.
			// Which ones are in scope depends on the do block the cursor sits in;
			// the walk is deferred because most modules have no DSL extensions.
			for _, provider := range s.dslProvidersInScope(currentModule, func() []string {
				return s.enclosingBlockPath(docURI, lineNum, col)
			}) {
				s.addGeneratedFunctionCompletions(provider.module, provider.beamPath, funcPrefix, seen, &items, inPipe, s.snippetSupport)
			}
		}

		// Check use-injected imports and inline defs (including transitive use chains)
		visitedCompletion := make(map[string]bool)
		useCalls := tf.ExtractUsesWithOpts(aliases)
		for i := len(useCalls) - 1; i >= 0; i-- {
			useCall := useCalls[i]
			s.addCompletionsFromUsingFor(useCall.Module, useCall.dispatchAtom(), useCall.Opts, funcPrefix, seen, &items, visitedCompletion, inPipe, s.snippetSupport)
		}

		// Variables in scope via tree-sitter
		var varsInScope []string
		if tree, src, release, ok := s.docs.GetTree(docURI); ok {
			defer release()
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

		if s.snippetSupport {
			for name, snippet := range elixirFormSnippets {
				if strings.HasPrefix(name, funcPrefix) && !seen[name] {
					seen[name] = true
					items = append(items, protocol.CompletionItem{
						Label:            name,
						Kind:             protocol.CompletionItemKindKeyword,
						Detail:           "special form",
						InsertText:       snippet,
						InsertTextFormat: protocol.InsertTextFormatSnippet,
					})
				}
			}
		}
		// Plain keyword completions (e.g., "do", "end"). These don't
		// expand to snippets; they just insert the keyword itself so
		// that pressing Enter doesn't replace the keyword with a
		// VS Code word-based suggestion.
		for _, kw := range elixirKeywords {
			if strings.HasPrefix(kw, funcPrefix) && !seen[kw] {
				seen[kw] = true
				items = append(items, protocol.CompletionItem{
					Label:     kw,
					Kind:      protocol.CompletionItemKindKeyword,
					Detail:    "keyword",
					Preselect: true,
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
// skip the store query. knownPath must be the choice of usingSourceFile from a
// fresh read of the index (uniqueUsingModules makes it for a list), so an
// entry for another file is replaced, and two callers never replace the entry
// of each other.
//
// An entry is used while its file is unchanged and the index still gives that
// file for the module. The file is checked with one stat; the index with one
// query, only after a write to the index since the last check. Otherwise the
// entry is dropped and the module is looked up again: the module can have
// moved to another file (git mv), and an entry that kept the old path would
// make every use chain through it resolve to nothing for the life of the
// daemon. A file that does not define the module is never cached, so a
// request between a change and its reindex cannot keep another module's body.
func (s *Server) cachedUsingWithPath(moduleName, knownPath string) *usingCacheEntry {
	// Recorded before any read of the index; see Store.WriteGeneration.
	gen := s.store.WriteGeneration()
	if knownPath != "" {
		knownPath = filepath.Clean(knownPath)
	}
	if entry, ok := s.usingCache.get(moduleName); ok {
		info, err := os.Stat(entry.filePath)
		if err == nil && info.ModTime().UnixNano() == entry.mtime {
			switch {
			case knownPath == entry.filePath || (knownPath == "" && entry.checkedGen.Load() == gen):
				entry.checkedGen.Store(gen)
				return entry
			case knownPath == "":
				indexed := s.usingSourceFile(moduleName)
				if indexed == entry.filePath {
					entry.checkedGen.Store(gen)
					return entry
				}
				knownPath = indexed
			}
		}
		s.usingCache.drop(moduleName, entry)
	}

	filePath := knownPath
	if filePath == "" {
		filePath = s.usingSourceFile(moduleName)
	}
	if filePath == "" {
		return nil
	}
	newEntry := s.parseUsingFile(filePath, moduleName)
	if newEntry == nil {
		return nil
	}
	newEntry.checkedGen.Store(gen)
	s.usingCache.put(moduleName, newEntry)
	return newEntry
}

// usingSourceFile picks the file whose __using__ body a module injects. When
// two files define the module (a copy in a test support directory, two
// releases of one library), the choice must not depend on which request or
// warm-up came first, or on the row order of the index: it is the file that
// defines __using__, and among several, the first path in byte order. The
// warm-up makes the same choice. A module without an indexed __using__ (an
// ExUnit.CaseTemplate `using` block) falls back to its module definitions with
// the same rule.
func (s *Server) usingSourceFile(moduleName string) string {
	if results, err := s.store.LookupFunction(moduleName, "__using__"); err == nil && len(results) > 0 {
		return firstPath(results)
	}
	if results, err := s.store.LookupModule(moduleName); err == nil && len(results) > 0 {
		return firstPath(results)
	}
	return ""
}

// firstPath returns the first file path of results in byte order: a
// deterministic choice when a module is defined in more than one file.
func firstPath(results []store.LookupResult) string {
	path := results[0].FilePath
	for _, r := range results[1:] {
		if r.FilePath < path {
			path = r.FilePath
		}
	}
	return filepath.Clean(path)
}

func (s *Server) parseUsingFile(filePath, moduleName string) *usingCacheEntry {
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
	text := string(fileData)
	// The file must still define the module: the index can be behind the
	// disk, and a file that now holds another module must not give its body.
	// The common one-module-per-file case avoids an extra tokenization.
	switch strings.Count(text, "defmodule") {
	case 0:
		return nil
	case 1:
		if !definesOnlyModule(text, moduleName) {
			return nil
		}
	default:
		scoped, ok := sourceForModule(text, moduleName)
		if !ok {
			return nil
		}
		text = scoped
	}
	imported, inlineDefs, transUses, transCalls, optBindings, aliases, derives, branchUses := parseUsingBodyDetailed(text)
	return &usingCacheEntry{
		mtime:    info.ModTime().UnixNano(),
		filePath: filePath,
		usingBody: usingBody{
			imports:     imported,
			inlineDefs:  inlineDefs,
			transUses:   transUses,
			transCalls:  transCalls,
			optBindings: optBindings,
			aliases:     aliases,
			derives:     derives,
			branchUses:  branchUses,
		},
		dispatch: parseDispatchBodies(text),
	}
}

var singleDefmodulePattern = regexp.MustCompile(`\bdefmodule\s*\(?\s*([A-Za-z_][A-Za-z0-9_.]*)`)

// definesOnlyModule reports whether text, which holds one defmodule, defines
// moduleName.
func definesOnlyModule(text, moduleName string) bool {
	m := singleDefmodulePattern.FindStringSubmatch(text)
	return m != nil && strings.TrimPrefix(m[1], "Elixir.") == moduleName
}

// lookupThroughUseOf looks up functionName through the `use` declarations in
// fullModule's source file. This handles qualified calls like M.func() where
// func is not defined directly in M but is injected by a macro M uses.
func (s *Server) lookupThroughUseOf(fullModule, functionName string) []store.LookupResult {
	return s.lookupThroughUseOfWithFollow(fullModule, functionName, s.followDelegates)
}

func (s *Server) lookupThroughUseOfWithFollow(fullModule, functionName string, followDelegates bool) []store.LookupResult {
	modResults, err := s.store.LookupModule(fullModule)
	if err != nil || len(modResults) == 0 {
		return nil
	}
	fileText, _, ok := s.readFileText(modResults[0].FilePath)
	if !ok {
		return nil
	}
	// Only the module's own uses: one in a __using__ quote injects into the
	// caller, not into this module.
	return s.lookupThroughUseCalls(ExtractModuleUsesWithOpts(fileText, ExtractAliases(fileText)), functionName, followDelegates)
}

// lookupThroughUse searches for functionName in definitions injected by `use`
// declarations. Inline defs (defined directly in the quote do block) take
// priority over imported ones. Later `use` declarations shadow earlier ones.
// Transitive use chains (use inside __using__ body) are followed recursively.
func (s *Server) lookupThroughUse(text, functionName string, aliases map[string]string) []store.LookupResult {
	return s.lookupThroughUseWithFollow(text, functionName, aliases, s.followDelegates)
}

func (s *Server) lookupThroughUseWithFollow(text, functionName string, aliases map[string]string, followDelegates bool) []store.LookupResult {
	return s.lookupThroughUseCalls(ExtractUsesWithOpts(text, aliases), functionName, followDelegates)
}

func (s *Server) lookupThroughUseCalls(useCalls []UseCall, functionName string, followDelegates bool) []store.LookupResult {
	visited := make(map[string]bool)

	for i := len(useCalls) - 1; i >= 0; i-- {
		if result := s.lookupInUsingEntryForWithFollow(useCalls[i].Module, functionName, useCalls[i].dispatchAtom(), useCalls[i].Opts, visited, followDelegates); result != nil {
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
	return s.lookupInUsingEntryForWithFollow(moduleName, functionName, "", consumerOpts, visited, s.followDelegates)
}

// bodyFor picks the injected body for a `use` call. An ordinary __using__ has a
// single body. An atom-dispatch entrypoint has one per target, selected by the
// literal atom; an atom that names no target injects nothing, because guessing
// a target would merge unrelated blocks.
func (e *usingCacheEntry) bodyFor(which string) *usingBody {
	if e.dispatch != nil && which != "" {
		return e.dispatch[which]
	}
	return &e.usingBody
}

// bodies returns every body a module's __using__ can inject: the ordinary one
// plus each atom-dispatch target. Used where the question is which modules
// *could* inject a name, rather than which one a given `use` site selects.
func (e *usingCacheEntry) bodies() []*usingBody {
	out := make([]*usingBody, 0, 1+len(e.dispatch))
	out = append(out, &e.usingBody)
	for _, b := range e.dispatch {
		out = append(out, b)
	}
	return out
}

func usingVisitKey(moduleName, which string) string {
	return moduleName + "\x00" + which
}

// lookupInUsingEntryFor is lookupInUsingEntry with the dispatch atom from the
// `use` site (empty for an ordinary `use Module`).
func (s *Server) lookupInUsingEntryFor(moduleName, functionName, which string, consumerOpts map[string]string, visited map[string]bool) []store.LookupResult {
	return s.lookupInUsingEntryForWithFollow(moduleName, functionName, which, consumerOpts, visited, s.followDelegates)
}

func (s *Server) lookupInUsingEntryForWithFollow(moduleName, functionName, which string, consumerOpts map[string]string, visited map[string]bool, followDelegates bool) []store.LookupResult {
	visitKey := usingVisitKey(moduleName, which)
	if visited[visitKey] {
		return nil
	}
	visited[visitKey] = true

	entry := s.cachedUsing(moduleName)
	if entry == nil {
		return nil
	}
	body := entry.bodyFor(which)
	if body == nil {
		return nil
	}

	// Inline defs take priority: directly injected by the quote do block
	if defs, ok := body.inlineDefs[functionName]; ok {
		var results []store.LookupResult
		for _, d := range defs {
			results = append(results, store.LookupResult{FilePath: entry.filePath, Line: d.line, Arity: d.arity, Kind: d.kind})
		}
		return results
	}

	// Static imports
	for j := len(body.imports) - 1; j >= 0; j-- {
		var results []store.LookupResult
		var err error
		if followDelegates {
			results, err = s.store.LookupFollowDelegate(body.imports[j], functionName)
			results = publicOnly(results)
		} else {
			results, err = s.store.LookupPublicFunction(body.imports[j], functionName)
		}
		if err != nil || len(results) == 0 {
			continue
		}
		return results
	}

	// Dynamic imports/uses driven by opts (e.g. `import unquote(mod)`)
	for _, b := range body.optBindings {
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
			if followDelegates {
				results, err = s.store.LookupFollowDelegate(mod, functionName)
				results = publicOnly(results)
			} else {
				results, err = s.store.LookupPublicFunction(mod, functionName)
			}
			if err == nil && len(results) > 0 {
				return results
			}
		case "use":
			if result := s.lookupInUsingEntryForWithFollow(mod, functionName, "", nil, visited, followDelegates); result != nil {
				return result
			}
		}
	}

	// Transitive uses: use Module inside the __using__ body (double-use chains)
	for k := len(body.transCalls) - 1; k >= 0; k-- {
		call := body.transCalls[k]
		if result := s.lookupInUsingEntryForWithFollow(call.Module, functionName, call.dispatchAtom(), call.Opts, visited, followDelegates); result != nil {
			return result
		}
	}
	for k := len(body.transUses) - 1; k >= 0; k-- {
		if body.hasTransCall(body.transUses[k]) {
			continue
		}
		if result := s.lookupInUsingEntryForWithFollow(body.transUses[k], functionName, "", nil, visited, followDelegates); result != nil {
			return result
		}
	}

	return nil
}

// resolveModuleViaUseChain returns the module name that provides functionName
// resolveModuleViaUseChainWithOpts is like resolveModuleViaUseChain but uses
// consumer-provided opts to resolve dynamic imports (e.g. `import unquote(mod)`).
func (s *Server) resolveModuleViaUseChainWithOpts(moduleName, functionName string, consumerOpts map[string]string, visited map[string]bool) string {
	return s.resolveModuleViaUseChainFor(moduleName, functionName, "", consumerOpts, visited)
}

// resolveModuleViaUseChainFor is resolveModuleViaUseChainWithOpts with the
// dispatch atom from the `use` site (empty for an ordinary `use Module`).
func (s *Server) resolveModuleViaUseChainFor(moduleName, functionName, which string, consumerOpts map[string]string, visited map[string]bool) string {
	visitKey := usingVisitKey(moduleName, which)
	if visited[visitKey] {
		return ""
	}
	visited[visitKey] = true

	entry := s.cachedUsing(moduleName)
	if entry == nil {
		return ""
	}
	body := entry.bodyFor(which)
	if body == nil {
		return ""
	}

	// Functions defined directly in the quote do block belong to the module
	// that injected them, not to any imported module.
	if _, ok := body.inlineDefs[functionName]; ok {
		return moduleName
	}

	for j := len(body.imports) - 1; j >= 0; j-- {
		if results, err := s.store.LookupPublicFunction(body.imports[j], functionName); err == nil && len(results) > 0 {
			return body.imports[j]
		}
	}

	for _, b := range body.optBindings {
		mod := consumerOpts[b.optKey]
		if mod == "" {
			mod = b.defaultMod
		}
		if mod == "" {
			continue
		}
		switch b.kind {
		case "import":
			if results, err := s.store.LookupPublicFunction(mod, functionName); err == nil && len(results) > 0 {
				return mod
			}
		case "use":
			if m := s.resolveModuleViaUseChainWithOpts(mod, functionName, nil, visited); m != "" {
				return m
			}
		}
	}

	for k := len(body.transCalls) - 1; k >= 0; k-- {
		call := body.transCalls[k]
		if mod := s.resolveModuleViaUseChainFor(call.Module, functionName, call.dispatchAtom(), call.Opts, visited); mod != "" {
			return mod
		}
	}
	for k := len(body.transUses) - 1; k >= 0; k-- {
		if body.hasTransCall(body.transUses[k]) {
			continue
		}
		if mod := s.resolveModuleViaUseChainWithOpts(body.transUses[k], functionName, nil, visited); mod != "" {
			return mod
		}
	}

	return ""
}

// usingEntryRef pairs a module with its parsed __using__ entry.
type usingEntryRef struct {
	module string
	entry  *usingCacheEntry
}

// loadUsingEntries returns the parsed __using__ entry of every module that
// defines one. Entries are loaded concurrently: each needs an os.Stat for
// mtime validation (and a file read on a cache miss), so the wall-clock cost
// is dominated by I/O rather than parsing.
func (s *Server) loadUsingEntries() []usingEntryRef {
	usingModules, err := s.store.LookupUsingModules()
	if err != nil {
		return nil
	}
	usingModules = uniqueUsingModules(usingModules)

	entries := make([]usingEntryRef, len(usingModules))
	var wg sync.WaitGroup
	for i, um := range usingModules {
		wg.Add(1)
		go func(i int, mod, path string) {
			defer wg.Done()
			defer s.index.reporter.Recover("request", "the __using__ cache")
			// The file path from the index skips the per-module LookupModule
			// query on a cache miss.
			entry := s.cachedUsingWithPath(mod, path)
			if entry != nil {
				entries[i] = usingEntryRef{mod, entry}
			}
		}(i, um.Module, um.FilePath)
	}
	wg.Wait()

	n := 0
	for _, c := range entries {
		if c.entry != nil {
			entries[n] = c
			n++
		}
	}
	return entries[:n]
}

// closeOverUsingChain walks the use chain upward from direct: if B.__using__
// uses one of the direct modules, and A.__using__ uses B, both join the set.
// seen must already hold the direct modules.
func closeOverUsingChain(entries []usingEntryRef, direct []string, seen map[string]bool) []string {
	all := append([]string{}, direct...)
	queue := append([]string{}, direct...)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, c := range entries {
			if seen[c.module] {
				continue
			}
			for _, body := range c.entry.bodies() {
				found := false
				for _, tu := range body.transUses {
					if tu == current {
						seen[c.module] = true
						all = append(all, c.module)
						queue = append(queue, c.module)
						found = true
						break
					}
				}
				if found {
					break
				}
			}
		}
	}

	return all
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
	entries := s.loadUsingEntries()

	// Step 1: Find modules whose __using__ directly imports targetModule.
	seen := make(map[string]bool)
	var directInjectors []string
	for _, c := range entries {
		for _, body := range c.entry.bodies() {
			found := false
			for _, imp := range body.imports {
				if imp == targetModule {
					directInjectors = append(directInjectors, c.module)
					seen[c.module] = true
					found = true
					break
				}
			}
			if found {
				break
			}
		}
	}

	if len(directInjectors) == 0 {
		return nil
	}

	// Step 2: Walk upward — find modules whose __using__ transitively uses
	// any of the direct injectors (via transUses in __using__ bodies).
	return closeOverUsingChain(entries, directInjectors, seen)
}

// injectedAlias is an alias that a __using__ block puts in the scope of every
// module that uses it. The call sites written through it carry only shortName,
// and the file holding them has no alias line of its own, so the index has
// them under that bare name.
type injectedAlias struct {
	shortName string   // name written at the call site — the last segment, or an `as:` name
	module    string   // module it resolves to: the target, or one under it
	injectors []string // modules whose `use` brings the alias in, transitively closed
}

type injectedAliasReference struct {
	store.ModuleReferenceResult
	written string
	columns []int
}

// matchInjectedAlias maps an alias declaration to the spelling and canonical
// module used for a target lookup. The alias may name the target, a descendant
// of it, or an ancestor of it.
func matchInjectedAlias(shortName, full, targetModule string) (written, resolved string, ok bool) {
	switch {
	case full == targetModule || strings.HasPrefix(full, targetModule+"."):
		return shortName, full, true
	case strings.HasPrefix(targetModule, full+"."):
		return shortName + targetModule[len(full):], targetModule, true
	default:
		return "", "", false
	}
}

// findInjectedAliasesFor returns the aliases that some module's __using__
// block binds to targetModule or a module under it.
//
// The alias line inside a __using__ body is itself an indexed reference to the
// target, so the injectors come out of targetRefs — the references the caller
// already looked up. That keeps this off the cost of loadUsingEntries, which
// stats every __using__ in the project; only the handful of modules that both
// define a __using__ and mention the target get their body loaded.
func (s *Server) findInjectedAliasesFor(targetModule string, targetRefs []store.ModuleReferenceResult) []injectedAlias {
	usingByFile := s.usingModulesByFile()
	if len(usingByFile) == 0 {
		return nil
	}

	// An injected alias may name a parent of the target: `alias MyApp.Repo`
	// makes `MyApp.Repo.Migrations` appear in consumer files as
	// `Repo.Migrations`. The target's own reference set cannot lead us to that
	// alias line, so add the exact references of each proper parent. These are
	// indexed point lookups, not prefix scans.
	refs := append([]store.ModuleReferenceResult(nil), targetRefs...)
	for parent := targetModule; ; {
		dot := strings.LastIndexByte(parent, '.')
		if dot < 0 {
			break
		}
		parent = parent[:dot]
		parentRefs, err := s.store.LookupReferences(parent, "")
		if err != nil {
			continue
		}
		for _, r := range parentRefs {
			refs = append(refs, store.ModuleReferenceResult{
				Module:   parent,
				FilePath: r.FilePath,
				Line:     r.Line,
				Kind:     r.Kind,
			})
		}
	}

	type aliasKey struct{ shortName, module string }
	direct := make(map[aliasKey][]string)
	checked := make(map[string]bool)
	for _, r := range refs {
		if r.Kind != "alias" && r.Kind != "require" && r.Kind != "import" {
			continue
		}
		for _, mod := range usingByFile[r.FilePath] {
			if checked[mod] {
				continue
			}
			checked[mod] = true
			// r.FilePath is one of the files of mod, not the choice of
			// usingSourceFile; the cache asks the index when it must.
			entry := s.cachedUsing(mod)
			if entry == nil {
				continue
			}
			for _, body := range entry.bodies() {
				for shortName, full := range body.aliases {
					written, resolved, ok := matchInjectedAlias(shortName, full, targetModule)
					if !ok {
						continue
					}
					k := aliasKey{written, resolved}
					direct[k] = append(direct[k], mod)
				}
			}
		}
	}
	if len(direct) == 0 {
		return nil
	}

	result := make([]injectedAlias, 0, len(direct))
	for k, injectors := range direct {
		result = append(result, injectedAlias{
			shortName: k.shortName,
			module:    k.module,
			injectors: s.closeOverInjectors(injectors, usingByFile),
		})
	}
	return result
}

// usingInjectsAlias reports whether the body selected by a concrete use call
// injects the requested alias, directly or through another use. Dispatch is
// significant: aliases from `:controller` must not leak into `:channel`.
func (s *Server) usingInjectsAlias(module, which, targetModule, written, resolved string, visited map[string]bool) bool {
	visitKey := usingVisitKey(module, which)
	if visited[visitKey] {
		return false
	}
	visited[visitKey] = true

	entry := s.cachedUsing(module)
	if entry == nil {
		return false
	}
	body := entry.bodyFor(which)
	if body == nil {
		return false
	}
	for shortName, full := range body.aliases {
		gotWritten, gotResolved, ok := matchInjectedAlias(shortName, full, targetModule)
		if ok && gotWritten == written && gotResolved == resolved {
			return true
		}
	}
	for i := len(body.transCalls) - 1; i >= 0; i-- {
		call := body.transCalls[i]
		if s.usingInjectsAlias(call.Module, call.dispatchAtom(), targetModule, written, resolved, visited) {
			return true
		}
	}
	for i := len(body.transUses) - 1; i >= 0; i-- {
		if body.hasTransCall(body.transUses[i]) {
			continue
		}
		if s.usingInjectsAlias(body.transUses[i], "", targetModule, written, resolved, visited) {
			return true
		}
	}
	return false
}

// lexicalScopeAt returns stable token positions for the block ancestry at a
// 1-based line, or immediately before beforeToken when it is non-negative. A
// token position is necessary for directives on lines that also open or close
// another block.
func lexicalScopeAt(tf *TokenizedFile, line, beforeToken int) []int {
	w := parser.NewTokenWalker(tf.source, tf.tokens)
	stack := make([]int, 0, 8)
	typespecEnd := -1
	for w.More() {
		tok := w.Current()
		if (beforeToken >= 0 && w.Pos() >= beforeToken) || (beforeToken < 0 && tok.Line > line) {
			break
		}
		switch tok.Kind {
		case parser.TokAttrType, parser.TokAttrSpec, parser.TokAttrCallback:
			typespecEnd = parser.ScanTypespecEnd(tf.source, tf.tokens, tf.n, w.Pos())
		case parser.TokDo, parser.TokFn:
			// `do:` and `fn:` keyword keys do not open blocks.
			next := parser.NextSigToken(tf.tokens, tf.n, w.Pos()+1)
			if next >= tf.n || tf.tokens[next].Kind != parser.TokColon {
				stack = append(stack, w.Pos())
			}
		case parser.TokEnd:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case parser.TokRightArrow:
			if w.Depth() == 0 && w.Pos() >= typespecEnd && len(stack) > 0 {
				stack[len(stack)-1] = w.Pos()
			}
		case parser.TokIdent:
			switch w.CurrentText() {
			case "else", "rescue", "catch", "after":
				next := parser.NextSigToken(tf.tokens, tf.n, w.Pos()+1)
				if w.Pos() >= typespecEnd && len(stack) > 0 && (next >= tf.n || tf.tokens[next].Kind != parser.TokColon) {
					stack[len(stack)-1] = w.Pos()
				}
			}
		}
		w.Advance()
	}
	return stack
}

// moduleEncloses reports whether a `use` written in outer can inject into a
// reference written in inner: the same module, or one nested inside it, since
// an injected alias is lexically scoped like a written one. Name nesting is
// necessary but not sufficient — two top-level modules can be named MyApp.A
// and MyApp.A.B — so the caller still checks scopeContains on the same file.
func moduleEncloses(outer, inner string) bool {
	return outer == inner || strings.HasPrefix(inner, outer+".")
}

func scopeContains(scope, parent []int) bool {
	if len(scope) < len(parent) {
		return false
	}
	for i := range parent {
		if scope[i] != parent[i] {
			return false
		}
	}
	return true
}

// usingModulesByFile groups the modules that inject a body into their users by
// the file that holds them. The __using__ definitions come from one small
// indexed query, served by the partial index on them.
func (s *Server) usingModulesByFile() map[string][]string {
	usingModules, err := s.store.LookupUsingModules()
	if err != nil {
		return nil
	}
	byFile := make(map[string][]string, len(usingModules))
	for _, um := range usingModules {
		byFile[um.FilePath] = append(byFile[um.FilePath], um.Module)
	}

	// An ExUnit.CaseTemplate writes its injected body as `using do` rather
	// than `defmacro __using__`, so it has no __using__ definition for the
	// query above to find — and test files reach their aliases through one of
	// these. Its `use ExUnit.CaseTemplate` line is indexed, which locates it
	// just as cheaply.
	caseTemplates, err := s.store.LookupCaseTemplateModules()
	if err != nil {
		return byFile
	}
	for _, ct := range caseTemplates {
		alreadyPresent := false
		for _, mod := range byFile[ct.FilePath] {
			if mod == ct.Module {
				alreadyPresent = true
				break
			}
		}
		if !alreadyPresent {
			byFile[ct.FilePath] = append(byFile[ct.FilePath], ct.Module)
		}
	}
	return byFile
}

// closeOverInjectors walks the use chain upward: a module whose own __using__
// uses an injector passes the injected alias on to its own users. Candidates
// come from the index — the `use` sites of each injector — so this costs one
// indexed query per injector rather than a scan of every __using__ body.
func (s *Server) closeOverInjectors(direct []string, usingByFile map[string][]string) []string {
	seen := make(map[string]bool, len(direct))
	all := make([]string, 0, len(direct))
	queue := make([]string, 0, len(direct))
	for _, m := range direct {
		if !seen[m] {
			seen[m] = true
			all = append(all, m)
			queue = append(queue, m)
		}
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		refs, err := s.store.LookupReferences(current, "")
		if err != nil {
			continue
		}
		for _, r := range refs {
			if r.Kind != "use" {
				continue
			}
			for _, mod := range usingByFile[r.FilePath] {
				if seen[mod] {
					continue
				}
				// The `use` has to sit inside the __using__ body to pass the
				// alias on. A module that uses the injector at its own top
				// level just consumes the alias.
				entry := s.cachedUsing(mod)
				if entry == nil {
					continue
				}
				passesOn := false
				for _, body := range entry.bodies() {
					for _, tu := range body.transUses {
						if tu == current {
							passesOn = true
							break
						}
					}
					if passesOn {
						break
					}
				}
				if !passesOn {
					continue
				}
				seen[mod] = true
				all = append(all, mod)
				queue = append(queue, mod)
			}
		}
	}

	return all
}

// injectedAliasRefs returns indexed references that name targetModule, or a
// module under it, through an alias injected by a __using__ block. The files
// holding them declare no alias of their own, so the index has them under the
// bare short name and a lookup for the target's full name cannot see them.
//
// Only files that actually `use` an injector are kept: the same short name
// written in a file with no such `use` is a different module.
// A functionName narrows the result to calls of that function through the
// injected alias (`Repo.all(...)` for MyApp.Repo.all); empty matches the
// module itself and every module under it.
func (s *Server) injectedAliasRefs(targetModule, functionName string, targetRefs []store.ModuleReferenceResult, moduleSitesOnly bool) []injectedAliasReference {
	aliases := s.findInjectedAliasesFor(targetModule, targetRefs)
	if len(aliases) == 0 {
		return nil
	}

	var out []injectedAliasReference
	type useSite struct {
		filePath string
		line     int
		token    int
		scope    []int
	}
	useSiteCache := make(map[string][]useSite)
	useCallCache := make(map[string][]UseCall)
	tokenizedFiles := make(map[string]*TokenizedFile)
	missingFiles := make(map[string]bool)
	type refKey struct {
		filePath string
		line     int
		module   string
		function string
	}
	type refOccurrence struct {
		token  int
		column int
		scope  []int
	}
	refCache := make(map[refKey][]refOccurrence)
	tokenizedAt := func(filePath string) (*TokenizedFile, bool) {
		tf, ok := tokenizedFiles[filePath]
		if !ok {
			if missingFiles[filePath] {
				return nil, false
			}
			text, _, found := s.readFileText(filePath)
			if !found {
				missingFiles[filePath] = true
				return nil, false
			}
			tf = NewTokenizedFile(text)
			tokenizedFiles[filePath] = tf
		}
		return tf, true
	}
	moduleAt := func(filePath string, line int) (string, bool) {
		tf, ok := tokenizedAt(filePath)
		if !ok {
			return "", false
		}
		return extractEnclosingModuleFromTokens(tf.source, tf.tokens, line-1), true
	}
	// scanRefStream collects the occurrences of module (optionally followed by
	// .function) on one line of a token stream. The interpolation stream is
	// scanned the same way, but its tokens have no place in the main stream, so
	// each occurrence borrows the position of the string token that holds it:
	// that is what lexical scope and use-site ordering are measured against.
	scanRefStream := func(tf *TokenizedFile, tokens []parser.Token, line int, module, function string, interp bool) []refOccurrence {
		n := len(tokens)
		var occurrences []refOccurrence
		// Both streams are in byte order, so their lines never decrease and the
		// first token on the line can be found without walking the file.
		start := sort.Search(n, func(k int) bool { return tokens[k].Line >= line })
		for i := start; i < n; i++ {
			if tokens[i].Line > line {
				break
			}
			if tokens[i].Line != line || tokens[i].Kind != parser.TokModule {
				continue
			}
			modName, next := tokCollectModuleName(tf.source, tokens, n, i)
			if modName != module {
				continue
			}
			if function != "" && (next+1 >= n || tokens[next].Kind != parser.TokDot || tokens[next+1].Kind != parser.TokIdent || parser.TokenText(tf.source, tokens[next+1]) != function) {
				continue
			}
			position := i
			column := tokens[i].Start - tf.lineStarts[line-1]
			if interp {
				// The last main-stream token starting at or before the
				// interpolation is the string literal that encloses it.
				position = sort.Search(tf.n, func(k int) bool { return tf.tokens[k].Start > tokens[i].Start }) - 1
				if position < 0 {
					position = 0
				}
				// LineStarts skips the newlines inside a string, so the column
				// of a token in a heredoc comes from the source itself.
				column = tokens[i].Start - bytes.LastIndexByte(tf.source[:tokens[i].Start], '\n') - 1
			}
			occurrences = append(occurrences, refOccurrence{
				token:  position,
				column: column,
				scope:  lexicalScopeAt(tf, line, position),
			})
			i = next - 1
		}
		return occurrences
	}
	refOccurrencesAt := func(filePath string, line int, module, function string) ([]refOccurrence, bool) {
		key := refKey{filePath, line, module, function}
		if occurrences, ok := refCache[key]; ok {
			return occurrences, true
		}
		tf, ok := tokenizedAt(filePath)
		if !ok {
			return nil, false
		}
		occurrences := scanRefStream(tf, tf.tokens, line, module, function, false)
		if len(tf.interp) > 0 {
			// A call written inside a #{} interpolation is indexed like any
			// other, but the main stream keeps the whole string as one token.
			occurrences = append(occurrences, scanRefStream(tf, tf.interp, line, module, function, true)...)
		}
		refCache[key] = occurrences
		return occurrences, len(occurrences) > 0
	}
	useCallsAt := func(filePath string, line int) []UseCall {
		tf, ok := tokenizedAt(filePath)
		if !ok {
			return nil
		}
		all, found := useCallCache[filePath]
		if !found {
			all = tf.ExtractUsesWithOpts(nil)
			useCallCache[filePath] = all
		}
		var calls []UseCall
		for _, call := range all {
			if call.line == line {
				calls = append(calls, call)
			}
		}
		return calls
	}
	resolvedUseModule := func(tf *TokenizedFile, call UseCall, want string) string {
		if call.Module == want {
			return call.Module
		}
		aliases := extractAliasesFromTokens(tf.source, tf.tokens[:call.token], call.line-1)
		current := extractEnclosingModuleFromTokens(tf.source, tf.tokens, call.line-1)
		return parser.ResolveModuleRef(call.moduleExpr, aliases, current)
	}
	for _, ia := range aliases {
		// Candidates come first: only a file holding one can yield a result, and
		// an injector is typically used by far more files than contain the name.
		// Tokenizing just those files is what keeps a hot target affordable.
		var functionRefs []store.ReferenceResult
		var prefixRefs []store.ModuleReferenceResult
		candidateFiles := make(map[string]bool)
		if functionName != "" {
			refs, err := s.store.LookupReferences(ia.shortName, functionName)
			if err != nil {
				continue
			}
			functionRefs = refs
			for _, r := range refs {
				candidateFiles[r.FilePath] = true
			}
		} else {
			// The short name stands in for ia.module, so `Repo.Migrations` under
			// an injected `alias MyApp.Repo` names `MyApp.Repo.Migrations`.
			refs, err := s.store.LookupReferencesByPrefix(ia.shortName)
			if err != nil {
				continue
			}
			for _, r := range refs {
				if moduleSitesOnly && r.Kind != "alias" && r.Kind != "import" && r.Kind != "use" && r.Kind != "quoted_use" && r.Kind != "require" {
					continue
				}
				prefixRefs = append(prefixRefs, r)
				candidateFiles[r.FilePath] = true
			}
		}
		if len(candidateFiles) == 0 {
			continue
		}

		// Exact lexical module scopes whose `use` brings this alias in. Keeping
		// the line also prevents an earlier bare name in the same module from
		// being mistaken for an alias that is introduced later. Sites are read
		// only in candidate files, which depend on nothing but the short name
		// within one call, so the cache key stays sound.
		consumers := make(map[string][]useSite)
		for _, inj := range ia.injectors {
			cacheKey := inj + "\x00" + ia.shortName + "\x00" + ia.module
			sites, ok := useSiteCache[cacheKey]
			if !ok {
				if refs, err := s.store.LookupReferences(inj, ""); err == nil {
					for _, r := range refs {
						if r.Kind != "use" || !candidateFiles[r.FilePath] {
							continue
						}
						tf, exists := tokenizedAt(r.FilePath)
						if !exists {
							continue
						}
						for _, call := range useCallsAt(r.FilePath, r.Line) {
							if resolvedUseModule(tf, call, inj) != inj || call.local || !s.usingInjectsAlias(inj, call.dispatchAtom(), targetModule, ia.shortName, ia.module, make(map[string]bool)) {
								continue
							}
							sites = append(sites, useSite{r.FilePath, r.Line, call.token, lexicalScopeAt(tf, r.Line, call.token)})
						}
					}
				}
				useSiteCache[cacheKey] = sites
			}
			for _, site := range sites {
				consumers[site.filePath] = append(consumers[site.filePath], site)
			}
		}
		if len(consumers) == 0 {
			continue
		}
		consumerColumns := func(filePath string, line int, module, function string) []int {
			useSites := consumers[filePath]
			if len(useSites) == 0 {
				return nil
			}
			refModule, ok := moduleAt(filePath, line)
			if !ok {
				return nil
			}
			occurrences, ok := refOccurrencesAt(filePath, line, module, function)
			if !ok {
				return nil
			}
			var columns []int
			for _, site := range useSites {
				if site.line > line {
					continue
				}
				useModule, found := moduleAt(filePath, site.line)
				if !found || !moduleEncloses(useModule, refModule) {
					continue
				}
				for _, occurrence := range occurrences {
					if site.line == line && site.token >= occurrence.token {
						continue
					}
					if scopeContains(occurrence.scope, site.scope) && !slices.Contains(columns, occurrence.column) {
						columns = append(columns, occurrence.column)
					}
				}
			}
			return columns
		}

		if functionName != "" {
			for _, r := range functionRefs {
				columns := consumerColumns(r.FilePath, r.Line, ia.shortName, functionName)
				if len(columns) == 0 {
					continue
				}
				out = append(out, injectedAliasReference{
					ModuleReferenceResult: store.ModuleReferenceResult{
						Module:   ia.module,
						FilePath: r.FilePath,
						Line:     r.Line,
						Kind:     r.Kind,
					},
					written: ia.shortName,
					columns: columns,
				})
			}
			continue
		}

		for _, r := range prefixRefs {
			columns := consumerColumns(r.FilePath, r.Line, r.Module, "")
			if len(columns) == 0 {
				continue
			}
			written := r.Module
			r.Module = ia.module + r.Module[len(ia.shortName):]
			out = append(out, injectedAliasReference{ModuleReferenceResult: r, written: written, columns: columns})
		}
	}
	return out
}

// injectedAliasReferences is injectedAliasRefs shaped for the References
// handler. moduleRefs are the module-level references to fullModule, which
// carry the alias line inside the injecting __using__ body.
func (s *Server) injectedAliasReferences(fullModule, functionName string, moduleRefs []store.ReferenceResult) []store.ReferenceResult {
	if len(moduleRefs) == 0 {
		return nil
	}
	adapted := make([]store.ModuleReferenceResult, 0, len(moduleRefs))
	for _, r := range moduleRefs {
		adapted = append(adapted, store.ModuleReferenceResult{
			Module:   fullModule,
			FilePath: r.FilePath,
			Line:     r.Line,
			Kind:     r.Kind,
		})
	}
	injected := s.injectedAliasRefs(fullModule, functionName, adapted, functionName == "")
	if len(injected) == 0 {
		return nil
	}
	out := make([]store.ReferenceResult, 0, len(injected))
	for _, r := range injected {
		// A module lookup answers with the alias/import/use sites that name
		// the module, not with every call made through it — the same
		// convention the direct lookup follows, which asks for refs with no
		// function. A function lookup wants exactly the call sites.
		if functionName == "" && r.Kind != "alias" && r.Kind != "import" && r.Kind != "use" && r.Kind != "quoted_use" && r.Kind != "require" {
			continue
		}
		out = append(out, store.ReferenceResult{FilePath: r.FilePath, Line: r.Line, Kind: r.Kind})
	}
	return out
}

// addCompletionsFromUsing adds completion items injected by a module's __using__
// body — inline defs, imported functions, and transitive uses — into items.
func (s *Server) addCompletionsFromUsingFor(moduleName, which string, consumerOpts map[string]string, funcPrefix string, seen map[string]bool, items *[]protocol.CompletionItem, visited map[string]bool, inPipe bool, useSnippets bool) {
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

	for funcName, defs := range body.inlineDefs {
		if !strings.HasPrefix(funcName, funcPrefix) {
			continue
		}
		if useSnippets && elixirFormSnippets[funcName] != "" {
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
				applySnippet(&item, funcName, d.arity, d.params, d.kind, inPipe, useSnippets)
				*items = append(*items, item)
			}
		}
	}

	addModule := func(mod string) {
		results, err := s.store.ListModuleFunctions(mod, true)
		if err != nil {
			return
		}
		for _, r := range results {
			key := funcKey(r.Function, r.Arity)
			if strings.HasPrefix(r.Function, funcPrefix) && !seen[key] {
				if useSnippets && elixirFormSnippets[r.Function] != "" {
					continue
				}
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
				applySnippet(&item, r.Function, r.Arity, r.Params, r.Kind, inPipe, useSnippets)
				*items = append(*items, item)
			}
		}
		s.addGeneratedFunctionCompletions(mod, "", funcPrefix, seen, items, inPipe, useSnippets)
	}
	for _, mod := range body.imports {
		addModule(mod)
	}

	for _, binding := range body.optBindings {
		mod := consumerOpts[binding.optKey]
		if mod == "" {
			mod = binding.defaultMod
		}
		if mod == "" {
			continue
		}
		switch binding.kind {
		case "import":
			addModule(mod)
		case "use":
			s.addCompletionsFromUsingFor(mod, "", nil, funcPrefix, seen, items, visited, inPipe, useSnippets)
		}
	}

	for _, call := range body.transCalls {
		s.addCompletionsFromUsingFor(call.Module, call.dispatchAtom(), call.Opts, funcPrefix, seen, items, visited, inPipe, useSnippets)
	}
	for _, transModule := range body.transUses {
		if !body.hasTransCall(transModule) {
			s.addCompletionsFromUsingFor(transModule, "", nil, funcPrefix, seen, items, visited, inPipe, useSnippets)
		}
	}
}

// resolveBareFunctionModule finds the module that defines a bare function name.
// Mirrors the go-to-definition priority: current file modules → imports → use chains → Kernel.
// Callers should pass pre-computed aliases to avoid redundant ExtractAliases scans.
func (s *Server) resolveBareFunctionModule(filePath, text string, tf *TokenizedFile, lineNum int, functionName string, aliases map[string]string) string {
	module, _ := s.resolveBareFunctionModuleWithOrigin(filePath, text, tf, lineNum, functionName, aliases)
	return module
}

// resolveBareFunctionModuleWithOrigin also reports whether the winner is an
// explicit definition in the cursor file. References uses that provenance to
// distinguish a consumer override from a function supplied by its use chain:
// both resolve to the consumer module, but only the latter should expand across
// other consumers of the injector.
func (s *Server) resolveBareFunctionModuleWithOrigin(filePath, text string, tf *TokenizedFile, lineNum int, functionName string, aliases map[string]string) (module string, definedInFile bool) {
	// Check all modules in the current file with a single query, preferring
	// the one closest to the cursor line (handles sibling nested modules).
	if mod, ok := s.store.LookupFunctionInFile(filePath, functionName, lineNum+1); ok {
		return mod, true
	}

	if tf == nil {
		tf = NewTokenizedFile(text)
	}

	// Explicit imports (direct definitions only — fast store lookup)
	imports := tf.ExtractImports()
	for _, mod := range imports {
		if results, err := s.store.LookupPublicFunction(mod, functionName); err == nil && len(results) > 0 {
			return mod, false
		}
	}

	// Use chains — use opts-aware resolution so `import unquote(mod)` patterns
	// resolve to the consumer-provided module rather than always using the default.
	useCalls := usesVisibleAt(tf.tokens, tf.ExtractUsesWithOpts(aliases), lineNum+1)
	for i := len(useCalls) - 1; i >= 0; i-- {
		uc := useCalls[i]
		if mod := s.resolveModuleViaUseChainFor(uc.Module, functionName, uc.dispatchAtom(), uc.Opts, map[string]bool{}); mod != "" {
			return mod, false
		}
	}

	// Kernel is always in scope
	if results, err := s.store.LookupPublicFunction("Kernel", functionName); err == nil && len(results) > 0 {
		return "Kernel", false
	}

	// An imported module can export a function that a macro generated, which
	// only its BEAM knows about.
	for _, mod := range imports {
		if _, found := s.generatedSymbol(mod, "", functionName); found {
			return mod, false
		}
	}

	// Slow fallback: function may be injected into an imported module via its
	// own use chain (e.g. MyApp.Factory uses ExMachina, which injects `insert`).
	for _, mod := range imports {
		if results := s.lookupThroughUseOf(mod, functionName); len(results) > 0 {
			return mod, false
		}
	}

	return "", false
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

// mergeAliasesFromUse augments the alias map with aliases injected by `use`
// declarations in the file. For example, if the file has `use MyApp.Schema`
// and MyApp.Schema.__using__ contains `alias MyApp.Repo`, then Repo is added.
func (s *Server) mergeAliasesFromUse(text string, aliases map[string]string) {
	s.mergeAliasesFromUseCalls(ExtractUsesWithOpts(text, aliases), aliases)
}

func (s *Server) mergeAliasesFromUseTokenized(tf *TokenizedFile, aliases map[string]string) {
	if tf == nil {
		return
	}
	s.mergeAliasesFromUseCalls(tf.ExtractUsesWithOpts(aliases), aliases)
}

func (s *Server) mergeAliasesFromUseCalls(useCalls []UseCall, aliases map[string]string) {
	visited := make(map[string]bool)
	for _, uc := range useCalls {
		s.mergeAliasesFromUsingEntryFor(uc.Module, uc.dispatchAtom(), uc.Opts, aliases, visited)
	}
}

func (s *Server) mergeAliasesFromUsingEntryFor(moduleName, which string, consumerOpts map[string]string, aliases map[string]string, visited map[string]bool) {
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

	for short, full := range body.aliases {
		if _, exists := aliases[short]; !exists {
			aliases[short] = full
		}
	}
	for _, binding := range body.optBindings {
		if binding.kind != "use" {
			continue
		}
		mod := consumerOpts[binding.optKey]
		if mod == "" {
			mod = binding.defaultMod
		}
		if mod != "" {
			s.mergeAliasesFromUsingEntryFor(mod, "", nil, aliases, visited)
		}
	}

	// Follow transitive uses
	for _, call := range body.transCalls {
		s.mergeAliasesFromUsingEntryFor(call.Module, call.dispatchAtom(), call.Opts, aliases, visited)
	}
	for _, transModule := range body.transUses {
		if !body.hasTransCall(transModule) {
			s.mergeAliasesFromUsingEntryFor(transModule, "", nil, aliases, visited)
		}
	}
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

// elixirKeywords are plain keyword completions (no snippet expansion).
// They prevent VS Code from falling back to word-based completions when the
// user types a keyword like "do" or "end" and presses Enter.
var elixirKeywords = []string{"do", "end"}

var elixirFormSnippets = map[string]string{
	"do":             "do\n\t$0\nend",
	"defmodule":      "defmodule ${1:Name} do\n\t$0\nend",
	"def":            "def ${1:name}$2 do\n\t$0\nend",
	"defp":           "defp ${1:name}$2 do\n\t$0\nend",
	"defmacro":       "defmacro ${1:name}$2 do\n\t$0\nend",
	"defmacrop":      "defmacrop ${1:name}$2 do\n\t$0\nend",
	"defstruct":      "defstruct [${1:fields}]$0",
	"defexception":   "defexception [${1:fields}]$0",
	"defprotocol":    "defprotocol ${1:Name} do\n\t$0\nend",
	"defimpl":        "defimpl ${1:Protocol}, for: ${2:Type} do\n\t$0\nend",
	"defdelegate":    "defdelegate ${1:func}$2, to: ${3:module}$0",
	"defguard":       "defguard ${1:name}$2 when ${3:condition}$0",
	"defguardp":      "defguardp ${1:name}$2 when ${3:condition}$0",
	"defoverridable": "defoverridable ${1:name}: ${2:arity}$0",
	"for":            "for ${1:pattern} <- ${2:enumerable} do\n\t$0\nend",
	"with":           "with ${1:pattern} <- ${2:expression} do\n\t$0\nend",
	"case":           "case ${1:expression} do\n\t${2:pattern} ->\n\t\t$0\nend",
	"cond":           "cond do\n\t${1:condition} ->\n\t\t$0\nend",
	"if":             "if ${1:condition} do\n\t$0\nelse\n\t${2}\nend",
	"unless":         "unless ${1:condition} do\n\t$0\nend",
	"receive":        "receive do\n\t${1:pattern} ->\n\t\t$0\nend",
	"try":            "try do\n\t$0\nrescue\n\t${1:exception} ->\n\t\t${2:handler}\nend",
	"quote":          "quote do\n\t$0\nend",
	"fn":             "fn ${1:args} -> $0 end",
}

// noParenFuncs are macros conventionally written without parentheses in Elixir,
// such as ExUnit's test/describe. When generating completion snippets or plain
// call text, these names produce `name arg1, arg2` instead of `name(arg1, arg2)`.
var noParenFuncs = map[string]bool{
	"test":            true,
	"describe":        true,
	"assert":          true,
	"refute":          true,
	"assert_raise":    true,
	"assert_receive":  true,
	"assert_received": true,
	"refute_receive":  true,
	"refute_received": true,
	"setup":           true,
	"setup_all":       true,
	"catch_error":     true,
	"catch_exit":      true,
	"catch_throw":     true,
}

// doBlockSnippets provides custom snippet templates for functions that take
// do/end blocks. These are applied when the function is in scope via the
// import/use-chain (unlike elixirFormSnippets which are global special forms).
var doBlockSnippets = map[string]string{
	"test":         "test \"${1:description}\" do\n\t$0\nend",
	"describe":     "describe \"${1:description}\" do\n\t$0\nend",
	"setup":        "setup do\n\t$0\nend",
	"setup_all":    "setup_all do\n\t$0\nend",
	"assert_raise": "assert_raise ${1:exception} do\n\t$0\nend",
}

func applySnippet(item *protocol.CompletionItem, name string, arity int, params string, kind string, inPipe bool, useSnippets bool) {
	item.Label = fmt.Sprintf("%s/%d", name, arity)
	item.FilterText = name

	// Check for a do-block snippet template first. These provide full
	// do/end block structure (e.g. `test "..." do ... end`) and take
	// priority over auto-generated arg lists.
	if useSnippets && isMacroKind(kind) {
		if tmpl, ok := doBlockSnippets[name]; ok {
			item.InsertTextFormat = protocol.InsertTextFormatSnippet
			item.InsertText = tmpl
			return
		}
	}

	snippetArity := arity
	snippetParams := params
	paramStartIndex := 1
	if inPipe && arity > 0 {
		snippetArity--
		paramStartIndex = 2
		if snippetParams != "" {
			if commaIdx := strings.IndexByte(snippetParams, ','); commaIdx >= 0 {
				snippetParams = snippetParams[commaIdx+1:]
			} else {
				snippetParams = ""
			}
		}
	}

	noParen := noParenFuncs[name]

	if !useSnippets {
		if snippetArity > 0 {
			item.InsertText = functionCallText(name, snippetArity, snippetParams, paramStartIndex, noParen)
		} else if noParen {
			item.InsertText = name
		} else {
			item.InsertText = name + "()"
		}
		return
	}

	if snippetArity > 0 {
		item.InsertTextFormat = protocol.InsertTextFormatSnippet
		item.InsertText = functionSnippet(name, snippetArity, snippetParams, paramStartIndex, noParen)
	} else if noParen {
		item.InsertText = name
	} else {
		item.InsertText = name + "()"
	}
}

func isMacroKind(kind string) bool {
	return kind == "defmacro" || kind == "defmacrop"
}

func functionSnippet(name string, arity int, params string, paramStartIndex int, noParen bool) string {
	return buildCallText(name, arity, params, true, paramStartIndex, noParen)
}

func functionCallText(name string, arity int, params string, paramStartIndex int, noParen bool) string {
	return buildCallText(name, arity, params, false, paramStartIndex, noParen)
}

func buildCallText(name string, arity int, params string, snippet bool, paramStartIndex int, noParen bool) string {
	if paramStartIndex < 1 {
		paramStartIndex = 1
	}
	var paramNames []string
	if params != "" {
		paramNames = strings.Split(params, ",")
	}
	var args []string
	for i := 0; i < arity; i++ {
		paramName := fmt.Sprintf("arg%d", paramStartIndex+i)
		if i < len(paramNames) {
			paramName = paramNames[i]
		}
		if snippet {
			args = append(args, fmt.Sprintf("${%d:%s}", i+1, paramName))
		} else {
			args = append(args, paramName)
		}
	}
	if noParen {
		if snippet {
			return name + " " + strings.Join(args, ", ") + "$0"
		}
		return name + " " + strings.Join(args, ", ")
	}
	call := name + "(" + strings.Join(args, ", ") + ")"
	if snippet {
		call += "$0"
	}
	return call
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
		FilePath          string `json:"filePath"`
		Line              int    `json:"line"`
		GeneratedModule   string `json:"generatedModule"`
		GeneratedFunction string `json:"generatedFunction"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return params, nil
	}
	if data.GeneratedModule != "" && data.GeneratedFunction != "" {
		if hover := s.hoverFromGenerated(data.GeneratedModule, "", data.GeneratedFunction); hover != nil {
			params.Documentation = hover.Contents
		}
		return params, nil
	}
	if data.FilePath == "" {
		return params, nil
	}

	cleaned := filepath.Clean(data.FilePath)
	inProject := strings.HasPrefix(cleaned, s.projectRoot+string(os.PathSeparator))
	stdlibRoot := s.StdlibRoot()
	inStdlib := stdlibRoot != "" && strings.HasPrefix(cleaned, stdlibRoot+string(os.PathSeparator))
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

	tf := NewTokenizedFile(string(fileData))
	doc, spec := tf.ExtractDocAbove(defIdx)
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

	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		s.debugf("Declaration: document not open")
		return nil, nil
	}

	path := uriToPath(params.TextDocument.URI)
	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)

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
		tf := s.docs.GetTokenizedFile(docURI)
		if tf == nil {
			tf = NewTokenizedFile(text)
		}
		exprCtx := tf.ExpressionAtCursor(lineNum, col)
		if exprCtx.Empty() {
			s.debugf("Declaration: no expression at cursor")
			return nil, nil
		}
		functionName = exprCtx.FunctionName
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
		s.mergeAliasesFromUse(text, aliases)
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
	uses := ExtractUsesWithOpts(text, aliases)
	visited := make(map[string]bool)
	var results []store.CallbackResult
	for _, call := range uses {
		s.collectCallbacksInChainFor(call.Module, call.dispatchAtom(), functionName, arity, visited, &results)
	}
	return results
}

func (s *Server) collectCallbacksInChainFor(moduleName, which, functionName string, arity int, visited map[string]bool, results *[]store.CallbackResult) {
	visitKey := usingVisitKey(moduleName, which)
	if visited[visitKey] {
		return
	}
	visited[visitKey] = true

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
	body := entry.bodyFor(which)
	if body == nil {
		return
	}
	for _, call := range body.transCalls {
		s.collectCallbacksInChainFor(call.Module, call.dispatchAtom(), functionName, arity, visited, results)
	}
	for _, transModule := range body.transUses {
		if !body.hasTransCall(transModule) {
			s.collectCallbacksInChainFor(transModule, "", functionName, arity, visited, results)
		}
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
		if filepath.Base(path) == ".formatter.exs" {
			s.restartBeamForFormatterConfig(path)
			continue
		}
		if base := filepath.Base(path); base == "mix.exs" || base == "mix.lock" {
			s.ManifestChanged()
		} else if strings.Contains(path, string(filepath.Separator)+"_build"+string(filepath.Separator)) {
			s.index.beamFailures.forget()
		}
		switch change.Type {
		case protocol.FileChangeTypeCreated, protocol.FileChangeTypeChanged:
			if s.workspaceEvents != nil {
				s.workspaceEvents.ReconcileFile(path)
			} else {
				go func() { // see DidSave
					defer s.index.reporter.Recover("index", "a single-file index update")
					_ = s.ReconcileFile(path)
				}()
			}
		case protocol.FileChangeTypeDeleted:
			if s.workspaceEvents != nil {
				s.workspaceEvents.RemoveFile(path)
			} else {
				go func() {
					defer s.index.reporter.Recover("index", "a single-file index removal")
					s.RemoveFile(path)
				}()
			}
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
	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)

	tree, src, release, hasTree := s.docs.GetTree(docURI)
	if !hasTree {
		return nil, nil
	}
	defer release()
	root := tree.RootNode()

	// Try scope-aware variable highlight first
	if occs := treesitter.FindVariableOccurrencesWithTree(root, src, uint(lineNum), uint(col)); len(occs) > 0 {
		var highlights []protocol.DocumentHighlight
		for _, occ := range occs {
			highlights = append(highlights, protocol.DocumentHighlight{
				Range: protocol.Range{
					Start: s.outPos(lines, int(occ.Line), int(occ.StartCol)),
					End:   s.outPos(lines, int(occ.Line), int(occ.EndCol)),
				},
				Kind: protocol.DocumentHighlightKindText,
			})
		}
		return highlights, nil
	}

	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}
	curCtx := tf.ExpressionAtCursor(lineNum, col)
	if curCtx.Empty() {
		return nil, nil
	}

	token := curCtx.FunctionName
	if token == "" {
		token = moduleLastSegment(curCtx.ModuleRef)
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
				Start: s.outPos(lines, int(occ.Line), int(occ.StartCol)),
				End:   s.outPos(lines, int(occ.Line), int(occ.EndCol)),
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
	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	var tokens []parser.Token
	var source []byte
	if cachedTokens, cachedSrc, ok := s.docs.GetTokens(docURI); ok {
		tokens = cachedTokens
		source = cachedSrc
	} else {
		source = []byte(text)
		tokens = parser.Tokenize(source)
	}

	lines := strings.Split(text, "\n")
	lastLine := len(lines) - 1
	n := len(tokens)

	tokText := func(t parser.Token) string { return string(source[t.Start:t.End]) }

	// tokCol returns the 0-based column of token t within its line.
	tokCol := func(t parser.Token) int {
		lineStart := t.Start
		for lineStart > 0 && source[lineStart-1] != '\n' {
			lineStart--
		}
		return t.Start - lineStart
	}

	// nextSig returns the index of the next significant (non-EOL, non-comment) token.
	nextSig := func(from int) int { return parser.NextSigToken(tokens, n, from) }

	type symbolEntry struct {
		symbol    protocol.DocumentSymbol
		module    string
		parentIdx int
	}

	type blockFrame struct {
		name     string
		depth    int
		entryIdx int
	}

	var entries []symbolEntry
	var moduleStack []blockFrame
	var funcStack []blockFrame
	depth := 0

	currentParentIdx := func() int {
		if len(funcStack) > 0 {
			return funcStack[len(funcStack)-1].entryIdx
		}
		if len(moduleStack) > 0 {
			return moduleStack[len(moduleStack)-1].entryIdx
		}
		return -1
	}

	currentModule := func() string {
		if len(moduleStack) > 0 {
			return moduleStack[len(moduleStack)-1].name
		}
		return ""
	}

	// lineEndChar returns the character length of the given 0-based line index.
	lineEndChar := func(lineIdx int) int {
		if lineIdx >= 0 && lineIdx < len(lines) {
			return len(lines[lineIdx])
		}
		return 0
	}

	// isLineFirstSignificant returns true if tokens[i] is the first
	// non-EOL/comment token on its line.
	isLineFirstSignificant := func(i int) bool {
		tokLine := tokens[i].Line
		for j := i - 1; j >= 0; j-- {
			k := tokens[j].Kind
			if k == parser.TokEOL || k == parser.TokEOF {
				return true
			}
			if k == parser.TokComment {
				continue
			}
			if tokens[j].Line == tokLine {
				return false
			}
			return true
		}
		return true
	}

	for i := 0; i < n; i++ {
		tok := tokens[i]

		switch tok.Kind {
		case parser.TokEnd:
			// An `end` after other code on its line, as in `def foo do :ok end`,
			// closes its frame too.
			lineIdx := tok.Line - 1
			endPos := s.outPos(lines, lineIdx, lineEndChar(lineIdx))

			prevDepth := depth
			parser.TrackBlockDepth(tok.Kind, &depth)

			if len(funcStack) > 0 && funcStack[len(funcStack)-1].depth == prevDepth {
				entries[funcStack[len(funcStack)-1].entryIdx].symbol.Range.End = endPos
				funcStack = funcStack[:len(funcStack)-1]
			} else if len(moduleStack) > 0 && moduleStack[len(moduleStack)-1].depth == prevDepth {
				entries[moduleStack[len(moduleStack)-1].entryIdx].symbol.Range.End = endPos
				moduleStack = moduleStack[:len(moduleStack)-1]
			}

		case parser.TokDefmodule, parser.TokDefprotocol, parser.TokDefimpl:
			if !isLineFirstSignificant(i) {
				continue
			}
			keyword := tokText(tok)
			lineIdx := tok.Line - 1
			indent := tokCol(tok)

			j := nextSig(i + 1)
			name, _ := parser.CollectModuleName(source, tokens, n, j)
			if name == "" {
				continue
			}

			curMod := currentModule()
			fullName := name
			if keyword != "defimpl" {
				fullName, _, _ = parser.NestedModuleName(name, curMod)
			}

			kind := defKindToSymbolKind(keyword)
			if keyword == "defmodule" {
				kind = defKindToSymbolKind("module")
			}

			nameCol := strings.Index(lines[lineIdx], name)
			if nameCol < 0 {
				nameCol = indent
			}

			entryIdx := len(entries)
			moduleParentIdx := -1
			if len(moduleStack) > 0 {
				moduleParentIdx = moduleStack[len(moduleStack)-1].entryIdx
			}
			entries = append(entries, symbolEntry{
				symbol: protocol.DocumentSymbol{
					Name:   name,
					Detail: keyword,
					Kind:   kind,
					Range: protocol.Range{
						Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
						End:   protocol.Position{Line: uint32(lastLine), Character: 0},
					},
					SelectionRange: protocol.Range{
						Start: s.outPos(lines, lineIdx, nameCol),
						End:   s.outPos(lines, lineIdx, nameCol+len(name)),
					},
				},
				module:    curMod,
				parentIdx: moduleParentIdx,
			})
			_, nextPos, hasDoBlock := parser.ScanForwardToBlockDo(tokens, n, j)
			if hasDoBlock {
				depth++
				moduleStack = append(moduleStack, blockFrame{name: fullName, depth: depth, entryIdx: entryIdx})
				i = nextPos - 1
			}

		case parser.TokDef, parser.TokDefp, parser.TokDefmacro, parser.TokDefmacrop,
			parser.TokDefguard, parser.TokDefguardp, parser.TokDefdelegate:
			if !isLineFirstSignificant(i) {
				continue
			}
			curMod := currentModule()
			if curMod == "" {
				continue
			}
			kind := tokText(tok)
			lineIdx := tok.Line - 1

			funcName, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
			if !ok {
				continue
			}
			nameCol := tokCol(tokens[j])
			j = nextSig(j + 1)
			arity, _, _, _ := parser.CollectParams(source, tokens, n, j)
			nameWithArity := fmt.Sprintf("%s/%d", funcName, arity)

			_, nextPos, hasDoBlock := parser.ScanForwardToBlockDo(tokens, n, j)

			rangeEnd := s.outPos(lines, lineIdx, lineEndChar(lineIdx))
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
						Start: s.outPos(lines, lineIdx, nameCol),
						End:   s.outPos(lines, lineIdx, nameCol+len(funcName)),
					},
				},
				module:    curMod,
				parentIdx: currentParentIdx(),
			})

			if hasDoBlock {
				depth++
				funcStack = append(funcStack, blockFrame{depth: depth, entryIdx: entryIdx})
				i = nextPos - 1
			}

		case parser.TokDefstruct:
			if !isLineFirstSignificant(i) {
				continue
			}
			curMod := currentModule()
			if curMod == "" {
				continue
			}
			lineIdx := tok.Line - 1
			indent := tokCol(tok)
			entries = append(entries, symbolEntry{
				symbol: protocol.DocumentSymbol{
					Name:   "defstruct",
					Detail: "defstruct",
					Kind:   defKindToSymbolKind("defstruct"),
					Range: protocol.Range{
						Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
						End:   s.outPos(lines, lineIdx, lineEndChar(lineIdx)),
					},
					SelectionRange: protocol.Range{
						Start: s.outPos(lines, lineIdx, indent),
						End:   s.outPos(lines, lineIdx, indent+9),
					},
				},
				module:    curMod,
				parentIdx: currentParentIdx(),
			})

		case parser.TokDefexception:
			if !isLineFirstSignificant(i) {
				continue
			}
			curMod := currentModule()
			if curMod == "" {
				continue
			}
			lineIdx := tok.Line - 1
			indent := tokCol(tok)
			entries = append(entries, symbolEntry{
				symbol: protocol.DocumentSymbol{
					Name:   "defexception",
					Detail: "defexception",
					Kind:   defKindToSymbolKind("defexception"),
					Range: protocol.Range{
						Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
						End:   s.outPos(lines, lineIdx, lineEndChar(lineIdx)),
					},
					SelectionRange: protocol.Range{
						Start: s.outPos(lines, lineIdx, indent),
						End:   s.outPos(lines, lineIdx, indent+12),
					},
				},
				module:    curMod,
				parentIdx: currentParentIdx(),
			})

		case parser.TokAttrType:
			if !isLineFirstSignificant(i) {
				continue
			}
			curMod := currentModule()
			if curMod == "" {
				continue
			}
			attrText := tokText(tok)
			var kind string
			switch attrText {
			case "@typep":
				kind = "typep"
			case "@type":
				kind = "type"
			case "@opaque":
				kind = "opaque"
			default:
				continue
			}
			lineIdx := tok.Line - 1

			name, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
			if !ok {
				continue
			}
			nameCol := tokCol(tokens[j])
			j = nextSig(j + 1)
			arity, _, _, _ := parser.CollectParams(source, tokens, n, j)
			nameWithArity := fmt.Sprintf("%s/%d", name, arity)

			entries = append(entries, symbolEntry{
				symbol: protocol.DocumentSymbol{
					Name:   nameWithArity,
					Detail: "@" + kind,
					Kind:   defKindToSymbolKind(kind),
					Range: protocol.Range{
						Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
						End:   s.outPos(lines, lineIdx, lineEndChar(lineIdx)),
					},
					SelectionRange: protocol.Range{
						Start: s.outPos(lines, lineIdx, nameCol),
						End:   s.outPos(lines, lineIdx, nameCol+len(name)),
					},
				},
				module:    curMod,
				parentIdx: currentParentIdx(),
			})

		case parser.TokAttrCallback:
			if !isLineFirstSignificant(i) {
				continue
			}
			curMod := currentModule()
			if curMod == "" {
				continue
			}
			attrText := tokText(tok)
			var kind string
			switch attrText {
			case "@callback":
				kind = "callback"
			case "@macrocallback":
				kind = "macrocallback"
			default:
				continue
			}
			lineIdx := tok.Line - 1

			name, j, ok := parser.StaticDeclarationName(source, tokens, n, i)
			if !ok {
				continue
			}
			nameCol := tokCol(tokens[j])
			j = nextSig(j + 1)
			arity, _, _, _ := parser.CollectParams(source, tokens, n, j)
			nameWithArity := fmt.Sprintf("%s/%d", name, arity)

			entries = append(entries, symbolEntry{
				symbol: protocol.DocumentSymbol{
					Name:   nameWithArity,
					Detail: "@" + kind,
					Kind:   defKindToSymbolKind(kind),
					Range: protocol.Range{
						Start: protocol.Position{Line: uint32(lineIdx), Character: 0},
						End:   s.outPos(lines, lineIdx, lineEndChar(lineIdx)),
					},
					SelectionRange: protocol.Range{
						Start: s.outPos(lines, lineIdx, nameCol),
						End:   s.outPos(lines, lineIdx, nameCol+len(name)),
					},
				},
				module:    curMod,
				parentIdx: currentParentIdx(),
			})

		case parser.TokIdent:
			// Bare macro calls with do blocks (describe, test, setup, etc.)
			curMod := currentModule()
			if curMod == "" {
				continue
			}
			if !isLineFirstSignificant(i) {
				continue
			}
			macroName := tokText(tok)
			if parser.IsElixirKeyword(macroName) {
				continue
			}
			// Skip keyword keys like "reduce:" — the token after the ident is TokColon
			if i+1 < n && tokens[i+1].Kind == parser.TokColon {
				continue
			}
			doIdx, nextPos, hasDoBlock := parser.ScanForwardToMacroCallBlockDo(tokens, n, i+1)
			if !hasDoBlock {
				continue
			}

			lineIdx := tok.Line - 1
			indent := tokCol(tok)

			// Extract the argument between the macro name and `do` from source bytes.
			// Collapse internal whitespace so a multi-line keyword-arg head renders
			// as a single-line outline label.
			label := macroName
			argBytes := source[tok.End:tokens[doIdx].Start]
			arg := strings.Join(strings.Fields(string(argBytes)), " ")
			if len(arg) >= 2 && arg[0] == '"' && arg[len(arg)-1] == '"' {
				arg = arg[1 : len(arg)-1]
			}
			if arg != "" {
				label = macroName + " " + arg
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
						Start: s.outPos(lines, lineIdx, indent),
						End:   s.outPos(lines, lineIdx, indent+len(macroName)),
					},
				},
				module:    curMod,
				parentIdx: currentParentIdx(),
			})
			depth++
			funcStack = append(funcStack, blockFrame{depth: depth, entryIdx: entryIdx})
			i = nextPos - 1

		case parser.TokDo, parser.TokFn:
			parser.TrackBlockDepth(tok.Kind, &depth)
		}
	}

	// Build hierarchical tree using parentIdx references.
	type symNode struct {
		sym      protocol.DocumentSymbol
		children []int
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
	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	// Use cached tokens if available
	var tokens []parser.Token
	var source []byte
	if cachedTokens, cachedSrc, ok := s.docs.GetTokens(docURI); ok {
		tokens = cachedTokens
		source = cachedSrc
	} else {
		source = []byte(text)
		tokens = parser.Tokenize(source)
	}
	n := len(tokens)

	var ranges []protocol.FoldingRange

	// Track do/fn..end blocks by depth
	type blockStart struct {
		line  int
		depth int
	}
	var stack []blockStart
	depth := 0

	// Track multi-line bracket pairs ({}, [], (), <<>>)
	type bracketFrame struct {
		line int
		open parser.TokenKind
	}
	var brackets []bracketFrame
	popBracket := func(open parser.TokenKind, line int) {
		if n := len(brackets); n > 0 && brackets[n-1].open == open {
			top := brackets[n-1]
			brackets = brackets[:n-1]
			if line > top.line {
				ranges = append(ranges, protocol.FoldingRange{
					StartLine: uint32(top.line - 1), // convert to 0-based
					EndLine:   uint32(line - 1),
				})
			}
		}
	}

	for i := 0; i < n; i++ {
		tok := tokens[i]

		switch tok.Kind {
		case parser.TokHeredoc:
			// Heredocs are single tokens spanning multiple lines — fold them
			// Find the end line by scanning for last newline in the token
			startLine := tok.Line
			endLine := startLine
			for j := tok.Start; j < tok.End; j++ {
				if source[j] == '\n' {
					endLine++
				}
			}
			if endLine > startLine {
				ranges = append(ranges, protocol.FoldingRange{
					StartLine: uint32(startLine - 1), // convert to 0-based
					EndLine:   uint32(endLine - 1),
				})
			}

		case parser.TokDo, parser.TokFn:
			parser.TrackBlockDepth(tok.Kind, &depth)
			stack = append(stack, blockStart{line: tok.Line, depth: depth})

		case parser.TokEnd:
			prevDepth := depth
			parser.TrackBlockDepth(tok.Kind, &depth)
			if len(stack) > 0 && stack[len(stack)-1].depth == prevDepth {
				top := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if tok.Line > top.line {
					ranges = append(ranges, protocol.FoldingRange{
						StartLine: uint32(top.line - 1), // convert to 0-based
						EndLine:   uint32(tok.Line - 1),
					})
				}
			}

		case parser.TokOpenBrace, parser.TokOpenBracket, parser.TokOpenParen, parser.TokOpenAngle:
			brackets = append(brackets, bracketFrame{line: tok.Line, open: tok.Kind})
		case parser.TokCloseBrace:
			popBracket(parser.TokOpenBrace, tok.Line)
		case parser.TokCloseBracket:
			popBracket(parser.TokOpenBracket, tok.Line)
		case parser.TokCloseParen:
			popBracket(parser.TokOpenParen, tok.Line)
		case parser.TokCloseAngle:
			popBracket(parser.TokOpenAngle, tok.Line)
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

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	formatted, err := s.formatContent(ctx, mixRoot, path, text)
	if err != nil {
		var formatErr *FormatError
		if errors.As(err, &formatErr) {
			s.publishFormatDiagnostic(params.TextDocument.URI, formatErr)
		}
		return nil, nil
	}

	s.clearFormatDiagnostics(params.TextDocument.URI)

	// If the document changed while we were formatting, the edits would be
	// against stale content and corrupt the buffer.
	currentText, _ := s.docs.Get(string(params.TextDocument.URI))
	if currentText != text {
		return nil, nil
	}

	return computeMinimalEdits(text, formatted), nil
}
func (s *Server) Hover(ctx context.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
	docURI := string(params.TextDocument.URI)

	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)

	if lineNum >= len(lines) {
		return nil, nil
	}

	// Get cached tokens for efficient multi-query operations
	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}

	exprCtx := tf.ExpressionAtCursor(lineNum, col)
	if exprCtx.Empty() {
		return nil, nil
	}

	// Erlang module atom (e.g. :lists.flatten) — fetch docs via BEAM process
	if strings.HasPrefix(exprCtx.ModuleRef, ":") {
		return s.erlangHover(ctx, uriToPath(params.TextDocument.URI), exprCtx.ModuleRef[1:], exprCtx.FunctionName)
	}

	expr := tf.ResolveModuleExpr(exprCtx.Expr(), lineNum)
	moduleRef, functionName := ExtractModuleAndFunction(expr)

	// Inside a multi-line alias block like "alias MyModule.{ Something }",
	// prepend the parent so "Something" resolves to "MyModule.Something".
	if moduleRef != "" {
		if aliasParent, inBlock := tf.ExtractAliasBlockParent(lineNum); inBlock {
			moduleRef = aliasParent + "." + moduleRef
		}
	}

	aliases := tf.ExtractAliasesInScope(lineNum)
	s.mergeAliasesFromUseTokenized(tf, aliases)

	if moduleRef == "" {
		if functionName == "" {
			return nil, nil
		}

		// A bound variable has no hover. Check it first, as Definition does, so
		// that a hover on a variable does not wait for the BEAM.
		if tree, src, release, ok := s.docs.GetTree(docURI); ok {
			occs := treesitter.FindVariableOccurrencesWithTree(tree.RootNode(), src, uint(lineNum), uint(col))
			release()
			if len(occs) > 0 {
				return nil, nil
			}
		}

		currentModule := tf.FirstDefmodule()
		fullModule := s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, tf, lineNum, functionName, aliases)

		if fullModule != "" {
			// Current module — hover from the buffer directly. In a typespec the
			// bare name is the type, everywhere else the function.
			if fullModule == currentModule {
				find := tf.FindFunctionDefinition
				if tf.InTypespec(lineNum) {
					find = tf.FindTypeDefinition
				}
				if line, found := find(functionName); found {
					return s.hoverFromBuffer(tf, text, line-1)
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
				hits := byKindForContext(tf, lineNum, results)
				return s.hoverFromFile(functionName, hits[0])
			}
		}

		// Fallback for use-chain inline defs (not stored as module definitions)
		if results := s.lookupThroughUse(text, functionName, aliases); len(results) > 0 {
			return s.hoverFromFile(functionName, results[0])
		}

		// Generated functions have no source definition to hover: a macro emitted
		// them into the compiled module. Look in the enclosing module and in the
		// modules whose DSL macros it pulls in.
		enclosing := s.store.LookupEnclosingModule(uriToPath(protocol.DocumentURI(docURI)), lineNum+1)
		if enclosing == "" {
			enclosing = currentModule
		}
		if hover := s.hoverFromGeneratedModules(enclosing, func() []string {
			return s.enclosingBlockPath(docURI, lineNum, col)
		}, functionName); hover != nil {
			return hover, nil
		}

		// Fallback: bare identifier might be an Erlang built-in type or function
		// (e.g. pos_integer, binary, term, length, is_atom)
		if hover, _ := s.erlangHover(ctx, uriToPath(protocol.DocumentURI(docURI)), "erlang", functionName); hover != nil {
			return hover, nil
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
			hits := byKindForContext(tf, lineNum, results)
			return s.hoverFromFile(functionName, hits[0])
		}
		// Nothing in the index: the function may exist only in the compiled module.
		if hover := s.hoverFromGenerated(fullModule, "", functionName); hover != nil {
			return hover, nil
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

	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)

	if lineNum >= len(lines) {
		return nil, nil
	}

	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}
	exprCtx := tf.ExpressionAtCursor(lineNum, col)
	functionName := exprCtx.FunctionName
	if functionName == "" {
		// On a protocol's name: its defimpl blocks and derives.
		if exprCtx.ModuleRef == "" {
			return nil, nil
		}
		aliases := tf.ExtractAliasesInScope(lineNum)
		impls, ok := s.ProtocolImplementations(s.resolveModuleWithNesting(exprCtx.ModuleRef, aliases, uriToPath(params.TextDocument.URI), lineNum))
		if !ok {
			return nil, nil
		}
		return s.protocolImplLocations(impls, nil), nil
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

	// Only proceed if this function is a declared callback in the current
	// module, or a function of the protocol the cursor is in.
	callbacks, err := s.store.LookupCallbackDef(currentModule, functionName)
	if err != nil || len(callbacks) == 0 {
		defs, declared := s.ProtocolFunctionImpls(currentModule, functionName)
		if !declared {
			return nil, nil
		}
		impls, _ := s.ProtocolImplementations(currentModule)
		var derives []ProtocolImpl
		for _, impl := range impls {
			if impl.Kind == "derive" {
				derives = append(derives, impl)
			}
		}
		return s.protocolImplLocations(derives, defs), nil
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

// protocolImplLocations renders protocol implementations as locations: the
// given definitions, then each defimpl or @derive line.
func (s *Server) protocolImplLocations(impls []ProtocolImpl, defs []store.LookupResult) []protocol.Location {
	locations := storeResultsToLocations(defs)
	for _, impl := range impls {
		if impl.Unsure {
			// A location cannot say that it is a guess, so the editor leaves
			// out a derive that Dexter cannot confirm; MCP lists it marked.
			continue
		}
		locations = append(locations, protocol.Location{
			URI:   protocol.DocumentURI(uri.File(impl.FilePath)),
			Range: lineRange(impl.Line - 1),
		})
	}
	return locations
}

func (s *Server) OnTypeFormatting(ctx context.Context, params *protocol.DocumentOnTypeFormattingParams) ([]protocol.TextEdit, error) {
	return nil, nil
}
func (s *Server) PrepareRename(ctx context.Context, params *protocol.PrepareRenameParams) (*protocol.Range, error) {
	docURI := string(params.TextDocument.URI)
	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)
	if lineNum >= len(lines) {
		return nil, nil
	}

	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}
	exprCtx := tf.ExpressionAtCursor(lineNum, col)
	moduleRef, functionName := exprCtx.ModuleRef, exprCtx.FunctionName
	exprStart := exprCtx.ExprStart

	// For bare identifiers (no module qualifier), check tree-sitter variables
	// first — a local variable shadows a same-named function in Elixir.
	if moduleRef == "" {
		if tree, src, release, ok := s.docs.GetTree(docURI); ok {
			defer release()
			if occs := treesitter.FindVariableOccurrencesWithTree(tree.RootNode(), src, uint(lineNum), uint(col)); len(occs) > 0 {
				for _, occ := range occs {
					if occ.Line == uint(lineNum) && uint(col) >= occ.StartCol && uint(col) < occ.EndCol {
						return &protocol.Range{
							Start: s.outPos(lines, int(occ.Line), int(occ.StartCol)),
							End:   s.outPos(lines, int(occ.Line), int(occ.EndCol)),
						}, nil
					}
				}
				return &protocol.Range{
					Start: s.outPos(lines, int(occs[0].Line), int(occs[0].StartCol)),
					End:   s.outPos(lines, int(occs[0].Line), int(occs[0].EndCol)),
				}, nil
			}
		}
	}

	// Try module/function rename via the index
	if !exprCtx.Empty() {
		aliases := tf.ExtractAliasesInScope(lineNum)

		// Detect `as:` aliases — these are file-local renames, not module renames.
		// An `as:` alias has a short name that differs from the last segment of
		// the resolved module (e.g. TransactionReceiptSchema → MyApp.Billing.TransactionReceipt).
		// This check runs before merging use-injected aliases because those
		// declarations live in another file's __using__ macro and must not be
		// treated as file-local renames.
		if moduleRef != "" && functionName == "" {
			if resolved, ok := aliases[moduleRef]; ok && moduleLastSegment(resolved) != moduleRef {
				// File-local alias rename: find all occurrences in this file
				return &protocol.Range{
					Start: s.outPos(lines, lineNum, exprStart),
					End:   s.outPos(lines, lineNum, exprStart+len(moduleRef)),
				}, nil
			}
		}

		s.mergeAliasesFromUseTokenized(tf, aliases)

		var tokenName string
		var fullModule string
		found := false

		if functionName != "" {
			tokenName = functionName
			if moduleRef != "" {
				fullModule = resolveModule(moduleRef, aliases)
			} else {
				fullModule = s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, tf, lineNum, functionName, aliases)
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
				if s.isStdlibPath(p) || s.isDepsFile(p) {
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
					Start: s.outPos(lines, lineNum, tokenStart),
					End:   s.outPos(lines, lineNum, tokenStart+len(tokenName)),
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

	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		s.debugf("References: document not found in store")
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)

	if lineNum >= len(lines) {
		s.debugf("References: line %d out of range (total %d)", lineNum, len(lines))
		return nil, nil
	}

	// Get cached tokens for efficient multi-query operations
	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}

	cursorCtx := tf.ExpressionAtCursor(lineNum, col)
	if cursorCtx.Empty() {
		s.debugf("References: no expression at cursor")
		return nil, nil
	}

	// Special case: cursor on defmacro __using__ — find all `use ModuleName` sites.
	if cursorCtx.Expr() == "__using__" {
		moduleName := tf.FirstDefmodule()
		if moduleName != "" {
			s.debugf("References: __using__ in module %s — looking up use sites", moduleName)
			allRefs, err := s.ReferenceNames(moduleName, "__using__", NameReferenceOptions{Context: ctx, ExcludeStdlib: true})
			if err != nil {
				return nil, nil
			}
			locations := nameLocationsToProtocol(allRefs)
			s.debugf("References: returning %d use sites", len(locations))
			return locations, nil
		}
		return nil, nil
	}

	expr := tf.ResolveModuleExpr(cursorCtx.Expr(), lineNum)
	moduleRef, functionName := ExtractModuleAndFunction(expr)

	if moduleRef != "" {
		if aliasParent, inBlock := tf.ExtractAliasBlockParent(lineNum); inBlock {
			moduleRef = aliasParent + "." + moduleRef
		}
	}

	aliases := tf.ExtractAliasesInScope(lineNum)
	s.mergeAliasesFromUseTokenized(tf, aliases)
	s.debugf("References: expr=%q module=%q function=%q", expr, moduleRef, functionName)

	var fullModule string
	var definedInCursorFile bool
	var generatedInjectors []string
	var generatedProvider string
	// Same-file occurrences of a name written in a typespec — see the note
	// below. They carry the bare type uses (`@type t :: schema | embedded`)
	// that the call-shaped scan cannot see.
	var typespecOccurrences []protocol.Location

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
		//
		// A name in a typespec is the exception: `@type schema` and a later
		// `@type t :: schema | embedded` look like a variable and its use, and
		// answering with those two lines alone would hide the real references
		// to the type. There the occurrences are merged with the type's
		// references instead of replacing them — they are the only way to find
		// a bare type use in the same file, and they are the whole answer for
		// a type parameter (the `t` in `@type wrapper(t) :: t`), which has no
		// definition to look up.
		if tree, src, release, ok := s.docs.GetTree(docURI); ok {
			defer release()
			if occs := treesitter.FindVariableOccurrencesWithTree(tree.RootNode(), src, uint(lineNum), uint(col)); len(occs) > 0 {
				var locations []protocol.Location
				for _, occ := range occs {
					locations = append(locations, protocol.Location{
						URI: params.TextDocument.URI,
						Range: protocol.Range{
							Start: s.outPos(lines, int(occ.Line), int(occ.StartCol)),
							End:   s.outPos(lines, int(occ.Line), int(occ.EndCol)),
						},
					})
				}
				if !tf.InTypespec(lineNum) {
					s.debugf("References: returning %d variable occurrences", len(locations))
					return locations, nil
				}
				typespecOccurrences = locations
			}
		}

		// Bare function — resolve to its defining module
		fullModule, definedInCursorFile = s.resolveBareFunctionModuleWithOrigin(uriToPath(protocol.DocumentURI(docURI)), text, tf, lineNum, functionName, aliases)
		s.debugf("References: resolved bare %q -> %q", functionName, fullModule)
		if fullModule == "" {
			currentModule := s.store.LookupEnclosingModule(uriToPath(protocol.DocumentURI(docURI)), lineNum+1)
			if provider, _, found := s.generatedSymbolInScope(currentModule, func() []string {
				return s.enclosingBlockPath(docURI, lineNum, col)
			}, functionName); found {
				fullModule = provider.module
				generatedProvider = provider.module
				for _, useCall := range tf.ExtractUsesWithOpts(aliases) {
					generatedInjectors = append(generatedInjectors, useCall.Module)
				}
				s.debugf("References: generated bare %q provider=%s injectors=%v", functionName, fullModule, generatedInjectors)
			}
		}
		if fullModule == "" {
			s.debugf("References: could not resolve bare function %q", functionName)
			if typespecOccurrences != nil {
				s.debugf("References: returning %d typespec occurrences", len(typespecOccurrences))
				return typespecOccurrences, nil
			}
			return nil, nil
		}
	} else {
		// When cursor is on a defmodule line, use the store's fully-qualified
		// name directly — the user is asking about the module being defined,
		// not a reference that might be shadowed by an alias with the same name.
		if _, isDefmod := IsDefmoduleLine(text, lineNum); isDefmod {
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
	injectors := generatedInjectors
	if functionName != "" && moduleRef == "" && !definedInCursorFile && len(injectors) == 0 {
		useCalls := tf.ExtractUsesWithOpts(aliases)
		for i := len(useCalls) - 1; i >= 0; i-- {
			uc := useCalls[i]
			if s.lookupInUsingEntryFor(uc.Module, functionName, uc.dispatchAtom(), uc.Opts, map[string]bool{}) != nil {
				injectors = append(injectors, uc.Module)
				s.debugf("References: opt-binding injector for %s: %s", functionName, uc.Module)
				break
			}
		}
	}

	kind := NameKindCallable
	if functionName == "" {
		kind = NameKindAny
	} else if tf.InTypespec(lineNum) {
		kind = NameKindType
	}
	semantic, err := s.ReferenceNames(fullModule, functionName, NameReferenceOptions{
		Context:            ctx,
		Kind:               kind,
		FollowDelegates:    s.followDelegates,
		IncludeDeclaration: params.Context.IncludeDeclaration,
		ExcludeStdlib:      true,
		InjectorModules:    injectors,
		GeneratedProvider:  generatedProvider,
	})
	if err != nil {
		s.debugf("References: semantic lookup error: %v", err)
		return nil, nil
	}

	type refKey struct {
		filePath string
		line     int
	}
	seen := make(map[refKey]struct{}, len(semantic))
	var locations []protocol.Location

	// Bare uses of a type in the file being edited, which no indexed ref and
	// no call-shaped scan can report.
	if typespecOccurrences != nil {
		docPath := uriToPath(protocol.DocumentURI(docURI))
		for _, l := range typespecOccurrences {
			line := int(l.Range.Start.Line)
			if line == lineNum && !params.Context.IncludeDeclaration {
				continue
			}
			seen[refKey{docPath, line + 1}] = struct{}{}
			locations = append(locations, l)
		}
	}
	for _, r := range semantic {
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

	s.debugf("References: returning %d locations", len(locations))
	return locations, nil
}

// Rename implements the protocol.Server interface. It exists only to satisfy
// the generated dispatcher; Serve intercepts textDocument/rename before that
// dispatcher runs so the reply can carry resource operations, which
// protocol.WorkspaceEdit cannot express.
func (s *Server) Rename(ctx context.Context, params *protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	edit, err := s.RenameEdit(ctx, params)
	return edit.toProtocol(), err
}

// RenameEdit computes the workspace edit for a textDocument/rename request.
func (s *Server) RenameEdit(ctx context.Context, params *protocol.RenameParams) (*WorkspaceEdit, error) {
	edit, err := s.renameEdit(ctx, params)
	if err != nil || edit == nil {
		return edit, err
	}
	s.useEditorURIs(edit)
	return edit, nil
}

func (s *Server) renameEdit(ctx context.Context, params *protocol.RenameParams) (*WorkspaceEdit, error) {
	docURI := string(params.TextDocument.URI)
	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)
	if lineNum >= len(lines) {
		return nil, nil
	}

	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}
	renameCtx := tf.ExpressionAtCursor(lineNum, col)
	moduleRef, functionName := renameCtx.ModuleRef, renameCtx.FunctionName

	// For bare identifiers, check tree-sitter variables first — a local
	// variable shadows a same-named function in Elixir.
	if moduleRef == "" {
		if tree, src, release, ok := s.docs.GetTree(docURI); ok {
			defer release()
			if occs := treesitter.FindVariableOccurrencesWithTree(tree.RootNode(), src, uint(lineNum), uint(col)); len(occs) > 0 {
				if !isValidVariableName(params.NewName) {
					return nil, fmt.Errorf("invalid variable name %q: it must start with a lowercase letter or _, and have only letters, digits, and _", params.NewName)
				}
				if treesitter.NameExistsInScopeOf(tree.RootNode(), src, uint(lineNum), uint(col), params.NewName) {
					return nil, fmt.Errorf("variable %q already exists in this scope", params.NewName)
				}
				changes := make(map[protocol.DocumentURI][]protocol.TextEdit)
				for _, occ := range occs {
					changes[protocol.DocumentURI(docURI)] = append(changes[protocol.DocumentURI(docURI)], protocol.TextEdit{
						Range: protocol.Range{
							Start: s.outPos(lines, int(occ.Line), int(occ.StartCol)),
							End:   s.outPos(lines, int(occ.Line), int(occ.EndCol)),
						},
						NewText: params.NewName,
					})
				}
				return &WorkspaceEdit{Changes: changes}, nil
			}
		}
	}

	// Try module/function rename via the index
	if !renameCtx.Empty() {
		aliases := tf.ExtractAliasesInScope(lineNum)

		// Detect `as:` aliases — file-local rename of the alias name, not
		// the underlying module. This check runs before merging use-injected
		// aliases because those declarations live in another file's __using__
		// macro and must not be treated as file-local renames.
		if moduleRef != "" && functionName == "" {
			if resolved, ok := aliases[moduleRef]; ok && moduleLastSegment(resolved) != moduleRef {
				if !isValidModuleName(params.NewName) {
					return nil, fmt.Errorf("invalid alias name %q: must start with an uppercase letter", params.NewName)
				}
				// Replace the alias's module tokens in this file, in code and
				// in #{} interpolations; not in strings, comments, or atoms
				// (`:Baz`). A token after a '.' is part of a qualified module
				// name, not the alias.
				changes := make(map[protocol.DocumentURI][]protocol.TextEdit)
				fileURI := protocol.DocumentURI(docURI)
				for _, toks := range [][]parser.Token{tf.tokens, tf.interp} {
					for i, tok := range toks {
						if tok.Kind != parser.TokModule || string(tf.source[tok.Start:tok.End]) != moduleRef {
							continue
						}
						if i > 0 && toks[i-1].Kind == parser.TokDot && toks[i-1].End == tok.Start {
							continue
						}
						line := tok.Line - 1
						col := tok.Start - tf.lineStarts[line]
						changes[fileURI] = append(changes[fileURI], protocol.TextEdit{
							Range: protocol.Range{
								Start: s.outPos(lines, line, col),
								End:   s.outPos(lines, line, col+len(moduleRef)),
							},
							NewText: params.NewName,
						})
					}
				}
				return &WorkspaceEdit{Changes: changes}, nil
			}
		}

		s.mergeAliasesFromUseTokenized(tf, aliases)

		if functionName != "" {
			var fullModule string
			if moduleRef != "" {
				fullModule = resolveModule(moduleRef, aliases)
			} else {
				fullModule = s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, tf, lineNum, functionName, aliases)
			}
			if fullModule != "" {
				if !isValidFunctionName(params.NewName) {
					return nil, fmt.Errorf("invalid function name %q: must match [a-z_][a-z0-9_?!]*", params.NewName)
				}
				// planFunctionRename checks that the new name is free, at the
				// renamed arities.
				return s.renameFunctionEdits(ctx, fullModule, functionName, params.NewName, requesterEditor, nil)
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
				return s.renameModuleEdits(ctx, fullModule, newModule, requesterEditor, nil)
			}
		}
	}

	return nil, nil
}

// renameFunctionEdits builds a WorkspaceEdit renaming all occurrences of
// module.functionName to newName across the codebase.
func (s *Server) renameFunctionEdits(ctx context.Context, module, functionName, newName string, requester renameRequester, report *RenameSummary) (*WorkspaceEdit, error) {
	// See IndexCoordinator.renameSerial.
	end, err := s.beginRename(ctx)
	if err != nil {
		return nil, err
	}
	defer end()
	s.index.renameReport, s.index.renameCtx = report, ctx
	defer func() { s.index.renameReport, s.index.renameCtx = nil, nil }()
	return s.retryWithFreshIndex(ctx, func() (*WorkspaceEdit, error) {
		return s.planFunctionRename(module, functionName, newName, requester)
	})
}

// planFunctionRename is one attempt of renameFunctionEdits; see
// retryWithFreshIndex.
func (s *Server) planFunctionRename(module, functionName, newName string, requester renameRequester) (*WorkspaceEdit, error) {
	// The sites come from the collector that references use too
	// (siteCollector), so a rename changes every line that references list.
	sites := newSiteCollector(s)
	addSiteOpts := sites.add
	source := sites.source
	// The family is the function and, when it is a callback of a project
	// behaviour or implements one, the callback and every implementation;
	// see renameFamily. Every member's definitions and calls are collected.
	family, err := s.renameFamily(module, functionName)
	if err != nil {
		return nil, err
	}
	arities := family.renameArities()
	if family.behaviour != "" {
		// The behaviour must not get two callbacks of one name and arity:
		// it would lose the renamed callback without a warning.
		if cbs, err := s.store.LookupCallbackDef(family.behaviour, newName); err == nil {
			for _, cb := range cbs {
				if family.arities[cb.Arity] {
					return nil, s.renameRefusal(fmt.Sprintf("Dexter: the rename was not applied, because callback %s.%s/%d already exists. No file was changed.",
						family.behaviour, newName, cb.Arity))
				}
			}
		}
	}
	for _, m := range family.modules {
		// In a callback family only the renamed arities can collide:
		// `def go(x)` next to the renamed go/2 is another function.
		if existing, err := s.store.LookupFunction(m, newName); err == nil {
			for _, d := range existing {
				if arities == nil || arities.forModule(m)[d.Arity] {
					return nil, fmt.Errorf("function %s.%s/%d already exists", m, newName, d.Arity)
				}
			}
		}
		if err := sites.collect(m, functionName, nil); err != nil {
			return nil, err
		}
	}
	family.addBehaviourSites(s, source, addSiteOpts)

	plan := s.planTextEdits(sites.sites, family.modules, arities, functionName, newName, requester)
	if plan.err != nil {
		return nil, plan.err
	}
	family.notifyDynamicCalls(s, source)

	// Update defdelegate lines that forward to this function: add or update
	// the `as:` option so the facade keeps working after the rename. The
	// facade changes join the plan before anything is written, so that a
	// facade that cannot be changed stops the whole rename instead of leaving
	// the facade pointing at a function that no longer exists.
	if s.followDelegates {
		var delegates []store.DelegateEntry
		for _, m := range family.modules {
			found, err := s.store.LookupDelegatesTo(m, functionName)
			if err != nil {
				return nil, fmt.Errorf("cannot rename %s.%s: cannot look up the defdelegate facades that forward to it (%v); nothing was renamed", m, functionName, err)
			}
			delegates = append(delegates, found...)
			// The facades in a __using__ quote, which the index does not
			// list: they keep their name, and their as: follows the rename.
			delegates = append(delegates, s.injectedDelegatesTo(m, functionName)...)
		}
		var facadeStale indexStaleness
		for _, del := range delegates {
			if s.isStdlibPath(del.FilePath) {
				continue
			}
			if s.isDepsFile(del.FilePath) {
				continue
			}
			var fileLines []string
			open := false
			if planned, ok := plan.diskWrites[del.FilePath]; ok {
				// The facade also holds call sites, whose edits are already
				// planned. Change the planned text, as a second write after
				// the first one would have.
				fileLines = strings.Split(planned, "\n")
			} else {
				read, ok := s.readFileForRename(del.FilePath, requester)
				if !ok {
					facadeStale.missing = append(facadeStale.missing, del.FilePath)
					continue
				}
				facadeStale.read(del.FilePath, read)
				if _, ok := plan.diskStamps[del.FilePath]; !ok {
					plan.diskStamps[del.FilePath] = read.stamp
				}
				if read.action == actionRefuse {
					plan.unsaved.add(del.FilePath)
					continue
				}
				plan.deny(del.FilePath, read.writeErr)
				if read.held {
					plan.held.add(del.FilePath)
				}
				fileLines = strings.Split(read.text, "\n")
				open = read.action == actionEdit
			}
			startLine := del.Line - 1
			if startLine >= len(fileLines) || !strings.Contains(fileLines[startLine], "defdelegate") ||
				findTokenColumn(fileLines[startLine], del.Function) < 0 {
				// The line the index has is not this facade's defdelegate
				// any more: the file changed since it was indexed.
				facadeStale.moved = append(facadeStale.moved, del.FilePath)
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

			if open {
				fileURI := pathToURI(del.FilePath)
				plan.openChanges[fileURI] = append(plan.openChanges[fileURI], protocol.TextEdit{
					Range: protocol.Range{
						Start: protocol.Position{Line: uint32(spanStart), Character: 0},
						End:   s.outPos(fileLines, spanEnd-1, len(fileLines[spanEnd-1])),
					},
					NewText: strings.Join(updatedSpan, "\n"),
				})
				continue
			}
			// Closed file: splice the updated span into the file's lines. The
			// file is written, and then indexed again, with the others.
			newFileLines := make([]string, 0, len(fileLines)-spanEnd+spanStart+len(updatedSpan))
			newFileLines = append(newFileLines, fileLines[:spanStart]...)
			newFileLines = append(newFileLines, updatedSpan...)
			newFileLines = append(newFileLines, fileLines[spanEnd:]...)
			if _, ok := plan.diskOrig[del.FilePath]; !ok {
				plan.diskOrig[del.FilePath] = strings.Join(fileLines, "\n")
			}
			plan.diskWrites[del.FilePath] = strings.Join(newFileLines, "\n")
		}
		if err := s.checkIndexCurrent(&facadeStale); err != nil {
			return nil, err
		}
	}

	return s.commitTextEdits(plan)
}

// renameModuleEdits builds a WorkspaceEdit renaming oldModule to newModule,
// including all submodules and their references.
//
// Files not currently open in the editor are written directly to disk in
// parallel goroutines. Only open buffers are included in the returned
// WorkspaceEdit, keeping the response small and avoiding editor freezes.
// Files following the naming convention are also renamed/moved: closed ones
// by the server, open ones by the client through rename operations.
func (s *Server) renameModuleEdits(ctx context.Context, oldModule, newModule string, requester renameRequester, report *RenameSummary) (*WorkspaceEdit, error) {
	// See IndexCoordinator.renameSerial.
	end, err := s.beginRename(ctx)
	if err != nil {
		return nil, err
	}
	defer end()
	s.index.renameReport, s.index.renameCtx = report, ctx
	defer func() { s.index.renameReport, s.index.renameCtx = nil, nil }()
	return s.retryWithFreshIndex(ctx, func() (*WorkspaceEdit, error) {
		return s.planModuleRename(oldModule, newModule, requester)
	})
}

// planModuleRename is one attempt of renameModuleEdits; see
// retryWithFreshIndex.
func (s *Server) planModuleRename(oldModule, newModule string, requester renameRequester) (*WorkspaceEdit, error) {
	mr := s.buildModuleRename(oldModule, newModule)
	mr.requester = requester

	// Check for collisions: verify that none of the target module names
	// (including submodules) already exist, and that no destination file
	// paths are occupied.
	if err := mr.checkCollisions(); err != nil {
		return nil, err
	}

	if err := mr.collectSites(); err != nil {
		return nil, err
	}

	fileCache, stale := mr.readFiles()
	mr.files = fileCache
	if err := mr.checkNestedDefs(); err != nil {
		return nil, err
	}
	if err := mr.collectNestedSites(); err != nil {
		return nil, err
	}
	if err := s.checkIndexCurrent(stale); err != nil {
		return nil, err
	}
	if err := s.unsavedErr(&mr.unsaved); err != nil {
		return nil, err
	}
	mr.expandMultiLineGroups(fileCache)

	diskMoves, clientRenames, err := mr.planMoves(fileCache)
	if err != nil {
		return nil, err
	}

	// Every closed file is written in place or moved. Check them all before
	// the first change.
	diskPaths := make([]string, 0, len(fileCache))
	var denied map[string]error
	for fp, fi := range fileCache {
		if !fi.open {
			diskPaths = append(diskPaths, fp)
			if fi.writeErr != nil {
				if denied == nil {
					denied = make(map[string]error)
				}
				denied[fp] = fi.writeErr
			}
		}
	}
	// A move also needs the source directory, to remove the old file, and
	// the destination directory, to make the new one.
	for oldPath, newPath := range diskMoves {
		for _, dir := range []string{filepath.Dir(oldPath), nearestExistingDir(filepath.Dir(newPath))} {
			if err := checkWritable(dir); err != nil {
				if denied == nil {
					denied = make(map[string]error)
				}
				denied[oldPath] = err
			}
		}
	}
	if err := mr.server.checkRenameWrites(diskPaths, denied); err != nil {
		return nil, err
	}

	// An edit that Dexter cannot make stops the rename before any change.
	for fp, sites := range mr.sitesByFile {
		if fi, ok := fileCache[fp]; ok {
			mr.applyEditsToLines(fp, fi.lines, sites)
		}
	}
	if mr.refuse != nil {
		return nil, mr.refuse
	}

	var txn diskTxn
	for _, oldPath := range sortedKeys(diskMoves) {
		lines := fileCache[oldPath].lines
		updated := mr.applyEditsToLines(oldPath, lines, mr.sitesByFile[oldPath])
		txn.move(oldPath, diskMoves[oldPath], mr.moveRoots[oldPath], strings.Join(lines, "\n"), fileCache[oldPath].stamp, strings.Join(updated, "\n"))
	}
	for _, fp := range sortedKeys(mr.sitesByFile) {
		fi, ok := fileCache[fp]
		if _, moves := diskMoves[fp]; !ok || fi.open || moves {
			continue
		}
		txn.write(fp, strings.Join(fi.lines, "\n"), fi.stamp, strings.Join(mr.applyEditsToLines(fp, fi.lines, mr.sitesByFile[fp]), "\n"))
	}
	res, err := s.commitDisk(&txn, mr.requester)
	if err != nil {
		return nil, err
	}
	movedFiles := make(map[string]string, len(diskMoves))
	for _, from := range res.changed {
		if to, ok := diskMoves[from]; ok {
			movedFiles[from] = to
		}
	}
	openChanges := mr.applyEdits(fileCache, movedFiles)
	indexed := mr.reindex(fileCache, movedFiles, clientRenames)
	var held heldFiles
	for fp, fi := range fileCache {
		if fi.held {
			held.add(fp)
		}
	}
	s.notifyHeldFiles(&held)

	if len(clientRenames) == 0 {
		return &WorkspaceEdit{Changes: openChanges, indexed: indexed}, nil
	}
	s.index.recordClientMoves(clientRenames)
	edit := renamesToDocumentChanges(openChanges, clientRenames)
	edit.indexed = indexed
	return edit, nil
}

// renamesToDocumentChanges folds the open buffers' text edits and the file
// moves the client must perform into a single ordered documentChanges list.
//
// Each renamed file's text edits come immediately before its rename
// operation: the client edits the buffer in place and then moves it, so the
// buffer follows the file and no stale copy is left behind to be saved back
// over the rename. Text edits and moves cannot be split across `changes` and
// `documentChanges` because a client that understands documentChanges ignores
// `changes` entirely.
func renamesToDocumentChanges(openChanges map[protocol.DocumentURI][]protocol.TextEdit, clientRenames map[string]string) *WorkspaceEdit {
	renamedPaths := make([]string, 0, len(clientRenames))
	for oldPath := range clientRenames {
		renamedPaths = append(renamedPaths, oldPath)
	}
	sort.Strings(renamedPaths)

	changes := make([]interface{}, 0, len(openChanges)+len(clientRenames))
	renamedURIs := make(map[protocol.DocumentURI]bool, len(clientRenames))
	for _, oldPath := range renamedPaths {
		oldURI := pathToURI(oldPath)
		renamedURIs[oldURI] = true
		if edits := openChanges[oldURI]; len(edits) > 0 {
			changes = append(changes, textDocumentEdit(oldURI, edits))
		}
		changes = append(changes, newRenameFile(oldPath, clientRenames[oldPath]))
	}

	otherURIs := make([]string, 0, len(openChanges))
	for fileURI := range openChanges {
		if !renamedURIs[fileURI] {
			otherURIs = append(otherURIs, string(fileURI))
		}
	}
	sort.Strings(otherURIs)
	for _, u := range otherURIs {
		fileURI := protocol.DocumentURI(u)
		changes = append(changes, textDocumentEdit(fileURI, openChanges[fileURI]))
	}

	return &WorkspaceEdit{DocumentChanges: changes}
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
	err               error // a failed index query; the rename must not go on
	requester         renameRequester
	unsaved           unsavedFiles // headless: files unsaved in an editor; they stop the rename

	// The files as read (readFiles), and what findModuleEdits learns from
	// them.
	files       map[string]moduleFileInfo
	tokenized   map[string]*TokenizedFile
	fileAliases map[string]map[string]string        // a file with one alias scope: aliasesAt
	multiModule map[string]bool                     // a file with more than one defmodule: aliasesAt
	mentions    map[string]bool                     // file + name: keepsShort
	dropped     map[string]map[int]bool             // file: droppedAliasLines
	groupDrops  map[string]map[int]moduleEditResult // file: droppedAliasLines
	codeCols    map[string]map[[2]int]bool          // file: line and column of each module token in code; isCode
	scoped      map[string]bool                     // file: has an alias inside a function body; aliasesAt
	refuse      error                               // an edit that Dexter cannot make; see refusal
	defines     map[string][]string                 // module: its files; definesModule
	moveRoots   map[string]string                   // moved file: the root of its namespace directories; see conventionalNewPath
}

type moduleEditSite struct {
	filePath string
	line     int
	token    string
	columns  []int
}

type moduleFileInfo struct {
	lines    []string
	open     bool      // the change goes into the WorkspaceEdit; see readFileForRename
	held     bool      // another editor has the file open
	writeErr error     // why the closed file cannot be written; see readFileForRename
	stamp    diskStamp // the disk state of the read
}

func (s *Server) buildModuleRename(oldModule, newModule string) *moduleRename {
	moduleRenames := map[string]string{oldModule: newModule}
	submodules, err := s.store.ListSubmodules(oldModule)
	for _, sub := range submodules {
		moduleRenames[sub] = newModule + sub[len(oldModule):]
	}

	tokenReplacements := make(map[string]string, len(moduleRenames))
	for old, newName := range moduleRenames {
		tokenReplacements[old] = newName
	}

	return &moduleRename{
		server:            s,
		oldModule:         oldModule,
		newModule:         newModule,
		moduleRenames:     moduleRenames,
		tokenReplacements: tokenReplacements,
		sitesByFile:       make(map[string][]moduleEditSite),
		err:               err,
	}
}

// checkCollisions verifies that none of the target module names (including
// submodules) already exist at unrelated file paths, and that no conventional
// destination file paths are already occupied on disk.
func (mr *moduleRename) checkCollisions() error {
	// Build the set of source file paths involved in this rename so we can
	// ignore stale index entries at those paths.
	//
	// A failed index query stops the rename. Without the query's answer there
	// is no way to know whether a destination is taken, and a move to a taken
	// path overwrites the file that is there.
	storeErr := func(err error) error {
		return fmt.Errorf("cannot rename %s: the index query failed (%v); nothing was renamed", mr.oldModule, err)
	}
	if mr.err != nil {
		return storeErr(mr.err)
	}
	oldDefs, err := mr.server.store.LookupModulesByPrefix(mr.oldModule)
	if err != nil {
		return storeErr(err)
	}
	ignorePaths := make(map[string]bool, len(oldDefs)*2)
	for _, r := range oldDefs {
		ignorePaths[r.FilePath] = true
	}

	for oldMod, newMod := range mr.moduleRenames {
		// Module name collision: check if newMod already exists outside our rename scope
		existing, err := mr.server.store.LookupModule(newMod)
		if err != nil {
			return storeErr(err)
		}
		for _, r := range existing {
			if !ignorePaths[r.FilePath] {
				return fmt.Errorf("module %s already exists", newMod)
			}
		}

		// File path collision: if the old module follows naming convention,
		// check that the destination path isn't already taken.
		oldDefs, err := mr.server.store.LookupModule(oldMod)
		if err != nil {
			return storeErr(err)
		}
		for _, r := range oldDefs {
			if newPath, _, ok := conventionalNewPath(r.FilePath, oldMod, newMod); ok && newPath != r.FilePath {
				if _, err := os.Lstat(newPath); err == nil {
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
	return mr.server.isStdlibPath(filePath) || mr.server.isDepsFile(filePath)
}

func (mr *moduleRename) collectSites() error {
	sitePositions := make(map[string]int)
	addSite := func(filePath string, line int, token string, columns []int) {
		if mr.isExcluded(filePath) {
			return
		}
		// A site at columns is a short spelling (an injected alias) and
		// stays apart from a site of the full spelling on the same line.
		k := filePath + "\x00" + strconv.Itoa(line) + "\x00" + token + "\x00" + strconv.FormatBool(columns != nil)
		if pos, exists := sitePositions[k]; exists {
			site := &mr.sitesByFile[filePath][pos]
			if site.columns == nil || columns == nil {
				site.columns = nil
				return
			}
			for _, col := range columns {
				if !slices.Contains(site.columns, col) {
					site.columns = append(site.columns, col)
				}
			}
			return
		}
		sitePositions[k] = len(mr.sitesByFile[filePath])
		mr.sitesByFile[filePath] = append(mr.sitesByFile[filePath], moduleEditSite{filePath, line, token, columns})
	}

	// Definition sites. A failed query stops the rename: a rename that
	// changed the definitions but not the references, or the reverse, would
	// leave a project that does not compile.
	allModuleDefs, err := mr.server.store.LookupModulesByPrefix(mr.oldModule)
	if err != nil {
		return fmt.Errorf("cannot rename %s: the index query failed (%v); nothing was renamed", mr.oldModule, err)
	}
	for _, r := range allModuleDefs {
		if _, ok := mr.moduleRenames[r.Module]; ok {
			addSite(r.FilePath, r.Line, r.Module, nil)
		}
	}
	mr.allModuleDefs = allModuleDefs

	// Reference sites
	refs, err := mr.server.store.LookupReferencesByPrefix(mr.oldModule)
	if err != nil {
		return fmt.Errorf("cannot rename %s: the index query failed (%v); nothing was renamed", mr.oldModule, err)
	}
	for _, r := range refs {
		if _, ok := mr.moduleRenames[r.Module]; !ok {
			newMod := mr.newModule + r.Module[len(mr.oldModule):]
			mr.moduleRenames[r.Module] = newMod
			mr.tokenReplacements[r.Module] = newMod
		}
		addSite(r.FilePath, r.Line, r.Module, nil)
	}

	if err := mr.collectImplSites(refs, addSite); err != nil {
		return err
	}

	// Sites reached through an alias injected by a __using__ block. The
	// file holding them has no alias line of its own, so the index has
	// them under the bare short name and the query above cannot see them.
	// They are added under the resolved name, at the columns of the short
	// spelling: findModuleEdits resolves a short spelling with no alias in
	// the file as the injected alias. A site written through an `as:` name
	// needs no edit: the alias line inside the __using__ body carries the
	// rename.
	for _, r := range mr.server.injectedAliasRefs(mr.oldModule, "", refs, false) {
		if _, ok := mr.moduleRenames[r.Module]; !ok {
			newMod := mr.newModule + r.Module[len(mr.oldModule):]
			mr.moduleRenames[r.Module] = newMod
			mr.tokenReplacements[r.Module] = newMod
		}
		if r.written == r.Module || strings.HasSuffix(r.Module, "."+r.written) {
			addSite(r.FilePath, r.Line, r.Module, r.columns)
		}
	}
	return nil
}

func (mr *moduleRename) readFiles() (map[string]moduleFileInfo, *indexStaleness) {
	type fileResult struct {
		path  string
		lines []string
		read  renameRead
	}
	resultsCh := make(chan fileResult, len(mr.sitesByFile))
	for fp := range mr.sitesByFile {
		go func() {
			defer func() {
				if value := recover(); value != nil {
					// The file counts as unread, which stops the rename.
					mr.server.index.reporter.Panicked("request", "a rename file read", value, debug.Stack())
					resultsCh <- fileResult{fp, nil, renameRead{}}
				}
			}()
			read, ok := mr.server.readFileForRename(fp, mr.requester)
			if ok {
				resultsCh <- fileResult{fp, strings.Split(read.text, "\n"), read}
			} else {
				resultsCh <- fileResult{fp, nil, read}
			}
		}()
	}
	fileCache := make(map[string]moduleFileInfo, len(mr.sitesByFile))
	stale := &indexStaleness{}
	for range mr.sitesByFile {
		r := <-resultsCh
		if r.lines == nil {
			// The index lists the file, and it is gone or cannot be read.
			stale.missing = append(stale.missing, r.path)
			continue
		}
		stale.read(r.path, r.read)
		if r.read.action == actionRefuse {
			mr.unsaved.add(r.path)
			continue
		}
		fileCache[r.path] = moduleFileInfo{r.lines, r.read.action == actionEdit, r.read.held, r.read.writeErr, r.read.stamp}
		if r.read.action == actionEdit && !r.read.held {
			// The requesting editor's buffer: every listed line must still
			// spell the module, or the buffer moved since it was indexed.
			for _, site := range mr.sitesByFile[r.path] {
				if site.line-1 >= len(r.lines) || !strings.Contains(r.lines[site.line-1], moduleLastSegment(site.token)) {
					stale.moved = append(stale.moved, r.path)
					break
				}
			}
		}
	}
	return fileCache, stale
}

// expandMultiLineGroups adds the member lines of grouped aliases that span
// lines:
//
//	alias Ecto.Query.{
//	  QueryExpr,
//	  SelectExpr
//	}
//
// The index has the reference on the `alias` line, where the member is not
// written, so renaming a member (not the prefix) must edit the member's own
// line, up to the closing brace.
func (mr *moduleRename) expandMultiLineGroups(fileCache map[string]moduleFileInfo) {
	for fp, sites := range mr.sitesByFile {
		fi, ok := fileCache[fp]
		if !ok {
			continue
		}
		var added []moduleEditSite
		for _, site := range sites {
			dot := strings.LastIndexByte(site.token, '.')
			if dot <= 0 || site.line-1 >= len(fi.lines) {
				continue
			}
			prefix, member := site.token[:dot], site.token[dot+1:]
			newToken, ok := mr.tokenReplacements[site.token]
			if !ok {
				continue
			}
			newDot := strings.LastIndexByte(newToken, '.')
			if newDot <= 0 || newToken[:newDot] != prefix || newToken[newDot+1:] == member {
				continue // the prefix changes, on the alias line itself
			}
			line := fi.lines[site.line-1]
			col, _, _ := findGroupedAlias(line, prefix)
			if col < 0 || strings.IndexByte(line[col:], '}') >= 0 {
				continue
			}
			for k := site.line; k < len(fi.lines); k++ {
				if len(findAllTokenColumns(fi.lines[k], member)) > 0 {
					mr.tokenReplacements[member] = newToken[newDot+1:]
					added = append(added, moduleEditSite{filePath: fp, line: k + 1, token: member})
				}
				if strings.IndexByte(fi.lines[k], '}') >= 0 {
					break
				}
			}
		}
		mr.sitesByFile[fp] = append(mr.sitesByFile[fp], added...)
	}
}

// findModuleEdits finds the spellings of a module on one line and what each
// spelling becomes.
//
// A module is written in full (`Ren.Probe.Mcp1`), or through an alias of the
// module, of a module inside it, or of a namespace above it (`Mcp1`,
// `Mcp1.Child`, `Probe.Mcp1`). The first segment of a short spelling is
// resolved through the aliases in scope at the line (ExtractAliasesInScope):
//
//   - The alias names the renamed module or a module inside it. The alias
//     line spells the full name, so it changes, and the short spelling takes
//     the last segment of the new alias target. When that segment is already
//     a name in the file, the alias line gets `as:` with the old segment, and
//     the short spelling stays. Dexter uses `as:` and not the full name here:
//     `as:` changes one line, and it cannot make a name that the file already
//     uses point to a different module.
//   - The alias names a namespace above the module, which the rename does not
//     change. The short spelling stays when the new name is still in that
//     namespace. If not, the full new name replaces it, and an alias line
//     that then has no use is removed (see droppedAliasLines), because an
//     unused alias is a compiler warning.
//
// A short spelling whose first segment has no alias in the file comes from
// an alias that a __using__ block injects, or from a nested defmodule. It is
// resolved as if an alias names the module that the spelling implies.
//
// A site with columns (an injected alias; see collectSites) is a short
// spelling at those columns, also on a line that spells the name in full.
func (mr *moduleRename) findModuleEdits(fp string, line int, lineText, token string, columns []int) []moduleEditResult {
	newToken, ok := mr.tokenReplacements[token]
	if !ok {
		return nil
	}
	if columns != nil {
		return mr.findShortEdits(fp, line, lineText, token, newToken, false)
	}
	_, full := mr.moduleRenames[token]
	var results []moduleEditResult
	for _, col := range findAllTokenColumns(lineText, token) {
		if col > 0 && lineText[col-1] == '.' && !strings.HasSuffix(lineText[:col], "Elixir.") {
			continue // the end of a longer name
		}
		spelled := newToken
		if full && !strings.HasSuffix(lineText[:col], "Elixir.") {
			// An alias in scope can make the first segment of the new name
			// mean another module (`alias X.Zed`, new name Zed.Bar).
			spelled = fullSpelling(newToken, mr.aliasesAt(fp, line))
		}
		results = append(results, moduleEditResult{col, len(token), spelled + mr.aliasAs(fp, line, lineText, col, token, newToken)})
	}
	if !full {
		// A member line of a multi-line group (see expandMultiLineGroups).
		return results
	}
	self := mr.selfEdits(fp, line, lineText, token, newToken)
	if len(results) == 0 && len(self) == 0 {
		if grouped, found := mr.findGroupedAliasEdits(fp, line, lineText, token, newToken); found {
			return grouped
		}
	}
	// A line can spell the module in full and through an alias too
	// (`{Mod.Full.f(), Full.g()}`): both change. On such a line a short
	// spelling counts only through an alias of the file; one with no alias
	// there (an injected alias) has its own site with columns.
	spelledHere := len(results) > 0 || len(self) > 0
	return mergeEdits(results, self, mr.findShortEdits(fp, line, lineText, token, newToken, spelledHere))
}

// mergeEdits joins edit lists, leaving out an edit that overlaps an earlier
// one.
func mergeEdits(lists ...[]moduleEditResult) []moduleEditResult {
	var out []moduleEditResult
	for _, l := range lists {
		for _, e := range l {
			if !overlapsClaimed(out, e) {
				out = append(out, e)
			}
		}
	}
	return out
}

// selfEdits finds `__MODULE__.Sub` spellings of token on the line, where
// __MODULE__ is the module at the line (`alias __MODULE__.Impl`,
// `__MODULE__.Bar.hi()`). The new spelling keeps `__MODULE__.` when the new
// name is still inside that module, and is the full new name otherwise. When
// the module at the line is itself renamed, __MODULE__ follows by itself.
func (mr *moduleRename) selfEdits(fp string, line int, lineText, token, newToken string) []moduleEditResult {
	const self = "__MODULE__."
	if !strings.Contains(lineText, self) {
		return nil
	}
	enclosing := mr.server.store.LookupEnclosingModule(fp, line)
	if enclosing == "" || !strings.HasPrefix(token, enclosing+".") {
		return nil
	}
	rel := token[len(enclosing)+1:]
	var out []moduleEditResult
	for from := 0; ; {
		i := strings.Index(lineText[from:], self)
		if i < 0 {
			break
		}
		col := from + i
		from = col + len(self)
		name := dottedNameAt(lineText, col+len(self))
		if name != rel && !strings.HasPrefix(name, rel+".") {
			continue
		}
		repl := fullSpelling(newToken, mr.aliasesAt(fp, line))
		if strings.HasPrefix(newToken, enclosing+".") {
			repl = self + newToken[len(enclosing)+1:]
		}
		out = append(out, moduleEditResult{col, len(self) + len(rel), repl})
	}
	return out
}

// aliasAs returns the `as:` option for an alias line that spells token in
// full at col, when the alias must keep its old short name (see keepsShort).
// Otherwise it returns "".
func (mr *moduleRename) aliasAs(fp string, line int, lineText string, col int, token, newToken string) string {
	if _, full := mr.moduleRenames[token]; !full || strings.TrimSpace(lineText[:col]) != "alias" {
		return ""
	}
	if strings.Contains(lineText[col+len(token):], "as:") {
		return ""
	}
	// The options can go on over the next lines (`alias Mod,\n as: M`).
	lines := mr.files[fp].lines
	for l, text := line, lineText; strings.HasSuffix(strings.TrimRight(text, " \t"), ",") && l < len(lines); l++ {
		text = lines[l]
		if strings.Contains(text, "as:") {
			return ""
		}
	}
	short := moduleLastSegment(token)
	if !mr.keepsShort(fp, short, moduleLastSegment(newToken)) {
		return ""
	}
	return ", as: " + short
}

// findShortEdits finds the spellings of token through an alias; see
// findModuleEdits.
func (mr *moduleRename) findShortEdits(fp string, line int, lineText, token, newToken string, aliasOnly bool) []moduleEditResult {
	segs := strings.Split(token, ".")
	var aliases map[string]string
	var results []moduleEditResult
	for first := 1; first < len(segs); first++ {
		written := strings.Join(segs[first:], ".")
		cols := shortColumns(lineText, written)
		if len(cols) == 0 {
			continue
		}
		if aliases == nil {
			aliases = mr.aliasesAt(fp, line)
		}
		head := segs[first]
		target := strings.Join(segs[:first+1], ".")
		inFile := false
		if t, ok := aliases[head]; ok {
			if t != target {
				continue // here the segment names a different module
			}
			inFile = true
		} else if aliasOnly {
			continue
		}
		var repl string
		if enclosing := strings.Join(segs[:first], "."); mr.multiModule[fp] && mr.definesModule(fp, enclosing) && mr.definesModule(fp, target) {
			repl = mr.nestedSpelling(enclosing, newToken, aliases)
		} else {
			repl = mr.shortSpelling(fp, head, target, inFile, written, newToken, aliases)
		}
		if repl == written {
			continue
		}
		for _, col := range cols {
			results = append(results, moduleEditResult{col, len(written), repl})
		}
	}
	if aliases == nil {
		aliases = mr.aliasesAt(fp, line)
	}
	return mergeEdits(results, mr.asAliasEdits(lineText, token, newToken, aliases))
}

// asAliasEdits finds spellings of token through an `as:` alias of a namespace
// above it (`alias A.Foo, as: F` and `F.Bar`): the alias line does not
// change, so the spelling keeps the alias when the new name is still in that
// namespace, and gets the full new name otherwise.
func (mr *moduleRename) asAliasEdits(lineText, token, newToken string, aliases map[string]string) []moduleEditResult {
	var out []moduleEditResult
	for name, target := range aliases {
		if moduleLastSegment(target) == name || !strings.HasPrefix(token, target+".") || mr.renamed(target) != target {
			continue
		}
		written := name + token[len(target):]
		repl := fullSpelling(newToken, aliases)
		if strings.HasPrefix(newToken, target+".") {
			repl = name + newToken[len(target):]
		}
		if repl == written {
			continue
		}
		for _, col := range shortColumns(lineText, written) {
			out = append(out, moduleEditResult{col, len(written), repl})
		}
	}
	return out
}

// nestedSpelling is the new spelling of a module that is defined nested in
// enclosing, in the file of enclosing, where the name is written relative to
// enclosing (`defmodule Tagged`, `%Tagged{}`). The new spelling is relative
// to the new name of enclosing. A module that leaves enclosing is refused
// before (see checkNestedDefs), so the full name is only a fallback.
func (mr *moduleRename) nestedSpelling(enclosing, newToken string, aliases map[string]string) string {
	newEnclosing := mr.renamed(enclosing)
	if strings.HasPrefix(newToken, newEnclosing+".") {
		return newToken[len(newEnclosing)+1:]
	}
	return fullSpelling(newToken, aliases)
}

// renamed returns the new name of mod, or mod when the rename does not
// change it.
func (mr *moduleRename) renamed(mod string) string {
	if mod == mr.oldModule || strings.HasPrefix(mod, mr.oldModule+".") {
		return mr.newModule + mod[len(mr.oldModule):]
	}
	return mod
}

// definesModule reports whether fp holds a defmodule of mod. The files of
// each module are looked up once for the rename.
func (mr *moduleRename) definesModule(fp, mod string) bool {
	if mod == "" {
		return false
	}
	files, ok := mr.defines[mod]
	if !ok {
		if defs, err := mr.server.store.LookupModule(mod); err == nil {
			for _, d := range defs {
				files = append(files, d.FilePath)
			}
		}
		if mr.defines == nil {
			mr.defines = make(map[string][]string)
		}
		mr.defines[mod] = files
	}
	return slices.Contains(files, fp)
}

// checkNestedDefs refuses a rename that moves a module defined nested in
// another module (`defmodule Tagged` inside `defmodule Ecto.Query`) out of
// the namespace of that module. A nested defmodule always defines a module
// inside the enclosing one, so Dexter cannot write the new name there.
func (mr *moduleRename) checkNestedDefs() error {
	for _, r := range mr.allModuleDefs {
		newMod, ok := mr.moduleRenames[r.Module]
		if !ok {
			continue
		}
		fi, read := mr.files[r.FilePath]
		if !read || r.Line-1 >= len(fi.lines) || len(findAllTokenColumns(fi.lines[r.Line-1], r.Module)) > 0 {
			continue // the defmodule line spells the full name
		}
		for dot := strings.LastIndexByte(r.Module, '.'); dot > 0; dot = strings.LastIndexByte(r.Module[:dot], '.') {
			enclosing := r.Module[:dot]
			if !mr.definesModule(r.FilePath, enclosing) {
				continue
			}
			newEnclosing := mr.renamed(enclosing)
			if !strings.HasPrefix(newMod, newEnclosing+".") {
				return fmt.Errorf("cannot rename %s to %s: %s is defined inside %s (%s:%d), and a nested defmodule cannot leave the module it is in. Move it out of %s first; nothing was renamed",
					mr.oldModule, mr.newModule, r.Module, enclosing, mr.server.displayPath(r.FilePath), r.Line, enclosing)
			}
			// The new defmodule line would get a dotted name
			// (`defmodule Deep.Inner`). Elixir puts that module inside
			// the enclosing one too, but the index does not yet: it would
			// lose the module.
			if strings.Count(newMod[len(newEnclosing)+1:], ".") != strings.Count(r.Module[len(enclosing)+1:], ".") {
				return fmt.Errorf("cannot rename %s to %s: %s is defined inside %s (%s:%d), and Dexter can only change the last segment of a nested module. Move it out of %s first; nothing was renamed",
					mr.oldModule, mr.newModule, r.Module, enclosing, mr.server.displayPath(r.FilePath), r.Line, enclosing)
			}
			break
		}
	}
	return nil
}

var defimplProtocol = regexp.MustCompile(`^\s*defimpl\s+([A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)*|__MODULE__)`)

// collectImplSites adds the uses of the implementation modules of a renamed
// module. `defimpl Proto, for: Mod` defines the module Proto.Mod, and code can
// name it (`Queryable.Ecto.SubQuery.to_query()`). When Mod is renamed, that
// module gets the new name, so these uses change with it. The `for:`
// references (kind "impl") give the protocol from their defimpl line.
func (mr *moduleRename) collectImplSites(refs []store.ModuleReferenceResult, addSite func(filePath string, line int, token string, columns []int)) error {
	for _, r := range refs {
		if r.Kind != "impl" {
			continue
		}
		if _, ok := mr.moduleRenames[r.Module]; !ok {
			continue
		}
		text, _, found := mr.server.readFileText(r.FilePath)
		if !found {
			continue
		}
		lines := strings.Split(text, "\n")
		if r.Line-1 >= len(lines) {
			continue
		}
		m := defimplProtocol.FindStringSubmatch(lines[r.Line-1])
		// `for:` can be on a line after the defimpl (`defimpl P,\n for: Mod`).
		for back := r.Line - 2; m == nil && back >= 0 && back >= r.Line-4; back-- {
			if !strings.HasSuffix(strings.TrimRight(lines[back], " \t"), ",") {
				break
			}
			m = defimplProtocol.FindStringSubmatch(lines[back])
		}
		if m == nil {
			continue
		}
		tf := NewTokenizedFile(text)
		proto := parser.ResolveModuleRef(m[1], tf.ExtractAliasesInScope(r.Line-1), mr.server.store.LookupEnclosingModule(r.FilePath, r.Line))
		if proto == "" {
			continue
		}
		impl := proto + "." + r.Module
		uses, err := mr.server.store.LookupReferencesByPrefix(impl)
		if err != nil {
			return fmt.Errorf("cannot rename %s: the index query failed (%v); nothing was renamed", mr.oldModule, err)
		}
		for _, u := range uses {
			mod := u.Module
			if mod != impl && !strings.HasPrefix(mod, impl+".") {
				continue
			}
			newMod := proto + "." + mr.renamed(r.Module) + mod[len(impl):]
			mr.moduleRenames[mod] = newMod
			mr.tokenReplacements[mod] = newMod
			addSite(u.FilePath, u.Line, mod, nil)
		}
	}
	return nil
}

var defmoduleName = regexp.MustCompile(`^\s*defmodule\s+([A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)*)`)

// collectNestedSites adds the uses of a nested module by its short name in
// the file of the enclosing module (`Inner.i()` after `defmodule Inner`
// inside `defmodule Ren.Outer`). The parser does not resolve the alias that
// a nested defmodule makes, so the index has these references under the
// short name, and the query of collectSites cannot see them. A use on a line
// before the defmodule is not one: the alias starts at the defmodule.
func (mr *moduleRename) collectNestedSites() error {
	for _, r := range mr.allModuleDefs {
		if _, ok := mr.moduleRenames[r.Module]; !ok {
			continue
		}
		fi, read := mr.files[r.FilePath]
		if !read || r.Line-1 >= len(fi.lines) {
			continue
		}
		m := defmoduleName.FindStringSubmatch(fi.lines[r.Line-1])
		if m == nil || m[1] == r.Module || !strings.HasSuffix(r.Module, "."+m[1]) {
			continue
		}
		written := m[1]
		enclosing := r.Module[:len(r.Module)-len(written)-1]
		refs, err := mr.server.store.LookupReferencesByPrefix(written)
		if err != nil {
			return fmt.Errorf("cannot rename %s: the index query failed (%v); nothing was renamed", mr.oldModule, err)
		}
		for _, ref := range refs {
			if ref.FilePath != r.FilePath || ref.Line <= r.Line {
				continue
			}
			// The alias that the nested defmodule makes ends with the
			// enclosing module: a sibling module can have its own Inner.
			if at := mr.server.store.LookupEnclosingModule(ref.FilePath, ref.Line); at != enclosing && !strings.HasPrefix(at, enclosing+".") {
				continue
			}
			full := r.Module + ref.Module[len(written):]
			if _, ok := mr.moduleRenames[full]; !ok {
				mr.moduleRenames[full] = mr.renamed(full)
				mr.tokenReplacements[full] = mr.renamed(full)
			}
			mr.sitesByFile[ref.FilePath] = append(mr.sitesByFile[ref.FilePath], moduleEditSite{filePath: ref.FilePath, line: ref.Line, token: full})
		}
	}
	return nil
}

// fullSpelling is the full new name, with `Elixir.` when its first segment
// is an alias in scope.
func fullSpelling(newToken string, aliases map[string]string) string {
	root := newToken
	if dot := strings.IndexByte(root, '.'); dot >= 0 {
		root = root[:dot]
	}
	if t, ok := aliases[root]; ok && t != root {
		return "Elixir." + newToken
	}
	return newToken
}

// shortSpelling is the new spelling of written, a short spelling of a
// renamed module whose first segment head is an alias of target.
func (mr *moduleRename) shortSpelling(fp, head, target string, inFile bool, written, newToken string, aliases map[string]string) string {
	rest := written[len(head):]
	if target == mr.oldModule || strings.HasPrefix(target, mr.oldModule+".") {
		short := moduleLastSegment(mr.newModule + target[len(mr.oldModule):])
		if inFile && (mr.keepsShort(fp, head, short) || mr.hasAsName(fp, head)) {
			short = head
		}
		return short + rest
	}
	// target is a namespace above the renamed module.
	if strings.HasPrefix(newToken, target+".") {
		return head + newToken[len(target):]
	}
	return fullSpelling(newToken, aliases)
}

// shortColumns returns the columns where written starts a module name on the
// line: not inside a longer name, and not a keyword key.
func shortColumns(lineText, written string) []int {
	var cols []int
	for _, col := range findAllTokenColumns(lineText, written) {
		if col > 0 && lineText[col-1] == '.' {
			continue
		}
		if end := col + len(written); end < len(lineText) && lineText[end] == ':' && (end+1 >= len(lineText) || lineText[end+1] != ':') {
			continue
		}
		cols = append(cols, col)
	}
	return cols
}

// keepsShort reports whether an alias whose short name changes from old to
// short must keep old with `as:`: the file already uses short as a name, for
// another alias or module, and a new alias with that name would change what
// those uses mean. The check is on the text, so a match in a comment or a
// string also keeps the old name; that is safe.
func (mr *moduleRename) keepsShort(fp, old, short string) bool {
	if short == old {
		return false
	}
	key := fp + "\x00" + short
	if v, ok := mr.mentions[key]; ok {
		return v
	}
	found := false
	for _, l := range mr.files[fp].lines {
		if len(shortColumns(l, short)) > 0 {
			found = true
			break
		}
	}
	if mr.mentions == nil {
		mr.mentions = make(map[string]bool)
	}
	mr.mentions[key] = found
	return found
}

// hasAsName reports whether the file has an alias with `as: name`. The alias
// line keeps that name when it changes, so the uses keep it too.
func (mr *moduleRename) hasAsName(fp, name string) bool {
	for _, l := range mr.files[fp].lines {
		for rest := l; ; {
			i := strings.Index(rest, "as:")
			if i < 0 {
				break
			}
			rest = rest[i+3:]
			if v := strings.TrimLeft(rest, " \t"); strings.HasPrefix(v, name) && (len(v) == len(name) || !isRenameIdentChar(rune(v[len(name)]))) {
				return true
			}
		}
	}
	return false
}

// aliasesAt returns the aliases in scope at the 1-based line of fp. A file
// with one module (defmodule, defimpl, or defprotocol) has one alias scope,
// so its aliases are read once.
func (mr *moduleRename) aliasesAt(fp string, line int) map[string]string {
	if a, ok := mr.fileAliases[fp]; ok {
		return a
	}
	tf, ok := mr.tokenized[fp]
	if !ok {
		lines := mr.files[fp].lines
		tf = NewTokenizedFile(strings.Join(lines, "\n"))
		if mr.tokenized == nil {
			mr.tokenized = make(map[string]*TokenizedFile)
			mr.fileAliases = make(map[string]map[string]string)
		}
		mr.tokenized[fp] = tf
		modules := 0
		for _, l := range lines {
			if strings.Contains(l, "defmodule") || strings.Contains(l, "defimpl") || strings.Contains(l, "defprotocol") {
				modules++
			}
		}
		if modules > 1 {
			if mr.multiModule == nil {
				mr.multiModule = make(map[string]bool)
			}
			mr.multiModule[fp] = true
		} else if !aliasInBody(tf.tokens) {
			a := tf.ExtractAliases()
			mr.fileAliases[fp] = a
			return a
		}
		if aliasInBody(tf.tokens) {
			if mr.scoped == nil {
				mr.scoped = make(map[string]bool)
			}
			mr.scoped[fp] = true
		}
	}
	if mr.scoped[fp] {
		return aliasesInBodyScope(tf, line)
	}
	return tf.ExtractAliasesInScope(line - 1)
}

// aliasesInBodyScope is ExtractAliasesInScope with the scope of a function
// body: an alias in another function's body does not reach the line. The
// other bodies (each a whole def ... end) are left out of the tokens.
func aliasesInBodyScope(tf *TokenizedFile, line int) map[string]string {
	spans := functionBodySpans(tf.tokens)
	tokens := make([]parser.Token, 0, len(tf.tokens))
	next := 0
	for _, b := range spans {
		end := min(b[1], len(tf.tokens))
		if tf.tokens[b[0]].Line <= line && line <= tf.tokens[end-1].Line {
			continue // the body that holds the line
		}
		tokens = append(tokens, tf.tokens[next:b[0]]...)
		next = end
	}
	tokens = append(tokens, tf.tokens[next:]...)
	return extractAliasesFromTokens(tf.source, tokens, line-1)
}

// aliasInBody reports whether an alias statement is inside a function body,
// where its scope is that body and not the whole module.
func aliasInBody(tokens []parser.Token) bool {
	for _, b := range functionBodySpans(tokens) {
		for i := b[0]; i < b[1] && i < len(tokens); i++ {
			if tokens[i].Kind == parser.TokAlias {
				return true
			}
		}
	}
	return false
}

// isCode reports whether a module token starts at the 1-based line and
// column of fp in code (also in a #{} interpolation), not in a string, a
// charlist, a sigil, or a comment.
func (mr *moduleRename) isCode(fp string, line, col int) bool {
	cols, ok := mr.codeCols[fp]
	if !ok {
		cols = make(map[[2]int]bool)
		src := []byte(strings.Join(mr.files[fp].lines, "\n"))
		res := parser.TokenizeFull(src)
		for _, toks := range [][]parser.Token{res.Tokens, res.Interp} {
			for _, t := range toks {
				if t.Kind == parser.TokModule && t.Line-1 < len(res.LineStarts) {
					cols[[2]int{t.Line, t.Start - res.LineStarts[t.Line-1]}] = true
				}
			}
		}
		if mr.codeCols == nil {
			mr.codeCols = make(map[string]map[[2]int]bool)
		}
		mr.codeCols[fp] = cols
	}
	return cols[[2]int{line, col}]
}

var simpleAliasLine = regexp.MustCompile(`^\s*alias\s+([A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)*)\s*$`)

var groupAliasLine = regexp.MustCompile(`^\s*alias\s+([A-Z][A-Za-z0-9_]*(?:\.[A-Z][A-Za-z0-9_]*)*)\.\{([^}]*)\}\s*$`)

// droppedAliasLines returns the 1-based lines of fp that hold an alias of a
// namespace above the renamed module that has no use after the rename:
// every use spells a module that moves out of that namespace, and gets the
// full new name (see findModuleEdits). The rename removes these lines,
// because an unused alias is a compiler warning. Only a line that holds the
// alias and nothing else is removed. In a one-line group (`alias A.{Foo,
// Svc}`), such a member leaves the group (see groupDrops); a group with no
// member left is removed.
func (mr *moduleRename) droppedAliasLines(fp string, lines []string) map[int]bool {
	if v, ok := mr.dropped[fp]; ok {
		return v
	}
	// unused reports whether an alias of target (head is its short name), on
	// line i, has only uses that move out of target after the rename.
	unused := func(i int, target string) bool {
		if !strings.HasPrefix(mr.oldModule, target+".") || strings.HasPrefix(mr.newModule, target+".") {
			return false
		}
		head := moduleLastSegment(target)
		moved, other := 0, 0
		for j, o := range lines {
			if j == i {
				continue
			}
			for _, col := range shortColumns(o, head) {
				if !mr.isCode(fp, j+1, col) {
					continue // a comment or a string is not a use
				}
				full := target + dottedNameAt(o, col)[len(head):]
				if full == mr.oldModule || strings.HasPrefix(full, mr.oldModule+".") {
					moved++
				} else {
					other++
				}
			}
		}
		return moved > 0 && other == 0
	}
	var drop map[int]bool
	var members map[int]moduleEditResult
	for i, l := range lines {
		if m := simpleAliasLine.FindStringSubmatch(l); m != nil {
			if unused(i, m[1]) {
				if drop == nil {
					drop = make(map[int]bool)
				}
				drop[i+1] = true
			}
			continue
		}
		m := groupAliasLine.FindStringSubmatchIndex(l)
		if m == nil {
			continue
		}
		prefix := l[m[2]:m[3]]
		var keep []string
		removed := false
		for _, member := range strings.Split(l[m[4]:m[5]], ",") {
			member = strings.TrimSpace(member)
			if member == "" {
				continue
			}
			if !strings.Contains(member, ".") && unused(i, prefix+"."+member) {
				removed = true
				continue
			}
			keep = append(keep, member)
		}
		switch {
		case !removed:
		case len(keep) == 0:
			if drop == nil {
				drop = make(map[int]bool)
			}
			drop[i+1] = true
		default:
			if members == nil {
				members = make(map[int]moduleEditResult)
			}
			members[i+1] = moduleEditResult{m[4], m[5] - m[4], strings.Join(keep, ", ")}
		}
	}
	if mr.dropped == nil {
		mr.dropped = make(map[string]map[int]bool)
		mr.groupDrops = make(map[string]map[int]moduleEditResult)
	}
	mr.dropped[fp] = drop
	mr.groupDrops[fp] = members
	return drop
}

// lineEdits returns the edits of each 1-based line of fp: those of its sites
// (in code only, with no two that overlap) and the group members that leave
// their group (see droppedAliasLines). Every edit is relative to the line
// as read.
func (mr *moduleRename) lineEdits(fp string, lines []string, sites []moduleEditSite) map[int][]moduleEditResult {
	claimed := make(map[int][]moduleEditResult)
	for _, es := range sites {
		if es.line-1 >= len(lines) {
			continue
		}
		for _, edit := range mr.editsForSite(lines[es.line-1], es) {
			if !mr.isCode(fp, es.line, edit.col) {
				continue // the name in a string or a comment stays
			}
			if !overlapsClaimed(claimed[es.line], edit) {
				claimed[es.line] = append(claimed[es.line], edit)
			}
		}
	}
	if len(sites) > 0 {
		mr.droppedAliasLines(fp, lines)
		for line, edit := range mr.groupDrops[fp] {
			if !overlapsClaimed(claimed[line], edit) {
				claimed[line] = append(claimed[line], edit)
			}
		}
	}
	return claimed
}

// dottedNameAt returns the module name that starts at col: segments that
// start with an upper-case letter, joined by dots.
func dottedNameAt(s string, col int) string {
	end := col
	for end < len(s) {
		c := s[end]
		switch {
		case c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			end++
		case c == '.' && end+1 < len(s) && s[end+1] >= 'A' && s[end+1] <= 'Z':
			end++
		default:
			return s[col:end]
		}
	}
	return s[col:end]
}

// findGroupedAliasEdits handles `alias Prefix.{A, B}` (and the require/import
// forms), where the module name is written once as the prefix and each member
// is indexed as its own reference — so the reference's full name never appears
// on the line. A member can have more than one segment
// (`alias Prefix.{Deep.Mod, B}`). found is false when the line holds no group
// for token.
//
// Which half moves depends on the rename. When the renamed module is the
// prefix or one of its ancestors, every member moves with it, so the prefix is
// rewritten; this is also correct for a group whose members continue on the
// next lines, where the index records every member on the opening line. Sites
// for the other members on the same line find nothing once the prefix is
// rewritten, so a group is only edited once. When only the member is renamed,
// the member is rewritten inside the braces, or, when it moves to another
// namespace, it leaves the group and gets its own alias. A member whose new
// short name is already a name in the file (see keepsShort) also leaves the
// group, with `as:` and its old short name.
func (mr *moduleRename) findGroupedAliasEdits(fp string, line int, lineText, token, newToken string) (results []moduleEditResult, found bool) {
	for dot := strings.LastIndexByte(token, '.'); dot > 0; dot = strings.LastIndexByte(token[:dot], '.') {
		prefix, member := token[:dot], token[dot+1:]
		prefixCol, groupStart, groupEnd := findGroupedAlias(lineText, prefix)
		if prefixCol < 0 {
			continue
		}
		return mr.groupEdits(fp, line, lineText, prefix, member, newToken, prefixCol, groupStart, groupEnd), true
	}
	return nil, false
}

func (mr *moduleRename) groupEdits(fp string, line int, lineText, prefix, member, newToken string, prefixCol, groupStart, groupEnd int) []moduleEditResult {
	if prefix == mr.oldModule || strings.HasPrefix(prefix, mr.oldModule+".") {
		newPrefix := mr.newModule + prefix[len(mr.oldModule):]
		if newPrefix == prefix {
			return nil
		}
		return []moduleEditResult{{prefixCol, len(prefix), newPrefix}}
	}
	if !strings.Contains(newToken, ".") {
		// The member lost its namespace; a grouped alias cannot express that.
		return nil
	}
	short := moduleLastSegment(member)
	keep := strings.TrimSpace(lineText[:prefixCol]) == "alias" && mr.keepsShort(fp, short, moduleLastSegment(newToken))
	// The member stays in the group when it keeps its prefix and its number
	// of segments, as on main; otherwise it leaves the group.
	newMember := strings.TrimPrefix(newToken, prefix+".")
	stays := newMember != newToken && !keep && strings.Count(newMember, ".") == strings.Count(member, ".")
	multiLine := groupEnd >= len(lineText) || lineText[groupEnd] != '}'
	if multiLine && !stays {
		// The members go on over the next lines; Dexter cannot move one out.
		if mr.refuse == nil {
			mr.refuse = fmt.Errorf("cannot rename %s to %s: %s is in the alias group at %s:%d, which goes over several lines, and it would have to leave the group. Put the group on one line, or give %s its own alias line, first; nothing was renamed",
				mr.oldModule, mr.newModule, prefix+"."+member, mr.server.displayPath(fp), line, prefix+"."+member)
		}
		return nil
	}
	memberCols := shortColumns(lineText[groupStart:groupEnd], member)
	if len(memberCols) == 0 {
		return nil // a multi-line group: the member's own line changes
	}
	if stays {
		if newMember == member {
			return nil
		}
		results := make([]moduleEditResult, 0, len(memberCols))
		for _, col := range memberCols {
			results = append(results, moduleEditResult{groupStart + col, len(member), newMember})
		}
		return results
	}
	return mr.splitGroupedAlias(fp, lineText, prefix, prefixCol, groupStart, groupEnd)
}

// splitGroupedAlias moves the members of `Prefix.{A, B}` that the rename
// changes and that cannot stay in the group to their own alias lines after
// the group (`alias New.A`), because only they change namespace. Rewriting
// the shared prefix instead would move the other members too. Every site on
// the line gets the same edit, so a group with two moved members (`{Bar,
// Bar.Sub}`) is split once. A group with no other member gets the new names
// in place. A member whose new short name is already a name in the file
// keeps its old one with `as:` (see keepsShort).
func (mr *moduleRename) splitGroupedAlias(fp, lineText, prefix string, prefixCol, groupStart, groupEnd int) []moduleEditResult {
	alias := strings.TrimSpace(lineText[:prefixCol]) == "alias"
	var rest, moved []string
	for _, m := range strings.Split(lineText[groupStart:groupEnd], ",") {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		full := prefix + "." + m
		newFull := mr.renamed(full)
		if newMember := strings.TrimPrefix(newFull, prefix+"."); newFull == full ||
			(newMember != newFull && strings.Count(newMember, ".") == strings.Count(m, ".") && (!alias || !mr.keepsShort(fp, moduleLastSegment(m), moduleLastSegment(newFull)))) {
			// It stays, renamed in place if the rename changes it.
			if newFull != full {
				m = newMember
			}
			rest = append(rest, m)
			continue
		}
		if alias && mr.keepsShort(fp, moduleLastSegment(m), moduleLastSegment(newFull)) {
			newFull += ", as: " + moduleLastSegment(m)
		}
		moved = append(moved, newFull)
	}
	span := groupEnd + 1 - prefixCol
	// A new line repeats what comes before the prefix: the indentation and
	// the alias, require, or import keyword.
	lead := lineText[:prefixCol]
	if len(rest) == 0 {
		return []moduleEditResult{{prefixCol, span, strings.Join(moved, "\n"+lead)}}
	}
	replacement := prefix + ".{" + strings.Join(rest, ", ") + "}"
	for _, m := range moved {
		replacement += "\n" + lead + m
	}
	return []moduleEditResult{{prefixCol, span, replacement}}
}

type moduleEditResult struct {
	col      int
	length   int
	newToken string
}

func (mr *moduleRename) editsForSite(lineText string, site moduleEditSite) []moduleEditResult {
	edits := mr.findModuleEdits(site.filePath, site.line, lineText, site.token, site.columns)
	if site.columns == nil {
		return edits
	}
	filtered := edits[:0]
	for _, edit := range edits {
		if slices.Contains(site.columns, edit.col) {
			filtered = append(filtered, edit)
		}
	}
	return filtered
}

func (mr *moduleRename) applyEditsToLines(fp string, lines []string, sites []moduleEditSite) []string {
	result := make([]string, len(lines))
	copy(result, lines)
	claimed := mr.lineEdits(fp, lines, sites)
	for line, edits := range claimed {
		sort.Slice(edits, func(i, j int) bool { return edits[i].col > edits[j].col })
		lineText := result[line-1]
		for _, edit := range edits {
			lineText = lineText[:edit.col] + edit.newToken + lineText[edit.col+edit.length:]
		}
		result[line-1] = lineText
	}
	if len(sites) == 0 {
		return result
	}
	drop := mr.droppedAliasLines(fp, lines)
	if len(drop) == 0 {
		return result
	}
	kept := result[:0]
	for i, l := range result {
		if !drop[i+1] {
			kept = append(kept, l)
		}
	}
	return kept
}

// conventionalNewPath is where the file of a module that the rename changes
// goes; see conventionalNewPath. A submodule in its own file
// (lib/ren/probe/mcp1/child.ex) moves with its namespace.
func (mr *moduleRename) conventionalNewPath(r store.LookupResult) (newPath, root string, ok bool) {
	newMod, renamed := mr.moduleRenames[r.Module]
	if !renamed {
		return "", "", false
	}
	return conventionalNewPath(r.FilePath, r.Module, newMod)
}

// planMoves works out which files that follow the naming convention move,
// and who moves each one. Nothing changes on disk here.
//
// Files open in the editor are NOT moved by the server when the client can
// apply rename resource operations: the client owns the buffer, so it must
// move the file itself (see renamesToDocumentChanges). Moving it behind the
// client's back leaves the editor with a modified buffer pointing at a deleted
// path, and saving that buffer recreates the old file with the new module
// name.
//
// diskMoves are the closed files the server moves; clientRenames are the open
// files the client moves.
func (mr *moduleRename) planMoves(fileCache map[string]moduleFileInfo) (diskMoves, clientRenames map[string]string, err error) {
	diskMoves = make(map[string]string)
	clientRenames = make(map[string]string)
	var openElsewhere []string
	defer func() {
		if len(openElsewhere) > 0 {
			diskMoves, clientRenames, err = nil, nil, mr.server.openMoveErr(openElsewhere)
		}
	}()
	for _, r := range mr.allModuleDefs {
		if _, ok := mr.moduleRenames[r.Module]; !ok {
			continue
		}
		newPath, root, follows := mr.conventionalNewPath(r)
		if !follows {
			continue
		}
		fi, hasContent := fileCache[r.FilePath]
		if !hasContent {
			continue
		}

		// A rename can leave the conventional path alone: two names that
		// snake_case alike (ABTest → AbTest), or a namespace move of a file
		// whose directories do not spell the namespace. There is nothing to
		// move. Writing the file and then removing the old path would
		// remove the file just written, and handing the client a rename
		// operation from a path to itself is no better. Fall through to
		// applyEdits, which rewrites the contents where they are.
		if newPath == r.FilePath {
			continue
		}

		others := mr.server.index.openDocs.heldTexts(mr.server.docs, r.FilePath)
		if renameFileAction(mr.requester, fi.open && !fi.held, others, "", true) == actionRefuseMove {
			openElsewhere = append(openElsewhere, r.FilePath)
			continue
		}

		if fi.open {
			// Client applies rename operations: leave both paths untouched.
			// applyEdits still emits TextEdits for the old URI, and the rename
			// operation queued after them carries the edited buffer to the new
			// path.
			if mr.server.renameFileOpsSupported {
				mr.server.debugf("Rename: %s → %s (client-applied)", r.FilePath, newPath)
				clientRenames[r.FilePath] = newPath
				continue
			}
			// Client cannot move the file and we must not do it behind its
			// back: deleting a path the editor still has open leaves a buffer
			// that recreates the file on the next save. Rename the contents in
			// place and leave the file where it is.
			log.Printf("Rename: leaving %s in place — client cannot apply rename operations and the file is open", r.FilePath)
			continue
		}
		diskMoves[r.FilePath] = newPath
		if mr.moveRoots == nil {
			mr.moveRoots = make(map[string]string)
		}
		mr.moveRoots[r.FilePath] = root
	}
	return diskMoves, clientRenames, nil
}

// applyEdits builds the TextEdits for the files whose change goes into the
// WorkspaceEdit. renameModuleEdits writes the closed files.
func (mr *moduleRename) applyEdits(fileCache map[string]moduleFileInfo, movedFiles map[string]string) map[protocol.DocumentURI][]protocol.TextEdit {
	openChanges := make(map[protocol.DocumentURI][]protocol.TextEdit)

	for fp, sites := range mr.sitesByFile {
		if _, moved := movedFiles[fp]; moved {
			continue
		}
		fi, ok := fileCache[fp]
		if !ok {
			continue
		}
		if fi.open {
			fileURI := pathToURI(fp)
			// Each site is matched against the original line, so two sites can
			// resolve to the same span — `alias Old.{A, B}` is one reference
			// per member but a single edit to the shared prefix. The on-disk
			// path rewrites the line as it goes and never sees the second
			// match; TextEdits are all relative to the original text, so
			// overlapping ones have to be dropped here or the editor applies
			// the replacement twice.
			claimed := mr.lineEdits(fp, fi.lines, sites)
			for _, line := range slices.Sorted(maps.Keys(claimed)) {
				for _, e := range claimed[line] {
					openChanges[fileURI] = append(openChanges[fileURI], protocol.TextEdit{
						Range: protocol.Range{
							Start: mr.server.outPos(fi.lines, line-1, e.col),
							End:   mr.server.outPos(fi.lines, line-1, e.col+e.length),
						},
						NewText: e.newToken,
					})
				}
			}
			if len(sites) == 0 {
				continue
			}
			// An alias line with no use left goes, with its line break.
			for _, line := range slices.Sorted(maps.Keys(mr.droppedAliasLines(fp, fi.lines))) {
				var end protocol.Position
				if line < len(fi.lines) {
					end = mr.server.outPos(fi.lines, line, 0)
				} else {
					end = mr.server.outPos(fi.lines, line-1, len(fi.lines[line-1]))
				}
				openChanges[fileURI] = append(openChanges[fileURI], protocol.TextEdit{
					Range:   protocol.Range{Start: mr.server.outPos(fi.lines, line-1, 0), End: end},
					NewText: "",
				})
			}
		}
	}
	return openChanges
}

// overlapsClaimed reports whether e covers any column already taken by an
// edit on the same line.
func overlapsClaimed(claimed []moduleEditResult, e moduleEditResult) bool {
	for _, c := range claimed {
		if e.col < c.col+c.length && c.col < e.col+e.length {
			return true
		}
	}
	return false
}

// reindex re-parses all touched files asynchronously after the rename.
//
// movedFiles were moved on disk by the server, so their new paths are read
// back from disk. clientRenames have not moved yet — the client applies them
// when it receives the reply — so their new paths are indexed from the text
// the edits produce.
func (mr *moduleRename) reindex(fileCache map[string]moduleFileInfo, movedFiles, clientRenames map[string]string) <-chan struct{} {
	removePaths := make([]string, 0, len(movedFiles)+len(clientRenames))
	for oldPath := range movedFiles {
		removePaths = append(removePaths, oldPath)
	}
	// A file that the editor moves keeps its rows at the old path until the
	// watcher sees the move. A rename before that finds the old path gone
	// and reports the index as behind (checkIndexCurrent), instead of
	// planning without the file: removing the rows now would hide its
	// references from that rename.

	var reindexPaths []string
	for _, newPath := range movedFiles {
		reindexPaths = append(reindexPaths, newPath)
	}

	var openReindexes []textReindex
	for fp := range mr.sitesByFile {
		if _, moved := movedFiles[fp]; moved {
			continue
		}
		fi, ok := fileCache[fp]
		if !ok || fi.held {
			// A held file changes only when this editor saves it; see
			// readFileForRename.
			continue
		}
		updatedLines := mr.applyEditsToLines(fp, fi.lines, mr.sitesByFile[fp])
		updatedText := strings.Join(updatedLines, "\n")
		if newPath, moved := clientRenames[fp]; moved {
			// Open file the client is about to move: index at the new path
			openReindexes = append(openReindexes, textReindex{newPath, updatedText})
		} else if fi.open {
			openReindexes = append(openReindexes, textReindex{fp, updatedText})
		} else {
			reindexPaths = append(reindexPaths, fp)
		}
	}

	// Also index client-renamed files that had no edit sites (e.g. the file
	// only contained the defmodule line which is already in allModuleDefs)
	for oldPath, newPath := range clientRenames {
		if _, hasSites := mr.sitesByFile[oldPath]; hasSites {
			continue
		}
		if fi, ok := fileCache[oldPath]; ok && !fi.held {
			updatedLines := mr.applyEditsToLines(oldPath, fi.lines, nil)
			openReindexes = append(openReindexes, textReindex{newPath, strings.Join(updatedLines, "\n")})
		}
	}

	return mr.server.reindexAfterRename(removePaths, reindexPaths, openReindexes)
}

type renameSite struct {
	filePath       string
	line           int
	includeKeyword bool // true for import-only lines where keyword keys ARE function names
	// fromIndex is true when the index lists a reference or definition of
	// the function on this line, so the line must hold the token.
	fromIndex bool
	// bareOK is true when an unqualified call on this line resolves to the
	// renamed function: the line is inside its module, or the file imports
	// the module (directly or through a __using__ block).
	bareOK bool
	// dynamicOK is true for a call through a variable (`impl.name(...)`) in
	// a behaviour's own file; see renameFamilyInfo.
	dynamicOK bool
	// module is the family member whose collection added the site: the
	// module of an unqualified call on the line, for the arity check.
	module string
	// kind is why the line is a site (siteDefinition, siteCall, ...).
	kind string
	// localArities, when set, are the arities of the name that the module
	// at the line defines itself: an unqualified occurrence with one of
	// them, or whose arity cannot be read, is that module's own function.
	localArities map[int]bool
	// ownArities, when set, are the arities that the renamed module defines
	// of the name, for a line in the module itself: an unqualified call of
	// another arity is a function that the module imports, and stays. A
	// call whose arity cannot be read changes.
	ownArities map[int]bool
	// delegateAs, when set, is the name a defdelegate on this line forwards
	// to without `as:`: the renamed facade itself. The rename adds
	// `as: :<delegateAs>` so that it still forwards to that function.
	delegateAs string
}

type textReindex struct {
	path string
	text string
}

// textEditPlan is a text rename worked out in memory. Nothing is written
// until commitTextEdits, so every check can refuse the rename while the files
// are still untouched.
type textEditPlan struct {
	openChanges   map[protocol.DocumentURI][]protocol.TextEdit // buffers this editor has open
	openReindexes []textReindex                                // their text after the edits
	diskWrites    map[string]string                            // closed file → new content
	diskOrig      map[string]string                            // closed file → the content the plan read
	denied        map[string]error                             // closed file → why it cannot be written
	held          heldFiles                                    // open in another editor; in openChanges
	diskStamps    map[string]diskStamp                         // closed file → its disk state at the read
	err           error                                        // the plan cannot be applied; see checkIndexCurrent
	unsaved       unsavedFiles                                 // headless: unsaved in an editor; stops the rename
	requester     renameRequester
}

// deny records that path cannot be written. A nil err records nothing.
func (p *textEditPlan) deny(path string, err error) {
	if err == nil {
		return
	}
	if p.denied == nil {
		p.denied = make(map[string]error)
	}
	p.denied[path] = err
}

// planTextEdits works out a token rename over sites without changing any
// file.
func (s *Server) planTextEdits(sites []renameSite, modules []string, arities *arityFilter, oldToken, newToken string, requester renameRequester) *textEditPlan {
	// Group sites by file
	sitesByFile := make(map[string][]renameSite, len(sites))
	for _, site := range sites {
		sitesByFile[site.filePath] = append(sitesByFile[site.filePath], site)
	}

	// Read all files in parallel, tracking whether each is open in the editor
	type fileResult struct {
		path  string
		lines []string
		read  renameRead
	}
	resultsCh := make(chan fileResult, len(sitesByFile))
	for fp := range sitesByFile {
		go func() {
			defer func() {
				if value := recover(); value != nil {
					// The file counts as unread, which stops the rename.
					s.index.reporter.Panicked("request", "a rename file read", value, debug.Stack())
					resultsCh <- fileResult{fp, nil, renameRead{}}
				}
			}()
			read, ok := s.readFileForRename(fp, requester)
			if ok {
				resultsCh <- fileResult{fp, strings.Split(read.text, "\n"), read}
			} else {
				resultsCh <- fileResult{fp, nil, read}
			}
		}()
	}
	type fileInfo struct {
		lines []string
		open  bool // the change goes into the WorkspaceEdit
		held  bool // another editor has the file open
		site  *siteFile
	}
	fileCache := make(map[string]fileInfo, len(sitesByFile))
	plan := &textEditPlan{
		openChanges: make(map[protocol.DocumentURI][]protocol.TextEdit),
		diskWrites:  make(map[string]string),
		diskOrig:    make(map[string]string),
		diskStamps:  make(map[string]diskStamp),
		requester:   requester,
	}
	var stale indexStaleness
	for range sitesByFile {
		r := <-resultsCh
		if r.lines == nil {
			// The index lists the file, and it is gone or cannot be read.
			stale.missing = append(stale.missing, r.path)
			continue
		}
		stale.read(r.path, r.read)
		if r.read.action == actionRefuse {
			plan.unsaved.add(r.path)
			continue
		}
		fileCache[r.path] = fileInfo{r.lines, r.read.action == actionEdit, r.read.held, &siteFile{path: r.path, lines: r.lines, modules: modules, name: oldToken, arities: arities}}
		plan.diskStamps[r.path] = r.read.stamp
		plan.deny(r.path, r.read.writeErr)
		if r.read.held {
			plan.held.add(r.path)
		}
	}

	// A line of the requesting editor's buffer that the index lists must
	// still hold the token: else the buffer moved since the index read it,
	// and an edit there would change the wrong place.
	for fp, fileSites := range sitesByFile {
		fi, ok := fileCache[fp]
		if !ok || !fi.open || fi.held {
			continue
		}
		for _, site := range fileSites {
			if site.fromIndex && (site.line-1 >= len(fi.lines) || len(findAllTokenColumns(fi.lines[site.line-1], oldToken)) == 0) {
				stale.moved = append(stale.moved, fp)
				break
			}
		}
	}
	if err := s.checkIndexCurrent(&stale); err != nil {
		plan.err = err
		return plan
	}

	// applyTokenEdits applies right-to-left token replacements for the given
	// sites, returning the updated lines. Shared by open and closed file paths.
	applyTokenEdits := func(sf *siteFile, origLines []string, fileSites []renameSite) []string {
		lines := make([]string, len(origLines))
		copy(lines, origLines)

		for _, site := range fileSites {
			if site.line-1 >= len(lines) {
				continue
			}
			lineText := lines[site.line-1]
			cols := s.siteColumns(sf, site, lineText, oldToken)
			for i := len(cols) - 1; i >= 0; i-- {
				lineText = lineText[:cols[i]] + newToken + lineText[cols[i]+len(oldToken):]
			}
			lines[site.line-1] = lineText
		}
		for _, site := range fileSites {
			if end, ok := delegateAsLine(lines, site); ok {
				lines[end] = strings.TrimRight(lines[end], " \t\r") + ", as: :" + site.delegateAs
			}
		}
		return lines
	}

	for fp, fileSites := range sitesByFile {
		fi, ok := fileCache[fp]
		if !ok {
			continue
		}

		// Compute edits once for both TextEdits and reindexing
		updatedLines := applyTokenEdits(fi.site, fi.lines, fileSites)

		if fi.open {
			// Open buffer: build TextEdits for the editor AND capture updated
			// text for reindexing (computed once, used for both purposes).
			fileURI := pathToURI(fp)
			for _, site := range fileSites {
				if site.line-1 >= len(fi.lines) {
					continue
				}
				lineText := fi.lines[site.line-1]
				for _, col := range s.siteColumns(fi.site, site, lineText, oldToken) {
					plan.openChanges[fileURI] = append(plan.openChanges[fileURI], protocol.TextEdit{
						Range: protocol.Range{
							Start: s.outPos(fi.lines, site.line-1, col),
							End:   s.outPos(fi.lines, site.line-1, col+len(oldToken)),
						},
						NewText: newToken,
					})
				}
				if end, ok := delegateAsLine(fi.lines, site); ok {
					at := s.outPos(fi.lines, end, len(strings.TrimRight(fi.lines[end], " \t\r")))
					plan.openChanges[fileURI] = append(plan.openChanges[fileURI], protocol.TextEdit{
						Range:   protocol.Range{Start: at, End: at},
						NewText: ", as: :" + site.delegateAs,
					})
				}
			}
			// A held file is not indexed now: neither the disk nor any
			// buffer of this editor has the new text yet. The editor's
			// didChange and didSave bring it.
			if !fi.held {
				plan.openReindexes = append(plan.openReindexes, textReindex{fp, strings.Join(updatedLines, "\n")})
			}
		} else {
			plan.diskWrites[fp] = strings.Join(updatedLines, "\n")
			plan.diskOrig[fp] = strings.Join(fi.lines, "\n")
		}
	}
	return plan
}

// delegateAsLine returns the 0-based last line of the defdelegate at a site
// that needs `as:` (see renameSite.delegateAs).
func delegateAsLine(lines []string, site renameSite) (int, bool) {
	if site.delegateAs == "" || site.line-1 >= len(lines) || !strings.Contains(lines[site.line-1], "defdelegate") {
		return 0, false
	}
	_, end := delegateStatementSpan(lines, site.line-1)
	return end - 1, true
}

// commitTextEdits checks that every planned disk write is safe, writes the
// closed files in parallel, and indexes the changed files again. It writes
// nothing when a check fails.
func (s *Server) commitTextEdits(plan *textEditPlan) (*WorkspaceEdit, error) {
	if plan.err != nil {
		return nil, plan.err
	}
	paths := make([]string, 0, len(plan.diskWrites))
	for fp := range plan.diskWrites {
		paths = append(paths, fp)
	}
	if err := s.unsavedErr(&plan.unsaved); err != nil {
		return nil, err
	}
	if err := s.checkRenameWrites(paths, plan.denied); err != nil {
		return nil, err
	}

	var txn diskTxn
	for _, fp := range sortedKeys(plan.diskWrites) {
		txn.write(fp, plan.diskOrig[fp], plan.diskStamps[fp], plan.diskWrites[fp])
	}
	res, err := s.commitDisk(&txn, plan.requester)
	if err != nil {
		return nil, err
	}
	reindexPaths := res.changed

	indexed := s.reindexAfterRename(nil, reindexPaths, plan.openReindexes)
	s.notifyHeldFiles(&plan.held)

	return &WorkspaceEdit{Changes: plan.openChanges, indexed: indexed}, nil
}

// checkRenameWrites refuses a rename before it changes anything when a file
// that it would write, move, or remove on disk cannot be written. The reads
// that planned the rename found these files; commitDisk checks the editors'
// open files again right before the writes.
func (s *Server) checkRenameWrites(diskPaths []string, deniedWrites map[string]error) error {
	// A file that cannot be written stops the rename before any file is
	// written. A rename that fails part of the way leaves the project half
	// renamed, and that does not compile. The reads that planned the rename
	// found these files (see readFileForRename). Only the files that are
	// written count: a file that the plan does not change may be read-only.
	var denied []string
	for _, fp := range diskPaths {
		if _, ok := deniedWrites[fp]; ok {
			denied = append(denied, fp)
		}
	}
	if len(denied) > 0 {
		sort.Strings(denied)
		first := deniedWrites[denied[0]]
		shown := make([]string, len(denied))
		for i, p := range denied {
			shown[i] = s.displayPath(p)
		}
		msg := fmt.Sprintf("Dexter: the rename was not applied, because Dexter cannot write %d of the files it must change: %s (%s). No file was changed.",
			len(denied), notify.Summarize(shown), errText(first))
		s.renameNotify(notify.Error, msg)
		return errors.New(strings.TrimPrefix(msg, "Dexter: "))
	}
	return nil
}

// reindexAfterRename applies every store mutation from a rename under one read
// lock. Besides keeping each write out of a cold build, holding the lock across
// removals and inserts prevents a build from observing an empty intermediate
// state and starting its insert-only transaction in the middle of the rename.
//
// The returned channel closes when the index has the rename: after the writes,
// and after a failed write is handed to the workspace, so the index then
// shows the rename, or will.
func (s *Server) reindexAfterRename(removePaths, diskPaths []string, textPaths []textReindex) <-chan struct{} {
	done := make(chan struct{})
	// The caller holds renameSerial (see beginRename).
	s.index.lastRenameIndexed = done
	s.index.backgroundWork.Add(1)
	go func() {
		defer s.index.backgroundWork.Done()
		defer close(done)
		// Deferred after close(done), so it runs first; the locks are
		// released in writeAfterRename before it.
		defer s.index.reporter.Recover("index", "the index update after a rename", s.index.faulted("the index update after a rename"))
		failed := s.writeAfterRename(removePaths, diskPaths, textPaths)
		if len(failed) == 0 {
			return
		}
		// A failed write keeps the file's old rows. A daemon-backed session
		// gives the paths to the workspace, which reconciles them from disk
		// and schedules a retry when that fails too. An embedded server only
		// logs: its next pass writes them, from their old mtime.
		log.Printf("Rename: %d files could not be written to the index", len(failed))
		if s.workspaceEvents != nil {
			for _, path := range failed {
				s.workspaceEvents.ReconcileFile(path)
			}
		}
	}()
	return done
}

// writeAfterRename does the store writes of reindexAfterRename under the
// mutation locks and returns the paths whose write failed. The caller reports
// them after it releases the locks, because the workspace queue can wait for
// the same locks.
//
// The files on disk go through the reconciliation's fast path: they are
// parsed on every core and written in batched transactions, with the same
// failure reporting (writeChangedInBatches). A rename of thousands of files
// would otherwise take one transaction for each file. A file whose stored
// mtime is not the one it was read with afterwards failed.
func (s *Server) writeAfterRename(removePaths, diskPaths []string, textPaths []textReindex) (failed []string) {
	s.index.reindexing.Lock()
	defer s.index.reindexing.Unlock()
	s.index.writes.RLock()
	defer s.index.writes.RUnlock()
	if s.index.unavailable {
		return nil
	}

	for _, path := range removePaths {
		err := testWriteFault(path)
		if err == nil {
			err = s.store.RemoveFile(path)
		}
		if err != nil {
			log.Printf("Error removing %s from index: %v", path, err)
			failed = append(failed, path)
		}
	}

	var changed []changedFile
	addDisk := func(path string) {
		path = filepath.Clean(path)
		// The full walk skips nested worktrees and the like; so does this.
		if !s.Indexable(path) {
			return
		}
		info, err := os.Stat(path)
		if err != nil {
			return
		}
		changed = append(changed, changedFile{path: path, mtimeNano: info.ModTime().UnixNano(), refs: true})
	}
	for _, path := range diskPaths {
		addDisk(path)
	}

	// Buffer text is indexed with store.BufferMtime, not the mtime of the
	// file on disk: the editor can still throw the edit away (undo, a
	// reload, a close without saving). A row with the disk mtime would then
	// look current to every later pass and keep text that is on no disk.
	// DidClose and the next pass read such a file from disk again.
	//
	// A path that is not on disk yet (the target of a move that the editor
	// makes) is not indexed here: the editor can refuse the move, and the
	// watcher indexes the file when it appears.
	//
	// This runs after the rename returned, maybe long after when a pass holds
	// the lock. If the editor closed the buffer in that time without saving
	// it, DidClose saw the disk rows and did nothing, so the file is indexed
	// from the disk here instead; so is a buffer that the editor saved.
	buffers := 0
	for _, entry := range textPaths {
		path := filepath.Clean(entry.path)
		if !s.Indexable(path) {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if _, open := s.docs.GetIfOpen(string(uri.File(path))); !open {
			addDisk(path)
			continue
		}
		if disk, err := os.ReadFile(path); err == nil && string(disk) == entry.text {
			addDisk(path)
			continue
		}
		defs, refs, err := parser.ParseText(path, entry.text)
		if err != nil {
			continue
		}
		err = testWriteFault(path)
		if err == nil {
			err = s.store.IndexBufferWithRefs(path, defs, refs)
		}
		if err != nil {
			log.Printf("Error indexing the buffer of %s: %v", path, err)
			s.index.failures.fail(path, err)
			failed = append(failed, path)
			continue
		}
		s.index.failures.ok(path)
		buffers++
	}

	written := 0
	if len(changed) > 0 {
		written, _ = s.writeChangedInBatches(s.index.work, changed, nil)
		paths := make([]string, len(changed))
		for i, c := range changed {
			paths[i] = c.path
		}
		stored, err := s.store.FileMtimes(paths)
		for _, c := range changed {
			if m, ok := stored[c.path]; err != nil || !ok || m != c.mtimeNano {
				failed = append(failed, c.path)
			}
		}
	}
	s.index.reportFileFailures()
	if written+buffers > 0 {
		log.Printf("Rename: reindexed %d files", written+buffers) // intentionally always logged — useful for user feedback
	}
	return failed
}

// isDepsFile returns true if filePath lives under the deps/ directory of some
// Mix project root (a directory containing mix.exs). It walks up from the
// file, and for each mix.exs found checks whether the file falls under that
// directory's deps/ subdirectory. Results are cached by directory so repeated
// calls for files in the same folder are O(1).
func (s *Server) isDepsFile(filePath string) bool {
	dir := filepath.Dir(filePath)

	if result, ok := s.depsCache.get(dir); ok {
		return result
	}
	result := isDepsFileUncached(filePath)
	s.depsCache.put(dir, result)
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
// document store for editor-owned (didOpen) buffers. The second return
// indicates whether the file is currently open in the editor — transient
// entries loaded from disk via GetOrLoad are NOT reported as open.
func (s *Server) readFileText(filePath string) (text string, open bool, ok bool) {
	uri := string(uri.File(filePath))
	if t, found := s.docs.GetIfOpen(uri); found {
		return t, true, true
	}
	if data, err := os.ReadFile(filePath); err == nil {
		return string(data), false, true
	}
	return "", false, false
}

// renameRequester is who asks for a rename, which decides what can receive
// the change of a file that an editor has open.
type renameRequester uint8

const (
	// requesterEditor is an LSP editor session. Its WorkspaceEdit can carry
	// the change of any file, and the user saves it there.
	requesterEditor renameRequester = iota
	// requesterHeadless has no editor to apply a WorkspaceEdit (an MCP tool,
	// a CLI command). Every change goes onto the disk.
	requesterHeadless
)

// renameAction is how a rename changes one file.
type renameAction uint8

const (
	actionDisk       renameAction = iota // the server writes (or moves) the file
	actionEdit                           // the change goes into the requester's WorkspaceEdit
	actionRefuse                         // unsaved in an editor: the whole rename stops
	actionRefuseMove                     // must move, open in another editor: the whole rename stops
)

// renameFileAction is the one place that decides how a rename changes a file.
// openHere is true when the requesting editor has the file open; others holds
// the buffer text of every other editor session that has it open; disk is the
// file's content on disk; moves is true when the module rename moves the file
// to a new path.
//
// Rule 1, for every requester: Dexter never writes, moves, or removes a file
// that an editor has open with unsaved changes (its buffer differs from the
// disk). That editor would keep its text and could save it back over the
// rename.
//
// Rule 2, for every requester: a file that must move and that another editor
// has open, clean or not, stops the whole rename. Whoever moves it, that
// editor's buffer keeps the old path, and its next save makes the old file
// again: the module would be defined twice, and the user's work would be in
// the wrong file.
//
//   - Editor requester: a file open in this or any other editor goes into the
//     requester's WorkspaceEdit. The user saves it there, and another editor
//     that has it open then sees an ordinary change on disk: it reloads a
//     clean buffer, or shows its own conflict warning for unsaved changes.
//     A closed file is written on disk.
//   - Headless requester: a file that is closed, or open only without unsaved
//     changes, is written on disk; the editor reloads the clean buffer. A file
//     with unsaved changes in an editor stops the whole rename.
func renameFileAction(requester renameRequester, openHere bool, others []string, disk string, moves bool) renameAction {
	if moves && len(others) > 0 {
		return actionRefuseMove
	}
	if requester == requesterEditor {
		if openHere || len(others) > 0 {
			return actionEdit
		}
		return actionDisk
	}
	for _, text := range others {
		if !sameText(text, disk) {
			return actionRefuse
		}
	}
	return actionDisk
}

// sameText reports whether an editor buffer holds the text on disk. It
// allows what editors do to a file they load without a change by the user:
// add or drop one final newline (Neovim's 'fixeol', for one), and show CRLF
// line ends as LF.
func sameText(buffer, disk string) bool {
	if buffer == disk || oneFinalNewline(buffer, disk) {
		return true
	}
	if !strings.Contains(buffer, "\r") && !strings.Contains(disk, "\r") {
		return false
	}
	buffer = strings.ReplaceAll(buffer, "\r\n", "\n")
	disk = strings.ReplaceAll(disk, "\r\n", "\n")
	return buffer == disk || oneFinalNewline(buffer, disk)
}

// oneFinalNewline reports whether a and b differ only in that one of them
// has one more final newline.
func oneFinalNewline(a, b string) bool {
	if len(a) < len(b) {
		a, b = b, a
	}
	return len(a) == len(b)+1 && a[len(a)-1] == '\n' && a[:len(b)] == b
}

// renameRead is one file as a rename reads it.
type renameRead struct {
	text     string       // the requesting editor's buffer, else the disk content
	action   renameAction // see renameFileAction
	held     bool         // the change is in the WorkspaceEdit because another editor has the file open
	writeErr error        // actionDisk: why the file cannot be written
	fromDisk bool         // text is the disk content, read when the file had stamp
	stamp    diskStamp
}

// readFileForRename reads a file that a rename may change and decides, through
// renameFileAction, how the rename changes it. ok is false when the file
// cannot be read.
//
// For an editor requester, a file that only another editor has open is read
// from the disk, because that is what the requesting editor opens to apply
// the edit. For a file the server writes, writeErr is the reason it cannot be
// written. A rename uses it to refuse the whole change before it writes the
// first file (see checkRenameWrites). The check runs in the parallel reads
// that plan the rename, so it adds no round of its own.
func (s *Server) readFileForRename(filePath string, requester renameRequester) (r renameRead, ok bool) {
	text, open := s.docs.GetIfOpen(string(uri.File(filePath)))
	if !open {
		// The stat comes first: a change during the read then shows as a
		// different stamp, never as the same one.
		info, err := os.Stat(filePath)
		if err != nil {
			return renameRead{}, false
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			return renameRead{}, false
		}
		text, r.fromDisk, r.stamp = string(data), true, stampOf(info)
	}
	r.text = text
	var others []string
	if !open {
		others = s.index.openDocs.heldTexts(s.docs, filePath)
	}
	r.action = renameFileAction(requester, open, others, text, false)
	switch r.action {
	case actionEdit:
		r.held = !open
	case actionDisk:
		r.writeErr = checkWritable(filePath)
	}
	return r, true
}

// unsavedFiles collects the files that stop a headless rename, because an
// editor has them open with unsaved changes.
type unsavedFiles struct {
	mu    sync.Mutex
	paths []string
}

func (u *unsavedFiles) add(path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.paths = append(u.paths, path)
}

// err returns the error that stops the rename, or nil.
func (s *Server) unsavedErr(u *unsavedFiles) error {
	if len(u.paths) == 0 {
		return nil
	}
	paths := make([]string, len(u.paths))
	for i, p := range u.paths {
		paths[i] = s.displayPath(p)
	}
	sort.Strings(paths)
	verb := "has"
	if len(paths) > 1 {
		verb = "have"
	}
	msg := fmt.Sprintf("%s %s unsaved changes in an editor; save or discard them there, then retry. Nothing was renamed.", notify.Summarize(paths), verb)
	log.Printf("Rename: %s", msg)
	return errors.New(msg)
}

// displayPath is path relative to the project root when it is inside it.
func (s *Server) displayPath(path string) string {
	if s.projectRoot != "" {
		if rel, err := filepath.Rel(s.projectRoot, path); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return path
}

// heldFiles collects the files that a rename puts into the requesting
// editor's WorkspaceEdit because another editor has them open.
type heldFiles struct {
	mu    sync.Mutex
	paths map[string]bool
}

func (h *heldFiles) add(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.paths == nil {
		h.paths = make(map[string]bool)
	}
	h.paths[path] = true
}

// notify tells the requesting editor, once for the whole rename, which files
// it must save. The other editor is not told: it sees the change on disk when
// the user saves.
func (s *Server) notifyHeldFiles(h *heldFiles) {
	if len(h.paths) == 0 {
		return
	}
	paths := make([]string, 0, len(h.paths))
	for p := range h.paths {
		paths = append(paths, s.displayPath(p))
	}
	sort.Strings(paths)
	if len(paths) == 1 {
		s.renameNotify(notify.Info, fmt.Sprintf(
			"Dexter: %s is also open in another editor, so its rename is in this editor as an unsaved change; save it here.", paths[0]))
		return
	}
	s.renameNotify(notify.Info, fmt.Sprintf(
		"Dexter: %s are also open in another editor, so their rename is in this editor as unsaved changes; save them here.", notify.Summarize(paths)))
}

// getFileLine returns the text of line lineNum (1-based) from the file at
// filePath, preferring the in-memory document store for editor-owned
// buffers. Transient entries loaded via GetOrLoad fall through to the
// disk path. For closed files, only reads up to the target line instead
// of the whole file.
// maxFileLineBytes is the longest line getFileLine reads from disk.
const maxFileLineBytes = 16 << 20

func (s *Server) getFileLine(filePath string, lineNum int) (string, bool) {
	// Editor-owned buffer: extract the single line from memory
	uri := string(uri.File(filePath))
	if text, ok := s.docs.GetIfOpen(uri); ok {
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
	// The default 64 KiB limit stops at a long generated line and hides
	// every line after it.
	scanner.Buffer(make([]byte, 0, 64*1024), maxFileLineBytes)
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
	own := make(map[int]bool, len(defResults))
	for _, r := range defResults {
		// A type's arity counts too: its uses in typespecs are references
		// of the type kind.
		defFilePaths[r.FilePath] = true
		own[r.Arity] = true
	}
	var refs []store.ReferenceResult
	for filePath := range defFilePaths {
		fileText, _, ok := s.readFileText(filePath)
		if !ok || !strings.Contains(fileText, functionName) {
			continue
		}
		src := []byte(fileText)
		res := parser.TokenizeFull(src)
		p := &parsedSource{text: fileText, src: src, tokens: res.Tokens, interp: res.Interp, lineStarts: res.LineStarts}
		// A bare name inside a typespec names the type, not the function, so
		// it is recorded under its own kind — the same split the indexed refs
		// use. A nested or a sibling module in the file does not see the
		// module's functions unqualified (see siteCollector.collect).
		// A call of an arity that the module does not define is a function
		// that it imports.
		for _, lineNum := range ownBareCallLines(p, functionName, own) {
			if enclosing := p.moduleAt(lineNum); enclosing != "" && enclosing != module {
				continue
			}
			refs = append(refs, store.ReferenceResult{FilePath: filePath, Line: lineNum, Kind: kindAt(p, lineNum)})
		}
	}
	return refs
}

func (s *Server) SignatureHelp(ctx context.Context, params *protocol.SignatureHelpParams) (*protocol.SignatureHelp, error) {
	docURI := string(params.TextDocument.URI)
	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)

	// Get cached tokens for efficient multi-query operations
	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}

	funcExpr, argIndex, found := tf.CallContextAtCursor(lineNum, col)
	if !found {
		return nil, nil
	}

	moduleRef, functionName := ExtractModuleAndFunction(funcExpr)
	if functionName == "" {
		return nil, nil
	}

	aliases := tf.ExtractAliasesInScope(lineNum)
	s.mergeAliasesFromUseTokenized(tf, aliases)

	// Resolve the function to a store lookup result
	var result *store.LookupResult
	var generatedModule string
	var generatedFunctions []beam.Function
	if moduleRef != "" {
		fullModule := resolveModule(moduleRef, aliases)
		if results, err := s.store.LookupFunction(fullModule, functionName); err == nil && len(results) > 0 {
			result = &results[0]
		} else if functions, found := s.generatedSymbol(fullModule, "", functionName); found {
			generatedModule, generatedFunctions = fullModule, functions
		}
	} else {
		// Bare function — check buffer, imports, use chains, Kernel
		if defLine, found := tf.FindFunctionDefinition(functionName); found {
			// Build signature from buffer
			paramNames := extractParamNames(lines, defLine-1)
			if paramNames == nil {
				return nil, nil
			}
			sig := buildSignature(functionName, paramNames, tf, lines, defLine-1)
			return &protocol.SignatureHelp{
				Signatures:      []protocol.SignatureInformation{sig},
				ActiveSignature: 0,
				ActiveParameter: uint32(argIndex),
			}, nil
		}

		for _, mod := range tf.ExtractImports() {
			mod = resolveModule(mod, aliases)
			if results, err := s.store.LookupPublicFunction(mod, functionName); err == nil && len(results) > 0 {
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
				if results = publicOnly(results); len(results) > 0 {
					result = &results[0]
				}
			}
		}

		if result == nil {
			currentModule := s.store.LookupEnclosingModule(uriToPath(protocol.DocumentURI(docURI)), lineNum+1)
			if provider, functions, found := s.generatedSymbolInScope(currentModule, func() []string {
				return s.enclosingBlockPath(docURI, lineNum, col)
			}, functionName); found {
				generatedModule, generatedFunctions = provider.module, functions
			}
		}
	}

	if len(generatedFunctions) > 0 {
		return s.generatedSignature(generatedFunctions, generatedModule, argIndex), nil
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

	tfDef := NewTokenizedFile(fileText)
	sig := buildSignature(functionName, paramNames, tfDef, fileLines, defIdx)
	return &protocol.SignatureHelp{
		Signatures:      []protocol.SignatureInformation{sig},
		ActiveSignature: 0,
		ActiveParameter: uint32(argIndex),
	}, nil
}

func buildSignature(functionName string, paramNames []string, tf *TokenizedFile, lines []string, defIdx int) protocol.SignatureInformation {
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
	doc, spec := tf.ExtractDocAbove(defIdx)
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

// workspaceSymbolLimit is how many index rows one workspace symbol answer
// holds.
const workspaceSymbolLimit = 50

func (s *Server) Symbols(ctx context.Context, params *protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	query := params.Query
	if query == "" {
		return nil, nil
	}

	// The same search as MCP: one row per function, and the functions that
	// `use` injects or the compiled module generates.
	found, err := s.SearchNames(query, false, workspaceSymbolLimit)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var symbols []protocol.SymbolInformation
	for _, r := range found.Results {
		if r.FilePath == "" {
			continue
		}
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
	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)
	if lineNum >= len(lines) {
		return nil, nil
	}

	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}
	typeCtx := tf.ExpressionAtCursor(lineNum, col)
	typeName := typeCtx.FunctionName
	if typeName == "" {
		return nil, nil
	}

	aliases := tf.ExtractAliasesInScope(lineNum)
	s.mergeAliasesFromUseTokenized(tf, aliases)
	fullModule := s.resolveModuleWithNesting(typeCtx.ModuleRef, aliases, uriToPath(protocol.DocumentURI(docURI)), lineNum)

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
	docURI := string(params.TextDocument.URI)
	text, ok := s.docs.GetOrLoad(docURI)
	if !ok {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	lineNum := int(params.Position.Line)
	col := s.inCol(lines, lineNum, params.Position.Character)
	if lineNum >= len(lines) {
		return nil, nil
	}

	tf := s.docs.GetTokenizedFile(docURI)
	if tf == nil {
		tf = NewTokenizedFile(text)
	}
	callCtx := tf.ExpressionAtCursor(lineNum, col)
	functionName := callCtx.FunctionName
	if functionName == "" {
		return nil, nil
	}

	aliases := tf.ExtractAliasesInScope(lineNum)
	s.mergeAliasesFromUseTokenized(tf, aliases)
	var fullModule string
	if callCtx.ModuleRef != "" {
		fullModule = resolveModule(callCtx.ModuleRef, aliases)
	} else {
		fullModule = s.resolveBareFunctionModule(uriToPath(protocol.DocumentURI(docURI)), text, tf, lineNum, functionName, aliases)
	}
	var generatedFunctions []beam.Function
	if fullModule == "" {
		currentModule := s.store.LookupEnclosingModule(uriToPath(protocol.DocumentURI(docURI)), lineNum+1)
		if provider, functions, found := s.generatedSymbolInScope(currentModule, func() []string {
			return s.enclosingBlockPath(docURI, lineNum, col)
		}, functionName); found {
			fullModule, generatedFunctions = provider.module, functions
		} else {
			return nil, nil
		}
	}

	defResults, err := s.store.LookupFunction(fullModule, functionName)
	if err == nil && len(defResults) == 0 && len(generatedFunctions) == 0 {
		// A function that `use` injects, as definition finds it.
		defResults = s.lookupThroughUseOfWithFollow(fullModule, functionName, false)
	}
	if err != nil || len(defResults) == 0 {
		if len(generatedFunctions) == 0 {
			generatedFunctions, _ = s.generatedSymbol(fullModule, "", functionName)
		}
		if len(generatedFunctions) == 0 {
			return nil, nil
		}
		defResults, _ = s.generatedDefinitionResultsFor(fullModule, "", generatedFunctions)
		if len(defResults) == 0 {
			return nil, nil
		}
	}

	r := defResults[0]
	if len(generatedFunctions) > 0 {
		r.Arity = generatedFunctions[0].Arity
		r.Kind = generatedFunctions[0].Kind
	}
	nameCol := 0
	defLine, _ := s.getFileLine(r.FilePath, r.Line)
	if col := findTokenColumn(defLine, functionName); col >= 0 {
		nameCol = col
	}

	item := protocol.CallHierarchyItem{
		Name:   fmt.Sprintf("%s.%s/%d", fullModule, functionName, r.Arity),
		Kind:   protocol.SymbolKindFunction,
		Detail: r.Kind,
		URI:    protocol.DocumentURI(uri.File(r.FilePath)),
		Range:  lineRange(r.Line - 1),
		SelectionRange: protocol.Range{
			Start: s.outPosLine(defLine, r.Line-1, nameCol),
			End:   s.outPosLine(defLine, r.Line-1, nameCol+len(functionName)),
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

	// The same sites as find-references, the CLI, and MCP.
	refs, err := s.ReferenceNames(module, functionName, NameReferenceOptions{
		Kind:            NameKindCallable,
		FollowDelegates: s.followDelegates,
		ExcludeStdlib:   true,
	})
	if err != nil {
		return nil, nil
	}

	type callerKey struct {
		filePath string
		line     int
		name     string
	}
	index := make(map[callerKey]int)
	var calls []protocol.CallHierarchyIncomingCall
	resolver := s.NewCallerResolver()
	for _, r := range refs {
		if r.Kind == "import_only" {
			// A key of an import's only: list names the function; it is
			// not a call.
			continue
		}
		caller := resolver.CallerAt(r.FilePath, r.Line)
		k := callerKey{r.FilePath, caller.Line, caller.Name()}
		callRange := lineRange(r.Line - 1)
		if at, ok := index[k]; ok {
			calls[at].FromRanges = append(calls[at].FromRanges, callRange)
			continue
		}
		index[k] = len(calls)
		calls = append(calls, protocol.CallHierarchyIncomingCall{
			From:       s.callerItem(r.FilePath, caller),
			FromRanges: []protocol.Range{callRange},
		})
	}
	return calls, nil
}

// callerItem is the call hierarchy item of a caller: a function, which can be
// expanded further, or a block or module body, which cannot.
func (s *Server) callerItem(filePath string, caller Caller) protocol.CallHierarchyItem {
	lineText, _ := s.getFileLine(filePath, caller.Line)
	token := caller.Function
	kind := protocol.SymbolKindFunction
	data := map[string]string{"module": caller.Module, "function": caller.Function}
	if caller.Function == "" {
		kind = protocol.SymbolKindModule
		token = moduleLastSegment(caller.Module)
		if caller.Block != "" {
			// A test or describe block runs like a function body.
			kind = protocol.SymbolKindFunction
			token, _, _ = strings.Cut(caller.Block, " ")
		}
	}
	nameCol := 0
	if col := findTokenColumn(lineText, token); col >= 0 {
		nameCol = col
	}
	return protocol.CallHierarchyItem{
		Name:  caller.Name(),
		Kind:  kind,
		URI:   protocol.DocumentURI(uri.File(filePath)),
		Range: lineRange(caller.Line - 1),
		SelectionRange: protocol.Range{
			Start: s.outPosLine(lineText, caller.Line-1, nameCol),
			End:   s.outPosLine(lineText, caller.Line-1, nameCol+len(token)),
		},
		Data: data,
	}
}

func (s *Server) OutgoingCalls(ctx context.Context, params *protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	module, functionName := extractCallHierarchyData(params.Item.Data)
	if module == "" || functionName == "" {
		return nil, nil
	}
	callees, err := s.CalleesOf(module, functionName)
	if err != nil {
		return nil, nil
	}
	calls := make([]protocol.CallHierarchyOutgoingCall, 0, len(callees))
	for _, c := range callees {
		lineText, _ := s.getFileLine(c.FilePath, c.Line)
		nameCol := 0
		if col := findTokenColumn(lineText, c.Function); col >= 0 {
			nameCol = col
		}
		ranges := make([]protocol.Range, 0, len(c.Calls))
		for _, l := range c.Calls {
			ranges = append(ranges, lineRange(l-1))
		}
		calls = append(calls, protocol.CallHierarchyOutgoingCall{
			To: protocol.CallHierarchyItem{
				Name:  fmt.Sprintf("%s.%s", c.Module, c.Function),
				Kind:  protocol.SymbolKindFunction,
				URI:   protocol.DocumentURI(uri.File(c.FilePath)),
				Range: lineRange(c.Line - 1),
				SelectionRange: protocol.Range{
					Start: s.outPosLine(lineText, c.Line-1, nameCol),
					End:   s.outPosLine(lineText, c.Line-1, nameCol+len(c.Function)),
				},
				Data: map[string]string{"module": c.Module, "function": c.Function},
			},
			FromRanges: ranges,
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
