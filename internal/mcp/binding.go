package mcp

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/stdlib"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
)

// indexWaitLimit caps how long a tool call waits for a workspace's initial
// index before reporting that it is still building. A variable so tests can
// shrink it.
var indexWaitLimit = 30 * time.Second

// binding is one workspace a negotiating server is serving: the store, the
// headless LSP server, and the file watcher for one resolved project root.
// Sessions whose roots resolve to the same project share a binding.
type binding struct {
	root    string
	store   *store.Store
	lsp     *lsp.Server
	watcher *Watcher

	initDone chan struct{} // closed once init finishes, successfully or not
	initErr  error
	indexed  chan struct{} // closed once the initial index pass completes
}

// init opens the workspace. It runs once, in the call that created the
// binding; everything else waits on initDone. The initial index runs in the
// background so a cold build does not stall the session's handler queue;
// tool calls gate on it through awaitIndex.
func (b *binding) init() {
	defer close(b.initDone)
	s, err := openStore(b.root)
	if err != nil {
		b.initErr = err
		return
	}
	b.store = s
	b.lsp = lsp.NewServer(s, b.root)
	if root, ok := stdlib.Resolve(s, "", b.root); ok {
		b.lsp.SetStdlibRoot(root)
	}
	b.lsp.WatchGitHead()
	if w, err := WatchFiles(b.lsp, s, b.root); err != nil {
		log.Printf("Warning: file watching unavailable for %s (%v); the index updates on branch switches and via dexter_reindex", b.root, err)
	} else {
		b.watcher = w
	}
	go func() {
		defer close(b.indexed)
		b.lsp.ReindexWorkspace()
	}()
}

// awaitIndex blocks until the workspace is ready to answer, or reports why
// it is not. Init failures surface here as retryable errors.
func (b *binding) awaitIndex(ctx context.Context) error {
	select {
	case <-b.initDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	if b.initErr != nil {
		return b.initErr
	}
	select {
	case <-b.indexed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(indexWaitLimit):
		return fmt.Errorf("the index for %s is still building; retry shortly", b.root)
	}
}

// close tears the workspace down: watcher first so no new index writes
// start, then the git-head watcher (joining any reindex it is running), then
// the initial index goroutine, and only then the store.
func (b *binding) close() {
	<-b.initDone
	if b.initErr != nil {
		return
	}
	if b.watcher != nil {
		if err := b.watcher.Close(); err != nil {
			log.Printf("Warning: closing file watcher for %s: %v", b.root, err)
		}
	}
	b.lsp.StopGitHeadWatch()
	<-b.indexed
	if err := b.store.Close(); err != nil {
		log.Printf("Warning: closing store for %s: %v", b.root, err)
	}
	log.Printf("MCP workspace closed: %s", b.root)
}

// openStore opens the index at root with the recovery a long-running server
// needs, like cmd's openStoreForServer but returning errors instead of
// exiting: a session must survive a workspace that fails to open. A corrupt
// database or a populated index from an older format is deleted; the reopened
// empty store is then cold-built by the binding's initial index pass.
func openStore(root string) (*store.Store, error) {
	s, err := store.Open(root)
	if err != nil {
		log.Printf("Failed to open index at %s (%v), rebuilding from scratch...", root, err)
		removeIndexFiles(root)
		if s, err = store.Open(root); err != nil {
			return nil, fmt.Errorf("opening index at %s: %w", root, err)
		}
	}
	if stored := s.GetIndexVersion(); stored != version.IndexVersion && !s.IsEmpty() {
		log.Printf("Index version mismatch at %s (stored: %d, current: %d), rebuilding index...", root, stored, version.IndexVersion)
		if err := s.Close(); err != nil {
			log.Printf("Warning: closing outdated store: %v", err)
		}
		removeIndexFiles(root)
		if s, err = store.Open(root); err != nil {
			return nil, fmt.Errorf("reopening index at %s: %w", root, err)
		}
	}
	return s, nil
}

func removeIndexFiles(root string) {
	dbPath := store.DBPath(root)
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(p)
	}
}
