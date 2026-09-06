// Package lsptest drives a dexter LSP server over stdio.
//
// It exists so tests can assert on what the server actually returns on the
// wire, rather than on the handlers' internals: the handlers reach the store,
// the tokenizer, tree-sitter and the __using__ cache, and only an end-to-end
// request exercises the whole chain an editor sees.
//
// Client returns errors and knows nothing about testing, so the same code drives
// the integration tests and cmd/lspprobe, which can be pointed at any project on
// disk. Tests use the T wrapper in testing.go, which turns errors into t.Fatal.
//
// A Client speaks enough of the protocol to be useful and no more: the
// initialize handshake, textDocument/didOpen, and the request methods below.
// Server-initiated traffic is handled so a chatty server cannot wedge a caller —
// notifications are dropped, and requests are answered with a null result.
package lsptest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds a single request. Callers working against a very large
// project should raise it with SetTimeout.
const DefaultTimeout = 30 * time.Second

// Position is a zero-based line/character pair, as in the protocol.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a start/end pair of positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a resolved position in a file.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Path returns the location's filesystem path, with the file:// scheme removed.
func (l Location) Path() string {
	trimmed := strings.TrimPrefix(l.URI, "file://")
	if p, err := url.PathUnescape(trimmed); err == nil {
		return p
	}
	return trimmed
}

// String renders a location as "<path>:<line>" with a 1-based line, matching
// what an editor shows and what a terminal can jump to.
func (l Location) String() string {
	return l.Path() + ":" + strconv.Itoa(l.Range.Start.Line+1)
}

// Client is a running dexter LSP server plus the stdio pipes to talk to it.
type Client struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	root    string
	timeout time.Duration

	mu     sync.Mutex
	nextID int
	opened map[string]bool
	closed bool
}

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Start launches binary as an LSP server rooted at root and completes the
// initialize handshake. Call Close when finished. stderr is where the server's
// own logging goes; pass nil to discard it.
func Start(binary, root string, stderr io.Writer) (*Client, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root %s: %w", root, err)
	}

	cmd := exec.Command(binary, "lsp")
	cmd.Dir = absRoot
	cmd.Stderr = stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}

	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReaderSize(stdout, 1<<20),
		root:    absRoot,
		timeout: DefaultTimeout,
		opened:  map[string]bool{},
	}

	if _, err := c.Request("initialize", map[string]interface{}{
		"processId": os.Getpid(),
		"rootUri":   URI(absRoot),
		"capabilities": map[string]interface{}{
			"textDocument": map[string]interface{}{
				"references": map[string]interface{}{},
				"definition": map[string]interface{}{},
				"hover":      map[string]interface{}{},
				"rename":     map[string]interface{}{"prepareSupport": true},
			},
		},
	}); err != nil {
		c.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := c.Notify("initialized", map[string]interface{}{}); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Root returns the project root the server was started in.
func (c *Client) Root() string { return c.root }

// SetTimeout changes the per-request deadline.
func (c *Client) SetTimeout(d time.Duration) { c.timeout = d }

// URI converts a filesystem path to a file:// URI.
func URI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "file://" + abs
}

func (c *Client) write(v interface{}) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if _, err := fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := c.stdin.Write(body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}
	return nil
}

func (c *Client) readMessage() (*message, error) {
	length := -1
	for {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, found := strings.Cut(line, ":")
		if found && strings.EqualFold(strings.TrimSpace(key), "content-length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length %q: %w", value, err)
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("message without Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c.stdout, body); err != nil {
		return nil, err
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("bad message body: %w", err)
	}
	return &m, nil
}

// Request sends a request and returns its raw result. Server-initiated traffic
// that arrives while waiting is handled: notifications are dropped, and requests
// are answered with a null result so the server never blocks on us.
func (c *Client) Request(method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	wantID := strconv.Itoa(id)
	if err := c.write(map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}); err != nil {
		return nil, err
	}

	type outcome struct {
		result json.RawMessage
		err    error
	}
	done := make(chan outcome, 1)

	go func() {
		for {
			m, err := c.readMessage()
			if err != nil {
				done <- outcome{nil, err}
				return
			}
			if m.ID == nil {
				continue // notification from the server
			}
			if string(*m.ID) == wantID {
				if m.Error != nil {
					done <- outcome{nil, fmt.Errorf("server error %d: %s", m.Error.Code, m.Error.Message)}
					return
				}
				done <- outcome{m.Result, nil}
				return
			}
			if m.Method != "" {
				// A request from the server. Answer it so it does not wait.
				_ = c.write(map[string]interface{}{"jsonrpc": "2.0", "id": json.RawMessage(*m.ID), "result": nil})
			}
		}
	}()

	select {
	case out := <-done:
		if out.err != nil {
			return nil, fmt.Errorf("%s: %w", method, out.err)
		}
		return out.result, nil
	case <-time.After(c.timeout):
		return nil, fmt.Errorf("%s timed out after %s", method, c.timeout)
	}
}

