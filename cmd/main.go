package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	dexter_lsp "github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/stdlib"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:          "dexter",
		Short:        "A lightning-fast Elixir LSP ⚡",
		SilenceUsage: true,
	}

	var force bool
	var profile bool
	initCmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Full index of an Elixir project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectRoot, err := resolvePath(args, 0)
			if err != nil {
				return err
			}
			cmdInit(projectRoot, force, profile)
			return nil
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "Delete and rebuild index from scratch")
	initCmd.Flags().BoolVar(&profile, "profile", false, "Print timing breakdown for each phase")

	reindexCmd := &cobra.Command{
		Use:   "reindex [file|path]",
		Short: "Re-index a single file or check all files for changes",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolvePath(args, 0)
			if err != nil {
				return err
			}
			cmdReindex(target)
			return nil
		},
	}

	var strict bool
	var noFollowDelegates bool
	lookupCmd := &cobra.Command{
		Use:   "lookup <module> [func]",
		Short: "Look up where a module/function is defined",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			module := args[0]
			function := ""
			if len(args) == 2 {
				function = args[1]
			}
			projectRoot, err := os.Getwd()
			if err != nil {
				return err
			}
			cmdLookup(projectRoot, module, function, strict, !noFollowDelegates)
			return nil
		},
	}
	lookupCmd.Flags().BoolVar(&strict, "strict", false, "Exit 1 if exact match not found (no fallback)")
	lookupCmd.Flags().BoolVar(&noFollowDelegates, "no-follow-delegates", false, "Don't follow defdelegate to the target module")

	referencesCmd := &cobra.Command{
		Use:   "references <module> [func]",
		Short: "Find references to a module/function",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			module := args[0]
			function := ""
			if len(args) == 2 {
				function = args[1]
			}
			projectRoot, err := os.Getwd()
			if err != nil {
				return err
			}
			cmdReferences(projectRoot, module, function)
			return nil
		},
	}

	lspCmd := &cobra.Command{
		Use:   "lsp [path]",
		Short: "Start the LSP server (stdio)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectRoot, err := resolvePath(args, 0)
			if err != nil {
				return err
			}
			cmdLSP(projectRoot)
			return nil
		},
	}

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(version.Version)
		},
	}

	rootCmd.AddCommand(initCmd, reindexCmd, lookupCmd, referencesCmd, lspCmd, versionCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// resolvePath returns the absolute path for args[index], or the current working
// directory if args doesn't have an entry at that index.
func resolvePath(args []string, index int) (string, error) {
	if index < len(args) {
		return filepath.Abs(args[index])
	}
	return os.Getwd()
}

func findProjectRoot(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		fatal(err)
	}
	if !info.IsDir() {
		path = filepath.Dir(path)
	}
	return store.FindProjectRoot(path, "mix.exs")
}

