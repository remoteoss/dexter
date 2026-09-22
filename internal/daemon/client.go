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
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// defaultStartTimeout bounds waiting for a spawned daemon to accept
	// connections. The daemon binds before it opens the workspace, so this only
	// has to cover process start plus stdlib detection. It is deliberately not
	// generous: a daemon that cannot bind should fail its frontend promptly
	// instead of leaving an editor staring at a silent startup.
	defaultStartTimeout = 30 * time.Second

	// clientHandshakeTimeout bounds waiting for a daemon to answer the
	// handshake. The socket may already be listening while the workspace is
	// still being opened, so this is generous; it exists so a wedged daemon
	// surfaces an error instead of hanging an editor or a CLI call forever.
	clientHandshakeTimeout = 30 * time.Second

	// ownerBindGrace is how long Ensure waits for a live workspace owner to serve
	// its socket before concluding that no daemon is coming. The daemon binds
	// before it opens the workspace, so the window this covers is the few
	// instructions between taking the ownership lock and listening; a second or
	// two would only pause a frontend that cannot be served anyway.
	ownerBindGrace = 500 * time.Millisecond

	// maxDaemonLogBytes bounds the per-workspace daemon log. It is append-only
	// across runs, and debug logging in a long-lived workspace would otherwise
	// grow it without limit, so a new daemon starts with a clean file once the
	// old one passes a size a human would still read.
	maxDaemonLogBytes = 4 << 20
)

// Client is a multiplexed control connection to one workspace daemon. Calls may
// run concurrently, and the daemon may push notifications between responses.
type Client struct {
	conn    net.Conn
	reader  *bufio.Reader
	writeMu sync.Mutex
	nextID  atomic.Uint64

	pendingMu sync.Mutex
	pending   map[uint64]chan response

	notifyMu  sync.RWMutex
	handlers  map[string]map[uint64]func(json.RawMessage)
	notifySeq atomic.Uint64

	readLoopDone chan struct{}
	readLoopErr  atomic.Value // error
	closeOnce    sync.Once
	closeErr     error
}

// Dial connects to an already-running daemon.
func Dial(ctx context.Context, root string) (*Client, error) {
	conn, reader, _, err := dialKind(ctx, root, kindControl, "")
	if err != nil {
		return nil, err
	}
	return newClient(conn, reader), nil
}

