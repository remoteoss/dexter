package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/remoteoss/dexter/internal/indexer"
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

	var stdlibRoot string
	if root, ok := stdlib.Resolve(s, "", projectRoot); ok {
		stdlibRoot = root
	}

	stats, err := indexer.FullBuild(s, projectRoot, indexer.Options{
		StdlibRoot: stdlibRoot,
		Warn: func(format string, args ...interface{}) {
			fmt.Fprintf(os.Stderr, "Warning: "+format+"\n", args...)
		},
	})
	if err != nil {
		fatal(err)
	}

	if profile {
		fmt.Fprintf(os.Stderr, "  walk: %s (%s files)\n", stats.Walk.Round(time.Millisecond), formatInt(stats.Files))
		fmt.Fprintf(os.Stderr, "  parse+write: parse %s across %d workers, write %s\n",
			stats.Parse.Round(time.Millisecond), stats.Workers, stats.Write.Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "  commit: %s\n", stats.Commit.Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "  create indices: %s\n", stats.CreateIndexes.Round(time.Millisecond))
	}

	fmt.Fprintf(os.Stderr, "Indexed %s files (%s definitions, %s references) in %s\n",
		formatInt(stats.Files), formatInt(stats.Definitions), formatInt(stats.References),
		stats.Total.Round(time.Millisecond))
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

	// A fresh store has version 0 but no stale data to discard. Let the live
	// server build that empty index in the background through indexer.FullBuild;
	// only a populated index from an older format needs the synchronous reset.
	if stored := s.GetIndexVersion(); stored != version.IndexVersion && !s.IsEmpty() {
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
