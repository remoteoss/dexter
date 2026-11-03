package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPeekMessageFindsMethodIDAndURI(t *testing.T) {
	cases := []struct {
		name string
		body string
		want peeked
	}{
		{"request id first", `{"jsonrpc":"2.0","id":3,"method":"textDocument/hover","params":{"x":1}}`, peeked{method: "textDocument/hover", id: []byte("3")}},
		{"request id last", `{"jsonrpc":"2.0","method":"textDocument/hover","params":{"a":{"id":9}},"id":"abc"}`, peeked{method: "textDocument/hover", id: []byte(`"abc"`)}},
		{"request id in the middle", `{"method":"x/y","id":4,"params":[1,2],"jsonrpc":"2.0"}`, peeked{method: "x/y", id: []byte("4")}},
		{"notification", `{"jsonrpc":"2.0","method":"window/logMessage","params":{"message":"id"}}`, peeked{method: "window/logMessage"}},
		{"notification with a nested id", `{"jsonrpc":"2.0","method":"custom/n","params":{"id":5}}`, peeked{method: "custom/n"}},
		{"response id last", `{"jsonrpc":"2.0","result":{"items":[{"id":1}],"s":"\"id\":2}"},"id":7}`, peeked{id: []byte("7"), response: true}},
		{"response id first", `{"id":7,"result":null,"jsonrpc":"2.0"}`, peeked{id: []byte("7"), response: true}},
		{"error response", `{"jsonrpc":"2.0","error":{"code":-32601,"message":"no"},"id":"x"}`, peeked{id: []byte(`"x"`), response: true}},
		{"null id", `{"jsonrpc":"2.0","id":null,"error":{"code":-32700}}`, peeked{response: true}},
		{"didOpen", `{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"text":"a \"b\" \\ {[","uri":"file:///p/a.ex","languageId":"elixir"}}}`, peeked{method: "textDocument/didOpen", uri: "file:///p/a.ex"}},
		{"didChange", `{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"contentChanges":[{"text":"}}"}],"textDocument":{"version":2,"uri":"file:///p/a%20b.ex"}}}`, peeked{method: "textDocument/didChange", uri: "file:///p/a%20b.ex"}},
		{"escaped uri", `{"method":"textDocument/didClose","params":{"textDocument":{"uri":"file:\/\/\/p\/x.ex"}}}`, peeked{method: "textDocument/didClose", uri: "file:///p/x.ex"}},
		{"spaces", "{ \"id\" : 12 ,\n \"method\" : \"m\" }", peeked{method: "m", id: []byte("12")}},
		{"not an object", `[1,2]`, peeked{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := peekMessage([]byte(tc.body))
			if got.method != tc.want.method || !bytes.Equal(got.id, tc.want.id) || got.response != tc.want.response || got.uri != tc.want.uri {
				t.Fatalf("peekMessage = %+v (id %s), want %+v (id %s)", got, got.id, tc.want, tc.want.id)
			}
		})
	}
}

// TestPeekMessageAgreesWithAFullDecode compares the scan with encoding/json on
// messages whose members come in every order and whose values hold the
// characters that a scan can get wrong.
func TestPeekMessageAgreesWithAFullDecode(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	values := []any{
		"plain", `with "quotes" and \ backslash`, "ends with \\", `"id":3}`, "{[}]",
		map[string]any{"id": 1, "nested": []any{"}", map[string]any{"id": "x"}}},
		[]any{1, "two", nil, true},
		nil, 3.5, false,
	}
	for i := 0; i < 2000; i++ {
		members := map[string]any{"jsonrpc": "2.0"}
		if rng.Intn(3) > 0 {
			members["method"] = fmt.Sprintf("m/%d", rng.Intn(5))
		}
		if rng.Intn(3) > 0 {
			if rng.Intn(2) == 0 {
				members["id"] = rng.Intn(1000)
			} else {
				members["id"] = fmt.Sprintf("s-%d", rng.Intn(1000))
			}
		}
		if _, ok := members["method"]; ok {
			members["params"] = values[rng.Intn(len(values))]
		} else if rng.Intn(2) == 0 {
			members["result"] = values[rng.Intn(len(values))]
		} else {
			members["error"] = map[string]any{"code": -1, "message": values[rng.Intn(len(values))]}
		}
		keys := make([]string, 0, len(members))
		for k := range members {
			keys = append(keys, k)
		}
		rng.Shuffle(len(keys), func(a, b int) { keys[a], keys[b] = keys[b], keys[a] })
		var b strings.Builder
		b.WriteString("{")
		for j, k := range keys {
			if j > 0 {
				b.WriteString(",")
			}
			key, _ := json.Marshal(k)
			value, _ := json.Marshal(members[k])
			b.Write(key)
			b.WriteString(":")
			b.Write(value)
		}
		b.WriteString("}")
		body := []byte(b.String())

		var full struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(body, &full); err != nil {
			t.Fatal(err)
		}
		got := peekMessage(body)
		wantID := full.ID
		if string(wantID) == "null" {
			wantID = nil
		}
		if got.method != full.Method || !bytes.Equal(got.id, wantID) {
			t.Fatalf("%s: peek = method %q id %s, decode = method %q id %s", body, got.method, got.id, full.Method, wantID)
		}
		_, hasResult := members["result"]
		_, hasError := members["error"]
		if got.response != (hasResult || hasError) {
			// The scan may stop before it reaches a result when it has the
			// method and the id; only a response without a method matters.
			if full.Method == "" {
				t.Fatalf("%s: response = %v", body, got.response)
			}
		}
	}
}

