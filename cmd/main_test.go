package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
