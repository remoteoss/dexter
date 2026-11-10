//go:build darwin || linux

package lsp

import (
	"errors"

	"golang.org/x/sys/unix"
)

// swapError maps the error of an atomic swap. The file systems and kernels
// that cannot swap answer with ENOTSUP or EOPNOTSUPP (HFS+, some network
// file systems), EINVAL (older overlayfs, NFS, tmpfs on old kernels), ENOSYS
// (a kernel older than renameat2), or EXDEV; all of them go to the hard-link
// fallback. Any other error is a real failure of the replace.
func swapError(err error) error {
	if err == nil {
		return nil
	}
	for _, unsupported := range []error{unix.ENOTSUP, unix.EOPNOTSUPP, unix.EINVAL, unix.ENOSYS, unix.EXDEV} {
		if errors.Is(err, unsupported) {
			return errSwapUnsupported
		}
	}
	return err
}
