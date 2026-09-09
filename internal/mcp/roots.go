package mcp

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/remoteoss/dexter/internal/store"
)

// fileURIToPath converts a file:// URI to an absolute filesystem path.
func fileURIToPath(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid root URI %q: %w", raw, err)
	}
	if u.Host != "" && u.Host != "localhost" {
		return "", fmt.Errorf("root URI %q names a remote host", raw)
	}
	path := filepath.Clean(filepath.FromSlash(u.Path))
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("root URI %q has no absolute path", raw)
	}
	return path, nil
}

// negotiatedRoot resolves a session's workspace root from the MCP roots the
// client advertises. ok is false when the client offers no usable root (no
// roots capability, an empty list, or no file:// root): callers fall back to
// the launch-directory root. A transport failure or an unusable file:// root
// is an error the caller should surface and retry, not cache.
//
// A usable root resolves like the LSP's Initialize does: upward from the
// given directory to an existing index (.dexter/dexter.db) or repository
// marker (.git), so an existing index is reused and a subdirectory root
// still lands on the project.
func negotiatedRoot(ctx context.Context, ss *mcp.ServerSession) (root string, ok bool, err error) {
	params := ss.InitializeParams()
	if params == nil || params.Capabilities == nil || params.Capabilities.RootsV2 == nil {
		return "", false, nil
	}
	res, err := ss.ListRoots(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("listing client roots: %w", err)
	}
	for _, r := range res.Roots {
		if !strings.HasPrefix(r.URI, "file:") {
			continue
		}
		path, err := fileURIToPath(r.URI)
		if err != nil {
			return "", false, err
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			return "", false, fmt.Errorf("client root %q is not a directory", path)
		}
		return store.FindProjectRoot(path), true, nil
	}
	return "", false, nil
}
