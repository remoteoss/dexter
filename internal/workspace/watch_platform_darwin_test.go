//go:build darwin && cgo

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsevents"
)

func TestFSEventsTranslatesEvents(t *testing.T) {
	root := t.TempDir()
	var paths []string
	full := 0
	w := &fseventsWatcher{
		root:      root,
		eventRoot: root,
		callbacks: WatchCallbacks{
			PathChanged:   func(path string) { paths = append(paths, path) },
			FullReconcile: func() { full++ },
		},
	}

	source := filepath.Join(root, "lib", "worker.ex")
	w.handle(fsevents.Event{Path: source, Flags: fsevents.ItemIsFile | fsevents.ItemModified})
	w.handle(fsevents.Event{Path: filepath.Join(root, "deps", "library", "lib", "ignored.ex"), Flags: fsevents.ItemIsFile | fsevents.ItemModified})
	w.handle(fsevents.Event{Path: filepath.Join(root, "lib"), Flags: fsevents.ItemIsDir | fsevents.ItemRenamed})
	w.handle(fsevents.Event{Path: root, Flags: fsevents.MustScanSubDirs | fsevents.UserDropped})

	if len(paths) != 1 || paths[0] != source {
		t.Fatalf("changed paths = %v, want [%s]", paths, source)
	}
	if full != 2 {
		t.Fatalf("full reconciliations = %d, want 2", full)
	}
}

func TestFSEventsReportsFileChange(t *testing.T) {
	root := t.TempDir()
	changes := make(chan string, 8)
	w, err := startFSEventsWatcher(root, WatchCallbacks{
		PathChanged:   func(path string) { changes <- path },
		FullReconcile: func() {},
	})
	if err != nil {
		t.Skipf("FSEvents unavailable: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	path := filepath.Join(root, "created.ex")
	if err := os.WriteFile(path, []byte("defmodule Created do\nend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case changed := <-changes:
			if changed == path {
				return
			}
		case <-deadline:
			t.Fatal("FSEvents did not report the created file")
		}
	}
}

func TestFSEventsFailureFallsBackToFSNotify(t *testing.T) {
	previous := newFSEventsBackend
	newFSEventsBackend = func(string, WatchCallbacks) (watchBackend, error) {
		return nil, errors.New("FSEvents unavailable")
	}
	t.Cleanup(func() { newFSEventsBackend = previous })

	watcher, kind, err := startPlatformWatcher(t.TempDir(), WatchCallbacks{
		PathChanged:     func(string) {},
		FullReconcile:   func() {},
		CoverageChanged: func(bool) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	if kind != "fsnotify" {
		t.Fatalf("watcher kind = %q, want fsnotify", kind)
	}
}
