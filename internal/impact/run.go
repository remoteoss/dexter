package impact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/remoteoss/dexter/internal/daemon"
	"github.com/remoteoss/dexter/internal/evidence"
	"github.com/remoteoss/dexter/internal/indexer"
	"github.com/remoteoss/dexter/internal/store"
)

const (
	defaultCacheMaxBytes = int64(1024 * 1024 * 1024)
	defaultCacheMaxAge   = 30 * 24 * time.Hour
)

type RunOptions struct {
	Base         string
	Head         string
	ScopeRoot    string
	BaseSnapshot string
	HeadSnapshot string
	CacheDir     string
	MaxDepth     int
	MaxNodes     int
	BaseEvidence []string
	HeadEvidence []string
}

type Revision struct {
	Requested string `json:"requested"`
	Commit    string `json:"commit"`
}

type Provenance struct {
	Repository  string   `json:"repository"`
	IndexPath   string   `json:"index_path"`
	ProjectPath string   `json:"project_path"`
	Base        Revision `json:"base"`
	Head        Revision `json:"head"`
}

type BuildStats struct {
	CacheHit             bool  `json:"cache_hit"`
	ReusedWorkspaceIndex bool  `json:"reused_workspace_index"`
	SnapshotBytes        int64 `json:"snapshot_bytes"`
	Files                int   `json:"files,omitempty"`
	Definitions          int   `json:"definitions,omitempty"`
	References           int   `json:"references,omitempty"`
	CallEdges            int   `json:"call_edges,omitempty"`
	TotalMilliseconds    int64 `json:"total_ms,omitempty"`
	CheckoutMilliseconds int64 `json:"checkout_ms,omitempty"`
	WalkMilliseconds     int64 `json:"source_walk_ms,omitempty"`
	ParseMilliseconds    int64 `json:"parse_cpu_ms,omitempty"`
	WriteMilliseconds    int64 `json:"compact_writer_ms,omitempty"`
	CompactMilliseconds  int64 `json:"compact_finalize_ms,omitempty"`
	IndexesMilliseconds  int64 `json:"create_indexes_ms,omitempty"`
	CommitMilliseconds   int64 `json:"commit_close_ms,omitempty"`
	RenameMilliseconds   int64 `json:"rename_ms,omitempty"`
}

type Report struct {
	ShadowOnly           bool       `json:"shadow_only"`
	Provenance           Provenance `json:"provenance"`
	BaseStats            BuildStats `json:"base_stats"`
	HeadStats            BuildStats `json:"head_stats"`
	Analysis             Result     `json:"analysis"`
	Warnings             []string   `json:"warnings,omitempty"`
	AnalysisMilliseconds int64      `json:"analysis_ms"`
	TotalMilliseconds    int64      `json:"total_ms"`
}

type snapshotResult struct {
	path     string
	stats    BuildStats
	warnings []string
}

type SnapshotReport struct {
	Commit string     `json:"commit"`
	Path   string     `json:"path"`
	Stats  BuildStats `json:"stats"`
}

// CreateSnapshot produces one portable artifact for CI publication.
func CreateSnapshot(projectRoot, revision, output, cacheDir string, evidencePaths []string) (SnapshotReport, error) {
	repository, err := gitOutput(projectRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return SnapshotReport{}, err
	}
	repository = strings.TrimSpace(repository)
	projectPath, err := filepath.Rel(repository, projectRoot)
	if err != nil {
		return SnapshotReport{}, err
	}
	commit, err := resolveRevision(repository, revision)
	if err != nil {
		return SnapshotReport{}, err
	}
	cache, err := impactCacheDir(cacheDir)
	if err != nil {
		return SnapshotReport{}, err
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return SnapshotReport{}, err
	}
	cacheLock, err := acquireCacheLock(cache)
	if err != nil {
		return SnapshotReport{}, err
	}
	defer releaseCacheLock(cacheLock)
	snapshot, err := ensureSnapshot(repository, projectRoot, projectPath, commit, "", cache, evidencePaths)
	if err != nil {
		return SnapshotReport{}, err
	}
	defer pruneImpactCache(cache, map[string]struct{}{snapshot.path: {}})
	absoluteOutput, err := filepath.Abs(output)
	if err != nil {
		return SnapshotReport{}, err
	}
	if absoluteOutput != snapshot.path {
		if err := copySnapshotAtomic(snapshot.path, absoluteOutput); err != nil {
			return SnapshotReport{}, err
		}
	}
	return SnapshotReport{Commit: commit, Path: absoluteOutput, Stats: snapshot.stats}, nil
}

