package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/remoteoss/dexter/internal/version"
)

// startupFailureWait bounds how long a proxy that cannot serve waits for the
// editor's initialize request. An editor sends it at once; the bound only keeps
// a proxy that nobody talks to from staying alive.
var startupFailureWait = 10 * time.Second

// lspInternalError is the JSON-RPC code of the initialize error. The LSP has no
// code for "this server cannot start", and InternalError is what editors show
// as a server failure.
const lspInternalError = -32603

// LSPStartupError is a failure to serve an editor. The editor already received
// Message, as the answer to its initialize request and as an error message, so
// the caller only has to exit.
type LSPStartupError struct {
	Message string
	Err     error
}

func (e *LSPStartupError) Error() string { return e.Message }
func (e *LSPStartupError) Unwrap() error { return e.Err }

// ExplainStartupFailure turns a failure to reach the workspace daemon into a
// message for the user: what happened, and what to do about it.
func ExplainStartupFailure(root string, err error) string {
	var incompatible *IncompatibleDaemonError
	var mismatch *RootMismatchError
	var message string
	switch {
	case errors.As(err, &incompatible) && !incompatible.ClientNewer:
		message = fmt.Sprintf(
			"Dexter cannot start for %s: this editor started an older Dexter build (%s, daemon contract %d) than the daemon that serves the project (contract %d). Restart this editor so that it starts the current dexter binary; check which dexter the PATH of the editor finds.",
			root, version.Version, ContractVersion, incompatible.DaemonContract)
	case errors.As(err, &incompatible):
		pid := "its process"
		if incompatible.DaemonPID > 0 {
			pid = fmt.Sprintf("pid %d", incompatible.DaemonPID)
		}
		message = fmt.Sprintf(
			"Dexter cannot start for %s: an older Dexter daemon (%s, contract %d) serves the project and could not be replaced (%v). Run `dexter stop --force` in the project directory, then restart this editor.",
			root, pid, incompatible.DaemonContract, err)
	case errors.As(err, &mismatch):
		message = fmt.Sprintf(
			"Dexter cannot start for %s: a Dexter daemon already serves this project as %s, and one project cannot be served through two path spellings. Open the project as %s, or run `dexter stop --force` in %s and restart this editor.",
			mismatch.Requested, mismatch.Daemon, mismatch.Daemon, mismatch.Daemon)
	default:
		message = fmt.Sprintf("Dexter cannot start for %s: %v.", root, err)
		if strings.Contains(err.Error(), "not serving its daemon socket") {
			message += " Wait until it finishes, then restart this editor. If no `dexter init` runs, `dexter stop --force` in the project directory stops the process that holds the project."
		} else {
			message += " Restart this editor to try again; if it fails again, `dexter stop --force` in the project directory stops a daemon that does not answer."
		}
		if tail := daemonLogTail(root, 3); tail != "" {
			message += " Last lines of the daemon log: " + tail
		}
	}
	if endpoint, epErr := ResolveEndpoint(root); epErr == nil {
		message += fmt.Sprintf(" (daemon log: %s)", endpoint.Log)
	}
	return message
}

// daemonLogTail returns the last lines of the daemon log, joined on one line,
// or "" when there is no log. It reads at most the last 4 KiB.
func daemonLogTail(root string, lines int) string {
	endpoint, err := ResolveEndpoint(root)
	if err != nil {
		return ""
	}
	f, err := os.Open(endpoint.Log)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	const window = 4 << 10
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := info.Size() - window
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	var kept []string
	for _, line := range strings.Split(string(buf), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	if offset > 0 && len(kept) > 0 {
		kept = kept[1:] // the first line can be cut
	}
	if len(kept) > lines {
		kept = kept[len(kept)-lines:]
	}
	return strings.Join(kept, " | ")
}

// serveStartupFailure speaks enough LSP to make the editor show why Dexter
// cannot serve it. It reads messages until the initialize request, sends
// window/showMessage with the explanation, answers initialize with an error
// that carries the same text, and returns. Any other request before that gets
// the same error. It also returns on `exit`, at the end of the input, and
// after wait.
func serveStartupFailure(in io.Reader, out io.Writer, message string, wait time.Duration) error {
	type frame struct {
		body []byte
		err  error
	}
	frames := make(chan frame)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		r := bufio.NewReader(in)
		for {
			body, err := readLSPFrame(r)
			select {
			case frames <- frame{body: body, err: err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	timeout := time.NewTimer(wait)
	defer timeout.Stop()
	for {
		var f frame
		select {
		case f = <-frames:
		case <-timeout.C:
			return errors.New("the editor sent no initialize request")
		}
		if f.err != nil {
			if errors.Is(f.err, io.EOF) {
				return nil
			}
			return f.err
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(f.body, &msg); err != nil {
			continue
		}
		hasID := len(msg.ID) > 0 && !bytes.Equal(msg.ID, []byte("null"))
		switch {
		case msg.Method == "exit":
			return nil
		case msg.Method == "initialize" && hasID:
			if err := writeLSPFrame(out, map[string]any{
				"jsonrpc": "2.0",
				"method":  "window/showMessage",
				"params":  map[string]any{"type": 1, "message": message},
			}); err != nil {
				return err
			}
			return writeLSPFrame(out, map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"error": map[string]any{
					"code":    lspInternalError,
					"message": message,
					"data":    map[string]any{"retry": false},
				},
			})
		case hasID:
			if err := writeLSPFrame(out, map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"error":   map[string]any{"code": lspInternalError, "message": message},
			}); err != nil {
				return err
			}
		}
	}
}

// maxStartupFrame bounds one message read by serveStartupFailure. An
// initialize request is a few KiB.
const maxStartupFrame = 16 << 20

func readLSPFrame(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("bad Content-Length %q", value)
			}
			length = n
		}
	}
	if length < 0 || length > maxStartupFrame {
		return nil, fmt.Errorf("bad LSP message length %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func writeLSPFrame(w io.Writer, message any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// failLSPStartup tells the editor why Dexter cannot serve it and returns the
// error for the caller to exit with.
func failLSPStartup(ctx context.Context, root string, err error, in io.Reader, out io.Writer) error {
	if ctx.Err() != nil {
		return err
	}
	message := ExplainStartupFailure(root, err)
	if serveErr := serveStartupFailure(in, out, message, startupFailureWait); serveErr != nil {
		err = errors.Join(err, fmt.Errorf("telling the editor: %w", serveErr))
	}
	return &LSPStartupError{Message: message, Err: err}
}
