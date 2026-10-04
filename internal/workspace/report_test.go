package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/notify/notifytest"
	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
)

const reportWait = 10 * time.Second

func quietReportEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
}

// writeStaleIndex leaves an index with one file in it, stamped with another
// index version.
func writeStaleIndex(t *testing.T, root string, indexVersion int) {
	t.Helper()
	path := writeTestModule(t, root, "lib/old.ex", "SharedLib.Old")
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.IndexFileWithRefs(path, defs, refs); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIndexVersion(indexVersion); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// openHeld opens a runtime and holds it before its first reconciliation until
// the returned release runs.
func openHeld(t *testing.T, root string) (*Runtime, func()) {
	t.Helper()
	hold := make(chan struct{})
	var once atomic.Bool
	release := func() {
		if once.CompareAndSwap(false, true) {
			close(hold)
		}
	}
	rt, err := OpenWithOptions(root, Options{NoWatch: true, BeforeInitialReconcile: func() { <-hold }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		_ = rt.Close()
	})
	return rt, release
}

// The incident this guards: an index written by a newer build was rebuilt for
// minutes, and the editor showed nothing.
func TestNewerIndexVersionIsShownUntilTheRebuildEnds(t *testing.T) {
	quietReportEnv(t)
	root := t.TempDir()
	writeTestModule(t, root, "mix.exs", "SharedLib.MixProject")
	writeStaleIndex(t, root, version.IndexVersion+1)

	rt, release := openHeld(t, root)
	client := notifytest.New()
	defer rt.Reporter().Attach(client, false)()

	want := fmt.Sprintf("Dexter: the index was written by a newer Dexter build (index version %d; this build uses %d). Rebuilding it now; navigation is limited until the rebuild ends. To avoid this, run the same Dexter build in every editor and terminal for this project.",
		version.IndexVersion+1, version.IndexVersion)
	got := client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "written by a newer Dexter build")
	if got.Message != want {
		t.Errorf("message =\n  %q\nwant\n  %q", got.Message, want)
	}
	conditions := rt.IndexConditions()
	if len(conditions) != 1 || conditions[0].Key != lsp.CondIndexRebuild {
		t.Fatalf("index conditions during the rebuild = %+v", conditions)
	}

	release()
	if err := rt.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: the index rebuild is complete (")
	if conditions := rt.IndexConditions(); len(conditions) != 0 {
		t.Errorf("index conditions after the rebuild = %+v", conditions)
	}
}

func TestIndexThatCannotBeOpenedIsShown(t *testing.T) {
	quietReportEnv(t)
	root := t.TempDir()
	writeTestModule(t, root, "lib/one.ex", "SharedLib.One")
	if err := os.MkdirAll(store.DBDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.DBPath(root), []byte(strings.Repeat("not a database ", 512)), 0o644); err != nil {
		t.Fatal(err)
	}

	rt, release := openHeld(t, root)
	client := notifytest.New()
	defer rt.Reporter().Attach(client, false)()
	got := client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "Dexter: the index at "+store.DBPath(root)+" is damaged (")
	if !strings.Contains(got.Message, "Dexter deleted it and is rebuilding it now") {
		t.Errorf("message does not say what Dexter does: %q", got.Message)
	}
	release()
	if err := rt.WaitReady(testContext(t, 30*time.Second)); err != nil {
		t.Fatal(err)
	}
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "the index rebuild is complete")
	if countModule(t, rt, "SharedLib.One") == 0 {
		t.Fatal("the rebuild did not index the project")
	}
}

func TestIndexMessagesSayWhatHappened(t *testing.T) {
	locked := lockedIndexMessage("/p", errors.New("database is locked"))
	if !strings.Contains(locked, "Dexter did not change the index") || !strings.Contains(locked, "dexter stop --force") {
		t.Errorf("locked message gives no hint: %q", locked)
	}
	other := otherOpenFailureMessage("/p", errors.New("permission denied"))
	if strings.Contains(other, "deleted it") || !strings.Contains(other, "did not delete it") {
		t.Errorf("a failure that is not damage claims a delete: %q", other)
	}
	if older := versionMismatchMessage(3, 4); !strings.Contains(older, "older Dexter build (index version 3; this build uses 4)") {
		t.Errorf("older-build message = %q", older)
	}
	if none := versionMismatchMessage(0, 4); !strings.Contains(none, "did not finish") {
		t.Errorf("no-version message = %q", none)
	}
}

