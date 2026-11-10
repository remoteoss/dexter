package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// The LSP proxy must know a few facts about each message it forwards: the
// method, the id, and whether it is a response; for document notifications
// also the document URI. Decoding every message would cost a full JSON parse
// per keystroke on a large file, so these functions scan only as far as the
// facts are, and skip strings with a byte search.

// maxProxyFrame bounds one LSP message that the proxy reads. It is far above
// any real message; it exists so that a damaged header cannot make the proxy
// allocate without limit.
const maxProxyFrame = 512 << 20

// readProxyFrame reads one LSP message and returns it framed again as
// "Content-Length: N\r\n\r\n" + body in one slice, so that it is written with
// one call, and the offset of the body in that slice.
func readProxyFrame(r *bufio.Reader) (frame []byte, body int, err error) {
	return readProxyFrameInto(r, nil)
}

// readProxyFrameInto is readProxyFrame that reuses buf when it is large
// enough. The caller must not keep the frame past the next read.
func readProxyFrameInto(r *bufio.Reader, buf []byte) (frame []byte, body int, err error) {
	length := -1
	sawHeader := false
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 && !sawHeader {
				return nil, 0, io.EOF
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				return nil, 0, errors.New("LSP header line too long")
			}
			if errors.Is(err, io.EOF) {
				return nil, 0, io.ErrUnexpectedEOF
			}
			return nil, 0, err
		}
		sawHeader = true
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			break
		}
		name, value, ok := bytes.Cut(line, []byte(":"))
		if ok && bytes.EqualFold(bytes.TrimSpace(name), []byte("Content-Length")) {
			n, convErr := strconv.Atoi(string(bytes.TrimSpace(value)))
			if convErr != nil {
				return nil, 0, fmt.Errorf("bad Content-Length %q", value)
			}
			length = n
		}
	}
	if length < 0 || length > maxProxyFrame {
		return nil, 0, fmt.Errorf("bad LSP message length %d", length)
	}
	header := "Content-Length: " + strconv.Itoa(length) + "\r\n\r\n"
	if n := len(header) + length; cap(buf) >= n {
		frame = buf[:n]
	} else {
		frame = make([]byte, n)
	}
	copy(frame, header)
	if _, err := io.ReadFull(r, frame[len(header):]); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, 0, err
	}
	return frame, len(header), nil
}

// frameMessage frames a body for the wire.
func frameMessage(body []byte) []byte {
	header := "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	frame := make([]byte, len(header)+len(body))
	copy(frame, header)
	copy(frame[len(header):], body)
	return frame
}

// peeked is what the proxy knows about one message.
type peeked struct {
	method string
	// id is the raw JSON of the id, or nil when there is none.
	id []byte
	// response is true for a message with a result or an error.
	response bool
	// uri is the textDocument.uri of a document notification.
	uri string
}

func (p peeked) isRequest() bool  { return p.method != "" && p.id != nil }
func (p peeked) isResponse() bool { return p.method == "" && p.id != nil }

// notificationMethods are the client-to-server methods that the protocol
// defines as notifications. They carry no id, so the scan can stop at their
// params.
var notificationMethods = map[string]bool{
	"initialized":                         true,
	"exit":                                true,
	"textDocument/didOpen":                true,
	"textDocument/didChange":              true,
	"textDocument/didClose":               true,
	"textDocument/didSave":                true,
	"textDocument/willSave":               true,
	"workspace/didChangeConfiguration":    true,
	"workspace/didChangeWatchedFiles":     true,
	"workspace/didChangeWorkspaceFolders": true,
	"workspace/didCreateFiles":            true,
	"workspace/didRenameFiles":            true,
	"workspace/didDeleteFiles":            true,
	"$/cancelRequest":                     true,
	"$/setTrace":                          true,
	"$/progress":                          true,
	"window/workDoneProgress/cancel":      true,
}

// documentMethods are the notifications whose document URI the proxy keeps.
var documentMethods = map[string]bool{
	"textDocument/didOpen":   true,
	"textDocument/didChange": true,
	"textDocument/didClose":  true,
	"textDocument/didSave":   true,
}

// peekMessage scans body for what the proxy needs. An unexpected shape gives
// an empty result, which the proxy forwards without keeping anything. The
// members can come in any order: an editor that builds the message from a
// hash table can put params before method.
func peekMessage(body []byte) peeked {
	p, paramsAt := peekMembers(body)
	if p.uri == "" && paramsAt >= 0 && documentMethods[p.method] {
		p.uri = findDocumentURI(body, paramsAt)
	}
	return p
}

