// Package daemon implements Dexter's per-workspace local daemon transport and
// process ownership.
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/remoteoss/dexter/internal/pathcase"
)

// ContractVersion is the workspace contract: the handshake, the control
// methods, and the index semantics a frontend relies on. Bump it only when a
// daemon from an older build must not keep serving a newer frontend — a
// breaking wire change, or an index or parser change whose answers would be
// wrong until the index is rebuilt. A bump makes the newer frontend replace the
// running daemon, whose startup then rebuilds a populated index whose
// IndexVersion differs. Routine changes that leave frontends and daemons
// compatible do not bump it, so a running daemon is left alone.
const ContractVersion = 5 // 5: IndexVersion 17. A later contract change in the same release keeps 5.

// maxSocketPath keeps a workspace socket inside sockaddr_un on every supported
// platform (about 104 bytes on macOS, 108 on Linux), including the NUL.
const maxSocketPath = 100

// Endpoint contains the deterministic machine-local paths for one workspace,
// plus the two spellings of its root that matter.
type Endpoint struct {
	// Root is the workspace path as the caller spelled it, made absolute. The
	// runtime indexes this spelling so stored paths keep matching the URIs an
	// editor sends. Resolving symlinks here would break every path-keyed lookup
	// for a project reached through one: on macOS a temp dir is /var/... to the
	// editor and /private/var/... after EvalSymlinks.
	Root string
	// Identity is the symlink-resolved path, in the case the file system
	// stores it. It decides which daemon owns the physical workspace; the
	// handshake then rejects a different Root spelling because path-keyed
	// answers cannot safely mix aliases.
	Identity string
	Socket   string
	Lock     string
	Log      string
}

// RuntimeDirEnv names a variable that replaces every runtime directory
// candidate. It exists for tests and sandboxes, which must not write into the
// user's real runtime directory. Every frontend and daemon for one project must
// see the same value; a GUI editor that does not see it would derive another
// lock and start a second daemon over the same index. Dexter does not change
// the directory it names: it creates its own subdirectory `dexter-<uid>` there,
// and only that one is made private and cleaned.
const RuntimeDirEnv = "DEXTER_RUNTIME_DIR"

// runtimeDirs returns the runtime directory candidates in order of preference.
func runtimeDirs() []string {
	if dir := os.Getenv(RuntimeDirEnv); dir != "" {
		return []string{filepath.Join(dir, fmt.Sprintf("dexter-%d", os.Getuid()))}
	}
	return platformDirs()
}

// platformDirs is platformRuntimeDirs; tests replace it.
var platformDirs = platformRuntimeDirs

// endpointKeys are the file names a workspace can use in one candidate
// directory: the full key first, and a shorter key for a directory whose path
// would put the full name past the sockaddr_un limit. Both are derived from the
// identity alone, so every process makes the same choice.
func endpointKeys(digest [sha256.Size]byte) []string {
	return []string{hex.EncodeToString(digest[:16]), hex.EncodeToString(digest[:8])}
}

// candidateEndpoint is the socket, lock, and log of one workspace in dir, or
// false when no key fits under maxSocketPath.
func candidateEndpoint(dir string, keys []string) (socket, lock, logPath string, ok bool) {
	for _, key := range keys {
		socket = filepath.Join(dir, key+".sock")
		if len(socket)+1 > maxSocketPath {
			continue
		}
		return socket, filepath.Join(dir, key+".lock"), filepath.Join(dir, key+".log"), true
	}
	return "", "", "", false
}

// ResolveEndpoint derives the private runtime paths for root. Runtime files go
// in an environment-independent directory owned by this user. A session-local
// location would let a GUI editor and a shell derive different ownership locks.
//
// The first usable candidate wins. A later candidate is used only when an
// earlier one cannot be used by anybody of this user, for example because
// another user created /tmp/dexter-<uid> first; that state is the same for
// every process of the user, so a GUI editor and a shell still agree.
func ResolveEndpoint(root string) (Endpoint, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Endpoint{}, err
	}
	identity := abs
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		identity = pathcase.DiskSpelling(resolved)
	}
	keys := endpointKeys(sha256.Sum256([]byte(identity)))

	var lastErr error
	for _, dir := range runtimeDirs() {
		socket, lock, logPath, ok := candidateEndpoint(dir, keys)
		if !ok {
			lastErr = fmt.Errorf("socket path in %s exceeds %d bytes", dir, maxSocketPath)
			continue
		}
		if err := prepareRuntimeDir(dir); err != nil {
			lastErr = err
			continue
		}
		return Endpoint{
			Root:     abs,
			Identity: identity,
			Socket:   socket,
			Lock:     lock,
			Log:      logPath,
		}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no runtime directory candidates")
	}
	return Endpoint{}, fmt.Errorf("no usable daemon runtime directory: %w", lastErr)
}

