//go:build !unix

package parser

// processRunning cannot ask the system here without more dependencies, so it
// answers that the process runs; StaleRenameTempAge then decides.
func processRunning(int) bool { return true }
