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
