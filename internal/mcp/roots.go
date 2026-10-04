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

// fileURIToPath converts a file:// URI to a clean absolute filesystem path, so
// that spellings such as a trailing slash name the same root.
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
// A usable root goes through resolve, which finds the project root the same
// way for every frontend. The spelling the client used is kept: the daemon
// indexes paths in the spelling of the frontend that started it, and refuses
// other spellings of the same directory.
func negotiatedRoot(ctx context.Context, ss *mcp.ServerSession, resolve func(string) (string, error)) (root string, ok bool, err error) {
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
		root, err := resolve(path)
		if err != nil {
			return "", false, err
		}
		return root, true, nil
	}
	return "", false, nil
}

// defaultResolveRoot finds the project root above dir with the store's marker
// search (an existing index, then a repository).
func defaultResolveRoot(dir string) (string, error) {
	return store.FindProjectRoot(dir), nil
}
