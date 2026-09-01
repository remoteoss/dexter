package mcp

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

// debounceWindow batches bursts of filesystem events (editor saves, git
// operations) into one reindex pass.
const debounceWindow = 300 * time.Millisecond

// Watcher keeps the index in sync with filesystem changes. Editors drive
// index updates through LSP events, but a headless MCP server gets none, so
// it watches the project tree directly (fsnotify).
type Watcher struct {
	fsw    *fsnotify.Watcher
	server *lsp.Server
	store  *store.Store
	root   string
	wg     sync.WaitGroup
}

// WatchFiles watches projectRoot recursively and incrementally reindexes
// Elixir files as they change. Index writes hold the server's reindex lock so
// they cannot interleave with a concurrent workspace reindex's walk-and-prune
// (which would drop a file indexed after the walk passed its directory). On
// event overflow the whole workspace is reindexed. Callers should treat an
// error as degraded service, not fatal: the index still updates on startup,
// on git branch switches, and via dexter_reindex.
func WatchFiles(server *lsp.Server, s *store.Store, projectRoot string) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{fsw: fsw, server: server, store: s, root: projectRoot}
	if err := w.watchTree(projectRoot); err != nil {
		_ = fsw.Close()
		return nil, err
	}
	w.wg.Add(1)
	go w.loop()
	return w, nil
}

// Close stops the watcher and waits for the event loop to exit.
func (w *Watcher) Close() error {
	err := w.fsw.Close()
	w.wg.Wait()
	return err
}

// skipDir reports whether a directory's subtree is not watched: build output,
// VCS metadata, and deps, which change only through mix and are covered by
// the startup reindex.
func skipDir(name string) bool {
	switch name {
	case "_build", ".git", "node_modules", "deps", ".dexter":
		return true
	}
	return false
}

// watchTree adds watches for root and every eligible directory below it.
func (w *Watcher) watchTree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if skipDir(d.Name()) {
			return filepath.SkipDir
		}
		return w.fsw.Add(path)
	})
}

func (w *Watcher) loop() {
	defer w.wg.Done()

	pending := make(map[string]struct{})
	var timer *time.Timer
	var timerC <-chan time.Time

	schedule := func() {
		if timer == nil {
			timer = time.NewTimer(debounceWindow)
			timerC = timer.C
		} else {
			timer.Reset(debounceWindow)
		}
	}

	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			// Directory events matter for watch maintenance; file events only
			// for Elixir sources. Everything else is noise.
			if parser.IsElixirFile(ev.Name) || ev.Op.Has(fsnotify.Create) || ev.Op.Has(fsnotify.Remove) || ev.Op.Has(fsnotify.Rename) {
				pending[ev.Name] = struct{}{}
				schedule()
			}
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				log.Printf("Warning: file watcher overflowed, reindexing workspace")
				w.server.ReindexWorkspace()
				continue
			}
			log.Printf("Warning: file watcher: %v", err)
		case <-timerC:
			timer = nil
			timerC = nil
			batch := pending
			pending = make(map[string]struct{})
			w.server.WithReindexLock(func() { w.apply(batch) })
		}
	}
}

// apply reconciles the index with a batch of changed paths.
func (w *Watcher) apply(batch map[string]struct{}) {
	for path := range batch {
		info, err := os.Stat(path)
		switch {
		case err != nil:
			w.removePath(path)
		case info.IsDir():
			if skipDir(filepath.Base(path)) {
				continue
			}
			// New directory (e.g. git checkout, mkdir && write): watch it and
			// index any Elixir files already inside, since their create events
			// may predate the watch.
			if err := w.watchTree(path); err != nil {
				log.Printf("Warning: watching %s: %v", path, err)
			}
			_ = parser.WalkElixirFiles(path, func(p string, _ fs.DirEntry) error {
				w.reindexFile(p)
				return nil
			})
		case parser.IsElixirFile(path):
			w.reindexFile(path)
		}
	}
}

func (w *Watcher) reindexFile(path string) {
	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		log.Printf("Warning: %s: %v", path, err)
		return
	}
	if err := w.store.IndexFileWithRefs(path, defs, refs); err != nil {
		log.Printf("Warning: %s: %v", path, err)
	}
}

// removePath drops a deleted file from the index. A deleted path may have
// been a directory, so entries under it are dropped too.
func (w *Watcher) removePath(path string) {
	_ = w.store.RemoveFile(path)
	stored, err := w.store.ListFilePaths()
	if err != nil {
		return
	}
	prefix := path + string(os.PathSeparator)
	var under []string
	for _, p := range stored {
		if strings.HasPrefix(p, prefix) {
			under = append(under, p)
		}
	}
	if len(under) > 0 {
		_ = w.store.RemoveFiles(under)
	}
}