func copySnapshotAtomic(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := os.CreateTemp(filepath.Dir(destination), ".impact-snapshot-*.db")
	if err != nil {
		return err
	}
	temporary := output.Name()
	defer func() { _ = os.Remove(temporary) }()
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}

// Run compares two exact revisions using portable snapshots. Missing snapshots
// are built once and retained in a bounded local cache.
func Run(projectRoot string, options RunOptions) (Report, error) {
	started := time.Now()
	repository, err := gitOutput(projectRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return Report{}, err
	}
	repository = strings.TrimSpace(repository)
	projectPath, err := filepath.Rel(repository, projectRoot)
	if err != nil || projectPath == ".." || strings.HasPrefix(projectPath, ".."+string(filepath.Separator)) {
		return Report{}, fmt.Errorf("project root %s is outside Git repository %s", projectRoot, repository)
	}
	scopeRoot := options.ScopeRoot
	if scopeRoot == "" {
		scopeRoot = projectRoot
	}
	scopeInfo, err := os.Stat(scopeRoot)
	if err != nil {
		return Report{}, fmt.Errorf("selection scope: %w", err)
	}
	if !scopeInfo.IsDir() {
		return Report{}, fmt.Errorf("selection scope %s is not a directory", scopeRoot)
	}
	scopePath, err := filepath.Rel(repository, scopeRoot)
	if err != nil || scopePath == ".." || strings.HasPrefix(scopePath, ".."+string(filepath.Separator)) {
		return Report{}, fmt.Errorf("selection scope %s is outside Git repository %s", scopeRoot, repository)
	}
	snapshotScope, err := filepath.Rel(projectRoot, scopeRoot)
	if err != nil || snapshotScope == ".." || strings.HasPrefix(snapshotScope, ".."+string(filepath.Separator)) {
		return Report{}, fmt.Errorf("selection scope %s is outside index root %s", scopeRoot, projectRoot)
	}
	baseCommit, err := resolveRevision(repository, options.Base)
	if err != nil {
		return Report{}, fmt.Errorf("resolve base revision: %w", err)
	}
	headCommit, err := resolveRevision(repository, options.Head)
	if err != nil {
		return Report{}, fmt.Errorf("resolve head revision: %w", err)
	}
	cacheDir, err := impactCacheDir(options.CacheDir)
	if err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return Report{}, err
	}
	cacheLock, err := acquireCacheLock(cacheDir)
	if err != nil {
		return Report{}, err
	}
	defer releaseCacheLock(cacheLock)

	baseSnapshot, err := ensureSnapshot(repository, projectRoot, projectPath, baseCommit, options.BaseSnapshot, cacheDir, options.BaseEvidence)
	if err != nil {
		return Report{}, fmt.Errorf("prepare base snapshot: %w", err)
	}
	headSnapshot, err := ensureSnapshot(repository, projectRoot, projectPath, headCommit, options.HeadSnapshot, cacheDir, options.HeadEvidence)
	if err != nil {
		return Report{}, fmt.Errorf("prepare head snapshot: %w", err)
	}
	defer pruneImpactCache(cacheDir, map[string]struct{}{baseSnapshot.path: {}, headSnapshot.path: {}})

	baseStore, err := openValidatedSnapshot(baseSnapshot.path, baseCommit, projectPath)
	if err != nil {
		return Report{}, fmt.Errorf("open base snapshot: %w", err)
	}
	defer func() { _ = baseStore.Close() }()
	headStore, err := openValidatedSnapshot(headSnapshot.path, headCommit, projectPath)
	if err != nil {
		return Report{}, fmt.Errorf("open head snapshot: %w", err)
	}
	defer func() { _ = headStore.Close() }()
	changedFiles, err := gitChangedFiles(repository, projectPath, baseCommit, headCommit)
	if err != nil {
		return Report{}, err
	}

	analysisStarted := time.Now()
	analysis, err := Analyze(baseStore, filepath.ToSlash(snapshotScope), headStore, filepath.ToSlash(snapshotScope), Options{
		MaxDepth: options.MaxDepth, MaxNodes: options.MaxNodes, ChangedFiles: changedFiles,
	})
	if err != nil {
		return Report{}, err
	}
	report := Report{
		ShadowOnly: true,
		Provenance: Provenance{
			Repository:  repository,
			IndexPath:   filepath.ToSlash(projectPath),
			ProjectPath: filepath.ToSlash(scopePath),
			Base:        Revision{Requested: options.Base, Commit: baseCommit},
			Head:        Revision{Requested: options.Head, Commit: headCommit},
		},
		BaseStats:            baseSnapshot.stats,
		HeadStats:            headSnapshot.stats,
		Analysis:             analysis,
		Warnings:             append(baseSnapshot.warnings, headSnapshot.warnings...),
		AnalysisMilliseconds: time.Since(analysisStarted).Milliseconds(),
		TotalMilliseconds:    time.Since(started).Milliseconds(),
	}
	sort.Strings(report.Warnings)
	return report, nil
}