func TestReadProxyFrameKeepsOneFramePerMessage(t *testing.T) {
	input := "Content-Length: 2\r\nContent-Type: application/vscode-jsonrpc; charset=utf-8\r\n\r\n{}content-length: 4\r\n\r\nnull"
	r := bufio.NewReader(strings.NewReader(input))
	frame, body, err := readProxyFrame(r)
	if err != nil || string(frame) != "Content-Length: 2\r\n\r\n{}" || string(frame[body:]) != "{}" {
		t.Fatalf("first frame = %q, %d, %v", frame, body, err)
	}
	frame, body, err = readProxyFrame(r)
	if err != nil || string(frame[body:]) != "null" {
		t.Fatalf("second frame = %q, %v", frame, err)
	}
	if _, _, err := readProxyFrame(r); err == nil {
		t.Fatal("no error at the end of the input")
	}
}

// BenchmarkPeekDidChange is the per-keystroke cost of the scan on a large
// document.
func BenchmarkPeekDidChange(b *testing.B) {
	text := strings.Repeat("  def run(x), do: IO.puts(\"value: #{inspect(x)}\")\n", 4000)
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didChange",
		"params": map[string]any{
			"textDocument":   map[string]any{"uri": "file:///p/lib/big.ex", "version": 3},
			"contentChanges": []any{map[string]any{"text": text}},
		},
	})
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if p := peekMessage(body); p.uri == "" {
			b.Fatal("no uri")
		}
	}
}

// BenchmarkPeekResponse is the cost of the scan on a large daemon answer.
func BenchmarkPeekResponse(b *testing.B) {
	items := make([]any, 2000)
	for i := range items {
		items[i] = map[string]any{"label": fmt.Sprintf("function_%d", i), "kind": 3, "detail": "def function(a, b)"}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "result": map[string]any{"items": items}, "id": 42})
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if p := peekMessage(body); !p.isResponse() {
			b.Fatal("not a response")
		}
	}
}

// decodedMessage is what encoding/json makes of a message, for comparisons
// with the scan. ok is false when the comparison does not apply: invalid
// JSON, duplicate keys (encoding/json keeps the last, the scan can stop at the
// first), invalid UTF-8, or a message that breaks JSON-RPC in a way that the
// scan does not have to see (an id on a notification method, a method on a
// response).
func decodedMessage(body []byte) (method string, id []byte, uri string, ok bool) {
	if !json.Valid(body) || !utf8.Valid(body) || hasDuplicateKeys(body) {
		return "", nil, "", false
	}
	// Maps, not structs: encoding/json matches struct fields without regard
	// to case, and JSON-RPC member names are exact.
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return "", nil, "", false
	}
	if raw, has := top["method"]; has && json.Unmarshal(raw, &method) != nil {
		return "", nil, "", false
	}
	_, hasResult := top["result"]
	_, hasError := top["error"]
	if method != "" && (hasResult || hasError) {
		return "", nil, "", false
	}
	rawID, hasID := top["id"]
	if notificationMethods[method] && hasID {
		return "", nil, "", false
	}
	if hasID && string(rawID) != "null" {
		id = rawID
	}
	if documentMethods[method] {
		var params map[string]json.RawMessage
		var doc map[string]json.RawMessage
		if json.Unmarshal(top["params"], &params) == nil && json.Unmarshal(params["textDocument"], &doc) == nil {
			var u string
			if json.Unmarshal(doc["uri"], &u) == nil {
				uri = u
			}
		}
	}
	return method, id, uri, true
}

