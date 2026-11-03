package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"
)

// lspEditor plays an editor on one LSP byte stream. It answers requests that
// the server sends with a null result, and it keeps the notifications it reads.
type lspEditor struct {
	tb testing.TB
	w  io.Writer
	r  *bufio.Reader

	wmu           sync.Mutex
	mu            sync.Mutex
	notifications []map[string]any

	// msgs is set by pump: a goroutine then reads the stream, so that the
	// other side never blocks on a write that nobody reads.
	msgs chan pumped
}

type pumped struct {
	message map[string]any
	err     error
}

// pump reads the stream in the background from now on.
func (e *lspEditor) pump() *lspEditor {
	e.msgs = make(chan pumped, 1024)
	go func() {
		for {
			message, err := e.read()
			e.msgs <- pumped{message, err}
			if err != nil {
				return
			}
		}
	}()
	return e
}

func newLSPEditor(tb testing.TB, w io.Writer, r io.Reader) *lspEditor {
	return &lspEditor{tb: tb, w: w, r: bufio.NewReader(r)}
}

func (e *lspEditor) send(message map[string]any) {
	e.tb.Helper()
	message["jsonrpc"] = "2.0"
	body, err := json.Marshal(message)
	if err != nil {
		e.tb.Fatal(err)
	}
	e.wmu.Lock()
	_, err = fmt.Fprintf(e.w, "Content-Length: %d\r\n\r\n%s", len(body), body)
	e.wmu.Unlock()
	if err != nil {
		e.tb.Fatal(err)
	}
}

func (e *lspEditor) request(id int, method string, params any) {
	e.tb.Helper()
	e.send(map[string]any{"id": id, "method": method, "params": params})
}

func (e *lspEditor) notify(method string, params any) {
	e.tb.Helper()
	e.send(map[string]any{"method": method, "params": params})
}

// next reads one message, or returns an error at the end of the stream.
func (e *lspEditor) next() (map[string]any, error) {
	if e.msgs != nil {
		select {
		case m, ok := <-e.msgs:
			if !ok {
				return nil, io.EOF
			}
			if m.err != nil {
				e.msgs <- m // every later call sees the end too
			}
			return m.message, m.err
		case <-time.After(10 * time.Second):
			return nil, errors.New("no message within 10s")
		}
	}
	return e.read()
}

func (e *lspEditor) read() (map[string]any, error) {
	body, err := readLSPFrame(e.r)
	if err != nil {
		return nil, err
	}
	var message map[string]any
	if err := json.Unmarshal(body, &message); err != nil {
		return nil, fmt.Errorf("decoding %q: %w", body, err)
	}
	return message, nil
}

// response reads until the response with id arrives.
func (e *lspEditor) response(id int) map[string]any {
	e.tb.Helper()
	for {
		message, err := e.next()
		if err != nil {
			e.tb.Fatalf("waiting for the response %d: %v", id, err)
		}
		if e.handle(message) {
			continue
		}
		if got, ok := idAsInt(message["id"]); ok && got == id {
			return message
		}
	}
}

// handle answers a server request and keeps a notification. It reports
// whether message was one of those.
func (e *lspEditor) handle(message map[string]any) bool {
	method, hasMethod := message["method"].(string)
	if !hasMethod {
		return false
	}
	if rawID, hasID := message["id"]; hasID {
		// From another goroutine: the caller may be the only reader while a
		// writer waits for the other side to read.
		go e.send(map[string]any{"id": rawID, "result": nil})
		return true
	}
	e.mu.Lock()
	e.notifications = append(e.notifications, message)
	e.mu.Unlock()
	_ = method
	return true
}

func (e *lspEditor) shown() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, n := range e.notifications {
		if n["method"] == "window/showMessage" {
			if params, ok := n["params"].(map[string]any); ok {
				out = append(out, fmt.Sprint(params["message"]))
			}
		}
	}
	return out
}

func fileURI(path string) string { return "file://" + path }

// proxyFixture is a small project with one call that hover and completion can
// resolve.
func proxyFixture(tb testing.TB) (root, caller, callerText string) {
	tb.Helper()
	root = tb.TempDir()
	write := func(rel, text string) string {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			tb.Fatal(err)
		}
		return path
	}
	write("mix.exs", "defmodule MyApp.MixProject do\n  use Mix.Project\nend\n")
	write("lib/my_app/accounts.ex", "defmodule MyApp.Accounts do\n  @doc \"Gets a user.\"\n  def get_user(id), do: id\n\n  def list_users, do: []\nend\n")
	callerText = "defmodule MyApp.Caller do\n  def run do\n    MyApp.Accounts.get_user(1)\n  end\nend\n"
	caller = write("lib/my_app/caller.ex", callerText)
	return root, caller, callerText
}

// initializeEditor runs the LSP startup and opens caller.
func initializeEditor(tb testing.TB, e *lspEditor, root, caller, text string) {
	tb.Helper()
	e.request(1, "initialize", map[string]any{
		"processId":    nil,
		"rootUri":      fileURI(root),
		"capabilities": map[string]any{},
	})
	e.response(1)
	e.notify("initialized", map[string]any{})
	e.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(caller), "languageId": "elixir", "version": 1, "text": text},
	})
}