func cmdInit(projectRoot string, force bool, profile bool) {
	dbPath := store.DBPath(projectRoot)
	if _, err := os.Stat(dbPath); err == nil {
		if !force {
			fmt.Fprintf(os.Stderr, "Index already exists at %s\n", dbPath)
			fmt.Fprintf(os.Stderr, "Run `dexter reindex` to update, or `dexter init --force` to delete and rebuild from scratch.\n")
			os.Exit(1)
		}
		for _, f := range []string{dbPath, dbPath + "-shm", dbPath + "-wal"} {
			_ = os.Remove(f) // files may not exist
		}
	}

	s, err := store.Open(projectRoot)
	if err != nil {
		fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close store: %v\n", err)
		}
	}()

	if err := s.SetBulkPragmas(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: bulk pragma setup: %v\n", err)
	}

	prof := &profiler{enabled: profile}
	start := time.Now()

	// Phase 1: collect file paths and mtimes
	type fileEntry struct {
		path      string
		mtimeNano int64
	}
	var files []fileEntry
	var stdlibFiles []fileEntry
	err = parser.WalkElixirFiles(projectRoot, func(path string, d fs.DirEntry) error {
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, fileEntry{path: path, mtimeNano: info.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		fatal(err)
	}
	var stdlibRoot string
	if root, ok := stdlib.Resolve(s, "", projectRoot); ok {
		stdlibRoot = root
		_ = parser.WalkElixirFiles(stdlibRoot, func(path string, d fs.DirEntry) error {
			info, err := d.Info()
			if err != nil {
				return nil
			}
			stdlibFiles = append(stdlibFiles, fileEntry{path: path, mtimeNano: info.ModTime().UnixNano()})
			return nil
		})
	}
	prof.log("  walk: %s (%s files)\n", prof.since(start).Round(time.Millisecond), formatInt(len(files)+len(stdlibFiles)))

	// Parse stdlib files in parallel (definitions only — refs are not indexed for stdlib).
	type stdlibResult struct {
		path      string
		mtimeNano int64
		defs      []parser.Definition
	}
	stdlibCh := make(chan stdlibResult, len(stdlibFiles))
	var stdlibWg sync.WaitGroup
	stdlibWorkers := runtime.NumCPU()
	stdlibFileCh := make(chan fileEntry, stdlibWorkers)
	for i := 0; i < stdlibWorkers; i++ {
		stdlibWg.Add(1)
		go func() {
			defer stdlibWg.Done()
			for f := range stdlibFileCh {
				defs, _, err := parser.ParseFile(f.path)
				if err != nil {
					continue
				}
				stdlibCh <- stdlibResult{f.path, f.mtimeNano, defs}
			}
		}()
	}
	go func() {
		for _, f := range stdlibFiles {
			stdlibFileCh <- f
		}
		close(stdlibFileCh)
		stdlibWg.Wait()
		close(stdlibCh)
	}()
	var stdlibResults []stdlibResult
	for r := range stdlibCh {
		stdlibResults = append(stdlibResults, r)
	}
	prof.log("  stdlib parse: %s files\n", formatInt(len(stdlibFiles)))

	// Phase 2: parse user files in parallel
	type parseResult struct {
		path      string
		mtimeNano int64
		defs      []parser.Definition
		refs      []parser.Reference
	}

	workers := runtime.NumCPU()
	fileCh := make(chan fileEntry, workers)
	resultCh := make(chan parseResult, 1024)

	var parseNanos atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range fileCh {
				t0 := prof.now()
				defs, refs, err := parser.ParseFile(f.path)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", f.path, err)
					continue
				}
				parseNanos.Add(int64(prof.since(t0)))
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

	// Phase 3: bulk insert to SQLite (single writer, single transaction, no indexes)
	pipelineStart := prof.now()
	if err := s.DropIndexes(); err != nil {
		fatal(err)
	}
	batch, err := s.BeginBulkInsert()
	if err != nil {
		fatal(err)
	}

	// Insert pre-parsed stdlib definitions (no refs)
	for _, sr := range stdlibResults {
		if err := batch.IndexFileWithMtimeAndRefs(sr.path, sr.mtimeNano, sr.defs, nil); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", sr.path, err)
		}
	}

	// Insert user file results as they stream in (store filters stdlib refs)
	count := len(stdlibResults)
	defCount := 0
	for _, sr := range stdlibResults {
		defCount += len(sr.defs)
	}
	refCount := 0
	var writeNanos int64
	for res := range resultCh {
		t0 := prof.now()
		if err := batch.IndexFileWithMtimeAndRefs(res.path, res.mtimeNano, res.defs, res.refs); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", res.path, err)
			continue
		}
		writeNanos += int64(prof.since(t0))
		count++
		defCount += len(res.defs)
		refCount += len(res.refs)
	}
	prof.log("  parse+write: %s (parse: %s across %d workers, write: %s)\n",
		prof.since(pipelineStart).Round(time.Millisecond),
		time.Duration(parseNanos.Load()).Round(time.Millisecond),
		workers,
		time.Duration(writeNanos).Round(time.Millisecond))

	commitStart := prof.now()
	if err := batch.Commit(); err != nil {
		fatal(err)
	}
	prof.log("  commit: %s\n", prof.since(commitStart).Round(time.Millisecond))

	indexStart := prof.now()
	if err := s.CreateIndexes(); err != nil {
		fatal(err)
	}
	prof.log("  create indices: %s\n", prof.since(indexStart).Round(time.Millisecond))

	if err := s.SetIndexVersion(version.IndexVersion); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to store index version: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "Indexed %s files (%s definitions, %s references) in %s\n", formatInt(count), formatInt(defCount), formatInt(refCount), time.Since(start).Round(time.Millisecond))
}

func cmdReindex(target string) {
	info, err := os.Stat(target)
	if err != nil {
		fatal(err)
	}

	projectRoot := findProjectRoot(target)
	s, err := store.Open(projectRoot)
	if err != nil {
		fatal(err)
	}
	storeClosed := false
	closeStore := func() {
		if !storeClosed {
			storeClosed = true
			if err := s.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to close store: %v\n", err)
			}
		}
	}
	defer closeStore()

	if stored := s.GetIndexVersion(); stored != version.IndexVersion {
		fmt.Fprintf(os.Stderr, "Index version mismatch (stored: %d, current: %d), performing full rebuild...\n", stored, version.IndexVersion)
		closeStore()
		cmdInit(projectRoot, true, false)
		return
	}

	if !info.IsDir() {
		reindexFile(s, target)
		return
	}

	start := time.Now()
	reindexed := 0
	skipped := 0

	walkFn := func(path string, d fs.DirEntry) error {
		info, err := d.Info()
		if err != nil {
			return nil
		}
		storedMtime, found := s.GetFileMtime(path)
		currentMtime := info.ModTime().UnixNano()
		if found && storedMtime == currentMtime {
			skipped++
			return nil
		}

		reindexFile(s, path)
		reindexed++
		return nil
	}

	err = parser.WalkElixirFiles(target, walkFn)
	if err != nil {
		fatal(err)
	}

	fmt.Fprintf(os.Stderr, "Reindexed %d files, %d unchanged (%s)\n", reindexed, skipped, time.Since(start).Round(time.Millisecond))
}

