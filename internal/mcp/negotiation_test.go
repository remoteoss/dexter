package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeBackend stands in for a workspace daemon connection. It answers every
// tool call with the root it was opened for.
type fakeBackend struct {
	root   string
	mu     sync.Mutex
	closed bool
}

func (b *fakeBackend) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return "root=" + b.root, nil
}

func (b *fakeBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func (b *fakeBackend) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// negotiationEnv is a negotiating Frontend over fake backends, plus helpers to
// connect clients that advertise chosen roots.
type negotiationEnv struct {
	t        *testing.T
	f        *Frontend
	fallback string

	mu       sync.Mutex
	backends []*fakeBackend
}

func setupNegotiation(t *testing.T) *negotiationEnv {
	t.Helper()
	return setupNegotiationWith(t, Config{Root: t.TempDir()})
}

func setupNegotiationWith(t *testing.T, cfg Config) *negotiationEnv {
	t.Helper()
	e := &negotiationEnv{t: t, fallback: cfg.Root}
	cfg.Connect = func(root string) Backend {
		b := &fakeBackend{root: root}
		e.mu.Lock()
		e.backends = append(e.backends, b)
		e.mu.Unlock()
		return b
	}
	e.f = NewFrontend(cfg)
	t.Cleanup(e.f.Close)
	return e
}

func (e *negotiationEnv) backendsFor(root string) []*fakeBackend {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []*fakeBackend
	for _, b := range e.backends {
		if b.root == root {
			out = append(out, b)
		}
	}
	return out
}

func (e *negotiationEnv) connCount() int {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	return len(e.f.conns)
}

// connect wires a new client session to the negotiating server. Roots are
// added before connecting so they are visible from the first roots/list.
func (e *negotiationEnv) connect(opts *mcp.ClientOptions, rootURIs ...string) (*mcp.ClientSession, *mcp.Client) {
	e.t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := NewServer(e.f).Connect(ctx, serverTransport, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, opts)
	for _, u := range rootURIs {
		client.AddRoots(&mcp.Root{URI: u})
	}
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = cs.Close() })
	return cs, client
}

// projectDir creates a repository directory and returns its path and file URI.
func projectDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	return dir, fileURI(dir)
}

func writeSource(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func toolText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error(), false
	}
	return resultText(res), !res.IsError
}

func mustTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	out, ok := toolText(t, cs, name, args)
	if !ok {
		t.Fatalf("CallTool(%s) failed: %s", name, out)
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestFileURIToPath(t *testing.T) {
	cases := []struct {
		uri  string
		want string // "" means an error is expected
	}{
		{"file:///a/b", "/a/b"},
		{"file:///a/b/", "/a/b"}, // trailing slash must not key a second workspace
		{"file:///a/./b/../b", "/a/b"},
		{"file://localhost/a/b", "/a/b"},
		{"file:///a/my%20project", "/a/my project"},
		{"file://otherhost/a", ""},
		{"file://a", ""}, // host form, no path
		{"file:relative", ""},
	}
	for _, tc := range cases {
		got, err := fileURIToPath(tc.uri)
		if tc.want == "" {
			if err == nil {
				t.Errorf("fileURIToPath(%q) = %q, want error", tc.uri, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("fileURIToPath(%q) = %q, %v; want %q", tc.uri, got, err, tc.want)
		}
	}
}

func TestNegotiation_BindsClientRoot(t *testing.T) {
	e := setupNegotiation(t)
	root, uri := projectDir(t)
	cs, _ := e.connect(nil, uri)

	wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "x"}), "root="+root)
	if len(e.backendsFor(e.fallback)) != 0 {
		t.Error("connected to the fallback root despite a negotiated root")
	}
}

