//go:build darwin && cgo

package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsevents"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/pathcase"
)

const fseventsLatency = 25 * time.Millisecond

// watcherSettle is how long FSEvents can hold an event before it delivers it:
// the stream latency, twice for margin. See Runtime.FlushEvents.
const watcherSettle = 2 * fseventsLatency

type fseventsWatcher struct {
	root      string
	eventRoot string
	stream    *fsevents.EventStream
	callbacks WatchCallbacks
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool
	// stop ends the loop. The events channel stays open: the FSEvents
	// callback sends on it without a check, so a close could make a late
	// callback panic.
	stop chan struct{}

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
	// The runtime tells the user, through Watcher.Fallback.
	fallback, fallbackErr := startFSNotifyWatcher(root, callbacks)
	if fallbackErr != nil {
		return nil, "", errors.Join(fmt.Errorf("start FSEvents: %w", err), fmt.Errorf("start fsnotify: %w", fallbackErr))
	}
	if w, ok := fallback.(*fsnotifyWatcher); ok {
		w.fallbackReason = fmt.Errorf("macOS FSEvents: %w", err)
	}
	return fallback, "fsnotify", nil
}

func startFSEventsWatcher(root string, callbacks WatchCallbacks) (*fseventsWatcher, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// FSEvents reports paths with symlinks resolved and in the case that the
	// file system stores. EvalSymlinks keeps the case that the caller typed,
	// so a root typed in another case on a case-insensitive volume would make
	// every event path fall outside the root, and the watcher would drop all
	// of them in silence.
	eventRoot := absRoot
	if resolved, resolveErr := filepath.EvalSymlinks(absRoot); resolveErr == nil {
		eventRoot = pathcase.DiskSpelling(resolved)
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
	w := &fseventsWatcher{root: absRoot, eventRoot: eventRoot, stream: stream, callbacks: callbacks, stop: make(chan struct{})}
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
		close(w.stop)
		w.wg.Wait()
	})
	return nil
}

func (w *fseventsWatcher) Degraded() bool { return false }

func (w *fseventsWatcher) loop() {
	defer w.wg.Done()
	for {
		select {
		case events := <-w.stream.Events:
			for _, event := range events {
				w.callbacks.handleSafely(func() { w.handle(event) })
			}
		case <-w.stop:
			return
		}
	}
}

func (w *fseventsWatcher) handle(event fsevents.Event) {
	flags := event.Flags
	if flags&fsevents.HistoryDone != 0 {
		return
	}
	if flags&(fsevents.RootChanged|fsevents.Unmount) != 0 && rootGone(w.root) {
		// The volume was unmounted, or the root moved or went away. The index
		// lives under the root, and SQLite maps its shared memory file: any
		// further access to it can end the process with SIGBUS. A reconcile
		// would also prune every file. The runtime stops instead.
		if w.callbacks.RootGone != nil {
			w.callbacks.RootGone()
		}
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
		if flags&(fsevents.ItemCreated|fsevents.ItemRenamed) != 0 {
			// One read of the .git file classifies the directory: two reads can
			// see an empty file and then a complete one while git writes it.
			switch parser.GitFile(path) {
			case parser.WorktreeGitFile:
				if w.tops.add(path) {
					w.callbacks.PathChanged(path)
				}
				return
			case parser.UnsettledGitFile:
				// A worktree that git is still moving has an empty .git file.
				// It is a top until it is checked again, so that no full
				// reconcile indexes it in the meantime.
				if w.tops.add(path) {
					w.callbacks.PathChanged(path)
				}
				w.checkTopLater(path)
				return
			}
		}
		if flags&(fsevents.ItemCreated|fsevents.ItemRemoved|fsevents.ItemRenamed) != 0 {
			w.directoryChanged(path, flags)
		}
		return
	}

	base := filepath.Base(path)
	manifest := base == "mix.lock" || base == "mix.exs"
	if parser.IsElixirFile(path) || manifest || flags&(fsevents.ItemRemoved|fsevents.ItemRenamed) != 0 {
		if flags&fsevents.ItemRenamed != 0 && w.callbacks.PathRenamed != nil {
			// A file renamed soon after a write is too new for the age rule
			// of the spelling check.
			w.callbacks.PathRenamed(path)
			return
		}
		w.callbacks.PathChanged(path)
	}
}

// directoryChanged reconciles the subtree of a directory that was created,
// removed or renamed, not the whole workspace. A walk of every file for each
// new directory makes a burst of them, such as a code generator or an
// unpacked archive, cost one full walk each on a large repository.
//
// A directory that is gone is sent as one path: the runtime removes everything
// indexed below it with one range read. A directory that is there is walked,
// as the fsnotify watcher walks a new directory, because a directory that is
// moved into place brings files that have no events of their own. A rename or
// removal of a directory that is there now can also have replaced files that
// were indexed below it, so the directory is sent too, and the runtime drops
// the indexed files below it that are gone. Lost events still ask for a full
// reconciliation in handle.
func (w *fseventsWatcher) directoryChanged(path string, flags fsevents.EventFlags) {
	if path == w.root {
		// The stream can deliver the creation of the root itself, from just
		// before it started; the walk at start covers that. A root that was
		// replaced or moved needs the full reconcile.
		if flags&(fsevents.ItemRemoved|fsevents.ItemRenamed) != 0 {
			w.callbacks.FullReconcile()
		}
		return
	}
	// After a rename that changes only the case (mv lib/Area lib/area), a
	// stat of the old spelling succeeds. It is gone all the same: a walk of
	// it would report its files in the old spelling.
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || !pathcase.SpelledBelow(w.root, path) {
		w.callbacks.PathChanged(path)
		return
	}
	if flags&(fsevents.ItemRemoved|fsevents.ItemRenamed) != 0 {
		w.callbacks.PathChanged(path)
	}
	w.walkNewDirectory(path)
}

// walkNewDirectory reports the Elixir files below dir that a full walk would
// index, with the skip rules of the index walks (parser.SkipDir: a deps
// directory is walked, as the full walk does), and a nested worktree below
// dir becomes a top, as a new .git file makes one. dir
// itself was classified by handle. A directory that cannot be read is skipped,
// as the other walkers skip it.
func (w *fseventsWatcher) walkNewDirectory(dir string) {
	var walk func(dir string, top bool)
	walk = func(dir string, top bool) {
		entries, err := parser.ReadDirUnsorted(dir)
		if err != nil {
			return
		}
		if !top {
			if w.tops.has(dir) {
				return
			}
			// One read of the .git file classifies the directory: two reads can
			// see an empty file and then a complete one while git writes it.
			switch parser.GitFileFromEntries(dir, entries) {
			case parser.WorktreeGitFile:
				if w.tops.add(dir) {
					w.callbacks.PathChanged(dir)
				}
				return
			case parser.UnsettledGitFile:
				if w.tops.add(dir) {
					w.callbacks.PathChanged(dir)
				}
				w.checkTopLater(dir)
				return
			}
		}
		for _, e := range entries {
			name := e.Name()
			path := filepath.Join(dir, name)
			if e.IsDir() {
				if !parser.SkipDir(name) {
					walk(path, false)
				}
				continue
			}
			if parser.IsElixirFile(name) {
				w.callbacks.PathChanged(path)
			}
		}
	}
	walk(dir, true)
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
			state := parser.GitFile(dir)
			if state == parser.WorktreeGitFile {
				return
			}
			// git may still be writing the .git file of a worktree it moved.
			if state == parser.UnsettledGitFile || recordedWorktree(w.root, dir) {
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
