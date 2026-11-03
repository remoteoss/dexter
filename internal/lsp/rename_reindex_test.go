package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

// The index after a large rename must equal a cold build of the files on
// disk, and the batched reindex must be much faster than one transaction for
// each file.
func TestRename_LargeReindexIsBatchedAndMatchesColdBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 2,000 files")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the timing comparison is not reliable with Windows file system overhead")
	}
	const n = 2000
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	core := servers[0]
	indexFile(t, st, dir, "lib/target.ex", "defmodule MyApp.Target do\n  def foo(x), do: x\nend\n")
	paths := []string{filepath.Join(dir, "lib/target.ex")}
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("lib/callers/caller_%d.ex", i)
		indexFile(t, st, dir, rel, fmt.Sprintf("defmodule MyApp.Caller%d do\n  alias MyApp.Target\n  def run(x), do: Target.foo(x) + MyApp.Target.foo(x)\nend\n", i))
		paths = append(paths, filepath.Join(dir, rel))
	}

	indexed, err := core.RenameFunctionHeadless(context.Background(), "MyApp.Target", "foo", "bar")
	if err != nil {
		t.Fatal(err)
	}
	<-indexed

	// A cold build of the same files, in another store.
	coldDir := t.TempDir()
	cold, err := store.Open(coldDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cold.Close() }()
	for _, p := range paths {
		defs, refs, err := parser.ParseFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := cold.IndexFileWithRefs(p, defs, refs); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.Stats()
	want, _ := cold.Stats()
	if got != want {
		t.Errorf("index after the rename %+v, cold build %+v", got, want)
	}
	gotStates, _ := st.FileStates()
	wantStates, _ := cold.FileStates()
	for p, w := range wantStates {
		if g, ok := gotStates[p]; !ok || g.Mtime != w.Mtime {
			t.Errorf("%s: mtime %d, cold build %d", p, g.Mtime, w.Mtime)
			break
		}
	}
	refsOf := func(s *store.Store, fn string) []string {
		rs, _ := s.LookupReferences("MyApp.Target", fn)
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = fmt.Sprintf("%s:%d", r.FilePath, r.Line)
		}
		sort.Strings(out)
		return out
	}
	if g, w := refsOf(st, "bar"), refsOf(cold, "bar"); fmt.Sprint(g) != fmt.Sprint(w) || len(g) < n {
		t.Errorf("references to bar: %d after the rename, %d in a cold build", len(g), len(w))
	}
	if g := refsOf(st, "foo"); len(g) != 0 {
		t.Errorf("%d references to foo are left", len(g))
	}

	// The same files through the rename's reindex and one by one.
	touch := func() {
		later := time.Now().Add(time.Second)
		for _, p := range paths {
			_ = os.Chtimes(p, later, later)
		}
	}
	touch()
	start := time.Now()
	<-core.reindexAfterRename(nil, paths, nil)
	batched := time.Since(start)
	touch()
	start = time.Now()
	core.index.reindexing.Lock()
	core.index.writes.RLock()
	for _, p := range paths {
		_ = core.indexOneFileLocked(p)
	}
	core.index.writes.RUnlock()
	core.index.reindexing.Unlock()
	oneByOne := time.Since(start)
	t.Logf("reindex of %d files: batched %v, one by one %v", len(paths), batched, oneByOne)
	if batched*2 > oneByOne {
		t.Errorf("the batched reindex (%v) is not much faster than one by one (%v)", batched, oneByOne)
	}
}
