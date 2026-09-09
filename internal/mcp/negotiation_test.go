package mcp

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/store"
)

// negotiationEnv is a negotiating Handler with no fixed workspace, plus
// helpers to connect clients that advertise chosen roots.
type negotiationEnv struct {
	t        *testing.T
	h        *Handler
	fallback string
}

func setupNegotiation(t *testing.T) *negotiationEnv {
	t.Helper()
	fallback := t.TempDir()
	h := NewHandler(Config{ProjectRoot: fallback, NegotiateRoots: true})
	t.Cleanup(h.Close)
	return &negotiationEnv{t: t, h: h, fallback: fallback}
}

// connect wires a new client session to the negotiating server. Roots are
// added before connecting so they are visible from the first roots/list.
func (e *negotiationEnv) connect(opts *mcp.ClientOptions, rootURIs ...string) (*mcp.ClientSession, *mcp.Client) {
	e.t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := NewServer(e.h).Connect(ctx, serverTransport, nil)
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

// projectDir creates a project directory containing one module and returns
// its path and file URI.
func projectDir(t *testing.T, module string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	writeSource(t, dir, "lib/mod.ex", "defmodule "+module+" do\n  def hello, do: :ok\nend\n")
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

func TestFileURIToPath(t *testing.T) {
	cases := []struct {
		uri  string
		want string // "" means an error is expected
	}{
		{"file:///a/b", "/a/b"},
		{"file:///a/b/", "/a/b"}, // trailing slash must not key a second workspace
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

func hasIndex(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".dexter", "dexter.db"))
	return err == nil
}

func TestNegotiation_BindsClientRoot(t *testing.T) {
	e := setupNegotiation(t)
	root, uri := projectDir(t, "NegBind.Hello")
	cs, _ := e.connect(nil, uri)

	out := mustTool(t, cs, "dexter_search", map[string]any{"query": "NegBind"})
	wantContains(t, out, "NegBind.Hello")

	if !hasIndex(root) {
		t.Error("no index created under the negotiated root")
	}
	if hasIndex(e.fallback) {
		t.Error("index created under the fallback root despite a negotiated root")
	}
	wantContains(t, mustTool(t, cs, "dexter_workspace", nil), root)
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
			writeSource(t, e.fallback, "lib/mod.ex", "defmodule NegFall.Hello do\nend\n")
			cs, _ := e.connect(tc.opts, tc.uris...)

			out := mustTool(t, cs, "dexter_search", map[string]any{"query": "NegFall"})
			wantContains(t, out, "NegFall.Hello")
			if !hasIndex(e.fallback) {
				t.Error("no index created under the fallback root")
			}
		})
	}
}

// A root inside a repository resolves upward to the repository, exactly like
// the LSP's Initialize: .dexter/dexter.db or .git win, and a nested mix.exs
// does not stop the walk.
func TestNegotiation_ResolvesRootLikeLSP(t *testing.T) {
	e := setupNegotiation(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	writeSource(t, repo, "apps/web/mix.exs", "defmodule Web.MixProject do\nend\n")
	writeSource(t, repo, "lib/top.ex", "defmodule NegRepo.Top do\nend\n")
	cs, _ := e.connect(nil, fileURI(filepath.Join(repo, "apps", "web")))

	// A module outside the advertised subdirectory is indexed, proving the
	// workspace anchored on the repository root.
	out := mustTool(t, cs, "dexter_search", map[string]any{"query": "NegRepo"})
	wantContains(t, out, "NegRepo.Top")
	if !hasIndex(repo) {
		t.Error("no index at the repository root")
	}
	if hasIndex(filepath.Join(repo, "apps", "web")) {
		t.Error("index created at the subdirectory instead of the repository root")
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
	root, goodURI := projectDir(t, "NegRetry.Hello")
	client.RemoveRoots(badURI)
	client.AddRoots(&mcp.Root{URI: goodURI})
	eventually(t, "session to bind the corrected root", func() bool {
		out, ok := toolText(t, cs, "dexter_search", map[string]any{"query": "NegRetry"})
		return ok && strings.Contains(out, "NegRetry.Hello")
	})
	if !hasIndex(root) {
		t.Error("no index created under the corrected root")
	}
}

// Sessions with different roots work concurrently against their own
// workspaces; sessions with the same root share one.
func TestNegotiation_MultipleRoots(t *testing.T) {
	e := setupNegotiation(t)
	rootA, uriA := projectDir(t, "NegMultiA.Mod")
	rootB, uriB := projectDir(t, "NegMultiB.Mod")
	csA, _ := e.connect(nil, uriA)
	csB, _ := e.connect(nil, uriB)
	csA2, _ := e.connect(nil, uriA)

	outA := mustTool(t, csA, "dexter_search", map[string]any{"query": "NegMulti"})
	wantContains(t, outA, "NegMultiA.Mod")
	wantNotContains(t, outA, "NegMultiB.Mod")

	outB := mustTool(t, csB, "dexter_search", map[string]any{"query": "NegMulti"})
	wantContains(t, outB, "NegMultiB.Mod")
	wantNotContains(t, outB, "NegMultiA.Mod")

	wantContains(t, mustTool(t, csA2, "dexter_search", map[string]any{"query": "NegMulti"}), "NegMultiA.Mod")

	if !hasIndex(rootA) || !hasIndex(rootB) {
		t.Error("expected an index under each negotiated root")
	}
	e.h.mu.Lock()
	nbindings := len(e.h.bindings)
	e.h.mu.Unlock()
	if nbindings != 2 {
		t.Errorf("3 sessions over 2 roots hold %d workspaces, want 2", nbindings)
	}
}

func TestNegotiation_RootsChangedSwapsWorkspace(t *testing.T) {
	e := setupNegotiation(t)
	rootA, uriA := projectDir(t, "NegSwapA.Mod")
	rootB, uriB := projectDir(t, "NegSwapB.Mod")
	cs, client := e.connect(nil, uriA)

	wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "NegSwapA"}), "NegSwapA.Mod")

	client.RemoveRoots(uriA)
	client.AddRoots(&mcp.Root{URI: uriB})
	eventually(t, "session to move to the new root", func() bool {
		out, ok := toolText(t, cs, "dexter_search", map[string]any{"query": "NegSwapB"})
		return ok && strings.Contains(out, "NegSwapB.Mod")
	})
	wantContains(t, mustTool(t, cs, "dexter_workspace", nil), rootB)

	// The old workspace is torn down: its watcher no longer indexes new files
	// into its store.
	e.h.mu.Lock()
	_, oldBound := e.h.bindings[rootA]
	e.h.mu.Unlock()
	if oldBound {
		t.Error("old workspace still held after the swap")
	}
	writeSource(t, rootA, "lib/late.ex", "defmodule NegSwapA.Late do\nend\n")
	time.Sleep(4 * debounceWindow)
	oldStore, err := store.Open(rootA)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = oldStore.Close() }()
	if results, err := oldStore.LookupModule("NegSwapA.Late"); err != nil || len(results) != 0 {
		t.Errorf("old workspace's watcher still indexing after teardown: %v, %v", results, err)
	}
}