// prepareRuntimeDir creates dir when it is missing and checks that it is a
// private directory of this user.
func prepareRuntimeDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := checkRuntimeDir(dir); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// otherCandidateLocks returns the lock files that the same workspace would use
// in the runtime directories it does not use now. They matter only while the
// choice of directory changes, for example when a directory that blocked the
// first candidate is removed.
func otherCandidateLocks(endpoint Endpoint) []string {
	keys := endpointKeys(sha256.Sum256([]byte(endpoint.Identity)))
	var out []string
	for _, dir := range runtimeDirs() {
		_, lock, _, ok := candidateEndpoint(dir, keys)
		if !ok || lock == endpoint.Lock {
			continue
		}
		if err := checkRuntimeDir(dir); err != nil {
			continue
		}
		if _, err := os.Lstat(lock); err == nil {
			out = append(out, lock)
		}
	}
	return out
}

// Ownership is a kernel-backed workspace ownership lock. The file may remain
// after Release; only the advisory lock held by the descriptor is authority.
type Ownership struct {
	// mu orders Release with relock, which the daemon's endpoint check calls
	// while the daemon may be shutting down.
	mu     sync.Mutex
	handle *lockHandle
	path   string
}

// ErrWorkspaceOwned means another live process holds the workspace lock.
var ErrWorkspaceOwned = fmt.Errorf("workspace daemon already owns the project")

// liveSocketProbe bounds the connect that AcquireOwnership makes to a socket it
// finds. A daemon accepts from its backlog at once, and a stale socket refuses
// at once, so this only matters for a wedged process.
const liveSocketProbe = 250 * time.Millisecond

// AcquireOwnership attempts to become the single workspace owner.
//
// The lock alone is not proof. A temp cleaner or a user can remove the lock
// file while a daemon holds the lock on it, and the next process then locks a
// new file at the same path. So a process that gets the lock also refuses
// ownership while a daemon answers on the workspace socket, or while the same
// workspace holds a lock in another runtime directory candidate.
func AcquireOwnership(root string) (*Ownership, Endpoint, error) {
	ep, err := ResolveEndpoint(root)
	if err != nil {
		return nil, Endpoint{}, err
	}
	handle, acquired, err := acquirePlatformLock(ep.Lock)
	if err != nil {
		return nil, Endpoint{}, err
	}
	if !acquired {
		return nil, ep, ErrWorkspaceOwned
	}
	o := &Ownership{handle: handle, path: ep.Lock}
	if otherOwnerExists(ep) {
		_ = o.Release()
		return nil, ep, ErrWorkspaceOwned
	}
	return o, ep, nil
}

// otherOwnerExists reports whether a process that does not hold this lock file
// still serves the workspace: a daemon whose lock file was removed answers on
// the socket, and a daemon in another runtime directory holds its lock there.
func otherOwnerExists(ep Endpoint) bool {
	if _, err := os.Lstat(ep.Socket); err == nil {
		if conn, dialErr := net.DialTimeout("unix", ep.Socket, liveSocketProbe); dialErr == nil {
			_ = conn.Close()
			return true
		}
	}
	for _, lock := range otherCandidateLocks(ep) {
		handle, acquired, err := acquirePlatformLock(lock)
		if err != nil {
			continue
		}
		if !acquired {
			return true
		}
		_ = releasePlatformLock(handle)
	}
	return false
}

// Release gives up ownership. Crashes release the same kernel lock
// automatically when the descriptor is closed by the OS.
func (o *Ownership) Release() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.handle == nil {
		return nil
	}
	err := releasePlatformLock(o.handle)
	o.handle = nil
	return err
}

// lockState is what a check of the lock path found.
type lockState int

const (
	lockHeld     lockState = iota // the path names the file this process locked
	lockMissing                   // nothing is at the path
	lockReplaced                  // another file is at the path
)

// state compares the lock path with the file this process holds locked.
func (o *Ownership) state() (lockState, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.handle == nil {
		return lockHeld, errors.New("ownership released")
	}
	held, err := o.handle.file.Stat()
	if err != nil {
		return lockHeld, err
	}
	current, err := os.Stat(o.path)
	if os.IsNotExist(err) {
		return lockMissing, nil
	}
	if err != nil {
		return lockHeld, err
	}
	if !os.SameFile(held, current) {
		return lockReplaced, nil
	}
	return lockHeld, nil
}

// relock locks the file that is now at the lock path, or a new one when the
// path is empty, and gives up the old lock. It reports false when another
// process holds the file at the path: that process owns the workspace now.
func (o *Ownership) relock() (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.handle == nil {
		return false, errors.New("ownership released")
	}
	handle, acquired, err := acquirePlatformLock(o.path)
	if err != nil || !acquired {
		return false, err
	}
	old := o.handle
	o.handle = handle
	return true, releasePlatformLock(old)
}
