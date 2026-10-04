package lsp

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/notify/notifytest"
)

const otpMismatchStderr = "** (UndefinedFunctionError) function :erlang.foo/0 is undefined: requires a more recent Erlang/OTP"

// otpMismatchProject makes a fake Elixir install whose elixir binary fails
// with an OTP mismatch and counts its starts, and whose mix formats by echoing
// its input unless a "mixfails" file exists next to it. It returns the server, its editor, and the path of the start log.
func otpMismatchProject(t *testing.T) (*Server, *notifytest.Client, string) {
	t.Helper()
	server, cleanup := setupTestServer(t)
	t.Cleanup(cleanup)
	bin := t.TempDir()
	starts := filepath.Join(bin, "starts")
	scripts := map[string]string{
		"elixir": "#!/bin/sh\necho start >> " + starts + "\necho '" + otpMismatchStderr + "' >&2\nexit 1\n",
		// mix fails with the same mismatch while a "mixfails" file exists.
		"mix": "#!/bin/sh\nif [ -e " + filepath.Join(bin, "mixfails") + " ]; then echo '" + otpMismatchStderr + "' >&2; exit 1; fi\ncat\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	server.mixBin = filepath.Join(bin, "mix")
	writeTestFile(t, server.projectRoot, "mix.exs", "defmodule MyApp.MixProject do\nend\n")
	client := attachFakeEditor(t, server, false)
	return server, client, starts
}

func beamStarts(t *testing.T, starts string) int {
	t.Helper()
	data, err := os.ReadFile(starts)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "start")
}

func formatOnce(t *testing.T, server *Server, content string) string {
	t.Helper()
	path := filepath.Join(server.projectRoot, "lib", "a.ex")
	got, err := server.formatContent(context.Background(), server.projectRoot, path, content)
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	return got
}

// A persistent BEAM that fails with an OTP mismatch must be told once. The
// mix format fallback still works, but that does not fix the mismatch: it
// must not say "works again", and the next save must not start the failing
// BEAM again only to report the mismatch again.
func TestOTPMismatchIsShownOnceAndFormatsThroughMix(t *testing.T) {
	server, client, starts := otpMismatchProject(t)

	content := "defmodule A do\nend\n"
	if got := formatOnce(t, server, content); got != content {
		t.Fatalf("first format = %q, want %q", got, content)
	}
	got := client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "Elixir/OTP version mismatch in "+server.projectRoot)
	if !strings.Contains(got.Message, "Formatting still works through the slower `mix format` fallback") {
		t.Errorf("the message does not say that formatting still works: %q", got.Message)
	}
	for i := 0; i < 4; i++ {
		if got := formatOnce(t, server, content); got != content {
			t.Fatalf("format %d = %q, want %q", i+2, got, content)
		}
	}

	time.Sleep(50 * time.Millisecond)
	if n := beamStarts(t, starts); n != 1 {
		t.Errorf("the failing BEAM started %d times, want 1", n)
	}
	if n := countMessages(client, "Elixir/OTP version mismatch"); n != 1 {
		t.Errorf("got %d OTP messages, want 1:\n%s", n, client.Dump())
	}
	if n := countMessages(client, "works again"); n != 0 {
		t.Errorf("a mix format fallback said that formatting works again:\n%s", client.Dump())
	}
}

// fakeBeam is a persistent formatter that answers every format request with
// its input in upper case, so a test can see that the BEAM formatted.
func fakeBeam(t *testing.T) *beamProcess {
	t.Helper()
	reqReader, reqWriter := io.Pipe()
	respReader, respWriter := io.Pipe()
	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = sleeper.Wait()
		close(done)
	}()
	bp := newTestBeamProcess(reqWriter, respReader, nil)
	bp.cmd = &commandHandle{process: sleeper.Process, done: done}
	bp.startedAt = time.Now()
	go bp.readLoop()
	go func() {
		for {
			if _, err := readByte(reqReader); err != nil {
				return
			}
			reqID, err := readUint32(reqReader)
			if err != nil {
				return
			}
			header := make([]byte, 6)
			if _, err := io.ReadFull(reqReader, header); err != nil {
				return
			}
			payload := make([]byte, binary.BigEndian.Uint32(header[2:]))
			if _, err := io.ReadFull(reqReader, payload); err != nil {
				return
			}
			configLen := int(binary.BigEndian.Uint16(payload))
			rest := payload[2+configLen:]
			nameLen := int(binary.BigEndian.Uint16(rest))
			rest = rest[2+nameLen+4:]
			writeTestResponseFrame(t, respWriter, reqID, 0, []byte(strings.ToUpper(string(rest))))
		}
	}()
	t.Cleanup(func() {
		bp.closeWithReason("test end")
		_ = reqWriter.Close()
		_ = respWriter.Close()
	})
	return bp
}

// The OTP mismatch ends when the persistent BEAM starts and formats, which
// Dexter tries when the Elixir install changes.
func TestOTPMismatchClearsWhenTheBeamStarts(t *testing.T) {
	server, client, starts := otpMismatchProject(t)
	content := "defmodule A do\nend\n"
	formatOnce(t, server, content)
	client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "Elixir/OTP version mismatch in "+server.projectRoot)

	previous := startBeam
	startBeam = func(*Server, string) (*beamProcess, error) { return fakeBeam(t), nil }
	t.Cleanup(func() { startBeam = previous })

	// Nothing changed yet: the fallback is still used.
	if got := formatOnce(t, server, content); got != content {
		t.Fatalf("format before the fix = %q, want the mix format output", got)
	}

	// The user installs a matching Elixir.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(filepath.Dir(server.mixBin), "elixir"), later, later); err != nil {
		t.Fatal(err)
	}
	if got := formatOnce(t, server, content); got != strings.ToUpper(content) {
		t.Fatalf("format after the fix = %q, want the BEAM output", got)
	}
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: the fast persistent formatter works again in "+server.projectRoot+".")
	if server.index.reporter.Active(condOTP + ":" + server.projectRoot) {
		t.Error("the OTP mismatch is still active after the BEAM formatted")
	}
	if n := beamStarts(t, starts); n != 1 {
		t.Errorf("the failing BEAM started %d times, want 1", n)
	}
}

