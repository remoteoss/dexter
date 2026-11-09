package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// proxyHarness runs an lspProxy between a test editor and daemons that the test
// plays over in-memory pipes. Each dial takes the next daemon from dials.
type proxyHarness struct {
	t      *testing.T
	editor *lspEditor
	dials  chan func() (net.Conn, *bufio.Reader, error)
	done   chan error
	in     io.WriteCloser
}

func newProxyHarness(t *testing.T, root string) (*proxyHarness, *lspEditor) {
	t.Helper()
	editorToProxy, proxyIn := io.Pipe()
	proxyOut, proxyToEditor := io.Pipe()
	h := &proxyHarness{
		t:     t,
		dials: make(chan func() (net.Conn, *bufio.Reader, error), 4),
		done:  make(chan error, 1),
		in:    proxyIn,
	}
	first, daemon := net.Pipe()
	proxy := newLSPProxy(context.Background(), root, editorToProxy, proxyToEditor, func(ctx context.Context) (net.Conn, *bufio.Reader, error) {
		select {
		case dial := <-h.dials:
			// A real dial ends when ctx ends; so does this one.
			type result struct {
				conn   net.Conn
				reader *bufio.Reader
				err    error
			}
			done := make(chan result, 1)
			go func() {
				conn, reader, err := dial()
				done <- result{conn, reader, err}
			}()
			select {
			case r := <-done:
				return r.conn, r.reader, r.err
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(5 * time.Second):
			return nil, nil, errors.New("no daemon")
		}
	})
	go func() {
		err := proxy.run(first, bufio.NewReader(first))
		h.done <- err
		_ = proxyToEditor.Close()
		// A test that writes after the proxy ended fails instead of hanging.
		_ = editorToProxy.CloseWithError(fmt.Errorf("the proxy ended: %v", err))
	}()
	t.Cleanup(func() { _ = proxyIn.Close() })
	h.editor = newLSPEditor(t, proxyIn, proxyOut).pump()
	return h, newLSPEditor(t, daemon, daemon).pump()
}

// nextDaemon makes the next dial succeed and returns the daemon side.
func (h *proxyHarness) nextDaemon() *lspEditor { return h.nextDaemonRaw().pump() }

// nextDaemonRaw is nextDaemon for a daemon side that reads only when the test
// does, so that the proxy's writes wait for it.
func (h *proxyHarness) nextDaemonRaw() *lspEditor {
	proxySide, daemonSide := net.Pipe()
	h.dials <- func() (net.Conn, *bufio.Reader, error) { return proxySide, bufio.NewReader(proxySide), nil }
	return newLSPEditor(h.t, daemonSide, daemonSide)
}

// expect reads the next message the daemon side gets and checks its method.
func expectMethod(t *testing.T, side *lspEditor, method string) map[string]any {
	t.Helper()
	message, err := side.next()
	if err != nil {
		t.Fatalf("waiting for %s: %v", method, err)
	}
	if message["method"] != method {
		t.Fatalf("got %v, want %s", message, method)
	}
	return message
}

func textOf(t *testing.T, message map[string]any) string {
	t.Helper()
	params, _ := message["params"].(map[string]any)
	if doc, ok := params["textDocument"].(map[string]any); ok {
		if text, ok := doc["text"].(string); ok {
			return text
		}
	}
	if changes, ok := params["contentChanges"].([]any); ok && len(changes) > 0 {
		if change, ok := changes[len(changes)-1].(map[string]any); ok {
			return fmt.Sprint(change["text"])
		}
	}
	t.Fatalf("no text in %v", message)
	return ""
}

// TestProxyReplaysTheSessionAndAnswersOpenRequests is the core of the proxy
// reconnect: a request that a stopped daemon did not answer gets an error at
// once, and the new daemon gets the session as the editor left it, unsaved
// text included, before any later message.
func TestProxyReplaysTheSessionAndAnswersOpenRequests(t *testing.T) {
	root := t.TempDir()
	h, daemon := newProxyHarness(t, root)
	editor := h.editor

	editor.request(1, "initialize", map[string]any{"rootUri": fileURI(root), "capabilities": map[string]any{}})
	init := expectMethod(t, daemon, "initialize")
	daemon.send(map[string]any{"id": init["id"], "result": map[string]any{"capabilities": map[string]any{}}})
	editor.response(1)
	editor.notify("initialized", map[string]any{})
	expectMethod(t, daemon, "initialized")
	editor.notify("workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{"dexter": map[string]any{"x": 1}}})
	expectMethod(t, daemon, "workspace/didChangeConfiguration")
	uri := fileURI(filepath.Join(root, "lib/a.ex"))
	closed := fileURI(filepath.Join(root, "lib/closed.ex"))
	editor.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "elixir", "version": 1, "text": "saved"}})
	expectMethod(t, daemon, "textDocument/didOpen")
	editor.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": closed, "languageId": "elixir", "version": 1, "text": "x"}})
	expectMethod(t, daemon, "textDocument/didOpen")
	editor.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": 2},
		"contentChanges": []any{map[string]any{"text": "unsaved \"quoted\" text"}},
	})
	expectMethod(t, daemon, "textDocument/didChange")
	editor.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": closed}})
	expectMethod(t, daemon, "textDocument/didClose")
	// A request the daemon will never answer, and a server request that the
	// editor answers only after the daemon is gone.
	editor.request(7, "textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": uri}})
	expectMethod(t, daemon, "textDocument/hover")
	daemon.send(map[string]any{"id": 1, "method": "window/workDoneProgress/create", "params": map[string]any{"token": "t"}})

	next := h.nextDaemon()
	// The daemon stops. The proxy answers the open request and the editor's
	// late answer to the old server request must not reach the new daemon.
	if err := daemon.w.(net.Conn).Close(); err != nil {
		t.Fatal(err)
	}
	for {
		message, err := editor.next()
		if err != nil {
			t.Fatal(err)
		}
		if message["method"] == "window/workDoneProgress/create" {
			continue // answered below, after the daemon is gone
		}
		if id, _ := idAsInt(message["id"]); id == 7 {
			errObj, _ := message["error"].(map[string]any)
			if code, _ := idAsInt(errObj["code"]); code != lspContentModified {
				t.Fatalf("open request got %v, want a ContentModified error", message)
			}
			break
		}
	}
	editor.send(map[string]any{"id": 1, "result": nil})

	replayed := expectMethod(t, next, "initialize")
	if id, isInt := idAsInt(replayed["id"]); isInt && id == 1 {
		t.Fatalf("the replayed initialize reuses the editor's id: %v", replayed)
	}
	// Server requests during startup reach the editor.
	next.send(map[string]any{"method": "window/logMessage", "params": map[string]any{"type": 4, "message": "hello"}})
	next.send(map[string]any{"id": replayed["id"], "result": map[string]any{"capabilities": map[string]any{}}})
	expectMethod(t, next, "initialized")
	expectMethod(t, next, "workspace/didChangeConfiguration")
	// One didOpen with the unsaved text: the proxy keeps one copy.
	open := expectMethod(t, next, "textDocument/didOpen")
	if got := textOf(t, open); got != `unsaved "quoted" text` {
		t.Fatalf("replayed didOpen text = %q", got)
	}
	if doc := open["params"].(map[string]any)["textDocument"].(map[string]any); doc["languageId"] != "elixir" || fmt.Sprint(doc["version"]) != "2" {
		t.Fatalf("replayed didOpen = %v", doc)
	}
	// The buffer has unsaved changes, so a didChange with the same text
	// follows: the new daemon must know it is unsaved.
	if got := textOf(t, expectMethod(t, next, "textDocument/didChange")); got != `unsaved "quoted" text` {
		t.Fatalf("replayed didChange text = %q", got)
	}
	// The closed document is not replayed, the editor's late answer was
	// dropped, and the next request goes to the new daemon.
	editor.request(8, "textDocument/definition", map[string]any{"textDocument": map[string]any{"uri": uri}})
	if got := expectMethod(t, next, "textDocument/definition"); fmt.Sprint(got["id"]) != "8" {
		t.Fatalf("request id = %v", got["id"])
	}
	next.send(map[string]any{"id": 8, "result": nil})
	if res := editor.response(8); res["error"] != nil {
		t.Fatalf("request after the reconnect failed: %v", res)
	}
	// The editor got the log line, but not the answer to the replayed
	// initialize.
	editor.mu.Lock()
	gotLog := false
	for _, n := range editor.notifications {
		if n["method"] == "window/logMessage" {
			gotLog = true
		}
	}
	editor.mu.Unlock()
	if !gotLog {
		t.Error("a notification during the replay did not reach the editor")
	}

	// The editor ends the session: shutdown, exit, and the end of its input.
	editor.request(9, "shutdown", nil)
	expectMethod(t, next, "shutdown")
	next.send(map[string]any{"id": 9, "result": nil})
	editor.response(9)
	editor.notify("exit", nil)
	expectMethod(t, next, "exit")
	_ = next.w.(net.Conn).Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("proxy exit = %v, want nil after exit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not end after exit")
	}
}