// peekMembers is peekMessage without the URI of a document message whose
// params came before its method; it returns where those params start, or -1.
func peekMembers(body []byte) (peeked, int) {
	var p peeked
	paramsAt := -1
	i := skipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return p, -1
	}
	i++
	for {
		i = skipSpace(body, i)
		if i >= len(body) || body[i] == '}' {
			return p, paramsAt
		}
		keyStart := i
		i = skipString(body, i)
		if i < 0 {
			return peeked{}, -1
		}
		key := body[keyStart+1 : i-1]
		i = skipSpace(body, i)
		if i >= len(body) || body[i] != ':' {
			return peeked{}, -1
		}
		i = skipSpace(body, i+1)
		valueStart := i
		switch string(key) {
		case "id":
			i = skipValue(body, i)
			if i < 0 {
				return peeked{}, -1
			}
			if raw := bytes.TrimSpace(body[valueStart:i]); !bytes.Equal(raw, []byte("null")) {
				p.id = raw
			}
		case "method":
			i = skipString(body, i)
			if i < 0 {
				return peeked{}, -1
			}
			p.method = unquote(body[valueStart:i])
		case "result", "error":
			p.response = true
			if p.id != nil {
				return p, paramsAt
			}
			if id, ok := tailID(body); ok {
				p.id = id
				return p, paramsAt
			}
			i = skipValue(body, i)
		case "params":
			paramsAt = valueStart
			if p.method != "" {
				if documentMethods[p.method] {
					p.uri = findDocumentURI(body, i)
				}
				if p.id != nil || notificationMethods[p.method] {
					return p, paramsAt
				}
				if id, ok := tailID(body); ok {
					p.id = id
					return p, paramsAt
				}
			}
			i = skipValue(body, i)
		default:
			i = skipValue(body, i)
		}
		if i < 0 {
			return peeked{}, -1
		}
		i = skipSpace(body, i)
		if i < len(body) && body[i] == ',' {
			i++
		}
	}
}

// tailID reads the id when it is the last member of the message, which is
// where the daemon's JSON-RPC encoder puts it after a result or params of any
// size. A quote cannot appear unescaped in a string, so the last `"id"` in
// valid JSON is a key; it is the top-level one only when a single `}` follows
// its value, because a nested object would close at least twice.
func tailID(body []byte) ([]byte, bool) {
	at := bytes.LastIndex(body, []byte(`"id"`))
	if at < 0 {
		return nil, false
	}
	i := skipSpace(body, at+4)
	if i >= len(body) || body[i] != ':' {
		return nil, false
	}
	i = skipSpace(body, i+1)
	start := i
	end := skipValue(body, i)
	if end < 0 {
		return nil, false
	}
	rest := skipSpace(body, end)
	if rest >= len(body) || body[rest] != '}' || skipSpace(body, rest+1) != len(body) {
		return nil, false
	}
	raw := bytes.TrimSpace(body[start:end])
	if bytes.Equal(raw, []byte("null")) {
		return nil, false
	}
	return raw, true
}

// findDocumentURI returns params.textDocument.uri, scanning from the start of
// the params value.
func findDocumentURI(body []byte, i int) string {
	i = descend(body, i, "textDocument")
	if i < 0 {
		return ""
	}
	i = descend(body, i, "uri")
	if i < 0 || i >= len(body) || body[i] != '"' {
		return ""
	}
	end := skipString(body, i)
	if end < 0 {
		return ""
	}
	return unquote(body[i:end])
}

// descend returns the offset of the value of key in the object that starts at
// i, or -1.
func descend(body []byte, i int, key string) int {
	if i >= len(body) || body[i] != '{' {
		return -1
	}
	i++
	for {
		i = skipSpace(body, i)
		if i >= len(body) || body[i] == '}' {
			return -1
		}
		keyStart := i
		i = skipString(body, i)
		if i < 0 {
			return -1
		}
		name := body[keyStart+1 : i-1]
		i = skipSpace(body, i)
		if i >= len(body) || body[i] != ':' {
			return -1
		}
		i = skipSpace(body, i+1)
		if string(name) == key {
			return i
		}
		i = skipValue(body, i)
		if i < 0 {
			return -1
		}
		i = skipSpace(body, i)
		if i < len(body) && body[i] == ',' {
			i++
		}
	}
}

func unquote(raw []byte) string {
	if len(raw) < 2 {
		return ""
	}
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1 : len(raw)-1])
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func skipSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

// skipString returns the offset after the string that starts at i, or -1.
func skipString(b []byte, i int) int {
	if i >= len(b) || b[i] != '"' {
		return -1
	}
	i++
	for {
		q := bytes.IndexByte(b[i:], '"')
		if q < 0 {
			return -1
		}
		i += q
		backslashes := 0
		for j := i - 1; j >= 0 && b[j] == '\\'; j-- {
			backslashes++
		}
		i++
		if backslashes%2 == 0 {
			return i
		}
	}
}

// skipValue returns the offset after the JSON value that starts at i, or -1.
func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			n := bytes.IndexAny(b[i:], "\"{}[]")
			if n < 0 {
				return -1
			}
			i += n
			switch b[i] {
			case '"':
				i = skipString(b, i)
				if i < 0 {
					return -1
				}
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			i++
			if depth == 0 {
				return i
			}
		}
		return -1
	default:
		for i < len(b) {
			switch b[i] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return i
			}
			i++
		}
		return i
	}
}
