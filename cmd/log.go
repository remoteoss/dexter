package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/remoteoss/dexter/internal/store"
)

// cmdLog prints the path of the daemon log of the project of start, so that
// `tail -F "$(dexter log)"` follows it. For a folder of repositories, it
// prints the log of each of its projects that has one, and names the project
// of each on stderr (the file names are hashes).
func cmdLog(start string, out, notes io.Writer) error {
	root := store.ResolveRoot(start)
	switch root.Kind {
	case store.OneProject:
		_, _ = fmt.Fprintln(out, daemonLogPath(root.Dir))
		return nil
	case store.Container:
		projects, _ := store.ContainerProjects(root.Dir)
		found := 0
		for _, p := range projects {
			path := daemonLogPath(p)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			rel, err := filepath.Rel(root.Dir, p)
			if err != nil {
				rel = p
			}
			_, _ = fmt.Fprintf(notes, "%s: %s\n", rel, path)
			_, _ = fmt.Fprintln(out, path)
			found++
		}
		if found == 0 {
			return fmt.Errorf("%s is a folder of repositories, and no project in it has a daemon log yet; open a file of a project first", root.Dir)
		}
		return nil
	default:
		return store.NonProjectRootError(root.Dir)
	}
}
