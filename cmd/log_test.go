package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/daemon"
)

func TestLogPrintsTheDaemonLogOfTheProject(t *testing.T) {
	runtime, err := os.MkdirTemp("/tmp", "dxl") // short: a socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })
	t.Setenv("DEXTER_RUNTIME_DIR", runtime)
	t.Setenv("HOME", t.TempDir())

	work := t.TempDir()
	shop := filepath.Join(work, "shop")
	web := filepath.Join(work, "web")
	for _, d := range []string{filepath.Join(work, ".git"), filepath.Join(shop, ".git"), filepath.Join(web, ".git"), filepath.Join(shop, "lib")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(shop, "mix.exs"), filepath.Join(web, "mix.exs")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ep, err := daemon.ResolveEndpoint(shop)
	if err != nil {
		t.Fatal(err)
	}

	// A project: its log path, also before a daemon wrote it.
	var out, notes bytes.Buffer
	if err := cmdLog(filepath.Join(shop, "lib"), &out, &notes); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != ep.Log {
		t.Fatalf("dexter log in a project = %q, want %q", got, ep.Log)
	}

	// A folder of repositories: no log yet is an error that says what to do.
	out.Reset()
	if err := cmdLog(work, &out, &notes); err == nil || !strings.Contains(err.Error(), "no project in it has a daemon log yet") {
		t.Fatalf("dexter log in a folder with no logs = %v", err)
	}
	// Then each project that has a log, named on stderr.
	if err := os.MkdirAll(filepath.Dir(ep.Log), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ep.Log, []byte("log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	notes.Reset()
	if err := cmdLog(work, &out, &notes); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != ep.Log {
		t.Errorf("dexter log in the folder = %q, want only shop's log %q", got, ep.Log)
	}
	if !strings.Contains(notes.String(), "shop: "+ep.Log) {
		t.Errorf("stderr does not name the project: %q", notes.String())
	}
}
