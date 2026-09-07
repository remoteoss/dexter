// Package indexer builds a complete index from source, in one pass, for a
// database that has nothing usable in it.
//
// Both entry points that need a cold index share this code: `dexter init`,
// which runs it in a process that exits afterwards, and the LSP server, which
// runs it in-process when it finds an empty index. The two used to have
// separate implementations, and the server's was the slower one by a wide
// margin — it parsed one file at a time and committed a transaction per file,
// against live indexes.
package indexer

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
)

// ErrUnindexed reports that the SQL indexes could not be recreated after they
// were dropped for a bulk load. The data may have committed or the load may
// have rolled back, but either way every query is now a full table scan.
var ErrUnindexed = errors.New("SQL indexes could not be created after bulk build")

// Options configures a full build.
type Options struct {
	// StdlibRoot is the Elixir stdlib lib root, or "" to index only the
	// project. Stdlib files are indexed for definitions only.
	StdlibRoot string

	// InProcess marks a caller that shares the database with live readers and
	// writers — the LSP server. It suppresses the connection-wide pragmas,
	// which cannot be applied or undone safely on a live pool. See
	// store.SetBulkPragmas.
	InProcess bool

	// Warn reports a recoverable per-file failure. Optional.
	//
	// It is called from every parse worker, so it must be safe for concurrent
	// use. FullBuild serialises its own calls through serialWarn, so a callback
	// that only forwards to log.Printf needs nothing extra; one that counts or
	// accumulates should still not assume single-threaded access from any other
	// caller.
	Warn func(format string, args ...interface{})
}

// serialWarn returns a callback that forwards to Warn under a mutex. Options is
// passed by value, so the lock cannot live on the struct — a copy would carry a
// copy of the mutex and guard nothing.
func (o Options) serialWarn() func(string, ...interface{}) {
	if o.Warn == nil {
		return func(string, ...interface{}) {}
	}
	var mu sync.Mutex
	return func(format string, args ...interface{}) {
		mu.Lock()
		defer mu.Unlock()
		o.Warn(format, args...)
	}
}

const (
	createIndexAttempts   = 3
	createIndexRetryDelay = 250 * time.Millisecond
)

// Stats reports what a full build did, and how long each phase took. The
// durations are always collected; `dexter init --profile` prints them.
type Stats struct {
	Files       int
	Definitions int
	References  int
	Workers     int

	Walk          time.Duration
	Parse         time.Duration // summed across workers, so larger than wall time
	Write         time.Duration
	Commit        time.Duration
	CreateIndexes time.Duration
	Total         time.Duration
}

type fileEntry struct {
	path      string
	mtimeNano int64
}

// statFilesParallel stats paths across all cores and returns the entries whose
// stat succeeded, in the order they were given. Nothing depends on that order —
// rows are keyed by path — but the walk order is the cheapest one to keep.
func statFilesParallel(paths []string) []fileEntry {
	if len(paths) == 0 {
		return nil
	}
	workers := runtime.NumCPU()
	if workers > len(paths) {
		workers = len(paths)
	}
	out := make([]fileEntry, len(paths))
	ok := make([]bool, len(paths))
	var wg sync.WaitGroup
	var next atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(paths) {
					return
				}
				info, err := os.Stat(paths[i])
				if err != nil {
					continue
				}
				out[i] = fileEntry{path: paths[i], mtimeNano: info.ModTime().UnixNano()}
				ok[i] = true
			}
		}()
	}
	wg.Wait()
	entries := out[:0]
	for i := range out {
		if ok[i] {
			entries = append(entries, out[i])
		}
	}
	return entries
}

