package lsp

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/parser"
)

// Write seams. Tests replace them to make a write or a replace fail.
var (
	// renameWriteTemp writes the new content of a file into its temp file.
	renameWriteTemp = func(f *os.File, data []byte) error {
		if _, err := f.Write(data); err != nil {
			return err
		}
		return syncFile(f)
	}
	// renameReplace puts a temp file in place of its target.
	renameReplace = os.Rename
)

// diskChange is one file that a rename writes on disk.
type diskChange struct {
	from    string    // the file as the index knows it
	to      string    // where the new content goes: from, or the target of a move
	target  string    // the file that is replaced: to, or, for an in-place write through a symlink, the file it points to
	orig    string    // the content the rename read, before its change
	stamp   diskStamp // the disk state of that read
	content []byte
	tmp     string   // the staged temp file
	backup  string   // a hard link to the old target during the replace
	created []string // directories made for a move, deepest last
	root    string   // for a move: the directory above the old namespace directories; see pruneOldDirs
}

// diskStamp is the size, modification time, and inode of a file when a
// rename read it. Stage compares the content with the planning read and takes
// a new stamp; right before the replace, a different stamp stops the rename.
type diskStamp struct {
	size  int64
	mtime int64
	ino   uint64
}

func stampOf(info os.FileInfo) diskStamp {
	return diskStamp{size: info.Size(), mtime: info.ModTime().UnixNano(), ino: inodeOf(info)}
}

// diskTxn writes the closed files of one rename in two phases, so that a
// failure changes as little as possible and every failure is reported
// exactly.
//
// Stage writes each new content into a temp file in the directory of its
// target, flushes it, and gives it the target's mode and owner. Nothing that
// a user or a build sees has changed yet: a failure here (a full disk, for
// example) removes the temp files and the directories made for moves, and
// the rename stops with no file changed. Commit then renames each temp file
// over its target, which replaces the file at once: a reader sees the old or
// the new content, never a part. For a move, the old file is removed after
// the new one is in place; if the removal fails, the new file is removed
// again, so that one module is never in two files.
//
// A symlinked file is written through: the file it points to is replaced and
// the link stays. A file in a directory where no temp file can be made stops
// the rename at stage: Dexter never writes a file in place, because a failure
// in the middle of such a write leaves the file partly written. Extended
// attributes are not copied.
type diskTxn struct {
	changes []*diskChange
	rel     func(string) string // how messages show a path
}

func (t *diskTxn) write(path, orig string, stamp diskStamp, content string) {
	t.changes = append(t.changes, &diskChange{from: path, to: path, orig: orig, stamp: stamp, content: []byte(content)})
}

func (t *diskTxn) move(from, to, root, orig string, stamp diskStamp, content string) {
	t.changes = append(t.changes, &diskChange{from: from, to: to, root: root, orig: orig, stamp: stamp, content: []byte(content)})
}

func (t *diskTxn) show(path string) string {
	if t.rel == nil {
		return path
	}
	return t.rel(path)
}

// stage prepares every change. On an error, it removes what it made and
// returns the error that names the file: nothing has changed.
func (t *diskTxn) stage() error {
	t.removeStaleTemps()
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
		sem   = make(chan struct{}, 16)
	)
	for _, c := range t.changes {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if value := recover(); value != nil {
					// Staging stops with an error: nothing has changed.
					(*notify.Reporter)(nil).Panicked("request", "a rename file stage", value, debug.Stack())
					mu.Lock()
					if first == nil {
						first = fmt.Errorf("the rename was not applied, because an internal error occurred while it prepared %s (%v). No file was changed", t.rel(c.from), value)
					}
					mu.Unlock()
				}
			}()
			if err := c.stage(); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if first != nil {
		t.discard()
		return first
	}
	return nil
}

