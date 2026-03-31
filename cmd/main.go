package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	dexter_lsp "gitlab.com/remote-com/employ-starbase/dexter/internal/lsp"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/store"
)

func usage() {
	fmt.Fprintf(os.Stderr, `dexter - Elixir module index

Usage:
  dexter init [--force] [path]    Full index of an Elixir project
  dexter reindex [file|path]      Re-index a single file or check all files for changes
  dexter lookup [flags] <module> [func]   Look up where a module/function is defined
    --strict               Exit 1 if exact match not found (no fallback)
    --no-follow-delegates  Don't follow defdelegate to the target module
  dexter lsp [path]               Start the LSP server (stdio)

Options:
  path defaults to the current directory
`)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "init":
		force := false
		pathIdx := -1
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "--force" {
				force = true
			} else {
				pathIdx = i
			}
		}
		var projectRoot string
		if pathIdx >= 0 {
			projectRoot = getPath(pathIdx)
		} else {
			projectRoot, _ = os.Getwd()
		}
		cmdInit(projectRoot, force)
	case "reindex":
		target := getPath(2)
		cmdReindex(target)
	case "lookup":
		strict := false
		followDelegates := true
		lookupArgs := []string{}
		for _, arg := range os.Args[2:] {
			switch {
			case arg == "--strict":
				strict = true
			case arg == "--no-follow-delegates":
				followDelegates = false
			case strings.HasPrefix(arg, "--"):
				fmt.Fprintf(os.Stderr, "Unknown option: %s\n", arg)
				os.Exit(1)
			default:
				lookupArgs = append(lookupArgs, arg)
			}
		}
		if len(lookupArgs) < 1 {
			usage()
		}
		module := lookupArgs[0]
		function := ""
		if len(lookupArgs) >= 2 {
			function = lookupArgs[1]
		}
		projectRoot, _ := os.Getwd()
		cmdLookup(projectRoot, module, function, strict, followDelegates)
	case "lsp":
		projectRoot, _ := os.Getwd()
		cmdLSP(projectRoot)
	default:
		usage()
	}
}

func getPath(argIndex int) string {
	if len(os.Args) > argIndex {
		p, err := filepath.Abs(os.Args[argIndex])
		if err != nil {
			fatal(err)
		}
		return p
	}
	p, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	return p
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
			os.Remove(f) // ignore errors — files may not exist
		}
	}

	s, err := store.Open(projectRoot)
	if err != nil {
		fatal(err)
	}
	defer s.Close()

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
	defer s.Close()

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
	defer s.Close()

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
	defer s.Close()

	log.SetOutput(os.Stderr)
	log.Printf("Dexter LSP starting (root: %s)", projectRoot)

	if err := dexter_lsp.Serve(os.Stdin, os.Stdout, s, projectRoot); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}
