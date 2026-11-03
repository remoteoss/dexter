package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
	"github.com/remoteoss/dexter/internal/workspace"
)

const (
	// DefaultIdleTimeout is how long a daemon with no connections waits before
	// exiting. An idle daemon costs almost no CPU — a 2s .git/HEAD stat — so
	// this is sized to keep caches warm across the gaps between agent tool
	// calls, editor restarts, and a CLI run a few minutes after the last one,
	// while still returning a large workspace's resident memory and watch
	// descriptors when the project is genuinely abandoned. Zero means never
	// exit. Override per machine with DEXTER_DAEMON_IDLE_TIMEOUT.
	DefaultIdleTimeout = 15 * time.Minute

	// serverHandshakeTimeout bounds reading the hello line from a peer that may
	// never send one.
	serverHandshakeTimeout = 10 * time.Second

	// maxConcurrentRequests bounds in-flight control requests per connection so
	// one client cannot exhaust the daemon. Excess requests get an error instead
	// of blocking the connection reader, which must remain able to observe close.
	maxConcurrentRequests = 64

	// maxReadyWait caps how long one request waits for the initial
	// reconciliation before answering from whatever is already indexed. It
	// matches the MCP frontend's own index barrier so a cold workspace reports
	// "still building, retry" through either path instead of parking a request
	// goroutine for minutes.
	maxReadyWait = 30 * time.Second
)

// socketCheckInterval is how often the daemon checks that its socket, its lock
// file, its index file, and its project root are still the ones it uses. Each
// check is a few stat calls. A variable so tests can shrink it.
var socketCheckInterval = 10 * time.Second

// touchInterval is how old the socket and the lock file may get before the
// daemon sets their times again. Age-based temp cleaners, such as
// systemd-tmpfiles, remove files that nobody touched for days; a daemon that
// runs for weeks must not lose its runtime files to them.
var touchInterval = time.Hour

// restartWindow limits how often one cause can restart the workspace runtime.
// A second restart for the same cause inside it is not made: a process that
// keeps replacing the index, or a panic that comes back on each pass, would
// otherwise keep the daemon rebuilding for ever.
var restartWindow = 10 * time.Minute

// ErrRootGone is returned by Run when the project root went away while the
// daemon served it, for example because its volume was ejected. The daemon then
// exits without closing the index: its files were on that volume, and SQLite
// maps one of them, so any access could end the process with SIGBUS.
var ErrRootGone = lsp.ErrRootGone

// CondIndexReplaced is the condition of an index file that another process
// replaced or removed while the daemon had it open.
const CondIndexReplaced = lsp.IndexConditionPrefix + "replaced"

// Control method names. These are reserved: RegisterMethod refuses them, and
// they are exported so an adapter or a frontend can call one without
// duplicating the string.
const (
	MethodStatus          = "daemon/status"
	MethodShutdown        = "daemon/shutdown"
	MethodWorkspaceStatus = "workspace/status"
	MethodLookup          = "workspace/lookup"
	MethodReferences      = "workspace/references"
	MethodReindex         = "workspace/reindex"
	MethodWatch           = "workspace/watch"
	MethodUnwatch         = "workspace/unwatch"

	// MethodCancel cancels one in-flight request of the same connection, like
	// $/cancelRequest in LSP. It is sent with id 0 and gets no response; the
	// canceled request still answers, usually with a context error, and the
	// client has stopped waiting for it.
	MethodCancel = "$/cancel"
)

// CancelParams names the request that MethodCancel cancels.
type CancelParams struct {
	ID uint64 `json:"id"`
}

// Status describes the daemon serving a workspace.
type Status struct {
	Root      string   `json:"root"`
	PID       int      `json:"pid"`
	Version   string   `json:"version"`
	Contract  int      `json:"contract"`
	Ready     bool     `json:"ready"`
	Clients   int64    `json:"clients"`
	UptimeMs  int64    `json:"uptimeMs"`
	Frontends []string `json:"frontends,omitempty"`
}

// SessionStatus describes one attached editor session.
type SessionStatus struct {
	ID            string `json:"id"`
	OpenDocuments int    `json:"openDocuments"`
	ConnectedMs   int64  `json:"connectedMs"`
}

// WorkspaceStatus is the readiness and index snapshot frontends gate on.
type WorkspaceStatus struct {
	Root                 string          `json:"root"`
	Ready                bool            `json:"ready"`
	Watching             bool            `json:"watching"`
	StdlibRoot           string          `json:"stdlibRoot,omitempty"`
	IndexVersion         int             `json:"indexVersion"`
	ExpectedIndexVersion int             `json:"expectedIndexVersion"`
	Files                int             `json:"files"`
	Definitions          int             `json:"definitions"`
	References           int             `json:"references"`
	Sessions             []SessionStatus `json:"sessions,omitempty"`
	// Conditions are the failures and degraded states that are active now,
	// the same ones that attached editors see.
	Conditions []Note `json:"conditions,omitempty"`
}