func ensureSnapshot(repository, currentProjectRoot, projectPath, commit, suppliedPath, cacheDir string, evidencePaths []string) (snapshotResult, error) {
	providers, err := evidence.LoadArtifacts(evidencePaths, commit)
	if err != nil {
		return snapshotResult{}, err
	}
	config, _, configErr := evidence.LoadRepositoryConfig(currentProjectRoot)
	if configErr != nil {
		return snapshotResult{}, configErr
	}
	currentCommit, _ := resolveRevision(repository, "HEAD")
	nativeCurrentBuild := currentCommit == commit && len(config.CompiledBuildRoots) > 0
	nativeKey := "source"
	if nativeCurrentBuild {
		nativeKey = "native"
	}
	if suppliedPath != "" {
		absolute, err := filepath.Abs(suppliedPath)
		if err != nil {
			return snapshotResult{}, err
		}
		if err := validateSnapshot(absolute, commit, projectPath); err != nil {
			return snapshotResult{}, err
		}
		if err := validateSnapshotEvidence(absolute, providers); err != nil {
			return snapshotResult{}, err
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return snapshotResult{}, err
		}
		return snapshotResult{path: absolute, stats: BuildStats{CacheHit: true, SnapshotBytes: info.Size()}}, nil
	}

	pathHash := sha256.Sum256([]byte(filepath.ToSlash(projectPath)))
	cachePath := filepath.Join(cacheDir, commit+"-"+fmt.Sprintf("%x", pathHash[:8])+"-"+nativeKey+"-"+evidenceCacheKey(providers)+"-v"+strconv.Itoa(store.ImpactSnapshotVersion)+".db")
	if info, err := os.Stat(cachePath); err == nil && !nativeCurrentBuild {
		if validateErr := validateSnapshot(cachePath, commit, projectPath); validateErr == nil && validateSnapshotEvidence(cachePath, providers) == nil {
			now := time.Now()
			_ = os.Chtimes(cachePath, now, now)
			return snapshotResult{path: cachePath, stats: BuildStats{CacheHit: true, SnapshotBytes: info.Size()}}, nil
		}
		_ = os.Remove(cachePath)
	}

	part, err := os.CreateTemp(cacheDir, ".impact-*.db")
	if err != nil {
		return snapshotResult{}, err
	}
	partPath := part.Name()
	if err := part.Close(); err != nil {
		return snapshotResult{}, err
	}
	_ = os.Remove(partPath)
	defer func() { _ = os.Remove(partPath) }()

	status, statusErr := gitOutput(repository, "status", "--porcelain", "--untracked-files=all")
	ignoredSources, ignoredErr := hasIgnoredImpactSources(repository, projectPath)
	clean := currentCommit == commit && statusErr == nil && strings.TrimSpace(status) == "" && ignoredErr == nil && !ignoredSources
	var result snapshotResult
	if clean {
		reuseStarted := time.Now()
		if reuseErr := exportWorkspaceSnapshot(repository, currentProjectRoot, projectPath, commit, partPath, evidencePaths); reuseErr == nil {
			info, statErr := os.Stat(partPath)
			if statErr != nil {
				return snapshotResult{}, statErr
			}
			result = snapshotResult{stats: BuildStats{
				ReusedWorkspaceIndex: true,
				SnapshotBytes:        info.Size(),
				TotalMilliseconds:    time.Since(reuseStarted).Milliseconds(),
			}}
		} else {
			result, err = buildSnapshot(currentProjectRoot, projectPath, commit, partPath, providers, nil)
			if err == nil {
				result.warnings = append(result.warnings, "workspace index reuse unavailable: "+reuseErr.Error())
			}
		}
	} else {
		result, err = buildSnapshotFromWorktree(repository, projectPath, commit, partPath, providers)
	}
	if err != nil {
		return snapshotResult{}, err
	}
	if err := os.Rename(partPath, cachePath); err != nil {
		return snapshotResult{}, err
	}
	if err := syncDirectory(cacheDir); err != nil {
		return snapshotResult{}, err
	}
	result.path = cachePath
	return result, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func exportWorkspaceSnapshot(repository, projectRoot, indexPath, commit, output string, evidencePaths []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client, err := daemon.Ensure(ctx, projectRoot)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var result daemon.ImpactSnapshotResult
	if err := client.Call(ctx, daemon.MethodImpactSnapshot, daemon.ImpactSnapshotParams{
		Path: output, Commit: commit, IndexPath: indexPath, EvidencePaths: evidencePaths,
	}, &result); err != nil {
		return err
	}
	if err := validateSnapshot(output, commit, indexPath); err != nil {
		return fmt.Errorf("validate workspace snapshot: %w", err)
	}
	providers, err := evidence.LoadArtifacts(evidencePaths, commit)
	if err != nil {
		return err
	}
	if err := validateSnapshotEvidence(output, providers); err != nil {
		return fmt.Errorf("validate workspace evidence: %w", err)
	}
	currentCommit, err := resolveRevision(repository, "HEAD")
	if err != nil || currentCommit != commit {
		return errors.New("HEAD changed while exporting workspace index")
	}
	status, err := gitOutput(repository, "status", "--porcelain", "--untracked-files=all")
	if err != nil || strings.TrimSpace(status) != "" {
		return errors.New("worktree changed while exporting workspace index")
	}
	ignoredSources, err := hasIgnoredImpactSources(repository, indexPath)
	if err != nil || ignoredSources {
		return errors.New("ignored source files prevent exact workspace index reuse")
	}
	return nil
}

func hasIgnoredImpactSources(repository, projectPath string) (bool, error) {
	pathspec := projectPath
	if pathspec == "." {
		pathspec = ""
	}
	output, err := gitOutput(repository, "ls-files", "--others", "--ignored", "--exclude-standard", "-z", "--", pathspec)
	if err != nil {
		return false, err
	}
	for _, path := range strings.Split(output, "\x00") {
		path = filepath.ToSlash(path)
		if path == "" || (!strings.HasSuffix(path, ".ex") && !strings.HasSuffix(path, ".exs")) {
			continue
		}
		if path == "deps" || strings.HasPrefix(path, "deps/") || strings.Contains(path, "/deps/") ||
			path == "_build" || strings.HasPrefix(path, "_build/") || strings.Contains(path, "/_build/") ||
			path == "node_modules" || strings.HasPrefix(path, "node_modules/") || strings.Contains(path, "/node_modules/") {
			continue
		}
		return true, nil
	}
	return false, nil
}

func buildSnapshotFromWorktree(repository, projectPath, commit, output string, providers []evidence.LoadedArtifact) (snapshotResult, error) {
	started := time.Now()
	temporaryRoot, err := os.MkdirTemp("", "dexter-impact-worktree-")
	if err != nil {
		return snapshotResult{}, err
	}
	defer func() { _ = os.RemoveAll(temporaryRoot) }()
	worktree := filepath.Join(temporaryRoot, "source")
	checkoutStarted := time.Now()
	if err := gitRun(repository, "worktree", "add", "--detach", worktree, commit); err != nil {
		return snapshotResult{}, err
	}
	checkoutElapsed := time.Since(checkoutStarted)
	removed := false
	defer func() {
		if !removed {
			_ = gitRun(repository, "worktree", "remove", "--force", worktree)
		}
	}()
	result, buildErr := buildSnapshot(filepath.Join(worktree, projectPath), projectPath, commit, output, providers, nil)
	removeErr := gitRun(repository, "worktree", "remove", "--force", worktree)
	removed = removeErr == nil
	if buildErr != nil {
		return snapshotResult{}, buildErr
	}
	if removeErr != nil {
		return snapshotResult{}, fmt.Errorf("remove temporary worktree: %w", removeErr)
	}
	result.stats.CheckoutMilliseconds = checkoutElapsed.Milliseconds()
	result.stats.TotalMilliseconds = time.Since(started).Milliseconds()
	return result, nil
}

func buildSnapshot(projectRoot, indexPath, commit, output string, providers []evidence.LoadedArtifact, projectFiles []string) (snapshotResult, error) {
	var warnings []string
	stats, buildErr := indexer.ImpactBuild(output, projectRoot, commit, indexPath, indexer.Options{Warn: func(format string, args ...interface{}) {
		warning := fmt.Sprintf(format, args...)
		warnings = append(warnings, strings.ReplaceAll(warning, projectRoot, "."))
	}, ProviderEvidence: providers, ProjectFiles: projectFiles})
	if buildErr != nil {
		return snapshotResult{}, buildErr
	}
	info, err := os.Stat(output)
	if err != nil {
		return snapshotResult{}, err
	}
	return snapshotResult{
		stats: BuildStats{
			SnapshotBytes:       info.Size(),
			Files:               stats.Files,
			Definitions:         stats.Definitions,
			References:          stats.References,
			CallEdges:           stats.CallEdges,
			TotalMilliseconds:   stats.Total.Milliseconds(),
			WalkMilliseconds:    stats.Walk.Milliseconds(),
			ParseMilliseconds:   stats.Parse.Milliseconds(),
			WriteMilliseconds:   stats.Write.Milliseconds(),
			CompactMilliseconds: stats.Finalize.Milliseconds(),
			IndexesMilliseconds: stats.CreateIndexes.Milliseconds(),
			CommitMilliseconds:  stats.Commit.Milliseconds(),
			RenameMilliseconds:  stats.Rename.Milliseconds(),
		},
		warnings: warnings,
	}, nil
}

func validateSnapshot(path, commit, indexPath string) error {
	snapshot, err := store.OpenImpactSnapshot(path)
	if err != nil {
		return err
	}
	actual, err := snapshot.ImpactSnapshotCommit()
	if err != nil {
		_ = snapshot.Close()
		return err
	}
	actualPath, err := snapshot.ImpactSnapshotIndexPath()
	closeErr := snapshot.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if actual != commit {
		return fmt.Errorf("snapshot commit %s, want %s", actual, commit)
	}
	if actualPath != filepath.ToSlash(indexPath) {
		return fmt.Errorf("snapshot index path %s, want %s", actualPath, filepath.ToSlash(indexPath))
	}
	return nil
}

func evidenceCacheKey(providers []evidence.LoadedArtifact) string {
	if len(providers) == 0 {
		return "e0"
	}
	h := sha256.New()
	for _, provider := range providers {
		_, _ = io.WriteString(h, provider.Artifact.Provider)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, provider.Digest)
		_, _ = io.WriteString(h, "\x00")
	}
	return fmt.Sprintf("e%x", h.Sum(nil)[:8])
}