// seedIndex writes an index that holds one file.
func seedIndex(t *testing.T, root string) {
	t.Helper()
	writeStaleIndex(t, root, version.IndexVersion)
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("index file is gone: %v", err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

// Another process can hold the index in a write transaction: `dexter init` and
// older releases use a rollback journal, which locks the whole file. Deleting
// the files under it loses its work in silence, so a locked index must never
// be deleted.
func TestLockedIndexIsNeverDeleted(t *testing.T) {
	quietReportEnv(t)
	root := t.TempDir()
	seedIndex(t, root)
	dbPath := store.DBPath(root)
	before := inode(t, dbPath)

	other, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	other.SetMaxOpenConns(1)
	conn, err := other.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, stmt := range []string{
		"PRAGMA journal_mode=MEMORY",
		"BEGIN EXCLUSIVE",
		"INSERT INTO metadata (key, value) VALUES ('other_process', 'wrote this')",
	} {
		if _, err := conn.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	previous := openBusyWait
	openBusyWait = 50 * time.Millisecond
	t.Cleanup(func() { openBusyWait = previous })
	r := notify.New()
	r.SetLogf(func(string, ...any) {})
	s, err := openStore(root, r)
	if err == nil {
		_ = s.Close()
		t.Fatal("openStore opened an index that another process holds locked")
	}
	if !strings.Contains(err.Error(), "another process holds the index") {
		t.Errorf("error = %q", err)
	}
	if !r.Active(lsp.CondIndexUnavailable) || r.Active(lsp.CondIndexRebuild) {
		t.Errorf("conditions = %+v, want only index.unavailable", r.Conditions())
	}
	if after := inode(t, dbPath); after != before {
		t.Fatal("the locked index was deleted and created again")
	}

	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := conn.QueryRowContext(context.Background(), "SELECT value FROM metadata WHERE key = 'other_process'").Scan(&value); err != nil || value != "wrote this" {
		t.Fatalf("the other process lost its write: %q, %v", value, err)
	}
}

// A permission error is not fixed by a rebuild. The index must stay, and the
// message must not say that it was deleted.
func TestUnreadableIndexIsNotDeleted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	quietReportEnv(t)
	root := t.TempDir()
	seedIndex(t, root)
	dir := store.DBDir(root)
	if err := os.Chmod(store.DBPath(root), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store.DBPath(root), 0o644) })
	r := notify.New()
	r.SetLogf(func(string, ...any) {})
	s, err := openStore(root, r)
	if err == nil {
		_ = s.Close()
		t.Fatal("openStore opened an index without read permission")
	}
	if _, statErr := os.Stat(store.DBPath(root)); statErr != nil {
		t.Fatalf("the index was deleted: %v", statErr)
	}
	conditions := r.Conditions()
	if len(conditions) != 1 || conditions[0].Key != lsp.CondIndexUnavailable || conditions[0].Severity != notify.Error ||
		!strings.Contains(conditions[0].Message, "Dexter did not delete it") || !strings.Contains(conditions[0].Message, dir) {
		t.Fatalf("conditions = %+v", conditions)
	}
}

