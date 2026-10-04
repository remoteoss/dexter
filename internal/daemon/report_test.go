package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
	"github.com/remoteoss/dexter/internal/workspace"
)

// heldServer is pipeServer for a workspace held before its first
// reconciliation until release runs.
func heldServer(t *testing.T, root string) (*server, Endpoint, func()) {
	t.Helper()
	ownership, endpoint, err := AcquireOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			close(hold)
		}
	}
	rt, err := workspace.OpenWithOptions(root, workspace.Options{NoWatch: true, BeforeInitialReconcile: func() { <-hold }})
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
	t.Cleanup(func() {
		release()
		s.cancelCtx()
		_ = rt.Close()
		_ = ownership.Release()
	})
	return s, endpoint, release
}

// editor is one LSP session over a pipe that keeps every message it reads.
type editor struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
	seen   []map[string]any
}

func attachEditor(t *testing.T, s *server, endpoint Endpoint, root string) *editor {
	t.Helper()
	conn, reader, res := pipeDial(t, s, endpoint, kindLSP, "")
	if !res.OK {
		t.Fatalf("LSP handshake refused: %s", res.Error)
	}
	e := &editor{t: t, conn: conn, reader: reader}
	writeLSP(t, conn, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"processId": nil, "rootUri": "file://" + root, "capabilities": map[string]any{}},
	})
	e.until("initialize response", func(m map[string]any) bool {
		id, ok := idAsInt(m["id"])
		return ok && id == 1 && m["method"] == nil
	})
	writeLSP(t, conn, map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
	return e
}

// until reads messages until match accepts one. Requests from the server are
// answered with null, as an editor would.
func (e *editor) until(what string, match func(map[string]any) bool) map[string]any {
	e.t.Helper()
	for _, m := range e.seen {
		if match(m) {
			return m
		}
	}
	_ = e.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	defer func() { _ = e.conn.SetReadDeadline(time.Time{}) }()
	for {
		m := readLSP(e.t, e.reader)
		e.seen = append(e.seen, m)
		method, _ := m["method"].(string)
		if rawID, hasID := m["id"]; hasID && method != "" {
			writeLSP(e.t, e.conn, map[string]any{"jsonrpc": "2.0", "id": rawID, "result": nil})
		}
		if match(m) {
			return m
		}
		if len(e.seen) > 200 {
			e.t.Fatalf("no %s in %d messages", what, len(e.seen))
		}
	}
}

func (e *editor) message(typ float64, text string) string {
	e.t.Helper()
	m := e.until(fmt.Sprintf("showMessage containing %q", text), func(m map[string]any) bool {
		if m["method"] != "window/showMessage" {
			return false
		}
		params, _ := m["params"].(map[string]any)
		message, _ := params["message"].(string)
		return params["type"] == typ && strings.Contains(message, text)
	})
	params := m["params"].(map[string]any)
	return params["message"].(string)
}