// FullBuild indexes projectRoot (and opts.StdlibRoot, when set) from scratch:
// walk and stat on all cores, parse on all cores, then one single-writer bulk
// transaction with the indexes dropped, and finally the index version.
//
// The caller is responsible for the database being empty, and for no other
// writer touching it while this runs. The bulk path is insert-only — it skips
// the DELETE that the incremental path does, and allocates file ids from a
// counter — so a concurrent write would produce duplicate rows or collide on a
// primary key.
func FullBuild(s *store.Store, projectRoot string, opts Options) (Stats, error) {
	var stats Stats
	start := time.Now()

	warn := opts.serialWarn()

	if !opts.InProcess {
		// Safe only because this caller owns the database outright and the
		// process exits after the build.
		if err := s.SetBulkPragmas(); err != nil {
			warn("bulk pragma setup: %v", err)
		}
	}

	// Phase 1: collect file paths and mtimes. Both halves run on all cores:
	// the traversal fans out per directory, and stat costs one syscall per
	// file — ~70k on a large monorepo, which dominated this phase.
	filePaths := parser.CollectElixirFilesParallel(projectRoot)
	var stdlibPaths []string
	if opts.StdlibRoot != "" {
		stdlibPaths = dedupeAgainst(parser.CollectElixirFilesParallel(opts.StdlibRoot), filePaths)
	}
	files := statFilesParallel(filePaths)
	stdlibFiles := statFilesParallel(stdlibPaths)
	stats.Walk = time.Since(start)

	// Phase 2a: parse stdlib files in parallel. Definitions only — refs are
	// not indexed for stdlib.
	stdlibResults := parseStdlib(stdlibFiles)

	// Phase 2b: parse project files in parallel, streaming to the writer.
	workers := runtime.NumCPU()
	stats.Workers = workers
	fileCh := make(chan fileEntry, workers)
	resultCh := make(chan parseResult, 1024)

	var parseNanos atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range fileCh {
				t0 := time.Now()
				defs, refs, err := parser.ParseFile(f.path)
				if err != nil {
					warn("%s: %v", f.path, err)
					continue
				}
				parseNanos.Add(int64(time.Since(t0)))
				resultCh <- parseResult{path: f.path, mtimeNano: f.mtimeNano, defs: defs, refs: refs}
			}
		}()
	}

	go func() {
		for _, f := range files {
			fileCh <- f
		}
		close(fileCh)
		wg.Wait()
		close(resultCh)
	}()

	// Phase 3: bulk insert (single writer, single transaction, no indexes).
	//
	// The parse workers keep producing whatever happens here, so every error
	// path below has to drain resultCh before returning: an early return would
	// block them forever on a full channel, and they are the goroutines the
	// caller's WaitGroup is waiting on.
	if err := s.DropIndexes(); err != nil {
		drain(resultCh)
		return stats, restoreIndexes(s, fmt.Errorf("drop indexes: %w", err))
	}

	batch, err := s.BeginBulkInsert()
	if err != nil {
		drain(resultCh)
		return stats, restoreIndexes(s, fmt.Errorf("begin bulk insert: %w", err))
	}

	// A write failure inside the bulk transaction is not recoverable, so it ends
	// the build rather than being warned about. The files row is written before
	// the definitions and refs (see store.Batch.indexFile), and insert-only mode
	// buffers rows across files, so continuing past one would commit a file with
	// a current mtime and no symbols — invisible to the mtime sweep forever —
	// and could also drop buffered rows belonging to files already counted here.
	// Returning instead leaves SetIndexVersion unreached, so the next start
	// rebuilds.
	for _, sr := range stdlibResults {
		if err := batch.IndexFileWithMtimeAndRefs(sr.path, sr.mtimeNano, sr.defs, nil); err != nil {
			return stats, abortBuild(s, batch, resultCh, fmt.Errorf("%s: %w", sr.path, err))
		}
		stats.Files++
		stats.Definitions += len(sr.defs)
	}

	var writeNanos time.Duration
	for res := range resultCh {
		writeStart := time.Now()
		err := batch.IndexFileWithMtimeAndRefs(res.path, res.mtimeNano, res.defs, res.refs)
		writeNanos += time.Since(writeStart)
		if err != nil {
			return stats, abortBuild(s, batch, resultCh, fmt.Errorf("%s: %w", res.path, err))
		}
		stats.Files++
		stats.Definitions += len(res.defs)
		stats.References += len(res.refs)
	}
	stats.Write = writeNanos
	stats.Parse = time.Duration(parseNanos.Load())

	commitStart := time.Now()
	if err := batch.Commit(); err != nil {
		return stats, restoreIndexes(s, fmt.Errorf("commit: %w", err))
	}
	stats.Commit = time.Since(commitStart)

	indexStart := time.Now()
	if err := createIndexesWithRetry(s); err != nil {
		return stats, fmt.Errorf("%w: %v", ErrUnindexed, err)
	}
	stats.CreateIndexes = time.Since(indexStart)

	// Last, so that a build interrupted before this point leaves a version the
	// next start rejects, and rebuilds.
	if err := s.SetIndexVersion(version.IndexVersion); err != nil {
		warn("failed to store index version: %v", err)
	}

	stats.Total = time.Since(start)
	return stats, nil
}

