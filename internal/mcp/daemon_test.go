package mcp

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// blockingClient is a daemon connection whose calls run until the caller
// cancels them or the connection closes, like a tool call that waits for a
// cold index or a rename that is writing files.
type blockingClient struct {
	done      chan struct{}
	closeOnce sync.Once
	inflight  atomic.Int32
	peak      atomic.Int32
}

func newBlockingClient() *blockingClient { return &blockingClient{done: make(chan struct{})} }

func (c *blockingClient) Call(ctx context.Context, method string, params, result any) error {
	n := c.inflight.Add(1)
	defer c.inflight.Add(-1)
	for {
		p := c.peak.Load()
		if n <= p || c.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("daemon connection closed")
	}
}

func (c *blockingClient) Done() <-chan struct{} { return c.done }

func (c *blockingClient) Close() error {
	c.closeOnce.Do(func() { close(c.done) })
	return nil
}

func backendOver(client *blockingClient) *daemonBackend {
	return newDaemonBackend("/project", func(context.Context, string) (controlClient, error) { return client, nil })
}

// Regression: a rename that the client canceled reported a bare context
// error, although the daemon can have written some files already.
func TestDaemonBackend_CanceledRenameSaysItMayBeApplied(t *testing.T) {
	b := backendOver(newBlockingClient())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := b.CallTool(ctx, renameToolName, nil)
	if err == nil || !strings.Contains(err.Error(), "may have been applied; check git status") {
		t.Fatalf("error = %v, want a note that the rename may have been applied", err)
	}

	// Other tools keep the plain context error.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel2()
	if _, err := b.CallTool(ctx2, "dexter_search", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the deadline", err)
	}
}

// Regression: when the client's roots changed during a call, the frontend
// closed the old workspace connection, and the call said "the MCP server is
// shutting down".
func TestDaemonBackend_RootsChangeDuringCall(t *testing.T) {
	for _, tool := range []string{"dexter_search", renameToolName} {
		b := backendOver(newBlockingClient())
		errc := make(chan error, 1)
		go func() {
			_, err := b.CallTool(context.Background(), tool, nil)
			errc <- err
		}()
		time.Sleep(10 * time.Millisecond)
		if err := b.retire(errRootsChanged); err != nil {
			t.Fatal(err)
		}
		var err error
		select {
		case err = <-errc:
		case <-time.After(5 * time.Second):
			t.Fatal("call did not end")
		}
		if err == nil || !strings.Contains(err.Error(), "roots changed") || strings.Contains(err.Error(), "shutting down") {
			t.Fatalf("%s: error = %v, want a roots-changed error", tool, err)
		}
		if tool == renameToolName && !strings.Contains(err.Error(), "may have been applied") {
			t.Fatalf("rename error = %v, want a note that it may have been applied", err)
		}
	}
}

// One MCP frontend must not take every request slot of the daemon
// connection: calls past the frontend's limit wait for a slot.
func TestDaemonBackend_LimitsConcurrentCalls(t *testing.T) {
	client := newBlockingClient()
	b := backendOver(client)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.CallTool(ctx, "dexter_search", nil)
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for client.inflight.Load() < maxConcurrentToolCalls && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()
	if peak := client.peak.Load(); peak != maxConcurrentToolCalls {
		t.Fatalf("peak concurrent daemon calls = %d, want %d", peak, maxConcurrentToolCalls)
	}
	if maxConcurrentToolCalls >= 64 {
		t.Fatalf("frontend limit %d is not below the daemon's 64", maxConcurrentToolCalls)
	}
}

// The frontend retires the old workspace connection with errRootsChanged, so
// a call still running on it says why it ended.
func TestFrontend_RootsChangeEndsCallWithRootsChangedError(t *testing.T) {
	var mu sync.Mutex
	clients := map[string]*blockingClient{}
	cfg := Config{Root: t.TempDir(), Connect: func(root string) Backend {
		mu.Lock()
		defer mu.Unlock()
		c := newBlockingClient()
		clients[root] = c
		return backendOver(c)
	}}
	e := setupNegotiationWith(t, cfg)
	e.f.cfg.Connect = cfg.Connect // setupNegotiationWith installs fake backends
	rootA, uriA := projectDir(t)
	_, uriB := projectDir(t)
	cs, client := e.connect(nil, uriA)
	t.Cleanup(func() {
		// Calls still waiting on a fake daemon end before the session closes.
		mu.Lock()
		defer mu.Unlock()
		for _, c := range clients {
			_ = c.Close()
		}
	})

	first := make(chan string, 1)
	go func() {
		out, _ := toolText(t, cs, "dexter_search", map[string]any{"query": "x"})
		first <- out
	}()
	eventually(t, "the first call to reach the daemon", func() bool {
		mu.Lock()
		defer mu.Unlock()
		c := clients[rootA]
		return c != nil && c.inflight.Load() == 1
	})

	client.RemoveRoots(uriA)
	client.AddRoots(&mcp.Root{URI: uriB})
	go func() {
		// The next call negotiates again and moves the session to rootB.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "dexter_search", Arguments: map[string]any{"query": "x"}})
	}()
	select {
	case out := <-first:
		wantContains(t, out, "roots changed")
		wantNotContains(t, out, "shutting down")
	case <-time.After(10 * time.Second):
		t.Fatal("the first call did not end")
	}
}

