package stdlib

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
//  4. Detection: mise where → asdf where → executable path → elixir subprocess → login shell
//
// A freshly detected path is written back to the cache.
func Resolve(cache Cache, explicitPath, projectRoot string) (string, bool) {
	// Explicit overrides bypass the cache entirely — they are the source of truth.
	if explicitPath != "" {
		return explicitPath, true
	}
	if v := strings.TrimSpace(os.Getenv("DEXTER_ELIXIR_LIB_ROOT")); v != "" {
		return v, true
	}

	// Ask the version manager for the active Elixir install. This is cheap enough
	// (~20ms) and catches version switches that leave the old path on disk.
	if root, ok := deriveFromVersionManager(projectRoot); ok {
		if cached, hasCached := cache.GetStdlibRoot(); !hasCached || cached != root {
			_ = cache.SetStdlibRoot(root)
		}
		return root, true
	}

	// No version manager available — use the cached path if it still exists.
	if cached, ok := cache.GetStdlibRoot(); ok && dirHasElixirSources(cached) {
		return cached, true
	}

	// Full detection as a last resort (includes subprocess-based strategies).
	root, ok := DetectElixirLibRoot(projectRoot)
	if ok {
		_ = cache.SetStdlibRoot(root)
	}
	return root, ok
}

// DetectElixirLibRoot runs the full detection chain with no caching. It tries
// each strategy in order and returns the first path that contains Elixir sources.
// projectRoot is used by mise/asdf to resolve the active version for the project.
func DetectElixirLibRoot(projectRoot string) (string, bool) {
	for _, fn := range []func() (string, bool){
		func() (string, bool) { return deriveFromMise(projectRoot) },
		func() (string, bool) { return deriveFromAsdf(projectRoot) },
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

// FindExecutable locates a tool the way a shell would, then the way a GUI
// editor's stripped PATH needs: PATH first, then the standard mise, asdf, and
// Homebrew locations. An editor-launched daemon often inherits a PATH that never
// ran the version manager's shell hook, and one failed detection would disable
// stdlib indexing (and formatting) for every frontend sharing that daemon.
func FindExecutable(name string) (string, bool) {
	if path, err := exec.LookPath(name); err == nil {
		return path, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	for _, candidate := range []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, ".local", "share", "mise", "bin", name),
		filepath.Join(home, ".local", "share", "mise", "shims", name),
		filepath.Join(home, ".asdf", "bin", name),
		filepath.Join(home, ".asdf", "shims", name),
		filepath.Join("/opt/homebrew", "bin", name),
		filepath.Join("/usr/local", "bin", name),
	} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

// FindViaLoginShell asks the user's login shell where a tool is. It is the last
// resort for an editor-launched process whose PATH never ran the mise or asdf
// hook; a login shell does.
func FindViaLoginShell(name string) (string, bool) {
	shell := os.Getenv("SHELL")
	if shell == "" || !filepath.IsAbs(shell) {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, shell, "-l", "-c", "command -v "+name).Output()
	if err != nil {
		return "", false
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", false
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", false
	}
	return path, true
}

// deriveFromVersionManager tries mise then asdf. Used by Resolve to validate
// the cache before falling back to the full detection chain.
func deriveFromVersionManager(projectRoot string) (string, bool) {
	if root, ok := deriveFromMise(projectRoot); ok {
		return root, true
	}
	return deriveFromAsdf(projectRoot)
}

// deriveFromMise asks mise for the active Elixir install path for the project.
func deriveFromMise(projectRoot string) (string, bool) {
	mise, ok := FindExecutable("mise")
	if !ok {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	args := []string{"where", "elixir"}
	if projectRoot != "" {
		args = append(args, "-C", projectRoot)
	}
	out, err := exec.CommandContext(ctx, mise, args...).Output()
	if err != nil {
		return "", false
	}
	installDir := strings.TrimSpace(string(out))
	if installDir == "" {
		return "", false
	}
	candidate := filepath.Join(installDir, "lib")
	if dirHasElixirSources(candidate) {
		return candidate, true
	}
	return "", false
}

// deriveFromAsdf asks asdf for the active Elixir install path for the project.
func deriveFromAsdf(projectRoot string) (string, bool) {
	asdf, ok := FindExecutable("asdf")
	if !ok {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, asdf, "where", "elixir")
	if projectRoot != "" {
		cmd.Dir = projectRoot
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	installDir := strings.TrimSpace(string(out))
	if installDir == "" {
		return "", false
	}
	candidate := filepath.Join(installDir, "lib")
	if dirHasElixirSources(candidate) {
		return candidate, true
	}
	return "", false
}

// deriveFromElixirExecutable resolves the elixir binary via PATH and derives
// the lib root from the install prefix. Works for Homebrew and direct installs
// where the executable is a real symlink to the versioned binary.
func deriveFromElixirExecutable() (string, bool) {
	exe, ok := FindExecutable("elixir")
	if !ok {
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
	elixir, ok := FindExecutable("elixir")
	if !ok {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), detectionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, elixir, "-e", "IO.puts(:code.lib_dir(:elixir))").Output()
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
