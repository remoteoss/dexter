package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
)

// testEnv is a full in-memory MCP round trip: client session <-> server with
// all tools registered, backed by a real store in a temp dir. Going through
// the SDK session exercises schema inference and argument validation, not
// just the handler bodies.
type testEnv struct {
	t       *testing.T
	store   *store.Store
	root    string
	session *mcp.ClientSession
}

func setupTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetIndexVersion(version.IndexVersion); err != nil {
		t.Fatal(err)
	}

	server := lsp.NewServer(s, root)
	h := NewHandler(Config{LSP: server, Store: s, ProjectRoot: root})

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := NewServer(h).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return &testEnv{t: t, store: s, root: root, session: session}
}

// indexFile writes an Elixir source file under the project root and indexes it.
func (e *testEnv) indexFile(relPath, content string) string {
	e.t.Helper()
	path := filepath.Join(e.root, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		e.t.Fatal(err)
	}
	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.store.IndexFileWithRefs(path, defs, refs); err != nil {
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
		wantReadOnly := tool.Name != "dexter_reindex"
		if a.ReadOnlyHint != wantReadOnly {
			t.Errorf("tool %s ReadOnlyHint = %v, want %v", tool.Name, a.ReadOnlyHint, wantReadOnly)
		}
	}
}
