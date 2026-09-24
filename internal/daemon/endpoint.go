// Package daemon implements Dexter's per-workspace local daemon transport and
// process ownership.
package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// ContractVersion is the workspace contract: the handshake, the control
// methods, and the index semantics a frontend relies on. Bump it only when a
// daemon from an older build must not keep serving a newer frontend — a
// breaking wire change, or an index or parser change whose answers would be
// wrong until the index is rebuilt. A bump makes the newer frontend replace the
// running daemon, whose startup then rebuilds a populated index whose
// IndexVersion differs. Routine changes that leave frontends and daemons
// compatible do not bump it, so a running daemon is left alone.
const ContractVersion = 2

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
	// Identity is the symlink-resolved path. It decides which daemon owns the
	// physical workspace; the handshake then rejects a different Root spelling
	// because path-keyed answers cannot safely mix aliases.
	Identity string
	Socket   string
	Lock     string
	Log      string
}

// ResolveEndpoint derives the private runtime paths for root. Runtime files go
// in an environment-independent directory owned by this user. A session-local
// location would let a GUI editor and a shell derive different ownership locks.
func ResolveEndpoint(root string) (Endpoint, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Endpoint{}, err
	}
	identity := abs
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		identity = resolved
	}
	digest := sha256.Sum256([]byte(identity))
	key := hex.EncodeToString(digest[:16])

	var lastErr error
	for _, dir := range platformRuntimeDirs() {
		socket := filepath.Join(dir, key+".sock")
		if len(socket)+1 > maxSocketPath {
			lastErr = fmt.Errorf("socket path %s exceeds %d bytes", socket, maxSocketPath)
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			lastErr = err
			continue
		}
		if err := checkRuntimeDir(dir); err != nil {
			lastErr = err
			continue
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			lastErr = err
			continue
		}
		return Endpoint{
			Root:     abs,
			Identity: identity,
			Socket:   socket,
			Lock:     filepath.Join(dir, key+".lock"),
			Log:      filepath.Join(dir, key+".log"),
		}, nil
	}
	return Endpoint{}, fmt.Errorf("no usable daemon runtime directory: %w", lastErr)
}

// Ownership is a kernel-backed workspace ownership lock. The file may remain
// after Release; only the advisory lock held by the descriptor is authority.
type Ownership struct {
	handle *lockHandle
}

// ErrWorkspaceOwned means another live process holds the workspace lock.
var ErrWorkspaceOwned = fmt.Errorf("workspace daemon already owns the project")

// AcquireOwnership attempts to become the single workspace owner.
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
	return &Ownership{handle: handle}, ep, nil
}

// Release gives up ownership. Crashes release the same kernel lock
// automatically when the descriptor is closed by the OS.
func (o *Ownership) Release() error {
	if o == nil || o.handle == nil {
		return nil
	}
	err := releasePlatformLock(o.handle)
	o.handle = nil
	return err
}
