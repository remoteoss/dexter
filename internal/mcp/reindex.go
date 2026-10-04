package mcp

import (
	"context"
	"fmt"
	"time"
)

type ReindexParams struct{}

func (h *Handler) reindex(ctx context.Context, args ReindexParams) (string, error) {
	// The workspace daemon owns the index: this is the same barrier as
	// `dexter reindex`. Every change that the daemon accepted before the call
	// is in the index when it returns.
	start := time.Now()
	if err := h.rt.Reindex(ctx); err != nil {
		return "", fmt.Errorf("reindexing: %w", err)
	}
	return fmt.Sprintf("Reindexed the workspace in %s. The index is up to date.", time.Since(start).Round(time.Millisecond)), nil
}