// TestProxyReplaysAnInitializeThatWasNotAnswered: when the daemon stops before
// it answers initialize, the editor still needs that answer, so the replay
// sends the editor's own request and passes the answer on.
func TestProxyReplaysAnInitializeThatWasNotAnswered(t *testing.T) {
	root := t.TempDir()
	h, daemon := newProxyHarness(t, root)
	h.editor.request(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	expectMethod(t, daemon, "initialize")
	next := h.nextDaemon()
	_ = daemon.w.(net.Conn).Close()
	replayed := expectMethod(t, next, "initialize")
	if id, _ := idAsInt(replayed["id"]); id != 1 {
		t.Fatalf("replayed initialize id = %v, want the editor's 1", replayed["id"])
	}
	next.send(map[string]any{"id": 1, "result": map[string]any{"capabilities": map[string]any{"hoverProvider": true}}})
	res := h.editor.response(1)
	if res["error"] != nil || res["result"] == nil {
		t.Fatalf("initialize answer = %v", res)
	}
}

// TestProxyAnswersAnOpenInitializeWhenItCannotReconnect: reconnect keeps an
// unanswered initialize for the replay. When no daemon can be reached, the
// editor must still get an answer to it, as at a failed start.
func TestProxyAnswersAnOpenInitializeWhenItCannotReconnect(t *testing.T) {
	root := t.TempDir()
	h, daemon := newProxyHarness(t, root)
	h.editor.request(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	expectMethod(t, daemon, "initialize")
	h.dials <- func() (net.Conn, *bufio.Reader, error) {
		return nil, nil, &IncompatibleDaemonError{Reason: "daemon contract 99", DaemonContract: ContractVersion + 1}
	}
	_ = daemon.w.(net.Conn).Close()
	assertConnectionLost(t, h, "older Dexter build")
	if res := h.editor.response(1); res["error"] == nil {
		t.Fatalf("initialize answer = %v, want an error", res)
	}
}

// TestProxyDropsALateAnswerToAStoppedDaemonsRequest: a stopped daemon's
// request has id 7 and no answer; the new daemon sends its own request with
// id 7. The editor's late answer to the old request must be dropped, and its
// answer to the new one must reach the new daemon with the daemon's id.
func TestProxyDropsALateAnswerToAStoppedDaemonsRequest(t *testing.T) {
	h, daemon := initializedHarness(t)
	daemon.send(map[string]any{"id": 7, "method": "window/workDoneProgress/create", "params": map[string]any{"token": "old"}})
	old := expectMethod(t, h.editor, "window/workDoneProgress/create")
	next := h.nextDaemon()
	_ = daemon.w.(net.Conn).Close()
	replayed := expectMethod(t, next, "initialize")
	next.send(map[string]any{"id": replayed["id"], "result": map[string]any{}})
	next.send(map[string]any{"id": 7, "method": "window/workDoneProgress/create", "params": map[string]any{"token": "new"}})
	fresh := expectMethod(t, h.editor, "window/workDoneProgress/create")
	if fmt.Sprint(fresh["id"]) == fmt.Sprint(old["id"]) {
		t.Fatalf("the editor got id %v for both daemons' requests", fresh["id"])
	}
	// The late answer to the old request, then the answer to the new one
	// (an error, to tell them apart).
	h.editor.send(map[string]any{"id": old["id"], "result": nil})
	h.editor.send(map[string]any{"id": fresh["id"], "error": map[string]any{"code": 1, "message": "new one"}})
	h.editor.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": "file:///x"}})
	var got []map[string]any
	for {
		m, err := next.next()
		if err != nil {
			t.Fatal(err)
		}
		if m["method"] == "textDocument/didSave" {
			break
		}
		got = append(got, m)
	}
	if len(got) != 1 || got[0]["error"] == nil {
		t.Fatalf("the new daemon got %v, want only the answer to its own request", got)
	}
	if id, _ := idAsInt(got[0]["id"]); id != 7 {
		t.Fatalf("the answer has id %v, want the daemon's 7", got[0]["id"])
	}
}

// TestProxyTellsTheEditorWhenANewerDaemonTookOver: an older proxy cannot
// attach to the daemon of a newer build. The editor must be told, and the
// proxy must exit with an error, not 0.
func TestProxyTellsTheEditorWhenANewerDaemonTookOver(t *testing.T) {
	root := t.TempDir()
	h, daemon := newProxyHarness(t, root)
	h.editor.request(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	init := expectMethod(t, daemon, "initialize")
	daemon.send(map[string]any{"id": init["id"], "result": map[string]any{}})
	h.editor.response(1)
	h.editor.request(2, "textDocument/hover", map[string]any{})
	expectMethod(t, daemon, "textDocument/hover")

	h.dials <- func() (net.Conn, *bufio.Reader, error) {
		return nil, nil, &IncompatibleDaemonError{Reason: "daemon contract 99", DaemonContract: ContractVersion + 1}
	}
	_ = daemon.w.(net.Conn).Close()
	assertConnectionLost(t, h, "older Dexter build")
}

// TestProxyStopsWhenTheProjectIsGone: a project on an ejected volume, or one
// that was removed, cannot be served again, and starting a daemon for it would
// create its directory anew. The proxy says so instead.
func TestProxyStopsWhenTheProjectIsGone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	h, daemon := newProxyHarness(t, root)
	h.editor.request(1, "initialize", map[string]any{})
	init := expectMethod(t, daemon, "initialize")
	daemon.send(map[string]any{"id": init["id"], "result": map[string]any{}})
	h.editor.response(1)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	_ = daemon.w.(net.Conn).Close()
	assertConnectionLost(t, h, "no longer available")
}

func assertConnectionLost(t *testing.T, h *proxyHarness, want string) {
	t.Helper()
	var shown []string
	for len(shown) == 0 {
		message, err := h.editor.next()
		if err != nil {
			t.Fatalf("the editor was not told: %v", err)
		}
		h.editor.handle(message)
		shown = h.editor.shown()
	}
	if !strings.Contains(shown[0], want) {
		t.Fatalf("showMessage = %q, want it to contain %q", shown[0], want)
	}
	select {
	case err := <-h.done:
		var lost *LSPConnectionLostError
		if !errors.As(err, &lost) {
			t.Fatalf("proxy exit = %v, want an *LSPConnectionLostError", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("proxy did not exit")
	}
}

// TestProxyEndsWhenTheEditorCloses: the end of the editor's input ends the
// session, and the proxy does not try to reach a daemon again.
func TestProxyEndsWhenTheEditorCloses(t *testing.T) {
	root := t.TempDir()
	h, daemon := newProxyHarness(t, root)
	_ = h.in.Close()
	// The daemon sees the end of its input and closes the session.
	if _, err := daemon.next(); err == nil {
		t.Fatal("the daemon got a message after the editor closed")
	}
	_ = daemon.w.(net.Conn).Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("proxy exit = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("proxy did not end")
	}
}

// runDaemonInProcess runs Run for root and returns a func that stops it and
// waits until it has returned.
func runDaemonInProcess(t *testing.T, root string) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, root, time.Minute) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-runErr:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrWorkspaceOwned) {
				t.Errorf("daemon exit: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("daemon did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

// TestProxyCarriesTheSessionToANewDaemon stops the daemon under an attached
// proxy. The proxy starts a new daemon and the editor's next request is
// answered from the unsaved text of its open document.
func TestProxyCarriesTheSessionToANewDaemon(t *testing.T) {
	quietEnv(t)
	requireSocketSupport(t)
	root, caller, text := proxyFixture(t)
	stopFirst := runDaemonInProcess(t, root)
	client := waitForClient(t, root, make(chan error))
	if _, err := client.WorkspaceStatus(context.Background(), 30_000); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()

	spawned := make(chan struct{}, 4)
	origSpawn := spawnDaemon
	spawnDaemon = func(root string) (<-chan error, error) {
		runDaemonInProcess(t, root)
		spawned <- struct{}{}
		return nil, nil
	}
	t.Cleanup(func() { spawnDaemon = origSpawn })

	editorToProxy, proxyIn := io.Pipe()
	proxyOut, proxyToEditor := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := ProxyLSP(context.Background(), root, editorToProxy, proxyToEditor)
		done <- err
		_ = proxyToEditor.Close()
		// A test that writes after the proxy ended fails instead of hanging.
		_ = editorToProxy.CloseWithError(fmt.Errorf("the proxy ended: %v", err))
	}()
	editor := newLSPEditor(t, proxyIn, proxyOut).pump()
	initializeEditor(t, editor, root, caller, text)
	// A call that exists only in the editor's buffer.
	unsaved := strings.Replace(text, "    MyApp.Accounts.get_user(1)\n", "    MyApp.Accounts.get_user(1)\n    MyApp.Accounts.list_users()\n", 1)
	editor.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": fileURI(caller), "version": 2},
		"contentChanges": []any{map[string]any{"text": unsaved}},
	})
	definition := map[string]any{
		"textDocument": map[string]any{"uri": fileURI(caller)},
		"position":     map[string]any{"line": 3, "character": 21},
	}
	editor.request(2, "textDocument/definition", definition)
	if res := editor.response(2); !strings.Contains(fmt.Sprint(res["result"]), "accounts.ex") {
		t.Fatalf("definition before the restart = %v", res)
	}

	stopFirst()
	select {
	case <-spawned:
	case <-time.After(20 * time.Second):
		t.Fatal("the proxy did not start a new daemon")
	}
	// The first request after the restart can race the replay; the proxy
	// holds it until the session is rebuilt, so it must succeed.
	editor.request(3, "textDocument/definition", definition)
	res := editor.response(3)
	if errObj, ok := res["error"].(map[string]any); ok {
		// The request reached the stopped daemon first. An editor repeats a
		// request that got ContentModified.
		if code, _ := idAsInt(errObj["code"]); code != lspContentModified {
			t.Fatalf("definition after the restart = %v", res)
		}
		editor.request(4, "textDocument/definition", definition)
		res = editor.response(4)
	}
	if !strings.Contains(fmt.Sprint(res["result"]), "accounts.ex") {
		t.Fatalf("definition from unsaved text after the restart = %v", res)
	}

	_ = proxyIn.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("proxy exit = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("proxy did not exit")
	}
}

// fakeLSPDaemon serves one LSP connection at root's socket the way a daemon
// does until it gets a request with method stopAt; then it stops without an
// answer, as a crashed daemon does.
func fakeLSPDaemon(t *testing.T, root, stopAt string) {
	t.Helper()
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", endpoint.Socket)
	if err != nil {
		t.Skipf("unix socket bind unavailable: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			reader := bufio.NewReader(conn)
			var h hello
			if err := readJSONLine(reader, &h); err != nil {
				_ = conn.Close()
				continue
			}
			_ = writeJSONLine(conn, helloResponse{OK: true, Contract: ContractVersion, PID: os.Getpid(), Session: "fake-1"})
			if h.Kind != kindLSP {
				// The control connection Ensure makes first; it only checks
				// that a daemon answers.
				go func() { _, _ = io.Copy(io.Discard, reader); _ = conn.Close() }()
				continue
			}
			side := &lspEditor{tb: t, w: conn, r: reader}
			for {
				message, err := side.read()
				if err != nil {
					break
				}
				if message["method"] == stopAt {
					break
				}
				if message["method"] == "initialize" {
					side.send(map[string]any{"id": message["id"], "result": map[string]any{"capabilities": map[string]any{}}})
				}
			}
			_ = conn.Close()
			_ = listener.Close()
			return
		}
	}()
}

// proxyOnRoot runs ProxyLSP for root over pipes.
func proxyOnRoot(t *testing.T, root string) (*lspEditor, io.Closer, chan error) {
	t.Helper()
	editorToProxy, proxyIn := io.Pipe()
	proxyOut, proxyToEditor := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := ProxyLSP(context.Background(), root, editorToProxy, proxyToEditor)
		done <- err
		_ = proxyToEditor.Close()
		// A test that writes after the proxy ended fails instead of hanging.
		_ = editorToProxy.CloseWithError(fmt.Errorf("the proxy ended: %v", err))
	}()
	t.Cleanup(func() { _ = proxyIn.Close() })
	return newLSPEditor(t, proxyIn, proxyOut).pump(), proxyIn, done
}

