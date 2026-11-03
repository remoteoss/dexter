package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/store"
)

// TestRequestPanicIsAnsweredAndTheSessionGoesOn: one daemon serves every
// editor of a workspace, and jsonrpc2 runs handlers on its own goroutine, so a
// panic in a handler stopped the process. Now the request gets an
// InternalError, the panic is a condition that editors see once, and the same
// session keeps answering.
func TestRequestPanicIsAnsweredAndTheSessionGoesOn(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	server.index.reporter.SetLogf(func(string, ...any) {})
	restore := SetRequestHookForTesting(func(method string) {
		if method == protocol.MethodTextDocumentHover {
			var nilServer *Server
			_ = nilServer.projectRoot // a nil dereference, as the audit injected
		}
	})
	defer restore()

	serverSide, editorSide := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- ServeStream(server, serverSide) }()
	editor := jsonrpc2.NewConn(jsonrpc2.NewStream(editorSide))
	editor.Go(context.Background(), func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		return reply(ctx, nil, nil)
	})
	defer func() {
		_ = editor.Close()
		<-served
	}()

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var result json.RawMessage
		_, err := editor.Call(ctx, protocol.MethodTextDocumentHover, map[string]any{
			"textDocument": map[string]any{"uri": "file:///nowhere.ex"},
			"position":     map[string]any{"line": 0, "character": 0},
		}, &result)
		cancel()
		var rpcErr *jsonrpc2.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != jsonrpc2.InternalError {
			t.Fatalf("hover %d = %v, want an InternalError answer", i, err)
		}
	}
	conditions := server.index.reporter.Conditions()
	if len(conditions) != 1 || conditions[0].Key != notify.InternalPrefix+"request" || conditions[0].Severity != notify.Error {
		t.Fatalf("conditions = %+v, want one internal.request error", conditions)
	}
	if !strings.Contains(conditions[0].Message, protocol.MethodTextDocumentHover) {
		t.Errorf("condition message does not name the request: %q", conditions[0].Message)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var shutdown json.RawMessage
	if _, err := editor.Call(ctx, protocol.MethodShutdown, nil, &shutdown); err != nil {
		t.Fatalf("the session stopped answering after the panic: %v", err)
	}
}

// TestIndexPanicIsReportedAndRestartsTheIndex: a panic in shared index work is
// recovered, every editor is told, and the workspace hears of it through
// OnFault, so that it can restart its index.
func TestIndexPanicIsReportedAndRestartsTheIndex(t *testing.T) {
	index := NewIndexCoordinator()
	index.reporter.SetLogf(func(string, ...any) {})
	faults := make(chan string, 1)
	index.OnFault(func(what string) { faults <- what })
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer index.reporter.Recover("index", "the test pass", index.faulted("the test pass"))
		panic("index bug")
	}()
	<-done
	if what := <-faults; what != "the test pass" {
		t.Fatalf("fault = %q", what)
	}
	if !index.reporter.Active(notify.InternalPrefix + "index") {
		t.Fatal("the panic is not a condition")
	}
}

// TestClosedSessionsDoNotLeakTheirRequestsToTheEditor: a session that closes
// before it answers client/registerCapability or
// window/workDoneProgress/create must not keep a goroutine, and with it the
// session, waiting for an answer that cannot come.
func TestClosedSessionsDoNotLeakTheirRequestsToTheEditor(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	index := NewIndexCoordinator()
	index.reporter.SetLogf(func(string, ...any) {})
	task := index.reporter.Begin("test", "Indexing", "", false)
	defer task.End("")

	settle := func() int {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	before := settle()
	const sessions = 20
	for i := 0; i < sessions; i++ {
		server := NewServerWithOptions(st, dir, ServerOptions{Index: index, ManageWorkspace: false})
		server.workDoneProgress = true
		serverSide, editorSide := net.Pipe()
		served := make(chan error, 1)
		go func() { served <- ServeStream(server, serverSide) }()
		writeFrame(t, editorSide, map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
		// Read until both requests have arrived, and never answer them.
		reader := bufio.NewReader(editorSide)
		seen := map[string]bool{}
		for !seen[protocol.MethodClientRegisterCapability] || !seen[protocol.MethodWorkDoneProgressCreate] {
			message := readFrame(t, reader)
			if method, _ := message["method"].(string); method != "" {
				seen[method] = true
			}
		}
		_ = editorSide.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Fatal("session did not end")
		}
	}
	// Well under clientCallTimeout and deliveryTimeout: the goroutines must
	// end because the session closed, not because a timer ran out.
	deadline := time.Now().Add(3 * time.Second)
	for {
		after := settle()
		if after <= before+3 {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("%d goroutines before %d closed sessions, %d after\n%s", before, sessions, after, buf[:n])
		}
	}
}

func writeFrame(t *testing.T, w io.Writer, message any) {
	t.Helper()
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatal(err)
	}
}

