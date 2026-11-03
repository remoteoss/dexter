package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.lsp.dev/protocol"
)

// stdlibEvents records the stdlib roots that sessions ask the workspace for,
// and applies them to the shared root as the runtime does.
type stdlibEvents struct {
	recordingWorkspaceEvents
	index *IndexCoordinator
	mu    sync.Mutex
	roots []string
}

func (e *stdlibEvents) SetStdlibRoot(_ context.Context, path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, changed := e.index.setStdlibRoot(path); changed {
		e.roots = append(e.roots, path)
	}
	return nil
}

// Two editors with different stdlibPath values switched the shared root back
// and forth, and each switch queued a full pass. The first explicit value is
// used for the life of the workspace; a later editor that asks for another is
// told, and only that editor.
func TestFirstExplicitStdlibPathWinsAndOthersAreWarned(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SHELL", "/bin/false")
	t.Setenv("HOME", t.TempDir())
	base, cleanup := setupTestServer(t)
	defer cleanup()
	index := NewIndexCoordinator()
	events := &stdlibEvents{index: index}
	newSession := func() *Server {
		return NewServerWithOptions(base.store, base.projectRoot, ServerOptions{Index: index, Events: events})
	}
	firstPath, secondPath := t.TempDir(), t.TempDir()
	initialize := func(s *Server, path string) {
		t.Helper()
		if _, err := s.Initialize(context.Background(), &protocol.InitializeParams{
			InitializationOptions: map[string]interface{}{"stdlibPath": path},
		}); err != nil {
			t.Fatal(err)
		}
	}

	first := newSession()
	initialize(first, firstPath)
	firstClient := attachFakeEditor(t, first, false)
	second := newSession()
	initialize(second, secondPath)
	secondClient := attachFakeEditor(t, second, false)
	third := newSession()
	initialize(third, firstPath)

	if got := index.getStdlibRoot(); got != firstPath {
		t.Fatalf("shared stdlib root = %s, want the first explicit %s", got, firstPath)
	}
	events.mu.Lock()
	roots := append([]string(nil), events.roots...)
	events.mu.Unlock()
	if len(roots) != 1 || roots[0] != firstPath {
		t.Fatalf("the shared root changed to %v, want only [%s]", roots, firstPath)
	}
	got := secondClient.WaitMessage(t, reportWait, protocol.MessageTypeWarning, "this editor sets stdlibPath to "+secondPath)
	if !strings.Contains(got.Message, "Dexter uses "+firstPath) {
		t.Errorf("the warning does not name the root in use: %q", got.Message)
	}
	time.Sleep(50 * time.Millisecond)
	if n := countMessages(firstClient, "stdlibPath"); n != 0 {
		t.Errorf("the first editor was told about the stdlibPath of another:\n%s", firstClient.Dump())
	}
}

// toolchainEnv makes an environment where mise and the login shell record
// their calls and find nothing. It returns the bin directory and a function
// that counts the recorded calls that contain text.
func toolchainEnv(t *testing.T) (string, func(text string) int) {
	t.Helper()
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	for name, script := range map[string]string{
		"mise":  "#!/bin/sh\necho \"mise $*\" >> " + calls + "\nexit 1\n",
		"shell": "#!/bin/sh\necho \"shell $*\" >> " + calls + "\nsleep 0.2\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("SHELL", filepath.Join(bin, "shell"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", "")
	return bin, func(text string) int {
		data, _ := os.ReadFile(calls)
		n := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, text) {
				n++
			}
		}
		return n
	}
}

