//go:build !unix

package parser

import (
	"os"
	"time"
)

// processRunning cannot ask the system here without more dependencies, so it
// answers that the process runs; StaleRenameTempAge then decides.
func processRunning(int) bool { return true }

// changedAt returns the modification time: here no inode change time is read.
func changedAt(_ string, info os.FileInfo) time.Time { return info.ModTime() }
