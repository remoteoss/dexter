package stdlib

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const detectionTimeout = 5 * time.Second

// Cache persists the detected stdlib root across invocations.
// Implemented by the store.
type Cache interface {
	GetStdlibRoot() (string, bool)
	SetStdlibRoot(root string) error
}

// Resolve returns the Elixir lib root to index.
//
// Priority order:
//  1. explicitPath (from LSP initializationOptions.stdlibPath) — used as-is, not cached
//  2. DEXTER_ELIXIR_LIB_ROOT env var — used as-is, not cached
//  3. Cached value from the DB — used if the path still exists on disk
//  4. Detection: mise → asdf → executable path → elixir subprocess → login shell
//
// A freshly detected path is written back to the cache.
func Resolve(cache Cache, explicitPath string) (string, bool) {
	// Explicit overrides bypass the cache entirely — they are the source of truth.
	if explicitPath != "" {
		return explicitPath, true
	}
	if v := strings.TrimSpace(os.Getenv("DEXTER_ELIXIR_LIB_ROOT")); v != "" {
		return v, true
	}

	// Use the cached path if it still exists on disk.
	if cached, ok := cache.GetStdlibRoot(); ok && dirHasElixirSources(cached) {
		return cached, true
	}

	// Detect and persist the result.
	root, ok := DetectElixirLibRoot()
	if ok {
		_ = cache.SetStdlibRoot(root)
	}
	return root, ok
}

// DetectElixirLibRoot runs the full detection chain with no caching. It tries
// each strategy in order and returns the first path that contains Elixir sources.
func DetectElixirLibRoot() (string, bool) {
	for _, fn := range []func() (string, bool){
		deriveFromMise,
		deriveFromAsdf,
		deriveFromElixirExecutable,
		detectViaRuntime,
		detectViaLoginShell,
	} {
		if root, ok := fn(); ok {
			return root, true
		}
	}
	return "", false
}

// deriveFromMise scans the mise installs directory for Elixir versions.
func deriveFromMise() (string, bool) {
	dataDir := os.Getenv("MISE_DATA_DIR")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		dataDir = filepath.Join(home, ".local", "share", "mise")
	}
	return scanVersionsDir(filepath.Join(dataDir, "installs", "elixir"))
}

// deriveFromAsdf scans the asdf installs directory for Elixir versions.
func deriveFromAsdf() (string, bool) {
	dataDir := os.Getenv("ASDF_DATA_DIR")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		dataDir = filepath.Join(home, ".asdf")
	}
	return scanVersionsDir(filepath.Join(dataDir, "installs", "elixir"))
}

// scanVersionsDir looks for the latest valid Elixir install under a
// version-per-subdirectory layout (used by both mise and asdf).
func scanVersionsDir(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}

	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(dir, entry.Name(), "lib")
		if dirHasElixirSources(candidate) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}

	// Sort ascending and take the last entry to prefer the latest version.
	sort.Strings(candidates)
	return candidates[len(candidates)-1], true
}

// deriveFromElixirExecutable resolves the elixir binary via PATH and derives
// the lib root from the install prefix. Works for Homebrew and direct installs
// where the executable is a real symlink to the versioned binary.
func deriveFromElixirExecutable() (string, bool) {
	exe, err := exec.LookPath("elixir")
	if err != nil || exe == "" {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
		exe = resolved
	}
	// Common layout: <prefix>/bin/elixir → <prefix>/lib/
	prefix := filepath.Clean(filepath.Join(filepath.Dir(exe), ".."))
	candidate := filepath.Join(prefix, "lib")
	if dirHasElixirSources(candidate) {
		return candidate, true
	}
	return "", false
}

// detectViaRuntime asks the Elixir runtime directly. This starts a VM, so it
// is slower than the filesystem-based approaches above.
func detectViaRuntime() (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "elixir", "-e", "IO.puts(:code.lib_dir(:elixir))").Output()
	if err != nil {
		return "", false
	}
	return parseLibDirOutput(string(out))
}

// detectViaLoginShell retries through a login shell for environments where the
// editor strips PATH (common with mise/asdf shims not inherited by LSP servers).
func detectViaLoginShell() (string, bool) {
	shell := os.Getenv("SHELL")
	if shell == "" || !filepath.IsAbs(shell) {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", "elixir -e 'IO.puts(:code.lib_dir(:elixir))'")
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return "", false
	}
	return parseLibDirOutput(buf.String())
}

// parseLibDirOutput interprets the output of :code.lib_dir(:elixir) and returns
// the parent lib directory (which contains elixir/, eex/, mix/, etc.).
func parseLibDirOutput(output string) (string, bool) {
	elixirAppDir := strings.TrimSpace(output)
	if elixirAppDir == "" {
		return "", false
	}
	// :code.lib_dir(:elixir) → e.g. /path/to/elixir/1.x/lib/elixir
	// The parent contains all bundled OTP apps.
	libDir := filepath.Dir(elixirAppDir)
	if dirHasElixirSources(libDir) {
		return libDir, true
	}
	return "", false
}

func dirHasElixirSources(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "elixir", "lib", "enum.ex"))
	return err == nil
}