// Notify sends a notification, for which no reply is expected.
func (c *Client) Notify(method string, params interface{}) error {
	return c.write(map[string]interface{}{"jsonrpc": "2.0", "method": method, "params": params})
}

// Open sends textDocument/didOpen for path, reading the file from disk. Opening
// the same path twice is a no-op, so callers can open freely before a request.
func (c *Client) Open(path string) error {
	c.mu.Lock()
	already := c.opened[path]
	c.opened[path] = true
	c.mu.Unlock()
	if already {
		return nil
	}

	text, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return c.Notify("textDocument/didOpen", map[string]interface{}{
		"textDocument": map[string]interface{}{
			"uri": URI(path), "languageId": "elixir", "version": 1, "text": string(text),
		},
	})
}

func (c *Client) position(path string, line, char int) (map[string]interface{}, error) {
	if err := c.Open(path); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"textDocument": map[string]interface{}{"uri": URI(path)},
		"position":     Position{Line: line, Character: char},
	}, nil
}

// References returns textDocument/references at a zero-based position.
func (c *Client) References(path string, line, char int, includeDeclaration bool) ([]Location, error) {
	params, err := c.position(path, line, char)
	if err != nil {
		return nil, err
	}
	params["context"] = map[string]interface{}{"includeDeclaration": includeDeclaration}
	raw, err := c.Request("textDocument/references", params)
	if err != nil {
		return nil, err
	}
	return decodeLocations(raw)
}

// Definition returns textDocument/definition at a zero-based position. The
// server may answer with a single Location or an array; both are accepted.
func (c *Client) Definition(path string, line, char int) ([]Location, error) {
	params, err := c.position(path, line, char)
	if err != nil {
		return nil, err
	}
	raw, err := c.Request("textDocument/definition", params)
	if err != nil {
		return nil, err
	}
	return decodeLocations(raw)
}

// Hover returns the hover text at a zero-based position, or "" when the server
// reports no hover.
func (c *Client) Hover(path string, line, char int) (string, error) {
	params, err := c.position(path, line, char)
	if err != nil {
		return "", err
	}
	raw, err := c.Request("textDocument/hover", params)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var hover struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &hover); err != nil {
		return "", fmt.Errorf("hover: %w", err)
	}
	var markup struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(hover.Contents, &markup); err == nil && markup.Value != "" {
		return markup.Value, nil
	}
	var plain string
	if err := json.Unmarshal(hover.Contents, &plain); err == nil {
		return plain, nil
	}
	return string(hover.Contents), nil
}

func decodeLocations(raw json.RawMessage) ([]Location, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var many []Location
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	var one Location
	if err := json.Unmarshal(raw, &one); err == nil && one.URI != "" {
		return []Location{one}, nil
	}
	return nil, fmt.Errorf("cannot decode locations from %s", truncate(string(raw)))
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// Lines renders locations as sorted "<path relative to root>:<1-based line>"
// strings. That form is stable across machines, readable in a failure message,
// and diffable between two runs.
func Lines(root string, locs []Location) []string {
	out := make([]string, 0, len(locs))
	for _, l := range locs {
		p := l.Path()
		if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = rel
		}
		out = append(out, p+":"+strconv.Itoa(l.Range.Start.Line+1))
	}
	sort.Strings(out)
	return out
}

// Find returns the zero-based line and character of the nth (1-based) occurrence
// of substr in the file at path, pointing at the first character of the match.
// Locating a cursor by what the source says, rather than by a hard-coded line
// number, keeps tests readable and survives edits to fixtures.
func Find(path, substr string, nth int) (line, char int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", path, err)
	}
	seen := 0
	for i, text := range strings.Split(string(data), "\n") {
		offset := 0
		for {
			idx := strings.Index(text[offset:], substr)
			if idx < 0 {
				break
			}
			seen++
			if seen == nth {
				return i, offset + idx, nil
			}
			offset += idx + 1
		}
	}
	return 0, 0, fmt.Errorf("%q occurrence %d not found in %s", substr, nth, path)
}

// Close shuts the server down. It is safe to call more than once.
func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	// Ask politely, then make sure the process is gone.
	_ = c.write(map[string]interface{}{"jsonrpc": "2.0", "id": 999999, "method": "shutdown", "params": nil})
	_ = c.Notify("exit", nil)
	_ = c.stdin.Close()

	done := make(chan struct{})
	go func() { _, _ = c.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
	}
}
