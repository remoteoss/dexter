package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/remoteoss/dexter/internal/daemon"
	"github.com/remoteoss/dexter/internal/indexer"
	"github.com/remoteoss/dexter/internal/stdlib"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
	"github.com/spf13/cobra"
)

func main() {
	var rootDir string
	rootCmd := &cobra.Command{
		Use:          "dexter",
		Short:        "A lightning-fast Elixir LSP ⚡",
		SilenceUsage: true,
		// --root applies to every subcommand, so a frontend can name the
		// workspace it means instead of depending on where it happens to run.
		// That is what lets a lookup or an editor attach to a project from
		// anywhere, including from a different checkout's subdirectory.
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return chdirToRoot(rootDir)
		},
	}
	rootCmd.PersistentFlags().StringVarP(&rootDir, "root", "C", "",
		"Run as if dexter was started in this directory (default: current directory)")

	var force bool
	var allowNonProject bool
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
			cmdInit(projectRoot, force, allowNonProject, profile)
			return nil
		},
	}
	initCmd.Flags().BoolVar(&force, "force", false, "Delete and rebuild index from scratch")
	initCmd.Flags().BoolVarP(&allowNonProject, "yes", "y", false, "Index even though this directory is not an Elixir project")
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
			cmdReindex(target, allowNonProject)
			return nil
		},
	}
	reindexCmd.Flags().BoolVarP(&allowNonProject, "yes", "y", false, "Operate even though this directory is not an Elixir project")

	var strict bool
	var noFollowDelegates bool
	var lookupOpts queryOptions
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
			cmdLookup(projectRoot, module, function, strict, !noFollowDelegates, lookupOpts, allowNonProject)
			return nil
		},
	}
	lookupCmd.Flags().BoolVar(&strict, "strict", false, "Exit 1 if exact match not found (no fallback)")
	lookupCmd.Flags().BoolVar(&noFollowDelegates, "no-follow-delegates", false, "Don't follow defdelegate to the target module")
	lookupCmd.Flags().DurationVar(&lookupOpts.wait, "wait", 0, "Wait up to this long for a cold index before answering (0 answers immediately)")
	lookupCmd.Flags().BoolVarP(&lookupOpts.quiet, "quiet", "q", false, "Suppress the note when the index is still building")
	lookupCmd.Flags().BoolVarP(&allowNonProject, "yes", "y", false, "Query even though this directory is not an Elixir project")

	var referencesOpts queryOptions
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
			cmdReferences(projectRoot, module, function, referencesOpts, allowNonProject)
			return nil
		},
	}
	referencesCmd.Flags().DurationVar(&referencesOpts.wait, "wait", 0, "Wait up to this long for a cold index before answering (0 answers immediately)")
	referencesCmd.Flags().BoolVarP(&referencesOpts.quiet, "quiet", "q", false, "Suppress the note when the index is still building")
	referencesCmd.Flags().BoolVarP(&allowNonProject, "yes", "y", false, "Query even though this directory is not an Elixir project")

	var forceStop bool
	stopCmd := &cobra.Command{
		Use:   "stop [path]",
		Short: "Stop the workspace daemon",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolvePath(args, 0)
			if err != nil {
				return err
			}
			cmdStop(target, forceStop)
			return nil
		},
	}
	stopCmd.Flags().BoolVar(&forceStop, "force", false, "Stop even while editor or CLI clients are attached")

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

	var daemonIdleTimeout time.Duration
	daemonCmd := &cobra.Command{
		Use:    "daemon <path>",
		Short:  "Run the internal per-workspace daemon",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectRoot, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			log.SetOutput(os.Stderr)
			err = daemon.Run(ctx, projectRoot, daemonIdleTimeout)
			if errors.Is(err, daemon.ErrWorkspaceOwned) {
				return nil
			}
			return err
		},
	}
	daemonCmd.Flags().DurationVar(&daemonIdleTimeout, "idle-timeout", defaultIdleTimeout(), "Exit after this long without clients (0 never exits)")

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(version.Version)
		},
	}

	rootCmd.AddCommand(initCmd, reindexCmd, lookupCmd, referencesCmd, stopCmd, lspCmd, daemonCmd, versionCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// chdirToRoot applies the --root flag: the command runs as if it had been
// started in that directory, so relative paths resolve from there and the
// workspace keeps the caller's spelling. $PWD is updated as well, because
// os.Getwd prefers it when it points at the process's directory; the spelled
// path is what the daemon indexes, and stored paths have to match the URIs an
// editor sends for the same workspace.
func chdirToRoot(root string) error {
	if root == "" {
		return nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("--root %s: %w", root, err)
	}
	if err := os.Chdir(abs); err != nil {
		return fmt.Errorf("--root %s: %w", root, err)
	}
	if err := os.Setenv("PWD", abs); err != nil {
		return fmt.Errorf("--root %s: %w", root, err)
	}
	return nil
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
	return findProjectRootWithMissing(path, false)
}

// findProjectRootWithMissing is findProjectRoot for a target that may have been
// deleted. The search starts from the nearest ancestor that still exists: the
// missing path itself can hold no marker, and returning it as the root would
// name a workspace that is not there.
func findProjectRootWithMissing(path string, allowMissing bool) string {
	info, err := os.Stat(path)
	for allowMissing && os.IsNotExist(err) {
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
		info, err = os.Stat(path)
	}
	if err != nil {
		fatal(err)
	}
	if !info.IsDir() {
		path = filepath.Dir(path)
	}
	root := store.FindProjectRoot(path, "mix.exs")
	if home, homeErr := os.UserHomeDir(); homeErr == nil && sameDir(root, home) {
		if mixRoot := findMarkerBefore(path, "mix.exs", home); mixRoot != "" {
			return mixRoot
		}
	}
	return root
}

func findMarkerBefore(path, marker, stop string) string {
	for dir := path; !sameDir(dir, stop); dir = filepath.Dir(dir) {
		if info, err := os.Stat(filepath.Join(dir, marker)); err == nil && info.Mode().IsRegular() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
	}
	return ""
}

// projectMarkers are the cheap signals that a directory is, or carries, a
// Dexter workspace. They match what store.FindProjectRoot trusts, and an
// a Dexter marker means an actual database, not an empty directory left by an
// interrupted operation.
func looksLikeProjectRoot(dir string) bool {
	return regularFile(filepath.Join(dir, "mix.exs")) ||
		gitMarker(filepath.Join(dir, ".git")) ||
		regularFile(store.DBPath(dir)) ||
		regularFile(store.LegacyDBPath(dir))
}

func hasDexterMarker(dir string) bool {
	return regularFile(store.DBPath(dir)) || regularFile(store.LegacyDBPath(dir))
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func gitMarker(path string) bool {
	info, err := os.Stat(path)
	return err == nil && (info.IsDir() || info.Mode().IsRegular())
}

// requireProjectRoot refuses to treat a directory that shows no sign of being
// an Elixir project as a workspace. A mistyped directory is far more likely
// than the intent to index one: a `dexter lookup` in the home directory would
// otherwise spend minutes of CPU building a database over every file the user
// owns, and would do it silently. -y/--yes is the way through, and it is
// needed only once because the database it creates is itself a marker.
func requireProjectRoot(dir string, allowNonProject bool) {
	if allowNonProject {
		return
	}
	if home, err := os.UserHomeDir(); err == nil && sameDir(dir, home) {
		if hasDexterMarker(dir) {
			return
		}
		fatal(fmt.Errorf("refusing to use %s as a workspace: it is your home directory, not a project\nhint: run from a project, pass --root <path>, or pass -y/--yes if you really mean it", dir))
	}
	if looksLikeProjectRoot(dir) {
		return
	}
	fatal(fmt.Errorf("refusing to use %s as a workspace: no mix.exs, .git, or Dexter database found, so it does not look like an Elixir project\nhint: run from a project, pass --root <path>, or pass -y/--yes to index it anyway", dir))
}

// warnProjectRoot is the LSP's version of the same check. An editor, unlike a
// shell command, is authoritative about what the user opened, and refusing to
// start would leave them with no language server and only a log line to
// explain it — so this warns loudly and serves the directory anyway. The warning
// is written to stderr, which every LSP client keeps in its server log.
func warnProjectRoot(dir string) {
	if home, err := os.UserHomeDir(); err == nil && sameDir(dir, home) {
		if hasDexterMarker(dir) {
			return
		}
		log.Printf("Warning: %s is your home directory, not a project; indexing it because the editor asked. Set --root <path> in the editor's dexter command if that is wrong.", dir)
		return
	}
	if looksLikeProjectRoot(dir) {
		return
	}
	log.Printf("Warning: %s does not look like an Elixir project (no mix.exs, .git, or Dexter database); indexing it because the editor asked. Set --root <path> if that is the wrong directory.", dir)
}

// sameDir reports whether two paths name the same directory. Stat is the
// authority so a symlinked spelling (or a case-insensitive filesystem) cannot
// sneak a home directory past the check.
func sameDir(a, b string) bool {
	ai, aErr := os.Stat(a)
	bi, bErr := os.Stat(b)
	if aErr != nil || bErr != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return os.SameFile(ai, bi)
}

// defaultIdleTimeout resolves the daemon idle timeout. DEXTER_DAEMON_IDLE_TIMEOUT
// overrides the built-in default for every daemon this machine spawns, including
// ones an editor starts, so it can be set once in a shell profile.
func defaultIdleTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("DEXTER_DAEMON_IDLE_TIMEOUT"))
	if raw == "" {
		return daemon.DefaultIdleTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		fmt.Fprintf(os.Stderr, "Warning: ignoring invalid DEXTER_DAEMON_IDLE_TIMEOUT %q\n", raw)
		return daemon.DefaultIdleTimeout
	}
	return d
}