func (c *diskChange) stage() error {
	c.target = c.to
	var info os.FileInfo
	if c.from == c.to {
		fi, err := os.Lstat(c.to)
		if err != nil {
			return &stagePathError{path: c.from, err: err}
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(c.to)
			if err != nil {
				return &stagePathError{path: c.from, err: err}
			}
			if fi, err = os.Stat(real); err != nil {
				return &stagePathError{path: c.from, err: err}
			}
			c.target = real
		}
		info = fi
	} else {
		fi, err := os.Stat(c.from)
		if err != nil {
			return &stagePathError{path: c.from, err: err}
		}
		info = fi
		if _, err := os.Lstat(c.to); err == nil {
			return &appearedError{path: c.to}
		}
		created, err := mkdirAllTracked(filepath.Dir(c.to))
		c.created = created
		if err != nil {
			return &stagePathError{path: filepath.Dir(c.to), err: err}
		}
	}
	// The file must still hold what the plan read. The stamp taken before
	// this read is what recheckDisk compares right before the replace.
	before, err := os.Stat(c.from)
	if err != nil {
		return &changedError{path: c.from}
	}
	data, err := os.ReadFile(c.from)
	if err != nil || string(data) != c.orig {
		return &changedError{path: c.from}
	}
	c.stamp = stampOf(before)
	mode := info.Mode().Perm()
	f, err := createTemp(c.target, mode)
	if err != nil {
		return &tempError{dir: filepath.Dir(c.target), err: err}
	}
	c.tmp = f.Name()
	err = renameWriteTemp(f, c.content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && mode&0o022 != 0 {
		// The umask can clear group and other write bits at create time.
		err = os.Chmod(c.tmp, mode)
	}
	if err != nil {
		return &stagePathError{path: c.to, err: err}
	}
	keepOwner(c.tmp, info)
	return nil
}

// tempSeq makes temp file names unique within this process; the pid makes
// them unique between processes.
var tempSeq atomic.Uint64

// createTemp makes the temp file for target in target's directory, with
// mode. See parser.RenameTempName for the name.
func createTemp(target string, mode fs.FileMode) (*os.File, error) {
	dir, base := filepath.Split(target)
	for {
		name := filepath.Join(dir, parser.RenameTempName(base, os.Getpid(), tempSeq.Add(1)))
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
}

// cleanedTempDirs holds the directories whose stale rename temp files this
// process removed. A directory needs it once: a temp file of this process is
// either renamed into place or removed, so only an earlier process can leave
// one behind.
var cleanedTempDirs sync.Map

// removeStaleTemps removes the temp files that a stopped process left in the
// directories this rename writes, the first time this process writes there.
func (t *diskTxn) removeStaleTemps() {
	for _, c := range t.changes {
		for _, p := range []string{c.to, c.from} {
			dir := filepath.Dir(p)
			if _, done := cleanedTempDirs.LoadOrStore(dir, true); !done {
				parser.RemoveStaleRenameTemps(dir)
			}
		}
	}
}

// discard removes every temp file and every directory made for a move.
func (t *diskTxn) discard() {
	for _, c := range t.changes {
		c.discard()
	}
}

func (c *diskChange) discard() {
	if c.tmp != "" {
		_ = os.Remove(c.tmp)
		c.tmp = ""
	}
	if c.backup != "" {
		// The target was not replaced, so the backup is a second name of it.
		_ = os.Remove(c.backup)
		c.backup = ""
	}
	for i := len(c.created) - 1; i >= 0; i-- {
		_ = os.Remove(c.created[i]) // only an empty directory goes
	}
	c.created = nil
}

// commitResult is what commit did.
type commitResult struct {
	changed []string // files (by from) that have the new content at to
	// rolledBack is the replace that failed, when commit took the other
	// replaces back. Empty when commit went through.
	rolledBack string
	cause      error
	failures   renameFailures   // what could not be taken back, or removed
	failed     map[string]error // the same files by their full path
}

// errSwapUnsupported is an atomic swap that the file system does not offer.
var errSwapUnsupported = errors.New("atomic swap not supported")

// Replace seams. Tests replace them to make a step fail.
var (
	// renameSwap swaps a staged temp file with its target in one step, so
	// that the temp file's name holds the old file: the backup costs no
	// extra system call. errSwapUnsupported makes the replace fall back to
	// renameBackup and renameReplace.
	renameSwap = exchangeFiles
	// renameBackup makes the backup of a file before its replace when no
	// swap is possible: a hard link, which copies nothing.
	renameBackup = os.Link
)

// commit puts every staged change in place, all or nothing.
//
// Phase 1 replaces the targets. A file that is changed in place is swapped
// with its temp file in one step (renamex_np RENAME_SWAP on macOS, renameat2
// RENAME_EXCHANGE on Linux), so the temp file's name keeps the old file as
// the backup; where no swap is possible, a hard link to the old file is the
// backup, and then the temp file is renamed over it. A moved file's new path
// is made. If one
// replace fails, every replace done so far is taken back (a backup renamed
// over its target, a new path removed), and the rename has changed nothing.
// Phase 2, after every replace succeeded, removes the old paths of moved
// files; removeBackups removes the backups after the lock is released. A removal that fails there is reported: the move is
// then taken back, or, if that fails too, both files are named. A file on a
// file system without hard links gets no backup; if a later replace fails,
// it stays changed, and the message names it.
//
// The caller holds renameMu, which stops every editor's document updates, so
// both phases run in parallel and stay short.
func (t *diskTxn) commit() *commitResult {
	res := &commitResult{}
	errs := parallel(len(t.changes), func(i int) error { return t.changes[i].replace() })

	first := -1
	for i, err := range errs {
		if err != nil {
			first = i
			break
		}
	}
	if first >= 0 {
		c := t.changes[first]
		log.Printf("Rename: cannot replace %s: %v; taking the rename back", c.from, errs[first])
		res.rolledBack, res.cause = c.from, errs[first]
		undo := parallel(len(t.changes), func(i int) error {
			var p *panicError
			// A replace that panicked after it put the file in place (its
			// temporary file is gone) is taken back like a done one.
			if errs[i] == nil || (errors.As(errs[i], &p) && t.changes[i].tmp == "") {
				return t.changes[i].undo()
			}
			t.changes[i].discard()
			return nil
		})
		for i, err := range undo {
			if err != nil {
				u := t.changes[i]
				log.Printf("Rename: cannot take back %s: %v", u.from, err)
				res.failures.add(t.show(u.from), err)
				res.fail(u.from, err)
				res.changed = append(res.changed, u.from)
			}
		}
		if len(res.changed) > 0 {
			res.failures.note(fmt.Sprintf("the rename failed at %s (%s), and these files could not be taken back", t.show(c.from), errText(errs[first])))
		}
		return res
	}

	finish := parallel(len(t.changes), func(i int) error { return t.changes[i].finish() })
	for i, c := range t.changes {
		if err := finish[i]; err != nil {
			log.Printf("Rename: cannot finish %s: %v", c.from, err)
			res.failures.add(t.show(c.from), err)
			res.fail(c.from, err)
			var both *bothFilesError
			if errors.As(err, &both) {
				res.failures.note(fmt.Sprintf("%s and %s both exist now; remove one of them", t.show(c.from), t.show(c.to)))
				res.changed = append(res.changed, c.from)
			}
			continue
		}
		res.changed = append(res.changed, c.from)
	}
	res.failures.changed = len(res.changed)
	t.pruneOldDirs(finish)
	return res
}

// pruneOldDirs removes the directories that a move left empty, from the
// directory of the old file up to its namespace root (see
// conventionalNewPath), which stays. os.Remove removes only an empty
// directory, so a directory that holds anything else, for example a file or
// directory that this rename made, or one that another process made in the
// meantime, stays. A move that failed removes nothing.
func (t *diskTxn) pruneOldDirs(finish []error) {
	var dirs []string
	for i, c := range t.changes {
		if c.from == c.to || finish[i] != nil || c.root == "" {
			continue
		}
		for d := filepath.Dir(c.from); d != c.root && strings.HasPrefix(d, c.root+string(filepath.Separator)); d = filepath.Dir(d) {
			dirs = append(dirs, d)
		}
	}
	// The deepest first, so that a parent is tried after its children.
	sort.Slice(dirs, func(i, j int) bool {
		if len(dirs[i]) != len(dirs[j]) {
			return len(dirs[i]) > len(dirs[j])
		}
		return dirs[i] < dirs[j]
	})
	for _, d := range slices.Compact(dirs) {
		_ = os.Remove(d)
	}
}

func (r *commitResult) fail(path string, err error) {
	if r.failed == nil {
		r.failed = make(map[string]error)
	}
	r.failed[path] = err
}

// report gives the result to the summary of a rename without an editor (see
// RenameSummary): the files changed, the moves made, and the files that could
// not be changed.
func (r *commitResult) report(summary *RenameSummary, txn *diskTxn) {
	if summary == nil {
		return
	}
	done := make(map[string]bool, len(r.changed))
	for _, from := range r.changed {
		done[from] = true
	}
	for _, c := range txn.changes {
		if !done[c.from] || r.failed[c.from] != nil {
			continue
		}
		if c.from != c.to {
			summary.moved(c.from, c.to)
		} else {
			summary.changed(c.from)
		}
	}
	for _, path := range sortedKeys(r.failed) {
		summary.failed(path, r.failed[path])
	}
}

// parallel runs f for 0..n-1 on up to 16 goroutines.
func parallel(n int, f func(i int) error) []error {
	var wg sync.WaitGroup
	errs := make([]error, n)
	sem := make(chan struct{}, 16)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if value := recover(); value != nil {
					// The caller sees the item fail, so a panic never reads
					// as a file that was read, staged, or replaced.
					(*notify.Reporter)(nil).Panicked("request", "a rename file operation", value, debug.Stack())
					errs[i] = &panicError{value}
				}
			}()
			errs[i] = f(i)
		}()
	}
	wg.Wait()
	return errs
}

