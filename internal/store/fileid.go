package store

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// SameDatabaseFile reports whether the file at the store's path is still the
// database file that Open opened. It is false when that path is now missing
// (ENOENT, ENOTDIR) or names another file: the root was moved and replaced by
// a copy, or the index was deleted or replaced under the process. Another
// error (EACCES, EIO, ESTALE) is returned, because it can clear again.
//
// The rule compares the inode only, not the device: an autofs, NFS, or FUSE
// remount can give the same file a new device number. Where the system gives
// no inode (Windows), the answer is true when the file is there.
func (s *Store) SameDatabaseFile() (bool, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return false, nil
		}
		return false, err
	}
	if s.inode == 0 {
		return true, nil
	}
	return fileInode(info) == s.inode, nil
}

// recordInode stores the inode of the database file just opened.
func (s *Store) recordInode() {
	if info, err := os.Stat(s.path); err == nil {
		s.inode = fileInode(info)
	}
}
