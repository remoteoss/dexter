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
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remoteoss/dexter/internal/lsp"
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

// socketCheckInterval is how often the daemon verifies that its socket file
// still exists. A variable so tests can shrink it.
var socketCheckInterval = 30 * time.Second

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
)

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

type LookupResult struct {
	Locations []lsp.NameLocation `json:"locations"`
	Ready     bool               `json:"ready"`
}

type ReferencesParams struct {
	Module      string `json:"module"`
	Function    string `json:"function,omitempty"`
	WaitReadyMs int    `json:"waitReadyMs,omitempty"`
}

type ReferencesResult struct {
	Locations []lsp.NameLocation `json:"locations"`
	Ready     bool               `json:"ready"`
}

type ReindexParams struct {
	Target string `json:"target"`
}

type ReindexResult struct {
	Elapsed time.Duration `json:"elapsed"`
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
	ownership, endpoint, err := AcquireOwnership(root)
	if err != nil {
		return err
	}
	defer func() { _ = ownership.Release() }()

	listener, err := listenSocket(endpoint)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(endpoint.Socket)
	}()

	runtime, err := workspace.Open(endpoint.Root)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := runtime.Close(); closeErr != nil {
			log.Printf("Warning: closing workspace: %v", closeErr)
		}
	}()

	s := newServer(ctx, endpoint, runtime, listener, idleTimeout)
	defer s.cancelCtx()
	log.Printf("Dexter daemon v%s listening (root: %s, socket: %s)", version.Version, endpoint.Root, endpoint.Socket)
	return s.serve()
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
		listener:    listener,
		socket:      endpoint.Socket,
		socketCheck: socketCheckInterval,
		idleTimeout: idleTimeout,
		started:     time.Now(),
		activity:    make(chan struct{}, 1),
		stopping:    make(chan struct{}),
		connections: make(map[net.Conn]struct{}),
	}
	s.ctx, s.cancelCtx = context.WithCancel(ctx)
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
	go s.watchSocket()
	return s
}

// watchSocket exits the daemon if its socket disappears from under it: a temp
// cleaner, or anything else that removes runtime files. Without this the daemon
// keeps the ownership lock while being unreachable, and nothing can replace it
// until the idle timeout or a forced stop. Exiting releases the lock, and the
// next frontend starts a daemon whose socket exists.
func (s *server) watchSocket() {
	ticker := time.NewTicker(s.socketCheck)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopping:
			return
		case <-ticker.C:
			if _, err := os.Stat(s.socket); os.IsNotExist(err) {
				log.Printf("Daemon socket %s disappeared; exiting so a replacement can start", s.socket)
				s.stop()
				return
			}
		}
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
	root        string
	identity    string
	runtime     *workspace.Runtime
	listener    net.Listener
	socket      string
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
	mc := MethodContext{
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
		select {
		case c.sem <- struct{}{}:
		case <-c.ctx.Done():
			return c.ctx.Err()
		default:
			_ = c.write(response{ID: req.ID, Error: "too many concurrent control requests"})
			continue
		}
		c.requests.Add(1)
		go func(req request) {
			res := response{ID: req.ID}
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Printf("Daemon control method %q panic: %v\n%s", req.Method, recovered, debug.Stack())
					res.Result = nil
					res.Error = fmt.Sprintf("control method %q panicked", req.Method)
				}
				_ = c.write(res)
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
		})
		if err != nil {
			return nil, err
		}
		return LookupResult{Locations: locations, Ready: s.runtime.IsReady()}, err
	case MethodReferences:
		var params ReferencesParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, err
		}
		s.waitReady(mc.Context, req.Method, params.WaitReadyMs)
		locations, err := mc.LSP().ReferenceNames(params.Module, params.Function, lsp.NameReferenceOptions{
			FollowDelegates: true,
			ExcludeStdlib:   true,
		})
		if err != nil {
			return nil, err
		}
		return ReferencesResult{Locations: locations, Ready: s.runtime.IsReady()}, err
	case MethodReindex:
		var params ReindexParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, err
		}
		start := time.Now()
		var err error
		if params.Target == "" {
			err = s.runtime.Reindex(mc.Context)
		} else {
			err = s.runtime.ReindexPath(mc.Context, params.Target)
		}
		return ReindexResult{Elapsed: time.Since(start).Round(time.Millisecond)}, err
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
	ctx, stop := context.WithCancel(mc.Context)
	id := c.addSub(func() {
		stop()
		cancel()
	})
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case change, ok := <-changes:
				if !ok {
					return
				}
				if err := mc.Notify(NotificationChanged, Changed{
					Subscription: id,
					Full:         change.Full,
					Paths:        change.Paths,
				}); err != nil {
					return
				}
			}
		}
	}()
	return Subscription{ID: id}, nil
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