func initializeSession(t *testing.T, base *Server, index *IndexCoordinator) *Server {
	t.Helper()
	session := NewServerWithOptions(base.store, base.projectRoot, ServerOptions{Index: index, Events: &stdlibEvents{index: index}})
	if _, err := session.Initialize(context.Background(), &protocol.InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	return session
}

// Detection of the standard library runs version managers and login shells,
// with a timeout of 5 s each. Each editor after the first must reuse the
// result, a result that found nothing too, until a version file changes.
func TestEditorsReuseStdlibDetection(t *testing.T) {
	_, count := toolchainEnv(t)
	base, cleanup := setupTestServer(t)
	defer cleanup()
	index := NewIndexCoordinator()

	initializeSession(t, base, index)
	first := count("mise")
	if first == 0 {
		t.Fatal("the first editor did not detect")
	}
	for i := 0; i < 2; i++ {
		initializeSession(t, base, index)
	}
	if n := count("mise"); n != first {
		t.Fatalf("later editors ran %d more stdlib detections, want 0: the first result is reused", n-first)
	}

	// A change of the version files of the project detects again.
	writeTestFile(t, base.projectRoot, ".tool-versions", "elixir 1.18.0\n")
	initializeSession(t, base, index)
	if count("mise") == first {
		t.Fatal("a changed .tool-versions did not detect again")
	}
}

// Editors that open together share one search for mix, a search that found
// nothing too, instead of each running a login shell.
func TestEditorsThatStartTogetherShareTheMixSearch(t *testing.T) {
	_, count := toolchainEnv(t)
	base, cleanup := setupTestServer(t)
	defer cleanup()
	index := NewIndexCoordinator()
	initializeSession(t, base, index) // detects the standard library first
	before := count("command -v mix")

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := initializeSession(t, base, index)
			if !session.mixMissing {
				t.Error("a fake environment without mix found mix")
			}
		}()
	}
	wg.Wait()
	if n := count("command -v mix") - before; n != 1 {
		t.Fatalf("3 editors that started together searched for mix %d times, want 1", n)
	}
}

// The Warning tells the user to install mix and restart the editor. The
// restart must find it: a search that found nothing was reused for 5 minutes.
func TestMixIsFoundAfterInstallAndRestart(t *testing.T) {
	bin, _ := toolchainEnv(t)
	base, cleanup := setupTestServer(t)
	defer cleanup()
	index := NewIndexCoordinator()
	if session := initializeSession(t, base, index); !session.mixMissing {
		t.Fatal("a fake environment without mix found mix")
	}
	mix := filepath.Join(bin, "mix")
	if err := os.WriteFile(mix, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if session := initializeSession(t, base, index); session.mixBin != mix {
		t.Fatalf("after mix was installed, the next editor has mix %q, want %s", session.mixBin, mix)
	}
}

// A daemon that found no standard library at start keeps an empty shared
// root. When a later editor finds one, after the user installed it, the
// workspace uses it, as the Warning says.
func TestLaterEditorFillsTheEmptySharedStdlib(t *testing.T) {
	_, _ = toolchainEnv(t)
	base, cleanup := setupTestServer(t)
	defer cleanup()
	index := NewIndexCoordinator()
	initializeSession(t, base, index)
	if root := index.getStdlibRoot(); root != "" {
		t.Fatalf("shared root = %q in an environment without Elixir", root)
	}
	installed := t.TempDir()
	writeTestFile(t, installed, "elixir/lib/enum.ex", "defmodule Enum do\nend\n")
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", installed)
	initializeSession(t, base, index)
	if root := index.getStdlibRoot(); root != installed {
		t.Fatalf("shared root = %q after a later editor found %s", root, installed)
	}
}

// Two spellings of one stdlibPath are one path: no conflict is reported.
func TestStdlibPathSpellingsAreOnePath(t *testing.T) {
	index := NewIndexCoordinator()
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, ok := index.claimExplicitStdlib(dir); !ok {
		t.Fatal("the first claim was refused")
	}
	for _, spelling := range []string{dir + "/", dir + "/.", link, link + "/"} {
		if inUse, ok := index.claimExplicitStdlib(spelling); !ok {
			t.Errorf("%s was refused as a conflict with %s", spelling, inUse)
		}
	}
	if _, ok := index.claimExplicitStdlib(t.TempDir()); ok {
		t.Error("another directory was accepted")
	}
}
