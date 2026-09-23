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
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/remoteoss/dexter/internal/beam"
	"github.com/remoteoss/dexter/internal/evidence"
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

	// ProviderEvidence is revision-matched generated evidence for ImpactBuild.
	// FullBuild ignores it.
	ProviderEvidence []evidence.LoadedArtifact

	// CompiledBuildRoots and CompiledApplications override repository config for
	// library callers and focused benchmarks.
	CompiledBuildRoots   []string
	CompiledApplications map[string]string
	ProjectFiles         []string
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
	CallEdges   int
	Workers     int

	Walk          time.Duration
	Parse         time.Duration // summed across workers, so larger than wall time
	Write         time.Duration
	Commit        time.Duration
	Finalize      time.Duration
	CreateIndexes time.Duration
	Rename        time.Duration
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
	resultCh, parseNanos, workers := parseProject(files, warn, false)
	stats.Workers = workers

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
	sourceIncomplete := false
	for res := range resultCh {
		if res.err != nil {
			sourceIncomplete = true
			continue
		}
		writeStart := time.Now()
		err := batch.IndexFileWithMtimeRefsAndCalls(res.path, res.mtimeNano, res.defs, res.refs, res.calls)
		writeNanos += time.Since(writeStart)
		if err != nil {
			return stats, abortBuild(s, batch, resultCh, fmt.Errorf("%s: %w", res.path, err))
		}
		stats.Files++
		stats.Definitions += len(res.defs)
		stats.References += len(res.refs)
		stats.CallEdges += len(res.calls)
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
	impactSourceVersion := store.ImpactSourceVersion
	if sourceIncomplete {
		impactSourceVersion = 0
	}
	if err := s.SetImpactSourceVersion(impactSourceVersion); err != nil {
		warn("failed to store impact source version: %v", err)
	}

	stats.Total = time.Since(start)
	return stats, nil
}

// ImpactBuild parses project source once and writes the immutable impact schema
// directly. It excludes dependency source before parsing and never creates a
// normal navigation index.
func ImpactBuild(output, projectRoot, commit, indexPath string, opts Options) (Stats, error) {
	var stats Stats
	started := time.Now()
	warn := opts.serialWarn()

	walkStarted := time.Now()
	projectFiles := opts.ProjectFiles
	if projectFiles == nil {
		projectFiles = parser.CollectImpactElixirFilesParallel(projectRoot)
	}
	files := statFilesParallel(projectFiles)
	stats.Walk = time.Since(walkStarted)

	builder, err := store.NewImpactSnapshotBuilder(output, commit, indexPath)
	if err != nil {
		return stats, err
	}
	config, _, err := evidence.LoadRepositoryConfig(projectRoot)
	if err != nil {
		builder.Abort()
		return stats, err
	}
	if len(opts.CompiledBuildRoots) > 0 {
		config.CompiledBuildRoots = opts.CompiledBuildRoots
	}
	if len(opts.CompiledApplications) > 0 {
		config.CompiledApplications = opts.CompiledApplications
	}
	builder.SetRepositoryEvidence(config)
	builder.SetProviderEvidence(opts.ProviderEvidence)
	resultCh, parseNanos, workers := parseProject(files, warn, true)
	stats.Workers = workers
	var writeNanos time.Duration
	for result := range resultCh {
		relative, err := filepath.Rel(projectRoot, result.path)
		if err != nil {
			builder.Abort()
			drain(resultCh)
			return stats, err
		}
		writeStarted := time.Now()
		err = builder.AddFile(relative, result.mtimeNano, result.defs, result.calls)
		writeNanos += time.Since(writeStarted)
		if err != nil {
			builder.Abort()
			drain(resultCh)
			return stats, fmt.Errorf("%s: %w", result.path, err)
		}
		if result.err != nil {
			builder.SetSourceEvidenceIncomplete()
		}
		stats.Files++
		stats.Definitions += len(result.defs)
		stats.References += len(result.refs)
		stats.CallEdges += len(result.calls)
	}
	stats.Parse = time.Duration(parseNanos.Load())
	stats.Write = writeNanos
	if err := AddCompiledEvidence(context.Background(), projectRoot, config, builder, warn); err != nil {
		builder.Abort()
		return stats, err
	}
	finalize, err := builder.Finish()
	if err != nil {
		return stats, err
	}
	stats.Finalize = finalize.Compact
	stats.Commit = finalize.Commit
	stats.CreateIndexes = finalize.CreateIndexes
	stats.Rename = finalize.Rename
	stats.Total = time.Since(started)
	return stats, nil
}

type compiledImplementor struct {
	module     string
	behaviours []string
	functions  map[parser.FunctionID]struct{}
}