func TestRootThatIsNotAProjectIsShown(t *testing.T) {
	r := notify.New()
	r.SetLogf(func(string, ...any) {})
	reportRoot(t.TempDir(), r)
	if c := r.Conditions(); len(c) != 1 || !strings.Contains(c[0].Message, "does not look like an Elixir project") {
		t.Fatalf("conditions = %+v", c)
	}

	project := t.TempDir()
	writeTestModule(t, project, "mix.exs", "SharedLib.MixProject")
	r = notify.New()
	reportRoot(project, r)
	if c := r.Conditions(); len(c) != 0 {
		t.Fatalf("a project was reported: %+v", c)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestModule(t, home, "mix.exs", "SharedLib.MixProject")
	r = notify.New()
	r.SetLogf(func(string, ...any) {})
	reportRoot(home, r)
	if c := r.Conditions(); len(c) != 1 || !strings.Contains(c[0].Message, "is your home directory, not a project") {
		t.Fatalf("conditions = %+v", c)
	}
}

func TestUnavailableWatchingIsShownAndItsRecovery(t *testing.T) {
	quietReportEnv(t)
	root := t.TempDir()
	previousStart := startNativeWatch
	previousInterval := watchRetryInterval
	var attempts atomic.Int32
	startNativeWatch = func(root string, callbacks WatchCallbacks) (*Watcher, error) {
		if attempts.Add(1) <= 2 {
			return nil, errors.New("too many open files")
		}
		return &Watcher{backend: &fsnotifyWatcher{failed: map[string]struct{}{}}, kind: "fake"}, nil
	}
	watchRetryInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		startNativeWatch = previousStart
		watchRetryInterval = previousInterval
	})
	rt, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		rt.watcherMu.Lock()
		rt.watcher = nil // the fake has no fsnotify handle to close
		rt.watcherMu.Unlock()
		_ = rt.Close()
	})
	client := notifytest.New()
	defer rt.Reporter().Attach(client, false)()

	got := client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "Dexter: cannot watch the files of "+root+" (too many open files)")
	if !strings.Contains(got.Message, "Files that you save in the editor are still indexed") {
		t.Errorf("message does not say what still works: %q", got.Message)
	}
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: file watching works again for "+root)
	watchMessages := 0
	for _, m := range client.Messages() {
		if strings.Contains(m.Message, "watch") {
			watchMessages++
		}
	}
	if watchMessages != 2 {
		t.Errorf("a failed retry sent a message again:\n%s", client.Dump())
	}
}

func TestWatcherFallbackAndCoverageAreShown(t *testing.T) {
	root := t.TempDir()
	rt := &Runtime{root: root, index: lsp.NewIndexCoordinator()}
	rt.Reporter().SetLogf(func(string, ...any) {})
	client := notifytest.New()
	defer rt.Reporter().Attach(client, false)()

	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	backend := &fsnotifyWatcher{
		failed:         map[string]struct{}{a: {}, b: {}},
		fallbackReason: errors.New("macOS FSEvents: stream failed"),
	}
	w := &Watcher{backend: backend, kind: "fsnotify"}
	rt.watcher = w
	rt.reportWatcherStarted(w)

	client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "the native file watcher is not available for "+root+" (macOS FSEvents: stream failed)")
	client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "Dexter: 2 directories ("+a+" and 1 more) under "+root+" cannot be watched")

	// One directory comes back: still degraded, so no new message.
	delete(backend.failed, a)
	rt.reportCoverage()
	delete(backend.failed, b)
	rt.reportCoverage()
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: file watching covers the whole project again")
	if n := len(client.Messages()); n != 3 {
		t.Errorf("got %d messages, want 3:\n%s", n, client.Dump())
	}
}

// racingBackend restores its last failed directory right after the first
// Degraded check, and runs the watcher's coverage callback for that restore
// before the caller of the check goes on, when nothing stops it.
type racingBackend struct {
	mu        sync.Mutex
	calls     int
	degraded  bool
	onRestore func()
}

func (b *racingBackend) Close() error { return nil }

func (b *racingBackend) Degraded() bool {
	b.mu.Lock()
	b.calls++
	first := b.calls == 1
	was := b.degraded
	if first {
		b.degraded = false
	}
	b.mu.Unlock()
	if first {
		done := make(chan struct{})
		go func() {
			b.onRestore()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
		return was
	}
	return b.degraded
}

// The last directory can come back between the check that the watcher is
// degraded and the warning. The warning must not stay after the report that
// coverage returned.
func TestCoverageRestoredDuringStartIsNotLeftDegraded(t *testing.T) {
	rt := &Runtime{root: t.TempDir(), index: lsp.NewIndexCoordinator()}
	rt.Reporter().SetLogf(func(string, ...any) {})
	backend := &racingBackend{degraded: true, onRestore: rt.reportCoverage}
	w := &Watcher{backend: backend, kind: "fsnotify"}
	rt.watcher = w
	rt.reportWatcherStarted(w)
	deadline := time.Now().Add(2 * time.Second)
	for rt.Reporter().Active(condWatchCoverage) {
		if time.Now().After(deadline) {
			t.Fatal("the coverage warning stayed after coverage came back")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