// One daemon serves several editors. An editor that attaches while a rebuild
// is in progress must learn about it, like the editor that was there first,
// and both must learn when it ends.
func TestEditorAttachingDuringRebuildIsTold(t *testing.T) {
	quietEnv(t)
	root := t.TempDir()
	path := writeModule(t, root, "lib/one.ex", "SharedLib.One")
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.IndexFileWithRefs(path, defs, refs); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIndexVersion(version.IndexVersion + 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	s, endpoint, release := heldServer(t, root)
	first := attachEditor(t, s, endpoint, root)
	first.message(2, "the index was written by a newer Dexter build")
	second := attachEditor(t, s, endpoint, root)
	second.message(2, "the index was written by a newer Dexter build")

	// The CLI is told too, with the same text.
	control := pipeClient(t, s, endpoint)
	var result LookupResult
	if err := control.Call(context.Background(), MethodLookup, LookupParams{Module: "SharedLib.One"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Ready || len(result.Notes) != 1 || result.Notes[0].Severity != "warning" ||
		!strings.Contains(result.Notes[0].Message, "newer Dexter build") {
		t.Fatalf("lookup during the rebuild: ready=%v notes=%+v", result.Ready, result.Notes)
	}

	release()
	for _, e := range []*editor{first, second} {
		e.message(3, "the index rebuild is complete")
	}
	if err := control.Call(context.Background(), MethodLookup, LookupParams{Module: "SharedLib.One", WaitReadyMs: 30_000}, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Notes) != 0 {
		t.Fatalf("notes after the rebuild: %+v", result.Notes)
	}
}

// lspRequest frames one request the way an editor writes it.
func lspRequest(t *testing.T, id int, method string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writeLSP(t, &buf, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": map[string]any{}})
	return buf.Bytes()
}

// startupFailureOutput returns the messages a failed proxy wrote.
func startupFailureOutput(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	r := bufio.NewReader(bytes.NewReader(out))
	var messages []map[string]any
	for {
		if _, err := r.Peek(1); errors.Is(err, io.EOF) {
			return messages
		}
		messages = append(messages, readLSP(t, r))
	}
}

func assertStartupFailureAnswer(t *testing.T, out []byte, wantText string) {
	t.Helper()
	messages := startupFailureOutput(t, out)
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want showMessage then the initialize error: %v", len(messages), messages)
	}
	show := messages[0]
	params, _ := show["params"].(map[string]any)
	if show["method"] != "window/showMessage" || params["type"] != float64(1) || !strings.Contains(params["message"].(string), wantText) {
		t.Fatalf("first message is not an error showMessage with %q: %v", wantText, show)
	}
	answer := messages[1]
	if id, ok := idAsInt(answer["id"]); !ok || id != 1 {
		t.Fatalf("answer is not for the initialize request: %v", answer)
	}
	rpcErr, _ := answer["error"].(map[string]any)
	if rpcErr["code"] != float64(lspInternalError) || rpcErr["message"] != params["message"] {
		t.Fatalf("initialize error = %v, want code %d and the shown message", rpcErr, lspInternalError)
	}
}

// When the proxy cannot reach a daemon, the editor must show why. Exiting with
// a line on stderr leaves the user with a language server that does nothing.
func TestProxyAnswersInitializeWhenTheWorkspaceIsHeld(t *testing.T) {
	quietEnv(t)
	root := t.TempDir()
	ownership, _, err := AcquireOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ownership.Release() }()

	var out bytes.Buffer
	in := bytes.NewReader(lspRequest(t, 1, "initialize"))
	err = ProxyLSP(context.Background(), root, in, &out)
	var startup *LSPStartupError
	if !errors.As(err, &startup) {
		t.Fatalf("ProxyLSP error = %v, want *LSPStartupError", err)
	}
	assertStartupFailureAnswer(t, out.Bytes(), "not serving its daemon socket")
	if !strings.Contains(startup.Message, "Wait until it finishes, then restart this editor") {
		t.Errorf("message does not say what to do: %q", startup.Message)
	}
}

func TestProxyAnswersInitializeWhenTheDaemonCannotStart(t *testing.T) {
	quietEnv(t)
	root := t.TempDir()
	origSpawn := spawnDaemon
	spawnDaemon = func(string) error { return errors.New("start workspace daemon: exec format error") }
	t.Cleanup(func() { spawnDaemon = origSpawn })

	var out bytes.Buffer
	// An editor can send a request of its own first; it gets the error too.
	in := io.MultiReader(bytes.NewReader(lspRequest(t, 7, "workspace/symbol")), bytes.NewReader(lspRequest(t, 1, "initialize")))
	err := ProxyLSP(context.Background(), root, in, &out)
	if err == nil {
		t.Fatal("ProxyLSP succeeded without a daemon")
	}
	messages := startupFailureOutput(t, out.Bytes())
	if len(messages) != 3 {
		t.Fatalf("got %d messages, want 3: %v", len(messages), messages)
	}
	if id, _ := idAsInt(messages[0]["id"]); id != 7 || messages[0]["error"] == nil {
		t.Fatalf("the early request got no error: %v", messages[0])
	}
	var rest bytes.Buffer
	for _, m := range messages[1:] {
		writeLSP(t, &rest, m)
	}
	assertStartupFailureAnswer(t, rest.Bytes(), "exec format error")
}

func TestStartupFailureEndsWithoutInitialize(t *testing.T) {
	var out bytes.Buffer
	if err := serveStartupFailure(bytes.NewReader(nil), &out, "x", time.Second); err != nil {
		t.Fatalf("end of input: %v", err)
	}
	exit := bytes.NewReader(append(lspRequest(t, 2, "shutdown"), []byte("Content-Length: 33\r\n\r\n{\"jsonrpc\":\"2.0\",\"method\":\"exit\"}")...))
	if err := serveStartupFailure(exit, &out, "x", time.Second); err != nil {
		t.Fatalf("exit: %v", err)
	}
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	if err := serveStartupFailure(reader, &out, "x", 20*time.Millisecond); err == nil {
		t.Fatal("a silent editor did not time out")
	}
}

func TestExplainStartupFailureSaysWhatToDo(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "daemon newer",
			err:  &IncompatibleDaemonError{DaemonContract: ContractVersion + 1, DaemonPID: 42},
			want: []string{"this editor started an older Dexter build", "Restart this editor so that it starts the current dexter binary"},
		},
		{
			name: "daemon older and stuck",
			err:  errors.Join(&IncompatibleDaemonError{DaemonContract: ContractVersion - 1, DaemonPID: 42, ClientNewer: true}, errors.New("daemon pid 42 did not exit")),
			want: []string{"an older Dexter daemon (pid 42", "could not be replaced", "dexter stop --force"},
		},
		{
			name: "root spelling",
			err:  &RootMismatchError{Requested: "/link/app", Daemon: "/real/app"},
			want: []string{"already serves this project as /real/app", "Open the project as /real/app", "dexter stop --force"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExplainStartupFailure("/real/app", tc.err)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("message %q does not contain %q", got, want)
				}
			}
		})
	}
}

