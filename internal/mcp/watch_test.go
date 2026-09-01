package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/store"
)

// eventually polls cond until it returns true or the deadline passes.
// Filesystem notification latency varies by platform, so watcher assertions
// must poll rather than sleep.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func setupWatcher(t *testing.T) (*store.Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	w, err := WatchFiles(s, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := w.Close(); err != nil {
			t.Errorf("closing watcher: %v", err)
		}
	})
	return s, root
}

func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func moduleIndexed(s *store.Store, module string) func() bool {
	return func() bool {
		results, err := s.LookupModule(module)
		return err == nil && len(results) > 0
	}
}

func TestWatcher_NewFileIndexed(t *testing.T) {
	s, root := setupWatcher(t)
	writeFile(t, root, "lib/fresh.ex", "defmodule MyApp.Fresh do\n  def hello, do: :ok\nend\n")
	eventually(t, "new file to be indexed", moduleIndexed(s, "MyApp.Fresh"))
}

func TestWatcher_ModifiedFileReindexed(t *testing.T) {
	s, root := setupWatcher(t)
	path := writeFile(t, root, "lib/acc.ex", "defmodule MyApp.Acc do\n  def old_fun, do: :ok\nend\n")
	eventually(t, "initial index", func() bool {
		r, _ := s.LookupFunction("MyApp.Acc", "old_fun")
		return len(r) > 0
	})

	if err := os.WriteFile(path, []byte("defmodule MyApp.Acc do\n  def new_fun, do: :ok\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}
	eventually(t, "modified file to be reindexed", func() bool {
		newR, _ := s.LookupFunction("MyApp.Acc", "new_fun")
		oldR, _ := s.LookupFunction("MyApp.Acc", "old_fun")
		return len(newR) > 0 && len(oldR) == 0
	})
}

func TestWatcher_DeletedFileRemoved(t *testing.T) {
	s, root := setupWatcher(t)
	path := writeFile(t, root, "lib/gone.ex", "defmodule MyApp.Gone do\nend\n")
	eventually(t, "initial index", moduleIndexed(s, "MyApp.Gone"))

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleted file to leave the index", func() bool {
		results, err := s.LookupModule("MyApp.Gone")
		return err == nil && len(results) == 0
	})
}

func TestWatcher_DeletedDirectoryRemoved(t *testing.T) {
	s, root := setupWatcher(t)
	writeFile(t, root, "lib/sub/a.ex", "defmodule MyApp.Sub.A do\nend\n")
	writeFile(t, root, "lib/sub/b.ex", "defmodule MyApp.Sub.B do\nend\n")
	eventually(t, "initial index", func() bool {
		a, _ := s.LookupModule("MyApp.Sub.A")
		b, _ := s.LookupModule("MyApp.Sub.B")
		return len(a) > 0 && len(b) > 0
	})

	if err := os.RemoveAll(filepath.Join(root, "lib/sub")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "deleted directory's files to leave the index", func() bool {
		a, _ := s.LookupModule("MyApp.Sub.A")
		b, _ := s.LookupModule("MyApp.Sub.B")
		return len(a) == 0 && len(b) == 0
	})
}

func TestWatcher_NewDirectoryWatched(t *testing.T) {
	s, root := setupWatcher(t)
	// Create the directory and its file separately so the file event can only
	// be seen by a watch added after the directory appeared.
	if err := os.MkdirAll(filepath.Join(root, "lib/newdir"), 0755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	writeFile(t, root, "lib/newdir/mod.ex", "defmodule MyApp.NewDir.Mod do\nend\n")
	eventually(t, "file in new directory to be indexed", moduleIndexed(s, "MyApp.NewDir.Mod"))
}

func TestWatcher_SkipsDepsAndNonElixir(t *testing.T) {
	s, root := setupWatcher(t)
	writeFile(t, root, "deps/pkg/lib/dep.ex", "defmodule DepPkg.Ignored do\nend\n")
	writeFile(t, root, "lib/notes.txt", "defmodule NotElixir do\nend\n")

	// Anchor on a real file so the negative checks below observe a watcher
	// that has demonstrably processed events.
	writeFile(t, root, "lib/anchor.ex", "defmodule MyApp.Anchor do\nend\n")
	eventually(t, "anchor file to be indexed", moduleIndexed(s, "MyApp.Anchor"))

	if r, _ := s.LookupModule("DepPkg.Ignored"); len(r) != 0 {
		t.Error("file under deps/ was indexed by the watcher")
	}
	if r, _ := s.LookupModule("NotElixir"); len(r) != 0 {
		t.Error("non-Elixir file was indexed")
	}
}
