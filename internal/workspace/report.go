package workspace

import (
	"fmt"

	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/store"
)

// Condition keys of the workspace runtime. The index keys are in package lsp,
// which also clears them.
const (
	condRoot          = "root"
	condWatch         = "watcher"
	condWatchCoverage = "watcher.coverage"
	condWatchFallback = "watcher.fallback"
)

// reportRoot warns when the workspace root does not look like a project. An
// editor decides what it opens, so Dexter serves the directory anyway; the
// warning is shown in every editor that attaches, because each of them pays for
// the index.
func reportRoot(root string, reporter *notify.Reporter) {
	if store.IsHomeDir(root) {
		if store.HasIndex(root) {
			return
		}
		reporter.Set(condRoot, notify.Warning, fmt.Sprintf(
			"Dexter: %s is your home directory, not a project, but the editor opened it as the workspace. Dexter indexes it anyway, which can take a long time. Open the project directory instead, or set --root <path> in the dexter command of the editor.", root))
		return
	}
	if store.LooksLikeProject(root) {
		return
	}
	reporter.Set(condRoot, notify.Warning, fmt.Sprintf(
		"Dexter: %s does not look like an Elixir project (no mix.exs, .git, or Dexter database). Dexter indexes it anyway. If it is the wrong directory, open the project directory instead, or set --root <path> in the dexter command of the editor.", root))
}

// damagedIndexMessage explains an index that was damaged and was deleted for
// a rebuild.
func damagedIndexMessage(root string, err error) string {
	return fmt.Sprintf(
		"Dexter: the index at %s is damaged (%v). Dexter deleted it and is rebuilding it now; navigation is limited until the rebuild ends.",
		store.DBPath(root), err)
}

// lockedIndexMessage explains an index that another process kept locked.
func lockedIndexMessage(root string, err error) string {
	return fmt.Sprintf(
		"Dexter: another process holds the index at %s locked (%v), so Dexter cannot open it. Dexter did not change the index. If `dexter init` runs for this project, wait until it ends; otherwise stop the other Dexter process (`dexter stop --force` in the project directory, or close the editor that runs a different Dexter build). Then restart this editor.",
		store.DBPath(root), err)
}

// otherOpenFailureMessage explains an index that could not be opened for a
// cause that a rebuild does not fix, such as permissions or a full disk.
func otherOpenFailureMessage(root string, err error) string {
	return fmt.Sprintf(
		"Dexter: the index at %s could not be opened (%v). Dexter did not delete it, because the cause is not a damaged index. Fix the cause (for example the file permissions of %s, free disk space, or the limit of open files), then restart this editor. To start again with an empty index, delete %s.",
		store.DBPath(root), err, store.DBDir(root), store.DBDir(root))
}

// versionMismatchMessage explains an index that was written by another index
// version and must be rebuilt.
func versionMismatchMessage(stored, current int) string {
	switch {
	case stored == 0:
		return "Dexter: the index has no version, so the build that wrote it did not finish. Rebuilding it now; navigation is limited until the rebuild ends."
	case stored > current:
		return fmt.Sprintf(
			"Dexter: the index was written by a newer Dexter build (index version %d; this build uses %d). Rebuilding it now; navigation is limited until the rebuild ends. To avoid this, run the same Dexter build in every editor and terminal for this project.",
			stored, current)
	default:
		return fmt.Sprintf(
			"Dexter: the index was written by an older Dexter build (index version %d; this build uses %d). Rebuilding it now; navigation is limited until the rebuild ends. This occurs one time after an upgrade.",
			stored, current)
	}
}

// reportWatchUnavailable tells the user that native file watching does not
// work. The runtime retries; a retry that fails again sends nothing new.
func (r *Runtime) reportWatchUnavailable(err error) {
	r.Reporter().Set(condWatch, notify.Warning, fmt.Sprintf(
		"Dexter: cannot watch the files of %s (%v). Changes made outside the editor (git checkout, code generators, other editors) are not indexed until Dexter can watch again; Dexter tries again every %s. Files that you save in the editor are still indexed.",
		r.root, err, watchRetryInterval))
}

// reportWatcherStarted ends an unavailable-watcher condition and reports what
// the new watcher cannot do.
func (r *Runtime) reportWatcherStarted(w *Watcher) {
	reporter := r.Reporter()
	reporter.Clear(condWatch, fmt.Sprintf(
		"Dexter: file watching works again for %s; Dexter is checking the files that changed in the meantime.", r.root))
	if err := w.Fallback(); err != nil {
		reporter.Set(condWatchFallback, notify.Warning, fmt.Sprintf(
			"Dexter: the native file watcher is not available for %s (%v), so Dexter watches each directory on its own. This uses one file descriptor for each directory; in a very large project, some directories can stay unwatched, and Dexter tells you if that occurs.",
			r.root, err))
	}
	r.reportCoverage()
}

// reportCoverage tells the user when directories cannot be watched, and when
// the watcher covers the whole project again. It reads the state of the
// watcher itself, under coverageMu, and does not trust a value that a caller
// read before: the watcher can restore its last directory between that read
// and the report, and its own report could then come first and leave a warning
// that never ends. Each call reports the state at that time, so the last call
// is always right.
func (r *Runtime) reportCoverage() {
	r.coverageMu.Lock()
	defer r.coverageMu.Unlock()
	r.watcherMu.RLock()
	w := r.watcher
	r.watcherMu.RUnlock()
	if w == nil {
		return // reportWatcherStarted reports when the watcher is in place
	}
	reporter := r.Reporter()
	if !w.Degraded() {
		reporter.Clear(condWatchCoverage, "Dexter: file watching covers the whole project again; Dexter is checking the files that changed in the meantime.")
		return
	}
	failed := w.FailedDirectories()
	what := "Some directories"
	if len(failed) == 1 {
		what = fmt.Sprintf("1 directory (%s)", failed[0])
	} else if len(failed) > 1 {
		what = fmt.Sprintf("%d directories (%s)", len(failed), notify.Summarize(failed))
	}
	reporter.Set(condWatchCoverage, notify.Warning, fmt.Sprintf(
		"Dexter: %s under %s cannot be watched, often because of the file watch limit of the system; see the log for the error. Changes in them made outside the editor are not indexed until Dexter can watch them; Dexter tries again every %s.",
		what, r.root, watchRetryInterval))
}