func readFrame(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading headers: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "Content-Length") {
			length, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(body, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

// TestCancelRequestStopsAReferencesSearch: an editor cancels a references
// request that it no longer needs. The search must stop early, not use its
// slot and CPU to the end, and the answer must be RequestCancelled.
func TestCancelRequestStopsAReferencesSearch(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	path := writeTestFile(t, server.projectRoot, "lib/caller.ex", "defmodule MyApp.Caller do\n  def run, do: MyApp.Worker.perform(1)\nend\n")
	started := make(chan struct{}, 1)
	stopped := make(chan time.Duration, 1)
	testHookReferences = func(ctx context.Context) {
		start := time.Now()
		started <- struct{}{}
		select {
		case <-ctx.Done():
			stopped <- time.Since(start)
		case <-time.After(10 * time.Second):
			stopped <- -1
		}
	}
	t.Cleanup(func() { testHookReferences = nil })

	serverSide, editorSide := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- ServeStream(server, serverSide) }()
	defer func() {
		_ = editorSide.Close()
		<-served
	}()
	reader := bufio.NewReader(editorSide)
	uri := "file://" + path
	go func() {
		writeFrame(t, editorSide, map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{
			"textDocument": map[string]any{"uri": uri, "languageId": "elixir", "version": 1, "text": "defmodule MyApp.Caller do\n  def run, do: MyApp.Worker.perform(1)\nend\n"}}})
		writeFrame(t, editorSide, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "textDocument/references", "params": map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     map[string]any{"line": 1, "character": 30},
			"context":      map[string]any{"includeDeclaration": false},
		}})
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			return
		}
		writeFrame(t, editorSide, map[string]any{"jsonrpc": "2.0", "method": "$/cancelRequest", "params": map[string]any{"id": 7}})
	}()
	for {
		message := readFrame(t, reader)
		if id, ok := message["id"].(float64); !ok || id != 7 || message["method"] != nil {
			continue
		}
		errObj, _ := message["error"].(map[string]any)
		if code, _ := errObj["code"].(float64); code != -32800 {
			t.Fatalf("canceled references = %v, want RequestCancelled", message)
		}
		break
	}
	if took := <-stopped; took < 0 || took > 2*time.Second {
		t.Fatalf("the search did not stop when canceled (%s)", took)
	}
}

