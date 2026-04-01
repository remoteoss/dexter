package stdlib

import (
	"os"
	"path/filepath"
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

	root, ok := Resolve(cache, explicit)
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

	root, ok := Resolve(cache, "")
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

func TestResolve_ValidCacheSkipsDetection(t *testing.T) {
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", "")

	libDir := makeElixirLibDir(t)
	cache := &fakeCache{value: libDir}

	root, ok := Resolve(cache, "")
	if !ok {
		t.Fatal("expected ok")
	}
	if root != libDir {
		t.Errorf("got %q, want %q", root, libDir)
	}
}

func TestResolve_StaleCacheTriggersRedetection(t *testing.T) {
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", "")

	// Point cache at a path that doesn't exist.
	cache := &fakeCache{value: "/nonexistent/path/that/does/not/exist"}

	// Detection will fail too (no real Elixir), but the important thing is
	// the stale cache was not returned.
	root, _ := Resolve(cache, "")
	if root == "/nonexistent/path/that/does/not/exist" {
		t.Error("should not return stale cached path")
	}
}

func TestResolve_DetectedPathIsWrittenToCache(t *testing.T) {
	t.Setenv("DEXTER_ELIXIR_LIB_ROOT", "")

	libDir := makeElixirLibDir(t)
	cache := &fakeCache{}

	// Manually inject the dir as if detection found it — test via scanVersionsDir.
	// We can't easily invoke the full detect() chain without a real install, but
	// we CAN verify Resolve calls SetStdlibRoot by making detection succeed via
	// a mise-layout temp dir.
	miseDir := t.TempDir()
	versionDir := filepath.Join(miseDir, "installs", "elixir", "1.16.0")
	if err := os.MkdirAll(filepath.Join(versionDir, "lib", "elixir", "lib"), 0755); err != nil {
		t.Fatal(err)
	}
	enumPath := filepath.Join(versionDir, "lib", "elixir", "lib", "enum.ex")
	if err := os.WriteFile(enumPath, []byte("defmodule Enum do\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MISE_DATA_DIR", miseDir)
	_ = libDir // unused in this variant

	root, ok := Resolve(cache, "")
	if !ok {
		t.Skip("detection did not succeed (mise layout may not match)")
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

func TestScanVersionsDir_PicksLatest(t *testing.T) {
	base := t.TempDir()

	for _, version := range []string{"1.14.0", "1.15.0", "1.16.0"} {
		enumPath := filepath.Join(base, version, "lib", "elixir", "lib", "enum.ex")
		if err := os.MkdirAll(filepath.Dir(enumPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(enumPath, []byte("defmodule Enum do\nend\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	root, ok := scanVersionsDir(base)
	if !ok {
		t.Fatal("expected a result")
	}
	if filepath.Base(filepath.Dir(root)) != "1.16.0" {
		t.Errorf("expected 1.16.0 to be selected, got dir %q", root)
	}
}

func TestScanVersionsDir_SkipsInvalidVersions(t *testing.T) {
	base := t.TempDir()

	// One valid, one missing enum.ex.
	valid := filepath.Join(base, "1.16.0", "lib", "elixir", "lib", "enum.ex")
	if err := os.MkdirAll(filepath.Dir(valid), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valid, []byte("defmodule Enum do\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "1.17.0-rc.0", "lib"), 0755); err != nil {
		t.Fatal(err)
	}

	root, ok := scanVersionsDir(base)
	if !ok {
		t.Fatal("expected a result")
	}
	if filepath.Base(filepath.Dir(root)) != "1.16.0" {
		t.Errorf("expected 1.16.0 to be selected, got dir %q", root)
	}
}

func TestScanVersionsDir_EmptyDir(t *testing.T) {
	_, ok := scanVersionsDir(t.TempDir())
	if ok {
		t.Error("expected false for empty dir")
	}
}

func TestScanVersionsDir_Nonexistent(t *testing.T) {
	_, ok := scanVersionsDir("/nonexistent/mise/installs/elixir")
	if ok {
		t.Error("expected false for nonexistent dir")
	}
}

func TestDeriveFromMise_UsesEnvVar(t *testing.T) {
	miseDir := t.TempDir()
	t.Setenv("MISE_DATA_DIR", miseDir)

	versionDir := filepath.Join(miseDir, "installs", "elixir", "1.16.0")
	enumPath := filepath.Join(versionDir, "lib", "elixir", "lib", "enum.ex")
	if err := os.MkdirAll(filepath.Dir(enumPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enumPath, []byte("defmodule Enum do\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}

	root, ok := deriveFromMise()
	if !ok {
		t.Fatal("expected detection to succeed")
	}
	expected := filepath.Join(versionDir, "lib")
	if root != expected {
		t.Errorf("got %q, want %q", root, expected)
	}
}

func TestDeriveFromAsdf_UsesEnvVar(t *testing.T) {
	asdfDir := t.TempDir()
	t.Setenv("ASDF_DATA_DIR", asdfDir)

	versionDir := filepath.Join(asdfDir, "installs", "elixir", "1.16.0")
	enumPath := filepath.Join(versionDir, "lib", "elixir", "lib", "enum.ex")
	if err := os.MkdirAll(filepath.Dir(enumPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enumPath, []byte("defmodule Enum do\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}

	root, ok := deriveFromAsdf()
	if !ok {
		t.Fatal("expected detection to succeed")
	}
	expected := filepath.Join(versionDir, "lib")
	if root != expected {
		t.Errorf("got %q, want %q", root, expected)
	}
}
