package lsp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
)

// Write batching for the warm reconciliation. One transaction per file made
// each commit write every index page the file touched: on a 280k-file index, a
// warm pass over 223k new files spent 59% of its CPU time in COMMIT and took
// almost four minutes. A batch shares those pages across many files. The time
// bound keeps the write lock short for any other writer: a batch commits at
// whichever limit it reaches first.
var (
	reconcileBatchFiles = 2048
	reconcileBatchTime  = 500 * time.Millisecond
)

// A change set this large, and at least 1/rebuildShare of the files the index
// will hold, is written with store.BeginRebuild instead: the live indexes then
// take no row-at-a-time updates at all. On a 280k-file index, a warm pass over
// 223k new files took 1m58s through batches and 44s as a rebuild (3m48s with
// one transaction per file). A batch costs about 0.5ms per changed file there,
// a rebuild about 0.16ms per file in the whole index, so they break even near
// a third; a quarter leaves margin for the larger index a rebuild re-sorts.
var (
	rebuildMinFiles = 4096
	rebuildShare    = 4
)

// testHookParse, when set by a test, runs before each changed file is parsed.
var testHookParse func(path string)

// ErrRootGone reports that the project root was moved, deleted, or unmounted,
// or that its path now names a different directory. A walk of it then yields
// nothing, and a prune after it would delete every stored file, so the pass
// stops before it writes or prunes anything. The workspace must stop too.
var ErrRootGone = errors.New("project root is gone")

// ErrIncomplete reports a pass that left the index incomplete: some index
// writes failed (for example because the disk is full), or the project root
// could not be read. The files that failed keep their old stored mtime, so a
// later pass writes them again; the caller schedules that pass.
var ErrIncomplete = errors.New("index is incomplete")

// errIndexUnavailable reports that the index lost its SQL indexes. The user
// was told to rebuild it, and no retry can help.
var errIndexUnavailable = errors.New("index is unavailable")

// checkProjectRoot reports whether the project root is still the project the
// index was built from. A false "gone" stops the daemon and ends every editor
// session, so only clear evidence counts:
//
//   - the root path does not exist, or is not a directory (ENOENT, ENOTDIR);
//   - the root is a different directory than the first check saw (dev and
//     inode), and the file at the index path is not the open index file
//     (store.SameDatabaseFile: missing, or another inode). A moved root takes
//     its index with it, and a copy put in its place has a copy of the index.
//
// Any other stat error (EACCES, EPERM from macOS privacy controls, EIO, ESTALE
// or ETIMEDOUT on a network mount) can clear again, so it gives an
// ErrIncomplete error: the pass changes nothing, and a retry comes later. A
// new root identity with the same index file is taken as a remount (autofs,
// NFS, FUSE can give the same directory a new device and inode); the new
// identity is recorded and the work goes on.
//
// The normal check is one stat.
func (s *Server) checkProjectRoot() error {
	root := s.projectRoot
	info, err := statRoot(root)
	switch {
	case isGoneErr(err):
		return fmt.Errorf("%w: %v", ErrRootGone, err)
	case err != nil:
		return fmt.Errorf("%w: cannot read the project root: %v", ErrIncomplete, err)
	case !info.IsDir():
		return fmt.Errorf("%w: %s is not a directory", ErrRootGone, root)
	}
	c := s.index
	c.rootMu.Lock()
	defer c.rootMu.Unlock()
	if c.rootPath != root || c.rootInfo == nil || os.SameFile(c.rootInfo, info) {
		c.rootPath, c.rootInfo = root, info
		return nil
	}
	same, dbErr := s.store.SameDatabaseFile()
	switch {
	case dbErr != nil:
		return fmt.Errorf("%w: cannot read the index file: %v", ErrIncomplete, dbErr)
	case !same:
		return fmt.Errorf("%w: %s is now a different directory, without the open index file %s", ErrRootGone, root, s.store.Path())
	}
	log.Printf("Warning: the project root %s has a new file identity, but the index file is the same; continuing (a remount can do this)", root)
	c.rootInfo = info
	return nil
}

// statRoot is os.Stat. Tests replace it to simulate errors.
var statRoot = os.Stat

// isGoneErr reports whether err from a stat says the path is not there.
func isGoneErr(err error) bool { return IsMissing(err) }