func validateSnapshotEvidence(path string, providers []evidence.LoadedArtifact) error {
	snapshot, err := store.OpenImpactSnapshot(path)
	if err != nil {
		return err
	}
	digests, err := snapshot.ImpactEvidenceDigests()
	closeErr := snapshot.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, provider := range providers {
		if digests[provider.Artifact.Provider] != provider.Digest {
			return fmt.Errorf("snapshot evidence provider %s does not match", provider.Artifact.Provider)
		}
	}
	return nil
}

func openValidatedSnapshot(path, commit, indexPath string) (*store.Store, error) {
	snapshot, err := store.OpenImpactSnapshot(path)
	if err != nil {
		return nil, err
	}
	actual, err := snapshot.ImpactSnapshotCommit()
	if err != nil {
		_ = snapshot.Close()
		return nil, err
	}
	if actual != commit {
		_ = snapshot.Close()
		return nil, fmt.Errorf("snapshot commit %s, want %s", actual, commit)
	}
	actualPath, err := snapshot.ImpactSnapshotIndexPath()
	if err != nil {
		_ = snapshot.Close()
		return nil, err
	}
	if actualPath != filepath.ToSlash(indexPath) {
		_ = snapshot.Close()
		return nil, fmt.Errorf("snapshot index path %s, want %s", actualPath, filepath.ToSlash(indexPath))
	}
	return snapshot, nil
}