// TestProxyAnswersARequestThatTheStoppedDaemonDropped: a daemon stops while
// it handles a request. The editor gets an error for that request at once, not
// a request that waits for ever, and the session goes on on a new daemon.
func TestProxyAnswersARequestThatTheStoppedDaemonDropped(t *testing.T) {
	quietEnv(t)
	requireSocketSupport(t)
	root, caller, text := proxyFixture(t)
	fakeLSPDaemon(t, root, "textDocument/hover")
	origSpawn := spawnDaemon
	spawnDaemon = func(root string) (<-chan error, error) { runDaemonInProcess(t, root); return nil, nil }
	t.Cleanup(func() { spawnDaemon = origSpawn })

	editor, _, _ := proxyOnRoot(t, root)
	initializeEditor(t, editor, root, caller, text)
	editor.request(2, "textDocument/hover", hoverParams(caller))
	res := editor.response(2)
	errObj, ok := res["error"].(map[string]any)
	if !ok {
		t.Fatalf("the dropped request got %v, want an error", res)
	}
	if code, _ := idAsInt(errObj["code"]); code != lspContentModified {
		t.Fatalf("the dropped request got code %v, want ContentModified", errObj["code"])
	}
	// The new daemon builds its index first; hover answers once it has.
	deadline := time.Now().Add(20 * time.Second)
	for id := 3; ; id++ {
		editor.request(id, "textDocument/hover", hoverParams(caller))
		res := editor.response(id)
		if strings.Contains(fmt.Sprint(res["result"]), "Gets a user") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hover on the new daemon = %v", res)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestProxyExitsWithAnErrorWhenANewerDaemonTakesOver: the daemon is replaced by
// one from a newer build, which this proxy cannot attach to. The editor is told
// what to do and the proxy exits with an error, not with 0.
func TestProxyExitsWithAnErrorWhenANewerDaemonTakesOver(t *testing.T) {
	quietEnv(t)
	requireSocketSupport(t)
	root, caller, text := proxyFixture(t)
	stop := runDaemonInProcess(t, root)
	// The proxy must find the daemon listening; this test is about the
	// takeover, not the start.
	_ = waitForClient(t, root, make(chan error)).Close()
	editor, _, done := proxyOnRoot(t, root)
	initializeEditor(t, editor, root, caller, text)
	editor.request(2, "textDocument/hover", hoverParams(caller))
	editor.response(2)

	origSpawn := spawnDaemon
	spawnDaemon = func(string) (<-chan error, error) { return nil, nil }
	t.Cleanup(func() { spawnDaemon = origSpawn })
	stop()
	fakeDaemon(t, root, helloResponse{Error: "newer", Contract: ContractVersion + 1, PID: os.Getpid(), Incompatible: true})
	told := func() bool {
		for _, text := range editor.shown() {
			if strings.Contains(text, "lost the connection") && strings.Contains(text, "older Dexter build") {
				return true
			}
		}
		return false
	}
	for !told() {
		message, err := editor.next()
		if err != nil {
			t.Fatalf("the editor was not told: %v (shown: %q)", err, editor.shown())
		}
		editor.handle(message)
	}
	select {
	case err := <-done:
		var lost *LSPConnectionLostError
		if !errors.As(err, &lost) {
			t.Fatalf("proxy exit = %v, want an *LSPConnectionLostError", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("proxy did not exit")
	}
}

// initializedHarness runs the start of a session on the first daemon.
func initializedHarness(t *testing.T) (*proxyHarness, *lspEditor) {
	t.Helper()
	h, daemon := newProxyHarness(t, t.TempDir())
	h.editor.request(1, "initialize", map[string]any{})
	init := expectMethod(t, daemon, "initialize")
	daemon.send(map[string]any{"id": init["id"], "result": map[string]any{}})
	h.editor.response(1)
	return h, daemon
}

// TestProxyReadsTheDaemonWhileItReplays: a daemon answers each replayed
// document with diagnostics before it reads the next message. If the proxy
// did not read while it wrote the documents, both sides would block on the
// socket (net.Pipe has no buffer, so the first write shows it).
func TestProxyReadsTheDaemonWhileItReplays(t *testing.T) {
	h, daemon := initializedHarness(t)
	const docs = 3
	for i := range docs {
		uri := fmt.Sprintf("file:///p/lib/doc%d.ex", i)
		h.editor.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "elixir", "version": 1, "text": "x"}})
		expectMethod(t, daemon, "textDocument/didOpen")
	}
	next := h.nextDaemonRaw()
	_ = daemon.w.(net.Conn).Close()
	opened := make(chan error, 1)
	go func() {
		seen := 0
		for seen < docs {
			m, err := next.read()
			if err != nil {
				opened <- err
				return
			}
			switch m["method"] {
			case "initialize":
				next.send(map[string]any{"id": m["id"], "result": map[string]any{}})
			case "textDocument/didOpen":
				seen++
				params, _ := m["params"].(map[string]any)
				doc, _ := params["textDocument"].(map[string]any)
				next.send(map[string]any{"method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": doc["uri"], "diagnostics": []any{}}})
			}
		}
		opened <- nil
	}()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the replay blocked: the proxy did not read the daemon while it wrote")
	}
}