type parseResult struct {
	path      string
	mtimeNano int64
	defs      []parser.Definition
	refs      []parser.Reference
}

type stdlibResult struct {
	path      string
	mtimeNano int64
	defs      []parser.Definition
}

func parseStdlib(stdlibFiles []fileEntry) []stdlibResult {
	if len(stdlibFiles) == 0 {
		return nil
	}
	results := make([]stdlibResult, 0, len(stdlibFiles))
	ch := make(chan stdlibResult, len(stdlibFiles))
	workers := runtime.NumCPU()
	fileCh := make(chan fileEntry, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range fileCh {
				defs, _, err := parser.ParseFile(f.path)
				if err != nil {
					continue
				}
				ch <- stdlibResult{f.path, f.mtimeNano, defs}
			}
		}()
	}
	go func() {
		for _, f := range stdlibFiles {
			fileCh <- f
		}
		close(fileCh)
		wg.Wait()
		close(ch)
	}()

	for r := range ch {
		results = append(results, r)
	}
	return results
}

// createIndexesWithRetry recreates the indexes, retrying a few times. The
// statements are CREATE ... IF NOT EXISTS, so a retry is free and idempotent,
// and the likeliest failure here is SQLITE_BUSY against another pooled
// connection, which clears on its own.
func createIndexesWithRetry(s *store.Store) error {
	var err error
	for attempt := 0; attempt < createIndexAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * createIndexRetryDelay)
		}
		if err = s.CreateIndexes(); err == nil {
			return nil
		}
	}
	return err
}

// dedupeAgainst returns the entries of paths that do not appear in exclude.
//
// The stdlib root can resolve inside the project root — an explicit
// initializationOptions.stdlibPath, DEXTER_ELIXIR_LIB_ROOT, or a vendored
// elixir checkout under deps/, none of which stdlib.Resolve checks — and the
// project walk descends into deps/. The insert-only batch cannot upsert, so an
// overlapping path would fail on files.path UNIQUE and end the build. Matching
// exact paths rather than testing directory containment also covers the cases
// a prefix test misses, such as two roots aliased through a symlink.
func dedupeAgainst(paths, exclude []string) []string {
	if len(paths) == 0 || len(exclude) == 0 {
		return paths
	}
	seen := make(map[string]struct{}, len(exclude))
	for _, p := range exclude {
		seen[p] = struct{}{}
	}
	out := paths[:0:0]
	for _, p := range paths {
		if _, dup := seen[p]; dup {
			continue
		}
		out = append(out, p)
	}
	return out
}

func drain(ch <-chan parseResult) {
	for range ch { //nolint:revive // draining so the parse workers can exit
	}
}

// abortBuild ends a build that has already opened its bulk transaction. The
// order matters: drain first, because the parse workers keep producing and the
// caller's WaitGroup is waiting on them; then roll the transaction back, or its
// write lock outlives the build and the pooled connection never comes back —
// restoreIndexes runs its DDL on a different connection and would block on it.
func abortBuild(s *store.Store, batch *store.Batch, resultCh <-chan parseResult, cause error) error {
	drain(resultCh)
	if err := batch.Rollback(); err != nil {
		cause = fmt.Errorf("%w (and rolling back failed: %v)", cause, err)
	}
	return restoreIndexes(s, cause)
}

func restoreIndexes(s *store.Store, cause error) error {
	if err := createIndexesWithRetry(s); err != nil {
		return errors.Join(cause, fmt.Errorf("%w: %v", ErrUnindexed, err))
	}
	return cause
}
