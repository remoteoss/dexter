package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/notify/notifytest"
)

const reportWait = 10 * time.Second

// attachFakeEditor connects a recording client to the server the way a real
// editor session does: capabilities in Initialize, reports from initialized.
func attachFakeEditor(t *testing.T, server *Server, progress bool) *notifytest.Client {
	t.Helper()
	client := notifytest.New()
	server.client = client
	server.workDoneProgress = progress
	if err := server.Initialized(context.Background(), &protocol.InitializedParams{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.detachFromReporter)
	return client
}

func countMessages(client *notifytest.Client, text string) int {
	n := 0
	for _, m := range client.Messages() {
		if strings.Contains(m.Message, text) {
			n++
		}
	}
	return n
}

// A cold build is long work: the editor must see that it started, its
// progress, and that it ended.
func TestColdBuildShowsProgressAndResult(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	writeTestFile(t, server.projectRoot, "lib/accounts.ex", "defmodule MyApp.Accounts do\n  def get, do: :ok\nend\n")
	client := attachFakeEditor(t, server, true)

	server.backgroundReindex()
	server.index.backgroundWork.Wait()

	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: building the index for the first time")
	client.WaitFor(t, reportWait, "progress begin", func(e notifytest.Event) bool {
		return e.Method == protocol.MethodProgress && e.Kind == "begin" && e.Title == "Dexter: building the index"
	})
	client.WaitFor(t, reportWait, "progress end", func(e notifytest.Event) bool {
		return e.Method == protocol.MethodProgress && e.Kind == "end" && strings.HasPrefix(e.Message, "Dexter: index built (1 files in ")
	})
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: index built (1 files in ")
	for _, c := range server.index.reporter.Conditions() {
		if strings.HasPrefix(c.Key, IndexConditionPrefix) {
			t.Errorf("index condition %q is still active after the build", c.Key)
		}
	}
}

// A workspace with no Elixir files stays empty, so each full pass over it
// starts as a cold build. The editors must hear about the first build once,
// not on every git HEAD change or watcher overflow.
func TestEmptyWorkspaceReportsTheFirstBuildOnce(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	client := attachFakeEditor(t, server, true)
	for i := 0; i < 3; i++ {
		server.backgroundReindex()
		server.index.backgroundWork.Wait()
	}
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: index built (0 files in ")
	time.Sleep(50 * time.Millisecond)
	begins := 0
	for _, e := range client.Events() {
		if e.Method == protocol.MethodProgress && e.Kind == "begin" {
			begins++
		}
	}
	if n := countMessages(client, "building the index for the first time"); n != 1 || begins != 1 || countMessages(client, "index built") != 1 {
		t.Fatalf("three passes over an empty workspace sent %d first-build messages and %d progress begins:\n%s", n, begins, client.Dump())
	}
}

// A rebuild (set by the workspace when it deleted an index) replaces the
// first-build message, and its end says that navigation works fully again.
func TestRebuildEndsWithItsOwnMessage(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	writeTestFile(t, server.projectRoot, "lib/accounts.ex", "defmodule MyApp.Accounts do\nend\n")
	server.index.reporter.Set(CondIndexRebuild, notify.Warning, "Dexter: the index was written by a newer Dexter build")
	client := attachFakeEditor(t, server, false)

	server.backgroundReindex()
	server.index.backgroundWork.Wait()

	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: the index rebuild is complete (1 files in ")
	if n := countMessages(client, "building the index for the first time"); n != 0 {
		t.Errorf("a rebuild was also told as a first build:\n%s", client.Dump())
	}
	if n := countMessages(client, "Dexter: index built"); n != 0 {
		t.Errorf("a rebuild sent a second end message:\n%s", client.Dump())
	}
	if server.index.reporter.Active(CondIndexRebuild) {
		t.Error("the rebuild condition is still active")
	}
}

func TestUnusableIndexIsAnError(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	client := attachFakeEditor(t, server, true)
	task := server.beginIndexBuild()
	server.indexBuildFailedUnindexed(task, errors.New("index creation failed"))

	got := client.WaitMessage(t, reportWait, protocol.MessageTypeError, "the index could not be completed (index creation failed)")
	if !strings.Contains(got.Message, "dexter init --force") {
		t.Errorf("message does not say what to do: %q", got.Message)
	}
	client.WaitFor(t, reportWait, "progress end", func(e notifytest.Event) bool {
		return e.Method == protocol.MethodProgress && e.Kind == "end"
	})
	if server.index.reporter.Active(CondIndexBuild) {
		t.Error("the build condition is still active")
	}
}

// Files that cannot be indexed are told in one message, not one each, and the
// user is told when they are indexed again.
func TestFileFailuresAreAggregated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read files without read permission")
	}
	server, cleanup := setupTestServer(t)
	defer cleanup()
	indexFile(t, server.store, server.projectRoot, "lib/good.ex", "defmodule Good do\nend\n")
	var bad []string
	for _, name := range []string{"a", "b", "c"} {
		path := filepath.Join(server.projectRoot, "lib", name+".ex")
		writeTestFile(t, server.projectRoot, "lib/"+name+".ex", "defmodule Bad do\nend\n")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		bad = append(bad, path)
	}
	t.Cleanup(func() {
		for _, path := range bad {
			_ = os.Chmod(path, 0o644)
		}
	})
	client := attachFakeEditor(t, server, false)

	server.backgroundReindex()
	server.index.backgroundWork.Wait()

	got := client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "Dexter: 3 files could not be indexed: "+bad[0]+" and 2 more (")
	if !strings.Contains(got.Message, "see the log") {
		t.Errorf("message does not point to the log: %q", got.Message)
	}

	for i, path := range bad {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		server.ReconcileFile(path)
		if i < len(bad)-1 && !server.index.reporter.Active(CondIndexFiles) {
			t.Fatal("condition cleared while files still fail")
		}
	}
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "all files that could not be indexed are indexed now")
	if n := countMessages(client, "could not be indexed:"); n != 1 {
		t.Errorf("got %d failure messages, want 1:\n%s", n, client.Dump())
	}
}