// panicError is an item of parallel that panicked.
type panicError struct{ value any }

func (e *panicError) Error() string { return fmt.Sprintf("an internal error occurred (%v)", e.value) }

type bothFilesError struct{ err error }

func (e *bothFilesError) Error() string { return e.err.Error() }
func (e *bothFilesError) Unwrap() error { return e.err }

// replace is phase 1 for one file.
func (c *diskChange) replace() error {
	if c.from == c.to {
		err := renameSwap(c.tmp, c.target)
		if err == nil {
			c.backup, c.tmp = c.tmp, ""
			return nil
		}
		if !errors.Is(err, errSwapUnsupported) {
			return err
		}
		dir, base := filepath.Split(c.target)
		bak := filepath.Join(dir, parser.RenameTempName(base, os.Getpid(), tempSeq.Add(1)))
		if err := renameBackup(c.target, bak); err == nil {
			c.backup = bak
		}
	}
	if err := renameReplace(c.tmp, c.target); err != nil {
		return err
	}
	c.tmp = ""
	return nil
}

// removeBackups removes the backups of a rename that went through. It runs
// in the background after renameMu is released: it is cleanup, and neither
// the requester nor another editor waits for it. A backup has the temp file
// name pattern, so a stop before the removal leaves nothing that the next
// start does not remove.
func (t *diskTxn) removeBackups() {
	parallel(len(t.changes), func(i int) error {
		if c := t.changes[i]; c.backup != "" {
			_ = os.Remove(c.backup)
			c.backup = ""
		}
		return nil
	})
}

