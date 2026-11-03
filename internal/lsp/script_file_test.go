package lsp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestScriptsOfTwoBuildsDoNotMix: two Dexter builds on one machine wrote and
// read the same beam_server.exs, so a BEAM could start from a partial file, a
// missing one, or the other build's script. Each content now has its own
// file, written atomically, and a reader always gets its own complete script.
func TestScriptsOfTwoBuildsDoNotMix(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scripts")
	contents := []string{
		strings.Repeat("IO.puts(:build_a)\n", 20000),
		strings.Repeat("IO.puts(:build_bb)\n", 20000),
	}
	var wg sync.WaitGroup
	errs := make(chan string, 1000)
	for _, content := range contents {
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(content string) {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					path, err := installScript(dir, "beam_server", ".exs", content)
					if err != nil {
						errs <- err.Error()
						return
					}
					got, err := os.ReadFile(path)
					if err != nil {
						errs <- err.Error()
						return
					}
					if string(got) != content {
						errs <- "a reader got a file that is not its own complete script"
						return
					}
				}
			}(content)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("files %v, want one script for each content and no temporary file", names)
	}
}

// TestScriptDirIsPrivate: a script directory that another user can write
// would let that user replace the script that this user runs.
func TestScriptDirIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scripts")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o777)
	if _, err := installScript(dir, "x", ".exs", "x"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		t.Fatalf("script directory mode %v; others can write it", info.Mode().Perm())
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	if _, err := installScript(link, "x", ".exs", "x"); err == nil {
		t.Fatal("a symlinked script directory was accepted")
	}
}

func scriptName(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "beam_server-" + hex.EncodeToString(sum[:8]) + ".exs"
}

// TestInstallScriptReplacesWhatItMustNotRun: a symlink at the script's path,
// even to a file with the right content, or a file that others can write, is
// replaced by a regular private file; it is never reused.
func TestInstallScriptReplacesWhatItMustNotRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scripts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "IO.puts(:ok)\n"
	path := filepath.Join(dir, scriptName(content))
	target := filepath.Join(t.TempDir(), "elsewhere.exs")
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skip(err)
	}
	got, err := installScript(dir, "beam_server", ".exs", content)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(got); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the script path is not a regular file: %v, %v", info.Mode(), err)
	}
	if err := os.Chmod(got, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := installScript(dir, "beam_server", ".exs", content); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(got); err != nil || info.Mode().Perm()&0o022 != 0 {
		t.Fatalf("a script that others can write was reused: %v, %v", info.Mode(), err)
	}
}

// TestFormatterScriptFallsBackAndReportsWhenNoDirectoryWorks: another user
// can create the temp directory first; the cache directory is used then. When
// no directory works, the editors are told, because every BEAM start fails.
func TestFormatterScriptFallsBackAndReportsWhenNoDirectoryWorks(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil { // not a directory
		t.Fatal(err)
	}
	fallback := filepath.Join(t.TempDir(), "cache", "scripts")
	path, err := installScriptAnywhere([]string{blocked, fallback}, "beam_server", ".exs", "x")
	if err != nil || filepath.Dir(path) != fallback {
		t.Fatalf("installScriptAnywhere = %q, %v; want a path in %s", path, err, fallback)
	}

	server, cleanup := setupTestServer(t)
	defer cleanup()
	server.index.reporter.SetLogf(func(string, ...any) {})
	previous := scriptDirCandidates
	scriptDirCandidates = func() []string { return []string{blocked} }
	t.Cleanup(func() { scriptDirCandidates = previous })
	if _, err := server.startBeamProcess(server.projectRoot); err == nil {
		t.Fatal("the BEAM started without a script")
	}
	if !server.index.reporter.Active(condFormatterScript) {
		t.Fatal("the editors were not told that the formatter script cannot be written")
	}
}