// TestProxyReplayQueueKeepsWhatItCouldNotSend: when the new daemon goes away
// while the proxy sends it the requests that waited, the request whose write
// failed is answered, and the ones after it go to the next daemon instead of
// waiting for ever.
func TestProxyReplayQueueKeepsWhatItCouldNotSend(t *testing.T) {
	h, daemon := initializedHarness(t)
	_ = daemon.w.(net.Conn).Close()
	time.Sleep(50 * time.Millisecond) // the proxy has no daemon now
	for id := 10; id <= 12; id++ {
		h.editor.request(id, "textDocument/hover", map[string]any{})
	}
	time.Sleep(50 * time.Millisecond)
	second := h.nextDaemonRaw()
	replayed := expectMethod(t, second, "initialize")
	second.send(map[string]any{"id": replayed["id"], "result": map[string]any{}})
	expectMethod(t, second, "textDocument/hover") // 10
	third := h.nextDaemon()
	_ = second.w.(net.Conn).Close()
	replayed = expectMethod(t, third, "initialize")
	third.send(map[string]any{"id": replayed["id"], "result": map[string]any{}})
	got := map[int]bool{}
	for len(got) < 3 {
		message, err := third.next()
		if err != nil {
			t.Fatalf("the third daemon: %v (got %v)", err, got)
		}
		if message["method"] == "textDocument/hover" {
			id, _ := idAsInt(message["id"])
			got[id] = true
			third.send(map[string]any{"id": message["id"], "result": nil})
		}
		if got[12] {
			break
		}
	}
	// The answers come in any order.
	answers := map[int]map[string]any{}
	for len(answers) < 3 {
		message, err := h.editor.next()
		if err != nil {
			t.Fatalf("answers so far %v: %v", answers, err)
		}
		if id, ok := idAsInt(message["id"]); ok && message["method"] == nil {
			answers[id] = message
		}
	}
	if answers[12]["error"] != nil {
		t.Fatalf("request 12 = %v, want an answer of the third daemon", answers[12])
	}
}

