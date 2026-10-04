//go:build !darwin || !cgo

package workspace

func startPlatformWatcher(root string, callbacks WatchCallbacks) (watchBackend, string, error) {
	watcher, err := startFSNotifyWatcher(root, callbacks)
	return watcher, "fsnotify", err
}
