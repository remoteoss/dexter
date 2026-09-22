package daemon

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// A message that is long but legal must decode even when it spans several reads
// of the buffered reader, and the reader must keep the following message.
func TestReadJSONLineKeepsFollowingMessage(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(`{"id":1}` + "\n")
	buf.WriteString(`{"id":2}` + "\n")
	// The small buffer forces ReadSlice to return ErrBufferFull chunks for the
	// long first message below rather than one complete line.
	reader := bufio.NewReaderSize(&buf, 8)

	var first, second struct {
		ID int `json:"id"`
	}
	if err := readJSONLine(reader, &first); err != nil {
		t.Fatalf("first line: %v", err)
	}
	if err := readJSONLine(reader, &second); err != nil {
		t.Fatalf("second line: %v", err)
	}
	if first.ID != 1 || second.ID != 2 {
		t.Fatalf("decoded ids %d, %d; want 1, 2", first.ID, second.ID)
	}
}

// A long payload under the limit must reassemble from ErrBufferFull chunks.
func TestReadJSONLineAssemblesLongLine(t *testing.T) {
	payload := strings.Repeat("x", 32*1024)
	reader := bufio.NewReaderSize(strings.NewReader(`{"pad":"`+payload+`"}`+"\n"), 64)

	var message struct {
		Pad string `json:"pad"`
	}
	if err := readJSONLine(reader, &message); err != nil {
		t.Fatalf("long line: %v", err)
	}
	if message.Pad != payload {
		t.Fatalf("pad length = %d, want %d", len(message.Pad), len(payload))
	}
}

// A peer that never sends a newline must be rejected at the limit instead of
// letting the daemon allocate without bound.
func TestReadJSONLineRejectsOversizedLine(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", maxProtocolLine+1)), 4096)

	var message map[string]any
	err := readJSONLine(reader, &message)
	if err == nil {
		t.Fatal("oversized line was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want it to report the line limit", err)
	}
}