// newClient wires the multiplexed reader onto an already handshaken connection.
// reader must be the one the handshake used, since it can hold bytes read
// alongside the handshake response.
func newClient(conn net.Conn, reader *bufio.Reader) *Client {
	c := &Client{
		conn:         conn,
		reader:       reader,
		pending:      make(map[uint64]chan response),
		handlers:     make(map[string]map[uint64]func(json.RawMessage)),
		readLoopDone: make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Ensure connects to the workspace daemon, starting it when necessary.
func Ensure(ctx context.Context, root string) (*Client, error) {
	client, err := Dial(ctx, root)
	if err == nil {
		return client, nil
	}
	// A daemon built by another version owns this workspace and cannot be
	// replaced by starting a second one; report it now instead of waiting out
	// the start timeout for a daemon that will never appear.
	var incompatible *IncompatibleDaemonError
	if errors.As(err, &incompatible) {
		if !incompatible.ClientNewer {
			// The daemon is newer than this frontend, so replacing it would be a
			// downgrade; the fix is restarting this client from the current
			// binary. Report it without waiting for a daemon that will not appear.
			return nil, incompatible
		}
		// An upgrade left a daemon from an older build owning the workspace. It
		// may already be stepping aside under the restart contract, or have to be
		// signaled because it predates it; either way, wait for its lock before
		// starting the current build. Editors attached to the old process have to
		// restart either way, because their proxy is the old binary too.
		log.Printf("Replacing older workspace daemon (pid %d): %s; restart editors to reconnect", incompatible.DaemonPID, incompatible.Reason)
		if replaceErr := ReplaceIncompatible(ctx, root, incompatible); replaceErr != nil {
			return nil, errors.Join(incompatible, replaceErr)
		}
		if client, err := Dial(ctx, root); err == nil {
			return client, nil
		}
	} else if ownership, _, ownErr := AcquireOwnership(root); errors.Is(ownErr, ErrWorkspaceOwned) {
		client, dialErr := dialWithin(ctx, root, ownerBindGrace)
		if client != nil {
			return client, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !socketExists(root) {
			return nil, fmt.Errorf("workspace %s is owned by another dexter process that is not serving its daemon socket (a `dexter init` may be rebuilding the index); retry when it finishes: %w", root, dialErr)
		}
		// The socket is there but not answering yet. A spawned daemon will lose
		// the ownership race, but the retry loop below still rides out a slow
		// startup and reports the log if the daemon never answers.
	} else if ownErr == nil {
		if err := ownership.Release(); err != nil {
			return nil, err
		}
	}

	if err := spawnDaemon(root); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(defaultStartTimeout)
	backoff := 5 * time.Millisecond
	var lastErr error
	for time.Now().Before(deadline) {
		client, err := Dial(ctx, root)
		if err == nil {
			return client, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 250*time.Millisecond {
			backoff *= 2
		}
	}
	hint := ""
	if endpoint, err := ResolveEndpoint(root); err == nil {
		hint = fmt.Sprintf(" (daemon log: %s)", endpoint.Log)
	}
	return nil, fmt.Errorf("daemon did not become ready%s: %w", hint, lastErr)
}

// dialWithin polls for a daemon connection for up to grace. A nil client with a
// nil error is impossible: the last dial error is returned when the grace runs
// out.
func dialWithin(ctx context.Context, root string, grace time.Duration) (*Client, error) {
	deadline := time.Now().Add(grace)
	var lastErr error
	for {
		client, err := Dial(ctx, root)
		if err == nil {
			return client, nil
		}
		lastErr = err
		if !time.Now().Before(deadline) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// socketExists reports whether the workspace socket path is present. It is a
// hint, not a verdict: a socket may appear right after the check, so it decides
// how to report a slow owner, never whether the workspace is usable.
func socketExists(root string) bool {
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		return false
	}
	_, err = os.Stat(endpoint.Socket)
	return err == nil
}

// AcquireMaintenance obtains exclusive workspace ownership for an offline
// operation such as `dexter init`. An idle daemon is asked to exit; a daemon
// with another attached client refuses the request.
func AcquireMaintenance(ctx context.Context, root string) (*Ownership, Endpoint, error) {
	ownership, endpoint, err := AcquireOwnership(root)
	if err == nil || !errors.Is(err, ErrWorkspaceOwned) {
		return ownership, endpoint, err
	}
	client, dialErr := Dial(ctx, root)
	if dialErr != nil {
		var incompatible *IncompatibleDaemonError
		if errors.As(dialErr, &incompatible) && incompatible.ClientNewer {
			if err := ReplaceIncompatible(ctx, root, incompatible); err != nil {
				return nil, endpoint, err
			}
			return AcquireMaintenance(ctx, root)
		}
		return nil, endpoint, fmt.Errorf("workspace is owned but its daemon is not accepting requests: %w", dialErr)
	}
	callErr := client.Call(ctx, MethodShutdown, ShutdownParams{}, nil)
	_ = client.Close()
	if callErr != nil {
		return nil, endpoint, callErr
	}

	backoff := 5 * time.Millisecond
	for {
		ownership, endpoint, err = AcquireOwnership(root)
		if err == nil || !errors.Is(err, ErrWorkspaceOwned) {
			return ownership, endpoint, err
		}
		select {
		case <-ctx.Done():
			return nil, endpoint, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 100*time.Millisecond {
			backoff *= 2
		}
	}
}

// readLoop demultiplexes the connection: notifications go to handlers, responses
// go to the caller waiting on their id. It is the only reader.
func (c *Client) readLoop() {
	defer close(c.readLoopDone)
	for {
		var env envelope
		if err := readJSONLine(c.reader, &env); err != nil {
			c.readLoopErr.Store(err)
			c.failPending(err)
			return
		}
		if env.Method != "" && env.ID == 0 {
			c.dispatch(env.Method, env.Params)
			continue
		}
		c.pendingMu.Lock()
		ch, ok := c.pending[env.ID]
		if ok {
			delete(c.pending, env.ID)
		}
		c.pendingMu.Unlock()
		if ok {
			ch <- response{ID: env.ID, Result: env.Result, Error: env.Error}
		}
	}
}

func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[uint64]chan response)
	c.pendingMu.Unlock()
	for _, ch := range pending {
		ch <- response{Error: err.Error()}
	}
}

func (c *Client) dispatch(method string, params json.RawMessage) {
	c.notifyMu.RLock()
	registered := c.handlers[method]
	handlers := make([]func(json.RawMessage), 0, len(registered))
	for _, h := range registered {
		handlers = append(handlers, h)
	}
	c.notifyMu.RUnlock()
	for _, h := range handlers {
		h(params)
	}
}

// OnNotify registers a handler for one server-initiated method and returns a
// func that removes it. Handlers run on the reader goroutine, so they must not
// block; copy what they need and hand it to another goroutine. Several handlers
// may share one method; their relative order is unspecified.
func (c *Client) OnNotify(method string, h func(json.RawMessage)) func() {
	id := c.notifySeq.Add(1)
	c.notifyMu.Lock()
	if c.handlers[method] == nil {
		c.handlers[method] = make(map[uint64]func(json.RawMessage))
	}
	c.handlers[method][id] = h
	c.notifyMu.Unlock()
	return func() {
		c.notifyMu.Lock()
		defer c.notifyMu.Unlock()
		delete(c.handlers[method], id)
	}
}

// Call invokes one versioned daemon control method. Concurrent calls are
// supported; each waits only on its own response.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.pendingMu.Lock()
	if c.pending == nil {
		c.pendingMu.Unlock()
		return errors.New("daemon connection is closed")
	}
	c.pending[id] = ch
	c.pendingMu.Unlock()

	c.writeMu.Lock()
	writeErr := writeJSONLine(c.conn, request{ID: id, Method: method, Params: raw})
	c.writeMu.Unlock()
	if writeErr != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return writeErr
	}

	select {
	case res := <-ch:
		if res.Error != "" {
			return errors.New(res.Error)
		}
		if result == nil || len(res.Result) == 0 {
			return nil
		}
		return json.Unmarshal(res.Result, result)
	case <-c.readLoopDone:
		return c.readError()
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return ctx.Err()
	}
}

func (c *Client) readError() error {
	if v := c.readLoopErr.Load(); v != nil {
		if err, ok := v.(error); ok && !errors.Is(err, io.EOF) {
			return fmt.Errorf("daemon connection closed: %w", err)
		}
	}
	return errors.New("daemon connection closed")
}

// Watch subscribes to coalesced index mutations over the control connection and
// reports them to onChange until the returned cancel runs or the connection
// ends. Frontends use this instead of watching the workspace themselves.
func (c *Client) Watch(ctx context.Context, buffer int, onChange func(Changed)) (func(), error) {
	var subMu sync.Mutex
	subscription := ""
	remove := c.OnNotify(NotificationChanged, func(raw json.RawMessage) {
		var change Changed
		if err := json.Unmarshal(raw, &change); err != nil {
			return
		}
		subMu.Lock()
		want := subscription
		subMu.Unlock()
		if want != "" && change.Subscription != want {
			return
		}
		onChange(change)
	})

	var res Subscription
	if err := c.Call(ctx, MethodWatch, WatchParams{Buffer: buffer}, &res); err != nil {
		remove()
		return nil, err
	}
	subMu.Lock()
	subscription = res.ID
	subMu.Unlock()

	return func() {
		remove()
		// Detach even if the caller's context is already done; the daemon
		// cleans up on disconnect anyway, so this is best effort and short.
		detachCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = c.Call(detachCtx, MethodUnwatch, res, nil)
	}, nil
}

// DaemonStatus reports the daemon's own state.
func (c *Client) DaemonStatus(ctx context.Context) (Status, error) {
	var status Status
	err := c.Call(ctx, MethodStatus, struct{}{}, &status)
	return status, err
}

// WorkspaceStatus reports readiness and index size, optionally waiting up to
// waitReadyMs for the initial reconciliation.
func (c *Client) WorkspaceStatus(ctx context.Context, waitReadyMs int) (WorkspaceStatus, error) {
	var status WorkspaceStatus
	err := c.Call(ctx, MethodWorkspaceStatus, StatusParams{WaitReadyMs: waitReadyMs}, &status)
	return status, err
}

// Close closes the control connection.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.conn.Close()
	})
	<-c.readLoopDone
	return c.closeErr
}

