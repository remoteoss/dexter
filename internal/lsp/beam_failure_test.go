package lsp

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"
)

// failingBeamProject makes a fake Elixir install whose elixir binary fails at
// startup for another cause than an OTP mismatch, and counts its starts.
func failingBeamProject(t *testing.T) (*Server, string) {
	t.Helper()
	checkStampEachTime(t)
	server, cleanup := setupTestServer(t)
	t.Cleanup(cleanup)
	bin := t.TempDir()
	starts := filepath.Join(bin, "starts")
	scripts := map[string]string{
		"elixir": "#!/bin/sh\necho start >> " + starts + "\necho '** (RuntimeError) could not load the formatter plugin' >&2\nexit 1\n",
		"mix":    "#!/bin/sh\ncat\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	server.mixBin = filepath.Join(bin, "mix")
	writeTestFile(t, server.projectRoot, "mix.exs", "defmodule MyApp.MixProject do\nend\n")
	return server, starts
}

// checkStampEachTime makes every check of a remembered failure read the stamp,
// so that a test sees a fix at once.
func checkStampEachTime(t *testing.T) {
	t.Helper()
	previous := beamStampInterval
	beamStampInterval = 0
	t.Cleanup(func() { beamStampInterval = previous })
}

// requestBeam asks for the BEAM as a request does (the prewarm of didOpen,
// Erlang hover and completion) and waits until a started process has exited.
func requestBeam(t *testing.T, server *Server) {
	t.Helper()
	bp := server.getBeamProcess(context.Background(), server.projectRoot)
	if bp == nil {
		return
	}
	select {
	case <-bp.cmd.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fake BEAM did not exit")
	}
}

// A BEAM that fails at startup for any cause must not start again on each
// request: five documents opened started five VMs. It is told once, and
// Dexter tries again when the Elixir install changes or after a delay.
func TestBeamStartFailureIsRememberedAndShownOnce(t *testing.T) {
	server, starts := failingBeamProject(t)
	client := attachFakeEditor(t, server, false)

	for i := 0; i < 5; i++ {
		requestBeam(t, server)
	}
	if n := beamStarts(t, starts); n != 1 {
		t.Fatalf("the failing BEAM started %d times for 5 requests, want 1", n)
	}
	got := client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "the persistent formatter BEAM for "+server.projectRoot+" cannot start")
	if !strings.Contains(got.Message, "could not load the formatter plugin") {
		t.Errorf("the message does not give the cause: %q", got.Message)
	}

	// The user fixes the Elixir install: the next request tries again.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(filepath.Dir(server.mixBin), "elixir"), later, later); err != nil {
		t.Fatal(err)
	}
	requestBeam(t, server)
	requestBeam(t, server)
	if n := beamStarts(t, starts); n != 2 {
		t.Fatalf("the BEAM started %d times after the Elixir install changed, want 2", n)
	}

	// Without a change, it is tried again after the delay.
	f := &server.index.beamFailures
	f.mu.Lock()
	f.keys[server.beamFailureKey(server.projectRoot)].at = time.Now().Add(-otpMismatchRetry)
	f.mu.Unlock()
	requestBeam(t, server)
	if n := beamStarts(t, starts); n != 3 {
		t.Fatalf("the BEAM started %d times after the retry delay, want 3", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := countMessages(client, "cannot start"); n != 1 {
		t.Errorf("got %d startup failure messages, want 1:\n%s", n, client.Dump())
	}
}

// The delay doubles after each failure that nothing fixed, up to a short
// maximum: the stamp cannot see every fix.
func TestBeamStartRetryBacksOff(t *testing.T) {
	f := &beamFailure{failures: 1}
	if got := f.retryAfter(); got != beamStartRetry {
		t.Fatalf("first delay = %s, want %s", got, beamStartRetry)
	}
	f.failures = 2
	if got := f.retryAfter(); got != 2*beamStartRetry {
		t.Fatalf("second delay = %s, want %s", got, 2*beamStartRetry)
	}
	f.failures = 100
	if got := f.retryAfter(); got != beamStartRetryMax || got > time.Minute {
		t.Fatalf("delay after many failures = %s, want %s, at most 1 minute", got, beamStartRetryMax)
	}
}

// Two Dexter versions share $TMPDIR/dexter. Each must start its BEAM with its
// own script and never replace the script of the other, which a shared name
// written in place did.
func TestBeamServerScriptIsPerVersionAndWrittenAtomically(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := filepath.Join(tmp, "dexter")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	otherVersion := "# the BEAM server of another Dexter version\n"
	shared := filepath.Join(dir, "beam_server.exs")
	if err := os.WriteFile(shared, []byte(otherVersion), 0o644); err != nil {
		t.Fatal(err)
	}
	otherPath, err := writeBeamServerScript(dir, otherVersion)
	if err != nil {
		t.Fatal(err)
	}

	server, cleanup := setupTestServer(t)
	defer cleanup()
	bin := t.TempDir()
	used := filepath.Join(bin, "used.exs")
	if err := os.WriteFile(filepath.Join(bin, "elixir"), []byte("#!/bin/sh\ncp \"$1\" "+used+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	server.mixBin = filepath.Join(bin, "mix")
	bp, err := server.startBeamProcess(server.projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	<-bp.cmd.done

	got, err := os.ReadFile(used)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != beamServerScript {
		t.Fatal("the BEAM started with a script that is not the script of this version")
	}
	for path, want := range map[string]string{shared: otherVersion, otherPath: otherVersion} {
		if content, err := os.ReadFile(path); err != nil || string(content) != want {
			t.Errorf("the script of another version at %s was changed", path)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("a temporary file was left: %s", e.Name())
		}
	}
}

// A compile, a new Erlang, a changed mix.lock, or an editor that connects can
// fix a BEAM that cannot start. Each must make the next request try again at
// once, not after the retry delay: the stamp held only the binaries and the
// mtime of _build, which a compile under _build/<env>/lib does not move.
func TestBeamStartFailureIsRetriedAfterAFix(t *testing.T) {
	server, starts := failingBeamProject(t)
	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	if err := os.MkdirAll(ebin, 0o755); err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	erl := filepath.Join(pathDir, "erl")
	if err := os.WriteFile(erl, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	t.Setenv("SHELL", "/bin/false")
	t.Setenv("HOME", t.TempDir())
	want := 0
	check := func(what string) {
		t.Helper()
		requestBeam(t, server)
		requestBeam(t, server)
		want++
		if n := beamStarts(t, starts); n != want {
			t.Fatalf("after %s, the BEAM started %d times in all, want %d", what, n, want)
		}
	}
	check("the first failure")

	// mix compile writes a BEAM into the ebin directory of the application.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(ebin, "Elixir.MyApp.beam"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(ebin, later, later); err != nil {
		t.Fatal(err)
	}
	check("a compile")

	// A new Erlang is installed (mise install erlang).
	later = later.Add(time.Minute)
	if err := os.Chtimes(erl, later, later); err != nil {
		t.Fatal(err)
	}
	check("a new Erlang")

	// mix.lock changes (mix deps.get), as the editor reports it.
	lock := writeTestFile(t, server.projectRoot, "mix.lock", "%{}\n")
	if err := server.DidChangeWatchedFiles(context.Background(), &protocol.DidChangeWatchedFilesParams{
		Changes: []*protocol.FileEvent{{URI: protocol.DocumentURI("file://" + lock), Type: protocol.FileChangeTypeChanged}},
	}); err != nil {
		t.Fatal(err)
	}
	check("a mix.lock change")

	// An editor connects, often after the user fixed something.
	session := NewServerWithOptions(server.store, server.projectRoot, ServerOptions{Index: server.index})
	if _, err := session.Initialize(context.Background(), &protocol.InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	check("an editor connected")
}

// Two sessions with different Elixir installs share the record of failures.
// Each must keep its own: one install that fails says nothing about the other,
// and a BEAM that starts in one session must not end the report of the other.
func TestBeamStartFailuresAreKeptForEachElixirInstall(t *testing.T) {
	first, firstStarts := failingBeamProject(t)
	second := NewServerWithOptions(first.store, first.projectRoot, ServerOptions{Index: first.index})
	bin := t.TempDir()
	secondStarts := filepath.Join(bin, "starts")
	if err := os.WriteFile(filepath.Join(bin, "elixir"), []byte("#!/bin/sh\necho start >> "+secondStarts+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	second.mixBin = filepath.Join(bin, "mix")

	for i := 0; i < 6; i++ {
		requestBeam(t, first)
		requestBeam(t, second)
	}
	if a, b := beamStarts(t, firstStarts), beamStarts(t, secondStarts); a != 1 || b != 1 {
		t.Fatalf("the failing BEAMs started %d and %d times for 6 requests each, want 1 and 1", a, b)
	}

	// A third install works in a third session. A new session, because the
	// startup goroutines of the BEAMs above can still read second.mixBin.
	third := NewServerWithOptions(first.store, first.projectRoot, ServerOptions{Index: first.index})
	third.mixBin = filepath.Join(t.TempDir(), "mix")
	third.beamStarted(third.projectRoot)
	if !first.beamFailureHolds(first.projectRoot, false) {
		t.Fatal("a BEAM that started with another install ended the failure of the first")
	}
	if !first.index.reporter.Active(beamStartKey(first.beamFailureKey(first.projectRoot))) {
		t.Fatal("a BEAM that started with another install ended the report of the first")
	}
}

// stuckBeam is a BEAM process that never becomes ready and started longer ago
// than beamStuckTimeout.
func stuckBeam(t *testing.T) *beamProcess {
	t.Helper()
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = sleeper.Wait()
		close(done)
	}()
	stdoutR, stdoutW := io.Pipe()
	_, stdinW := io.Pipe()
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = stdoutW.Close()
	})
	return &beamProcess{
		cmd:       &commandHandle{process: sleeper.Process, done: done},
		stdin:     stdinW,
		stdout:    stdoutR,
		stderr:    newStderrCapture(),
		pending:   make(map[uint32]chan beamResponse),
		startedAt: time.Now().Add(-beamStuckTimeout - time.Second),
		ready:     make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

// A BEAM that is stuck in its startup is evicted by the next format. That is a
// startup failure: the eviction's close must not hide it, or each request
// starts a BEAM that gets stuck again.
func TestStuckBeamIsRememberedAsAFailure(t *testing.T) {
	server, _ := failingBeamProject(t)
	starts := 0
	previous := startBeam
	startBeam = func(*Server, string) (*beamProcess, error) {
		starts++
		return stuckBeam(t), nil
	}
	t.Cleanup(func() { startBeam = previous })
	server.beams = map[string]*beamProcess{server.projectRoot: stuckBeam(t)}

	path := filepath.Join(server.projectRoot, "lib", "a.ex")
	if _, err := server.formatContent(context.Background(), server.projectRoot, path, "defmodule A do\nend\n"); err != nil {
		t.Fatal(err)
	}
	if bp := server.getBeamProcess(context.Background(), server.projectRoot); bp != nil || starts != 0 {
		t.Fatalf("a BEAM started %d times after a stuck one, want 0", starts)
	}
}

// An OTP mismatch and another startup failure of one BEAM replace each other.
// The report of the cause that is gone must end with it.
func TestBeamFailureReportFollowsTheCause(t *testing.T) {
	server, _ := failingBeamProject(t)
	root := server.projectRoot
	server.rememberOTPMismatch(root)
	server.reportBeamOTP(root, root, nil)
	if !server.index.reporter.Active(condOTP + ":" + root) {
		t.Fatal("no OTP report")
	}
	server.rememberBeamFailure(root, nil, false, "a plugin crashed")
	if server.index.reporter.Active(condOTP + ":" + root) {
		t.Fatal("the OTP report stayed after the cause changed")
	}
	key := beamStartKey(server.beamFailureKey(root))
	if !server.index.reporter.Active(key) {
		t.Fatal("no report of the new cause")
	}
	server.rememberOTPMismatch(root)
	if server.index.reporter.Active(key) {
		t.Fatal("the startup failure report stayed after the cause became an OTP mismatch")
	}
}

// While a failure is remembered, every request that needs the BEAM checks it,
// under beamMu. The stamp reads an entry for each compiled application, more
// than 1 ms in a large umbrella, so a stamp found unchanged is trusted for a
// short interval.
func TestBeamStampIsReadAtMostOncePerInterval(t *testing.T) {
	server, _ := failingBeamProject(t)
	beamStampInterval = time.Hour
	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	if err := os.MkdirAll(ebin, 0o755); err != nil {
		t.Fatal(err)
	}
	server.rememberBeamFailure(server.projectRoot, nil, false, "a plugin crashed")
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(ebin, later, later); err != nil {
		t.Fatal(err)
	}
	if !server.beamFailureHolds(server.projectRoot, false) {
		t.Fatal("the stamp was read again inside the interval")
	}
	beamStampInterval = 0
	if server.beamFailureHolds(server.projectRoot, false) {
		t.Fatal("the changed stamp was not seen after the interval")
	}
}

// writeBeamServerScript puts script in dir under a name that holds a hash of
// its content, and returns the path; see installScript.
func writeBeamServerScript(dir, script string) (string, error) {
	return installScript(dir, "beam_server", ".exs", script)
}
