package daemon

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

// TestMain gives the package's tests a private runtime directory, so that they
// never write sockets, locks, or logs into the user's real one. The directory
// is under /tmp, not the default temp directory: sockaddr_un paths are capped
// near 104 bytes, and a macOS temp directory is long enough to use most of that.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "dxt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "private runtime directory: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv(RuntimeDirEnv, dir)
	// The test binary is os.Executable here: a real spawn would run every
	// test of the package again as a "daemon". A test that needs a daemon
	// to start replaces spawnDaemon with one that runs Run in-process.
	spawnDaemon = func(string) (<-chan error, error) {
		return nil, errors.New("this test does not start a daemon process")
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
