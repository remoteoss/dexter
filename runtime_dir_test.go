package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/remoteoss/dexter/internal/daemon"
)

// TestMain gives every daemon these tests start a private runtime directory,
// so that they never write sockets, locks, or logs into the user's real one.
// Spawned binaries inherit it through the environment. It is under /tmp
// because sockaddr_un paths are capped near 104 bytes.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "dxt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "private runtime directory: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv(daemon.RuntimeDirEnv, dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