// OpenLSP opens a raw LSP stream after the daemon handshake. The returned
// session id identifies this editor session to the daemon; pass it to another
// frontend that should see these unsaved buffers.
func OpenLSP(ctx context.Context, root string) (conn net.Conn, reader *bufio.Reader, sessionID string, err error) {
	conn, reader, hello, err := dialKind(ctx, root, kindLSP, "")
	return conn, reader, hello.Session, err
}

// IncompatibleDaemonError reports a daemon this build cannot share a workspace
// with, because the contract version differs. The direction matters:
// when this build is newer the daemon has to step aside (it may already be
// doing so under the restart contract, or its pid can be signaled when it
// predates the contract); when the daemon is newer, the frontend is the one
// that needs upgrading.
type IncompatibleDaemonError struct {
	Reason         string
	DaemonContract int
	DaemonPID      int
	// ClientNewer is true when this build is newer than the daemon.
	ClientNewer bool
	// Exiting is true when the daemon understood the restart contract and is
	// shutting itself down, so the frontend only has to wait for its lock.
	Exiting bool
}

func (e *IncompatibleDaemonError) Error() string {
	what := e.Reason
	if what == "" {
		what = fmt.Sprintf("daemon contract %d, client contract %d", e.DaemonContract, ContractVersion)
	}
	if !e.ClientNewer {
		return fmt.Sprintf("%s; this frontend is older than the running daemon, upgrade Dexter", what)
	}
	if e.Exiting {
		return fmt.Sprintf("%s; the daemon is shutting down to make room for this build", what)
	}
	if e.DaemonPID > 0 {
		return fmt.Sprintf("%s; run `dexter stop --force` to replace daemon pid %d", what, e.DaemonPID)
	}
	return fmt.Sprintf("%s; run `dexter stop --force` to locate and replace it", what)
}

