//go:build unix

package parser

import (
	"errors"
	"syscall"
)

// processRunning reports whether a process with pid exists. A process that
// another user owns exists too (EPERM).
func processRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
