//go:build darwin && cgo

package workspace

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
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
	w.wg.Add(1)
	go w.loop()
	return w, nil
}

func (w *fseventsWatcher) Close() error {
	w.closeOnce.Do(func() {
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
		w.callbacks.FullReconcile()
		return
	}

	path, ok := w.workspacePath(event.Path)
	if !ok {
		return
	}
	if ignoredWatchPath(w.root, path) {
		return
	}
	if flags&fsevents.ItemIsDir != 0 {
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

func (w *fseventsWatcher) workspacePath(eventPath string) (string, bool) {
	rel, err := filepath.Rel(w.eventRoot, filepath.Clean(eventPath))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Join(w.root, rel), true
}
