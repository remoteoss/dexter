package lsp

import (
	"context"
	"io"
	"sort"
	"strings"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.uber.org/zap"

	"github.com/remoteoss/dexter/internal/store"
)

// This file is the exported, name-based surface of the LSP server used by
// callers outside the LSP session (the MCP server and the CLI). Everything
// here delegates to the same internals the LSP handlers use, so results are
// identical regardless of which front end asked.

// Serve runs the Server over the given reader/writer (typically
// stdin/stdout). It blocks until the connection closes.
func Serve(server *Server, in io.Reader, out io.Writer) error {
	logger, _ := zap.NewProduction()
	stream := jsonrpc2.NewStream(stdinoutCloser{in, out})
	conn := jsonrpc2.NewConn(stream)
	server.client = protocol.ClientDispatcher(conn, logger)
	server.conn = conn

	handler := protocol.ServerHandler(server, nil)
	ctx := context.Background()

	conn.Go(ctx, handler)
	<-conn.Done()
	return conn.Err()
}

// SetStdlibRoot records the Elixir stdlib directory so lookups can classify
// stdlib symbols. The LSP session sets this during Initialize; headless
// callers (MCP) set it explicitly after resolving the stdlib themselves.
func (s *Server) SetStdlibRoot(root string) {
	s.stdlibRoot = root
}

// StdlibRoot returns the Elixir stdlib directory, or "" if not detected. In
// attached MCP mode this is set by Initialize after the Handler is built, so
// callers must read it per request rather than caching it.
func (s *Server) StdlibRoot() string {
	return s.stdlibRoot
}

// CollectReferences gathers references to module (or module.function) across
// the workspace, name-based. It mirrors the collection performed by the LSP
// References handler: direct refs, transitive refs through static __using__
// import chains, bare intra-module calls in definition files, and refs to
// defdelegate facades that target the function. Results are deduplicated by
// file+line, stdlib-filtered, and sorted by file then line.
func (s *Server) CollectReferences(module, function string) []store.ReferenceResult {
	refResults, err := s.store.LookupReferences(module, function)
	if err != nil {
		return nil
	}

	if function != "" {
		// Transitive refs via static __using__ import chains. Call sites of
		// use-injected functions are attributed to the injecting module in the
		// store, so we look up refs under each injector too.
		for _, mod := range s.findModulesWhoseUsingImports(module) {
			if transitive, err := s.store.LookupReferences(mod, function); err == nil {
				refResults = append(refResults, transitive...)
			}
		}

		// Bare intra-module calls in definition files are not indexed.
		refResults = append(refResults, s.findBareCallRefs(module, function)...)

		// Follow defdelegate in reverse: calls to facades that delegate here.
		if s.followDelegates {
			if delegates, err := s.store.LookupDelegatesTo(module, function); err == nil {
				for _, del := range delegates {
					if delegateRefs, err := s.store.LookupReferences(del.Module, del.Function); err == nil {
						refResults = append(refResults, delegateRefs...)
					}
					refResults = append(refResults, s.findBareCallRefs(del.Module, del.Function)...)
				}
			}
		}
	}

	type refKey struct {
		filePath string
		line     int
	}
	seen := make(map[refKey]struct{}, len(refResults))
	var out []store.ReferenceResult
	for _, r := range refResults {
		if s.stdlibRoot != "" && strings.HasPrefix(r.FilePath, s.stdlibRoot) {
			continue
		}
		k := refKey{r.FilePath, r.Line}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FilePath != out[j].FilePath {
			return out[i].FilePath < out[j].FilePath
		}
		return out[i].Line < out[j].Line
	})
	return out
}
