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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/store"
)

// routeHarness runs an lspProxy whose dials reach daemons that the test plays,
// one queue of daemon sides per project root.
type routeHarness struct {
	t      *testing.T
	editor *lspEditor
	done   chan error
	in     io.WriteCloser

	mu      sync.Mutex
	daemons map[string]chan *lspEditor
	dialed  []string
}

func (h *routeHarness) queue(root string) chan *lspEditor {
	h.mu.Lock()
	defer h.mu.Unlock()
	q := h.daemons[root]
	if q == nil {
		q = make(chan *lspEditor, 8)
		h.daemons[root] = q
	}
	return q
}

// daemon returns the next daemon side that the proxy reached for root.
func (h *routeHarness) daemon(root string) *lspEditor {
	h.t.Helper()
	select {
	case d := <-h.queue(root):
		return d
	case <-time.After(5 * time.Second):
		h.mu.Lock()
		defer h.mu.Unlock()
		h.t.Fatalf("the proxy did not reach a daemon for %s (dials: %v)", root, h.dialed)
		return nil
	}
}

func (h *routeHarness) dials() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.dialed...)
}

// newRouteHarness starts a proxy on root. For a root that is not a
// container, the primary daemon side is returned.
func newRouteHarness(t *testing.T, root string, container bool) (*routeHarness, *lspEditor) {
	t.Helper()
	editorToProxy, proxyIn := io.Pipe()
	proxyOut, proxyToEditor := io.Pipe()
	h := &routeHarness{t: t, done: make(chan error, 1), in: proxyIn, daemons: make(map[string]chan *lspEditor)}
	proxy := newLSPProxy(context.Background(), root, container, editorToProxy, proxyToEditor, func(ctx context.Context, dialRoot string) (net.Conn, *bufio.Reader, error) {
		h.mu.Lock()
		h.dialed = append(h.dialed, dialRoot)
		h.mu.Unlock()
		proxySide, daemonSide := net.Pipe()
		h.queue(dialRoot) <- newLSPEditor(t, daemonSide, daemonSide).pump()
		return proxySide, bufio.NewReader(proxySide), nil
	})
	var primary *lspEditor
	var first net.Conn
	var reader *bufio.Reader
	if !container {
		var daemonSide net.Conn
		first, daemonSide = net.Pipe()
		reader = bufio.NewReader(first)
		primary = newLSPEditor(t, daemonSide, daemonSide).pump()
	}
	go func() {
		err := proxy.run(first, reader)
		h.done <- err
		_ = proxyToEditor.Close()
		_ = editorToProxy.CloseWithError(fmt.Errorf("the proxy ended: %v", err))
	}()
	t.Cleanup(func() { _ = proxyIn.Close() })
	h.editor = newLSPEditor(t, proxyIn, proxyOut).pump()
	return h, primary
}

// connectUpstream answers the proxy's initialize on a new daemon side and
// checks that it names root, then reads the replayed state (initialized and
// the configuration, when the editor sent them) up to the didOpen of uri.
func connectUpstream(t *testing.T, d *lspEditor, root, uri string) map[string]any {
	t.Helper()
	init := expectMethod(t, d, "initialize")
	id, isString := init["id"].(string)
	if !isString || !strings.HasPrefix(id, "dexter-proxy-") {
		t.Fatalf("the upstream initialize has id %v, want an id of the proxy", init["id"])
	}
	params, _ := init["params"].(map[string]any)
	if params["rootUri"] != fileURI(root) || params["rootPath"] != root {
		t.Fatalf("the upstream initialize names %v / %v, want %s", params["rootUri"], params["rootPath"], root)
	}
	folders, _ := params["workspaceFolders"].([]any)
	if len(folders) != 1 || folders[0].(map[string]any)["uri"] != fileURI(root) {
		t.Fatalf("the upstream initialize has workspace folders %v, want only %s", folders, root)
	}
	d.send(map[string]any{"id": init["id"], "result": map[string]any{"capabilities": map[string]any{}}})
	for {
		m := expectAny(t, d)
		if m["method"] == "textDocument/didOpen" && docURI(m) == uri {
			return init
		}
	}
}

func expectAny(t *testing.T, side *lspEditor) map[string]any {
	t.Helper()
	m, err := side.next()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func docURI(m map[string]any) string {
	params, _ := m["params"].(map[string]any)
	if doc, ok := params["textDocument"].(map[string]any); ok {
		return fmt.Sprint(doc["uri"])
	}
	return ""
}

func openDoc(e *lspEditor, path, text string) {
	e.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(path), "languageId": "elixir", "version": 1, "text": text}})
}

func at(path string) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}, "position": map[string]any{"line": 0, "character": 0}}
}

// answerNext reads the next message of d, checks its method, and answers it.
func answerNext(t *testing.T, d *lspEditor, method string, result any) map[string]any {
	t.Helper()
	m := expectMethod(t, d, method)
	d.send(map[string]any{"id": m["id"], "result": result})
	return m
}

