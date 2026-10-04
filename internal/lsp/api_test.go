package lsp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A rename without an editor writes every changed file, reports the files, and
// returns only when the index shows the new name.
func TestRenameFunction_HeadlessWritesFilesAndIndex(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def fetch_user(id), do: id
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/caller.ex", `defmodule MyApp.Caller do
  def go(id), do: MyApp.Accounts.fetch_user(id)
end
`)
	callerPath := filepath.Join(server.projectRoot, "lib/caller.ex")

	summary, err := server.RenameFunction("MyApp.Accounts", "fetch_user", "get_user")
	if err != nil {
		t.Fatal(err)
	}

	if len(summary.FilesChanged) != 2 || len(summary.FilesFailed) != 0 {
		t.Errorf("FilesChanged = %v, FilesFailed = %v; want both files changed", summary.FilesChanged, summary.FilesFailed)
	}
	data, err := os.ReadFile(callerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "MyApp.Accounts.get_user(id)") {
		t.Errorf("caller not written to disk:\n%s", data)
	}
	// No wait here: RenameFunction returns after the index update.
	results, err := server.store.LookupFunction("MyApp.Accounts", "get_user")
	if err != nil || len(results) == 0 {
		t.Errorf("index not updated when the rename returned: %v, %v", results, err)
	}
}

// A file that the rename cannot write is reported, and is not counted as
// changed, so the caller can tell the user which files still use the old name.
func TestRenameFunction_ReportsFilesItCouldNotWrite(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a file that the process cannot write")
	}
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def fetch_user(id), do: id
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/caller.ex", `defmodule MyApp.Caller do
  def go(id), do: MyApp.Accounts.fetch_user(id)
end
`)
	callerPath := filepath.Join(server.projectRoot, "lib/caller.ex")
	if err := os.Chmod(callerPath, 0444); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(callerPath, 0644) }()

	summary, err := server.RenameFunction("MyApp.Accounts", "fetch_user", "get_user")
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.FilesFailed) != 1 || summary.FilesFailed[0] != callerPath || summary.FailureReason == "" {
		t.Errorf("FilesFailed = %v (%q), want %s", summary.FilesFailed, summary.FailureReason, callerPath)
	}
	for _, path := range summary.FilesChanged {
		if path == callerPath {
			t.Errorf("FilesChanged includes the file that could not be written: %v", summary.FilesChanged)
		}
	}
}

// Without an editor, nothing is open, so the server moves conventional files
// itself and the summary reports the move.
func TestRenameModule_HeadlessMovesFilesItself(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def list_users, do: []
end
`)
	oldPath := filepath.Join(server.projectRoot, "lib/accounts.ex")
	newPath := filepath.Join(server.projectRoot, "lib/auth.ex")

	summary, err := server.RenameModule("MyApp.Accounts", "MyApp.Auth")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(oldPath); err == nil {
		t.Error("expected accounts.ex to be gone")
	}
	data, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("expected auth.ex on disk: %v", err)
	}
	if !strings.Contains(string(data), "defmodule MyApp.Auth") {
		t.Errorf("expected 'defmodule MyApp.Auth', got:\n%s", data)
	}
	if summary.FilesMoved[oldPath] != newPath {
		t.Errorf("summary reports moves %v, want %s → %s", summary.FilesMoved, oldPath, newPath)
	}
	if results, err := server.store.LookupModule("MyApp.Auth"); err != nil || len(results) == 0 {
		t.Errorf("index not updated when the rename returned: %v, %v", results, err)
	}
}

// A namespace-only module rename keeps the conventional file path, so the
// summary reports no move and the file is renamed in place.
func TestRenameModule_NamespaceOnlyKeepsPath(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	path := filepath.Join(server.projectRoot, "lib", "user.ex")
	indexFile(t, server.store, server.projectRoot, "lib/user.ex", `defmodule MyApp.Accounts.User do
  def name(u), do: u.name
end
`)

	summary, err := server.RenameModule("MyApp.Accounts.User", "MyApp.Billing.User")
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.FilesMoved) != 0 {
		t.Errorf("namespace-only rename reported moves: %v", summary.FilesMoved)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file left its conventional path: %v", err)
	}
	if !strings.Contains(string(data), "defmodule MyApp.Billing.User") {
		t.Errorf("module not renamed in place:\n%s", data)
	}
}

func TestRenameValidation(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", `defmodule MyApp.Accounts do
  def fetch_user(id), do: id
  def list_users, do: []
end
`)
	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"invalid function name", func() error { _, err := server.RenameFunction("MyApp.Accounts", "fetch_user", "NotValid"); return err }, "invalid function name"},
		{"existing function", func() error {
			_, err := server.RenameFunction("MyApp.Accounts", "fetch_user", "list_users")
			return err
		}, "already exists"},
		{"missing function", func() error { _, err := server.RenameFunction("MyApp.Accounts", "missing", "other"); return err }, "not found"},
		{"invalid module name", func() error { _, err := server.RenameModule("MyApp.Accounts", "my_app"); return err }, "invalid module name"},
		{"missing module", func() error { _, err := server.RenameModule("MyApp.Missing", "MyApp.Other"); return err }, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// Regression: a rename that waited for the rename lock, or for the index to
// show the previous rename, ignored its context, so a rename that the client
// had canceled could still start to write.
func TestRenameContext_CanceledWhileWaitingChangesNothing(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", "defmodule MyApp.Accounts do\n  def fetch_user(id), do: id\nend\n")
	path := filepath.Join(server.projectRoot, "lib/accounts.ex")

	// Another rename holds the lock, and its index update has not finished.
	pending := make(chan struct{})
	server.index.renameSerial.Lock()
	server.index.renameIndexed = pending

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := server.RenameFunctionContext(ctx, "MyApp.Accounts", "fetch_user", "get_user")
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	server.index.renameSerial.Unlock() // the other rename's writes end; its index update does not

	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "canceled before it changed any file") {
			t.Fatalf("error = %v, want a cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the canceled rename kept waiting for the previous rename's index update")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "get_user") {
		t.Fatalf("a canceled rename wrote the file:\n%s", data)
	}
	close(pending)
}

// A rename canceled while another rename held the lock does not write once
// it gets the lock.
func TestRenameContext_CanceledWhileLockedChangesNothing(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	indexFile(t, server.store, server.projectRoot, "lib/accounts.ex", "defmodule MyApp.Accounts do\n  def fetch_user(id), do: id\nend\n")
	path := filepath.Join(server.projectRoot, "lib/accounts.ex")

	server.index.renameSerial.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := server.RenameModuleContext(ctx, "MyApp.Accounts", "MyApp.Users")
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	server.index.renameSerial.Unlock()

	if err := <-errc; err == nil || !strings.Contains(err.Error(), "canceled before it changed any file") {
		t.Fatalf("error = %v, want a cancellation", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "MyApp.Users") {
		t.Fatalf("a canceled rename wrote the file:\n%s", data)
	}
}
