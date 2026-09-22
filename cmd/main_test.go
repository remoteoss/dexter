package main

import (
	"context"
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
