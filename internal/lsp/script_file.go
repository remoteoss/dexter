package lsp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// scriptDirs are where Dexter can put the scripts it runs, in order: a
// directory of this user under the temp directory, then one under the user's
// cache directory. A path that every user shares (such as /tmp/dexter on
// Linux) would let another user replace a script that this user then runs. On
// a shared machine another user can also create the first directory first;
// the second one is then used.
func scriptDirs() []string {
	dirs := []string{filepath.Join(os.TempDir(), fmt.Sprintf("dexter-scripts-%d", os.Getuid()))}
	if cache, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cache) {
		dirs = append(dirs, filepath.Join(cache, "dexter", "scripts"))
	}
	return dirs
}

// scriptDirCandidates is scriptDirs; tests replace it.
var scriptDirCandidates = scriptDirs

// installScriptAnywhere installs content in the first script directory that
// can be used.
func installScriptAnywhere(dirs []string, name, ext, content string) (string, error) {
	var errs []error
	for _, dir := range dirs {
		path, err := installScript(dir, name, ext, content)
		if err == nil {
			return path, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", dir, err))
	}
	if len(errs) == 0 {
		return "", errors.New("no script directory")
	}
	return "", errors.Join(errs...)
}

// installScript makes sure that dir holds content under a name made from its
// hash, and returns the path. Each content has its own file, so two Dexter
// builds on one machine (a release and a development build, or two releases
// during an upgrade) never run each other's script. The file is written to a
// temporary name in the same directory and renamed into place, so a reader
// never sees a partial or a missing file.
func installScript(dir, name, ext, content string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := checkPrivateDir(dir); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(content))
	path := filepath.Join(dir, name+"-"+hex.EncodeToString(sum[:8])+ext)
	// Reused only when it is a regular file (not a symlink) of this user that
	// nobody else can write, with the right content. Anything else is
	// replaced: the rename below replaces a symlink itself, not its target.
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && privateFile(info) {
		if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
			return path, nil
		}
	}
	tmp, err := os.CreateTemp(dir, name+"-*.tmp")
	if err != nil {
		return "", err
	}
	_, writeErr := tmp.WriteString(content)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(tmp.Name())
		if writeErr != nil {
			return "", writeErr
		}
		return "", closeErr
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return path, nil
}