// hoverParams points at get_user in the caller fixture.
func hoverParams(caller string) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": fileURI(caller)},
		"position":     map[string]any{"line": 2, "character": 21},
	}
}

func completionParams(caller string) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": fileURI(caller)},
		"position":     map[string]any{"line": 2, "character": 19},
	}
}

// percentile returns the p-th percentile of sorted durations.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

// benchEditorMode attaches an editor through ProxyLSP ("proxy"), straight to
// the daemon's LSP socket ("direct"), or through the plain byte copy that the
// proxy was before it carried sessions over ("pipe"), so that one run compares
// all three.
func benchEditorMode(b *testing.B, mode string) (*lspEditor, string) {
	viaProxy := mode != "direct"
	b.Helper()
	quietEnv(b)
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	root, caller, text := proxyFixture(b)
	client := startDaemon(b, root, time.Minute)
	if _, err := client.WorkspaceStatus(context.Background(), 30_000); err != nil {
		b.Fatal(err)
	}
	var e *lspEditor
	if viaProxy {
		editorToProxy, proxyIn := io.Pipe()
		proxyOut, proxyToEditor := io.Pipe()
		done := make(chan error, 1)
		go func() {
			if mode == "pipe" {
				conn, reader, err := connectFrontend(context.Background(), root, kindLSP, "")
				if err == nil {
					err = pipeFrontend(conn, reader, editorToProxy, proxyToEditor)
				}
				done <- err
			} else {
				done <- ProxyLSP(context.Background(), root, editorToProxy, proxyToEditor)
			}
			_ = proxyToEditor.Close()
			_ = editorToProxy.CloseWithError(errors.New("the proxy ended"))
			// A test that writes after the proxy ended fails instead of hanging.
			_ = editorToProxy.CloseWithError(errors.New("the proxy ended"))
		}()
		b.Cleanup(func() {
			_ = proxyIn.Close()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				b.Error("proxy did not exit")
			}
		})
		e = newLSPEditor(b, proxyIn, proxyOut)
	} else {
		conn, reader, _, err := OpenLSP(context.Background(), root)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = conn.Close() })
		e = &lspEditor{tb: b, w: conn, r: reader}
	}
	initializeEditor(b, e, root, caller, text)
	return e, caller
}

func benchmarkRoundTrips(b *testing.B, viaProxy bool, method string) {
	benchmarkRoundTripsMode(b, map[bool]string{true: "proxy", false: "direct"}[viaProxy], method)
}

func benchmarkRoundTripsMode(b *testing.B, mode, method string) {
	e, caller := benchEditorMode(b, mode)
	params := hoverParams(caller)
	if method == "textDocument/completion" {
		params = completionParams(caller)
	}
	// Warm the caches before timing.
	for i := 0; i < 20; i++ {
		e.request(1000+i, method, params)
		e.response(1000 + i)
	}
	durations := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		e.request(10_000+i, method, params)
		e.response(10_000 + i)
		durations = append(durations, time.Since(start))
	}
	b.StopTimer()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	b.ReportMetric(float64(percentile(durations, 0.5).Microseconds()), "p50-µs")
	b.ReportMetric(float64(percentile(durations, 0.95).Microseconds()), "p95-µs")
}

// benchmarkBurst sends 100 requests back to back while another goroutine
// reads the answers, as an editor does while the user types.
func benchmarkBurst(b *testing.B, viaProxy bool) {
	benchmarkBurstMode(b, map[bool]string{true: "proxy", false: "direct"}[viaProxy])
}

func benchmarkBurstMode(b *testing.B, mode string) {
	e, caller := benchEditorMode(b, mode)
	params := completionParams(caller)
	const burst = 100
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		base := 100_000 + i*burst
		read := make(chan struct{})
		go func() {
			defer close(read)
			for j := 0; j < burst; j++ {
				e.response(base + j)
			}
		}()
		for j := 0; j < burst; j++ {
			e.request(base+j, "textDocument/completion", params)
		}
		<-read
	}
}

func BenchmarkLSPHoverDirect(b *testing.B) { benchmarkRoundTrips(b, false, "textDocument/hover") }
func BenchmarkLSPHoverProxy(b *testing.B)  { benchmarkRoundTrips(b, true, "textDocument/hover") }
func BenchmarkLSPCompletionDirect(b *testing.B) {
	benchmarkRoundTrips(b, false, "textDocument/completion")
}
func BenchmarkLSPCompletionProxy(b *testing.B) {
	benchmarkRoundTrips(b, true, "textDocument/completion")
}
func BenchmarkLSPHoverPipe(b *testing.B) { benchmarkRoundTripsMode(b, "pipe", "textDocument/hover") }
func BenchmarkLSPCompletionPipe(b *testing.B) {
	benchmarkRoundTripsMode(b, "pipe", "textDocument/completion")
}
func BenchmarkLSPCompletionBurstPipe(b *testing.B)   { benchmarkBurstMode(b, "pipe") }
func BenchmarkLSPCompletionBurstDirect(b *testing.B) { benchmarkBurst(b, false) }
func BenchmarkLSPCompletionBurstProxy(b *testing.B)  { benchmarkBurst(b, true) }