// When mix format fails with the same mismatch as the BEAM, formatting does not
// work at all. The user must see only that Error, never also the Warning that
// formatting still works through mix format. When mix works again, the Error
// ends and the Warning applies, once.
func TestOTPMismatchInBeamAndMixShowsOnlyTheError(t *testing.T) {
	server, client, starts := otpMismatchProject(t)
	mixFails := filepath.Join(filepath.Dir(starts), "mixfails")
	if err := os.WriteFile(mixFails, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(server.projectRoot, "lib", "a.ex")
	content := "defmodule A do\nend\n"
	for i := 0; i < 3; i++ {
		if _, err := server.formatContent(context.Background(), server.projectRoot, path, content); err == nil {
			t.Fatal("formatting worked although mix fails")
		}
	}
	client.WaitMessage(t, reportWait, protocol.MessageTypeError, "formatting does not work in "+server.projectRoot+": Elixir/OTP version mismatch")
	time.Sleep(50 * time.Millisecond)
	if n := countMessages(client, "still works"); n != 0 {
		t.Fatalf("the user was told that formatting still works while it does not:\n%s", client.Dump())
	}
	var formatterConditions []string
	for _, c := range server.index.reporter.Conditions() {
		if strings.HasPrefix(c.Key, condFormatter) {
			formatterConditions = append(formatterConditions, c.Key)
		}
	}
	if len(formatterConditions) != 1 || formatterConditions[0] != condFormatter+":"+server.projectRoot {
		t.Fatalf("formatter conditions = %v, want only the Error", formatterConditions)
	}

	if err := os.Remove(mixFails); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := formatOnce(t, server, content); got != content {
			t.Fatalf("format = %q, want %q", got, content)
		}
	}
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: formatting works again in "+server.projectRoot+".")
	client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "Formatting still works through the slower `mix format` fallback")
	time.Sleep(50 * time.Millisecond)
	if n := countMessages(client, "still works"); n != 1 {
		t.Errorf("got %d Warnings, want 1:\n%s", n, client.Dump())
	}
	if server.index.reporter.Active(condFormatter + ":" + server.projectRoot) {
		t.Error("the Error is still active after mix format worked")
	}
	if n := beamStarts(t, starts); n != 1 {
		t.Errorf("the failing BEAM started %d times, want 1", n)
	}
}

// In an umbrella, the Mix projects share one build root and so one BEAM and
// one Warning. When one project's mix format also fails with the mismatch and
// another's works, saves that alternate between them must not drop and set the
// Warning again each time.
func TestOTPMismatchInUmbrellaIsStableUnderAlternateSaves(t *testing.T) {
	server, client, starts := otpMismatchProject(t)
	bin := filepath.Dir(starts)
	aFails := filepath.Join(bin, "afails")
	script := "#!/bin/sh\ncase \"$PWD\" in */apps/a) if [ -e " + aFails + " ]; then echo '" + otpMismatchStderr + "' >&2; exit 1; fi ;; esac\ncat\n"
	if err := os.WriteFile(server.mixBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aFails, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	appA := filepath.Join(server.projectRoot, "apps", "a")
	appB := filepath.Join(server.projectRoot, "apps", "b")
	for _, dir := range []string{appA, appB} {
		if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	content := "defmodule A do\nend\n"
	save := func(app string) error {
		got, err := server.formatContent(context.Background(), app, filepath.Join(app, "lib", "x.ex"), content)
		if err == nil && got != content {
			t.Fatalf("format in %s = %q, want %q", app, got, content)
		}
		return err
	}

	for i, app := range []string{appA, appB, appA, appB, appA} {
		err := save(app)
		if app == appA && err == nil {
			t.Fatalf("save %d in app a worked although its mix fails", i+1)
		}
		if app == appB && err != nil {
			t.Fatalf("save %d in app b failed: %v", i+1, err)
		}
	}
	client.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "Formatting still works through the slower `mix format` fallback")
	client.WaitMessage(t, reportWait, protocol.MessageTypeError, "formatting does not work in "+appA)
	time.Sleep(50 * time.Millisecond)
	if n := countMessages(client, "still works"); n != 1 {
		t.Errorf("alternate saves sent %d Warnings, want 1:\n%s", n, client.Dump())
	}
	if n := countMessages(client, "formatting does not work in "+appA); n != 1 {
		t.Errorf("got %d Errors for app a, want 1:\n%s", n, client.Dump())
	}

	if err := os.Remove(aFails); err != nil {
		t.Fatal(err)
	}
	if err := save(appA); err != nil {
		t.Fatalf("app a did not format after its mix works: %v", err)
	}
	client.WaitMessage(t, reportWait, protocol.MessageTypeInfo, "Dexter: formatting works again in "+appA+".")
	time.Sleep(50 * time.Millisecond)
	if n := countMessages(client, "still works"); n != 1 {
		t.Errorf("the Warning was sent again:\n%s", client.Dump())
	}
	if !server.index.reporter.Active(condOTP + ":" + server.projectRoot) {
		t.Error("the Warning is not active although the BEAM still cannot start")
	}
	if n := beamStarts(t, starts); n != 1 {
		t.Errorf("the failing BEAM started %d times, want 1", n)
	}
}