// TestProxyObeysShutdownAndExitWhileItReconnects: one daemon start can take
// 30 s; an editor that closes meanwhile must get its shutdown answered and see
// the proxy exit, not wait for the reconnection.
func TestProxyObeysShutdownAndExitWhileItReconnects(t *testing.T) {
	h, daemon := initializedHarness(t)
	h.dials <- func() (net.Conn, *bufio.Reader, error) {
		time.Sleep(30 * time.Second) // a daemon that does not come
		return nil, nil, errors.New("too late")
	}
	_ = daemon.w.(net.Conn).Close()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	h.editor.request(9, "shutdown", nil)
	res := h.editor.response(9)
	if res["error"] != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("shutdown = %v after %s, want a null result at once", res, time.Since(start))
	}
	h.editor.notify("exit", nil)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("proxy exit = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the proxy did not exit on exit")
	}
}

// TestProxyReportsTheCauseNotTheDeadline: when every reconnect fails, the
// editor must read why the daemon did not start, not "context deadline
// exceeded".
func TestProxyReportsTheCauseNotTheDeadline(t *testing.T) {
	previous := reconnectBudget
	reconnectBudget = 1500 * time.Millisecond
	t.Cleanup(func() { reconnectBudget = previous })
	h, daemon := initializedHarness(t)
	h.dials <- func() (net.Conn, *bufio.Reader, error) {
		return nil, nil, errors.New("the index at /p/.dexter/dexter.db cannot be opened")
	}
	_ = daemon.w.(net.Conn).Close()
	assertConnectionLost(t, h, "cannot be opened")
}

// TestProxyEndsTheProgressOfAStoppedDaemon: a daemon that stops during an
// index build cannot end its progress; the proxy ends it, or the editor shows
// it for ever.
func TestProxyEndsTheProgressOfAStoppedDaemon(t *testing.T) {
	h, daemon := initializedHarness(t)
	daemon.send(map[string]any{"method": "$/progress", "params": map[string]any{"token": "dexter/a/index/1", "value": map[string]any{"kind": "begin", "title": "Indexing"}}})
	daemon.send(map[string]any{"method": "$/progress", "params": map[string]any{"token": "dexter/a/index/2", "value": map[string]any{"kind": "begin", "title": "Indexing"}}})
	daemon.send(map[string]any{"method": "$/progress", "params": map[string]any{"token": "dexter/a/index/2", "value": map[string]any{"kind": "end"}}})
	ended := map[string]int{}
	for ended["dexter/a/index/2"] == 0 {
		message, err := h.editor.next()
		if err != nil {
			t.Fatal(err)
		}
		if params, ok := message["params"].(map[string]any); ok && message["method"] == "$/progress" {
			if value, _ := params["value"].(map[string]any); value["kind"] == "end" {
				ended[fmt.Sprint(params["token"])]++
			}
		}
	}
	h.nextDaemon()
	_ = daemon.w.(net.Conn).Close()
	deadline := time.Now().Add(5 * time.Second)
	for ended["dexter/a/index/1"] == 0 && time.Now().Before(deadline) {
		message, err := h.editor.next()
		if err != nil {
			t.Fatal(err)
		}
		if params, ok := message["params"].(map[string]any); ok && message["method"] == "$/progress" {
			if value, _ := params["value"].(map[string]any); value["kind"] == "end" {
				ended[fmt.Sprint(params["token"])]++
			}
		}
	}
	if ended["dexter/a/index/1"] != 1 || ended["dexter/a/index/2"] != 1 {
		t.Fatalf("ends = %v, want one end for each token", ended)
	}
}

func docFrame(t *testing.T, message map[string]any) ([]byte, int) {
	t.Helper()
	message["jsonrpc"] = "2.0"
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	frame := frameMessage(body)
	return frame, len(frame) - len(body)
}

