package lsp

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.lsp.dev/protocol"
)

// clientLogBuffer is how many lines may wait for a slow editor before new ones
// are dropped. Debug logging is bursty, a few dozen lines per request.
const clientLogBuffer = 512

// clientLog forwards one editor session's log lines to that editor as
// window/logMessage, so they appear in the editor's own log (Neovim's lsp.log,
// VS Code's Output panel) as well as in the daemon's. Every session in the
// daemon has its own, so an editor sees only its own requests; the headless
// service behind CLI and MCP calls has none.
//
// Sending never blocks the caller: a request must not wait on an editor that is
// slow to read, so lines beyond the buffer are dropped and the editor is told
// how many were lost.
type clientLog struct {
	lines   chan string
	dropped atomic.Int64
}

func startClientLog(client protocol.Client, done <-chan struct{}) *clientLog {
	l := &clientLog{lines: make(chan string, clientLogBuffer)}
	go func() {
		for {
			select {
			case <-done:
				return
			case line := <-l.lines:
				if n := l.dropped.Swap(0); n > 0 {
					l.deliver(client, fmt.Sprintf("[dexter] %d log lines dropped while the editor was slow to read", n))
				}
				l.deliver(client, line)
			}
		}
	}()
	return l
}

func (l *clientLog) deliver(client protocol.Client, line string) {
	_ = client.LogMessage(context.Background(), &protocol.LogMessageParams{Type: protocol.MessageTypeLog, Message: line})
}

// send queues one line for the editor. It is safe on a nil clientLog, which is
// what a session without an editor has.
func (l *clientLog) send(line string) {
	if l == nil {
		return
	}
	select {
	case l.lines <- line:
	default:
		l.dropped.Add(1)
	}
}
