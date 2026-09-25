package lsp

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// recordingClient is an editor that records window/logMessage and can be made
// to stop reading. Other client methods are not expected here.
type recordingClient struct {
	protocol.Client
	messages chan string
	release  chan struct{}
}

func (c *recordingClient) LogMessage(_ context.Context, params *protocol.LogMessageParams) error {
	if c.release != nil {
		<-c.release
	}
	c.messages <- params.Message
	return nil
}

func receiveLog(t *testing.T, messages <-chan string) string {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(5 * time.Second):
		t.Fatal("no log message reached the editor")
		return ""
	}
}

func TestClientLogDeliversInOrder(t *testing.T) {
	client := &recordingClient{messages: make(chan string, 8)}
	done := make(chan struct{})
	defer close(done)
	log := startClientLog(client, done)
	log.send("first")
	log.send("second")
	if got := receiveLog(t, client.messages); got != "first" {
		t.Fatalf("first message = %q", got)
	}
	if got := receiveLog(t, client.messages); got != "second" {
		t.Fatalf("second message = %q", got)
	}
}

// An editor that stops reading must never block the request that logs; the
// lines it misses are counted and reported once it reads again.
func TestClientLogDropsInsteadOfBlocking(t *testing.T) {
	client := &recordingClient{messages: make(chan string, clientLogBuffer+8), release: make(chan struct{})}
	done := make(chan struct{})
	defer close(done)
	log := startClientLog(client, done)

	sent := make(chan struct{})
	go func() {
		// One line is held by the blocked editor, the buffer fills, and the
		// rest must be dropped rather than wait.
		for i := 0; i < clientLogBuffer+10; i++ {
			log.send("line")
		}
		close(sent)
	}()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("send blocked on an editor that stopped reading")
	}
	// The drop count is not checked directly: the forwarder may already have
	// taken it for its pending notice. The notice is what the editor sees.
	close(client.release)
	log.send("after")
	for {
		message := receiveLog(t, client.messages)
		if strings.Contains(message, "log lines dropped") {
			return
		}
		if message == "after" {
			t.Fatal("the editor was not told about dropped lines")
		}
	}
}

func TestNilClientLogIgnoresLines(t *testing.T) {
	var log *clientLog
	log.send("nobody is listening") // must not panic
}

// A session served over a stream forwards its debug lines to that editor as
// window/logMessage, which is what puts them in the editor's own log.
func TestSessionDebugLinesReachTheEditor(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	server.debug = true

	serverSide, editorSide := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- ServeStream(server, serverSide) }()

	messages := make(chan string, 16)
	editor := jsonrpc2.NewConn(jsonrpc2.NewStream(editorSide))
	editor.Go(context.Background(), func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		if req.Method() == protocol.MethodWindowLogMessage {
			var params protocol.LogMessageParams
			if err := json.Unmarshal(req.Params(), &params); err == nil && params.Type == protocol.MessageTypeLog {
				messages <- params.Message
			}
		}
		return reply(ctx, nil, nil)
	})
	defer func() {
		_ = editor.Close()
		<-served
	}()

	// ServeStream installs the forwarder before it serves; wait for that
	// instead of racing it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var shutdown json.RawMessage
		if _, err := editor.Call(context.Background(), protocol.MethodShutdown, nil, &shutdown); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session did not answer")
		}
	}
	server.debugf("resolved %s", "MyApp.Repo")
	if got, want := receiveLog(t, messages), "[debug] resolved MyApp.Repo"; got != want {
		t.Fatalf("editor log = %q, want %q", got, want)
	}
}
