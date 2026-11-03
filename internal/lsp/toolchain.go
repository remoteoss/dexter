package lsp

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/remoteoss/dexter/internal/stdlib"
)

// toolchainTTL bounds how long a detection result is reused when nothing that
// selects the Elixir version of the project changed. A variable so tests can
// shrink it.
var toolchainTTL = 5 * time.Minute

// toolchainCache keeps the last detections of one workspace: the Elixir
// standard library, and mix where the standard library does not give it.
// Detection can run version managers and login shells with a timeout of 5 s
// each, and it finds the same result for every editor of the workspace, so
// each editor after the first reuses it, a result that found nothing too. mu
// is held during a detection: a second editor that starts at the same time
// waits for it instead of running its own.
type toolchainCache struct {
	mu sync.Mutex

	stdlibSet   bool
	stdlibAt    time.Time
	stdlibStamp string
	stdlibRoot  string

	mixSet bool
	mixAt  time.Time
	mixBin string
}

// toolchainStamp identifies what selects the Elixir install of root: the
// version files of mise and asdf, and the environment variable that names
// the standard library.
func toolchainStamp(root string) string {
	stamp := os.Getenv("DEXTER_ELIXIR_LIB_ROOT")
	for _, name := range []string{".tool-versions", "mise.toml", ".mise.toml", ".elixir-version"} {
		stamp += fmt.Sprint("|", statFileStamp(filepath.Join(root, name)))
	}
	return stamp
}

// DetectStdlib returns the Elixir standard library of root, or "" when there
// is none. It detects again only when the cached result is older than
// toolchainTTL, when a version file of the project changed, or when the found
// library is gone (an Elixir version that was uninstalled).
func (c *IndexCoordinator) DetectStdlib(cache stdlib.Cache, root string) string {
	t := &c.toolchain
	t.mu.Lock()
	defer t.mu.Unlock()
	stamp := toolchainStamp(root)
	if t.stdlibSet && time.Since(t.stdlibAt) < toolchainTTL && t.stdlibStamp == stamp &&
		(t.stdlibRoot == "" || fileExists(filepath.Join(t.stdlibRoot, "elixir", "lib", "enum.ex"))) {
		return t.stdlibRoot
	}
	t.stdlibRoot = ""
	if found, ok := stdlib.Resolve(cache, "", root); ok {
		t.stdlibRoot = found
	}
	t.stdlibSet, t.stdlibAt, t.stdlibStamp = true, time.Now(), stamp
	return t.stdlibRoot
}

// findMix returns the mix binary of the Elixir install whose standard library
// is stdlibRoot, or else the one that searchMix finds. since is when the
// session started to initialize.
func (c *IndexCoordinator) findMix(stdlibRoot string, since time.Time) string {
	if mix := mixBesideStdlib(stdlibRoot); mix != "" {
		return mix
	}
	return c.searchMix(since)
}

// searchMix looks for mix on the PATH, in the standard version-manager
// locations, and from a login shell, because an editor-launched process often
// never ran the mise or asdf hook that puts mix on its PATH.
//
// A mix that was found is kept like a detected standard library. A search
// that found nothing is reused only by a session that started to initialize
// before that search ended, such as editors that open together and wait for
// the one search: the Warning tells the user to install mix and restart the
// editor, and that restart must search again.
func (c *IndexCoordinator) searchMix(since time.Time) string {
	t := &c.toolchain
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.mixSet {
		if t.mixBin != "" && time.Since(t.mixAt) < toolchainTTL && fileExists(t.mixBin) {
			return t.mixBin
		}
		if t.mixBin == "" && !t.mixAt.Before(since) {
			return ""
		}
	}
	t.mixBin = ""
	if p, ok := stdlib.FindExecutable("mix"); ok {
		log.Printf("Mix binary at: %s (PATH fallback)", p)
		t.mixBin = p
	} else if p, ok := stdlib.FindViaLoginShell("mix"); ok {
		log.Printf("Mix binary at: %s (login shell)", p)
		t.mixBin = p
	}
	t.mixSet, t.mixAt = true, time.Now()
	return t.mixBin
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// mixBesideStdlib derives the mix binary from the same Elixir install as the
// standard library.
func mixBesideStdlib(stdlibRoot string) string {
	if stdlibRoot == "" {
		return ""
	}
	mixBin := filepath.Join(stdlibRoot, "..", "bin", "mix")
	if resolved, err := filepath.Abs(mixBin); err == nil {
		mixBin = resolved
	}
	if _, err := os.Stat(mixBin); err != nil {
		return ""
	}
	return mixBin
}

// claimExplicitStdlib decides which explicit stdlibPath the workspace uses.
// The first one that an editor sends is used for the life of the workspace,
// so that two editors with different values do not switch the shared index
// back and forth, with a full pass for each switch. Two spellings of one
// directory (a trailing slash, a symlink) are the same path. It returns the
// path in use and whether path is it.
func (c *IndexCoordinator) claimExplicitStdlib(path string) (string, bool) {
	c.stdlibMu.Lock()
	defer c.stdlibMu.Unlock()
	if c.explicitStdlib == "" {
		c.explicitStdlib = path
		return path, true
	}
	return c.explicitStdlib, samePath(c.explicitStdlib, path)
}

// samePath reports whether a and b name one directory: equal when cleaned, or
// when their symlinks are resolved.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// stdlibConflictMessage tells one editor that its stdlibPath is not used.
func stdlibConflictMessage(asked, inUse string) string {
	return fmt.Sprintf(
		"Dexter: this editor sets stdlibPath to %s, but another editor of this workspace set it to %s first, and the workspace has one index. Dexter uses %s. To use another standard library, set the same stdlibPath in every editor of this project, then run `dexter stop` in the project directory and open the editors again.",
		asked, inUse, inUse)
}
