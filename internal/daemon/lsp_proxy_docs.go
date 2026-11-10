package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// proxyDoc is one open document as the proxy keeps it for a replay: one copy
// of its text. open is the editor's didOpen until the first change; change is
// then the last full didChange (the editor's own message, or one the proxy
// made after it applied range edits), and languageID is kept from the didOpen.
type proxyDoc struct {
	seq        uint64
	open       []byte
	change     []byte
	languageID string
	// changed is true when the editor changed the buffer after its open or
	// its last save. The daemon reads a buffer that a didOpen set as saved,
	// so a replay must also say that it has unsaved changes, or MCP tools and
	// the CLI would read the disk text instead.
	changed bool
}

type docMessage struct {
	Params struct {
		TextDocument struct {
			URI        string          `json:"uri"`
			LanguageID string          `json:"languageId"`
			Version    json.RawMessage `json:"version"`
			Text       *string         `json:"text"`
		} `json:"textDocument"`
		ContentChanges []contentChange `json:"contentChanges"`
	} `json:"params"`
}

type contentChange struct {
	Range *struct {
		Start position `json:"start"`
		End   position `json:"end"`
	} `json:"range"`
	Text string `json:"text"`
}

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

func parseDocMessage(frame []byte) (docMessage, error) {
	var msg docMessage
	_, body, ok := splitFrame(frame)
	if !ok {
		return msg, errors.New("no LSP frame")
	}
	err := json.Unmarshal(body, &msg)
	return msg, err
}

// text returns the document's current text, version, and URI.
func (d *proxyDoc) text() (text, uri string, version json.RawMessage, err error) {
	if d.change != nil {
		msg, err := parseDocMessage(d.change)
		if err != nil || len(msg.Params.ContentChanges) == 0 {
			return "", "", nil, errors.New("bad stored didChange")
		}
		changes := msg.Params.ContentChanges
		return changes[len(changes)-1].Text, msg.Params.TextDocument.URI, msg.Params.TextDocument.Version, nil
	}
	msg, err := parseDocMessage(d.open)
	if err != nil || msg.Params.TextDocument.Text == nil {
		return "", "", nil, errors.New("bad stored didOpen")
	}
	return *msg.Params.TextDocument.Text, msg.Params.TextDocument.URI, msg.Params.TextDocument.Version, nil
}

// applyChange records one didChange. Dexter's server asks for full document
// sync, so a change normally carries the whole text and the proxy keeps the
// message as it is, without a parse. A change with a range is applied to the
// proxy's copy, so that a replay never sends stale text. The quoted "range"
// key cannot appear unescaped inside the text, so a message without it has
// no range.
func (d *proxyDoc) applyChange(frame []byte, body int) bool {
	if d.open != nil && d.languageID == "" {
		if msg, err := parseDocMessage(d.open); err == nil {
			d.languageID = msg.Params.TextDocument.LanguageID
		}
	}
	if !bytes.Contains(frame[body:], []byte(`"range"`)) {
		d.change, d.open, d.changed = frame, nil, true
		return true
	}
	msg, err := parseDocMessage(frame)
	if err != nil {
		return false
	}
	text, uri, _, err := d.text()
	if err != nil {
		return false
	}
	for _, change := range msg.Params.ContentChanges {
		if change.Range == nil {
			text = change.Text
			continue
		}
		start := utf16Offset(text, change.Range.Start)
		end := utf16Offset(text, change.Range.End)
		if end < start {
			start, end = end, start
		}
		text = text[:start] + change.Text + text[end:]
	}
	full, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didChange",
		"params": map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": msg.Params.TextDocument.Version},
			"contentChanges": []any{map[string]any{"text": text}},
		},
	})
	if err != nil {
		return false
	}
	d.change, d.open, d.changed = frameMessage(full), nil, true
	return true
}

// replayFrames are the messages that open the document on a new daemon with
// its current text: a didOpen, and for a buffer with unsaved changes the last
// full didChange, so that the new daemon also knows the buffer is unsaved.
func (d *proxyDoc) replayFrames() ([][]byte, error) {
	open, err := d.replayFrame()
	if err != nil {
		return nil, err
	}
	if d.changed && d.change != nil {
		return [][]byte{open, d.change}, nil
	}
	return [][]byte{open}, nil
}

// replayFrame is the didOpen that opens the document on a new daemon with
// its current text.
func (d *proxyDoc) replayFrame() ([]byte, error) {
	if d.change == nil {
		return d.open, nil
	}
	text, uri, version, err := d.text()
	if err != nil {
		return nil, err
	}
	if len(version) == 0 {
		version = json.RawMessage("0")
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didOpen",
		"params": map[string]any{
			"textDocument": map[string]any{"uri": uri, "languageId": d.languageID, "version": version, "text": text},
		},
	})
	if err != nil {
		return nil, err
	}
	return frameMessage(body), nil
}

// utf16Offset converts an LSP position to a byte offset in text, by the LSP
// rules: lines end at "\n", "\r\n", or "\r"; character counts UTF-16 code
// units; a character past the end of the line means the end of the line,
// before its line end; a line past the end of the text means the end of the
// text. A character inside a surrogate pair stops before that character, so
// an edit never splits it.
func utf16Offset(text string, pos position) int {
	offset := 0
	for line := 0; line < pos.Line; line++ {
		end := strings.IndexAny(text[offset:], "\r\n")
		if end < 0 {
			return len(text)
		}
		offset += end
		if text[offset] == '\r' && offset+1 < len(text) && text[offset+1] == '\n' {
			offset += 2
		} else {
			offset++
		}
	}
	units := 0
	for offset < len(text) {
		r, size := utf8.DecodeRuneInString(text[offset:])
		if r == '\n' || r == '\r' {
			break
		}
		n := utf16.RuneLen(r)
		if n < 0 {
			n = 1
		}
		if units+n > pos.Character {
			break
		}
		units += n
		offset += size
	}
	return offset
}
