package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/workspace"
)

// testEnv is a full in-memory MCP round trip: client session <-> Frontend
// with all tools declared <-> the tool bodies over a real workspace runtime in
// a temp dir. Going through the SDK session exercises schema inference and
// argument validation; the local backend calls the same Handler.Call that the
// daemon's control method calls.
type testEnv struct {
	t       *testing.T
	rt      *workspace.Runtime
	store   *store.Store
	lsp     *lsp.Server
	root    string
	session *mcp.ClientSession
}

// localBackend runs tool bodies in-process, as the daemon does.
type localBackend struct {
	h    *Handler
	wait time.Duration
}

func (b *localBackend) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return b.h.Call(ctx, name, args, b.wait)
}

func (b *localBackend) Close() error { return nil }

// openTestRuntime opens a workspace runtime over root without native
// watching, so tests decide when the index changes.
func openTestRuntime(t *testing.T, root string, opts workspace.Options) *workspace.Runtime {
	t.Helper()
	// Keep stdlib and version-manager detection from finding a real Elixir
	// install and indexing it.
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	opts.NoWatch = true
	rt, err := workspace.OpenWithOptions(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	rt := openTestRuntime(t, root, workspace.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rt.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(rt, rt.LanguageServices())
	session := connectFrontend(t, NewFrontend(Config{Root: root, Fixed: true, Connect: func(string) Backend {
		return &localBackend{h: h, wait: time.Second}
	}}))
	return &testEnv{t: t, rt: rt, store: rt.Store(), lsp: rt.LanguageServices(), root: root, session: session}
}

// connectFrontend connects an in-memory MCP client to a server over f.
func connectFrontend(t *testing.T, f *Frontend, rootURIs ...string) *mcp.ClientSession {
	t.Helper()
	t.Cleanup(f.Close)
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := NewServer(f).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	for _, u := range rootURIs {
		client.AddRoots(&mcp.Root{URI: u})
	}
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// indexFile writes an Elixir source file under the project root and waits
// until the workspace has indexed it.
func (e *testEnv) indexFile(relPath, content string) string {
	e.t.Helper()
	path := filepath.Join(e.root, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := e.rt.ReindexPath(ctx, path); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func (e *testEnv) callTool(name string, args map[string]any) string {
	e.t.Helper()
	res, err := e.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatalf("CallTool(%s): %v", name, err)
	}
	if res.IsError {
		e.t.Fatalf("CallTool(%s) returned tool error: %s", name, resultText(res))
	}
	return resultText(res)
}

func (e *testEnv) callToolExpectError(name string, args map[string]any) string {
	e.t.Helper()
	res, err := e.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		e.t.Fatalf("CallTool(%s) succeeded, want error; got: %s", name, resultText(res))
	}
	return resultText(res)
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func wantContains(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q.\nFull output:\n%s", w, got)
		}
	}
}

func wantNotContains(t *testing.T, got string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(got, w) {
			t.Errorf("output unexpectedly contains %q.\nFull output:\n%s", w, got)
		}
	}
}

func TestListTools(t *testing.T) {
	e := setupTestEnv(t)
	res, err := e.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"dexter_call_hierarchy",
		"dexter_definition",
		"dexter_file_outline",
		"dexter_implementations",
		"dexter_module_api",
		"dexter_references",
		"dexter_reindex",
		"dexter_rename_symbol",
		"dexter_search",
		"dexter_workspace",
	}
	if len(res.Tools) != len(want) {
		t.Errorf("registered %d tools, want %d", len(res.Tools), len(want))
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("tool %s not registered; got %v", w, got)
		}
	}

	for _, tool := range res.Tools {
		a := tool.Annotations
		if a == nil {
			t.Errorf("tool %s has no annotations", tool.Name)
			continue
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("tool %s not marked closed-world", tool.Name)
		}
		wantReadOnly := tool.Name != "dexter_reindex" && tool.Name != "dexter_rename_symbol"
		if a.ReadOnlyHint != wantReadOnly {
			t.Errorf("tool %s ReadOnlyHint = %v, want %v", tool.Name, a.ReadOnlyHint, wantReadOnly)
		}
	}
}
