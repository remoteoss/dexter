package mcp

import (
	"context"
	"errors"
	"time"
)

// settleWait bounds the wait in settled. The watcher delivers a file the agent
// has just written in 20–50 ms, but after a git checkout or a large copy the
// queue holds a reconcile of thousands of files, which can take most of a
// minute. The agent gets its answer at once then, with a note.
var settleWait = 2 * time.Second

// settled is for a lookup that found nothing: it waits once, for the call,
// until the index has every file change that the watcher has seen (a file
// the agent has just written), and reports whether the lookup should run
// again. Without a native watcher there is nothing to wait for.
func (h *Handler) settled(ctx context.Context) bool {
	if h.flushed || h.rt == nil || !h.rt.Watching() {
		return false
	}
	h.flushed = true
	waitCtx, cancel := context.WithTimeout(ctx, settleWait)
	defer cancel()
	err := h.rt.FlushEvents(waitCtx)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		h.behind = true
	}
	return err == nil
}

// behindNote ends an answer for which settled stopped waiting.
func (h *Handler) behindNote() string {
	if !h.behind {
		return ""
	}
	return "Note: the index is still applying file changes (for example after a git checkout), so this answer can be incomplete. Retry shortly."
}

// notWatchingNote ends a "not in the index" answer when the index does not
// follow file changes by itself.
func (h *Handler) notWatchingNote() string {
	if h.rt == nil || h.rt.Watching() {
		return ""
	}
	return " File watching is not active, so call dexter_reindex after changing files."
}