// caseInsensitiveRoot makes a directory with upper-case letters and returns
// its spelling and a lower-case spelling. It skips the test where the file
// system tells the two apart.
func caseInsensitiveRoot(t *testing.T) (stored, other string) {
	t.Helper()
	stored = filepath.Join(t.TempDir(), "CaseApp")
	if err := os.Mkdir(stored, 0o755); err != nil {
		t.Fatal(err)
	}
	other = filepath.Join(filepath.Dir(stored), "caseapp")
	if _, err := os.Stat(other); err != nil {
		t.Skip("the file system is case-sensitive")
	}
	return stored, other
}

// On a case-insensitive file system, two spellings of one directory are one
// workspace. Two identities would give two locks and two daemons that build
// one index at the same time.
func TestCaseSpellingsShareOneWorkspace(t *testing.T) {
	stored, other := caseInsensitiveRoot(t)
	a, err := ResolveEndpoint(stored)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ResolveEndpoint(other)
	if err != nil {
		t.Fatal(err)
	}
	if a.Identity != b.Identity || a.Lock != b.Lock || a.Socket != b.Socket {
		t.Fatalf("one directory has two identities:\n  %+v\n  %+v", a, b)
	}
	if b.Root != other {
		t.Errorf("Root = %q, want the caller's spelling %q", b.Root, other)
	}

	ownership, _, err := AcquireOwnership(stored)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ownership.Release() }()
	if _, _, err := AcquireOwnership(other); !errors.Is(err, ErrWorkspaceOwned) {
		t.Fatalf("the other spelling took a second lock: %v", err)
	}
}

// The second spelling reaches the daemon of the first, which refuses it with
// the root mismatch that the editor then shows.
func TestCaseSpellingGetsRootMismatch(t *testing.T) {
	quietEnv(t)
	stored, other := caseInsensitiveRoot(t)
	s, endpoint := pipeServer(t, stored)
	otherEndpoint, err := ResolveEndpoint(other)
	if err != nil {
		t.Fatal(err)
	}
	_, _, res := pipeDial(t, s, otherEndpoint, kindLSP, "")
	if res.OK || res.Root != endpoint.Root {
		t.Fatalf("handshake = %+v, want a refusal that names %s", res, endpoint.Root)
	}
}
