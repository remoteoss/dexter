package notify_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/notify/notifytest"
)

const wait = 5 * time.Second

func quiet(r *notify.Reporter) *notify.Reporter {
	r.SetLogf(func(string, ...any) {})
	return r
}

func TestSetShowsOnceAndLogs(t *testing.T) {
	r := notify.New()
	var mu sync.Mutex
	var logged []string
	r.SetLogf(func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, strings.TrimSpace(sprintf(format, args...)))
	})
	client := notifytest.New()
	detach := r.Attach(client, false)
	defer detach()

	r.Set("index.rebuild", notify.Warning, "Dexter: rebuilding (1 file)")
	// The same severity again only updates the text: no second message.
	r.Set("index.rebuild", notify.Warning, "Dexter: rebuilding (2 files)")
	r.Set("index.unavailable", notify.Error, "Dexter: the index is broken")

	client.WaitMessage(t, wait, protocol.MessageTypeError, "the index is broken")
	messages := client.Messages()
	if len(messages) != 2 {
		t.Fatalf("got %d messages, want 2:\n%s", len(messages), client.Dump())
	}
	if messages[0].Type != protocol.MessageTypeWarning || messages[0].Message != "Dexter: rebuilding (1 file)" {
		t.Fatalf("first message = %v", messages[0])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 2 || logged[0] != "Warning: rebuilding (1 file)" || logged[1] != "Error: the index is broken" {
		t.Fatalf("log = %q", logged)
	}
}

func TestClearSaysThatTheConditionStopped(t *testing.T) {
	r := quiet(notify.New())
	client := notifytest.New()
	defer r.Attach(client, false)()

	if r.Clear("watcher", "Dexter: watching again") {
		t.Fatal("Clear of a condition that is not active reported true")
	}
	r.Set("watcher", notify.Warning, "Dexter: cannot watch")
	if !r.Clear("watcher", "Dexter: watching again") {
		t.Fatal("Clear of an active condition reported false")
	}
	client.WaitMessage(t, wait, protocol.MessageTypeInfo, "watching again")
	if r.Active("watcher") || len(r.Conditions()) != 0 {
		t.Fatalf("condition is still active: %+v", r.Conditions())
	}
	if n := len(client.Messages()); n != 2 {
		t.Fatalf("got %d messages, want 2:\n%s", n, client.Dump())
	}
}

// An editor that attaches after a condition started still has to learn about
// it, and about work that is in progress.
func TestAttachReplaysConditionsAndWorkInProgress(t *testing.T) {
	r := quiet(notify.New())
	r.Set("root", notify.Warning, "Dexter: not a project")
	r.Set("index.rebuild", notify.Warning, "Dexter: rebuilding")
	r.Set("gone", notify.Warning, "Dexter: gone")
	r.Clear("gone", "")
	task := r.Begin("index.build", "Dexter: indexing", "12 files", false)

	late := notifytest.New()
	defer r.Attach(late, true)()
	late.WaitFor(t, wait, "replayed progress begin", func(e notifytest.Event) bool {
		return e.Method == protocol.MethodProgress && e.Kind == "begin" && e.Title == "Dexter: indexing" && e.Message == "12 files"
	})
	messages := late.Messages()
	if len(messages) != 2 || messages[0].Message != "Dexter: not a project" || messages[1].Message != "Dexter: rebuilding" {
		t.Fatalf("replayed messages in the wrong order or count:\n%s", late.Dump())
	}

	task.Report("40 files", -1)
	task.End("Dexter: indexed 40 files")
	late.WaitFor(t, wait, "progress end", func(e notifytest.Event) bool {
		return e.Method == protocol.MethodProgress && e.Kind == "end" && e.Message == "Dexter: indexed 40 files"
	})
	events := late.Events()
	var kinds []string
	for _, e := range events {
		if e.Method == protocol.MethodProgress {
			kinds = append(kinds, e.Kind)
		}
		if e.Method == protocol.MethodWorkDoneProgressCreate && e.Token == "" {
			t.Fatal("progress token is empty")
		}
	}
	if strings.Join(kinds, ",") != "begin,report,end" {
		t.Fatalf("progress kinds = %v:\n%s", kinds, late.Dump())
	}

	// Ended work is not replayed.
	later := notifytest.New()
	defer r.Attach(later, true)()
	later.WaitMessage(t, wait, protocol.MessageTypeWarning, "rebuilding")
	time.Sleep(20 * time.Millisecond)
	for _, e := range later.Events() {
		if e.Method != protocol.MethodWindowShowMessage {
			t.Fatalf("ended work was replayed:\n%s", later.Dump())
		}
	}
}

// A client without work-done progress gets a message at the start and at the
// end, but only when the work asks for that fallback.
func TestProgressFallsBackToMessages(t *testing.T) {
	r := quiet(notify.New())
	plain := notifytest.New()
	defer r.Attach(plain, false)()
	refusing := notifytest.New()
	refusing.RefuseProgress = true
	defer r.Attach(refusing, true)()

	task := r.Begin("index.reconcile", "Dexter: updating the index", "1000 changed files", true)
	task.Report("2000 changed files", -1)
	task.End("Dexter: updated 2400 files in 3s")
	silent := r.Begin("index.build", "Dexter: indexing", "", false)
	silent.End("Dexter: built")

	for _, client := range []*notifytest.Client{plain, refusing} {
		client.WaitMessage(t, wait, protocol.MessageTypeInfo, "updated 2400 files")
		messages := client.Messages()
		if len(messages) != 2 || messages[0].Message != "Dexter: updating the index: 1000 changed files" {
			t.Fatalf("fallback messages:\n%s", client.Dump())
		}
		for _, e := range client.Events() {
			if e.Method == protocol.MethodProgress {
				t.Fatalf("progress sent to a client that cannot show it:\n%s", client.Dump())
			}
		}
	}
}

func TestNilReporterIsSafe(t *testing.T) {
	var r *notify.Reporter
	r.Set("a", notify.Error, "x")
	r.Clear("a", "y")
	r.Notify(notify.Info, "z")
	task := r.Begin("b", "t", "m", true)
	task.Report("m", 1)
	task.End("e")
	r.Attach(notifytest.New(), true)()
	if r.Active("a") || r.Conditions() != nil {
		t.Fatal("nil reporter kept state")
	}
}

func TestDetachStopsDelivery(t *testing.T) {
	r := quiet(notify.New())
	client := notifytest.New()
	detach := r.Attach(client, false)
	r.Notify(notify.Warning, "Dexter: first")
	client.WaitMessage(t, wait, protocol.MessageTypeWarning, "first")
	detach()
	detach()
	r.Notify(notify.Warning, "Dexter: second")
	time.Sleep(20 * time.Millisecond)
	if n := len(client.Messages()); n != 1 {
		t.Fatalf("detached client received %d messages", n)
	}
}

func TestSummarize(t *testing.T) {
	cases := map[string][]string{
		"":                 nil,
		"/a.ex":            {"/a.ex"},
		"/a.ex and 2 more": {"/a.ex", "/b.ex", "/c.ex"},
	}
	for want, paths := range cases {
		if got := notify.Summarize(paths); got != want {
			t.Errorf("Summarize(%v) = %q, want %q", paths, got, want)
		}
	}
}

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
