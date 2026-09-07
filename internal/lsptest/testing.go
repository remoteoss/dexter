package lsptest

import (
	"os"
	"testing"
	"time"
)

// T wraps a Client for use in a test: every error becomes t.Fatal, so test
// bodies read as a list of assertions rather than a chain of error checks.
type T struct {
	*Client
	t *testing.T
}

// StartT launches a server rooted at root and registers its shutdown with
// t.Cleanup. The server's stderr is discarded unless the test is verbose.
func StartT(t *testing.T, binary, root string) *T {
	t.Helper()
	var stderr *os.File
	if testing.Verbose() {
		stderr = os.Stderr
	}
	client, err := Start(binary, root, stderr)
	if err != nil {
		t.Fatalf("lsptest: %v", err)
	}
	t.Cleanup(client.Close)
	return &T{Client: client, t: t}
}

// SetTimeout changes the per-request deadline.
func (x *T) SetTimeout(d time.Duration) { x.Client.SetTimeout(d) }

// References returns the reference locations at a zero-based position.
func (x *T) References(path string, line, char int, includeDeclaration bool) []Location {
	x.t.Helper()
	locs, err := x.Client.References(path, line, char, includeDeclaration)
	if err != nil {
		x.t.Fatalf("lsptest: %v", err)
	}
	return locs
}

// Definition returns the definition locations at a zero-based position.
func (x *T) Definition(path string, line, char int) []Location {
	x.t.Helper()
	locs, err := x.Client.Definition(path, line, char)
	if err != nil {
		x.t.Fatalf("lsptest: %v", err)
	}
	return locs
}

// Hover returns the hover text at a zero-based position.
func (x *T) Hover(path string, line, char int) string {
	x.t.Helper()
	text, err := x.Client.Hover(path, line, char)
	if err != nil {
		x.t.Fatalf("lsptest: %v", err)
	}
	return text
}

// RefLines is References rendered through Lines, the form assertions compare.
func (x *T) RefLines(path string, line, char int, includeDeclaration bool) []string {
	x.t.Helper()
	return Lines(x.Root(), x.References(path, line, char, includeDeclaration))
}

// DefLines is Definition rendered through Lines.
func (x *T) DefLines(path string, line, char int) []string {
	x.t.Helper()
	return Lines(x.Root(), x.Definition(path, line, char))
}

// FindT is Find, failing the test when the needle is absent.
func FindT(t *testing.T, path, substr string, nth int) (line, char int) {
	t.Helper()
	line, char, err := Find(path, substr, nth)
	if err != nil {
		t.Fatalf("lsptest: %v", err)
	}
	return line, char
}