// TestProxyKeepsOneCopyOfEachDocument: the proxy kept the didOpen and the
// last didChange, two copies of each document (with a 1 MB document the
// proxy grew from 12.8 to 20.7 MB). It keeps one.
func TestProxyKeepsOneCopyOfEachDocument(t *testing.T) {
	text := strings.Repeat("x", 1<<20)
	doc := &proxyDoc{}
	doc.open, _ = docFrame(t, map[string]any{"method": "textDocument/didOpen", "params": map[string]any{
		"textDocument": map[string]any{"uri": "file:///p/a.ex", "languageId": "elixir", "version": 1, "text": text}}})
	frame, body := docFrame(t, map[string]any{"method": "textDocument/didChange", "params": map[string]any{
		"textDocument": map[string]any{"uri": "file:///p/a.ex", "version": 2}, "contentChanges": []any{map[string]any{"text": text + "y"}}}})
	if !doc.applyChange(frame, body) {
		t.Fatal("the change was not kept")
	}
	if got := doc.retained(); got > len(text)+1024 {
		t.Fatalf("the proxy keeps %d bytes for a %d-byte document", got, len(text))
	}
	replay, err := doc.replayFrame()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := parseDocMessage(replay)
	if err != nil || msg.Params.TextDocument.Text == nil || *msg.Params.TextDocument.Text != text+"y" ||
		msg.Params.TextDocument.LanguageID != "elixir" || string(msg.Params.TextDocument.Version) != "2" {
		t.Fatalf("replayed didOpen is wrong: %v", err)
	}
}

// TestProxyAppliesRangeChanges: the server asks for full document sync, but an
// editor can still send range edits. The proxy applies them to its copy,
// counting characters in UTF-16 code units as LSP does, so a replay never
// sends stale text.
func TestProxyAppliesRangeChanges(t *testing.T) {
	doc := &proxyDoc{}
	doc.open, _ = docFrame(t, map[string]any{"method": "textDocument/didOpen", "params": map[string]any{
		"textDocument": map[string]any{"uri": "file:///p/a.ex", "languageId": "elixir", "version": 1, "text": "abc\n😀def\nghi\n"}}})
	rng := func(sl, sc, el, ec int) map[string]any {
		return map[string]any{"start": map[string]any{"line": sl, "character": sc}, "end": map[string]any{"line": el, "character": ec}}
	}
	frame, body := docFrame(t, map[string]any{"method": "textDocument/didChange", "params": map[string]any{
		"textDocument": map[string]any{"uri": "file:///p/a.ex", "version": 2},
		"contentChanges": []any{
			map[string]any{"range": rng(1, 2, 1, 3), "text": "D"},   // after the emoji (2 UTF-16 units): d -> D
			map[string]any{"range": rng(0, 3, 0, 3), "text": "!"},   // insert at the end of line 0
			map[string]any{"range": rng(2, 0, 3, 0), "text": "end"}, // replace the last line with its newline
		}}})
	if !doc.applyChange(frame, body) {
		t.Fatal("the change was not applied")
	}
	text, _, version, err := doc.text()
	if err != nil {
		t.Fatal(err)
	}
	if want := "abc!\n😀Def\nend"; text != want || string(version) != "2" {
		t.Fatalf("text = %q (version %s), want %q", text, version, want)
	}
	// A full change after range changes replaces everything.
	frame, body = docFrame(t, map[string]any{"method": "textDocument/didChange", "params": map[string]any{
		"textDocument": map[string]any{"uri": "file:///p/a.ex", "version": 3}, "contentChanges": []any{map[string]any{"text": "new"}}}})
	doc.applyChange(frame, body)
	if text, _, _, _ := doc.text(); text != "new" {
		t.Fatalf("text after a full change = %q", text)
	}
}

// TestProxyAnswersCancelsForRequestsThatNoDaemonHas: a cancel for a request
// that waits for a daemon removes it and answers RequestCancelled; a cancel
// for a request that the proxy answered when the old daemon stopped does not
// go to the new daemon, which never had it.
func TestProxyAnswersCancelsForRequestsThatNoDaemonHas(t *testing.T) {
	h, daemon := initializedHarness(t)
	h.editor.request(21, "textDocument/references", map[string]any{})
	expectMethod(t, daemon, "textDocument/references")
	_ = daemon.w.(net.Conn).Close()
	if res := h.editor.response(21); res["error"] == nil {
		t.Fatalf("request 21 = %v, want an error", res)
	}
	h.editor.request(20, "textDocument/hover", map[string]any{})
	time.Sleep(50 * time.Millisecond)
	h.editor.notify("$/cancelRequest", map[string]any{"id": 20})
	res := h.editor.response(20)
	if errObj, _ := res["error"].(map[string]any); fmt.Sprint(errObj["code"]) != "-32800" {
		t.Fatalf("canceled queued request = %v, want RequestCancelled", res)
	}
	h.editor.notify("$/cancelRequest", map[string]any{"id": 21})
	next := h.nextDaemon()
	replayed := expectMethod(t, next, "initialize")
	next.send(map[string]any{"id": replayed["id"], "result": map[string]any{}})
	h.editor.request(22, "textDocument/hover", map[string]any{})
	if got := expectMethod(t, next, "textDocument/hover"); fmt.Sprint(got["id"]) != "22" {
		t.Fatalf("the new daemon got %v, want only request 22", got)
	}
}

// referenceOffset is utf16Offset written another way, for comparison: it
// splits the text into lines first, and encodes each line to UTF-16.
func referenceOffset(text string, pos position) int {
	starts, ends := []int{0}, []int{}
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			ends = append(ends, i)
			starts = append(starts, i+1)
		case '\r':
			ends = append(ends, i)
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
			starts = append(starts, i+1)
		}
	}
	ends = append(ends, len(text))
	if pos.Line < 0 {
		pos.Line = 0
	}
	if pos.Line >= len(starts) {
		return len(text)
	}
	line := text[starts[pos.Line]:ends[pos.Line]]
	offset, units := 0, 0
	for offset < len(line) {
		r, size := utf8.DecodeRuneInString(line[offset:])
		n := len(utf16.Encode([]rune{r}))
		if units+n > pos.Character {
			break
		}
		units += n
		offset += size
	}
	return starts[pos.Line] + offset
}