// A roots change that resolves to the same project keeps the workspace: no
// teardown, no rebuild.
func TestNegotiation_SameRootChangeIsNoop(t *testing.T) {
	e := setupNegotiation(t)
	root, uri := projectDir(t, "NegNoop.Mod")
	cs, client := e.connect(nil, uri)
	mustTool(t, cs, "dexter_search", map[string]any{"query": "NegNoop"})

	e.h.mu.Lock()
	before := e.h.bindings[root]
	e.h.mu.Unlock()

	// Same project, different advertised directory: the first call binds it,
	// so the subdirectory resolves upward via .dexter/dexter.db.
	subdir := filepath.Join(root, "lib")
	client.AddRoots(&mcp.Root{URI: fileURI(subdir)})
	eventually(t, "roots change notification to arrive", func() bool {
		e.h.mu.Lock()
		defer e.h.mu.Unlock()
		for _, d := range e.h.dirty {
			if d {
				return true
			}
		}
		return false
	})
	mustTool(t, cs, "dexter_search", map[string]any{"query": "NegNoop"}) // renegotiates

	e.h.mu.Lock()
	after := e.h.bindings[root]
	e.h.mu.Unlock()
	if before != after {
		t.Error("workspace was rebuilt for a change that resolves to the same root")
	}
}

// A workspace root with characters that URI-encode (spaces) binds correctly.
func TestNegotiation_RootWithSpaces(t *testing.T) {
	e := setupNegotiation(t)
	root := filepath.Join(t.TempDir(), "my project")
	writeSource(t, root, "lib/mod.ex", "defmodule NegSpace.Mod do\nend\n")
	uri := fileURI(root)
	if !strings.Contains(uri, "%20") {
		t.Fatalf("test URI %q does not exercise percent-encoding", uri)
	}
	cs, _ := e.connect(nil, uri)
	wantContains(t, mustTool(t, cs, "dexter_search", map[string]any{"query": "NegSpace"}), "NegSpace.Mod")
	if !hasIndex(root) {
		t.Error("no index created under the percent-encoded root")
	}
}

// A tool call during a long initial index reports that the workspace is still
// building instead of hanging.
func TestNegotiation_ReportsInitializing(t *testing.T) {
	prev := indexWaitLimit
	indexWaitLimit = 10 * time.Millisecond
	defer func() { indexWaitLimit = prev }()

	e := setupNegotiation(t)
	// A pre-installed workspace whose initial index never finishes.
	b := &binding{root: e.fallback, initDone: make(chan struct{}), indexed: make(chan struct{})}
	close(b.initDone)
	e.h.mu.Lock()
	e.h.bindings[e.fallback] = b
	e.h.mu.Unlock()

	cs, _ := e.connect(nil)
	out, ok := toolText(t, cs, "dexter_search", map[string]any{"query": "x"})
	if ok {
		t.Fatalf("tool call succeeded against an unindexed workspace: %s", out)
	}
	if !strings.Contains(out, "still building") {
		t.Errorf("error does not report the index build: %s", out)
	}
	close(b.indexed) // let Close tear it down without blocking
	b.initErr = context.Canceled
}

// A session disconnecting releases its workspace.
func TestNegotiation_SessionCloseReleasesWorkspace(t *testing.T) {
	e := setupNegotiation(t)
	root, uri := projectDir(t, "NegClose.Mod")
	cs, _ := e.connect(nil, uri)
	mustTool(t, cs, "dexter_search", map[string]any{"query": "NegClose"})

	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "workspace to be released", func() bool {
		e.h.mu.Lock()
		defer e.h.mu.Unlock()
		_, held := e.h.bindings[root]
		return !held && len(e.h.sessions) == 0
	})
}
