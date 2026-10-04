package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxProtocolLine = 16 << 20

type hello struct {
	// Contract is the frontend's workspace contract version. It travels with the
	// handshake so a build that cannot share this workspace's index or protocol
	// with a running daemon makes that daemon step aside.
	Contract int    `json:"contract"`
	Kind     string `json:"kind"`
	// Root is the absolute workspace spelling used by this frontend. Identity
	// proves physical ownership; Root must also match because index paths and LSP
	// document URIs are spelling-sensitive.
	Root string `json:"root"`
	// Identity is the symlink-resolved workspace path, so aliases reach the same
	// ownership endpoint before Root validation rejects mixed path spellings.
	Identity string `json:"identity"`
	// Session names an editor session this connection explicitly attaches to.
	// Empty means the workspace's headless language service.
	Session string `json:"session,omitempty"`
}

type helloResponse struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Contract int    `json:"contract"`
	PID      int    `json:"pid,omitempty"`
	Root     string `json:"root,omitempty"`
	// Session is the id assigned to an LSP connection. A frontend that wants
	// that editor's document overlay sends it back in a later handshake.
	Session string `json:"session,omitempty"`
	// Incompatible marks a refusal caused by version drift rather than a bad
	// request: the peer should replace this daemon instead of fixing its input.
	Incompatible bool `json:"incompatible,omitempty"`
	// Exiting reports that the daemon is already shutting itself down because
	// the peer is newer, so the peer only has to wait for its lock.
	Exiting bool `json:"exiting,omitempty"`
}

type request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// notification is a server-initiated message: a method with no id.
type notification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// envelope decodes either direction so a client can separate a notification
// from a response without parsing the payload twice.
type envelope struct {
	ID     uint64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

const (
	kindControl = "control"
	kindLSP     = "lsp"
)

// NotificationChanged is pushed to control connections subscribed with
// workspace/watch.
const NotificationChanged = "workspace/changed"

func readJSONLine(r *bufio.Reader, dst any) error {
	// Read through the connection's own reader and cap the accumulation. A
	// second buffered reader over r would look simpler but could pull the next
	// message into its private buffer and lose it on return.
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > maxProtocolLine {
			return fmt.Errorf("daemon protocol line exceeds %d bytes", maxProtocolLine)
		}
		line = append(line, chunk...)
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
	if err := json.Unmarshal(line, dst); err != nil {
		return fmt.Errorf("decode daemon protocol: %w", err)
	}
	return nil
}

// errLineTooLarge reports a message the peer's reader would refuse. It is
// returned before anything is written, so the stream stays in step.
var errLineTooLarge = errors.New("daemon protocol line too large")

func writeJSONLine(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	// readJSONLine counts the newline against the limit.
	if len(data)+1 > maxProtocolLine {
		return fmt.Errorf("%w: %d bytes, limit %d", errLineTooLarge, len(data)+1, maxProtocolLine)
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}
