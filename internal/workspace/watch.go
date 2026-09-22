package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"

	"github.com/remoteoss/dexter/internal/parser"
)

// Watcher owns the one native recursive watcher for a workspace daemon.
type Watcher struct {
	fsw      *fsnotify.Watcher
	root     string
	onChange func(string)
	wg       sync.WaitGroup

	mu      sync.Mutex
	missing int // directories the kernel refused to watch
}

// Degraded reports whether any directory could not be watched. The runtime uses
// it to add a periodic reconciliation: a project tree that outgrew the kernel's
// watch limit still has to converge, just more slowly.
func (w *Watcher) Degraded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.missing > 0
}

// Watch starts recursive native watching. Dependencies are reconciled when
// their Mix manifests change instead of consuming one watch per deps directory.
// It fails only when no directory at all can be watched; a partial watcher is
// still valuable, and the runtime adds the periodic fallback for the rest.
func Watch(root string, onChange func(string)) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{fsw: fsw, root: root, onChange: onChange}
	if watched := w.watchTree(root); watched == 0 {
		_ = fsw.Close()
		return nil, fmt.Errorf("no directory under %s could be watched", root)
	}
	w.wg.Add(1)
	go w.loop()
	return w, nil
}

func (w *Watcher) Close() error {
	err := w.fsw.Close()
	w.wg.Wait()
	return err
}

func skipWatchDir(name string) bool {
	switch name {
	case "_build", ".git", "node_modules", "deps", ".dexter":
		return true
	}
	return false
}

// watchTree adds root and every directory below it, skipping build and VCS
// trees. A directory the kernel refuses — an inotify watch limit, a mount point
// with a different watch capability — is logged and skipped, and the count of
// directories left unwatchable is what makes the watcher degraded. Aborting the
// whole tree because one directory could not be watched would leave a large
// repository with no native watching at all, which is far worse.
func (w *Watcher) watchTree(root string) int {
	watched := 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if skipWatchDir(d.Name()) {
			return filepath.SkipDir
		}
		if addErr := w.fsw.Add(path); addErr != nil {
			w.mu.Lock()
			w.missing++
			w.mu.Unlock()
			log.Printf("Warning: cannot watch %s: %v", path, addErr)
			return nil
		}
		watched++
		return nil
	})
	if walkErr != nil {
		log.Printf("Warning: walking %s to add watches: %v", root, walkErr)
	}
	return watched
}

func (w *Watcher) loop() {
	defer w.wg.Done()
	for {
		select {
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			w.handle(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				// A root event forces the runtime's manifest/full-reconcile path.
				w.onChange(filepath.Join(w.root, "mix.lock"))
				continue
			}
			log.Printf("Warning: workspace watcher: %v", err)
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	path := ev.Name
	if ev.Op.Has(fsnotify.Create) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if skipWatchDir(filepath.Base(path)) {
				return
			}
			if added := w.watchTree(path); added == 0 {
				log.Printf("Warning: no directory under %s could be watched", path)
			}
			_ = parser.WalkElixirFiles(path, func(file string, _ fs.DirEntry) error {
				w.onChange(file)
				return nil
			})
			return
		}
	}

	base := filepath.Base(path)
	manifest := base == "mix.lock" || base == "mix.exs"
	if parser.IsElixirFile(path) || manifest || ev.Op.Has(fsnotify.Remove) || ev.Op.Has(fsnotify.Rename) {
		// Ignore events from explicitly skipped subtrees that can still be
		// delivered for a watched parent during directory replacement.
		rel, err := filepath.Rel(w.root, path)
		if err == nil {
			for _, part := range strings.Split(rel, string(os.PathSeparator)) {
				if skipWatchDir(part) && part != "deps" {
					return
				}
			}
		}
		w.onChange(path)
	}
}
