package mcp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCheckListenAddr(t *testing.T) {
	for addr, ok := range map[string]bool{
		"localhost:0":       true,
		"127.0.0.1:8080":    true,
		"127.1.2.3:8080":    true,
		"[::1]:8080":        true,
		":8080":             false,
		"0.0.0.0:8080":      false,
		"[::]:8080":         false,
		"192.168.1.20:8080": false,
		"example.com:8080":  false,
		"localhost":         false, // no port
	} {
		err := CheckListenAddr(addr, false)
		if (err == nil) != ok {
			t.Errorf("CheckListenAddr(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
	if err := CheckListenAddr("0.0.0.0:8080", true); err != nil {
		t.Errorf("an explicit unsafe listen was refused: %v", err)
	}
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func postMCP(t *testing.T, url string, body io.Reader, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func httpTestServer(t *testing.T) (*httptest.Server, *negotiationEnv) {
	t.Helper()
	e := setupNegotiationWith(t, Config{Root: t.TempDir(), Fixed: true})
	srv := httptest.NewServer(HTTPHandler(e.f))
	t.Cleanup(srv.Close)
	return srv, e
}

// Regression: a request body had no size limit; a 300 MB POST made the server
// hold gigabytes.
func TestHTTPHandler_CapsBodySize(t *testing.T) {
	srv, _ := httpTestServer(t)
	if res := postMCP(t, srv.URL, strings.NewReader(initializeBody), nil); res.StatusCode != http.StatusOK {
		t.Fatalf("a small initialize got status %d", res.StatusCode)
	}
	// Valid JSON, padded past the limit.
	padded := initializeBody + strings.Repeat(" ", maxHTTPBodyBytes)
	if res := postMCP(t, srv.URL, bytes.NewReader([]byte(padded)), nil); res.StatusCode == http.StatusOK {
		t.Fatalf("a %d-byte body was accepted", len(padded))
	}
}

// A web page in the user's browser must not drive the tools.
func TestHTTPHandler_RefusesCrossOriginRequests(t *testing.T) {
	srv, _ := httpTestServer(t)
	for _, header := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "http://attacker.example"},
	} {
		if res := postMCP(t, srv.URL, strings.NewReader(initializeBody), header); res.StatusCode != http.StatusForbidden {
			t.Errorf("cross-origin request %v got status %d, want 403", header, res.StatusCode)
		}
	}
}

// Regression: HTTP sessions never timed out, so a client that went away
// without closing its session kept its workspace connection, and the daemon,
// alive forever.
func TestHTTPHandler_IdleSessionReleasesWorkspace(t *testing.T) {
	old := httpSessionTimeout
	httpSessionTimeout = 200 * time.Millisecond
	t.Cleanup(func() { httpSessionTimeout = old })
	srv, e := httpTestServer(t)

	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: srv.URL, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	mustTool(t, cs, "dexter_search", map[string]any{"query": "x"})
	if n := e.connCount(); n != 1 {
		t.Fatalf("%d workspace connections, want 1", n)
	}
	// The client stays silent; the session times out and lets go.
	eventually(t, "the idle session to release its workspace connection", func() bool { return e.connCount() == 0 })
}
