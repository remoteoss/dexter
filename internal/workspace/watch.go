package workspace

import (
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/parser"
)

// WatchCallbacks translates platform events into workspace reconciliation work.
type WatchCallbacks struct {
	PathChanged   func(string)
	FullReconcile func()
	// CoverageChanged runs when the watcher becomes degraded, and when a
	// directory that it could not watch is watched again. Both need a
	// reconciliation.
	CoverageChanged func(bool)
	// CoverageCountChanged runs when the set of directories that cannot be
	// watched changed without such an edge, for example when a second
	// directory fails. It needs no reconciliation; it keeps the report
	// current.
	CoverageCountChanged func()
	// RootGone reports that the project root is not there any more, for
	// example because its volume was unmounted. It may be nil.
	RootGone func()
	// Panicked reports a panic that the watcher recovered while it handled an
	// event. The watcher then asks for a full reconcile, because the event is
	// lost. It may be nil.
	Panicked func(what string, value any, stack []byte)
}

// handleSafely runs one event handler and keeps a panic in it from stopping
// the process; the event is lost, so a full reconcile replaces it.
func (c WatchCallbacks) handleSafely(handle func()) {
	defer func() {
		value := recover()
		if value == nil {
			return
		}
		if c.Panicked != nil {
			c.Panicked("the file watcher", value, debug.Stack())
		} else {
			log.Printf("Error: panic in the file watcher: %v\n%s", value, debug.Stack())
		}
		if c.FullReconcile != nil {
			c.FullReconcile()
		}
	}()
	handle()
}

// rootGone reports whether root is not there any more.
func rootGone(root string) bool {
	info, err := os.Stat(root)
	return lsp.IsMissing(err) || (err == nil && !info.IsDir())
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

// FailedDirectories lists the directories that the watcher cannot watch now,
// sorted. Only a per-directory backend can have them.
func (w *Watcher) FailedDirectories() []string {
	if b, ok := w.backend.(interface{ failedDirectories() []string }); ok {
		return b.failedDirectories()
	}
	return nil
}

// Fallback reports why the preferred backend of the platform could not start,
// or nil when it runs.
func (w *Watcher) Fallback() error {
	if b, ok := w.backend.(interface{ fallback() error }); ok {
		return b.fallback()
	}
	return nil
}

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

// worktreeTops is the set of nested worktree tops that a watcher knows. With it,
// a watcher drops events from inside a worktree with map lookups only, and it
// can find a top whose .git file went away.
type worktreeTops struct {
	mu   sync.Mutex
	tops map[string]struct{}
}

func (s *worktreeTops) add(dir string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tops == nil {
		s.tops = make(map[string]struct{})
	}
	if _, ok := s.tops[dir]; ok {
		return false
	}
	s.tops[dir] = struct{}{}
	return true
}

func (s *worktreeTops) remove(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tops, dir)
}

func (s *worktreeTops) has(dir string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.tops[dir]
	return ok
}

func (s *worktreeTops) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tops := make([]string, 0, len(s.tops))
	for dir := range s.tops {
		tops = append(tops, dir)
	}
	return tops
}

// under reports whether path lies below a known top, not counting the top.
func (s *worktreeTops) under(root, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tops) == 0 {
		return false
	}
	for dir := filepath.Dir(path); len(dir) > len(root); dir = filepath.Dir(dir) {
		if _, ok := s.tops[dir]; ok {
			return true
		}
	}
	return false
}

// recordedWorktree reports whether git still records dir, a known nested
// worktree top without a linked .git file, as a worktree of root's repository.
// It does while git worktree remove deletes the checkout, and until git
// worktree prune after the .git file alone is deleted. The walkers skip such a
// directory, so the watchers must not index it either. It reads git's records,
// so it is only for the rare check of a top whose .git file went away.
func recordedWorktree(root, dir string) bool {
	return slices.Contains(parser.NestedWorktreeTops(root), dir)
}