func reindexFile(s *store.Store, path string) {
	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", path, err)
		return
	}
	if err := s.IndexFileWithRefs(path, defs, refs); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", path, err)
	}
}

func cmdLookup(projectRoot string, module string, function string, strict bool, followDelegates bool) {
	projectRoot = findProjectRoot(projectRoot)
	s, err := store.Open(projectRoot)
	if err != nil {
		fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close store: %v\n", err)
		}
	}()

	if function != "" {
		var results []store.LookupResult
		if followDelegates {
			results, err = s.LookupFollowDelegate(module, function)
		} else {
			results, err = s.LookupFunction(module, function)
		}
		if err != nil {
			fatal(err)
		}
		if len(results) > 0 {
			for _, r := range results {
				fmt.Printf("%s:%d\n", r.FilePath, r.Line)
			}
			return
		}
		if strict {
			os.Exit(1)
		}
	}

	// Fall back to module lookup (or if no function specified)
	results, err := s.LookupModule(module)
	if err != nil {
		fatal(err)
	}
	if len(results) == 0 && strict {
		os.Exit(1)
	}
	for _, r := range results {
		fmt.Printf("%s:%d\n", r.FilePath, r.Line)
	}
}

func cmdReferences(projectRoot string, module string, function string) {
	projectRoot = findProjectRoot(projectRoot)
	s, err := store.Open(projectRoot)
	if err != nil {
		fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close store: %v\n", err)
		}
	}()

	results, err := s.LookupReferences(module, function)
	if err != nil {
		fatal(err)
	}
	if len(results) == 0 {
		fmt.Fprintf(os.Stderr, "No references found for %s", module)
		if function != "" {
			fmt.Fprintf(os.Stderr, ".%s", function)
		}
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}
	for _, r := range results {
		fmt.Printf("%s:%d\n", r.FilePath, r.Line)
	}
}

func cmdLSP(projectRoot string) {
	projectRoot = findProjectRoot(projectRoot)

	const maxOpenAttempts = 3
	var s *store.Store
	for attempt := 1; ; attempt++ {
		var err error
		s, err = store.Open(projectRoot)
		if err == nil {
			break
		}
		if attempt >= maxOpenAttempts {
			fatal(fmt.Errorf("failed to open index after %d attempts: %w. Try `dexter init --force` in your project root and then restart your editor/the LSP", maxOpenAttempts, err))
		}
		// DB may be corrupted (e.g. ctrl-c, process killed, power loss during a previous init).
		// cmdInit with force=true deletes and rebuilds from scratch.
		log.SetOutput(os.Stderr)
		log.Printf("Failed to open index (attempt %d/%d: %v), rebuilding from scratch...", attempt, maxOpenAttempts, err)
		cmdInit(projectRoot, true, false)
	}

	if stored := s.GetIndexVersion(); stored != version.IndexVersion {
		log.SetOutput(os.Stderr)
		log.Printf("Index version mismatch (stored: %d, current: %d), rebuilding index...", stored, version.IndexVersion)
		if err := s.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close store: %v\n", err)
		}
		cmdInit(projectRoot, true, false)
		var openErr error
		s, openErr = store.Open(projectRoot)
		if openErr != nil {
			fatal(openErr)
		}
	}
	defer func() {
		if err := s.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close store: %v\n", err)
		}
	}()

	log.SetOutput(os.Stderr)
	log.Printf("Dexter LSP v%s starting (root: %s)", version.Version, projectRoot)

	if err := dexter_lsp.Serve(os.Stdin, os.Stdout, s, projectRoot); err != nil {
		fatal(err)
	}
}

func formatInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(ch))
	}
	return string(result)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

type profiler struct {
	enabled bool
}

func (p *profiler) now() time.Time {
	if !p.enabled {
		return time.Time{}
	}
	return time.Now()
}

func (p *profiler) since(start time.Time) time.Duration {
	if !p.enabled {
		return 0
	}
	return time.Since(start)
}

func (p *profiler) log(format string, args ...interface{}) {
	if p.enabled {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}