// undo takes phase 1 back for one file.
func (c *diskChange) undo() error {
	if c.from != c.to {
		err := os.Remove(c.to)
		c.discard()
		return err
	}
	if c.backup == "" {
		return errors.New("no backup on this file system")
	}
	if err := os.Rename(c.backup, c.target); err != nil {
		return err
	}
	c.backup = ""
	return nil
}

// finish is phase 2 for one file.
func (c *diskChange) finish() error {
	if c.from == c.to {
		return nil
	}
	if err := os.Remove(c.from); err != nil {
		// Take the move back: the old file still holds the module.
		if rerr := os.Remove(c.to); rerr != nil {
			return &bothFilesError{err}
		}
		return err
	}
	return nil
}

// mkdirAllTracked is os.MkdirAll that returns the directories it made,
// outermost first.
func mkdirAllTracked(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	var created []string
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return created, err
		}
		created = append(created, missing[i])
	}
	return created, nil
}

// stageError is the refusal for a rename whose files could not be prepared.
func (s *Server) stageError(err error) error {
	var msg string
	var te *tempError
	var pe *stagePathError
	var ce *changedError
	var ae *appearedError
	switch {
	case errors.As(err, &ce):
		msg = fmt.Sprintf("Dexter: the rename was not applied, because %s changed on disk while the rename ran. No file was changed; rename again.", s.displayPath(ce.path))
	case errors.As(err, &ae):
		msg = fmt.Sprintf("Dexter: the rename was not applied, because %s appeared while the rename ran, and the rename must move a file there. No file was changed.", s.displayPath(ae.path))
	case errors.As(err, &te):
		msg = fmt.Sprintf("Dexter: the rename was not applied, because Dexter cannot create a temporary file in %s (%s), and it writes no file in place. No file was changed. Make the directory writable, then rename again.",
			s.displayPath(te.dir), errText(te.err))
	case errors.As(err, &pe):
		msg = fmt.Sprintf("Dexter: the rename was not applied, because Dexter could not write the new text of %s (%s). No file was changed.",
			s.displayPath(pe.path), errText(pe.err))
	default:
		msg = fmt.Sprintf("Dexter: the rename was not applied, because Dexter could not write the new text (%v). No file was changed.", err)
	}
	s.renameNotify(notify.Error, msg)
	return errors.New(strings.TrimPrefix(msg, "Dexter: "))
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// nearestExistingDir returns dir, or its nearest ancestor that exists: the
// directory in which a move makes the missing ones.
func nearestExistingDir(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || filepath.Dir(d) == d {
			return d
		}
	}
}