func acquireCacheLock(cacheDir string) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(cacheDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func releaseCacheLock(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func impactCacheDir(configured string) (string, error) {
	if configured != "" {
		return filepath.Abs(configured)
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "dexter", "impact"), nil
}

func pruneImpactCache(cacheDir string, keep map[string]struct{}) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	type cacheFile struct {
		path    string
		size    int64
		modTime time.Time
	}
	files := make([]cacheFile, 0, len(entries))
	var total int64
	now := time.Now()
	versionSuffix := "-v" + strconv.Itoa(store.ImpactSnapshotVersion) + ".db"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".db") {
			continue
		}
		path := filepath.Join(cacheDir, entry.Name())
		if strings.Contains(entry.Name(), "-v") && !strings.HasSuffix(entry.Name(), versionSuffix) {
			removeSnapshotFiles(path)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if _, retained := keep[path]; !retained && now.Sub(info.ModTime()) > defaultCacheMaxAge {
			removeSnapshotFiles(path)
			continue
		}
		files = append(files, cacheFile{path: path, size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files {
		if total <= defaultCacheMaxBytes {
			break
		}
		if _, retained := keep[file.path]; retained {
			continue
		}
		removeSnapshotFiles(file.path)
		total -= file.size
	}
}

func removeSnapshotFiles(path string) {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(candidate)
	}
}

func resolveRevision(repository, revision string) (string, error) {
	output, err := gitOutput(repository, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func gitChangedFiles(repository, projectPath, base, head string) ([]string, error) {
	output, err := gitOutput(repository, "diff", "--name-only", "-z", base, head, "--")
	if err != nil {
		return nil, err
	}
	prefix := ""
	if projectPath != "." {
		prefix = filepath.ToSlash(projectPath) + "/"
	}
	var result []string
	for _, path := range strings.Split(output, "\x00") {
		path = filepath.ToSlash(path)
		if path == "" || (prefix != "" && !strings.HasPrefix(path, prefix)) {
			continue
		}
		result = append(result, strings.TrimPrefix(path, prefix))
	}
	sort.Strings(result)
	return result, nil
}

func gitRun(repository string, args ...string) error {
	_, err := gitOutput(repository, args...)
	return err
}

func gitOutput(repository string, args ...string) (string, error) {
	arguments := append([]string{"-C", repository}, args...)
	command := exec.Command("git", arguments...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