// incompatibleDaemonError builds the typed mismatch and its direction.
func incompatibleDaemonError(res helloResponse) *IncompatibleDaemonError {
	return &IncompatibleDaemonError{
		Reason:         res.Error,
		DaemonContract: res.Contract,
		DaemonPID:      res.PID,
		ClientNewer:    ContractVersion > res.Contract,
		Exiting:        res.Exiting,
	}
}

func dialKind(ctx context.Context, root, kind, session string) (net.Conn, *bufio.Reader, helloResponse, error) {
	return dialKindTimeout(ctx, root, kind, session, clientHandshakeTimeout)
}

// dialKindTimeout is dialKind with a caller-chosen handshake deadline, for
// callers that must not stall on a wedged socket.
func dialKindTimeout(ctx context.Context, root, kind, session string, timeout time.Duration) (net.Conn, *bufio.Reader, helloResponse, error) {
	var none helloResponse
	ep, err := ResolveEndpoint(root)
	if err != nil {
		return nil, nil, none, err
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", ep.Socket)
	if err != nil {
		return nil, nil, none, err
	}
	reader := bufio.NewReader(conn)
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = conn.Close()
		return nil, nil, none, err
	}
	if err := writeJSONLine(conn, hello{Contract: ContractVersion, Kind: kind, Identity: ep.Identity, Session: session}); err != nil {
		_ = conn.Close()
		return nil, nil, none, err
	}
	var res helloResponse
	if err := readJSONLine(reader, &res); err != nil {
		_ = conn.Close()
		return nil, nil, none, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, nil, none, err
	}
	if !res.OK {
		_ = conn.Close()
		if res.Incompatible || res.Contract != ContractVersion {
			return nil, nil, none, incompatibleDaemonError(res)
		}
		return nil, nil, none, errors.New(res.Error)
	}
	if res.Contract != ContractVersion {
		// A daemon that predates the version checks answered anyway; it is
		// still a build this one cannot share a workspace with.
		_ = conn.Close()
		return nil, nil, none, incompatibleDaemonError(res)
	}
	return conn, reader, res, nil
}