// openMoveErr is the refusal for a rename that must move files that another
// editor has open (rule 2 of renameFileAction).
func (s *Server) openMoveErr(paths []string) error {
	shown := make([]string, len(paths))
	for i, p := range paths {
		shown[i] = s.displayPath(p)
	}
	sort.Strings(shown)
	verb := "is"
	if len(shown) > 1 {
		verb = "are"
	}
	msg := fmt.Sprintf("Dexter: %s %s open in another editor; close it there, then rename again. The rename must move the file, and the other editor would save it back under its old name. No file was changed.",
		notify.Summarize(shown), verb)
	s.renameNotify(notify.Error, msg)
	return errors.New(strings.TrimPrefix(msg, "Dexter: "))
}

// renameTestHookBeforeCommit, when a test sets it, runs after the files are
// staged and before the editors' open files are checked again.
var renameTestHookBeforeCommit func()

// renameTestHookBeforeStage, when a test sets it, runs after the plan and
// before the files are staged.
var renameTestHookBeforeStage func()

// commitDisk stages the files of txn, checks the editors' open files again,
// and puts the staged files in place.
//
// Staging is the slow part (a write and a flush for each file), and it runs
// without renameMu, so an editor that types meanwhile does not wait. Then,
// with renameMu held for writing, so that no document opens, changes, or
// closes, each file is decided again with renameFileAction: a file that an
// editor opened, or changed, after the planning reads stops the rename, the
// staged files are removed, and no file changes. The renames that put the
// staged files in place run under the same lock, and they are quick.
func (s *Server) commitDisk(txn *diskTxn, requester renameRequester) (*commitResult, error) {
	txn.rel = s.displayPath
	canceled := func() error {
		if ctx := s.index.renameCtx; ctx != nil && ctx.Err() != nil {
			return fmt.Errorf("the rename was canceled before it changed any file: %w", ctx.Err())
		}
		return nil
	}
	if err := canceled(); err != nil {
		return nil, err
	}
	if renameTestHookBeforeStage != nil {
		renameTestHookBeforeStage()
	}
	if err := txn.stage(); err != nil {
		return nil, s.stageError(err)
	}
	if renameTestHookBeforeCommit != nil {
		renameTestHookBeforeCommit()
	}
	if err := canceled(); err != nil {
		txn.discard()
		return nil, err
	}
	// The lock is released also on a panic: didOpen, didChange, and didClose
	// of every session wait for it.
	res, err := func() (*commitResult, error) {
		s.index.renameMu.Lock()
		defer s.index.renameMu.Unlock()
		if err := s.recheckOpenFiles(txn, requester); err != nil {
			return nil, err
		}
		if err := s.recheckDisk(txn); err != nil {
			return nil, err
		}
		return txn.commit(), nil
	}()
	if err != nil {
		txn.discard()
		return nil, err
	}
	res.report(s.index.renameReport, txn)
	s.index.backgroundWork.Add(1)
	go func() {
		defer s.index.backgroundWork.Done()
		defer s.index.reporter.Recover("request", "the removal of rename backups")
		txn.removeBackups()
	}()
	if res.rolledBack != "" && len(res.changed) == 0 {
		return nil, s.renameRefusal(fmt.Sprintf(
			"Dexter: the rename was not applied, because Dexter could not replace %s (%s). The files it had already replaced were restored; no file was changed.",
			s.displayPath(res.rolledBack), errText(res.cause)))
	}
	if msg := res.failures.message(); msg != "" {
		s.renameNotify(notify.Error, msg)
		if res.rolledBack != "" {
			return nil, errors.New(strings.TrimPrefix(msg, "Dexter: "))
		}
	}
	return res, nil
}

