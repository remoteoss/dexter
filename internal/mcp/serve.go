package mcp

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunStdio serves MCP over stdin/stdout until ctx is canceled or the client
// disconnects.
func RunStdio(ctx context.Context, f *Frontend) error {
	return NewServer(f).Run(ctx, &mcp.StdioTransport{})
}

// HTTPHandler returns a streamable-HTTP handler serving MCP. Each session
// gets its own protocol server; they all share the Frontend.
func HTTPHandler(f *Frontend) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return NewServer(f) }, nil)
}
