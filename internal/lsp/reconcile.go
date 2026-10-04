package lsp

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"os"
	"runtime"
	"sync"
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

// changedFile is one file the walk found new or changed.
type changedFile struct {
	path      string
	mtimeNano int64 // from the walk's lstat; 0 for a symlink, statted on parse
	symlink   bool
	refs      bool
}

type parsedFile struct {
	changedFile
	defs     []parser.Definition
	refsList []parser.Reference
}

// reconcileChangedFiles walks the stdlib and project roots and indexes every
// file whose mtime differs from the stored one. It returns the set of paths the
// walk saw (for the prune), how many files it wrote, and false when the index
// is unavailable or the pass was canceled, in which case nothing may be pruned.
//
// The stored mtimes are read with one query before the walk instead of one
// query per file, and the walk itself is unchanged: one lstat per file, from
// the directory entry. Changed files are parsed on every core while the walk
// continues, and one writer stores them in batched transactions. A file and its
// rows are always in one transaction, so a canceled pass leaves each file either
// fully old or fully new, and the next pass redoes the rest from their mtimes.
func (s *Server) reconcileChangedFiles(progress *reconcileProgress) (map[string]struct{}, int, bool) {
	ctx := s.index.work
	// The walk writes, so it takes indexWrites for reading, the same as every
	// other single-file write. That is what keeps it from overlapping a cold
	// build.
	s.index.writes.RLock()
	defer s.index.writes.RUnlock()
	if s.index.unavailable || ctx.Err() != nil {
		return nil, 0, false
	}

	stored, err := s.store.FileStates()
	if err != nil {
		log.Printf("Warning: reading stored file states: %v", err)
		return nil, 0, false
	}

	seen := make(map[string]struct{}, len(stored))
	var changed []changedFile
	walk := func(root string, indexRefs bool) {
		_ = parser.WalkElixirFiles(root, func(path string, d fs.DirEntry) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// The stdlib root can lie inside the project; the second visit of
			// a path must not queue it again.
			if _, dup := seen[path]; dup {
				return nil
			}
			seen[path] = struct{}{}

			info, err := d.Info()
			if err != nil {
				return nil
			}
			mtime := info.ModTime().UnixNano()
			if st, found := stored[path]; found && st.Mtime == mtime {
				return nil
			}
			f := changedFile{path: path, mtimeNano: mtime, refs: indexRefs}
			if d.Type()&fs.ModeSymlink != 0 {
				// The stored mtime is the target's, as the cold build and
				// single-file writes record it.
				f.symlink, f.mtimeNano = true, 0
			}
			changed = append(changed, f)
			return nil
		})
	}

	if stdlibRoot := s.StdlibRoot(); stdlibRoot != "" {
		walk(stdlibRoot, false)
	}
	walk(s.projectRoot, true)

	if ctx.Err() != nil {
		return seen, 0, false
	}

	written := 0
	switch {
	case len(changed) == 0:
	case len(changed) >= rebuildMinFiles && len(changed)*rebuildShare >= len(stored)+len(changed):
		// The rebuild must be the only writer for its whole transaction, as a
		// cold build is, so it trades the read lock for the write lock. A
		// single-file write that lands in between is either rewritten here or
		// copied over as it is.
		s.index.writes.RUnlock()
		n, ok := s.rebuildChanged(ctx, changed, progress)
		s.index.writes.RLock()
		written = n
		if !ok && ctx.Err() == nil && !s.index.unavailable {
			// Batches are slower but need nothing the rebuild did, so a
			// failed rebuild does not leave the change set unindexed.
			log.Printf("Warning: index rebuild failed, writing %d changed files in batches", len(changed))
			written = s.writeChangedInBatches(ctx, changed, progress)
		}
	default:
		written = s.writeChangedInBatches(ctx, changed, progress)
	}
	if ctx.Err() != nil {
		return seen, written, false
	}
	return seen, written, true
}

// writeChangedInBatches parses changed files on every core and writes them
// through batched transactions. It returns how many files it wrote.
func (s *Server) writeChangedInBatches(ctx context.Context, changed []changedFile, progress *reconcileProgress) int {
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
				if testHookParse != nil {
					testHookParse(f.path)
				}
				if f.symlink {
					info, err := os.Stat(f.path)
					if err != nil {
						onError(f.path, err)
						continue
					}
					f.mtimeNano = info.ModTime().UnixNano()
				}
				defs, refs, err := parser.ParseFile(f.path)
				if err != nil {
					onError(f.path, err)
					continue
				}
				if !f.refs {
					refs = nil
				}
				select {
				case p.results <- parsedFile{changedFile: f, defs: defs, refsList: refs}:
				case <-ctx.Done():
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(p.results)
	}()
	return p
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
	written chan int
}

func (s *Server) startReconcilePipeline(ctx context.Context, progress *reconcileProgress) *reconcilePipeline {
	p := &reconcilePipeline{parsePool: newParsePool(ctx, s.reconcileParseFailed), written: make(chan int, 1)}
	go func() { p.written <- s.writeReconciled(ctx, p.results, progress) }()
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
// many files were written.
func (p *reconcilePipeline) finish() int {
	close(p.work)
	return <-p.written
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
	results := s.parseChanged(ctx, changed)
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
// an error it returns fails that write.
var testHookWrite func(path string) error

func writeParsed(b *store.Batch, res parsedFile) error {
	if testHookWrite != nil {
		if err := testHookWrite(res.path); err != nil {
			return err
		}
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
// batch and only drains.
func (s *Server) writeReconciled(ctx context.Context, results <-chan parsedFile, progress *reconcileProgress) int {
	var (
		batch   *store.Batch
		group   []parsedFile // the files in batch, kept for a one-by-one retry
		started time.Time
		written int
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
	return written
}

// writeOne writes one parsed file in its own transaction.
func (s *Server) writeOne(ctx context.Context, res parsedFile) error {
	b, err := s.store.BeginBatchContext(ctx)
	if err != nil {
		return err
	}
	if err := writeParsed(b, res); err != nil {
		_ = b.Rollback()
		return err
	}
	return b.Commit()
}
