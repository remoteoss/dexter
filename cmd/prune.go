package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/remoteoss/dexter/internal/daemon"
	"github.com/remoteoss/dexter/internal/store"
)

// cmdPrune lists the old Dexter indexes that no session uses: those in the
// directories above the project of start, and the own index of a folder that
// only holds other projects. With del, it deletes each one that no daemon
// owns (store.RemoveIndex). It returns an error when an index could not be
// deleted.
func cmdPrune(start string, del bool, out io.Writer) error {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format, args...) }
	root := store.ResolveRoot(start)
	var stray []store.StrayIndex
	if root.Kind == store.Container {
		if s, ok := store.StrayIndexAt(root.Dir); ok {
			stray = append(stray, s)
		}
	}
	stray = append(stray, store.StrayIndexesAbove(root.Dir)...)
	if len(stray) == 0 {
		say("No old Dexter indexes for %s.\n", root.Dir)
		return nil
	}
	var total int64
	say("Old Dexter indexes that no session uses:\n")
	for _, s := range stray {
		say("  %s (%s)\n", store.DBDir(s.Dir), store.FormatBytes(s.Bytes))
		total += s.Bytes
	}
	if !del {
		say("Run `dexter prune --delete` to delete them (%s).\n", store.FormatBytes(total))
		return nil
	}

	failed := 0
	var freed int64
	for _, s := range stray {
		// The workspace lock: a daemon that serves that directory (started
		// with --root, or by an editor there) keeps its index, and no daemon
		// can open the index while it goes.
		ownership, _, err := daemon.AcquireOwnership(s.Dir)
		if errors.Is(err, daemon.ErrWorkspaceOwned) {
			say("Skipped %s: a Dexter daemon uses it. Close the editor that has %s open, or run `dexter stop --root %s`, then run this again.\n", store.DBDir(s.Dir), s.Dir, s.Dir)
			failed++
			continue
		}
		if err != nil {
			say("Skipped %s: %v\n", store.DBDir(s.Dir), err)
			failed++
			continue
		}
		err = store.RemoveIndex(s.Dir)
		_ = ownership.Release()
		var other *store.OtherFilesError
		if errors.As(err, &other) {
			freed += s.Bytes
			say("Deleted the index in %s (%s). The directory stays: it also holds %s.\n", store.DBDir(s.Dir), store.FormatBytes(s.Bytes), strings.Join(other.Names, ", "))
			continue
		}
		if err != nil {
			say("%s: %v\n", store.DBDir(s.Dir), err)
			failed++
			continue
		}
		freed += s.Bytes
		say("Deleted %s (%s)\n", store.DBDir(s.Dir), store.FormatBytes(s.Bytes))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d old indexes were not deleted", failed, len(stray))
	}
	say("Freed %s.\n", store.FormatBytes(freed))
	return nil
}