// Note is one active condition of the workspace, for a frontend that cannot
// show LSP messages. Key names the condition (for example "index.rebuild");
// Severity is "error", "warning", or "info".
type Note struct {
	Key      string `json:"key,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

func notesFrom(conditions []notify.Condition) []Note {
	if len(conditions) == 0 {
		return nil
	}
	out := make([]Note, len(conditions))
	for i, c := range conditions {
		out[i] = Note{Key: c.Key, Severity: c.Severity.String(), Message: c.Message}
	}
	return out
}

// StatusParams optionally waits for the initial reconciliation before
// answering, replacing a frontend-side index barrier.
type StatusParams struct {
	WaitReadyMs int `json:"waitReadyMs,omitempty"`
}

// ShutdownParams configures daemon/shutdown. Force stops the workspace even
// while other clients are attached, which is what `dexter stop --force` needs:
// an editor session that would otherwise hold the daemon alive indefinitely.
type ShutdownParams struct {
	Force bool `json:"force,omitempty"`
}

type LookupParams struct {
	Module          string `json:"module"`
	Function        string `json:"function,omitempty"`
	FollowDelegates bool   `json:"followDelegates"`
	Strict          bool   `json:"strict"`
	WaitReadyMs     int    `json:"waitReadyMs,omitempty"`
}

// LookupResult travels in the location encoding described at locationsWire.
// Notes are the active conditions of the index, such as a rebuild, so that a
// result from an incomplete index is not taken as complete.
type LookupResult struct {
	Locations []lsp.NameLocation
	Ready     bool
	Notes     []Note
}

func (r LookupResult) MarshalJSON() ([]byte, error) {
	return marshalLocations(r.Locations, r.Ready, r.Notes)
}

func (r *LookupResult) UnmarshalJSON(data []byte) error {
	var err error
	r.Locations, r.Ready, r.Notes, err = unmarshalLocations(data)
	return err
}

type ReferencesParams struct {
	Module      string `json:"module"`
	Function    string `json:"function,omitempty"`
	WaitReadyMs int    `json:"waitReadyMs,omitempty"`
}

// ReferencesResult travels in the location encoding described at
// locationsWire.
type ReferencesResult struct {
	Locations []lsp.NameLocation
	Ready     bool
	Notes     []Note
}

func (r ReferencesResult) MarshalJSON() ([]byte, error) {
	return marshalLocations(r.Locations, r.Ready, r.Notes)
}

func (r *ReferencesResult) UnmarshalJSON(data []byte) error {
	var err error
	r.Locations, r.Ready, r.Notes, err = unmarshalLocations(data)
	return err
}

// locationsWire is the wire shape of a location list: each path once in a file
// table, and locations that index into it in their original order. A hot
// reference result repeats a few thousand absolute paths across ~100k
// locations; spelling every path out pushed such a result past the protocol
// line limit, and the table keeps it about a fifth of that size.
type locationsWire struct {
	Files     []string       `json:"files"`
	Locations []locationWire `json:"locations"`
	Ready     bool           `json:"ready"`
	Notes     []Note         `json:"notes,omitempty"`
}

type locationWire struct {
	File        int    `json:"file"`
	Line        int    `json:"line"`
	Kind        string `json:"kind,omitempty"`
	Arity       int    `json:"arity,omitempty"`
	Declaration bool   `json:"declaration,omitempty"`
}

func marshalLocations(locations []lsp.NameLocation, ready bool, notes []Note) ([]byte, error) {
	wire := locationsWire{
		Files:     []string{},
		Locations: make([]locationWire, len(locations)),
		Ready:     ready,
		Notes:     notes,
	}
	fileIndex := make(map[string]int)
	for i, location := range locations {
		index, ok := fileIndex[location.FilePath]
		if !ok {
			index = len(wire.Files)
			fileIndex[location.FilePath] = index
			wire.Files = append(wire.Files, location.FilePath)
		}
		wire.Locations[i] = locationWire{
			File:        index,
			Line:        location.Line,
			Kind:        location.Kind,
			Arity:       location.Arity,
			Declaration: location.IsDeclaration,
		}
	}
	return json.Marshal(wire)
}

func unmarshalLocations(data []byte) ([]lsp.NameLocation, bool, []Note, error) {
	var wire locationsWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, false, nil, err
	}
	locations := make([]lsp.NameLocation, len(wire.Locations))
	for i, location := range wire.Locations {
		if location.File < 0 || location.File >= len(wire.Files) {
			return nil, false, nil, fmt.Errorf("location %d names file %d of %d", i, location.File, len(wire.Files))
		}
		locations[i] = lsp.NameLocation{
			FilePath:      wire.Files[location.File],
			Line:          location.Line,
			Kind:          location.Kind,
			Arity:         location.Arity,
			IsDeclaration: location.Declaration,
		}
	}
	return locations, wire.Ready, wire.Notes, nil
}

type ReindexParams struct {
	Target string `json:"target"`
}

type ReindexResult struct {
	Elapsed time.Duration `json:"elapsed"`
	// Missing reports a target that neither exists nor had anything indexed
	// under it, so the reindex had nothing to do. It is not an error: the index
	// already matches the disk, and the watcher may simply have pruned a
	// deleted file first. A mistyped path is the usual cause.
	Missing bool `json:"missing,omitempty"`
	// Notes are the active conditions of the index after the reindex.
	Notes []Note `json:"notes,omitempty"`
}

// WatchParams subscribes a connection to coalesced index mutations.
type WatchParams struct {
	Buffer int `json:"buffer,omitempty"`
}

// Subscription identifies one workspace/watch registration. Subscribing returns
// it and workspace/unwatch takes it back, so the two cannot drift apart.
type Subscription struct {
	ID string `json:"subscription"`
}

// Changed is one coalesced index mutation batch pushed to a subscriber.
type Changed struct {
	Subscription string   `json:"subscription"`
	Full         bool     `json:"full"`
	Paths        []string `json:"paths,omitempty"`
}

// Run acquires the workspace lock and serves until ctx is canceled or the
// workspace has no connections for idleTimeout.
func Run(ctx context.Context, root string, idleTimeout time.Duration) error {
	// A daemon for a root that is not there would create it again when it
	// opens the index: a mount point of an ejected volume, for example.
	if info, err := os.Stat(root); err != nil {
		return fmt.Errorf("project root: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("project root %s is not a directory", root)
	}
	ownership, endpoint, err := acquireOwnershipPatiently(root)
	if err != nil {
		return err
	}
	defer func() { _ = ownership.Release() }()

	listener, err := listenSocket(endpoint)
	if err != nil {
		return err
	}
	socketInfo, _ := os.Stat(endpoint.Socket)
	defer func() {
		_ = listener.Close()
		// Only this daemon's socket: a daemon that another one replaced must
		// not remove the socket of its replacement.
		if current, err := os.Stat(endpoint.Socket); err == nil && socketInfo != nil && os.SameFile(current, socketInfo) {
			_ = os.Remove(endpoint.Socket)
		}
	}()
	go cleanRuntimeDirSafely(endpoint)

	runtime, err := workspace.Open(endpoint.Root)
	if err != nil {
		return err
	}

	s := newServer(ctx, endpoint, runtime, listener, idleTimeout)
	s.ownership = ownership
	s.socketInfo = socketInfo
	s.lockPath = endpoint.Lock
	defer func() {
		if s.abandoned.Load() {
			// The index files were on the root's volume; see ErrRootGone.
			log.Printf("Not closing the index: the project root %s is not there any more", endpoint.Root)
			return
		}
		s.rtMu.Lock()
		defer s.rtMu.Unlock()
		if s.runtime == nil {
			return
		}
		if closeErr := s.runtime.Close(); closeErr != nil {
			log.Printf("Warning: closing workspace: %v", closeErr)
		}
	}()
	defer s.cancelCtx()
	go s.watchEndpoint()
	log.Printf("Dexter daemon v%s listening (root: %s, socket: %s)", version.Version, endpoint.Root, endpoint.Socket)
	err = s.serve()
	if goneErr := s.goneErr(); goneErr != nil {
		return goneErr
	}
	return err
}

// goneErr returns why the project root is gone, or nil.
func (s *server) goneErr() error {
	if !s.abandoned.Load() {
		return nil
	}
	if rt := s.currentRuntime(); rt != nil {
		if err := rt.GoneErr(); err != nil {
			return err
		}
	}
	return fmt.Errorf("%w: %s", ErrRootGone, s.root)
}

// ownershipPatience is how long a starting daemon tries again for a lock that
// is held. A cleanup of the runtime directory, or `dexter stop`, holds the
// lock of another workspace for a moment; a daemon that started in that moment
// would exit, and its frontend would find nobody serving.
var ownershipPatience = 100 * time.Millisecond

func acquireOwnershipPatiently(root string) (*Ownership, Endpoint, error) {
	deadline := time.Now().Add(ownershipPatience)
	for {
		ownership, endpoint, err := AcquireOwnership(root)
		if !errors.Is(err, ErrWorkspaceOwned) || !time.Now().Before(deadline) {
			return ownership, endpoint, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// listenSocket prepares and binds the workspace's local socket. The caller must
// already hold workspace ownership: removing a socket a live process is still
// serving would split one workspace into two.
func listenSocket(endpoint Endpoint) (net.Listener, error) {
	// Ownership proves no live daemon can be using this endpoint. Only now is it
	// safe to remove a socket left by an ungraceful exit.
	if err := os.Remove(endpoint.Socket); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale daemon socket: %w", err)
	}

	// Bind before opening the workspace. A client that races startup finds a
	// listening socket at once and its handshake completes from the backlog as
	// soon as the runtime exists. Blocking here until the initial
	// reconciliation finished would make a cold CLI call wait for a full index
	// build; frontends gate on readiness through workspace/status instead.
	listener, err := net.Listen("unix", endpoint.Socket)
	if err != nil {
		return nil, fmt.Errorf("listen on daemon socket: %w", err)
	}
	// Run removes the socket itself, and only when it is still this one: a
	// daemon that another one replaced must not unlink the socket of its
	// replacement when it closes.
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(endpoint.Socket, 0o600); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("secure daemon socket: %w", err)
	}
	return listener, nil
}

// newServer wires the state a workspace server needs. ctx cancels its lifetime.
func newServer(ctx context.Context, endpoint Endpoint, rt *workspace.Runtime, listener net.Listener, idleTimeout time.Duration) *server {
	s := &server{
		root:        endpoint.Root,
		identity:    endpoint.Identity,
		runtime:     rt,
		openRuntime: workspace.Open,
		listener:    listener,
		socket:      endpoint.Socket,
		socketCheck: socketCheckInterval,
		idleTimeout: idleTimeout,
		started:     time.Now(),
		activity:    make(chan struct{}, 1),
		stopping:    make(chan struct{}),
		connections: make(map[net.Conn]struct{}),
		restarts:    make(map[string]time.Time),
	}
	s.ctx, s.cancelCtx = context.WithCancel(ctx)
	// A workspace whose root is gone cannot be served: every answer would come
	// from files that are no longer there. The daemon exits and releases the
	// workspace, so the next frontend fails clearly or starts a new daemon on
	// the directory the path now names. watchEndpoint follows Runtime.Gone of the
	// runtime that serves now.
	go func() {
		select {
		case <-ctx.Done():
			s.stop()
		case <-s.stopping:
		}
	}()
	if idleTimeout > 0 {
		go s.watchIdle()
	}
	return s
}

// watchEndpoint checks, every socketCheck, that the files this daemon depends
// on are still its own, and acts on the reports of the runtime:
//
//   - The socket must be the one this daemon bound. A temp cleaner can remove
//     it, and a second daemon can bind a new one at the path after the lock
//     file was removed too. Either way this daemon cannot be reached, so it
//     exits; its editors reconnect to whatever serves the path now.
//   - The lock file must be the one this daemon locked. When it was removed,
//     or replaced by a file that nobody holds, the daemon locks the file at the
//     path again. When another process holds that file, that process owns the
//     workspace, and this daemon exits.
//   - The runtime directory must still be the one that a new frontend
//     resolves, or new frontends would start a second daemon elsewhere.
//   - The index file must be the one the store opened (see
//     workspace.CheckIndexFile); the runtime restarts when it is not.
//   - The project root must be there; see ErrRootGone.
//
// Socket and lock file get new times once per touchInterval.
func (s *server) watchEndpoint() {
	defer func() {
		if value := recover(); value != nil {
			log.Printf("Error: panic in the daemon's endpoint check: %v\n%s", value, debug.Stack())
			// Without this check nothing notices a replaced lock or socket;
			// exiting is the safe state, and editors reconnect.
			s.stop()
		}
	}()
	ticker := time.NewTicker(s.socketCheck)
	defer ticker.Stop()
	for {
		rt := s.currentRuntime()
		if rt == nil {
			return
		}
		select {
		case <-s.stopping:
			return
		case what := <-rt.Faults():
			s.restartRuntime("fault", fmt.Sprintf("an internal error in %s", what), nil)
		case <-rt.Gone():
			log.Printf("Daemon exiting: %v", rt.GoneErr())
			// The editors are told why before their connections close.
			rt.Reporter().Flush(2 * time.Second)
			s.abandon()
			return
		case <-ticker.C:
			if !s.checkEndpoint(rt) {
				return
			}
		}
	}
}

// checkEndpoint runs one round of the checks of watchEndpoint. It reports
// false when the daemon is stopping.
func (s *server) checkEndpoint(rt *workspace.Runtime) bool {
	if rt.CheckRoot() {
		return true // Gone is closed; the next round of watchEndpoint exits
	}
	current, err := os.Stat(s.socket)
	switch {
	case os.IsNotExist(err):
		log.Printf("Daemon socket %s disappeared; exiting so a replacement can start", s.socket)
		s.stop()
		return false
	case err == nil && s.socketInfo != nil && !os.SameFile(current, s.socketInfo):
		log.Printf("Daemon socket %s was replaced by another process; exiting so that only one daemon serves %s", s.socket, s.root)
		s.stop()
		return false
	}
	if s.ownership != nil {
		state, stateErr := s.ownership.state()
		if stateErr == nil && state != lockHeld {
			what := "removed"
			if state == lockReplaced {
				what = "replaced"
			}
			ok, relockErr := s.ownership.relock()
			if !ok {
				log.Printf("Daemon lock file %s was %s and another process holds the new one (%v); exiting so that only one daemon serves %s", s.lockPath, what, relockErr, s.root)
				s.stop()
				return false
			}
			log.Printf("Daemon lock file %s was %s; locked it again", s.lockPath, what)
		}
	}
	if endpoint, err := ResolveEndpoint(s.root); err == nil && endpoint.Socket != s.socket {
		log.Printf("New frontends resolve the runtime directory of %s to %s now, not %s; exiting so that the next frontend starts a daemon there", s.root, filepath.Dir(endpoint.Socket), filepath.Dir(s.socket))
		s.stop()
		return false
	}
	s.touchRuntimeFiles()
	switch rt.CheckIndexFile() {
	case workspace.IndexFileReplaced:
		s.indexFileChanged(rt, "replaced")
	case workspace.IndexFileRemoved:
		s.indexFileChanged(rt, "removed")
	}
	return true
}

// touchRuntimeFiles keeps age-based temp cleaners away from the socket and the
// lock file.
func (s *server) touchRuntimeFiles() {
	now := time.Now()
	if now.Sub(s.lastTouch) < touchInterval {
		return
	}
	s.lastTouch = now
	for _, path := range []string{s.socket, s.lockPath} {
		if path != "" {
			_ = os.Chtimes(path, now, now)
		}
	}
}

// indexFileChanged restarts the runtime on the index file that is at the path
// now, or on a new one, and tells the editors why.
func (s *server) indexFileChanged(rt *workspace.Runtime, what string) {
	path := store.DBPath(s.root)
	message := fmt.Sprintf(
		"Dexter: the index file %s was %s by another process, for example an older Dexter build that an editor runs in-process, or `git clean`. Dexter restarted its index on the file that is there now and brings it up to date. Do not let Dexter builds without the workspace daemon (0.7.x and earlier) and later builds serve one project at the same time.",
		path, what)
	restarted := s.restartRuntime("index file "+what, "the index file was "+what, func(r *notify.Reporter) {
		r.Set(CondIndexReplaced, notify.Warning, message)
	})
	if !restarted {
		rt.Reporter().Set(CondIndexReplaced, notify.Error, fmt.Sprintf(
			"Dexter: another process keeps replacing or removing the index file %s, for example an older Dexter build that an editor runs in-process. Dexter does not restart its index again and answers from the copy it has open, which other processes do not see. Stop the other process, then run `dexter stop --force` in the project directory and restart the editors.",
			path))
	}
}

// currentRuntime returns the runtime that serves now.
func (s *server) currentRuntime() *workspace.Runtime {
	s.rtMu.RLock()
	defer s.rtMu.RUnlock()
	return s.runtime
}

// restartRuntime closes the workspace runtime and opens a new one on the same
// root. Every connection is closed first, because each holds the runtime it
// started with; editors reconnect, and the LSP proxy rebuilds their sessions.
// It reports false when restartWindow refused the restart. report runs on the
// new runtime's reporter before any frontend can attach.
func (s *server) restartRuntime(cause, reason string, report func(*notify.Reporter)) bool {
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	if last, ok := s.restarts[cause]; ok && time.Since(last) < restartWindow {
		log.Printf("Not restarting the workspace runtime for %s again within %s", reason, restartWindow)
		return false
	}
	s.restarts[cause] = time.Now()
	log.Printf("Restarting the workspace runtime: %s", reason)

	// A pending writer stops new readers, so connections that arrive now wait
	// for the new runtime; the ones that hold the old runtime are closed until
	// the lock is free.
	acquired := make(chan struct{})
	go func() {
		s.rtMu.Lock()
		close(acquired)
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for waiting := true; waiting; {
		s.closeConnections()
		select {
		case <-acquired:
			waiting = false
		case <-ticker.C:
		}
	}
	defer s.rtMu.Unlock()
	// A daemon that stops while this waited closes the runtime itself; a new
	// one opened now would outlive it.
	select {
	case <-s.stopping:
		return true
	default:
	}

	old := s.runtime
	s.runtime = nil
	if err := old.Close(); err != nil {
		log.Printf("Warning: closing the workspace runtime: %v", err)
	}
	rt, err := s.openRuntime(s.root)
	if err != nil {
		log.Printf("Error: the workspace runtime did not open again (%v); the daemon exits", err)
		go s.stop()
		return true
	}
	if report != nil {
		report(rt.Reporter())
	}
	if report != nil {
		// Editors reconnect within a second or two; they see the condition
		// when they attach. It ends when the new runtime is ready, but not
		// before a reconnecting editor can see it.
		go func() {
			select {
			case <-rt.Ready():
			case <-s.stopping:
				return
			}
			select {
			case <-time.After(time.Minute):
				rt.Reporter().Clear(CondIndexReplaced, "")
			case <-s.stopping:
			}
		}()
	}
	s.runtime = rt
	return true
}

// abandon stops the daemon when its root went away, without touching the index.
func (s *server) abandon() {
	s.abandoned.Store(true)
	log.Printf("Error: the project root %s is not there any more (volume ejected, or directory moved or removed); the daemon exits without closing the index", s.root)
	s.stop()
}

// closeConnections closes every open connection.
func (s *server) closeConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.connections {
		_ = conn.Close()
	}
}

// serve accepts connections until the listener closes or the server stops, then
// joins every connection handler. It returns nil for a deliberate shutdown and
// the accept error otherwise.
func (s *server) serve() error {
	backoff := time.Duration(0)
	for {
		conn, acceptErr := s.listener.Accept()
		if acceptErr != nil {
			// Sample the intent before stop() closes it, or every accept error
			// would look like a deliberate shutdown.
			intentional := false
			select {
			case <-s.stopping:
				intentional = true
			default:
			}
			// Running out of file descriptors or losing a connection during the
			// accept is transient. Backing off keeps the workspace alive instead
			// of making one fd spike take the daemon and every client down.
			if !intentional {
				if ne, ok := acceptErr.(net.Error); ok && ne.Temporary() { //nolint:staticcheck // still how net reports retryable accepts
					if backoff == 0 {
						backoff = 5 * time.Millisecond
					} else if backoff *= 2; backoff > time.Second {
						backoff = time.Second
					}
					log.Printf("Daemon accept: %v (retrying in %s)", acceptErr, backoff)
					select {
					case <-time.After(backoff):
					case <-s.stopping:
					}
					continue
				}
			}
			// Either way, connection handlers are joined first: the caller's
			// deferred workspace close checkpoints and closes the store, and a
			// handler still answering a request must not be reaching into it at
			// that moment.
			s.stop()
			s.wg.Wait()
			if intentional {
				return nil
			}
			return acceptErr
		}
		backoff = 0
		if !s.addConn(conn) {
			_ = conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.removeConn(conn)
			defer func() { _ = conn.Close() }()
			// One connection handler must not be able to take the workspace down
			// for every other editor: a panic in one session's handler ends that
			// connection and is logged, but the daemon keeps serving.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("Daemon connection panic: %v\n%s", r, debug.Stack())
				}
			}()
			if err := s.serveConn(conn); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
				log.Printf("Daemon connection: %v", err)
			}
		}()
	}
}

type server struct {
	// root is the spelling the daemon indexes; identity is its symlink-resolved
	// form, which is what a handshake presents.
	root     string
	identity string
	// runtime serves the workspace. A connection holds rtMu for reading for
	// as long as it runs, so that restartRuntime, which takes it for writing,
	// never swaps the runtime under a request.
	rtMu        sync.RWMutex
	runtime     *workspace.Runtime
	openRuntime func(root string) (*workspace.Runtime, error)
	restartMu   sync.Mutex
	restarts    map[string]time.Time
	abandoned   atomic.Bool

	listener    net.Listener
	socket      string
	socketInfo  os.FileInfo
	lockPath    string
	ownership   *Ownership
	lastTouch   time.Time
	socketCheck time.Duration
	idleTimeout time.Duration
	started     time.Time

	ctx       context.Context
	cancelCtx context.CancelFunc

	active   atomic.Int64
	activity chan struct{}
	stopping chan struct{}
	stopOnce sync.Once

	mu          sync.Mutex
	connections map[net.Conn]struct{}
	wg          sync.WaitGroup
}

// addConn registers an accepted connection. It reports false when the server is
// already stopping: a connection accepted just before stop() took its snapshot
// would otherwise be registered after it and never closed, leaving serve to
// wait on a handler the client keeps alive.
func (s *server) addConn(conn net.Conn) bool {
	s.mu.Lock()
	select {
	case <-s.stopping:
		s.mu.Unlock()
		return false
	default:
	}
	s.connections[conn] = struct{}{}
	s.mu.Unlock()
	s.active.Add(1)
	s.touch()
	return true
}

func (s *server) removeConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.connections, conn)
	s.mu.Unlock()
	s.active.Add(-1)
	s.touch()
}

func (s *server) touch() {
	select {
	case s.activity <- struct{}{}:
	default:
	}
}

func (s *server) stop() {
	s.stopOnce.Do(func() {
		close(s.stopping)
		s.cancelCtx()
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.mu.Lock()
		for conn := range s.connections {
			_ = conn.Close()
		}
		s.mu.Unlock()
	})
}

func (s *server) watchIdle() {
	timer := time.NewTimer(s.idleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-s.stopping:
			return
		case <-s.activity:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if s.active.Load() == 0 {
				timer.Reset(s.idleTimeout)
			}
		case <-timer.C:
			if s.active.Load() == 0 {
				s.stop()
				return
			}
			timer.Reset(s.idleTimeout)
		}
	}
}

// conn is one accepted connection's server-side state: a cancellation scope,
// serialized writes, and the watch subscriptions it owns.
type conn struct {
	s        *server
	raw      net.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	sem      chan struct{}
	requests sync.WaitGroup

	// inflight maps the id of each running request to the cancel func of its
	// context, for MethodCancel.
	inflightMu sync.Mutex
	inflight   map[uint64]context.CancelFunc

	writeMu sync.Mutex
	subsMu  sync.Mutex
	subs    map[string]func()
	subSeq  int
}

func (c *conn) write(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeJSONLine(c.raw, v)
}

func (c *conn) notify(method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeJSONLine(c.raw, notification{Method: method, Params: raw})
}

func (c *conn) addSub(cancel func()) string {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	if c.subs == nil {
		c.subs = make(map[string]func())
	}
	c.subSeq++
	id := fmt.Sprintf("w%d", c.subSeq)
	c.subs[id] = cancel
	return id
}

func (c *conn) dropSub(id string) {
	c.subsMu.Lock()
	cancel, ok := c.subs[id]
	if ok {
		delete(c.subs, id)
	}
	c.subsMu.Unlock()
	if ok {
		cancel()
	}
}

func (c *conn) dropAllSubs() {
	c.subsMu.Lock()
	subs := c.subs
	c.subs = nil
	c.subsMu.Unlock()
	for _, cancel := range subs {
		cancel()
	}
}

func (s *server) serveConn(raw net.Conn) error {
	// Held for the whole connection: see server.rtMu.
	s.rtMu.RLock()
	defer s.rtMu.RUnlock()
	if s.runtime == nil {
		return errors.New("the workspace runtime is not open")
	}
	_ = raw.SetReadDeadline(time.Now().Add(serverHandshakeTimeout))
	reader := bufio.NewReader(raw)
	var h hello
	if err := readJSONLine(reader, &h); err != nil {
		return err
	}
	reject := func(reason string) error {
		_ = writeJSONLine(raw, helloResponse{Error: reason, Contract: ContractVersion, PID: os.Getpid()})
		return nil
	}
	// The restart contract: a frontend whose contract version is newer
	// than this daemon's means a newer build is on the machine. Answer with the
	// mismatch and shut down gracefully so the frontend can start the current
	// build immediately; the idle timeout remains the fallback when nobody says
	// so. A frontend that is *older* is told to upgrade, not accommodated, and
	// the daemon keeps serving everyone else.
	rejectVersion := func(reason string, peerNewer bool) error {
		// A frontend that predates the typed-error handling only reads Error,
		// so it has to make sense on its own.
		message := fmt.Sprintf("this workspace is served by a newer dexter build; restart this client from the current dexter binary to attach (%s)", reason)
		if peerNewer {
			message = fmt.Sprintf("a newer dexter build is taking over this workspace; this daemon is shutting down (%s)", reason)
		}
		if err := writeJSONLine(raw, helloResponse{
			Error: message, Contract: ContractVersion,
			PID: os.Getpid(), Incompatible: true, Exiting: peerNewer,
		}); err != nil {
			return err
		}
		if peerNewer {
			go func() {
				// Let the response reach the peer before the listener closes.
				time.Sleep(10 * time.Millisecond)
				s.stop()
			}()
		}
		return nil
	}
	if h.Contract != ContractVersion {
		return rejectVersion(fmt.Sprintf("daemon contract %d, client contract %d", ContractVersion, h.Contract),
			h.Contract > ContractVersion)
	}
	if h.Identity != s.identity {
		return reject("workspace does not match daemon")
	}
	if h.Root != s.root {
		_ = writeJSONLine(raw, helloResponse{
			Error:    fmt.Sprintf("workspace is indexed as %s, not %s", s.root, h.Root),
			Contract: ContractVersion,
			PID:      os.Getpid(),
			Root:     s.root,
		})
		return nil
	}
	if h.Kind != kindControl && h.Kind != kindLSP {
		if _, ok := lookupFrontend(h.Kind); !ok {
			return reject(fmt.Sprintf("unsupported connection kind %q", h.Kind))
		}
	}
	if h.Kind != kindLSP && h.Session != "" {
		if _, ok := s.runtime.Session(h.Session); !ok {
			return reject(fmt.Sprintf("unknown editor session %q", h.Session))
		}
	}

	ctx, cancel := context.WithCancel(s.ctx)
	c := &conn{s: s, raw: raw, ctx: ctx, cancel: cancel, sem: make(chan struct{}, maxConcurrentRequests)}
	defer func() {
		cancel()
		_ = raw.Close()
		c.dropAllSubs()
		c.requests.Wait()
	}()

	if h.Kind == kindLSP {
		id, session, release := s.runtime.AttachLSPSession()
		defer release()
		if err := writeJSONLine(raw, helloResponse{OK: true, Contract: ContractVersion, PID: os.Getpid(), Session: id}); err != nil {
			return err
		}
		_ = raw.SetReadDeadline(time.Time{})
		return lsp.ServeStream(session, lspStream{reader: reader, conn: raw})
	}

	if err := writeJSONLine(raw, helloResponse{OK: true, Contract: ContractVersion, PID: os.Getpid()}); err != nil {
		return err
	}
	_ = raw.SetReadDeadline(time.Time{})

	if h.Kind == kindControl {
		return s.serveControl(c, reader, h.Session)
	}
	frontend, _ := lookupFrontend(h.Kind)
	return frontend.Serve(FrontendConn{
		Conn:    raw,
		Reader:  reader,
		Context: ctx,
		Runtime: s.runtime,
		Session: h.Session,
		LSP:     s.lspResolver(h.Session),
		Notify:  c.notify,
		Done:    ctx.Done(),
	})
}

// lspResolver returns the language service a connection should answer from: the
// editor session it explicitly named, or the headless workspace instance. It
// re-resolves per call so a session that disconnects mid-request degrades to
// disk state instead of failing.
func (s *server) lspResolver(sessionID string) func() *lsp.Server {
	return func() *lsp.Server {
		if sessionID != "" {
			if srv, ok := s.runtime.Session(sessionID); ok {
				return srv
			}
		}
		return s.runtime.LanguageServices()
	}
}

// lspStream adapts a daemon connection to an LSP session's transport. Reads come
// from the buffered reader because it may already hold bytes read alongside the
// handshake; Close closes the socket, so an editor's `exit` ends the session
// rather than leaving it holding a lease on the daemon until the peer hangs up.
type lspStream struct {
	reader *bufio.Reader
	conn   net.Conn
}

func (s lspStream) Read(p []byte) (int, error)  { return s.reader.Read(p) }
func (s lspStream) Write(p []byte) (int, error) { return s.conn.Write(p) }
func (s lspStream) Close() error                { return s.conn.Close() }

// serveControl handles requests concurrently so a long reindex cannot block a
// lookup, and serializes only the writes.
func (s *server) serveControl(c *conn, reader *bufio.Reader, sessionID string) error {
	base := MethodContext{
		Context: c.ctx,
		Runtime: s.runtime,
		Session: sessionID,
		LSP:     s.lspResolver(sessionID),
		Notify:  c.notify,
		Done:    c.ctx.Done(),
	}
	for {
		var req request
		if err := readJSONLine(reader, &req); err != nil {
			return err
		}
		if req.Method == MethodCancel {
			// Handled on the reader, outside the request limit, so a cancel
			// gets through when every slot is taken.
			var params CancelParams
			if err := json.Unmarshal(req.Params, &params); err == nil {
				c.cancelRequest(params.ID)
			}
			continue
		}
		select {
		case c.sem <- struct{}{}:
		case <-c.ctx.Done():
			return c.ctx.Err()
		default:
			_ = c.write(response{ID: req.ID, Error: "too many concurrent control requests"})
			continue
		}
		c.requests.Add(1)
		// Each request has its own context, so a cancel ends its waits and
		// frees its slot before the connection ends.
		mc := base
		var cancel context.CancelFunc
		mc.Context, cancel = context.WithCancel(c.ctx)
		c.trackRequest(req.ID, cancel)
		go func(req request) {
			res := response{ID: req.ID}
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Printf("Daemon control method %q panic: %v\n%s", req.Method, recovered, debug.Stack())
					res.Result = nil
					res.Error = fmt.Sprintf("control method %q panicked", req.Method)
				}
				if err := c.write(res); errors.Is(err, errLineTooLarge) {
					// Nothing was written, so the stream is still in step and only
					// this call fails; the connection's other calls carry on.
					log.Printf("Daemon control method %q: %v", req.Method, err)
					_ = c.write(response{ID: req.ID, Error: fmt.Sprintf("%s result is too large to send (%d bytes, limit %d); narrow the query", req.Method, len(res.Result), maxProtocolLine)})
				}
				c.untrackRequest(req.ID)
				cancel()
				<-c.sem
				c.requests.Done()
			}()
			result, err := s.handleRequest(c, mc, req)
			if err != nil {
				res.Error = err.Error()
			} else if result != nil {
				res.Result, err = json.Marshal(result)
				if err != nil {
					res.Error = err.Error()
				}
			}
		}(req)
	}
}

func (c *conn) trackRequest(id uint64, cancel context.CancelFunc) {
	c.inflightMu.Lock()
	if c.inflight == nil {
		c.inflight = make(map[uint64]context.CancelFunc)
	}
	c.inflight[id] = cancel
	c.inflightMu.Unlock()
}

func (c *conn) untrackRequest(id uint64) {
	c.inflightMu.Lock()
	delete(c.inflight, id)
	c.inflightMu.Unlock()
}

func (c *conn) cancelRequest(id uint64) {
	c.inflightMu.Lock()
	cancel := c.inflight[id]
	c.inflightMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *server) handleRequest(c *conn, mc MethodContext, req request) (any, error) {
	switch req.Method {
	case MethodStatus:
		return Status{
			Root:      s.root,
			PID:       os.Getpid(),
			Version:   version.Version,
			Contract:  ContractVersion,
			Ready:     s.runtime.IsReady(),
			Clients:   s.active.Load(),
			UptimeMs:  time.Since(s.started).Milliseconds(),
			Frontends: RegisteredFrontends(),
		}, nil
	case MethodShutdown:
		var params ShutdownParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return nil, err
			}
		}
		if !params.Force && s.active.Load() > 1 {
			return nil, fmt.Errorf("workspace has %d other attached clients", s.active.Load()-1)
		}
		// Let the response reach the maintenance client before closing the
		// listener and its connections.
		go func() {
			time.Sleep(10 * time.Millisecond)
			s.stop()
		}()
		return struct{}{}, nil
	case MethodWorkspaceStatus:
		var params StatusParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return nil, err
			}
		}
		s.waitReady(mc.Context, req.Method, params.WaitReadyMs)
		st, err := s.runtime.IndexStatus()
		if err != nil {
			return nil, err
		}
		out := WorkspaceStatus{
			Root: st.Root, Ready: st.Ready, Watching: st.Watching, StdlibRoot: st.StdlibRoot,
			IndexVersion: st.IndexVersion, ExpectedIndexVersion: st.ExpectedIndexVersion,
			Files: st.Files, Definitions: st.Definitions, References: st.References,
			Conditions: notesFrom(s.runtime.Reporter().Conditions()),
		}
		for _, sess := range s.runtime.Sessions() {
			out.Sessions = append(out.Sessions, SessionStatus{
				ID:            sess.ID,
				OpenDocuments: sess.OpenDocuments,
				ConnectedMs:   time.Since(sess.ConnectedAt).Milliseconds(),
			})
		}
		return out, nil
	case MethodLookup:
		var params LookupParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, err
		}
		s.waitReady(mc.Context, req.Method, params.WaitReadyMs)
		locations, err := mc.LSP().LookupName(params.Module, params.Function, lsp.NameLookupOptions{
			FollowDelegates:  params.FollowDelegates,
			FallbackToModule: !params.Strict,
			ExactModule:      params.Strict,
		})
		if err != nil {
			return nil, err
		}
		return LookupResult{Locations: locations, Ready: s.runtime.IsReady(), Notes: notesFrom(s.runtime.IndexConditions())}, err
	case MethodReferences:
		var params ReferencesParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, err
		}
		s.waitReady(mc.Context, req.Method, params.WaitReadyMs)
		locations, err := mc.LSP().ReferenceNames(params.Module, params.Function, lsp.NameReferenceOptions{
			Context:         mc.Context, // a client that disconnects stops the search
			FollowDelegates: true,
			ExcludeStdlib:   true,
		})
		if err != nil {
			return nil, err
		}
		return ReferencesResult{Locations: locations, Ready: s.runtime.IsReady(), Notes: notesFrom(s.runtime.IndexConditions())}, err
	case MethodReindex:
		var params ReindexParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, err
		}
		start := time.Now()
		var err error
		missing := false
		if params.Target == "" {
			err = s.runtime.Reindex(mc.Context)
		} else {
			// Checked before the reindex, which prunes whatever was indexed.
			if _, statErr := os.Stat(params.Target); lsp.IsMissing(statErr) {
				indexed, indexErr := s.runtime.Indexes(params.Target)
				if indexErr != nil {
					return nil, indexErr
				}
				missing = !indexed
			}
			err = s.runtime.ReindexPath(mc.Context, params.Target)
		}
		return ReindexResult{Elapsed: time.Since(start).Round(time.Millisecond), Missing: missing, Notes: notesFrom(s.runtime.IndexConditions())}, err
	case MethodWatch:
		var params WatchParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return nil, err
			}
		}
		return s.watch(c, mc, params)
	case MethodUnwatch:
		var params Subscription
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, err
		}
		c.dropSub(params.ID)
		return struct{}{}, nil
	}
	if handler, ok := lookupMethod(req.Method); ok {
		return handler(mc, req.Params)
	}
	return nil, fmt.Errorf("unknown daemon method %q", req.Method)
}

// watch subscribes this connection to coalesced index mutations and pushes them
// until the subscription is dropped or the connection ends. The workspace owns
// the only watchers, so a frontend uses this instead of watching the tree again.
func (s *server) watch(c *conn, mc MethodContext, params WatchParams) (any, error) {
	changes, cancel := s.runtime.Subscribe(params.Buffer)
	// The subscription outlives the request that made it: it ends with the
	// connection or an unwatch.
	ctx, stop := context.WithCancel(c.ctx)
	id := c.addSub(func() {
		stop()
		cancel()
	})
	go func() {
		defer cancel()
		defer func() {
			if value := recover(); value != nil {
				log.Printf("Error: panic in a workspace/watch subscription: %v\n%s", value, debug.Stack())
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case change, ok := <-changes:
				if !ok {
					return
				}
				if err := notifyChange(mc.Notify, id, change); err != nil {
					return
				}
			}
		}
	}()
	return Subscription{ID: id}, nil
}

// notifyChange pushes one index change to a watch subscription. A change with
// too many paths for one protocol line is reported as a full change, as the
// runtime does when a subscriber drops changes, so the subscriber still learns
// that something changed instead of silently losing the subscription.
func notifyChange(notify func(method string, params any) error, id string, change workspace.Change) error {
	err := notify(NotificationChanged, Changed{
		Subscription: id,
		Full:         change.Full,
		Paths:        change.Paths,
	})
	if errors.Is(err, errLineTooLarge) {
		err = notify(NotificationChanged, Changed{Subscription: id, Full: true})
	}
	return err
}

// readyLogThreshold is how long a request must actually have blocked on the
// initial build before it earns a line in the daemon log.
const readyLogThreshold = 25 * time.Millisecond

// waitReady blocks up to ms for the initial reconciliation. Zero answers
// immediately from whatever is indexed, which keeps a cold CLI call fast.
//
// A request that does block is logged with how long it cost: the caller asked to
// wait, and that duration is the number that decides whether waiting was the
// right call. This is the daemon-side record; the client-facing note is separate
// and suppressible, and every response carries `ready` so a programmatic caller
// needs neither.
func (s *server) waitReady(ctx context.Context, method string, ms int) {
	if ms <= 0 || s.runtime.IsReady() {
		return
	}
	d := time.Duration(ms) * time.Millisecond
	if d > maxReadyWait {
		d = maxReadyWait
	}
	waitCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	start := time.Now()
	_ = s.runtime.WaitReady(waitCtx)
	if waited := time.Since(start); waited >= readyLogThreshold {
		log.Printf("%s blocked %s on the initial index build (ready: %v)", method, waited.Round(time.Millisecond), s.runtime.IsReady())
	}
}
