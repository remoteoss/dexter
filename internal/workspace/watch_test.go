package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

type watchAddStub struct {
	mu      sync.Mutex
	failing map[string]bool
	calls   []string
}

func (s *watchAddStub) add(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, path)
	if s.failing[path] {
		return errors.New("watch limit reached")
	}
	return nil
}

func (s *watchAddStub) setFailing(path string, failing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[path] = failing
}

func (s *watchAddStub) resetCalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

func (s *watchAddStub) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func newTestWatcher(add func(string) error, changed func(bool)) *fsnotifyWatcher {
	return &fsnotifyWatcher{add: add, failed: make(map[string]struct{}), onCoverageChange: changed}
}

func TestWatcherRetriesOnlyFailedDirectoriesAndReportsRestoration(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good")
	failed := filepath.Join(root, "failed")
	stillFailed := filepath.Join(root, "still-failed")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(failed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stillFailed, 0o755); err != nil {
		t.Fatal(err)
	}

	stub := &watchAddStub{failing: map[string]bool{failed: true, stillFailed: true}}
	var transitions []bool
	w := newTestWatcher(stub.add, func(degraded bool) { transitions = append(transitions, degraded) })
	w.watchTree(root)
	if got := w.failedDirectories(); !slices.Equal(got, []string{failed, stillFailed}) {
		t.Fatalf("failed directories = %v, want [%s %s]", got, failed, stillFailed)
	}
	if !slices.Equal(transitions, []bool{true}) {
		t.Fatalf("coverage transitions = %v, want [true]", transitions)
	}

	stub.resetCalls()
	w.retryFailed()
	if got := stub.paths(); !slices.Equal(got, []string{failed, stillFailed}) {
		t.Fatalf("retry paths = %v, want [%s %s]", got, failed, stillFailed)
	}
	if !slices.Equal(transitions, []bool{true}) {
		t.Fatalf("repeated failure emitted another transition: %v", transitions)
	}

	stub.setFailing(failed, false)
	stub.resetCalls()
	w.retryFailed()
	if got := stub.paths(); !slices.Equal(got, []string{failed, stillFailed}) {
		t.Fatalf("restoration retry paths = %v, want [%s %s]", got, failed, stillFailed)
	}
	if got := w.failedDirectories(); !slices.Equal(got, []string{stillFailed}) {
		t.Fatalf("failed directories after partial restoration = %v", got)
	}
	if !slices.Equal(transitions, []bool{true, true}) {
		t.Fatalf("coverage transitions = %v, want [true true]", transitions)
	}

	stub.setFailing(stillFailed, false)
	w.retryFailed()
	if !slices.Equal(transitions, []bool{true, true, false}) {
		t.Fatalf("coverage transitions = %v, want [true true false]", transitions)
	}
}

func TestWatcherTracksStartupTotalFailure(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "lib")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}

	stub := &watchAddStub{failing: map[string]bool{root: true, child: true}}
	var transitions []bool
	w := newTestWatcher(stub.add, func(degraded bool) { transitions = append(transitions, degraded) })
	if watched := w.watchTree(root); watched != 0 {
		t.Fatalf("watched %d directories during total failure", watched)
	}
	if got := w.failedDirectories(); !slices.Equal(got, []string{root, child}) {
		t.Fatalf("failed directories = %v, want [%s %s]", got, root, child)
	}
	if !slices.Equal(transitions, []bool{true}) {
		t.Fatalf("coverage transitions = %v, want [true]", transitions)
	}

	stub.setFailing(root, false)
	stub.setFailing(child, false)
	w.retryFailed()
	if !slices.Equal(transitions, []bool{true, true, false}) {
		t.Fatalf("coverage transitions after recovery = %v, want [true true false]", transitions)
	}
}

func TestWatcherTracksFailureForNewDirectory(t *testing.T) {
	root := t.TempDir()
	created := filepath.Join(root, "new")
	if err := os.Mkdir(created, 0o755); err != nil {
		t.Fatal(err)
	}

	stub := &watchAddStub{failing: map[string]bool{created: true}}
	var transitions []bool
	w := newTestWatcher(stub.add, func(degraded bool) { transitions = append(transitions, degraded) })
	w.watchTree(created)
	if got := w.failedDirectories(); !slices.Equal(got, []string{created}) {
		t.Fatalf("failed directories = %v, want [%s]", got, created)
	}
	nested := filepath.Join(created, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	stub.setFailing(created, false)
	stub.resetCalls()
	w.retryFailed()
	if got := stub.paths(); !slices.Equal(got, []string{created, nested}) {
		t.Fatalf("restoration watch paths = %v, want [%s %s]", got, created, nested)
	}
	if !slices.Equal(transitions, []bool{true, false}) {
		t.Fatalf("coverage transitions = %v, want [true false]", transitions)
	}
}

func TestIgnoredWatchPath(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		filepath.Join(root, ".git", "HEAD"),
		filepath.Join(root, "deps", "library", "lib", "module.ex"),
		filepath.Join(root, "assets", "node_modules", "package", "index.js"),
		filepath.Join(root, ".dexter", "index.db"),
	} {
		if !ignoredWatchPath(root, path) {
			t.Errorf("ignoredWatchPath(%q) = false", path)
		}
	}
	if path := filepath.Join(root, "apps", "my_app", "lib", "module.ex"); ignoredWatchPath(root, path) {
		t.Errorf("ignoredWatchPath(%q) = true", path)
	}
}