// TestUTF16OffsetFollowsTheLSPRules covers the line ends and the clamping
// that the first version got wrong.
func TestUTF16OffsetFollowsTheLSPRules(t *testing.T) {
	cases := []struct {
		text      string
		line, col int
		want      int
	}{
		{"ab\r\ncd", 0, 9, 2},   // past a CRLF line end: before \r, not between \r and \n
		{"ab\rcd", 1, 1, 4},     // a lone \r ends a line
		{"ab\rcd", 0, 5, 2},     // and the line is clamped before it
		{"😀x", 0, 1, 0},         // inside a surrogate pair: before the character
		{"😀x", 0, 2, 4},         // after it
		{"a\n\nb", 2, 0, 3},     // an empty line
		{"a\r\n\r\nb", 2, 1, 6}, // CRLF lines
		{"abc", 5, 0, 3},        // past the last line
		{"abc", 0, -1, 0},       // a negative character
	}
	for _, tc := range cases {
		pos := position{Line: tc.line, Character: tc.col}
		if got := utf16Offset(tc.text, pos); got != tc.want {
			t.Errorf("utf16Offset(%q, %d:%d) = %d, want %d", tc.text, tc.line, tc.col, got, tc.want)
		}
		if got := referenceOffset(tc.text, pos); got != tc.want {
			t.Errorf("referenceOffset(%q, %d:%d) = %d, want %d", tc.text, tc.line, tc.col, got, tc.want)
		}
	}
}

// FuzzUTF16Offset compares utf16Offset with the reference on any text.
func FuzzUTF16Offset(f *testing.F) {
	for _, seed := range []string{"ab\r\ncd", "ab\rcd", "😀x", "a\n\nb", "a\r\n\r\nb", "x\r", "\r\n", "é😀\r\n𝄞b", "\xff\xfe\n"} {
		for _, pos := range [][2]int{{0, 0}, {0, 1}, {0, 9}, {1, 1}, {2, 0}} {
			f.Add(seed, pos[0], pos[1])
		}
	}
	f.Fuzz(func(t *testing.T, text string, line, col int) {
		pos := position{Line: line % 64, Character: col % 256}
		got := utf16Offset(text, pos)
		if want := referenceOffset(text, pos); got != want {
			t.Fatalf("utf16Offset(%q, %+v) = %d, reference %d", text, pos, got, want)
		}
	})
}

// TestProxyRetainsOneDocumentCopy: with a 1 MB document the proxy process grew
// by about 8 MB. That is garbage that the collector had not freed yet (each
// message is read into a new buffer, and GOGC lets the heap reach twice the
// live size), not a retained buffer: after a collection the proxy holds about
// one copy of the document.
func TestProxyRetainsOneDocumentCopy(t *testing.T) {
	editorToProxy, proxyIn := io.Pipe()
	proxySide, daemonSide := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, daemonSide) }()
	proxy := newLSPProxy(context.Background(), t.TempDir(), editorToProxy, io.Discard, func(context.Context) (net.Conn, *bufio.Reader, error) {
		return nil, nil, errors.New("no reconnect in this test")
	})
	proxy.conn = proxySide
	done := make(chan struct{})
	go func() {
		proxy.readEditor()
		close(done)
	}()
	text := strings.Repeat("defmodule A do\n  def x, do: 1\nend\n", (1<<20)/34)
	lastFrame := 0
	send := func(message map[string]any) {
		message["jsonrpc"] = "2.0"
		body, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if message["method"] == "textDocument/didChange" {
			lastFrame = len(frameMessage(body))
		}
		if _, err := proxyIn.Write(frameMessage(body)); err != nil {
			t.Fatal(err)
		}
	}
	measure := func() uint64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := measure()
	uri := "file:///p/lib/a.ex"
	send(map[string]any{"method": "textDocument/didOpen", "params": map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "elixir", "version": 1, "text": text}}})
	for v := 2; v < 22; v++ {
		send(map[string]any{"method": "textDocument/didChange", "params": map[string]any{
			"textDocument": map[string]any{"uri": uri, "version": v}, "contentChanges": []any{map[string]any{"text": text + strconv.Itoa(v)}}}})
	}
	send(map[string]any{"method": "textDocument/hover", "id": 1, "params": map[string]any{}}) // the last change is read
	after := measure()
	runtime.KeepAlive(text)
	retained := int64(after) - int64(before)
	debug.FreeOSMemory()
	proxy.stateMu.Lock()
	kept := 0
	for _, doc := range proxy.docs {
		kept += doc.retained()
	}
	proxy.stateMu.Unlock()
	t.Logf("document %d bytes, kept in the document state %d bytes, heap growth after GC %d bytes", len(text), kept, retained)
	if kept != lastFrame {
		t.Fatalf("the document state keeps %d bytes, want one message of %d bytes", kept, lastFrame)
	}
	// One document message, plus the reader buffers (64 KiB) and the maps.
	if retained > int64(lastFrame)+512<<10 {
		t.Fatalf("the proxy heap grew by %d bytes for one %d-byte document message", retained, lastFrame)
	}
	_ = proxyIn.Close()
	<-done
	runtime.KeepAlive(proxy)
}

// TestUnsavedBufferStaysUnsavedAfterADaemonRestart: MCP tools and the CLI
// read an editor's buffer only while it has unsaved changes. A replay that
// only opened the document made the buffer look saved on the new daemon, so
// they read the disk text until the next keystroke. A saved buffer stays
// saved.
func TestUnsavedBufferStaysUnsavedAfterADaemonRestart(t *testing.T) {
	quietEnv(t)
	requireSocketSupport(t)
	root, caller, text := proxyFixture(t)
	saved := filepath.Join(root, "lib", "my_app", "accounts.ex")
	method := fmt.Sprintf("test/unsaved-%d", time.Now().UnixNano())
	RegisterMethod(method, func(mc MethodContext, raw json.RawMessage) (any, error) {
		var path string
		if err := json.Unmarshal(raw, &path); err != nil {
			return nil, err
		}
		buffer, ok := mc.Runtime.UnsavedBuffer(path) // what MCP tools read
		if !ok {
			return "", nil
		}
		return buffer.Text, nil
	})
	unsavedText := func(path string) string {
		client, err := Dial(context.Background(), root)
		if err != nil {
			return "<no daemon>"
		}
		defer func() { _ = client.Close() }()
		var got string
		if err := client.Call(context.Background(), method, path, &got); err != nil {
			return "<" + err.Error() + ">"
		}
		return got
	}
	stopFirst := runDaemonInProcess(t, root)
	_ = waitForClient(t, root, make(chan error)).Close()
	origSpawn := spawnDaemon
	spawned := make(chan struct{}, 4)
	spawnDaemon = func(root string) (<-chan error, error) {
		runDaemonInProcess(t, root)
		spawned <- struct{}{}
		return nil, nil
	}
	t.Cleanup(func() { spawnDaemon = origSpawn })

	editor, _, _ := proxyOnRoot(t, root)
	initializeEditor(t, editor, root, caller, text)
	edited := text + "# unsaved\n"
	editor.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": fileURI(caller), "version": 2},
		"contentChanges": []any{map[string]any{"text": edited}},
	})
	savedText, _ := os.ReadFile(saved)
	editor.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(saved), "languageId": "elixir", "version": 1, "text": string(savedText)}})
	editor.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": fileURI(saved), "version": 2},
		"contentChanges": []any{map[string]any{"text": string(savedText)}},
	})
	editor.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": fileURI(saved)}})
	eventually(t, 5*time.Second, func() bool { return unsavedText(caller) == edited && unsavedText(saved) == "" })

	stopFirst()
	select {
	case <-spawned:
	case <-time.After(20 * time.Second):
		t.Fatal("the proxy did not start a new daemon")
	}
	eventually(t, 10*time.Second, func() bool { return unsavedText(caller) == edited })
	if got := unsavedText(saved); got != "" {
		t.Fatalf("a saved buffer is unsaved after the restart: %q", got)
	}
}