// IsMissing reports whether err from a stat says the path is not there:
// ENOENT, or ENOTDIR when a directory in the path is now a file. Everything
// that decides "missing" must use it; os.IsNotExist is false for ENOTDIR.
func IsMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// CheckProjectRoot is checkProjectRoot for the workspace runtime, which checks
// the root before it applies a batch of watcher events.
func (s *Server) CheckProjectRoot() error { return s.checkProjectRoot() }

// changedFile is one file the walk found new or changed.
type changedFile struct {
	path      string
	mtimeNano int64 // from the walk's lstat; the target's stat for a symlink
	refs      bool
}

type parsedFile struct {
	changedFile
	defs     []parser.Definition
	refsList []parser.Reference
}

// reconcileChangedFiles walks the stdlib and project roots and indexes every
// file whose mtime differs from the stored one. It returns the set of paths the
// walk saw (for the prune), how many files it wrote, and how many failed to
// write. A non-nil error means the walk is not a complete answer (the index is
// unavailable, the pass was canceled, or the project root could not be walked
// or is gone), and nothing may be pruned.
//
// The stored mtimes are read with one query before the walk instead of one
// query per file, and the walk itself is unchanged: one lstat per file, from
// the directory entry. Changed files are parsed on every core while the walk
// continues, and one writer stores them in batched transactions. A file and its
// rows are always in one transaction, so a canceled pass leaves each file either
// fully old or fully new, and the next pass redoes the rest from their mtimes.
// rebuildWithoutReadLock runs rebuildChanged without the read lock on writes
// that the caller holds, and takes the read lock again in a defer. Without
// the defer, a panic in the rebuild would unwind into the caller's deferred
// RUnlock of a lock that is not held, which is a fatal error that no recover
// stops, and every new daemon would repeat it on the same changed files.
func (s *Server) rebuildWithoutReadLock(ctx context.Context, changed []changedFile, progress *reconcileProgress) (int, bool) {
	s.index.writes.RUnlock()
	defer s.index.writes.RLock()
	return s.rebuildChanged(ctx, changed, progress)
}

func (s *Server) reconcileChangedFiles(progress *reconcileProgress) (seen map[string]struct{}, written, failed int, err error) {
	ctx := s.index.work
	// The walk writes, so it takes indexWrites for reading, the same as every
	// other single-file write. That is what keeps it from overlapping a cold
	// build.
	s.index.writes.RLock()
	defer s.index.writes.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, 0, 0, err
	}
	if s.index.unavailable {
		return nil, 0, 0, errIndexUnavailable
	}
	if err := s.checkProjectRoot(); err != nil {
		return nil, 0, 0, err
	}

	stored, err := s.store.FileStates()
	if err != nil {
		log.Printf("Warning: reading stored file states: %v", err)
		return nil, 0, 0, fmt.Errorf("%w: reading stored file states: %v", ErrIncomplete, err)
	}

	seen = make(map[string]struct{}, len(stored))
	var changed []changedFile
	walk := func(root string, indexRefs bool) error {
		return parser.WalkElixirFiles(root, func(path string, d fs.DirEntry) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// The stdlib root can lie inside the project; the second visit of
			// a path must not queue it again.
			if _, dup := seen[path]; dup {
				return nil
			}
			seen[path] = struct{}{}

			var info fs.FileInfo
			var err error
			if d.Type()&fs.ModeSymlink != 0 {
				// The stored mtime is the target's, as the cold build and
				// single-file writes record it, so it is compared with the
				// target's. The link's own mtime never matches it, and every
				// pass wrote the file again. Only a symlink pays this stat.
				info, err = os.Stat(path)
				if err != nil {
					// A link whose target is gone keeps its old rows, and
					// the editor is told, as for a file that cannot be read.
					s.reconcileParseFailed(path, err)
					return nil
				}
			} else if info, err = d.Info(); err != nil {
				return nil
			}
			mtime := info.ModTime().UnixNano()
			if st, found := stored[path]; found && st.Mtime == mtime {
				return nil
			}
			changed = append(changed, changedFile{path: path, mtimeNano: mtime, refs: indexRefs})
			return nil
		})
	}

	// A stdlib root that cannot be walked (an uninstalled Elixir version, for
	// example) is not an error: its files are gone, and the prune removes them
	// one by one after it stats each of them.
	if stdlibRoot := s.StdlibRoot(); stdlibRoot != "" {
		_ = walk(stdlibRoot, false)
	}
	walkErr := walk(s.projectRoot, true)
	if err := ctx.Err(); err != nil {
		return seen, 0, 0, err
	}
	// The root can go during the walk, which then sees only part of the tree.
	// The second check also finds a root that was replaced while it was walked.
	if walkErr != nil {
		if rootErr := s.checkProjectRoot(); rootErr != nil {
			return nil, 0, 0, rootErr
		}
		return nil, 0, 0, fmt.Errorf("walking %s: %w", s.projectRoot, walkErr)
	}
	if err := s.checkProjectRoot(); err != nil {
		return nil, 0, 0, err
	}

	switch {
	case len(changed) == 0:
	case len(changed) >= rebuildMinFiles && len(changed)*rebuildShare >= len(stored)+len(changed):
		// The rebuild must be the only writer for its whole transaction, as a
		// cold build is, so it trades the read lock for the write lock. A
		// single-file write that lands in between is either rewritten here or
		// copied over as it is.
		n, ok := s.rebuildWithoutReadLock(ctx, changed, progress)
		written = n
		if !ok && ctx.Err() == nil && !s.index.unavailable {
			// Batches are slower but need nothing the rebuild did, so a
			// failed rebuild does not leave the change set unindexed.
			log.Printf("Warning: index rebuild failed, writing %d changed files in batches", len(changed))
			written, failed = s.writeChangedInBatches(ctx, changed, progress)
		}
	default:
		written, failed = s.writeChangedInBatches(ctx, changed, progress)
	}
	if err := ctx.Err(); err != nil {
		return seen, written, failed, err
	}
	return seen, written, failed, nil
}

