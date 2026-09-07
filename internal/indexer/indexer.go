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
	Warn func(format string, args ...interface{})
}

func (o Options) warn(format string, args ...interface{}) {
	if o.Warn != nil {
		o.Warn(format, args...)
	}
}

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

	if !opts.InProcess {
		// Safe only because this caller owns the database outright and the
		// process exits after the build.
		if err := s.SetBulkPragmas(); err != nil {
			opts.warn("bulk pragma setup: %v", err)
		}
	}

	// Phase 1: collect file paths and mtimes. Both halves run on all cores:
	// the traversal fans out per directory, and stat costs one syscall per
	// file — ~70k on a large monorepo, which dominated this phase.
	filePaths := parser.CollectElixirFilesParallel(projectRoot)
	var stdlibPaths []string
	if opts.StdlibRoot != "" {
		stdlibPaths = parser.CollectElixirFilesParallel(opts.StdlibRoot)
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
					opts.warn("%s: %v", f.path, err)
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
		return stats, fmt.Errorf("drop indexes: %w", err)
	}

	batch, err := s.BeginBulkInsert()
	if err != nil {
		drain(resultCh)
		return stats, restoreIndexes(s, fmt.Errorf("begin bulk insert: %w", err))
	}

	for _, sr := range stdlibResults {
		if err := batch.IndexFileWithMtimeAndRefs(sr.path, sr.mtimeNano, sr.defs, nil); err != nil {
			opts.warn("%s: %v", sr.path, err)
			continue
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
			opts.warn("%s: %v", res.path, err)
			continue
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
	if err := s.CreateIndexes(); err != nil {
		return stats, fmt.Errorf("create indexes: %w", err)
	}
	stats.CreateIndexes = time.Since(indexStart)

	// Last, so that a build interrupted before this point leaves a version the
	// next start rejects, and rebuilds.
	if err := s.SetIndexVersion(version.IndexVersion); err != nil {
		opts.warn("failed to store index version: %v", err)
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

func drain(ch <-chan parseResult) {
	for range ch { //nolint:revive // draining so the parse workers can exit
	}
}

// restoreIndexes puts the indexes back after a failed bulk load, so that a
// database left behind by a failure is slow rather than unusable. The original
// error is what the caller sees; a failure to restore is appended to it.
func restoreIndexes(s *store.Store, cause error) error {
	if err := s.CreateIndexes(); err != nil {
		return fmt.Errorf("%w (and restoring indexes failed: %v)", cause, err)
	}
	return cause
}
