package workspace

import (
	"path/filepath"
	"strings"
)

// WatchCallbacks translates platform events into workspace reconciliation work.
type WatchCallbacks struct {
	PathChanged     func(string)
	FullReconcile   func()
	CoverageChanged func(bool)
}

type watchBackend interface {
	Close() error
	Degraded() bool
}

// Watcher owns one recursive change source for a workspace daemon.
type Watcher struct {
	backend watchBackend
	kind    string
}

// Watch starts the best recursive watcher available on this platform.
func Watch(root string, callbacks WatchCallbacks) (*Watcher, error) {
	backend, kind, err := startPlatformWatcher(root, callbacks)
	if err != nil {
		return nil, err
	}
	return &Watcher{backend: backend, kind: kind}, nil
}

func (w *Watcher) Close() error { return w.backend.Close() }

func (w *Watcher) Degraded() bool { return w.backend.Degraded() }

func (w *Watcher) Kind() string { return w.kind }

func skipWatchDir(name string) bool {
	switch name {
	case "_build", ".git", "node_modules", "deps", ".dexter":
		return true
	}
	return false
}

func ignoredWatchPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if skipWatchDir(part) {
			return true
		}
	}
	return false
}
