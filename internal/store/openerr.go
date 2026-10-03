package store

import (
	"errors"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// OpenFailure is the kind of an error from Open. The kind decides what the
// caller may do with the database files.
type OpenFailure int

const (
	// OpenFailureOther is a failure that does not come from the database
	// file itself: permissions, a full disk, too many open files, a directory
	// that cannot be made. Deleting the index does not fix it, and it can
	// destroy an index that is good.
	OpenFailureOther OpenFailure = iota
	// OpenFailureDamaged is a file that is not a database or is malformed.
	// The index is a derived cache, so deleting it and rebuilding is safe.
	OpenFailureDamaged
	// OpenFailureBusy is a database that another connection holds locked.
	// That process can still be writing it: deleting the files then loses
	// its work in silence.
	OpenFailureBusy
)

// ClassifyOpenError tells which kind of failure an error from Open is.
func ClassifyOpenError(err error) OpenFailure {
	var se sqlite3.Error
	if errors.As(err, &se) {
		switch se.Code {
		case sqlite3.ErrCorrupt, sqlite3.ErrNotADB:
			return OpenFailureDamaged
		case sqlite3.ErrBusy, sqlite3.ErrLocked:
			return OpenFailureBusy
		}
	}
	return OpenFailureOther
}