// A failed file was never stored, so removing its directory from the index
// does not list it. The report must still end when the directory goes.
func TestRemovedDirectoryEndsItsFileFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read files without read permission")
	}
	server, cleanup := setupTestServer(t)
	defer cleanup()
	indexFile(t, server.store, server.projectRoot, "lib/good.ex", "defmodule Good do\nend\n")
	writeTestFile(t, server.projectRoot, "lib/gen/bad.ex", "defmodule Gen.Bad do\nend\n")
	bad := filepath.Join(server.projectRoot, "lib", "gen", "bad.ex")
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatal(err)
	}
	client := attachFakeEditor(t, server, false)
	server.backgroundReindex()
	server.index.backgroundWork.Wait()
	client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "1 file could not be indexed: "+bad)

	gen := filepath.Join(server.projectRoot, "lib", "gen")
	moved := filepath.Join(t.TempDir(), "gen")
	if err := os.Rename(gen, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(moved, "bad.ex"), 0o644) })
	// What the workspace runtime does for a directory that went away.
	under, err := server.store.ListFilePathsUnder(gen)
	if err != nil {
		t.Fatal(err)
	}
	server.RemoveFiles(append(under, gen))
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "all files that could not be indexed are indexed now")
}

// A full build sees every file, so after it the failures are exactly its own.
// A file that failed in an earlier build and is now indexed, or gone, must end
// the warning. The index stays empty after a build in which the only file
// failed, so the next pass is a full build again.
func TestFullBuildReplacesFileFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read files without read permission")
	}
	for _, fix := range []string{"readable", "deleted"} {
		t.Run(fix, func(t *testing.T) {
			server, cleanup := setupTestServer(t)
			defer cleanup()
			path := writeTestFile(t, server.projectRoot, "lib/only.ex", "defmodule Only do\nend\n")
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
			client := attachFakeEditor(t, server, false)
			server.backgroundReindex()
			server.index.backgroundWork.Wait()
			client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "1 file could not be indexed: "+path)
			if !server.store.IsEmpty() {
				t.Fatal("the index is not empty, so the next pass is not a full build")
			}

			if fix == "readable" {
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			server.backgroundReindex()
			server.index.backgroundWork.Wait()
			client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "all files that could not be indexed are indexed now")
			if server.index.reporter.Active(CondIndexFiles) {
				t.Error("the warning is still active")
			}
		})
	}
}

// A file that went away during a walk is not a failure.
func TestMissingFileIsNotAFailure(t *testing.T) {
	var f fileFailures
	f.fail("/gone.ex", &os.PathError{Op: "open", Path: "/gone.ex", Err: os.ErrNotExist})
	if f.count.Load() != 0 {
		t.Fatal("a missing file was recorded as a failure")
	}
}

