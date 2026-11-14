package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/daemon"
	"github.com/remoteoss/dexter/internal/store"
)

// pruneTree makes a home with an old index in code/, and a folder of
// repositories, code/ws, with its own old index and a clone shop that has
// its own (used) index.
func pruneTree(t *testing.T) (home, ws, shop string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	runtime, err := os.MkdirTemp("/tmp", "dxp") // short: a socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })
	t.Setenv("DEXTER_RUNTIME_DIR", runtime)
	ws = filepath.Join(home, "code", "ws")
	shop = filepath.Join(ws, "shop")
	for _, d := range []string{filepath.Join(ws, ".git"), filepath.Join(shop, ".git"), store.DBDir(shop), store.DBDir(ws), store.DBDir(filepath.Join(home, "code"))} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]int{
		filepath.Join(shop, "mix.exs"):               1,
		store.DBPath(shop):                           1,
		store.DBPath(ws):                             2 << 20,
		store.DBPath(ws) + "-wal":                    1 << 20,
		filepath.Join(store.DBDir(ws), ".gitignore"): 1,
		store.DBPath(filepath.Join(home, "code")):    1 << 20,
	}
	for path, size := range files {
		if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home, ws, shop
}

func TestPruneListsAndDeletesOldIndexes(t *testing.T) {
	home, ws, shop := pruneTree(t)

	var out bytes.Buffer
	if err := cmdPrune(filepath.Join(shop, "lib"), false, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{store.DBDir(ws) + " (3 MB)", store.DBDir(filepath.Join(home, "code")) + " (1 MB)", "dexter prune --delete"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the list does not say %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), store.DBDir(shop)) {
		t.Errorf("the project's own index is listed:\n%s", out.String())
	}
	if !store.HasIndex(ws) {
		t.Fatal("prune without --delete deleted an index")
	}

	out.Reset()
	if err := cmdPrune(shop, true, &out); err != nil {
		t.Fatalf("prune --delete: %v\n%s", err, out.String())
	}
	for _, dir := range []string{ws, filepath.Join(home, "code")} {
		if _, err := os.Stat(store.DBDir(dir)); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", store.DBDir(dir), err)
		}
	}
	if !store.HasIndex(shop) {
		t.Error("prune deleted the index of the project itself")
	}
	if !strings.Contains(out.String(), "Freed 4 MB") {
		t.Errorf("output does not say what was freed:\n%s", out.String())
	}
}

// At the root of a folder of repositories, its own index is old too.
func TestPruneAtAContainerRoot(t *testing.T) {
	_, ws, _ := pruneTree(t)
	var out bytes.Buffer
	if err := cmdPrune(ws, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), store.DBDir(ws)+" (3 MB)") {
		t.Errorf("the container's own index is not listed:\n%s", out.String())
	}
}

// An index whose workspace lock a daemon holds is not deleted.
func TestPruneSkipsAnIndexThatADaemonUses(t *testing.T) {
	_, ws, shop := pruneTree(t)
	ownership, _, err := daemon.AcquireOwnership(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ownership.Release() }()

	var out bytes.Buffer
	err = cmdPrune(shop, true, &out)
	if err == nil || !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("prune --delete = %v, want 1 of 2 not deleted", err)
	}
	if !strings.Contains(out.String(), "Skipped "+store.DBDir(ws)+": a Dexter daemon uses it") {
		t.Errorf("output does not say why it skipped:\n%s", out.String())
	}
	if !store.HasIndex(ws) {
		t.Error("an index that a daemon uses was deleted")
	}
}