// hasDuplicateKeys reports whether any object in body repeats a key.
func hasDuplicateKeys(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	var walk func() bool
	walk = func() bool {
		tok, err := dec.Token()
		if err != nil {
			return true
		}
		switch tok {
		case json.Delim('{'):
			seen := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return true
				}
				k, _ := key.(string)
				if seen[k] {
					return true
				}
				seen[k] = true
				if walk() {
					return true
				}
			}
			_, err := dec.Token()
			return err != nil
		case json.Delim('['):
			for dec.More() {
				if walk() {
					return true
				}
			}
			_, err := dec.Token()
			return err != nil
		}
		return false
	}
	return walk()
}

func checkPeek(t *testing.T, body []byte) {
	t.Helper()
	got := peekMessage(body) // must not panic on any input
	method, id, uri, ok := decodedMessage(body)
	if !ok {
		return
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return // valid JSON, but not an object
	}
	if got.method != method || !bytes.Equal(bytes.TrimSpace(got.id), bytes.TrimSpace(id)) {
		t.Fatalf("%s: scan = method %q id %s, decode = method %q id %s", body, got.method, got.id, method, id)
	}
	if documentMethods[method] && got.uri != uri {
		t.Fatalf("%s: scan uri %q, decode uri %q", body, got.uri, uri)
	}
}

// TestPeekMessageFindsTheURIInAnyMemberOrder: Neovim builds a message from a
// Lua table, so params can come before method; the URI must still be found,
// or the unsaved text of the document is not replayed.
func TestPeekMessageFindsTheURIInAnyMemberOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 2000; i++ {
		method := []string{"textDocument/didOpen", "textDocument/didChange", "textDocument/didClose"}[rng.Intn(3)]
		doc := map[string]any{"uri": fmt.Sprintf("file:///p/a%d \\\"x\\\".ex", i), "version": i, "text": `x "y" {z}`}
		params := map[string]any{"textDocument": doc, "contentChanges": []any{map[string]any{"text": "}\"{"}}}
		members := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
		body := shuffledObject(rng, members, func(v any) []byte {
			if m, ok := v.(map[string]any); ok {
				return shuffledObject(rng, m, nil)
			}
			b, _ := json.Marshal(v)
			return b
		})
		got := peekMessage(body)
		if got.method != method || got.uri != doc["uri"] {
			t.Fatalf("%s: method %q uri %q", body, got.method, got.uri)
		}
		checkPeek(t, body)
	}
}

// shuffledObject writes members in a random order, with values written by
// value (or json.Marshal when value is nil).
func shuffledObject(rng *rand.Rand, members map[string]any, value func(any) []byte) []byte {
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	rng.Shuffle(len(keys), func(a, b int) { keys[a], keys[b] = keys[b], keys[a] })
	var b bytes.Buffer
	b.WriteString("{")
	for j, k := range keys {
		if j > 0 {
			b.WriteString(",")
		}
		key, _ := json.Marshal(k)
		b.Write(key)
		b.WriteString(":")
		if value != nil {
			b.Write(value(members[k]))
		} else {
			v, _ := json.Marshal(members[k])
			b.Write(v)
		}
	}
	b.WriteString("}")
	return b.Bytes()
}

// FuzzPeekMessage: the scan must not panic on any input, and on valid JSON it
// must agree with encoding/json.
func FuzzPeekMessage(f *testing.F) {
	for _, seed := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"textDocument/hover","params":{}}`,
		`{"params":{"textDocument":{"uri":"file:///a.ex"}},"method":"textDocument/didOpen"}`,
		`{"jsonrpc":"2.0","result":{"x":[1,"}"]},"id":"a"}`,
		`{"method":"textDocument/didChange","params":{"textDocument":{"uri"`,
		`{"method":"textDocument/didOpen","params":{"textDocument":{"uri":`,
		`{"id":null,"error":{"code":1}}`,
		`{"a":"\\\"","method":"xA"}`,
		`{`, `"`, `[]`, ``,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		checkPeek(t, body)
	})
}