// writeChangedInBatches parses changed files on every core and writes them
// through batched transactions. It returns how many files it wrote and how
// many failed to write.
func (s *Server) writeChangedInBatches(ctx context.Context, changed []changedFile, progress *reconcileProgress) (written, failed int) {
	pipe := s.startReconcilePipeline(ctx, progress)
	for _, f := range changed {
		if pipe.send(f) != nil {
			break
		}
	}
	return pipe.finish()
}

// parsePool parses changed files on every core.
type parsePool struct {
	ctx     context.Context
	work    chan changedFile
	results chan parsedFile
}

// newParsePool starts the parse workers. onError, which several workers can
// call at once, is told about each file that could not be read or parsed.
func newParsePool(ctx context.Context, onError func(path string, err error)) *parsePool {
	workers := runtime.NumCPU()
	p := &parsePool{
		ctx:     ctx,
		work:    make(chan changedFile, workers*4),
		results: make(chan parsedFile, workers*4),
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range p.work {
				if ctx.Err() != nil {
					continue // drain without parsing
				}
				p.parseOne(f, onError)
			}
		}()
	}
	go func() {
		wg.Wait()
		close(p.results)
	}()
	return p
}

// parseOne parses one changed file and sends the result. A panic is the
// failure of that file only, so the pool keeps draining its work.
func (p *parsePool) parseOne(f changedFile, onError func(path string, err error)) {
	defer func() {
		if value := recover(); value != nil {
			log.Printf("Error: panic while parsing %s: %v\n%s", f.path, value, debug.Stack())
			onError(f.path, fmt.Errorf("internal error: %v", value))
		}
	}()
	if testHookParse != nil {
		testHookParse(f.path)
	}
	defs, refs, err := parser.ParseFile(f.path)
	if err != nil {
		onError(f.path, err)
		return
	}
	if !f.refs {
		refs = nil
	}
	select {
	case p.results <- parsedFile{changedFile: f, defs: defs, refsList: refs}:
	case <-p.ctx.Done():
	}
}

