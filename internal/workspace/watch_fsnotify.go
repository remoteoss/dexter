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
	remove           func(string) error
	watchList        func() []string
	wg               sync.WaitGroup

	// tops are the nested worktrees found so far. Only a top itself is watched,
	// for its .git file. pending holds tops whose .git file went away; the retry
	// tick checks them, after git worktree remove has had time to finish. Only
	// the loop goroutine uses pending.
	tops    worktreeTops
	pending map[string]struct{}

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
		remove:          fsw.Remove,
		watchList:       fsw.WatchList,
		failed:          make(map[string]struct{}),
	}
	// Worktrees that git records but whose .git file is gone are skipped by the
	// walkers, so they are tops too; the rest are found by the walk below.
	for _, dir := range parser.NestedWorktreeTops(root) {
		w.tops.add(dir)
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
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || skipWatchDir(info.Name()) {
		return 0
	}
	watched := 0
	var walk func(dir string)
	walk = func(dir string) {
		if dir != root || includeRoot {
			if addErr := w.add(dir); addErr != nil {
				w.setFailed(dir, true)
				log.Printf("Warning: cannot watch %s: %v", dir, addErr)
			} else {
				w.setFailed(dir, false)
				watched++
			}
		}
		// A top stays watched, so that a change to its .git file is seen.
		if dir != w.root && w.tops.has(dir) {
			return
		}
		entries, err := parser.ReadDirUnsorted(dir)
		if err != nil {
			return
		}
		// The entries show a nested worktree without another syscall.
		if dir != w.root && parser.HasLinkedWorktreeGitFile(dir, entries) {
			w.tops.add(dir)
			return
		}
		for _, e := range entries {
			if e.IsDir() && !skipWatchDir(e.Name()) {
				walk(filepath.Join(dir, e.Name()))
			}
		}
	}
	walk(root)
	return watched
}

// unwatchBelow drops the watches below dir, which turned out to be a nested
// worktree after it was watched, and forgets their failures so the retry timer
// does not add them back. It reads the watch list in memory rather than the
// tree on disk, which can be a whole checkout.
func (w *fsnotifyWatcher) unwatchBelow(dir string) {
	prefix := dir + string(filepath.Separator)
	if w.watchList != nil && w.remove != nil {
		for _, path := range w.watchList() {
			if strings.HasPrefix(path, prefix) {
				_ = w.remove(path)
			}
		}
	}
	for _, path := range w.failedDirectories() {
		if strings.HasPrefix(path, prefix) {
			w.setFailed(path, false)
		}
	}
}

// checkPending handles tops whose .git file went away. A top that is gone, or
// is a worktree again, needs nothing. A top that git still records is checked
// again on the next tick, until the record goes. A top that is now a plain
// directory is watched and indexed like any new directory.
func (w *fsnotifyWatcher) checkPending() {
	var recorded []string
	for dir := range w.pending {
		delete(w.pending, dir)
		info, err := os.Stat(dir)
		if err == nil && info.IsDir() {
			if parser.IsLinkedWorktree(dir) {
				continue
			}
			if recordedWorktree(w.root, dir) {
				recorded = append(recorded, dir)
				continue
			}
		}
		w.tops.remove(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		w.watchTree(dir)
		_ = parser.WalkElixirFiles(dir, func(file string, _ fs.DirEntry) error {
			w.onChange(file)
			return nil
		})
	}
	for _, dir := range recorded {
		w.markPending(dir)
	}
}

func (w *fsnotifyWatcher) markPending(dir string) {
	if w.pending == nil {
		w.pending = make(map[string]struct{})
	}
	w.pending[dir] = struct{}{}
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
		if err != nil || !info.IsDir() || w.tops.under(w.root, path) {
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
				// Lost events can hide a top that went away or became plain.
				for _, dir := range w.tops.list() {
					w.markPending(dir)
				}
				w.onFullReconcile()
				continue
			}
			log.Printf("Warning: workspace watcher: %v", err)
		case <-ticker.C:
			w.retryFailed()
			w.checkPending()
		}
	}
}

func (w *fsnotifyWatcher) handle(ev fsnotify.Event) {
	path := ev.Name
	base := filepath.Base(path)
	dir := filepath.Dir(path)
	gone := ev.Op.Has(fsnotify.Remove) || ev.Op.Has(fsnotify.Rename)

	// A nested worktree top is watched only for its .git file. Nothing was
	// indexed from it, so its own removal needs no work either.
	if w.tops.has(dir) {
		if base == ".git" && gone {
			w.markPending(dir)
		}
		return
	}
	if gone && w.tops.has(path) {
		w.tops.remove(path)
		delete(w.pending, path)
		return
	}
	// Events from deeper inside a top can still be queued from before it
	// turned out to be a worktree.
	if w.tops.under(w.root, path) {
		return
	}

	if ev.Op.Has(fsnotify.Create) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			if skipWatchDir(base) {
				return
			}
			if added := w.watchTree(path); added == 0 {
				log.Printf("Warning: no directory under %s could be watched", path)
			}
			w.retryFailedUnder(path)
			if w.tops.has(path) {
				return
			}
			_ = parser.WalkElixirFiles(path, func(file string, _ fs.DirEntry) error {
				w.onChange(file)
				return nil
			})
			return
		}
	}

	if base == ".git" {
		// git worktree add creates the directory before its .git file, and cp -r
		// can copy subdirectories first, so a tree can be watched before it
		// turns out to be a worktree. Anything indexed from it before then is
		// removed when the runtime is told about the directory. The file can be
		// empty when its Create event is read, so its Write is checked as well.
		if dir != w.root && (ev.Op.Has(fsnotify.Create) || ev.Op.Has(fsnotify.Write)) && parser.IsLinkedWorktree(dir) && w.tops.add(dir) {
			w.unwatchBelow(dir)
			w.onChange(dir)
		}
		return
	}
	manifest := base == "mix.lock" || base == "mix.exs"
	if parser.IsElixirFile(path) || manifest || gone {
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
