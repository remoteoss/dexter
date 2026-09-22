package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/workspace"
)

// These tests drive connection handling over net.Pipe rather than a Unix socket,
// so the handshake rules, control dispatch, notifications, session registry, and
// the LSP fast path are all covered without binding an address. Run itself —
// ownership, listening, accept, idle exit — is covered in server_test.go.

// pipeServer builds the state Run would, minus the listener. Native watching is
// off so the index only changes when a test says so.
func pipeServer(t *testing.T, root string) (*server, Endpoint) {
	t.Helper()
	ownership, endpoint, err := AcquireOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := workspace.OpenWithOptions(root, workspace.Options{NoWatch: true})
	if err != nil {
		_ = ownership.Release()
		t.Fatal(err)
	}
	s := &server{
		root:        endpoint.Root,
		identity:    endpoint.Identity,
		runtime:     rt,
		idleTimeout: time.Minute,
		started:     time.Now(),
		activity:    make(chan struct{}, 1),
		stopping:    make(chan struct{}),
		connections: make(map[net.Conn]struct{}),
	}
	s.ctx, s.cancelCtx = context.WithCancel(context.Background())
	waitCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := rt.WaitReady(waitCtx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.cancelCtx()
		_ = rt.Close()
		_ = ownership.Release()
	})
	return s, endpoint
}

// pipeDial performs the handshake over an in-memory pipe and returns the client
// side. The server side is served exactly as Run's accept loop would.
func pipeDial(t *testing.T, s *server, endpoint Endpoint, kind, session string) (net.Conn, *bufio.Reader, helloResponse) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- s.serveConn(serverConn) }()
	t.Cleanup(func() {
		_ = clientConn.Close()
		select {
		case <-served:
		case <-time.After(20 * time.Second):
			t.Error("connection handler did not exit")
		}
	})

	reader := bufio.NewReader(clientConn)
	helloMsg := hello{Contract: ContractVersion, Kind: kind, Root: endpoint.Root, Identity: endpoint.Identity, Session: session}
	if err := writeJSONLine(clientConn, helloMsg); err != nil {
		t.Fatal(err)
	}
	var res helloResponse
	if err := readJSONLine(reader, &res); err != nil {
		t.Fatalf("handshake for kind %q: %v", kind, err)
	}
	return clientConn, reader, res
}