func TestNegotiation_FallsBackWithoutUsableRoots(t *testing.T) {
	cases := []struct {
		name string
		opts *mcp.ClientOptions
		uris []string
	}{
		// A default go-sdk client advertises roots with an empty list; this is
		// what most clients look like, not an edge case.
		{name: "empty roots list", opts: nil},
		{name: "roots capability off", opts: &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}}},
		{name: "non-file roots only", opts: nil, uris: []string{"https://example.com/project"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := setupNegotiation(t)
			cs, _ := e.connect(tc.opts, tc.uris...)
			wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "x"}), "root="+e.fallback)
		})
	}
}

// A launch directory that is not a project is not indexed: a session without
// roots gets the reason, and a session with roots still works.
func TestNegotiation_FallbackRefused(t *testing.T) {
	e := setupNegotiationWith(t, Config{Root: t.TempDir(), FallbackErr: errors.New("refusing to use the launch directory")})
	noRoots, _ := e.connect(&mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	out, ok := toolText(t, noRoots, "dexter_search", map[string]any{"query": "x"})
	if ok || !strings.Contains(out, "refusing to use the launch directory") {
		t.Fatalf("session without roots was not refused: %s", out)
	}
	if len(e.backendsFor(e.fallback)) != 0 {
		t.Error("connected to a refused fallback root")
	}

	root, uri := projectDir(t)
	withRoots, _ := e.connect(nil, uri)
	wantContains(t, mustTool(t, withRoots, "dexter_search", map[string]any{"query": "x"}), "root="+root)
}

// A root inside a repository resolves upward to the repository: an existing
// index or .git wins, and a nested mix.exs does not stop the walk.
func TestNegotiation_ResolvesRootUpward(t *testing.T) {
	e := setupNegotiation(t)
	repo, _ := projectDir(t)
	writeSource(t, repo, "apps/web/mix.exs", "defmodule Web.MixProject do\nend\n")
	cs, _ := e.connect(nil, fileURI(filepath.Join(repo, "apps", "web")))

	wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "x"}), "root="+repo)
}

// The resolver that cmd passes decides the root, so that the MCP frontend
// attaches to the same daemon as the CLI and the editor.
func TestNegotiation_UsesConfiguredResolver(t *testing.T) {
	resolved := t.TempDir()
	var asked string
	e := setupNegotiationWith(t, Config{Root: t.TempDir(), ResolveRoot: func(dir string) (string, error) {
		asked = dir
		return resolved, nil
	}})
	dir, uri := projectDir(t)
	cs, _ := e.connect(nil, uri)

	wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "x"}), "root="+resolved)
	if asked != dir {
		t.Errorf("resolver got %q, want the client root %q", asked, dir)
	}
}

func TestNegotiation_BadRootIsRetryable(t *testing.T) {
	e := setupNegotiation(t)
	badURI := "file:///nonexistent/dexter-negotiation-test"
	cs, client := e.connect(nil, badURI)

	out, ok := toolText(t, cs, "dexter_search", map[string]any{"query": "x"})
	if ok {
		t.Fatalf("tool call succeeded against a nonexistent root: %s", out)
	}
	if !strings.Contains(out, "not a directory") {
		t.Errorf("error does not name the problem: %s", out)
	}

	// The failure is not cached: with the roots fixed, the same session works.
	root, goodURI := projectDir(t)
	client.RemoveRoots(badURI)
	client.AddRoots(&mcp.Root{URI: goodURI})
	eventually(t, "session to bind the corrected root", func() bool {
		out, ok := toolText(t, cs, "dexter_search", map[string]any{"query": "x"})
		return ok && strings.Contains(out, "root="+root)
	})
}

