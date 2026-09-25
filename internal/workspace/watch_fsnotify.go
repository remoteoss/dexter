package workspace

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/remoteoss/dexter/internal/parser"
)

type fsnotifyWatcher struct {
	fsw              *fsnotify.Watcher
	root             string
	onChange         func(string)
	onFullReconcile  func()
	onCoverageChange func(bool)
	add              func(string) error
	wg               sync.WaitGroup

	mu     sync.Mutex
	failed map[string]struct{}
}

var watchRetryInterval = 5 * time.Second

// Degraded reports whether any directory could not be watched.
func (w *fsnotifyWatcher) Degraded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.failed) > 0
}

func (w *fsnotifyWatcher) failedDirectories() []string {
	w.mu.Lock()
	paths := make([]string, 0, len(w.failed))
	for path := range w.failed {
		paths = append(paths, path)
	}
	w.mu.Unlock()
	sort.Strings(paths)
	return paths
}

func startFSNotifyWatcher(root string, callbacks WatchCallbacks) (watchBackend, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &fsnotifyWatcher{
		fsw:             fsw,
		root:            root,
		onChange:        callbacks.PathChanged,
		onFullReconcile: callbacks.FullReconcile,
		add:             fsw.Add,
		failed:          make(map[string]struct{}),
	}
	if watched := w.watchTree(root); watched == 0 {
		log.Printf("Warning: no directory under %s could be watched", root)
	}
	// The runtime's initial index covers startup gaps. Report only later coverage
	// edges, which need their own reconciliation, and try transient failures once
	// before waiting for the retry timer.
	w.retryFailed()
	w.onCoverageChange = callbacks.CoverageChanged
	w.wg.Add(1)
	go w.loop()
	return w, nil
}

func (w *fsnotifyWatcher) Close() error {
	err := w.fsw.Close()
	w.wg.Wait()
	return err
}

// watchTree adds root and every directory below it, skipping build and VCS
// trees. A directory the kernel refuses — an inotify watch limit, a mount point
// with a different watch capability — is logged and skipped, and the count of
// directories left unwatchable is what makes the watcher degraded. Aborting the
// whole tree because one directory could not be watched would leave a large
// repository with no native watching at all, which is far worse.
func (w *fsnotifyWatcher) watchTree(root string) int {
	return w.walkDirectories(root, true)
}

func (w *fsnotifyWatcher) walkDirectories(root string, includeRoot bool) int {
	watched := 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if skipWatchDir(d.Name()) {
			return filepath.SkipDir
		}
		if path != w.root && parser.IsLinkedWorktree(path) {
			return filepath.SkipDir
		}
		if path == root && !includeRoot {
			return nil
		}
		if addErr := w.add(path); addErr != nil {
			w.setFailed(path, true)
			log.Printf("Warning: cannot watch %s: %v", path, addErr)
			return nil
		}
		w.setFailed(path, false)
		watched++
		return nil
	})
	if walkErr != nil {
		log.Printf("Warning: walking %s to add watches: %v", root, walkErr)
	}
	return watched
}

// setFailed emits only coverage edges. The callback runs without mu held because
// it can enqueue runtime work and must not block watch registration or shutdown.
func (w *fsnotifyWatcher) setFailed(path string, failed bool) {
	w.mu.Lock()
	wasDegraded := len(w.failed) > 0
	_, wasFailed := w.failed[path]
	if failed {
		w.failed[path] = struct{}{}
	} else {
		delete(w.failed, path)
	}
	isDegraded := len(w.failed) > 0
	w.mu.Unlock()
	// Every restored subtree needs a reconciliation, even if another failed
	// directory keeps the watcher degraded.
	if ((!wasDegraded && isDegraded) || (!failed && wasFailed)) && w.onCoverageChange != nil {
		w.onCoverageChange(isDegraded)
	}
}

func (w *fsnotifyWatcher) retryFailed() {
	w.retryPaths(w.failedDirectories())
}

func (w *fsnotifyWatcher) retryFailedUnder(root string) {
	paths := w.failedDirectories()
	selected := paths[:0]
	for _, path := range paths {
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			selected = append(selected, path)
		}
	}
	w.retryPaths(selected)
}

func (w *fsnotifyWatcher) retryPaths(paths []string) {
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			w.setFailed(path, false)
			continue
		}
		if err := w.add(path); err != nil {
			continue
		}
		// The recovered parent watch closes the race with this walk. Add any
		// descendants created while the parent had no coverage before restoring.
		w.walkDirectories(path, false)
		w.setFailed(path, false)
	}
}

func (w *fsnotifyWatcher) loop() {
	defer w.wg.Done()
	ticker := time.NewTicker(watchRetryInterval)
	defer ticker.Stop()
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
				w.onFullReconcile()
				continue
			}
			log.Printf("Warning: workspace watcher: %v", err)
		case <-ticker.C:
			w.retryFailed()
		}
	}
}

func (w *fsnotifyWatcher) handle(ev fsnotify.Event) {
	path := ev.Name
	if ev.Op.Has(fsnotify.Create) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if skipWatchDir(filepath.Base(path)) {
				return
			}
			if added := w.watchTree(path); added == 0 {
				log.Printf("Warning: no directory under %s could be watched", path)
			}
			w.retryFailedUnder(path)
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
