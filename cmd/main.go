package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/spf13/cobra"
	dexter_lsp "gitlab.com/remote-com/employ-starbase/dexter/internal/lsp"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/store"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/version"
)

func main() {
	rootCmd := &cobra.Command{
		Use:          "dexter",
		Short:        "Elixir module index",
		SilenceUsage: true,
	}

	var force bool
	initCmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Full index of an Elixir project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectRoot, err := resolvePath(args, 0)
			if err != nil {
				return err
			}
			cmdInit(projectRoot, force)
			return nil
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "Delete and rebuild index from scratch")

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

	rootCmd.AddCommand(initCmd, reindexCmd, lookupCmd, lspCmd, versionCmd)

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

	for _, marker := range []string{".dexter.db", ".git", "mix.exs"} {
		dir := path
		for {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return path
}

func cmdInit(projectRoot string, force bool) {
	dbPath := filepath.Join(projectRoot, ".dexter.db")
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

	start := time.Now()

	// Phase 1: collect file paths
	var paths []string
	err = parser.WalkElixirFiles(projectRoot, func(path string, info os.FileInfo) error {
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		fatal(err)
	}

	// Phase 2: parse in parallel
	type parseResult struct {
		path string
		defs []parser.Definition
	}

	workers := runtime.NumCPU()
	pathCh := make(chan string, workers)
	resultCh := make(chan parseResult, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range pathCh {
				defs, err := parser.ParseFile(path)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", path, err)
					continue
				}
				resultCh <- parseResult{path: path, defs: defs}
			}
		}()
	}

	go func() {
		for _, p := range paths {
			pathCh <- p
		}
		close(pathCh)
		wg.Wait()
		close(resultCh)
	}()

	// Phase 3: batch write to SQLite (single writer)
	count := 0
	defCount := 0
	for res := range resultCh {
		if err := s.IndexFile(res.path, res.defs); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", res.path, err)
			continue
		}
		count++
		defCount += len(res.defs)
	}

	if err := s.SetIndexVersion(version.IndexVersion); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to store index version: %v\n", err)
	}

	fmt.Fprintf(os.Stderr, "Indexed %d files (%d definitions) in %s\n", count, defCount, time.Since(start).Round(time.Millisecond))
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
		cmdInit(projectRoot, true)
		return
	}

	if !info.IsDir() {
		reindexFile(s, target)
		return
	}

	start := time.Now()
	reindexed := 0
	skipped := 0

	err = parser.WalkElixirFiles(target, func(path string, fi os.FileInfo) error {
		storedMtime, found := s.GetFileMtime(path)
		currentMtime := fi.ModTime().UnixNano()
		if found && storedMtime == currentMtime {
			skipped++
			return nil
		}

		reindexFile(s, path)
		reindexed++
		return nil
	})
	if err != nil {
		fatal(err)
	}

	fmt.Fprintf(os.Stderr, "Reindexed %d files, %d unchanged (%s)\n", reindexed, skipped, time.Since(start).Round(time.Millisecond))
}

func reindexFile(s *store.Store, path string) {
	defs, err := parser.ParseFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %s: %v\n", path, err)
		return
	}
	if err := s.IndexFile(path, defs); err != nil {
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

func cmdLSP(projectRoot string) {
	projectRoot = findProjectRoot(projectRoot)

	s, err := store.Open(projectRoot)
	if err != nil {
		fatal(err)
	}

	if stored := s.GetIndexVersion(); stored != version.IndexVersion {
		log.SetOutput(os.Stderr)
		log.Printf("Index version mismatch (stored: %d, current: %d), rebuilding index...", stored, version.IndexVersion)
		if err := s.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to close store: %v\n", err)
		}
		cmdInit(projectRoot, true)
		s, err = store.Open(projectRoot)
		if err != nil {
			fatal(err)
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

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
