package daemon

import (
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// staleLogAge is how old the log of a workspace that no daemon serves must be
// before a starting daemon removes it. A recent log is kept: it can hold the
// reason a daemon stopped.
var staleLogAge = 14 * 24 * time.Hour

// maxCleanupEntries bounds the work of one cleanup.
const maxCleanupEntries = 20000

// cleanRuntimeDirSafely is cleanRuntimeDir for a goroutine of a starting
// daemon: housekeeping must never stop the daemon.
func cleanRuntimeDirSafely(endpoint Endpoint) {
	defer func() {
		if value := recover(); value != nil {
			log.Printf("Error: panic while cleaning the runtime directory: %v\n%s", value, debug.Stack())
		}
	}()
	removed := cleanRuntimeDir(filepath.Dir(endpoint.Socket), endpoint.Lock, time.Now())
	if removed > 0 {
		log.Printf("Removed %d runtime files of workspaces that no daemon serves", removed)
	}
}

// runtimeKey returns the workspace key and the extension of a runtime file
// name, or false for a name that Dexter did not make.
func runtimeKey(name string) (key, ext string, ok bool) {
	ext = filepath.Ext(name)
	switch ext {
	case ".sock", ".lock", ".log":
	default:
		return "", "", false
	}
	key = strings.TrimSuffix(name, ext)
	if len(key) != 32 && len(key) != 16 {
		return "", "", false
	}
	for _, c := range key {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", "", false
		}
	}
	return key, ext, true
}

// socketAnswers reports whether a process accepts connections on socket.
func socketAnswers(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, liveSocketProbe)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// cleanRuntimeDir removes the runtime files of workspaces that no daemon
// serves: sockets, lock files, and logs older than staleLogAge. Only names
// that Dexter makes are touched, and only for a workspace whose lock this
// process can take, which proves that no daemon holds it. The lock file is
// removed while it is held; acquirePlatformLock checks after each lock that
// the path still names the locked file, so a process that opened the old file
// cannot take ownership of a file that nobody else can see. own is the lock
// file of the calling daemon, which is skipped. It returns the number of
// files removed.
func cleanRuntimeDir(dir, own string, now time.Time) int {
	if runtime.GOOS == "windows" {
		return 0 // an open file cannot be removed there; nothing accumulates fast
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	if len(entries) > maxCleanupEntries {
		entries = entries[:maxCleanupEntries]
	}
	files := make(map[string]map[string]os.DirEntry)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		key, ext, ok := runtimeKey(entry.Name())
		if !ok {
			continue
		}
		if files[key] == nil {
			files[key] = make(map[string]os.DirEntry)
		}
		files[key][ext] = entry
	}
	removed := 0
	for key, byExt := range files {
		lockPath := filepath.Join(dir, key+".lock")
		if lockPath == own {
			continue
		}
		// A log alone is removed only when it is old; it needs no lock to
		// decide that a daemon is not writing it now, because a live daemon
		// always has a lock file next to its log.
		handle, acquired, err := acquirePlatformLock(lockPath)
		if err != nil || !acquired {
			continue
		}
		// A daemon whose lock file was removed still answers on its socket,
		// and it locks a new file at the path on its next check. It is alive.
		if _, ok := byExt[".sock"]; ok && socketAnswers(filepath.Join(dir, key+".sock")) {
			_ = releasePlatformLock(handle)
			continue
		}
		if _, ok := byExt[".sock"]; ok {
			if os.Remove(filepath.Join(dir, key+".sock")) == nil {
				removed++
			}
		}
		if entry, ok := byExt[".log"]; ok {
			if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > staleLogAge {
				if os.Remove(filepath.Join(dir, key+".log")) == nil {
					removed++
				}
			}
		}
		_, hadLock := byExt[".lock"]
		if os.Remove(lockPath) == nil && hadLock {
			removed++
		}
		_ = releasePlatformLock(handle)
	}
	return removed
}