func TestMissingStdlibAndMixAreShown(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", "")
	server, cleanup := setupTestServer(t)
	defer cleanup()
	server.manageWorkspace = false // no background build in this test
	server.mixBin = ""

	if _, err := server.Initialize(context.Background(), &protocol.InitializeParams{
		Capabilities: protocol.ClientCapabilities{Window: &protocol.WindowClientCapabilities{WorkDoneProgress: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if !server.workDoneProgress {
		t.Error("the workDoneProgress capability was not read")
	}
	client := attachFakeEditor(t, server, true)
	client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "could not find the Elixir standard library")
	client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "could not find the `mix` binary, so formatting does not work in this editor")

	server.SetStdlibRoot("/opt/elixir/lib")
	server.ReportStdlib()
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "found the Elixir standard library at /opt/elixir/lib")
}

// Editors share one stdlib root and one reporter, but each finds mix for
// itself. An editor that cannot find either on its own must not tell every
// editor that the workspace has no stdlib when another editor already set it,
// and its missing mix concerns only that editor.
func TestStdlibIsReportedFromTheSharedRootAndMixPerEditor(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", "")
	first, cleanup := setupTestServer(t)
	defer cleanup()
	first.manageWorkspace = false
	stdlibRoot := t.TempDir()
	first.SetStdlibRoot(stdlibRoot) // as an editor with stdlibPath does
	firstClient := attachFakeEditor(t, first, false)

	second := NewServerWithOptions(first.store, first.projectRoot, ServerOptions{Index: first.index})
	if _, err := second.Initialize(context.Background(), &protocol.InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	secondClient := attachFakeEditor(t, second, false)
	secondClient.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "could not find the `mix` binary")

	time.Sleep(50 * time.Millisecond)
	if first.index.reporter.Active(condStdlib) {
		t.Errorf("the stdlib is reported missing although the workspace has %s", stdlibRoot)
	}
	for _, m := range firstClient.Messages() {
		t.Errorf("the first editor received a report about the second: %v", m)
	}
}

// fakeMix writes a mix script that fails `mix format` in a directory named
// "broken" and with a syntax error in a directory named "typo", and that
// formats everywhere else by echoing its input.
func fakeMix(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mix")
	script := `#!/bin/sh
case "$PWD" in
  */broken) echo "** (Mix) Formatter plugin Styler cannot be found" >&2; exit 1 ;;
  */typo) echo "** (SyntaxError) lib/a.ex:3: unexpected token" >&2; exit 1 ;;
esac
cat
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// In an umbrella or a monorepo, one Mix project can fail to format while
// another works. Saves that alternate between them must not send a warning and
// a "works again" message to every editor each time.
func TestFormatterFailuresAreKeptForEachProject(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	server.mixBin = fakeMix(t)
	client := attachFakeEditor(t, server, false)
	broken := filepath.Join(server.projectRoot, "apps", "broken")
	good := filepath.Join(server.projectRoot, "apps", "good")
	typo := filepath.Join(server.projectRoot, "apps", "typo")
	for _, dir := range []string{broken, good, typo} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	format := func(mixRoot string) error {
		_, err := server.formatWithMixFormat(context.Background(), mixRoot, filepath.Join(mixRoot, "lib", "a.ex"), "x\n")
		return err
	}

	for i := 0; i < 3; i++ {
		if err := format(broken); err == nil {
			t.Fatal("the broken project formatted")
		}
		if err := format(good); err != nil {
			t.Fatalf("the good project did not format: %v", err)
		}
	}
	if err := format(typo); err == nil {
		t.Fatal("the project with a syntax error formatted")
	}
	got := client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "formatting does not work in "+broken+": `mix format` failed (** (Mix) Formatter plugin Styler cannot be found)")
	if !strings.Contains(got.Message, "mix deps.get") {
		t.Errorf("message does not say what to do: %q", got.Message)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(client.Messages()); n != 1 {
		t.Fatalf("alternate saves sent %d messages, want 1:\n%s", n, client.Dump())
	}
	if server.index.reporter.Active(condFormatter + ":" + typo) {
		t.Error("a syntax error in user code was reported as a formatter failure")
	}

	// A later success in the broken project is what clears its report.
	server.reportMixFormatWorks(broken)
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: formatting works again in "+broken+".")

	server.rememberOTPMismatch(good)
	server.reportBeamOTP(good, good, nil)
	client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "Elixir/OTP version mismatch in "+good)
	other := filepath.Join(server.projectRoot, "apps", "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := format(other); err != nil {
		t.Fatal(err)
	}
	if !server.index.reporter.Active(condOTP + ":" + good) {
		t.Fatal("a success in one project cleared the OTP report of another")
	}
}

func TestRenameFailuresAreShownToTheRequestingEditor(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	client := notifytest.New()
	server.client = client
	var failures renameFailures
	failures.add("/p/lib/b.ex", errors.New("permission denied"))
	failures.add("/p/lib/a.ex", errors.New("read-only file system"))
	server.reportRenameFailures(&failures)
	got := client.WaitMessage(t, reportWait, protocol.MessageTypeError, "the rename could not change 2 files: /p/lib/a.ex and 1 more (permission denied)")
	if !strings.Contains(got.Message, "still use the old name") {
		t.Errorf("message does not say what is left: %q", got.Message)
	}
	server.reportRenameFailures(&renameFailures{})
	time.Sleep(20 * time.Millisecond)
	if n := len(client.Messages()); n != 1 {
		t.Errorf("a rename without failures sent a message:\n%s", client.Dump())
	}
}

// Sessions attach in initialized and detach when they close, so a closed
// editor gets nothing more.
func TestClosedSessionStopsReceivingReports(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	client := attachFakeEditor(t, server, false)
	server.index.reporter.Notify(notify.Warning, "Dexter: before close")
	client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "before close")
	server.CloseSession()
	server.index.reporter.Notify(notify.Warning, "Dexter: after close")
	time.Sleep(20 * time.Millisecond)
	if n := countMessages(client, "after close"); n != 0 {
		t.Errorf("a closed session received a report:\n%s", client.Dump())
	}
}

// The warm pass writes through the batched writer or the table rebuild. Both
// must tell the editor about a file whose write fails, show progress for a
// large change set, and clear the failure once the file is written. The
// second pass clears it through the same path as the first: on the rebuild
// path, through a rebuild that succeeds.
func TestReconcilePathsReportFailuresAndProgress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		minFiles int
	}{
		{"batched", 1 << 30},
		{"rebuild", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setReconcileVars(t, 3, tc.minFiles, 4)
			oldThreshold := reconcileProgressThreshold
			reconcileProgressThreshold = 3
			t.Cleanup(func() { reconcileProgressThreshold = oldThreshold })

			server, cleanup := setupTestServer(t)
			defer cleanup()
			writeTestFile(t, server.projectRoot, "lib/kept.ex", "defmodule MyApp.Kept do\n  def here, do: :ok\nend\n")
			reindexOnce(t, server) // cold: full build
			client := attachFakeEditor(t, server, false)

			var paths []string
			for i := 0; i < 6; i++ {
				paths = append(paths, writeTestFile(t, server.projectRoot, fmt.Sprintf("lib/gen%d.ex", i), moduleSource(i, 0)))
			}
			failing := paths[2]
			testHookWrite = func(path string) error {
				if path == failing {
					return errors.New("disk I/O error")
				}
				return nil
			}
			t.Cleanup(func() { testHookWrite = nil })
			reindexOnce(t, server)

			client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "Dexter: 1 file could not be indexed: "+failing)
			client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: updating the index")
			client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: index updated (5 changed files")
			if r, _ := server.store.LookupFunction("MyApp.Gen2", "run_v0"); len(r) != 0 {
				t.Error("the file whose write failed is in the index")
			}

			// The write works again. Touch every generated file, so that the
			// rebuild path is taken again where the test asks for it.
			testHookWrite = nil
			future := time.Now().Add(time.Hour)
			for _, path := range paths {
				if err := os.Chtimes(path, future, future); err != nil {
					t.Fatal(err)
				}
			}
			reindexOnce(t, server)
			client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "all files that could not be indexed are indexed now")
			if r, _ := server.store.LookupFunction("MyApp.Gen2", "run_v0"); len(r) != 1 {
				t.Errorf("the file is not indexed after its write works: %d rows", len(r))
			}
			if n := countMessages(client, "could not be indexed:"); n != 1 {
				t.Errorf("got %d failure messages, want 1:\n%s", n, client.Dump())
			}
		})
	}
}