// parseDaemonPid finds the workspace daemon in `ps -A -o uid=,pid=,args=`
// output. A daemon's command line ends with "daemon <root>", which is specific
// enough to locate a process whose handshake could not carry a pid; a lookup or
// lsp invocation never ends that way. Only this user's processes are eligible,
// so a shared machine cannot make dexter signal someone else's daemon.
func parseDaemonPid(psOutput, root string) (int, bool) {
	suffix := "daemon " + root
	uid := strconv.Itoa(os.Getuid())
	for _, line := range strings.Split(psOutput, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasSuffix(line, suffix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != uid {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil || pid == os.Getpid() {
			continue
		}
		return pid, true
	}
	return 0, false
}

// FindDaemonProcess reports the pid of the daemon serving root, located by
// command line. It is diagnostic; StopForced is the action.
func FindDaemonProcess(root string) (int, bool) { return findDaemonProcess(root) }

// StopForced terminates the workspace daemon without a protocol handshake by
// locating its process. It is the recovery path for a wedged daemon and the
// only path that works when a daemon from another build predates every
// pid-carrying refusal. It reports whether a matching process was found.
func StopForced(ctx context.Context, root string) (bool, error) {
	pid, ok := findDaemonProcess(root)
	if !ok {
		return false, nil
	}
	return true, stopPid(ctx, root, pid, signalGrace)
}

// signalGrace is how long an explicitly forced stop waits for a daemon to exit
// before killing it. A variable so tests can shrink it.
var signalGrace = 5 * time.Second

// replacementGrace is the same wait for the automatic replacement path, where
// the older daemon may be draining a long index pass it started. It is much
// longer than an explicit force: an automatic swap must never cost a healthy
// daemon work it was legitimately finishing, while `dexter stop --force` is the
// user saying they want it gone now.
var replacementGrace = 30 * time.Second

func stopPid(ctx context.Context, root string, pid int, grace time.Duration) error {
	if !processAlive(pid) {
		return nil
	}
	if err := terminatePlatformProcess(pid); err != nil {
		return fmt.Errorf("stop daemon pid %d: %w", pid, err)
	}
	if err := waitForProcessExit(ctx, pid, grace); err == nil {
		return nil
	}
	log.Printf("Daemon pid %d did not exit after SIGTERM; killing it", pid)
	if err := killPlatformProcess(pid); err != nil {
		return fmt.Errorf("kill daemon pid %d: %w", pid, err)
	}
	if err := waitForProcessExit(ctx, pid, grace); err == nil {
		return nil
	}
	// A zombie whose parent is not reaping reports as alive but cannot hold the
	// workspace lock, which the kernel releases at process exit.
	if ownership, _, ownErr := AcquireOwnership(root); ownErr == nil {
		return ownership.Release()
	}
	return fmt.Errorf("daemon pid %d did not exit; it may be wedged", pid)
}

// waitForProcessExit polls until the process itself is gone. The workspace lock
// is not a substitute: a process that never held it (or whose lock file was
// removed) would make a lock-based wait report success while the process lives.
func waitForProcessExit(ctx context.Context, pid int, grace time.Duration) error {
	deadline := time.Now().Add(grace)
	for {
		if !processAlive(pid) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("process %d is still running", pid)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// waitForWorkspaceFree waits until a compatible daemon serves the workspace or
// the ownership lock is released, so a caller can start the current build.
func waitForWorkspaceFree(ctx context.Context, root string, grace time.Duration) error {
	deadline := time.Now().Add(grace)
	for {
		if client, err := Dial(ctx, root); err == nil {
			_ = client.Close()
			return nil // a compatible daemon is already serving
		}
		ownership, _, ownErr := AcquireOwnership(root)
		if ownErr == nil {
			return ownership.Release() // the workspace lock is free
		}
		if !errors.Is(ownErr, ErrWorkspaceOwned) {
			return fmt.Errorf("waiting for workspace %s to be free: %w", root, ownErr)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("workspace %s is still owned; its daemon may be wedged", root)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// ReplaceIncompatible clears a daemon this build cannot share the workspace
// with. A daemon that understands the restart contract is already shutting
// itself down and only has to be waited out; one that predates the contract is
// signaled by the pid its refusal carried, or located by command line when the
// refusal predates the pid too. Either way it returns once the workspace
// ownership lock is free, so the caller can start the current build.
func ReplaceIncompatible(ctx context.Context, root string, e *IncompatibleDaemonError) error {
	if e == nil {
		return errors.New("no daemon to replace")
	}
	if !e.Exiting {
		pid := e.DaemonPID
		if pid <= 0 {
			found, ok := findDaemonProcess(root)
			if !ok {
				return fmt.Errorf("no daemon from another build reports a pid or matches `dexter daemon %s`; it will be replaced on its next idle exit", root)
			}
			pid = found
		}
		// Last-moment check: a compatible daemon answering now means the one we
		// were about to signal is gone or superseded, and killing on stale
		// information is the one way this path could hurt a healthy workspace.
		// A short deadline keeps a wedged socket from stalling the replacement.
		if conn, _, _, err := dialKindTimeout(ctx, root, kindControl, "", 2*time.Second); err == nil {
			_ = conn.Close()
			return nil
		}
		return stopPid(ctx, root, pid, replacementGrace)
	}
	if e.DaemonPID > 0 {
		if err := waitForWorkspaceFree(ctx, root, replacementGrace); err == nil {
			return nil
		}
		// The contract said it was exiting; it is not. Escalate.
		log.Printf("Daemon pid %d did not finish exiting; killing it", e.DaemonPID)
		if err := killPlatformProcess(e.DaemonPID); err != nil {
			return fmt.Errorf("kill daemon pid %d: %w", e.DaemonPID, err)
		}
	}
	return waitForWorkspaceFree(ctx, root, replacementGrace)
}

// spawnDaemon starts the detached workspace daemon. It is a variable so tests
// can exercise Ensure's replacement decision without launching a process.
var spawnDaemon = spawn

func spawn(root string) error {
	ep, err := ResolveEndpoint(root)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	// The log is append-only across daemon runs; a debug-heavy editor can make
	// it grow for as long as the workspace lives, so truncate it when it gets
	// past a size a human would still read. The current run starts clean.
	if info, statErr := os.Stat(ep.Log); statErr == nil && info.Size() > maxDaemonLogBytes {
		_ = os.Truncate(ep.Log, 0)
	}
	logFile, err := os.OpenFile(ep.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	cmd := exec.Command(executable, "daemon", ep.Root)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Run from the workspace, not from whatever directory the frontend happened
	// to be in: version-manager and stdlib detection resolve the active Elixir
	// from the project, and an editor often spawns us from /.
	cmd.Dir = ep.Root
	configureDetachedProcess(&cmd.SysProcAttr)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start workspace daemon: %w", err)
	}
	_ = logFile.Close()
	// Reap the child if it exits while this frontend is still alive — the loser
	// of a startup race does exactly that, and an editor can live for hours.
	// The daemon normally outlives this process and is reparented when it exits.
	go func() { _ = cmd.Wait() }()
	return nil
}

// ProxyLSP connects stdio to a daemon-hosted LSP session.
func ProxyLSP(ctx context.Context, root string, in io.Reader, out io.Writer) error {
	return ProxyFrontend(ctx, root, kindLSP, "", in, out)
}

// ProxyFrontend connects stdio to a daemon-hosted protocol adapter without
// decoding or re-encoding messages after the handshake, so the daemon hop costs
// a copy rather than a parse. session names an editor session the adapter should
// share overlays with; empty means headless.
func ProxyFrontend(ctx context.Context, root, kind, session string, in io.Reader, out io.Writer) error {
	// A control connection starts the daemon and verifies it is accepting.
	// Closing it before opening the frontend stream is safe because the
	// daemon's idle timeout is not zero.
	client, err := Ensure(ctx, root)
	if err != nil {
		return err
	}
	_ = client.Close()

	conn, reader, _, err := dialKind(ctx, root, kind, session)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	type closeWriter interface{ CloseWrite() error }
	writeDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(conn, in)
		if cw, ok := conn.(closeWriter); ok {
			_ = cw.CloseWrite()
		}
		writeDone <- copyErr
	}()
	_, readErr := io.Copy(out, reader)
	if readErr != nil {
		return readErr
	}
	select {
	case err := <-writeDone:
		return err
	default:
		return nil
	}
}
