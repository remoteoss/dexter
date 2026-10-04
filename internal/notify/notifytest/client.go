// Package notifytest has a fake LSP client that records what Dexter shows to
// the user, for tests of the notify path.
package notifytest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/protocol"
)

// Event is one thing the fake client received.
type Event struct {
	// Method is window/showMessage, window/workDoneProgress/create, or
	// $/progress.
	Method string
	// Type is the showMessage type.
	Type protocol.MessageType
	// Kind is the progress kind: begin, report, or end.
	Kind string
	// Token is the progress token.
	Token string
	// Title is the progress title of a begin.
	Title string
	// Message is the showMessage text or the progress message.
	Message string
}

func (e Event) String() string {
	switch e.Method {
	case protocol.MethodWindowShowMessage:
		return fmt.Sprintf("showMessage(%s): %s", e.Type, e.Message)
	case protocol.MethodProgress:
		return fmt.Sprintf("progress %s %s: %s %s", e.Kind, e.Token, e.Title, e.Message)
	default:
		return fmt.Sprintf("%s %s", e.Method, e.Token)
	}
}

// Client records showMessage, workDoneProgress/create, and $/progress. Other
// methods panic through the nil embedded interface, so a test finds out when
// Dexter sends something it did not expect.
type Client struct {
	protocol.Client

	// RefuseProgress makes window/workDoneProgress/create fail, as an editor
	// can do.
	RefuseProgress bool

	mu     sync.Mutex
	events []Event
	signal chan struct{}
}

// New returns an empty recording client.
func New() *Client { return &Client{signal: make(chan struct{}, 1)} }

func (c *Client) record(e Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

func (c *Client) ShowMessage(_ context.Context, params *protocol.ShowMessageParams) error {
	c.record(Event{Method: protocol.MethodWindowShowMessage, Type: params.Type, Message: params.Message})
	return nil
}

func (c *Client) LogMessage(context.Context, *protocol.LogMessageParams) error { return nil }

func (c *Client) WorkDoneProgressCreate(_ context.Context, params *protocol.WorkDoneProgressCreateParams) error {
	c.record(Event{Method: protocol.MethodWorkDoneProgressCreate, Token: params.Token.String()})
	if c.RefuseProgress {
		return fmt.Errorf("progress refused")
	}
	return nil
}

func (c *Client) Progress(_ context.Context, params *protocol.ProgressParams) error {
	raw, err := json.Marshal(params.Value)
	if err != nil {
		return err
	}
	var value struct {
		Kind    string `json:"kind"`
		Title   string `json:"title"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	c.record(Event{Method: protocol.MethodProgress, Token: params.Token.String(), Kind: value.Kind, Title: value.Title, Message: value.Message})
	return nil
}

func (c *Client) RegisterCapability(context.Context, *protocol.RegistrationParams) error { return nil }

// Events returns a copy of what was received so far.
func (c *Client) Events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.events...)
}

// Messages returns the showMessage events.
func (c *Client) Messages() []Event {
	var out []Event
	for _, e := range c.Events() {
		if e.Method == protocol.MethodWindowShowMessage {
			out = append(out, e)
		}
	}
	return out
}

// WaitFor waits until match accepts one event and returns it. It fails the
// test after timeout and lists what arrived.
func (c *Client) WaitFor(t testing.TB, timeout time.Duration, what string, match func(Event) bool) Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		for _, e := range c.Events() {
			if match(e) {
				return e
			}
		}
		select {
		case <-c.signal:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("no %s; received:\n%s", what, c.Dump())
			return Event{}
		}
	}
}

// WaitMessage waits for a showMessage of the given type that contains text.
func (c *Client) WaitMessage(t testing.TB, timeout time.Duration, typ protocol.MessageType, text string) Event {
	t.Helper()
	return c.WaitFor(t, timeout, fmt.Sprintf("showMessage(%s) containing %q", typ, text), func(e Event) bool {
		return e.Method == protocol.MethodWindowShowMessage && e.Type == typ && strings.Contains(e.Message, text)
	})
}

// Dump lists every event, one on each line.
func (c *Client) Dump() string {
	var b strings.Builder
	for _, e := range c.Events() {
		b.WriteString("  ")
		b.WriteString(e.String())
		b.WriteByte('\n')
	}
	return b.String()
}