// recheckOpenFiles decides each file of txn again, now that editors cannot
// open or change documents (the caller holds renameMu): this session's
// editor, or another, may have opened one since the plan. It costs one
// registry lookup and one lookup in this session for each file that no
// editor has open.
func (s *Server) recheckOpenFiles(txn *diskTxn, requester renameRequester) error {
	var unsaved unsavedFiles
	var moved, opened []string
	for _, c := range txn.changes {
		here, openHere := s.docs.GetIfOpen(pathKey(c.from))
		others := s.index.openDocs.heldTexts(s.docs, c.from)
		if openHere && requester != requesterEditor {
			others = append(others, here)
		}
		if !openHere && len(others) == 0 {
			continue
		}
		switch renameFileAction(requester, openHere, others, c.orig, c.from != c.to) {
		case actionDisk:
		case actionRefuse:
			unsaved.add(c.from)
		case actionRefuseMove:
			moved = append(moved, c.from)
		default:
			opened = append(opened, s.displayPath(c.from))
		}
	}
	if err := s.unsavedErr(&unsaved); err != nil {
		return err
	}
	if len(moved) > 0 {
		return s.openMoveErr(moved)
	}
	if len(opened) > 0 {
		sort.Strings(opened)
		msg := fmt.Sprintf("Dexter: the rename was not applied, because an editor opened %s while the rename ran. No file was changed; rename again.",
			notify.Summarize(opened))
		s.renameNotify(notify.Error, msg)
		return errors.New(strings.TrimPrefix(msg, "Dexter: "))
	}
	return nil
}

// recheckDisk refuses the rename when a file it will replace changed on disk
// after the rename read it (a git checkout, a formatter): the staged text
// would drop that change. A file with the stamp of the read is unchanged and
// costs one stat; another stamp makes it compare the content.
func (s *Server) recheckDisk(txn *diskTxn) error {
	var changed, appeared []string
	for _, c := range txn.changes {
		info, err := os.Stat(c.from)
		if err != nil || stampOf(info) != c.stamp {
			changed = append(changed, s.displayPath(c.from))
		}
		if c.from != c.to {
			if _, err := os.Lstat(c.to); err == nil {
				appeared = append(appeared, s.displayPath(c.to))
			}
		}
	}
	if len(appeared) > 0 {
		sort.Strings(appeared)
		return s.renameRefusal(fmt.Sprintf("Dexter: the rename was not applied, because %s appeared while the rename ran, and the rename must move a file there. No file was changed.",
			notify.Summarize(appeared)))
	}
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)
	return s.renameRefusal(fmt.Sprintf("Dexter: the rename was not applied, because %s changed on disk while the rename ran. No file was changed; rename again.",
		notify.Summarize(changed)))
}

// stagePathError is a stage failure for one path.
type stagePathError struct {
	path string
	err  error
}

func (e *stagePathError) Error() string { return e.path + ": " + e.err.Error() }
func (e *stagePathError) Unwrap() error { return e.err }

// changedError is a file that changed on disk after the rename read it.
type changedError struct{ path string }

func (e *changedError) Error() string { return e.path + " changed on disk" }

// appearedError is a file that appeared at the destination of a move after
// the rename checked it.
type appearedError struct{ path string }

func (e *appearedError) Error() string { return e.path + " exists" }

// tempError is a directory where no temp file can be made.
type tempError struct {
	dir string
	err error
}

func (e *tempError) Error() string { return e.dir + ": " + e.err.Error() }
func (e *tempError) Unwrap() error { return e.err }

// errText is the cause of err without the path that an *os.PathError adds,
// because the messages name the path relative to the project themselves.
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
