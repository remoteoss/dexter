package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A rename writes each new file text into a temp file next to its target and
// then renames the temp file over the target (see lsp.diskTxn). The temp file
// is named ".<target name>.dexter-<pid>-<n>.tmp". The name does not end in an
// Elixir extension, so the index and the watchers ignore it.
//
// A process that stops between the two steps (a crash, a kill) leaves its
// temp files. The walks remove them as they pass: the reconciliation that
// every daemon start runs, and the rename itself for the directories it
// writes. Only a name that matches the pattern exactly is touched, and only
// when the process that made it is not running, or when the file is older
// than StaleRenameTempAge.

const renameTempMarker = ".dexter-"

// StaleRenameTempAge is the age after which a rename temp file is stale even
// when a process with its pid runs: a pid can be used again, and no rename
// takes this long.
const StaleRenameTempAge = time.Hour

// RenameTempName returns the temp file name for target's base name, made by
// process pid as its n-th temp file.
func RenameTempName(base string, pid int, n uint64) string {
	return fmt.Sprintf(".%s%s%d-%d.tmp", base, renameTempMarker, pid, n)
}

// renameTempPID returns the pid in a rename temp file name. ok is false for
// any other name.
func renameTempPID(name string) (pid int, ok bool) {
	if len(name) < 2 || name[0] != '.' || !strings.HasSuffix(name, ".tmp") {
		return 0, false
	}
	i := strings.LastIndex(name, renameTempMarker)
	if i <= 1 {
		return 0, false
	}
	rest := name[i+len(renameTempMarker) : len(name)-len(".tmp")]
	pidText, nText, found := strings.Cut(rest, "-")
	if !found {
		return 0, false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return 0, false
	}
	if _, err := strconv.ParseUint(nText, 10, 64); err != nil {
		return 0, false
	}
	return pid, true
}

// RemoveStaleRenameTemp removes path when its name is a rename temp file name
// and the file is stale: its process is not running, or it is older than
// StaleRenameTempAge. A temp file of this process is in use and is kept. It
// reports whether the name matched, so a walk can skip the entry. The first
// test is one byte compare, so a walk pays nothing for ordinary names.
func RemoveStaleRenameTemp(path string) bool {
	name := filepath.Base(path)
	if len(name) == 0 || name[0] != '.' {
		return false
	}
	pid, ok := renameTempPID(name)
	if !ok {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return true
	}
	old := time.Since(info.ModTime()) > StaleRenameTempAge
	if pid == os.Getpid() && !old {
		return true
	}
	if old || !processRunning(pid) {
		_ = os.Remove(path)
	}
	return true
}

// RemoveStaleRenameTemps removes the stale rename temp files in dir.
func RemoveStaleRenameTemps(dir string) {
	entries, err := ReadDirUnsorted(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if name := e.Name(); len(name) > 0 && name[0] == '.' && !e.IsDir() {
			RemoveStaleRenameTemp(filepath.Join(dir, name))
		}
	}
}