// linkWorktree makes dir a linked worktree of repo, as git worktree add does.
func linkWorktree(t *testing.T, repo, dir string) {
	t.Helper()
	admin := filepath.Join(repo, ".git", "worktrees", filepath.Base(dir))
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, text := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// containerFixture is a directory that is a repository itself and holds a
// monorepo clone (shop), two linked worktrees of it, and a clone that is not
// Elixir (web).
func containerFixture(t *testing.T) (work, shop, featureA, featureB string) {
	t.Helper()
	work = t.TempDir()
	shop = filepath.Join(work, "shop")
	featureA = filepath.Join(work, "shop-worktrees", "feature-a")
	featureB = filepath.Join(work, "shop-worktrees", "feature-b")
	for _, dir := range []string{filepath.Join(work, ".git"), filepath.Join(shop, ".git"), filepath.Join(work, "web", ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFiles(t, shop, map[string]string{"apps/shop/mix.exs": "", "libs/util/mix.exs": ""})
	for _, wt := range []string{featureA, featureB} {
		linkWorktree(t, shop, wt)
		writeFiles(t, wt, map[string]string{"apps/shop/mix.exs": "", "libs/util/mix.exs": ""})
	}
	writeFiles(t, work, map[string]string{"web/package.json": "{}"})
	if !store.IsProjectContainer(work) {
		t.Fatal("the fixture is not a container")
	}
	return work, shop, featureA, featureB
}

// TestProxyRoutesEachFileOfAContainerToItsOwnProject: a session that starts
// in a directory with a clone and worktrees of it starts no daemon for that
// directory. Each file goes to the daemon of its own checkout, and the
// editor sees one server.
func TestProxyRoutesEachFileOfAContainerToItsOwnProject(t *testing.T) {
	work, shop, featureA, featureB := containerFixture(t)
	h, _ := newRouteHarness(t, work, true)
	e := h.editor

	e.request(1, "initialize", map[string]any{
		"rootUri":               fileURI(work),
		"capabilities":          map[string]any{"window": map[string]any{"workDoneProgress": true}},
		"initializationOptions": map[string]any{"followDelegates": false},
	})
	res := e.response(1)
	want, _ := json.Marshal(lsp.NewInitializeResult())
	got, _ := json.Marshal(res["result"])
	var wantMap, gotMap map[string]any
	_ = json.Unmarshal(want, &wantMap)
	_ = json.Unmarshal(got, &gotMap)
	if fmt.Sprint(wantMap) != fmt.Sprint(gotMap) {
		t.Fatalf("initialize result = %s, want the daemon's %s", got, want)
	}
	e.notify("initialized", map[string]any{})
	e.notify("workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{}})
	time.Sleep(50 * time.Millisecond)
	if dials := h.dials(); len(dials) != 0 {
		t.Fatalf("the proxy dialed %v before a file was opened; the container has no daemon", dials)
	}

	fileA := filepath.Join(featureA, "apps", "shop", "lib", "a.ex")
	fileB := filepath.Join(featureB, "apps", "shop", "lib", "b.ex")
	fileS := filepath.Join(shop, "libs", "util", "lib", "s.ex")
	openDoc(e, fileA, "a")
	da := h.daemon(featureA)
	initA := connectUpstream(t, da, featureA, fileURI(fileA))
	if opts, _ := initA["params"].(map[string]any)["initializationOptions"].(map[string]any); opts["followDelegates"] != false {
		t.Fatalf("the upstream initialize lost the editor's options: %v", initA["params"])
	}
	openDoc(e, fileB, "b")
	db := h.daemon(featureB)
	connectUpstream(t, db, featureB, fileURI(fileB))
	openDoc(e, fileS, "s")
	ds := h.daemon(shop)
	connectUpstream(t, ds, shop, fileURI(fileS))

	// Requests go to the daemon of their file.
	e.request(10, "textDocument/hover", at(fileB))
	answerNext(t, db, "textDocument/hover", "from feature-b")
	if r := e.response(10); r["result"] != "from feature-b" {
		t.Fatalf("hover in feature-b = %v", r)
	}
	e.request(11, "textDocument/definition", at(fileA))
	answerNext(t, da, "textDocument/definition", "from feature-a")
	if r := e.response(11); r["result"] != "from feature-a" {
		t.Fatalf("definition in feature-a = %v", r)
	}
	e.request(12, "callHierarchy/incomingCalls", map[string]any{"item": map[string]any{"name": "f", "uri": fileURI(fileS)}})
	answerNext(t, ds, "callHierarchy/incomingCalls", "from shop")
	if r := e.response(12); r["result"] != "from shop" {
		t.Fatalf("incoming calls in shop = %v", r)
	}

	// workspace/symbol asks every daemon and joins the results.
	e.request(13, "workspace/symbol", map[string]any{"query": "x"})
	answerNext(t, da, "workspace/symbol", []any{map[string]any{"name": "a"}})
	answerNext(t, db, "workspace/symbol", []any{})
	answerNext(t, ds, "workspace/symbol", []any{map[string]any{"name": "s1"}, map[string]any{"name": "s2"}})
	symbols, _ := e.response(13)["result"].([]any)
	var names []string
	for _, s := range symbols {
		names = append(names, fmt.Sprint(s.(map[string]any)["name"]))
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "a,s1,s2" {
		t.Fatalf("workspace/symbol = %v, want the symbols of every daemon", symbols)
	}

	// A cancel goes to the daemon that holds the request, and only there.
	e.request(14, "textDocument/references", at(fileB))
	expectMethod(t, db, "textDocument/references")
	e.notify("$/cancelRequest", map[string]any{"id": 14})
	if c := expectMethod(t, db, "$/cancelRequest"); fmt.Sprint(c["params"].(map[string]any)["id"]) != "14" {
		t.Fatalf("cancel = %v", c)
	}
	db.send(map[string]any{"id": 14, "error": map[string]any{"code": lspRequestCancelled, "message": "canceled"}})
	e.response(14)
	e.request(15, "textDocument/hover", at(fileA))
	if m := expectMethod(t, da, "textDocument/hover"); fmt.Sprint(m["id"]) != "15" {
		t.Fatalf("feature-a got %v", m)
	}
	da.send(map[string]any{"id": 15, "result": nil})
	e.response(15)

	// Two daemons send requests with the same id and the same progress
	// token. The editor gets two ids and two tokens, and each answer goes
	// back to the daemon that asked.
	da.send(map[string]any{"id": 1, "method": "window/workDoneProgress/create", "params": map[string]any{"token": "dexter/x/index/1"}})
	db.send(map[string]any{"id": 1, "method": "window/workDoneProgress/create", "params": map[string]any{"token": "dexter/x/index/1"}})
	var creates []map[string]any
	for len(creates) < 2 {
		m := expectAny(t, e)
		if m["method"] == "window/workDoneProgress/create" {
			creates = append(creates, m)
		}
	}
	if fmt.Sprint(creates[0]["id"]) == fmt.Sprint(creates[1]["id"]) {
		t.Fatalf("the editor got id %v for both daemons' requests", creates[0]["id"])
	}
	tokenOf := func(m map[string]any) string { return fmt.Sprint(m["params"].(map[string]any)["token"]) }
	if tokenOf(creates[0]) == tokenOf(creates[1]) {
		t.Fatalf("the editor got token %s from both daemons", tokenOf(creates[0]))
	}
	e.send(map[string]any{"id": creates[1]["id"], "result": nil})
	e.send(map[string]any{"id": creates[0]["id"], "result": nil})
	for _, d := range []*lspEditor{da, db} {
		if r := expectAny(t, d); fmt.Sprint(r["id"]) != "1" || r["method"] != nil {
			t.Fatalf("the daemon got %v, want the answer to its request 1", r)
		}
	}
	da.send(map[string]any{"method": "$/progress", "params": map[string]any{"token": "dexter/x/index/1", "value": map[string]any{"kind": "begin", "title": "Indexing"}}})
	tokens := map[string]bool{tokenOf(creates[0]): true, tokenOf(creates[1]): true}
	for {
		m := expectAny(t, e)
		if m["method"] == "$/progress" {
			if !tokens[tokenOf(m)] {
				t.Fatalf("progress token %s is not one the editor created", tokenOf(m))
			}
			break
		}
	}
	// The editor cancels that progress: the daemon that made it gets its
	// own token.
	for _, c := range creates {
		e.notify("window/workDoneProgress/cancel", map[string]any{"token": c["params"].(map[string]any)["token"]})
	}
	for _, d := range []*lspEditor{da, db} {
		if m := expectMethod(t, d, "window/workDoneProgress/cancel"); tokenOf(m) != "dexter/x/index/1" {
			t.Fatalf("progress cancel = %v", m)
		}
	}

	// The second registration of the file watcher is answered by the proxy.
	register := map[string]any{"registrations": []any{map[string]any{"id": "dexter-file-watcher", "method": "workspace/didChangeWatchedFiles"}}}
	da.send(map[string]any{"id": 2, "method": "client/registerCapability", "params": register})
	for {
		m := expectAny(t, e)
		if m["method"] == "client/registerCapability" {
			e.send(map[string]any{"id": m["id"], "result": nil})
			break
		}
	}
	if r := expectAny(t, da); fmt.Sprint(r["id"]) != "2" {
		t.Fatalf("feature-a got %v", r)
	}
	db.send(map[string]any{"id": 2, "method": "client/registerCapability", "params": register})
	if r := expectAny(t, db); fmt.Sprint(r["id"]) != "2" || r["error"] != nil {
		t.Fatalf("the proxy's answer to the second registration = %v", r)
	}

	// Watched file changes are split by project.
	e.notify("workspace/didChangeWatchedFiles", map[string]any{"changes": []any{
		map[string]any{"uri": fileURI(filepath.Join(featureA, "apps", "shop", "lib", "c.ex")), "type": 2},
		map[string]any{"uri": fileURI(filepath.Join(featureB, "apps", "shop", "lib", "d.ex")), "type": 2},
	}})
	for d, want := range map[*lspEditor]string{da: "c.ex", db: "d.ex"} {
		m := expectMethod(t, d, "workspace/didChangeWatchedFiles")
		changes := m["params"].(map[string]any)["changes"].([]any)
		if len(changes) != 1 || !strings.HasSuffix(fmt.Sprint(changes[0].(map[string]any)["uri"]), want) {
			t.Fatalf("watched changes = %v, want only %s", changes, want)
		}
	}

	// Shutdown goes to every daemon; the editor gets one answer.
	e.request(20, "shutdown", nil)
	for _, d := range []*lspEditor{da, db, ds} {
		answerNext(t, d, "shutdown", nil)
	}
	if r := e.response(20); r["error"] != nil {
		t.Fatalf("shutdown = %v", r)
	}
	e.notify("exit", nil)
	for _, d := range []*lspEditor{da, db, ds} {
		expectMethod(t, d, "exit")
		_ = d.w.(net.Conn).Close()
	}
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("proxy exit = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy did not end")
	}
}

// TestProxyRestartsOneUpstreamWhileTheOtherServes: the daemon of one
// worktree stops. Its session is replayed on a new daemon of that worktree,
// with only its own documents, and the other worktree is answered meanwhile.
func TestProxyRestartsOneUpstreamWhileTheOtherServes(t *testing.T) {
	work, _, featureA, featureB := containerFixture(t)
	h, _ := newRouteHarness(t, work, true)
	e := h.editor
	e.request(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	e.response(1)
	e.notify("initialized", map[string]any{})
	fileA := filepath.Join(featureA, "lib", "a.ex")
	fileB := filepath.Join(featureB, "lib", "b.ex")
	openDoc(e, fileA, "a")
	da := h.daemon(featureA)
	connectUpstream(t, da, featureA, fileURI(fileA))
	openDoc(e, fileB, "b")
	db := h.daemon(featureB)
	connectUpstream(t, db, featureB, fileURI(fileB))

	e.request(5, "textDocument/hover", at(fileA))
	expectMethod(t, da, "textDocument/hover")
	_ = da.w.(net.Conn).Close()
	if r := e.response(5); r["error"] == nil {
		t.Fatalf("the open request of the stopped daemon = %v, want an error", r)
	}
	next := h.daemon(featureA)
	// feature-b is answered while feature-a's new daemon has not answered
	// its initialize.
	init := expectMethod(t, next, "initialize")
	e.request(6, "textDocument/hover", at(fileB))
	answerNext(t, db, "textDocument/hover", "b")
	if r := e.response(6); r["result"] != "b" {
		t.Fatalf("hover in feature-b during the restart = %v", r)
	}
	e.request(7, "textDocument/hover", at(fileA))
	next.send(map[string]any{"id": init["id"], "result": map[string]any{}})
	expectMethod(t, next, "initialized")
	if m := expectMethod(t, next, "textDocument/didOpen"); docURI(m) != fileURI(fileA) {
		t.Fatalf("the replay opened %v, want only feature-a's document", docURI(m))
	}
	answerNext(t, next, "textDocument/hover", "a again")
	if r := e.response(7); r["result"] != "a again" {
		t.Fatalf("hover in feature-a after the restart = %v", r)
	}
}

// TestProxyRoutesANestedWorktreeToItsOwnDaemon: an agent worktree inside a
// project is a project of its own. Its files go to its own daemon, not to
// the daemon of the project that holds it.
func TestProxyRoutesANestedWorktreeToItsOwnDaemon(t *testing.T) {
	shop := t.TempDir()
	if err := os.MkdirAll(filepath.Join(shop, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, shop, map[string]string{"mix.exs": ""})
	nested := filepath.Join(shop, ".agents", "worktrees", "x")
	linkWorktree(t, shop, nested)
	writeFiles(t, nested, map[string]string{"mix.exs": ""})

	h, primary := newRouteHarness(t, shop, false)
	e := h.editor
	e.request(1, "initialize", map[string]any{"rootUri": fileURI(shop), "capabilities": map[string]any{}})
	answerNext(t, primary, "initialize", map[string]any{})
	e.response(1)
	e.notify("initialized", map[string]any{})
	expectMethod(t, primary, "initialized")

	own := filepath.Join(shop, "lib", "a.ex")
	inNested := filepath.Join(nested, "lib", "a.ex")
	openDoc(e, own, "own")
	expectMethod(t, primary, "textDocument/didOpen")
	openDoc(e, inNested, "nested")
	dx := h.daemon(nested)
	connectUpstream(t, dx, nested, fileURI(inNested))
	e.request(2, "textDocument/hover", at(inNested))
	answerNext(t, dx, "textDocument/hover", "nested")
	if r := e.response(2); r["result"] != "nested" {
		t.Fatalf("hover in the nested worktree = %v", r)
	}
	e.request(3, "textDocument/hover", at(own))
	answerNext(t, primary, "textDocument/hover", "own")
	if r := e.response(3); r["result"] != "own" {
		t.Fatalf("hover in the project = %v", r)
	}
}

// TestProxyPassesASingleProjectSessionAsItIs: in the normal case, an editor
// opened in one project, every message reaches the one daemon byte for byte,
// files outside the project included, and the proxy dials nothing more.
// (The test compares decoded messages; the proxy forwards these frames
// without a rewrite.)
func TestProxyPassesASingleProjectSessionAsItIs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"mix.exs": "", "deps/dep/mix.exs": ""})
	h, primary := newRouteHarness(t, root, false)
	e := h.editor
	file := fileURI(filepath.Join(root, "lib", "a.ex"))
	dep := fileURI(filepath.Join(root, "deps", "dep", "lib", "dep.ex"))
	outside := "file:///usr/lib/elixir/lib/enum.ex"
	bodies := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"rootUri":"` + fileURI(root) + `","capabilities":{}}}`,
		`{"jsonrpc":"2.0","method":"initialized","params":{}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"` + file + `","languageId":"elixir","version":1,"text":"x"}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"` + file + `","version":2},"contentChanges":[{"text":"y"}]}}`,
		`{"jsonrpc":"2.0","id":2,"method":"textDocument/hover","params":{"textDocument":{"uri":"` + file + `"},"position":{"line":0,"character":0}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"textDocument/definition","params":{"textDocument":{"uri":"` + outside + `"},"position":{"line":0,"character":0}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"textDocument/hover","params":{"textDocument":{"uri":"` + dep + `"},"position":{"line":0,"character":0}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"workspace/symbol","params":{"query":"x"}}`,
		`{"jsonrpc":"2.0","method":"workspace/didChangeWatchedFiles","params":{"changes":[{"uri":"` + file + `","type":2},{"uri":"` + outside + `","type":2}]}}`,
		`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":5}}`,
		`{"jsonrpc":"2.0","id":6,"method":"shutdown"}`,
	}
	for _, body := range bodies {
		e.wmu.Lock()
		_, err := fmt.Fprintf(e.w, "Content-Length: %d\r\n\r\n%s", len(body), body)
		e.wmu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range bodies {
		m := expectAny(t, primary)
		raw, _ := json.Marshal(m)
		var wantMsg map[string]any
		_ = json.Unmarshal([]byte(body), &wantMsg)
		wantRaw, _ := json.Marshal(wantMsg)
		if string(raw) != string(wantRaw) {
			t.Fatalf("the daemon got %s, want %s", raw, wantRaw)
		}
		if m["method"] == "initialize" {
			primary.send(map[string]any{"id": 1, "result": map[string]any{}})
		}
	}
	if dials := h.dials(); len(dials) != 0 {
		t.Fatalf("the proxy dialed %v in a single-project session", dials)
	}
}

// TestProxyCancelsAFanOutRequest: the editor cancels a workspace/symbol
// that went to two daemons. It gets RequestCancelled at once, each daemon a
// cancel for its part, and late answers are dropped.
func TestProxyCancelsAFanOutRequest(t *testing.T) {
	work, _, featureA, featureB := containerFixture(t)
	h, _ := newRouteHarness(t, work, true)
	e := h.editor
	e.request(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	e.response(1)
	fileA := filepath.Join(featureA, "lib", "a.ex")
	fileB := filepath.Join(featureB, "lib", "b.ex")
	openDoc(e, fileA, "a")
	da := h.daemon(featureA)
	connectUpstream(t, da, featureA, fileURI(fileA))
	openDoc(e, fileB, "b")
	db := h.daemon(featureB)
	connectUpstream(t, db, featureB, fileURI(fileB))

	e.request(9, "workspace/symbol", map[string]any{"query": "x"})
	partA := expectMethod(t, da, "workspace/symbol")
	partB := expectMethod(t, db, "workspace/symbol")
	e.notify("$/cancelRequest", map[string]any{"id": 9})
	r := e.response(9)
	if errObj, _ := r["error"].(map[string]any); fmt.Sprint(errObj["code"]) != fmt.Sprint(lspRequestCancelled) {
		t.Fatalf("canceled workspace/symbol = %v", r)
	}
	for d, part := range map[*lspEditor]map[string]any{da: partA, db: partB} {
		c := expectMethod(t, d, "$/cancelRequest")
		if fmt.Sprint(c["params"].(map[string]any)["id"]) != fmt.Sprint(part["id"]) {
			t.Fatalf("cancel %v, want the part id %v", c, part["id"])
		}
		d.send(map[string]any{"id": part["id"], "result": []any{}})
	}
	e.request(10, "textDocument/hover", at(fileA))
	answerNext(t, da, "textDocument/hover", "a")
	for {
		m := expectAny(t, e)
		if id, _ := idAsInt(m["id"]); id == 9 {
			t.Fatalf("the editor got a second answer to the canceled request: %v", m)
		}
		if id, _ := idAsInt(m["id"]); id == 10 {
			break
		}
	}
}

// TestProxyAnswersAContainerWithNoProjectYet: before any file of a project
// is open, a container has no daemon; requests get empty answers and
// shutdown and exit end the session.
func TestProxyAnswersAContainerWithNoProjectYet(t *testing.T) {
	work, _, _, _ := containerFixture(t)
	h, _ := newRouteHarness(t, work, true)
	e := h.editor
	e.request(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	e.response(1)
	e.request(2, "workspace/symbol", map[string]any{"query": "x"})
	if r := e.response(2); fmt.Sprint(r["result"]) != "[]" {
		t.Fatalf("workspace/symbol with no project = %v, want []", r)
	}
	// A file of the container itself has no project of its own.
	e.request(3, "textDocument/hover", at(filepath.Join(work, "notes.ex")))
	if r := e.response(3); r["result"] != nil || r["error"] != nil {
		t.Fatalf("hover with no project = %v, want null", r)
	}
	e.request(4, "shutdown", nil)
	e.response(4)
	e.notify("exit", nil)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("proxy exit = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy did not end")
	}
	if dials := h.dials(); len(dials) != 0 {
		t.Fatalf("the proxy dialed %v", dials)
	}
}

// TestProxyGoesOnWhenAnUpstreamCannotStart: the daemon of one project cannot
// start. The editor is told, the requests of that project get errors, and
// the session goes on for the other projects.
func TestProxyGoesOnWhenAnUpstreamCannotStart(t *testing.T) {
	previous := reconnectDelays
	reconnectDelays = []time.Duration{0}
	previousRetry := failedRetryDelay
	failedRetryDelay = 300 * time.Millisecond
	t.Cleanup(func() { reconnectDelays, failedRetryDelay = previous, previousRetry })
	work, _, featureA, featureB := containerFixture(t)
	editorToProxy, proxyIn := io.Pipe()
	proxyOut, proxyToEditor := io.Pipe()
	done := make(chan error, 1)
	sides := make(chan *lspEditor, 4)
	var failA atomic.Bool
	failA.Store(true)
	proxy := newLSPProxy(context.Background(), work, true, editorToProxy, proxyToEditor, func(_ context.Context, root string) (net.Conn, *bufio.Reader, error) {
		if root == featureA && failA.Load() {
			return nil, nil, errors.New("the index cannot be opened")
		}
		proxySide, daemonSide := net.Pipe()
		sides <- newLSPEditor(t, daemonSide, daemonSide).pump()
		return proxySide, bufio.NewReader(proxySide), nil
	})
	go func() { done <- proxy.run(nil, nil); _ = proxyToEditor.Close() }()
	t.Cleanup(func() { _ = proxyIn.Close() })
	e := newLSPEditor(t, proxyIn, proxyOut).pump()
	e.request(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	e.response(1)
	fileA := filepath.Join(featureA, "lib", "a.ex")
	fileB := filepath.Join(featureB, "lib", "b.ex")
	openDoc(e, fileA, "a")
	e.request(2, "textDocument/hover", at(fileA))
	if r := e.response(2); r["error"] == nil {
		t.Fatalf("hover in a project with no daemon = %v, want an error", r)
	}
	if shown := e.shown(); len(shown) == 0 || !strings.Contains(shown[0], "cannot be opened") {
		t.Fatalf("the editor was told %q", shown)
	}
	openDoc(e, fileB, "b")
	db := <-sides
	connectUpstream(t, db, featureB, fileURI(fileB))
	e.request(3, "textDocument/hover", at(fileB))
	answerNext(t, db, "textDocument/hover", "b")
	if r := e.response(3); r["result"] != "b" {
		t.Fatalf("hover in the other project = %v", r)
	}
	select {
	case err := <-done:
		t.Fatalf("the proxy ended: %v", err)
	default:
	}

	// The daemon can start now. After the wait, the next message for the
	// project starts a new attempt; the session of that project is replayed,
	// and the editor is told once that it works again.
	failA.Store(false)
	time.Sleep(failedRetryDelay + 100*time.Millisecond)
	e.request(4, "textDocument/hover", at(fileA))
	da := <-sides
	connectUpstream(t, da, featureA, fileURI(fileA))
	answerNext(t, da, "textDocument/hover", "a")
	if r := e.response(4); r["result"] != "a" {
		t.Fatalf("hover after the retry = %v", r)
	}
	again := func() int {
		n := 0
		for _, text := range e.shown() {
			if strings.Contains(text, "reaches the workspace daemon") {
				n++
			}
		}
		return n
	}
	// The notice follows the replay, so it can come after the answer.
	for deadline := time.Now().Add(5 * time.Second); again() == 0 && time.Now().Before(deadline); {
		if m, err := e.next(); err == nil {
			e.handle(m)
		}
	}
	if again() != 1 {
		t.Fatalf("the editor was told %q, want one message that the project works again", e.shown())
	}
}

// TestProxyServesWorktreesOfAContainerFromTheirOwnDaemons runs real daemons:
// two worktrees define the same function in different files, and a
// definition is answered from the worktree of the file that asks.
func TestProxyServesWorktreesOfAContainerFromTheirOwnDaemons(t *testing.T) {
	quietEnv(t)
	requireSocketSupport(t)
	work := t.TempDir()
	shop := filepath.Join(work, "shop")
	if err := os.MkdirAll(filepath.Join(work, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shop, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	caller := "defmodule Shop.Caller do\n  def run do\n    Shop.Accounts.get_user(1)\n  end\nend\n"
	writeFiles(t, shop, map[string]string{"mix.exs": "defmodule Shop.MixProject do\nend\n"})
	worktrees := map[string]string{"feature-a": "accounts_a.ex", "feature-b": "accounts_b.ex"}
	for name, file := range worktrees {
		dir := filepath.Join(work, "shop-worktrees", name)
		linkWorktree(t, shop, dir)
		writeFiles(t, dir, map[string]string{
			"mix.exs":            "defmodule Shop.MixProject do\nend\n",
			"lib/shop/" + file:   "defmodule Shop.Accounts do\n  def get_user(id), do: id\nend\n",
			"lib/shop/caller.ex": caller,
		})
	}
	origSpawn := spawnDaemon
	spawnDaemon = func(root string) (<-chan error, error) { runDaemonInProcess(t, root); return nil, nil }
	t.Cleanup(func() { spawnDaemon = origSpawn })

	editor, _, done := proxyOnRoot(t, work)
	editor.request(1, "initialize", map[string]any{"processId": nil, "rootUri": fileURI(work), "capabilities": map[string]any{}})
	if r := editor.response(1); r["error"] != nil {
		t.Fatalf("initialize = %v", r)
	}
	editor.notify("initialized", map[string]any{})
	id := 10
	for name, file := range worktrees {
		path := filepath.Join(work, "shop-worktrees", name, "lib", "shop", "caller.ex")
		openDoc(editor, path, caller)
		params := map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}, "position": map[string]any{"line": 2, "character": 20}}
		deadline := time.Now().Add(20 * time.Second)
		for {
			id++
			editor.request(id, "textDocument/definition", params)
			res := fmt.Sprint(editor.response(id)["result"])
			if strings.Contains(res, filepath.Join(name, "lib", "shop", file)) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("definition in %s = %s, want %s of that worktree", name, res, file)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	editor.request(99, "shutdown", nil)
	editor.response(99)
	editor.notify("exit", nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("proxy exit = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("proxy did not exit")
	}
}

// TestProxyFindsTheProjectOfEachFile covers the project rule: below the
// start root, only a nested clone (a .git directory) or a linked worktree is
// a project of its own. A Dexter index alone, a submodule, a git dependency,
// and a stale index of the container are not; a file outside the start root
// has no project of its own.
func TestProxyFindsTheProjectOfEachFile(t *testing.T) {
	work, shop, featureA, _ := containerFixture(t)
	writeFiles(t, work, map[string]string{
		".dexter/dexter.db":                "x", // a stale index of the container
		"shop/apps/shop/.dexter/dexter.db": "x", // a leftover index in a monorepo app
		"shop/libs/vendored/.git":          "gitdir: ../../.git/modules/vendored\n",
	})
	if err := os.MkdirAll(filepath.Join(shop, "deps", "git_dep", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(shop, "tools", "nested_clone", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	container := newLSPProxy(context.Background(), work, true, nil, io.Discard, nil)
	cases := map[string]string{
		filepath.Join(shop, "apps", "shop", "lib"):            shop,
		filepath.Join(shop, "libs", "vendored", "lib"):        shop,
		filepath.Join(shop, "deps", "git_dep", "lib"):         shop,
		filepath.Join(shop, "tools", "nested_clone", "lib"):   filepath.Join(shop, "tools", "nested_clone"),
		filepath.Join(featureA, "apps", "shop", "lib", "x"):   featureA,
		filepath.Join(work, "web", "src"):                     filepath.Join(work, "web"),
		filepath.Join(work, "scripts"):                        "",
		filepath.Join(filepath.Dir(work), "elsewhere", "lib"): "",
	}
	for dir, want := range cases {
		if got := container.projectRoot(dir); got != want {
			t.Errorf("projectRoot(%s) = %q, want %q", dir, got, want)
		}
	}

	// A project as the start root: its own files, a leftover index in one
	// of its apps, a submodule, and its git dependencies are its own; a
	// nested clone and a nested linked worktree are not.
	nested := filepath.Join(shop, ".agents", "worktrees", "x")
	linkWorktree(t, shop, nested)
	project := newLSPProxy(context.Background(), shop, false, nil, io.Discard, nil)
	for dir, want := range map[string]string{
		filepath.Join(shop, "apps", "shop", "lib"):          "",
		filepath.Join(shop, "libs", "vendored", "lib"):      "",
		filepath.Join(shop, "deps", "git_dep", "lib"):       "",
		filepath.Join(shop, "tools", "nested_clone", "lib"): filepath.Join(shop, "tools", "nested_clone"),
		filepath.Join(nested, "lib"):                        nested,
		filepath.Join(work, "web"):                          "",
	} {
		if got := project.projectRoot(dir); got != want {
			t.Errorf("projectRoot(%s) from the project = %q, want %q", dir, got, want)
		}
	}
}

// TestProxyGivesOneUpstreamToTwoLetterCasesOfAProject: on a file system that
// ignores letter case (the macOS default), two URIs that differ only in case
// name one project. They share one upstream, as they share one daemon.
func TestProxyGivesOneUpstreamToTwoLetterCasesOfAProject(t *testing.T) {
	work, _, featureA, _ := containerFixture(t)
	upper := filepath.Join(filepath.Dir(featureA), "Feature-A")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("the file system is case-sensitive")
	}
	h, _ := newRouteHarness(t, work, true)
	e := h.editor
	e.request(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	e.response(1)
	fileA := filepath.Join(featureA, "lib", "a.ex")
	openDoc(e, fileA, "a")
	da := h.daemon(featureA)
	connectUpstream(t, da, featureA, fileURI(fileA))
	other := filepath.Join(upper, "lib", "b.ex")
	openDoc(e, other, "b")
	if m := expectMethod(t, da, "textDocument/didOpen"); docURI(m) != fileURI(other) {
		t.Fatalf("the daemon of feature-a got %v", m)
	}
	if dials := h.dials(); len(dials) != 1 {
		t.Fatalf("the proxy dialed %v, want one daemon for both spellings", dials)
	}
}

// trySend is send for a daemon side whose stream the proxy may have closed
// already.
func trySend(side *lspEditor, message map[string]any) {
	message["jsonrpc"] = "2.0"
	body, _ := json.Marshal(message)
	_, _ = fmt.Fprintf(side.w, "Content-Length: %d\r\n\r\n%s", len(body), body)
}

// expectEOF waits until side reads the end of its stream: the proxy closed
// the daemon's input.
func expectEOF(t *testing.T, side *lspEditor) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := side.next(); err != nil {
			if strings.Contains(err.Error(), "no message within") {
				break
			}
			return
		}
	}
	t.Fatal("the daemon did not see the end of its input")
}

func expectProxyEnd(t *testing.T, done chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy did not end after the editor's input ended")
	}
}

// TestProxyEndsWhenTheEditorGoesDuringAReconnect: the editor's input ends
// (without exit) while a new daemon of the start root has not answered the
// replayed initialize. The new daemon must see the end of its input, and the
// proxy must end; before, the connection that the replay made current was
// never closed, and the proxy waited for ever.
func TestProxyEndsWhenTheEditorGoesDuringAReconnect(t *testing.T) {
	h, daemon := initializedHarness(t)
	next := h.nextDaemon()
	_ = daemon.w.(net.Conn).Close()
	init := expectMethod(t, next, "initialize")
	_ = h.in.Close()
	time.Sleep(50 * time.Millisecond)
	trySend(next, map[string]any{"id": init["id"], "result": map[string]any{}})
	expectEOF(t, next)
	_ = next.w.(net.Conn).Close()
	expectProxyEnd(t, h.done)
}

// TestProxyEndsWhenTheEditorGoesDuringAFirstConnect is the same for the
// first connect of the upstream of another project, which often waits for a
// daemon to start.
func TestProxyEndsWhenTheEditorGoesDuringAFirstConnect(t *testing.T) {
	work, _, featureA, _ := containerFixture(t)
	h, _ := newRouteHarness(t, work, true)
	e := h.editor
	e.request(1, "initialize", map[string]any{"capabilities": map[string]any{}})
	e.response(1)
	fileA := filepath.Join(featureA, "lib", "a.ex")
	openDoc(e, fileA, "a")
	da := h.daemon(featureA)
	init := expectMethod(t, da, "initialize")
	_ = h.in.Close()
	time.Sleep(50 * time.Millisecond)
	trySend(da, map[string]any{"id": init["id"], "result": map[string]any{}})
	expectEOF(t, da)
	_ = da.w.(net.Conn).Close()
	expectProxyEnd(t, h.done)
}

// TestProxyCachedRouteDropsAFailedFallback: the last route is kept for the
// next message about the same file. For a file with no project of its own in
// a container, it must give what route gives: not an upstream that failed
// since.
func TestProxyCachedRouteDropsAFailedFallback(t *testing.T) {
	work, _, featureA, _ := containerFixture(t)
	p := newLSPProxy(context.Background(), work, true, nil, io.Discard, nil)
	u := p.newUpstream(featureA, 1)
	p.upstreams[resolveRootKey(featureA)] = u
	p.upstreamList = []*upstream{u}
	p.fallbackUp = u
	raw := []byte(`"` + fileURI(filepath.Join(work, "notes.ex")) + `"`)
	if got := p.upstreamFor(raw, "textDocument/hover"); got != u {
		t.Fatalf("fallback = %v, want the upstream of the last file", got)
	}
	u.failed = true
	u.retryAt = time.Now().Add(time.Hour)
	if got := p.upstreamFor(raw, "textDocument/hover"); got != nil {
		t.Fatalf("cached fallback = %v after its upstream failed, want none, as route gives", got)
	}
}

// A repository of a container that is a symlink gets the files of both of
// its paths: through the link, and resolved (outside the container). A file
// outside the container that is in no such repository has no project.
func TestProxyRoutesTheResolvedPathOfALinkedRepository(t *testing.T) {
	work := t.TempDir()
	elsewhere := t.TempDir()
	writeFiles(t, work, map[string]string{"README.md": "x"})
	writeFiles(t, elsewhere, map[string]string{"shop/mix.exs": "x", "shop/lib/a.ex": "x", "other/lib/b.ex": "x"})
	for _, d := range []string{filepath.Join(work, ".git"), filepath.Join(elsewhere, "shop", ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(work, "shop")
	if err := os.Symlink(filepath.Join(elsewhere, "shop"), link); err != nil {
		t.Fatal(err)
	}
	if !store.IsProjectContainer(work) {
		t.Fatal("a folder with a symlinked clone is not a container")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(elsewhere, "shop"))
	if err != nil {
		t.Fatal(err)
	}
	p := newLSPProxy(context.Background(), work, true, nil, io.Discard, nil)
	if got := p.projectRoot(filepath.Join(link, "lib")); got != link {
		t.Errorf("projectRoot(link/lib) = %q, want %q", got, link)
	}
	if got := p.projectRoot(filepath.Join(resolved, "lib")); got != link {
		t.Errorf("projectRoot(resolved/lib) = %q, want %q", got, link)
	}
	otherResolved, _ := filepath.EvalSymlinks(filepath.Join(elsewhere, "other"))
	if got := p.projectRoot(filepath.Join(otherResolved, "lib")); got != "" {
		t.Errorf("projectRoot(a folder outside the container) = %q, want \"\"", got)
	}
}