func pipeClient(t *testing.T, s *server, endpoint Endpoint) *Client {
	t.Helper()
	conn, reader, res := pipeDial(t, s, endpoint, kindControl, "")
	if !res.OK {
		t.Fatalf("control handshake refused: %s", res.Error)
	}
	client := newClient(conn, reader)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func lookupCount(t *testing.T, client *Client, module string) int {
	t.Helper()
	var result LookupResult
	if err := client.Call(context.Background(), MethodLookup, LookupParams{Module: module}, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Ready {
		t.Fatal("lookup reported a workspace that is not ready")
	}
	return len(result.Locations)
}

// A peer that does not match this workspace must be refused with a reason, and
// the refusal must still carry the daemon's contract version so a client can
// tell "wrong workspace" from "wrong build".
func TestPipeHandshakeRejectsMismatchedPeers(t *testing.T) {
	quietEnv(t)
	root := t.TempDir()
	s, endpoint := pipeServer(t, root)

	cases := []struct {
		name         string
		hello        hello
		incompatible bool
		exiting      bool
	}{
		{"newer contract", hello{Contract: ContractVersion + 1, Kind: kindControl, Root: endpoint.Root, Identity: endpoint.Identity}, true, true},
		{"older contract", hello{Contract: ContractVersion - 1, Kind: kindControl, Root: endpoint.Root, Identity: endpoint.Identity}, true, false},
		{"other workspace", hello{Contract: ContractVersion, Kind: kindControl, Root: endpoint.Root, Identity: filepath.Join(t.TempDir(), "elsewhere")}, false, false},
		{"other root spelling", hello{Contract: ContractVersion, Kind: kindControl, Root: filepath.Join(t.TempDir(), "alias"), Identity: endpoint.Identity}, false, false},
		{"unknown kind", hello{Contract: ContractVersion, Kind: "not-a-frontend", Root: endpoint.Root, Identity: endpoint.Identity}, false, false},
		{"unknown session", hello{Contract: ContractVersion, Kind: kindControl, Root: endpoint.Root, Identity: endpoint.Identity, Session: "nosuch-999"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serverConn, clientConn := net.Pipe()
			served := make(chan error, 1)
			go func() { served <- s.serveConn(serverConn) }()
			defer func() {
				_ = clientConn.Close()
				<-served
			}()
			reader := bufio.NewReader(clientConn)
			if err := writeJSONLine(clientConn, tc.hello); err != nil {
				t.Fatal(err)
			}
			var res helloResponse
			if err := readJSONLine(reader, &res); err != nil {
				t.Fatal(err)
			}
			if res.OK {
				t.Fatalf("handshake accepted: %+v", res)
			}
			if res.Error == "" {
				t.Fatal("refusal carried no reason")
			}
			if res.Contract != ContractVersion {
				t.Fatalf("refusal did not report contract %d: %+v", ContractVersion, res)
			}
			if res.Incompatible != tc.incompatible {
				t.Fatalf("Incompatible = %v, want %v: %+v", res.Incompatible, tc.incompatible, res)
			}
			if res.Exiting != tc.exiting {
				t.Fatalf("Exiting = %v, want %v: %+v", res.Exiting, tc.exiting, res)
			}
			if tc.exiting {
				// The restart contract: a newer frontend makes this daemon step
				// aside so the frontend can start the current build.
				select {
				case <-s.stopping:
				case <-time.After(2 * time.Second):
					t.Fatal("daemon did not start shutting down for a newer frontend")
				}
			}
		})
	}
}

// The control surface answers from the workspace the daemon owns, and an
// explicit reindex is visible to the next query and to subscribers.
func TestPipeControlServesWorkspaceQueries(t *testing.T) {
	quietEnv(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	s, endpoint := pipeServer(t, root)
	client := pipeClient(t, s, endpoint)
	ctx := context.Background()

	status, err := client.WorkspaceStatus(ctx, 30_000)
	if err != nil {
		t.Fatal(err)
	}
	if status.Root != endpoint.Root {
		t.Fatalf("status root %q, want %q", status.Root, endpoint.Root)
	}
	if !status.Ready {
		t.Fatal("workspace is not ready")
	}
	if status.Files < 1 || status.Definitions < 1 {
		t.Fatalf("index looks empty: %+v", status)
	}
	if status.Watching {
		t.Fatal("runtime reports watching while opened with NoWatch")
	}
	if got := lookupCount(t, client, "SharedLib.One"); got == 0 {
		t.Fatal("indexed module does not resolve")
	}

	daemonStatus, err := client.DaemonStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !daemonStatus.Ready || daemonStatus.Root != endpoint.Root || daemonStatus.Contract != ContractVersion {
		t.Fatalf("unexpected daemon status: %+v", daemonStatus)
	}

	// With watching off, a new file is invisible until the workspace is told
	// about it. That is what makes the notification below deterministic.
	path := writeModule(t, root, "lib/two.ex", "SharedLib.Two")
	if got := lookupCount(t, client, "SharedLib.Two"); got != 0 {
		t.Fatalf("an unreconciled file is already indexed (%d results)", got)
	}

	changes := make(chan Changed, 16)
	unwatch, err := client.Watch(ctx, 8, func(change Changed) {
		select {
		case changes <- change:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	var reindexed ReindexResult
	if err := client.Call(ctx, MethodReindex, ReindexParams{Target: path}, &reindexed); err != nil {
		t.Fatal(err)
	}
	if got := lookupCount(t, client, "SharedLib.Two"); got == 0 {
		t.Fatal("reindexed file does not resolve")
	}
	select {
	case change := <-changes:
		if change.Subscription == "" {
			t.Fatalf("change without a subscription id: %+v", change)
		}
		if !change.Full && len(change.Paths) == 0 {
			t.Fatalf("change names nothing: %+v", change)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notification for an explicit reindex")
	}

	// A module lookup that misses falls back to the module itself unless the
	// caller asked for strict matching, matching the CLI's long-standing
	// behavior.
	var strict LookupResult
	if err := client.Call(ctx, MethodLookup, LookupParams{Module: "SharedLib.Two", Function: "nonexistent_fn", Strict: true}, &strict); err != nil {
		t.Fatal(err)
	}
	if len(strict.Locations) != 0 {
		t.Fatalf("strict lookup fell back to the module: %+v", strict.Locations)
	}
	var loose LookupResult
	if err := client.Call(ctx, MethodLookup, LookupParams{Module: "SharedLib.Two", Function: "nonexistent_fn"}, &loose); err != nil {
		t.Fatal(err)
	}
	if len(loose.Locations) == 0 {
		t.Fatal("non-strict lookup did not fall back to the module")
	}

	unwatch()
	if err := client.Call(ctx, MethodUnwatch, Subscription{ID: "w999"}, nil); err != nil {
		t.Fatalf("unwatching an unknown subscription should be harmless: %v", err)
	}
}

// One connection carries many in-flight calls, so a response must reach the
// caller that asked for it. A cross-wired id would answer one module's lookup
// with another's locations.
func TestPipeConcurrentCallsReachTheirOwnCallers(t *testing.T) {
	quietEnv(t)
	root := t.TempDir()
	const modules = 5
	for i := 0; i < modules; i++ {
		writeModule(t, root, fmt.Sprintf("lib/mod%d.ex", i), fmt.Sprintf("SharedLib.Mod%d", i))
	}
	s, endpoint := pipeServer(t, root)
	client := pipeClient(t, s, endpoint)
	ctx := context.Background()
	if _, err := client.WorkspaceStatus(ctx, 30_000); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	failures := make(chan error, modules*8)
	for i := 0; i < modules; i++ {
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				module := fmt.Sprintf("SharedLib.Mod%d", i)
				want := fmt.Sprintf("mod%d.ex", i)
				for n := 0; n < 3; n++ {
					var result LookupResult
					if err := client.Call(ctx, MethodLookup, LookupParams{Module: module}, &result); err != nil {
						failures <- fmt.Errorf("%s: %w", module, err)
						return
					}
					if len(result.Locations) == 0 {
						failures <- fmt.Errorf("%s: no locations", module)
						return
					}
					for _, loc := range result.Locations {
						if !strings.HasSuffix(loc.FilePath, want) {
							failures <- fmt.Errorf("%s resolved to %s", module, loc.FilePath)
							return
						}
					}
				}
			}(i)
		}
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

// An LSP connection is a raw stream after the handshake: the daemon serves a
// real session over it, names it so another frontend can opt into its overlay,
// and survives that editor's exit.
func TestPipeLSPConnectionServesARealSession(t *testing.T) {
	quietEnv(t)
	root := t.TempDir()
	writeModule(t, root, "lib/one.ex", "SharedLib.One")
	s, endpoint := pipeServer(t, root)

	conn, reader, res := pipeDial(t, s, endpoint, kindLSP, "")
	if !res.OK {
		t.Fatalf("LSP handshake refused: %s", res.Error)
	}
	if res.Session == "" {
		t.Fatal("LSP connection was not given a session id")
	}

	writeLSP(t, conn, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"processId":    nil,
			"rootUri":      "file://" + root,
			"capabilities": map[string]any{},
		},
	})
	initialize := readLSPUntilID(t, conn, reader, 1)
	result, ok := initialize["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize returned no result: %v", initialize)
	}
	if _, ok := result["capabilities"].(map[string]any); !ok {
		t.Fatalf("initialize returned no capabilities: %v", result)
	}

	control := pipeClient(t, s, endpoint)
	status, err := control.WorkspaceStatus(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, session := range status.Sessions {
		if session.ID == res.Session {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("session %q is not in %+v", res.Session, status.Sessions)
	}

	// shutdown/exit ends the session, not the daemon: an editor disconnecting
	// must not take down every other frontend on the workspace.
	writeLSP(t, conn, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "shutdown"})
	readLSPUntilID(t, conn, reader, 2)
	writeLSP(t, conn, map[string]any{"jsonrpc": "2.0", "method": "exit"})

	deadline := time.Now().Add(20 * time.Second)
	for {
		status, err := control.WorkspaceStatus(context.Background(), 0)
		if err != nil {
			t.Fatalf("daemon stopped serving after the LSP session exited: %v", err)
		}
		if len(status.Sessions) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %q was never released: %+v", res.Session, status.Sessions)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeLSP(t *testing.T, w io.Writer, message any) {
	t.Helper()
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
}

// readLSPUntilID reads LSP messages until the response with wantID arrives.
// Server-initiated requests are answered with a null result so a chatty server
// cannot wedge the exchange, and notifications are dropped.
func readLSPUntilID(t *testing.T, conn net.Conn, r *bufio.Reader, wantID int) map[string]any {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		message := readLSP(t, r)
		_, hasMethod := message["method"].(string)
		rawID, hasID := message["id"]
		if hasMethod && hasID {
			writeLSP(t, conn, map[string]any{"jsonrpc": "2.0", "id": rawID, "result": nil})
			continue
		}
		if hasMethod {
			continue
		}
		id, ok := idAsInt(rawID)
		if ok && id == wantID {
			return message
		}
	}
	t.Fatalf("no LSP response with id %d", wantID)
	return nil
}

func idAsInt(raw any) (int, bool) {
	switch v := raw.(type) {
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	}
	return 0, false
}

func readLSP(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading LSP headers: %v", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			break
		}
		if len(trimmed) > len("content-length:") && strings.EqualFold(trimmed[:len("content-length:")], "content-length:") {
			n, err := strconv.Atoi(strings.TrimSpace(trimmed[len("content-length:"):]))
			if err != nil {
				t.Fatalf("bad Content-Length %q: %v", trimmed, err)
			}
			length = n
		}
	}
	if length < 0 {
		t.Fatal("LSP message without Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatalf("reading LSP body: %v", err)
	}
	var message map[string]any
	if err := json.Unmarshal(body, &message); err != nil {
		t.Fatalf("decoding LSP body %q: %v", body, err)
	}
	return message
}
