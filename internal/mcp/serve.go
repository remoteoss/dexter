package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunStdio serves MCP over stdin/stdout until ctx is canceled or the client
// disconnects.
func RunStdio(ctx context.Context, f *Frontend) error {
	return NewServer(f).Run(ctx, &mcp.StdioTransport{})
}

// httpSessionTimeout closes an HTTP session that sends no request for this
// long. A client that goes away without closing its session would otherwise
// keep its workspace connection, and with it the daemon, alive forever. A
// variable so tests can shrink it.
var httpSessionTimeout = 30 * time.Minute

// maxHTTPBodyBytes caps one HTTP request body. MCP requests from an agent are
// small; the cap keeps one request from filling the server's memory.
const maxHTTPBodyBytes = 4 << 20

// HTTPHandler returns a streamable-HTTP handler serving MCP. Each session
// gets its own protocol server; they all share the Frontend.
//
// The SDK refuses a request that reaches a loopback address with a Host that
// is not loopback (DNS rebinding). Cross-origin browser requests are refused
// too, so a web page cannot drive the tools, and bodies are capped.
func HTTPHandler(f *Frontend) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return NewServer(f) }, &mcp.StreamableHTTPOptions{
		SessionTimeout: httpSessionTimeout,
	})
	return http.MaxBytesHandler(http.NewCrossOriginProtection().Handler(h), maxHTTPBodyBytes)
}

// CheckListenAddr refuses an HTTP listen address that other machines can
// reach, because the server has no authentication: anyone who reaches it can
// read the code and rename symbols. allowRemote is the explicit way through.
func CheckListenAddr(addr string, allowRemote bool) error {
	if allowRemote {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", addr, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("refusing to listen on %q: the MCP HTTP server has no authentication, so it listens only on a loopback address (for example localhost:8080 or 127.0.0.1:8080). Pass --listen-unsafe to listen on %q anyway", addr, addr)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