// retained reports how many message bytes the document keeps.
func (d *proxyDoc) retained() int { return len(d.open) + len(d.change) }

// TestProxySendsAnInitializeFromAReconnectOnce: the editor sends initialize
// while the proxy has no daemon. The replay sends the kept initialize; the
// queue must not send it a second time, or the editor gets two answers.
func TestProxySendsAnInitializeFromAReconnectOnce(t *testing.T) {
	h, daemon := newProxyHarness(t, t.TempDir())
	release := make(chan struct{})
	proxySide, daemonSide := net.Pipe()
	h.dials <- func() (net.Conn, *bufio.Reader, error) {
		<-release
		return proxySide, bufio.NewReader(proxySide), nil
	}
	next := newLSPEditor(t, daemonSide, daemonSide).pump()
	_ = daemon.w.(net.Conn).Close()
	time.Sleep(100 * time.Millisecond) // the proxy waits for a daemon
	h.editor.request(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	time.Sleep(50 * time.Millisecond)
	close(release)

	first := expectMethod(t, next, "initialize")
	next.send(map[string]any{"id": first["id"], "result": map[string]any{"capabilities": map[string]any{}}})
	if res := h.editor.response(1); res["error"] != nil {
		t.Fatalf("initialize answer = %v", res)
	}
	h.editor.request(2, "textDocument/hover", map[string]any{})
	if m := expectMethod(t, next, "textDocument/hover"); m != nil {
		next.send(map[string]any{"id": m["id"], "result": nil})
	}
	for {
		m, err := h.editor.next()
		if err != nil {
			t.Fatal(err)
		}
		if h.editor.handle(m) {
			continue
		}
		id, _ := idAsInt(m["id"])
		if id == 1 {
			t.Fatalf("the editor got a second answer to initialize: %v", m)
		}
		if id == 2 {
			return
		}
	}
}

// TestProxyDoesNotReplayASaveAfterLaterChanges: a save that the editor sends
// while there is no daemon must not reach the new daemon after the replayed
// buffer, or the daemon takes the newer, unsaved text as saved.
func TestProxyDoesNotReplayASaveAfterLaterChanges(t *testing.T) {
	h, daemon := initializedHarness(t)
	const uri = "file:///p/lib/a.ex"
	h.editor.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": uri, "languageId": "elixir", "version": 1, "text": "v1"}})
	expectMethod(t, daemon, "textDocument/didOpen")

	release := make(chan struct{})
	proxySide, daemonSide := net.Pipe()
	h.dials <- func() (net.Conn, *bufio.Reader, error) {
		<-release
		return proxySide, bufio.NewReader(proxySide), nil
	}
	next := newLSPEditor(t, daemonSide, daemonSide).pump()
	_ = daemon.w.(net.Conn).Close()
	time.Sleep(100 * time.Millisecond) // the proxy waits for a daemon
	change := func(version int, text string) {
		h.editor.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []any{map[string]any{"text": text}},
		})
	}
	change(2, "v2")
	h.editor.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": uri}})
	change(3, "v3")
	time.Sleep(50 * time.Millisecond)
	close(release)

	init := expectMethod(t, next, "initialize")
	next.send(map[string]any{"id": init["id"], "result": map[string]any{}})
	h.editor.request(2, "textDocument/hover", map[string]any{})
	var last string
	for {
		m, err := next.next()
		if err != nil {
			t.Fatal(err)
		}
		switch m["method"] {
		case "textDocument/didSave":
			t.Fatalf("the new daemon got a save after the replay; the buffer %q is unsaved", last)
		case "textDocument/didOpen", "textDocument/didChange":
			last = textOf(t, m)
		case "textDocument/hover":
			if last != "v3" {
				t.Fatalf("the new daemon has %q, want v3", last)
			}
			next.send(map[string]any{"id": m["id"], "result": nil})
			return
		}
	}
}

// TestProxyPassesARegistrationTheEditorNeverGot: when the first daemon stops
// before it registers for file watching, the next daemon's registration goes
// to the editor. Later daemons' registrations are answered by the proxy.
func TestProxyPassesARegistrationTheEditorNeverGot(t *testing.T) {
	h, daemon := initializedHarness(t)
	register := func(side *lspEditor, id int) {
		side.send(map[string]any{"id": id, "method": "client/registerCapability", "params": map[string]any{"registrations": []any{}}})
	}
	reconnect := func(old *lspEditor) *lspEditor {
		next := h.nextDaemon()
		_ = old.w.(net.Conn).Close()
		init := expectMethod(t, next, "initialize")
		next.send(map[string]any{"id": init["id"], "result": map[string]any{}})
		return next
	}

	second := reconnect(daemon)
	register(second, 7)
	for {
		m, err := h.editor.next()
		if err != nil {
			t.Fatal(err)
		}
		if m["method"] == "client/registerCapability" {
			h.editor.send(map[string]any{"id": m["id"], "result": nil})
			break
		}
	}
	if res, err := second.next(); err != nil || res["error"] != nil {
		t.Fatalf("answer to the registration = %v, %v", res, err)
	}

	third := reconnect(second)
	register(third, 8)
	res, err := third.next()
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := idAsInt(res["id"]); id != 8 || res["error"] != nil {
		t.Fatalf("the proxy's answer to a repeated registration = %v", res)
	}
}
