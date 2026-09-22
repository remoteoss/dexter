package stdlib

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCache implements Cache for testing.
type fakeCache struct {
	value string
}

func (f *fakeCache) GetStdlibRoot() (string, bool) {
	if f.value == "" {
		return "", false
	}
	return f.value, true
}

func (f *fakeCache) SetStdlibRoot(root string) error {
	f.value = root
	return nil
}

// makeElixirLibDir creates a directory structure that passes dirHasElixirSources.
func makeElixirLibDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	enumPath := filepath.Join(dir, "elixir", "lib", "enum.ex")
	if err := os.MkdirAll(filepath.Dir(enumPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enumPath, []byte("defmodule Enum do\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolve_ExplicitPathBypassesCache(t *testing.T) {
	cache := &fakeCache{value: "/some/cached/path"}
	explicit := t.TempDir()

	root, ok := Resolve(cache, explicit, "")
	if !ok {
		t.Fatal("expected ok")
	}
	if root != explicit {
		t.Errorf("got %q, want %q", root, explicit)
	}
	// Cache should not be overwritten.
	if cache.value != "/some/cached/path" {
		t.Errorf("cache should not be modified when explicit path is given")
	}
}

func TestResolve_EnvVarBypassesCache(t *testing.T) {
	envDir := t.TempDir()
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", envDir)

	cache := &fakeCache{value: "/some/cached/path"}

	root, ok := Resolve(cache, "", "")
	if !ok {
		t.Fatal("expected ok")
	}
	if root != envDir {
		t.Errorf("got %q, want %q", root, envDir)
	}
	// Cache should not be overwritten.
	if cache.value != "/some/cached/path" {
		t.Errorf("cache should not be modified when env var is set")
	}
}

func TestResolve_ValidCacheUsedWhenNoVersionManager(t *testing.T) {
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", "")
	t.Setenv("PATH", t.TempDir()) // no mise/asdf/elixir in PATH
	t.Setenv("HOME", t.TempDir()) // and none in the standard install locations
	t.Setenv("SHELL", "/bin/false")

	libDir := makeElixirLibDir(t)
	cache := &fakeCache{value: libDir}

	root, ok := Resolve(cache, "", "")
	if !ok {
		t.Fatal("expected ok")
	}
	if root != libDir {
		t.Errorf("got %q, want %q", root, libDir)
	}
}

func TestResolve_VersionManagerOverridesCache(t *testing.T) {
	if _, err := exec.LookPath("mise"); err != nil {
		t.Skip("mise not available in PATH")
	}
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", "")

	// Cache points to a fake dir that still has valid Elixir sources.
	staleDir := makeElixirLibDir(t)
	cache := &fakeCache{value: staleDir}

	root, ok := Resolve(cache, "", "")
	if !ok {
		t.Skip("mise has no elixir version configured")
	}
	// The result should come from mise, not the stale cache.
	if root == staleDir {
		t.Error("expected version manager to override stale cache")
	}
	if !dirHasElixirSources(root) {
		t.Errorf("version-manager result %q does not contain Elixir sources", root)
	}
	// Cache should be updated.
	if cache.value != root {
		t.Errorf("cache should be updated to %q, got %q", root, cache.value)
	}
}

func TestResolve_StaleCacheTriggersRedetection(t *testing.T) {
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", "")
	t.Setenv("PATH", t.TempDir())

	// Point cache at a path that doesn't exist.
	cache := &fakeCache{value: "/nonexistent/path/that/does/not/exist"}

	// Detection will fail too (no real Elixir), but the important thing is
	// the stale cache was not returned.
	root, _ := Resolve(cache, "", "")
	if root == "/nonexistent/path/that/does/not/exist" {
		t.Error("should not return stale cached path")
	}
}

func TestResolve_DetectedPathIsWrittenToCache(t *testing.T) {
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", "")

	cache := &fakeCache{}

	root, ok := Resolve(cache, "", "")
	if !ok {
		t.Skip("detection did not succeed (no Elixir install found)")
	}
	if cache.value == "" {
		t.Error("expected cache to be populated after successful detection")
	}
	if cache.value != root {
		t.Errorf("cache value %q does not match returned root %q", cache.value, root)
	}
}

func TestDirHasElixirSources(t *testing.T) {
	dir := makeElixirLibDir(t)
	if !dirHasElixirSources(dir) {
		t.Error("expected true for valid lib dir")
	}
	if dirHasElixirSources(t.TempDir()) {
		t.Error("expected false for empty dir")
	}
	if dirHasElixirSources("/nonexistent") {
		t.Error("expected false for nonexistent dir")
	}
}

func TestDeriveFromMise(t *testing.T) {
	if _, err := exec.LookPath("mise"); err != nil {
		t.Skip("mise not available in PATH")
	}
	root, ok := deriveFromMise("")
	if !ok {
		t.Skip("mise has no elixir version configured")
	}
	if !dirHasElixirSources(root) {
		t.Errorf("mise-derived path %q does not contain Elixir sources", root)
	}
}

func TestDeriveFromMise_RespectsProjectRoot(t *testing.T) {
	if _, err := exec.LookPath("mise"); err != nil {
		t.Skip("mise not available in PATH")
	}
	// Use the current working directory as project root — should resolve to
	// whatever version mise has active.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, ok := deriveFromMise(cwd)
	if !ok {
		t.Skip("mise has no elixir version configured for cwd")
	}
	if !dirHasElixirSources(root) {
		t.Errorf("mise-derived path %q does not contain Elixir sources", root)
	}
	// The path should contain the mise installs directory.
	if !strings.Contains(root, "mise") {
		t.Errorf("expected mise install path, got %q", root)
	}
}

// TestFindExecutableFindsVersionManagerLocations covers the editor case: a GUI
// process starts dexter with a PATH that never ran the mise or asdf hook, so
// the standard install locations have to be searched too.
func TestFindExecutableFindsVersionManagerLocations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // hide the real tools

	shim := filepath.Join(home, ".asdf", "shims", "mix")
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, ok := FindExecutable("mix")
	if !ok || got != shim {
		t.Fatalf("FindExecutable(mix) = %q, %v; want %q", got, ok, shim)
	}
	if _, ok := FindExecutable("definitely-not-a-tool"); ok {
		t.Fatal("FindExecutable found a tool that does not exist")
	}
}