// Sessions with different roots use their own workspaces; sessions with the
// same root share one connection.
func TestNegotiation_MultipleRoots(t *testing.T) {
	e := setupNegotiation(t)
	rootA, uriA := projectDir(t)
	rootB, uriB := projectDir(t)
	csA, _ := e.connect(nil, uriA)
	csB, _ := e.connect(nil, uriB)
	csA2, _ := e.connect(nil, uriA)

	wantContains(t, mustTool(t, csA, "dexter_search", map[string]any{"query": "x"}), "root="+rootA)
	wantContains(t, mustTool(t, csB, "dexter_search", map[string]any{"query": "x"}), "root="+rootB)
	wantContains(t, mustTool(t, csA2, "dexter_search", map[string]any{"query": "x"}), "root="+rootA)

	if n := e.connCount(); n != 2 {
		t.Errorf("3 sessions over 2 roots hold %d workspace connections, want 2", n)
	}
	if n := len(e.backendsFor(rootA)); n != 1 {
		t.Errorf("2 sessions on one root opened %d connections, want 1", n)
	}
}

func TestNegotiation_RootsChangedSwapsWorkspace(t *testing.T) {
	e := setupNegotiation(t)
	rootA, uriA := projectDir(t)
	rootB, uriB := projectDir(t)
	cs, client := e.connect(nil, uriA)

	wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "x"}), "root="+rootA)

	client.RemoveRoots(uriA)
	client.AddRoots(&mcp.Root{URI: uriB})
	eventually(t, "session to move to the new root", func() bool {
		out, ok := toolText(t, cs, "dexter_search", map[string]any{"query": "x"})
		return ok && strings.Contains(out, "root="+rootB)
	})

	// The old workspace connection is closed: nothing else uses it.
	for _, b := range e.backendsFor(rootA) {
		if !b.isClosed() {
			t.Error("old workspace connection still open after the swap")
		}
	}
	if n := e.connCount(); n != 1 {
		t.Errorf("%d workspace connections after the swap, want 1", n)
	}
}

// A roots change that resolves to the same project keeps the connection.
func TestNegotiation_SameRootChangeIsNoop(t *testing.T) {
	e := setupNegotiation(t)
	root, uri := projectDir(t)
	cs, client := e.connect(nil, uri)
	mustTool(t, cs, "dexter_search", map[string]any{"query": "x"})

	// Same project, different advertised directory: the subdirectory
	// resolves upward through .git.
	subdir := filepath.Join(root, "lib")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	client.AddRoots(&mcp.Root{URI: fileURI(subdir)})
	eventually(t, "roots change notification to arrive", func() bool {
		e.f.mu.Lock()
		defer e.f.mu.Unlock()
		for _, st := range e.f.sessions {
			if st.dirty {
				return true
			}
		}
		return false
	})
	wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "x"}), "root="+root) // negotiates again

	backends := e.backendsFor(root)
	if len(backends) != 1 || backends[0].isClosed() {
		t.Errorf("workspace connection was replaced for a change that resolves to the same root")
	}
}

// A workspace root with characters that URI-encode (spaces) binds correctly.
func TestNegotiation_RootWithSpaces(t *testing.T) {
	e := setupNegotiation(t)
	root := filepath.Join(t.TempDir(), "my project")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	uri := fileURI(root)
	if !strings.Contains(uri, "%20") {
		t.Fatalf("test URI %q does not exercise percent-encoding", uri)
	}
	cs, _ := e.connect(nil, uri)
	wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "x"}), "root="+root)
}

// A session disconnecting releases its workspace connection.
func TestNegotiation_SessionCloseReleasesWorkspace(t *testing.T) {
	e := setupNegotiation(t)
	root, uri := projectDir(t)
	cs, _ := e.connect(nil, uri)
	mustTool(t, cs, "dexter_search", map[string]any{"query": "x"})

	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "workspace connection to be released", func() bool {
		backends := e.backendsFor(root)
		return len(backends) == 1 && backends[0].isClosed() && e.connCount() == 0
	})
}

// A fixed root (`dexter mcp <path>`) ignores the client's roots.
func TestFixedRootIgnoresClientRoots(t *testing.T) {
	fixed := t.TempDir()
	e := setupNegotiationWith(t, Config{Root: fixed, Fixed: true})
	_, uri := projectDir(t)
	cs, _ := e.connect(nil, uri)
	wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "x"}), "root="+fixed)
}
