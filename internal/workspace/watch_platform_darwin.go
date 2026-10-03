//go:build darwin && cgo

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
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsevents"

	"github.com/remoteoss/dexter/internal/parser"
)

const fseventsLatency = 25 * time.Millisecond

type fseventsWatcher struct {
	root      string
	eventRoot string
	stream    *fsevents.EventStream
	callbacks WatchCallbacks
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool

	// tops are the nested worktrees this watcher knows, from git's records at
	// start and from .git files and directories that appear later. FSEvents
	// watches the whole tree, so events from inside a top are dropped with map
	// lookups. checking holds the tops that a checkTopLater timer waits on.
	tops     worktreeTops
	checking worktreeTops
}

var newFSEventsBackend = func(root string, callbacks WatchCallbacks) (watchBackend, error) {
	return startFSEventsWatcher(root, callbacks)
}

func startPlatformWatcher(root string, callbacks WatchCallbacks) (watchBackend, string, error) {
	watcher, err := newFSEventsBackend(root, callbacks)
	if err == nil {
		return watcher, "fsevents", nil
	}
	log.Printf("Warning: macOS FSEvents unavailable for %s: %v; using fsnotify", root, err)
	fallback, fallbackErr := startFSNotifyWatcher(root, callbacks)
	if fallbackErr != nil {
		return nil, "", errors.Join(fmt.Errorf("start FSEvents: %w", err), fmt.Errorf("start fsnotify: %w", fallbackErr))
	}
	return fallback, "fsnotify", nil
}

func startFSEventsWatcher(root string, callbacks WatchCallbacks) (*fseventsWatcher, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	eventRoot := absRoot
	if resolved, resolveErr := filepath.EvalSymlinks(absRoot); resolveErr == nil {
		eventRoot = resolved
	}
	stream := &fsevents.EventStream{
		Events:  make(chan []fsevents.Event, 256),
		Paths:   []string{eventRoot},
		Flags:   fsevents.FileEvents | fsevents.WatchRoot | fsevents.NoDefer,
		Latency: fseventsLatency,
	}
	if err := stream.Start(); err != nil {
		return nil, err
	}
	w := &fseventsWatcher{root: absRoot, eventRoot: eventRoot, stream: stream, callbacks: callbacks}
	for _, dir := range parser.NestedWorktreeTops(absRoot, eventRoot) {
		w.tops.add(dir)
	}
	w.wg.Add(1)
	go w.loop()
	return w, nil
}

func (w *fseventsWatcher) Close() error {
	w.closeOnce.Do(func() {
		w.closed.Store(true)
		w.stream.Flush(true)
		w.stream.Stop()
		close(w.stream.Events)
		w.wg.Wait()
	})
	return nil
}

func (w *fseventsWatcher) Degraded() bool { return false }

func (w *fseventsWatcher) loop() {
	defer w.wg.Done()
	for events := range w.stream.Events {
		for _, event := range events {
			w.handle(event)
		}
	}
}

func (w *fseventsWatcher) handle(event fsevents.Event) {
	flags := event.Flags
	if flags&fsevents.HistoryDone != 0 {
		return
	}
	if flags&(fsevents.MustScanSubDirs|fsevents.UserDropped|fsevents.KernelDropped|fsevents.EventIDsWrapped|fsevents.RootChanged|fsevents.Mount|fsevents.Unmount) != 0 {
		// Lost events can hide a top that went away or became plain.
		for _, dir := range w.tops.list() {
			w.checkTopLater(dir)
		}
		w.callbacks.FullReconcile()
		return
	}

	path, ok := w.workspacePath(event.Path)
	if !ok {
		return
	}
	// ignoredWatchPath drops every path with a .git part, so a worktree's .git
	// file is handled first.
	if filepath.Base(path) == ".git" && flags&fsevents.ItemIsFile != 0 {
		if dir := filepath.Dir(path); dir != w.root && !ignoredWatchPath(w.root, dir) && !w.tops.under(w.root, dir) {
			w.gitFileChanged(dir)
		}
		return
	}
	if ignoredWatchPath(w.root, path) || w.tops.under(w.root, path) {
		return
	}
	if flags&fsevents.ItemIsDir != 0 {
		if w.tops.has(path) {
			// Nothing was indexed from a top, so its removal needs no reconcile.
			if _, err := os.Stat(path); err != nil {
				w.tops.remove(path)
			}
			return
		}
		// A worktree moved or copied into place is a top from now on. The
		// runtime is told once, as for a new .git file, to drop anything
		// indexed from it; it needs no full reconcile.
		if flags&(fsevents.ItemCreated|fsevents.ItemRenamed) != 0 && parser.IsLinkedWorktree(path) {
			if w.tops.add(path) {
				w.callbacks.PathChanged(path)
			}
			return
		}
		if flags&(fsevents.ItemCreated|fsevents.ItemRemoved|fsevents.ItemRenamed) != 0 {
			w.callbacks.FullReconcile()
		}
		return
	}

	base := filepath.Base(path)
	manifest := base == "mix.lock" || base == "mix.exs"
	if parser.IsElixirFile(path) || manifest || flags&(fsevents.ItemRemoved|fsevents.ItemRenamed) != 0 {
		w.callbacks.PathChanged(path)
	}
}

// gitFileChanged handles a .git file that appeared, changed or went away in dir.
// A new worktree becomes a top, and the runtime drops anything indexed from it
// before its .git file appeared. A top whose .git file went away is checked
// again later, after git worktree remove has had time to delete the directory.
func (w *fseventsWatcher) gitFileChanged(dir string) {
	if parser.IsLinkedWorktree(dir) {
		if w.tops.add(dir) {
			w.callbacks.PathChanged(dir)
		}
		return
	}
	if w.tops.has(dir) {
		w.checkTopLater(dir)
	}
}

// checkTopLater indexes dir as a plain directory if, after the retry interval,
// it still exists and is no longer a worktree. While git still records it, it
// is checked again after each interval, until the record goes.
func (w *fseventsWatcher) checkTopLater(dir string) {
	if !w.checking.add(dir) {
		return
	}
	time.AfterFunc(watchRetryInterval, func() {
		w.checking.remove(dir)
		if w.closed.Load() {
			return
		}
		info, err := os.Stat(dir)
		if err == nil && info.IsDir() {
			if parser.IsLinkedWorktree(dir) {
				return
			}
			if recordedWorktree(w.root, dir) {
				w.checkTopLater(dir)
				return
			}
		}
		w.tops.remove(dir)
		if err != nil || !info.IsDir() {
			return
		}
		_ = parser.WalkElixirFiles(dir, func(file string, _ fs.DirEntry) error {
			w.callbacks.PathChanged(file)
			return nil
		})
	})
}

func (w *fseventsWatcher) workspacePath(eventPath string) (string, bool) {
	rel, err := filepath.Rel(w.eventRoot, filepath.Clean(eventPath))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(w.root, rel), true
}
