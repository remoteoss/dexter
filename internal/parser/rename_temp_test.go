package parser

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// deadPID returns the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot run a child process")
	}
	return cmd.Process.Pid
}

func TestRemoveStaleRenameTemps(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if age > 0 {
			old := time.Now().Add(-age)
			_ = os.Chtimes(p, old, old)
		}
		return p
	}
	dead := write(RenameTempName("beta.ex", deadPID(t), 1), 0)
	live := write(RenameTempName("beta.ex", os.Getppid(), 2), 0)
	own := write(RenameTempName("beta.ex", os.Getpid(), 3), 0)
	oldLive := write(RenameTempName("gamma.ex", os.Getppid(), 4), 2*StaleRenameTempAge)
	other := write(".beta.ex.dexter-notes.tmp", 0)
	userFile := write(".beta.ex.dexter-12-x.tmp", 0)

	RemoveStaleRenameTemps(dir)
	for path, want := range map[string]bool{dead: false, live: true, own: true, oldLive: false, other: true, userFile: true} {
		_, err := os.Stat(path)
		if exists := err == nil; exists != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(path), exists, want)
		}
	}
}

// A start's reconciliation walk removes temp files that a stopped rename
// left, without a walk of its own.
func TestWalkElixirFilesRemovesStaleRenameTemps(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "lib", RenameTempName("beta.ex", deadPID(t), 1))
	if err := os.WriteFile(stale, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "lib", ".keep.tmp")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = WalkElixirFiles(dir, func(string, fs.DirEntry) error { return nil })
	if _, err := os.Stat(stale); err == nil {
		t.Error("the walk left a stale rename temp file")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("the walk removed a file that is not a rename temp file")
	}

	stale2 := filepath.Join(dir, "lib", RenameTempName("gamma.ex", deadPID(t), 2))
	if err := os.WriteFile(stale2, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = CollectElixirFilesParallel(dir)
	if _, err := os.Stat(stale2); err == nil {
		t.Error("the parallel walk left a stale rename temp file")
	}
}