// TestClosedSessionRunsNoQueuedMessageAndStartsNoBEAM: a slow request runs on
// the dispatcher's worker, and a didOpen waits behind it. The editor goes
// away. The queued didOpen must not run on the released session: it would
// start a BEAM that lives as an orphan child of the daemon. The slow request
// sees a canceled context, and the session starts no BEAM after it closed.
func TestClosedSessionRunsNoQueuedMessageAndStartsNoBEAM(t *testing.T) {
	previousWait := sessionDrainWait
	sessionDrainWait = 200 * time.Millisecond
	t.Cleanup(func() { sessionDrainWait = previousWait })
	server, cleanup := setupTestServer(t)
	defer cleanup()
	logs := captureLog(t)
	caller := writeTestFile(t, server.projectRoot, "lib/caller.ex", "defmodule MyApp.Caller do\n  def run, do: MyApp.Worker.perform(1)\nend\n")
	opened := writeTestFile(t, server.projectRoot, "lib/opened.ex", "defmodule MyApp.Opened do\nend\n")

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	ctxAtEnd := make(chan error, 1)
	testHookReferences = func(ctx context.Context) {
		started <- struct{}{}
		<-release // a handler that does not check its context
		ctxAtEnd <- ctx.Err()
	}
	t.Cleanup(func() { testHookReferences = nil })

	var didOpens atomic.Int32
	restore := SetRequestHookForTesting(func(method string) {
		if method == protocol.MethodTextDocumentDidOpen {
			didOpens.Add(1)
		}
	})
	t.Cleanup(restore)
	server.mixBin = "/nonexistent/dexter-test/mix" // a start is tried, and logged
	serverSide, editorSide := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- ServeStream(server, serverSide) }()
	go func() { _, _ = io.Copy(io.Discard, editorSide) }()
	writeFrame(t, editorSide, map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{
		"textDocument": map[string]any{"uri": "file://" + caller, "languageId": "elixir", "version": 1, "text": "defmodule MyApp.Caller do\n  def run, do: MyApp.Worker.perform(1)\nend\n"}}})
	writeFrame(t, editorSide, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "textDocument/references", "params": map[string]any{
		"textDocument": map[string]any{"uri": "file://" + caller},
		"position":     map[string]any{"line": 1, "character": 30},
		"context":      map[string]any{"includeDeclaration": false},
	}})
	<-started
	writeFrame(t, editorSide, map[string]any{"jsonrpc": "2.0", "method": "textDocument/didOpen", "params": map[string]any{
		"textDocument": map[string]any{"uri": "file://" + opened, "languageId": "elixir", "version": 1, "text": "defmodule MyApp.Opened do\nend\n"}}})
	_ = editorSide.Close()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeStream did not return")
	}
	server.CloseSession()              // what the daemon's release does
	time.Sleep(100 * time.Millisecond) // the first didOpen's warm-up ends
	before := len(logs.String())
	close(release)
	if err := <-ctxAtEnd; err == nil {
		t.Error("the slow request's context was not canceled when the connection closed")
	}
	time.Sleep(200 * time.Millisecond) // a queued didOpen would run now
	if n := didOpens.Load(); n != 1 {
		t.Fatalf("%d didOpen messages ran, want 1: a queued didOpen ran on the closed session", n)
	}
	if bp := server.getBeamProcess(context.Background(), server.projectRoot); bp != nil {
		t.Fatal("a closed session started a BEAM")
	}
	if after := logs.String()[before:]; strings.Contains(after, "BEAM: failed to start") {
		t.Fatalf("a closed session tried to start a BEAM:\n%s", after)
	}
}

// TestCanceledErrorIsRequestCancelled: a handler that returns the error of
// its canceled context must answer RequestCancelled (-32800), not an internal
// error.
func TestCanceledErrorIsRequestCancelled(t *testing.T) {
	handler := cancelHandler(func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		return reply(ctx, nil, fmt.Errorf("symbols: %w", context.Canceled))
	})
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(3), protocol.MethodWorkspaceSymbol, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	_ = handler(context.Background(), func(_ context.Context, _ any, err error) error {
		got = err
		return nil
	}, call)
	var rpcErr *jsonrpc2.Error
	if !errors.As(got, &rpcErr) || rpcErr.Code != -32800 {
		t.Fatalf("answer = %v, want RequestCancelled", got)
	}
}

// TestDispatcherQueueIsBounded: when the queue behind a slow request is full,
// the reader waits, as it did when every message ran on it, instead of
// queuing without end.
func TestDispatcherQueueIsBounded(t *testing.T) {
	previous := maxQueuedMessages
	maxQueuedMessages = 2
	t.Cleanup(func() { maxQueuedMessages = previous })
	release := make(chan struct{})
	d := newSerialDispatcher(func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		if req.Method() == protocol.MethodTextDocumentReferences {
			<-release
		}
		return nil
	})
	defer d.close(time.Second)
	slow, _ := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), protocol.MethodTextDocumentReferences, nil)
	_ = d.handle(context.Background(), nil, slow)
	time.Sleep(20 * time.Millisecond) // the worker takes the slow request
	note, _ := jsonrpc2.NewNotification(protocol.MethodTextDocumentDidChange, nil)
	_ = d.handle(context.Background(), nil, note)
	_ = d.handle(context.Background(), nil, note)
	blocked := make(chan struct{})
	go func() {
		_ = d.handle(context.Background(), nil, note)
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("the reader queued a third message behind a full queue")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the reader did not go on when the queue had room")
	}
}