// errorClient is a daemon connection whose calls fail with one error.
type errorClient struct {
	err  error
	done chan struct{}
}

func (c *errorClient) Call(context.Context, string, any, any) error { return c.err }
func (c *errorClient) Done() <-chan struct{}                        { return c.done }
func (c *errorClient) Close() error                                 { return nil }

// Regression: recovery hints said to run a plain dexter stop, which a daemon
// refuses while this MCP session is attached.
func TestRecoveryHintsSayForce(t *testing.T) {
	b := newDaemonBackend("/project", func(context.Context, string) (controlClient, error) {
		return &errorClient{err: errors.New(`unknown daemon method "mcp/tool"`), done: make(chan struct{})}, nil
	})
	_, err := b.CallTool(context.Background(), "dexter_search", nil)
	if err == nil || !strings.Contains(err.Error(), "dexter stop --force") {
		t.Errorf("old-daemon error = %v, want the --force hint", err)
	}

	e := setupProject(t)
	if err := e.store.SetIndexVersion(1); err != nil {
		t.Fatal(err)
	}
	out := e.callTool("dexter_workspace", nil)
	wantContains(t, out, "dexter stop --force")
}

// slowEnsure is a daemon connect that blocks until the test releases it, also
// when its context is canceled, like a daemon start that waits for a lock.
type slowEnsure struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
	client  *blockingClient
}

func newSlowEnsure() *slowEnsure {
	return &slowEnsure{entered: make(chan struct{}, 8), release: make(chan struct{}), client: newBlockingClient()}
}

func (s *slowEnsure) ensure(context.Context, string) (controlClient, error) {
	s.calls.Add(1)
	s.entered <- struct{}{}
	<-s.release
	return s.client, nil
}

// Regression: Close and retire waited for the lock that a slow daemon connect
// held, so a shutdown or a change of roots hung while a daemon started.
func TestDaemonBackend_CloseDoesNotWaitForConnect(t *testing.T) {
	for _, retire := range []bool{false, true} {
		s := newSlowEnsure()
		b := newDaemonBackend("/project", s.ensure)
		errc := make(chan error, 1)
		go func() {
			_, err := b.CallTool(context.Background(), "dexter_search", nil)
			errc <- err
		}()
		<-s.entered

		closed := make(chan struct{})
		go func() {
			if retire {
				_ = b.retire(errRootsChanged)
			} else {
				_ = b.Close()
			}
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			close(s.release)
			t.Fatalf("retire=%v: Close waited for the connect", retire)
		}

		// The connect ends later. Its connection is closed, not kept, and the
		// call gets the close reason.
		close(s.release)
		var err error
		select {
		case err = <-errc:
		case <-time.After(5 * time.Second):
			t.Fatal("call did not end")
		}
		want := errClosed
		if retire {
			want = errRootsChanged
		}
		if !errors.Is(err, want) {
			t.Fatalf("retire=%v: error = %v, want %v", retire, err, want)
		}
		select {
		case <-s.client.Done():
		default:
			t.Fatalf("retire=%v: the connection that the connect made after Close is still open", retire)
		}
	}
}

// Calls that arrive while a connect runs share it instead of starting a
// second connect, and a caller that stops waiting does not end it for the
// others.
func TestDaemonBackend_ConcurrentCallsShareOneConnect(t *testing.T) {
	s := newSlowEnsure()
	b := newDaemonBackend("/project", s.ensure)
	defer func() { _ = b.Close() }()

	impatient, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := b.connection(impatient)
		errc <- err
	}()
	<-s.entered
	got := make(chan controlClient, 4)
	for i := 0; i < 4; i++ {
		go func() {
			c, err := b.connection(context.Background())
			if err != nil {
				t.Error(err)
			}
			got <- c
		}()
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			close(s.release)
			t.Fatalf("canceled caller error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		close(s.release)
		t.Fatal("a canceled caller waited for the connect")
	}
	close(s.release)
	for i := 0; i < 4; i++ {
		if c := <-got; c != s.client {
			t.Fatalf("connection = %v, want the shared client", c)
		}
	}
	if n := s.calls.Load(); n != 1 {
		t.Fatalf("connects = %d, want 1", n)
	}
}