// send queues one changed file for parsing. It returns the context error when
// the pass is canceled.
func (p *parsePool) send(f changedFile) error {
	select {
	case p.work <- f:
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

// reconcilePipeline is a parse pool with one writer that stores its results in
// batched transactions.
type reconcilePipeline struct {
	*parsePool
	counts chan [2]int // files written, files that failed to write
}

func (s *Server) startReconcilePipeline(ctx context.Context, progress *reconcileProgress) *reconcilePipeline {
	p := &reconcilePipeline{parsePool: newParsePool(ctx, s.reconcileParseFailed), counts: make(chan [2]int, 1)}
	go func() {
		defer func() {
			if value := recover(); value != nil {
				// A batch transaction can be open; the workspace restarts its
				// index. The parse workers must not wait on a writer that is
				// gone, and the pass must get its counts.
				s.index.reporter.Panicked("index", "the index writer", value, debug.Stack())
				s.index.faulted("the index writer")(value)
				for range p.results {
				}
				p.counts <- [2]int{0, 0}
			}
		}()
		written, failed := s.writeReconciled(ctx, p.results, progress)
		p.counts <- [2]int{written, failed}
	}()
	return p
}

// reconcileParseFailed records a changed file that could not be read or
// parsed. Its old rows stay, and the editor is told about it.
func (s *Server) reconcileParseFailed(path string, err error) {
	if !errors.Is(err, fs.ErrNotExist) {
		log.Printf("Warning: reindex %s: %v", path, err)
	}
	s.index.failures.fail(path, err)
}

// reconcileWritten records a file whose new rows are committed.
func (s *Server) reconcileWritten(path string, progress *reconcileProgress) {
	s.index.failures.ok(path)
	if progress != nil {
		progress.file()
	}
}

// reconcileWriteFailed records a changed file whose rows could not be
// written. It keeps its old mtime, so the next pass tries it again.
func (s *Server) reconcileWriteFailed(path string, err error) {
	log.Printf("Warning: reindex %s: %v", path, err)
	s.index.failures.fail(path, err)
}

// finish waits for every queued file to be parsed and written, and returns how
// many files were written and how many failed to write.
func (p *reconcilePipeline) finish() (written, failed int) {
	close(p.work)
	c := <-p.counts
	return c[0], c[1]
}

// rebuildChanged parses the change set on every core and writes it through one
// rebuild transaction. It returns how many files it wrote, and false when the
// rebuild failed or was canceled, in which case it wrote none.
func (s *Server) rebuildChanged(ctx context.Context, changed []changedFile, progress *reconcileProgress) (int, bool) {
	s.index.writes.Lock()
	defer s.index.writes.Unlock()
	if s.index.unavailable {
		return 0, false
	}
	start := time.Now()
	batch, err := s.store.BeginRebuild(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("Warning: starting index rebuild: %v", err)
		}
		return 0, false
	}
	// A panic must not leave the transaction open with the SQLite write lock;
	// after Commit or an earlier Rollback this does nothing.
	defer func() { _ = batch.Rollback() }()
	// Canceled on return, so that after a panic below the parse workers stop
	// instead of waiting for ever on a reader that is gone.
	parseCtx, stopParse := context.WithCancel(ctx)
	defer stopParse()
	results := s.parseChanged(parseCtx, changed)
	files := 0
	var writtenPaths []string // marked written once the rebuild commits
	var writeErr error
	for res := range results {
		if writeErr != nil || ctx.Err() != nil {
			continue // drain so the parse workers can exit
		}
		if writeErr = writeParsed(batch, res); writeErr == nil {
			files++
			writtenPaths = append(writtenPaths, res.path)
		}
	}
	if writeErr == nil {
		writeErr = ctx.Err()
	}
	if writeErr != nil {
		_ = batch.Rollback()
		if ctx.Err() == nil {
			log.Printf("Warning: index rebuild: %v", writeErr)
		}
		return 0, false
	}
	if files == 0 {
		// Every changed file failed to parse, so nothing changes: their old
		// rows stay. Copying and swapping the tables would only cost time, and
		// the next start would do it again for the same files.
		_ = batch.Rollback()
		log.Printf("Index rebuild skipped: none of %d changed files could be parsed", len(changed))
		return 0, true
	}
	written := time.Now()
	if err := batch.Commit(); err != nil {
		if ctx.Err() == nil {
			log.Printf("Warning: index rebuild: %v", err)
		}
		return 0, false
	}
	for _, path := range writtenPaths {
		s.reconcileWritten(path, progress)
	}
	log.Printf("Rebuilt the index with %d changed files (parse and write %s, swap and index %s)", files,
		written.Sub(start).Round(time.Millisecond), time.Since(written).Round(time.Millisecond))
	return files, true
}

// testHookWrite, when set by a test, runs before each parsed file is written;
// an error it returns fails that write. It is atomic because a test can set it
// while a pass started by a timer is in flight.
var testHookWrite atomic.Pointer[func(path string) error]

// SetTestWriteFault makes each index write of a reconciliation pass, and each
// single-file write, call fn first and fail with the error it returns. Tests in
// other packages use it to simulate a full disk. The returned function removes
// fn.
func SetTestWriteFault(fn func(path string) error) (restore func()) {
	testHookWrite.Store(&fn)
	return func() { testHookWrite.Store(nil) }
}

func testWriteFault(path string) error {
	if hook := testHookWrite.Load(); hook != nil {
		return (*hook)(path)
	}
	return nil
}

func writeParsed(b *store.Batch, res parsedFile) error {
	if err := testWriteFault(res.path); err != nil {
		return err
	}
	return b.IndexFileWithMtimeAndRefs(res.path, res.mtimeNano, res.defs, res.refsList)
}

// parseChanged parses files on every core and streams the results. The channel
// closes when every file is parsed, or soon after ctx is canceled.
func (s *Server) parseChanged(ctx context.Context, changed []changedFile) <-chan parsedFile {
	p := newParsePool(ctx, s.reconcileParseFailed)
	go func() {
		for _, f := range changed {
			if p.send(f) != nil {
				break
			}
		}
		close(p.work)
	}()
	return p.results
}

// writeReconciled stores parsed files in batched transactions until results
// closes. When a batch fails for a reason other than cancellation, it is rolled
// back and its files are written again one by one, each in its own
// transaction, so only a file that fails by itself is lost; it keeps its old
// mtime, so the next pass retries it. After cancellation it rolls back the open
// batch and only drains. It returns how many files it wrote and how many failed
// to write.
func (s *Server) writeReconciled(ctx context.Context, results <-chan parsedFile, progress *reconcileProgress) (written, failed int) {
	var (
		batch   *store.Batch
		group   []parsedFile // the files in batch, kept for a one-by-one retry
		started time.Time
	)
	retryOneByOne := func(cause error) {
		log.Printf("Warning: reindex batch of %d files failed, writing them one by one: %v", len(group), cause)
		for _, res := range group {
			if ctx.Err() != nil {
				return
			}
			if err := s.writeOne(ctx, res); err != nil {
				if ctx.Err() == nil {
					s.reconcileWriteFailed(res.path, err)
					failed++
				}
				continue
			}
			s.reconcileWritten(res.path, progress)
			written++
		}
	}
	commit := func() {
		if batch == nil {
			return
		}
		err := batch.Commit()
		switch {
		case err == nil:
			written += len(group)
			for _, res := range group {
				s.reconcileWritten(res.path, progress)
			}
		case ctx.Err() == nil:
			retryOneByOne(err)
		}
		batch, group = nil, group[:0]
	}
	// A panic must not leave a batch open: its transaction would hold the
	// SQLite write lock for as long as the process runs.
	defer func() {
		if batch != nil {
			_ = batch.Rollback()
		}
	}()
	abort := func() {
		if batch != nil {
			_ = batch.Rollback()
			batch, group = nil, group[:0]
		}
	}

	for res := range results {
		if ctx.Err() != nil {
			abort()
			continue
		}
		if batch == nil {
			b, err := s.store.BeginBatchContext(ctx)
			if err != nil {
				if ctx.Err() == nil {
					s.reconcileWriteFailed(res.path, err)
					failed++
				}
				continue
			}
			batch, started = b, time.Now()
		}
		group = append(group, res)
		if err := writeParsed(batch, res); err != nil {
			// The failed file may be partly written inside the transaction,
			// so the batch rolls back, and its files are written one by one.
			_ = batch.Rollback()
			batch = nil
			if ctx.Err() == nil {
				retryOneByOne(err)
			}
			group = group[:0]
			continue
		}
		if len(group) >= reconcileBatchFiles || time.Since(started) >= reconcileBatchTime {
			commit()
		}
	}
	if ctx.Err() != nil {
		abort()
	} else {
		commit()
	}
	return written, failed
}

// writeOne writes one parsed file in its own transaction.
func (s *Server) writeOne(ctx context.Context, res parsedFile) error {
	b, err := s.store.BeginBatchContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = b.Rollback() }() // after Commit it does nothing
	if err := writeParsed(b, res); err != nil {
		_ = b.Rollback()
		return err
	}
	return b.Commit()
}
