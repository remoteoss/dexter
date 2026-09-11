package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/version"
)

type ReindexParams struct{}

func (h *Handler) reindexHandler(ctx context.Context, req *mcp.CallToolRequest, args ReindexParams) (*mcp.CallToolResult, any, error) {
	// A version mismatch requires a full rebuild, which must not happen under a
	// live store handle; that is handled at server startup instead.
	if stored := h.store.GetIndexVersion(); stored != version.IndexVersion {
		return textResult(fmt.Sprintf("Index version %d does not match this binary (%d). Restart dexter mcp to rebuild the index.", stored, version.IndexVersion)), nil, nil
	}

	updated, elapsed := h.lsp.ReindexWorkspace()
	return textResult(fmt.Sprintf("Reindexed %d file(s) in %s. The index is up to date.", updated, elapsed.Round(time.Millisecond))), nil, nil
}
