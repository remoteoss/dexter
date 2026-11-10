//go:build !darwin || !cgo

package workspace

import "time"

// watcherSettle is how long fsnotify can take to deliver an event that the
// kernel queued. See Runtime.FlushEvents.
const watcherSettle = 20 * time.Millisecond

func startPlatformWatcher(root string, callbacks WatchCallbacks) (watchBackend, string, error) {
	watcher, err := startFSNotifyWatcher(root, callbacks)
	return watcher, "fsnotify", err
}
