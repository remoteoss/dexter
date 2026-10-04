package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/daemon"
)

func TestWaitForDaemonStopBoundsProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	replacementPID, err := waitForDaemonStop(ctx, 42, func(probeCtx context.Context) stopProbeResult {
		if _, ok := probeCtx.Deadline(); !ok {
			t.Error("post-shutdown probe has no deadline")
		}
		<-probeCtx.Done()
		return stopProbeResult{running: true, pid: 42}
	})
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("waitForDaemonStop took %v, want a bounded wait", elapsed)
	}
	if replacementPID != 0 {
		t.Fatalf("replacement pid = %d, want 0", replacementPID)
	}
	if err == nil || !strings.Contains(err.Error(), "dexter stop --force") {
		t.Fatalf("error = %v, want a force-stop recommendation", err)
	}
}

// A deleted target resolves its workspace from the nearest ancestor that still
// exists, never from the missing path itself.
func TestFindProjectRootWithMissingStartsFromExistingAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "mix.exs"), []byte("defmodule P.MixProject do\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{
		filepath.Join(root, "lib", "gone.ex"),
		filepath.Join(root, "lib", "deleted", "tree", "gone.ex"),
		filepath.Join(root, "gone"),
	} {
		if got := findProjectRootWithMissing(missing, true); got != root {
			t.Errorf("root for %s = %s, want %s", missing, got, root)
		}
	}

	// With no project around it, the root is what the ancestor resolves to on
	// its own, which is an existing directory rather than the deleted file.
	elsewhere := t.TempDir()
	if got, want := findProjectRootWithMissing(filepath.Join(elsewhere, "a", "gone.ex"), true), findProjectRoot(elsewhere); got != want {
		t.Errorf("root outside a project = %s, want %s", got, want)
	}
}

// captureStderr runs fn with os.Stderr sent to a pipe and returns what it
// wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()
	fn()
	_ = w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// A rebuilding or unusable index is stated on the CLI, so that an answer from
// it is not taken as complete. --quiet hides the warning, never the error.
func TestIndexNotesAreStatedOnStderr(t *testing.T) {
	t.Setenv("DEXTER_QUIET", "")
	notes := []daemon.Note{
		{Key: "index.build", Severity: "info", Message: "Dexter: building the index for the first time"},
		{Key: "index.rebuild", Severity: "warning", Message: "Dexter: the index was written by a newer Dexter build. Rebuilding it now."},
		{Key: "index.unavailable", Severity: "error", Message: "Dexter: the index could not be completed."},
	}
	got := captureStderr(t, func() {
		printIndexNotes(notes, queryOptions{})
		warnIfIndexBuilding(false, notes, queryOptions{})
	})
	want := "note: the index was written by a newer Dexter build. Rebuilding it now.\nerror: the index could not be completed.\n"
	if got != want {
		t.Errorf("stderr =\n%q\nwant\n%q", got, want)
	}
	quiet := captureStderr(t, func() { printIndexNotes(notes, queryOptions{quiet: true}) })
	if quiet != "error: the index could not be completed.\n" {
		t.Errorf("quiet stderr = %q", quiet)
	}
	building := captureStderr(t, func() { warnIfIndexBuilding(false, notes[:1], queryOptions{}) })
	if !strings.Contains(building, "still building") {
		t.Errorf("a first build is not stated: %q", building)
	}
}

// A file that could not be indexed during the first build does not say that
// the build is still running, so it must not hide the note that does.
func TestFileFailureDoesNotHideTheBuildingNote(t *testing.T) {
	t.Setenv("DEXTER_QUIET", "")
	notes := []daemon.Note{
		{Key: "index.build", Severity: "info", Message: "Dexter: building the index for the first time"},
		{Key: "index.files", Severity: "warning", Message: "Dexter: 1 file could not be indexed: /p/lib/a.ex"},
	}
	got := captureStderr(t, func() { warnIfIndexBuilding(false, notes, queryOptions{}) })
	if !strings.Contains(got, "still building") {
		t.Errorf("the building note is hidden: %q", got)
	}
}

// Regression: the MCP fallback root and the roots that MCP clients give were
// resolved by two different searches (one treated mix.exs as a marker, one did
// not), so a session with roots and a session without them could start two
// daemons for one directory. Both now use the CLI's search.
func TestMCPRootsResolveLikeCLI(t *testing.T) {
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "mix.exs"), []byte("defmodule App.MixProject do\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(project, "lib")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := mcpConfig(sub, false)
	if err != nil {
		t.Fatal(err)
	}
	want := findProjectRoot(sub)
	if cfg.Root != want {
		t.Errorf("fallback root = %q, want the CLI root %q", cfg.Root, want)
	}
	negotiated, err := cfg.ResolveRoot(sub)
	if err != nil {
		t.Fatal(err)
	}
	if negotiated != want {
		t.Errorf("negotiated root = %q, want the CLI root %q", negotiated, want)
	}
	if cfg.FallbackErr != nil {
		t.Errorf("a Mix project was refused as the fallback root: %v", cfg.FallbackErr)
	}
}

// A launch directory that is not a project is not indexed when the MCP client
// gives no root.
func TestMCPFallbackRefusesNonProject(t *testing.T) {
	cfg, err := mcpConfig(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FallbackErr == nil || !strings.Contains(cfg.FallbackErr.Error(), "does not look like an Elixir project") {
		t.Errorf("FallbackErr = %v, want a refusal", cfg.FallbackErr)
	}
	explicit, err := mcpConfig(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if explicit.FallbackErr != nil {
		t.Errorf("an explicit root was refused: %v", explicit.FallbackErr)
	}
}

// Regression: a client root that was not a project (or was the home
// directory) started a daemon that indexed all of it. It is refused like the
// launch directory, so the frontend skips it as unusable.
func TestMCPClientRootRefusesNonProject(t *testing.T) {
	cfg, err := mcpConfig(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	plain := t.TempDir()
	if _, err := cfg.ResolveRoot(plain); err == nil || !strings.Contains(err.Error(), "does not look like an Elixir project") {
		t.Errorf("ResolveRoot(%s) error = %v, want a refusal", plain, err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := cfg.ResolveRoot(home); err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Errorf("ResolveRoot(home) error = %v, want a refusal", err)
	}
}