// AddCompiledEvidence scans configured repository-built BEAM files and streams
// native deltas to a private snapshot. It never invokes a compiler.
func AddCompiledEvidence(ctx context.Context, projectRoot string, config evidence.RepositoryConfig,
	sink store.CompiledEvidenceSink, warn func(string, ...interface{}),
) error {
	if warn == nil {
		warn = func(string, ...interface{}) {}
	}
	callbacks := builtinCompiledCallbacks()
	for _, callback := range config.BehaviourCallbacks {
		callbacks[callback.Behaviour] = append(callbacks[callback.Behaviour], parser.FunctionID{
			Module: callback.Behaviour, Function: callback.Function, Arity: callback.Arity,
		})
	}
	var implementors []compiledImplementor
	compiledModules := make(map[string]string)
	compiledDigests := make(map[string][sha256.Size]byte)
	compiledComplete := len(config.CompiledBuildRoots) > 0
	for _, configuredRoot := range config.CompiledBuildRoots {
		buildRoot := filepath.FromSlash(configuredRoot)
		if !filepath.IsAbs(buildRoot) {
			buildRoot = filepath.Join(projectRoot, buildRoot)
		}
		paths, inventoryErr := declaredBeamPaths(buildRoot)
		if inventoryErr != nil || len(paths) == 0 {
			compiledComplete = false
			warn("compiled evidence unavailable at %s: %v", buildRoot, inventoryErr)
			continue
		}
		inventoryDigests := make(map[string][sha256.Size]byte, len(paths))
		for _, path := range paths {
			digest, err := fileSHA256(path)
			if err != nil {
				compiledComplete = false
				warn("compiled evidence unavailable at %s: %v", path, err)
				continue
			}
			inventoryDigests[path] = digest
		}
		err := beam.ScanCompiledEvidence(ctx, paths, runtime.NumCPU(), func(beamPath string, compiled beam.CompiledEvidence, readErr error) error {
			if readErr != nil {
				opaque, opaqueErr := beam.ReadOpaqueEvidence(beamPath)
				if opaqueErr != nil {
					compiledComplete = false
					module := strings.TrimSuffix(filepath.Base(beamPath), ".beam")
					module = strings.TrimPrefix(module, "Elixir.")
					return sink.AddCompiledOpaque(module, readErr.Error()+"; "+opaqueErr.Error())
				}
				compiled = opaque
			}
			if previous, duplicate := compiledModules[compiled.Module]; duplicate && previous != beamPath {
				return fmt.Errorf("duplicate compiled module %s: %s and %s", compiled.Module, previous, beamPath)
			}
			compiledModules[compiled.Module] = beamPath
			fileDigest, ok := inventoryDigests[beamPath]
			if !ok {
				compiledComplete = false
				return sink.AddCompiledOpaque(compiled.Module, "compiled file was unreadable during inventory hashing")
			}
			compiledDigests[compiled.Module] = fileDigest
			applyCompiledResourceFingerprints(projectRoot, &compiled)
			if len(compiled.Callbacks) > 0 {
				callbacks[compiled.Module] = append(callbacks[compiled.Module], compiled.Callbacks...)
			}
			if len(compiled.Behaviours) > 0 {
				functions := make(map[parser.FunctionID]struct{}, len(compiled.Functions))
				for _, function := range compiled.Functions {
					functions[function.Function] = struct{}{}
				}
				implementors = append(implementors, compiledImplementor{
					module: compiled.Module, behaviours: compiled.Behaviours, functions: functions,
				})
			}
			source := compiledSourcePath(beamPath, compiled, config.CompiledApplications)
			if err := ctx.Err(); err != nil {
				return err
			}
			return sink.AddCompiledEvidence(source, compiled)
		})
		if err != nil {
			return err
		}
		pathsAfter, err := declaredBeamPaths(buildRoot)
		if err != nil || !equalStrings(paths, pathsAfter) {
			return fmt.Errorf("compiled inventory changed while scanning %s", buildRoot)
		}
		for _, path := range paths {
			before, ok := inventoryDigests[path]
			if !ok {
				continue
			}
			digest, err := fileSHA256(path)
			if err != nil || digest != before {
				return fmt.Errorf("compiled file changed while scanning %s", path)
			}
		}
	}
	callbackEdges := make(map[parser.CallEdge]struct{})
	for _, edge := range builtinCallbackDispatches() {
		callbackEdges[edge] = struct{}{}
	}
	for _, implementor := range implementors {
		for _, behaviour := range implementor.behaviours {
			for _, callback := range callbacks[behaviour] {
				implementation := parser.FunctionID{Module: implementor.module, Function: callback.Function, Arity: callback.Arity}
				if _, ok := implementor.functions[implementation]; !ok {
					continue
				}
				callbackEdges[parser.CallEdge{
					Caller: parser.FunctionID{Module: "callback:" + behaviour, Function: callback.Function, Arity: callback.Arity},
					Callee: implementation, Kind: "callback",
				}] = struct{}{}
			}
		}
	}
	compiledCallbackEdges := make([]parser.CallEdge, 0, len(callbackEdges))
	for edge := range callbackEdges {
		compiledCallbackEdges = append(compiledCallbackEdges, edge)
	}
	if err := sink.AddCompiledEdges(compiledCallbackEdges); err != nil {
		return err
	}
	inventoryDigest := compiledEvidenceDigest(compiledDigests)
	if compiledComplete {
		sink.SetCompiledEvidenceComplete(inventoryDigest)
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func declaredBeamPaths(buildRoot string) ([]string, error) {
	appFiles, err := filepath.Glob(filepath.Join(buildRoot, "*", "ebin", "*.app"))
	if err != nil {
		return nil, err
	}
	if len(appFiles) == 0 {
		return nil, errors.New("no .app declarations")
	}
	seen := make(map[string]string)
	var paths []string
	for _, appFile := range appFiles {
		data, err := os.ReadFile(appFile)
		if err != nil {
			return nil, err
		}
		modules, err := parseAppModules(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", appFile, err)
		}
		for _, module := range modules {
			path := filepath.Join(filepath.Dir(appFile), module+".beam")
			if previous, ok := seen[module]; ok {
				return nil, fmt.Errorf("duplicate declared module %s: %s and %s", module, previous, path)
			}
			if _, err := os.Stat(path); err != nil {
				return nil, fmt.Errorf("declared module %s: %w", module, err)
			}
			seen[module] = path
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func parseAppModules(data []byte) ([]string, error) {
	text := string(data)
	marker := strings.Index(text, "{modules")
	if marker < 0 {
		return nil, errors.New("missing modules declaration")
	}
	start := strings.IndexByte(text[marker:], '[')
	if start < 0 {
		return nil, errors.New("invalid modules declaration")
	}
	start += marker + 1
	var modules []string
	for i := start; i < len(text); {
		for i < len(text) && (text[i] == ' ' || text[i] == '\t' || text[i] == '\r' || text[i] == '\n' || text[i] == ',') {
			i++
		}
		if i >= len(text) {
			return nil, errors.New("unterminated modules declaration")
		}
		if text[i] == ']' {
			return modules, nil
		}
		if text[i] == '%' {
			if end := strings.IndexByte(text[i:], '\n'); end >= 0 {
				i += end + 1
				continue
			}
			return nil, errors.New("unterminated modules declaration")
		}
		var module strings.Builder
		if text[i] == '\'' {
			i++
			closed := false
			for i < len(text) {
				if text[i] == '\\' && i+1 < len(text) {
					module.WriteByte(text[i+1])
					i += 2
					continue
				}
				if text[i] == '\'' {
					i++
					closed = true
					break
				}
				module.WriteByte(text[i])
				i++
			}
			if !closed {
				return nil, errors.New("unterminated quoted module")
			}
		} else {
			for i < len(text) && text[i] != ',' && text[i] != ']' && text[i] != ' ' && text[i] != '\t' && text[i] != '\r' && text[i] != '\n' {
				module.WriteByte(text[i])
				i++
			}
		}
		if module.Len() == 0 {
			return nil, errors.New("empty module atom")
		}
		modules = append(modules, module.String())
	}
	return nil, errors.New("unterminated modules declaration")
}

func fileSHA256(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return digest, err
	}
	defer func() { _ = file.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return digest, err
	}
	copy(digest[:], h.Sum(nil))
	return digest, nil
}

func compiledEvidenceDigest(digests map[string][sha256.Size]byte) string {
	modules := make([]string, 0, len(digests))
	for module := range digests {
		modules = append(modules, module)
	}
	sort.Strings(modules)
	h := sha256.New()
	_, _ = h.Write([]byte("dexter:compiled-inventory:v1\x00"))
	for _, module := range modules {
		_, _ = h.Write([]byte(module))
		_, _ = h.Write([]byte{0})
		digest := digests[module]
		_, _ = h.Write(digest[:])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func applyCompiledResourceFingerprints(projectRoot string, compiled *beam.CompiledEvidence) {
	if len(compiled.Resources) == 0 {
		return
	}
	metadata := parser.FunctionID{Module: compiled.Module, Function: "__module_metadata__", Arity: 0}
	for i := range compiled.Functions {
		if compiled.Functions[i].Function != metadata {
			continue
		}
		h := sha256.New()
		_, _ = h.Write([]byte("dexter:compiled-resources:v1\x00"))
		_, _ = h.Write(compiled.Functions[i].Fingerprint[:])
		for _, resource := range compiled.Resources {
			path := resource
			if !filepath.IsAbs(path) {
				path = filepath.Join(projectRoot, filepath.FromSlash(resource))
			}
			data, err := os.ReadFile(path)
			if err != nil {
				compiled.Unresolved = append(compiled.Unresolved, beam.CompiledUnresolved{
					Caller: metadata, Kind: "external_resource",
				})
				continue
			}
			_, _ = h.Write([]byte(filepath.ToSlash(resource)))
			resourceHash := sha256.Sum256(data)
			_, _ = h.Write(resourceHash[:])
		}
		h.Sum(compiled.Functions[i].Fingerprint[:0])
		return
	}
}

func builtinCompiledCallbacks() map[string][]parser.FunctionID {
	result := make(map[string][]parser.FunctionID)
	for _, module := range []string{"GenServer", "gen_server"} {
		for _, callback := range []struct {
			name  string
			arity int
		}{
			{"init", 1}, {"handle_call", 3}, {"handle_cast", 2}, {"handle_info", 2},
			{"handle_continue", 2}, {"terminate", 2}, {"code_change", 3},
		} {
			result[module] = append(result[module], parser.FunctionID{Module: module, Function: callback.name, Arity: callback.arity})
		}
	}
	return result
}

func builtinCallbackDispatches() []parser.CallEdge {
	type dispatch struct {
		module, function string
		arity            int
		callback         string
		callbackArity    int
	}
	dispatches := []dispatch{
		{"GenServer", "start", 3, "init", 1},
		{"GenServer", "start_link", 3, "init", 1},
		{"GenServer", "call", 2, "handle_call", 3},
		{"GenServer", "call", 3, "handle_call", 3},
		{"GenServer", "cast", 2, "handle_cast", 2},
		{"gen_server", "start", 3, "init", 1},
		{"gen_server", "start_link", 3, "init", 1},
		{"gen_server", "call", 2, "handle_call", 3},
		{"gen_server", "call", 3, "handle_call", 3},
		{"gen_server", "cast", 2, "handle_cast", 2},
	}
	edges := make([]parser.CallEdge, 0, len(dispatches))
	for _, dispatch := range dispatches {
		edges = append(edges, parser.CallEdge{
			Caller: parser.FunctionID{Module: dispatch.module, Function: dispatch.function, Arity: dispatch.arity},
			Callee: parser.FunctionID{Module: "callback:" + dispatch.module, Function: dispatch.callback, Arity: dispatch.callbackArity},
			Kind:   "callback_dispatch",
		})
	}
	return edges
}

func compiledSourcePath(beamPath string, compiled beam.CompiledEvidence, applications map[string]string) string {
	app := filepath.Base(filepath.Dir(filepath.Dir(beamPath)))
	if appRoot, ok := applications[app]; ok {
		source := filepath.ToSlash(compiled.Source)
		if source != "" && !filepath.IsAbs(source) && source != ".." && !strings.HasPrefix(source, "../") {
			return filepath.ToSlash(filepath.Join(appRoot, filepath.FromSlash(source)))
		}
	}
	return filepath.ToSlash(filepath.Join(".dexter-dependency", app, compiled.Module+".ex"))
}

func parseProject(files []fileEntry, warn func(string, ...interface{}), impactOnly bool) (<-chan parseResult, *atomic.Int64, int) {
	workers := runtime.NumCPU()
	fileCh := make(chan fileEntry, workers)
	resultCh := make(chan parseResult, 1024)
	parseNanos := &atomic.Int64{}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range fileCh {
				t0 := time.Now()
				var defs []parser.Definition
				var refs []parser.Reference
				var calls []parser.CallEdge
				var err error
				if impactOnly {
					defs, calls, err = parser.ParseFileForImpact(f.path)
				} else {
					defs, refs, calls, err = parser.ParseFileWithCalls(f.path)
				}
				if err != nil {
					warn("%s: %v", f.path, err)
					resultCh <- parseResult{path: f.path, mtimeNano: f.mtimeNano, err: err}
					continue
				}
				parseNanos.Add(int64(time.Since(t0)))
				resultCh <- parseResult{path: f.path, mtimeNano: f.mtimeNano, defs: defs, refs: refs, calls: calls}
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
	return resultCh, parseNanos, workers
}

type parseResult struct {
	path      string
	mtimeNano int64
	defs      []parser.Definition
	refs      []parser.Reference
	calls     []parser.CallEdge
	err       error
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