// envFlag reports whether a boolean environment variable holds a truthy value.
func envFlag(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func cmdInit(projectRoot string, force bool, allowNonProject bool, profile bool) {
	requireProjectRoot(projectRoot, allowNonProject)
	dbPath := store.DBPath(projectRoot)
	if _, err := os.Stat(dbPath); err == nil && !force {
		fmt.Fprintf(os.Stderr, "Index already exists at %s\n", dbPath)
		fmt.Fprintf(os.Stderr, "Run `dexter reindex` to update, or `dexter init --force` to delete and rebuild from scratch.\n")
		os.Exit(1)
	}
	maintenanceCtx, cancelMaintenance := context.WithTimeout(context.Background(), controlCallTimeout)
	defer cancelMaintenance()
	ownership, _, err := daemon.AcquireMaintenance(maintenanceCtx, projectRoot)
	if err != nil {
		if errors.Is(err, daemon.ErrWorkspaceOwned) {
			fatal(errors.New("the Dexter daemon owns this workspace; close attached editors and retry init"))
		}
		fatal(err)
	}
	defer func() {
		if err := ownership.Release(); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to release workspace ownership: %v\n", err)
		}
	}()

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

func cmdReindex(target string, allowNonProject bool) {
	projectRoot := findProjectRootWithMissing(target, true)
	requireProjectRoot(projectRoot, allowNonProject)
	client, err := daemon.Ensure(context.Background(), projectRoot)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()
	var result daemon.ReindexResult
	callCtx, cancel := context.WithTimeout(context.Background(), controlCallTimeout)
	defer cancel()
	if err := client.Call(callCtx, daemon.MethodReindex, daemon.ReindexParams{Target: target}, &result); err != nil {
		fatal(err)
	}
	switch {
	case result.Missing:
		fmt.Fprintf(os.Stderr, "Nothing to reindex at %s: it does not exist and nothing is indexed there\n", target)
	case target == projectRoot:
		fmt.Fprintf(os.Stderr, "Reindexed workspace (%s)\n", result.Elapsed)
	default:
		fmt.Fprintf(os.Stderr, "Reindexed %s (%s)\n", target, result.Elapsed)
	}
}

func cmdLookup(projectRoot string, module string, function string, strict bool, followDelegates bool, opts queryOptions, allowNonProject bool) {
	projectRoot = findProjectRoot(projectRoot)
	requireProjectRoot(projectRoot, allowNonProject)
	client, err := daemon.Ensure(context.Background(), projectRoot)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()
	var result daemon.LookupResult
	callCtx, cancel := context.WithTimeout(context.Background(), controlCallTimeout)
	defer cancel()
	if err := client.Call(callCtx, daemon.MethodLookup, daemon.LookupParams{
		Module: module, Function: function, FollowDelegates: followDelegates, Strict: strict,
		WaitReadyMs: opts.waitReadyMs(),
	}, &result); err != nil {
		fatal(err)
	}
	if len(result.Locations) == 0 {
		warnIfIndexBuilding(result.Ready, opts)
		if strict {
			os.Exit(1)
		}
	}
	for _, r := range result.Locations {
		fmt.Printf("%s:%d\n", r.FilePath, r.Line)
	}
}

func cmdReferences(projectRoot string, module string, function string, opts queryOptions, allowNonProject bool) {
	projectRoot = findProjectRoot(projectRoot)
	requireProjectRoot(projectRoot, allowNonProject)
	client, err := daemon.Ensure(context.Background(), projectRoot)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()
	var result daemon.ReferencesResult
	callCtx, cancel := context.WithTimeout(context.Background(), controlCallTimeout)
	defer cancel()
	if err := client.Call(callCtx, daemon.MethodReferences, daemon.ReferencesParams{
		Module: module, Function: function, WaitReadyMs: opts.waitReadyMs(),
	}, &result); err != nil {
		fatal(err)
	}
	if len(result.Locations) == 0 {
		warnIfIndexBuilding(result.Ready, opts)
		fmt.Fprintf(os.Stderr, "No references found for %s", module)
		if function != "" {
			fmt.Fprintf(os.Stderr, ".%s", function)
		}
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}
	for _, r := range result.Locations {
		fmt.Printf("%s:%d\n", r.FilePath, r.Line)
	}
}

// cmdStop asks the workspace daemon to exit. The workspace is shared, so an
// editor or CLI client normally holds it alive; --force is the manual escape
// hatch for a daemon that is stuck, misbehaving, or simply in the way. It is
// deliberately idempotent: stopping a workspace with no daemon is success so a
// script can call it without first checking.
func cmdStop(projectRoot string, force bool) {
	projectRoot = findProjectRoot(projectRoot)
	ctx := context.Background()

	// --force is the manual escape hatch: terminate by process, with no
	// handshake, so it also works for a wedged daemon or one built by another
	// version. Without it, the control protocol asks politely and refuses while
	// other clients are attached.
	if force {
		if handled, err := daemon.StopForced(ctx, projectRoot); handled {
			if err != nil {
				fatal(err)
			}
			fmt.Fprintf(os.Stderr, "Stopped workspace daemon\n")
			return
		}
	}

	client, err := daemon.Dial(ctx, projectRoot)
	if err != nil {
		var incompatible *daemon.IncompatibleDaemonError
		if errors.As(err, &incompatible) {
			cmdStopIncompatible(ctx, projectRoot, incompatible, force)
			return
		}
		var mismatch *daemon.RootMismatchError
		if errors.As(err, &mismatch) {
			if force {
				handled, stopErr := daemon.StopForced(ctx, mismatch.Daemon)
				if stopErr != nil {
					fatal(stopErr)
				}
				if handled {
					fmt.Fprintf(os.Stderr, "Stopped workspace daemon\n")
					return
				}
			}
			fatal(mismatch)
		}
		if pid, ok := daemon.FindDaemonProcess(projectRoot); ok {
			fatal(fmt.Errorf("workspace daemon (pid %d) is not answering; use `dexter stop --force` to terminate it", pid))
		}
		fmt.Fprintf(os.Stderr, "No workspace daemon is running for %s\n", projectRoot)
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, controlCallTimeout)
	defer cancel()
	status, err := client.DaemonStatus(callCtx)
	if err != nil {
		_ = client.Close()
		fatal(err)
	}
	pid := status.PID

	callErr := client.Call(callCtx, daemon.MethodShutdown, daemon.ShutdownParams{Force: force}, nil)
	_ = client.Close()
	if callErr != nil && !force {
		fatal(fmt.Errorf("%w\nhint: `dexter stop --force` stops it anyway; attached editors lose the workspace until it restarts", callErr))
	}
	if callErr != nil {
		fatal(callErr)
	}

	// Shutdown is asynchronous: it answers first, then drains and exits. Wait
	// for the process to go away. A replacement daemon another frontend started
	// in the meantime has a different pid and counts as stopped.
	waitCtx, cancelWait := context.WithTimeout(ctx, 5*time.Second)
	defer cancelWait()
	replacementPID, waitErr := waitForDaemonStop(waitCtx, pid, func(probeCtx context.Context) stopProbeResult {
		probe, dialErr := daemon.Dial(probeCtx, projectRoot)
		if dialErr != nil {
			return stopProbeResult{running: daemon.ProcessAlive(pid), pid: pid}
		}
		next, statusErr := probe.DaemonStatus(probeCtx)
		_ = probe.Close()
		if statusErr != nil {
			return stopProbeResult{running: true, pid: pid}
		}
		return stopProbeResult{running: true, pid: next.PID}
	})
	if waitErr != nil {
		fatal(waitErr)
	}
	if replacementPID != 0 {
		fmt.Fprintf(os.Stderr, "Stopped workspace daemon (pid %d); a new one (pid %d) is already serving\n", pid, replacementPID)
		return
	}
	fmt.Fprintf(os.Stderr, "Stopped workspace daemon (pid %d)\n", pid)
}

type stopProbeResult struct {
	running bool
	pid     int
}

func waitForDaemonStop(ctx context.Context, pid int, probe func(context.Context) stopProbeResult) (int, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("daemon (pid %d) did not stop; use `dexter stop --force` to terminate it", pid)
		}
		result := probe(ctx)
		if err := ctx.Err(); err != nil {
			return 0, fmt.Errorf("daemon (pid %d) did not stop; use `dexter stop --force` to terminate it", pid)
		}
		if !result.running {
			return 0, nil
		}
		if result.pid != pid {
			return result.pid, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("daemon (pid %d) did not stop; use `dexter stop --force` to terminate it", pid)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// cmdStopIncompatible stops a daemon this build cannot speak to: a binary from
// another version owns the workspace, and starting a replacement is impossible
// while its lock is held. The control protocol has no way to ask it to exit, so
// --force terminates it by pid; without --force the user is told what that
// would cost.
func cmdStopIncompatible(ctx context.Context, root string, e *daemon.IncompatibleDaemonError, force bool) {
	if !force {
		fatal(fmt.Errorf("%w\nhint: `dexter stop --force` terminates it, dropping any editors still attached to the old daemon", e))
	}
	if err := daemon.ReplaceIncompatible(ctx, root, e); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "Stopped workspace daemon (pid %d)\n", e.DaemonPID)
}

// cmdLSP serves one editor as a thin stdio proxy onto the workspace daemon, so
// the index, watchers, and language caches are shared with every other
// frontend. The daemon starts on demand: it belongs to the workspace, not to
// this editor, the CLI, or any other frontend that happens to reach it first.
func cmdLSP(projectRoot string) {
	projectRoot = findProjectRoot(projectRoot)
	warnProjectRoot(projectRoot)
	log.SetOutput(os.Stderr)
	log.Printf("Dexter LSP proxy v%s starting (root: %s, daemon log: %s)", version.Version, projectRoot, daemonLogPath(projectRoot))
	if err := daemon.ProxyLSP(context.Background(), projectRoot, os.Stdin, os.Stdout); err != nil {
		fatal(err)
	}
}

// daemonLogPath is where the daemon writes its own log, including the debug
// output of a session that enabled it. A daemon-backed session cannot write to
// the editor's stderr, so the proxy's log line has to say where to look.
func daemonLogPath(root string) string {
	endpoint, err := daemon.ResolveEndpoint(root)
	if err != nil {
		return "unknown"
	}
	return endpoint.Log
}

// controlCallTimeout bounds one CLI call to the daemon. A daemon whose store is
// stuck on an unavailable filesystem must not hang a shell forever; this is
// generous enough for an incremental reindex of a large workspace.
const controlCallTimeout = 2 * time.Minute

// queryOptions carries the flags the read-only commands share.
type queryOptions struct {
	// wait blocks the request until the workspace finishes its initial index
	// build, up to this long. Zero — the default — answers immediately from
	// whatever is indexed, so a cold call stays as fast as a warm one.
	wait time.Duration
	// quiet suppresses the human-readable note on stderr.
	quiet bool
}

func (o queryOptions) waitReadyMs() int {
	if o.wait <= 0 {
		return 0
	}
	return int(o.wait.Milliseconds())
}

// warnIfIndexBuilding keeps an empty result from being read as "no matches" when
// the workspace has not finished its first reconciliation. It is a convenience
// for a human on stderr, so --quiet and DEXTER_QUIET suppress it. Callers on the
// control protocol never see it: every response carries `ready`, which is the
// programmatic way to make the same decision, and the daemon logs any request
// that actually blocked on the build.
func warnIfIndexBuilding(ready bool, opts queryOptions) {
	if ready || opts.quiet || envFlag("DEXTER_QUIET") {
		return
	}
	fmt.Fprintln(os.Stderr, "note: the workspace index is still building; re-run shortly for complete results")
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
