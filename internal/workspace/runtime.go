// Package workspace owns the long-lived, protocol-independent state for one
// Dexter project root.
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/remoteoss/dexter/internal/lsp"
	"github.com/remoteoss/dexter/internal/notify"
	"github.com/remoteoss/dexter/internal/parser"
	"github.com/remoteoss/dexter/internal/stdlib"
	"github.com/remoteoss/dexter/internal/store"
	"github.com/remoteoss/dexter/internal/version"
)

// eventDebounce bounds how long a burst of filesystem events is coalesced into
// one pass. An isolated event is never delayed by it; the mutation loop opens
// this window only when more events are already queued. A variable so tests can
// shrink it.
var eventDebounce = 25 * time.Millisecond

var startNativeWatch = Watch

type eventKind uint8

const (
	eventPath eventKind = iota
	eventFull
	eventStdlib
	eventStop
)

type event struct {
	kind eventKind
	path string
	done chan error
}

// Runtime owns one workspace store, its index mutation coordinator, its native
// watchers, and the headless language-service instance used by non-editor
// frontends. Open must only be called by the process holding the workspace's
// external ownership lock.
type Runtime struct {
	root  string
	hook  func()
	store *store.Store
	index *lsp.IndexCoordinator
	core  *lsp.Server

	events   chan event
	ready    chan struct{}
	readyErr error

	sendMu    sync.Mutex
	closing   bool
	closeOnce sync.Once
	closeErr  error

	coverageMu   sync.Mutex // orders coverage reports; see reportCoverage
	watcherMu    sync.RWMutex
	watcher      *Watcher
	watcherReady chan struct{}
	watcherStop  chan struct{}
	watcherWG    sync.WaitGroup
	gitStop      chan struct{}
	gitWG        sync.WaitGroup
	loopWG       sync.WaitGroup

	subsMu sync.Mutex
	subs   map[*changeSubscriber]struct{}

	sessMu   sync.Mutex
	sessions map[string]*lspSession
	sessSeq  int
	sessTag  string
}

// Options configures a workspace runtime.
type Options struct {
	// NoWatch leaves native filesystem watching off. The workspace still
	// reconciles editor notifications, git HEAD changes, and explicit reindex
	// requests; it just does not observe the tree itself. Tests use it to drive
	// the mutation queue deterministically.
	NoWatch bool

	// BeforeInitialReconcile runs on the mutation loop before the first
	// reconciliation. Tests use it to hold a workspace in that state, for
	// example to attach an editor while a rebuild is in progress.
	BeforeInitialReconcile func()
}

// Open creates and starts a workspace runtime with native watching enabled.
// Watchers start before the initial reconciliation; their events queue behind
// it so no startup change is lost. Ready is closed once that reconciliation
// completes.
func Open(root string) (*Runtime, error) { return OpenWithOptions(root, Options{}) }

// OpenWithOptions creates and starts a workspace runtime. Open must only be
// called by the process holding the workspace's ownership lock.
func OpenWithOptions(root string, opts Options) (*Runtime, error) {
	index := lsp.NewIndexCoordinator()
	reporter := index.Reporter()
	// Before the store opens: opening it creates the database, which is
	// itself a project marker.
	reportRoot(root, reporter)
	s, err := openStore(root, reporter)
	if err != nil {
		return nil, err
	}

	previousStdlibRoot, _ := s.GetStdlibRoot()
	stdlibRoot := ""
	if resolved, ok := stdlib.Resolve(s, "", root); ok {
		stdlibRoot = resolved
	}
	core := lsp.NewServerWithOptions(s, root, lsp.ServerOptions{
		Index:             index,
		ManageWorkspace:   false,
		InitialStdlibRoot: stdlibRoot,
	})
	core.ReportStdlib()
	if previousStdlibRoot != "" && previousStdlibRoot != stdlibRoot {
		core.RemoveFilesUnderRoot(previousStdlibRoot)
	}
	r := &Runtime{
		root:         root,
		hook:         opts.BeforeInitialReconcile,
		store:        s,
		index:        index,
		core:         core,
		events:       make(chan event, 4096),
		ready:        make(chan struct{}),
		watcherReady: make(chan struct{}),
		watcherStop:  make(chan struct{}),
		gitStop:      make(chan struct{}),
		subs:         make(map[*changeSubscriber]struct{}),
		sessions:     make(map[string]*lspSession),
		sessTag:      newSessionTag(),
	}

	r.startGitWatch()
	if opts.NoWatch {
		close(r.watcherReady)
	} else {
		r.watcherWG.Add(1)
		go r.startNativeWatcher()
	}
	r.loopWG.Add(1)
	go r.loop()
	return r, nil
}

// Root returns the canonical project root served by the runtime.
func (r *Runtime) Root() string { return r.root }

// Ready closes after the initial full-or-incremental reconciliation.
func (r *Runtime) Ready() <-chan struct{} { return r.ready }

// WaitReady waits until the runtime can answer index-dependent requests.
func (r *Runtime) WaitReady(ctx context.Context) error {
	select {
	case <-r.ready:
		return r.readyErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AttachLSPSession returns session-local LSP state over the daemon's shared
// store and index coordinator, registered under a new session id so another
// frontend can explicitly opt into this editor's document overlay. Open
// documents and client capabilities are never shared between sessions. Call
// release when the connection ends: it unregisters the session and frees its
// parser and BEAM caches, so a caller cannot leak them by forgetting.
func (r *Runtime) AttachLSPSession() (id string, session *lsp.Server, release func()) {
	session = lsp.NewServerWithOptions(r.store, r.root, lsp.ServerOptions{
		Index:           r.index,
		Events:          r,
		ManageWorkspace: false,
	})
	r.sessMu.Lock()
	r.sessSeq++
	id = fmt.Sprintf("%s-%d", r.sessTag, r.sessSeq)
	r.sessions[id] = &lspSession{server: session, connectedAt: time.Now()}
	r.sessMu.Unlock()
	return id, session, func() {
		// Both steps are idempotent, so releasing twice is safe.
		r.sessMu.Lock()
		delete(r.sessions, id)
		r.sessMu.Unlock()
		session.CloseSession()
	}
}

type lspSession struct {
	server      *lsp.Server
	connectedAt time.Time
}

// Session returns the LSP session registered under id. A frontend uses it to
// answer from one editor's overlay. It must be given the id by that editor;
// picking an arbitrary session would answer from someone else's unsaved
// buffers.
func (r *Runtime) Session(id string) (*lsp.Server, bool) {
	r.sessMu.Lock()
	defer r.sessMu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return nil, false
	}
	return s.server, true
}

// EditorBuffer is a buffer that an attached editor holds open with changes it
// has not saved.
type EditorBuffer struct {
	Text      string
	ChangedAt time.Time // the time of the last change in the editor
}

// UnsavedBuffer returns the newest buffer for path that an attached editor
// holds open with unsaved changes, or false when there is none. A frontend
// without an editor, such as MCP, uses it to read what the user is editing
// instead of the disk. A buffer the editor has not changed since it opened or
// saved it is not returned: it can be older than the disk.
func (r *Runtime) UnsavedBuffer(path string) (EditorBuffer, bool) {
	r.sessMu.Lock()
	if len(r.sessions) == 0 {
		r.sessMu.Unlock()
		return EditorBuffer{}, false
	}
	servers := make([]*lsp.Server, 0, len(r.sessions))
	for _, s := range r.sessions {
		servers = append(servers, s.server)
	}
	r.sessMu.Unlock()
	var out EditorBuffer
	var newest uint64
	found := false
	for _, srv := range servers {
		if t, seq, at, ok := srv.UnsavedBuffer(path); ok && (!found || seq > newest) {
			out, newest, found = EditorBuffer{Text: t, ChangedAt: at}, seq, true
		}
	}
	return out, found
}

// SessionInfo describes one attached editor session for status output.
type SessionInfo struct {
	ID            string
	OpenDocuments int
	ConnectedAt   time.Time
}

// Sessions lists attached editor sessions, oldest connection first.
func (r *Runtime) Sessions() []SessionInfo {
	r.sessMu.Lock()
	defer r.sessMu.Unlock()
	out := make([]SessionInfo, 0, len(r.sessions))
	for id, s := range r.sessions {
		out = append(out, SessionInfo{ID: id, OpenDocuments: s.server.OpenDocuments(), ConnectedAt: s.connectedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConnectedAt.Before(out[j].ConnectedAt) })
	return out
}

func newSessionTag() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b[:])
}

// Reporter returns the reporter that tells every attached editor about
// failures, degraded states, and long work in this workspace.
func (r *Runtime) Reporter() *notify.Reporter { return r.index.Reporter() }

// IndexConditions returns the active conditions that describe the index, such
// as a rebuild or an index that cannot be used. The CLI prints them.
func (r *Runtime) IndexConditions() []notify.Condition {
	var out []notify.Condition
	for _, c := range r.Reporter().Conditions() {
		if strings.HasPrefix(c.Key, lsp.IndexConditionPrefix) {
			out = append(out, c)
		}
	}
	return out
}

// Store returns the workspace index. Frontends read through it instead of
// opening a second handle, so one writer owns the file.
func (r *Runtime) Store() *store.Store { return r.store }

// StdlibRoot returns the resolved Elixir stdlib directory, or "" when none was
// detected.
func (r *Runtime) StdlibRoot() string { return r.core.StdlibRoot() }

// Watching reports whether native filesystem watching is active. When false the
// workspace still reconciles on editor notifications, git HEAD changes, and
// explicit reindex requests.
func (r *Runtime) Watching() bool {
	r.watcherMu.RLock()
	defer r.watcherMu.RUnlock()
	return r.watcher != nil
}

// IsReady reports whether the initial reconciliation has completed.
func (r *Runtime) IsReady() bool {
	select {
	case <-r.ready:
		return true
	default:
		return false
	}
}

// IndexStatus is a protocol-neutral snapshot of workspace readiness and index
// size.
type IndexStatus struct {
	Root                 string
	Ready                bool
	Watching             bool
	StdlibRoot           string
	IndexVersion         int
	ExpectedIndexVersion int
	Files                int
	Definitions          int
	References           int
}

// IndexStatus reads the current snapshot. Callers that only need readiness
// should use IsReady, which does not touch the store.
func (r *Runtime) IndexStatus() (IndexStatus, error) {
	st, err := r.store.Stats()
	if err != nil {
		return IndexStatus{}, err
	}
	return IndexStatus{
		Root:                 r.root,
		Ready:                r.IsReady(),
		Watching:             r.Watching(),
		StdlibRoot:           r.StdlibRoot(),
		IndexVersion:         r.store.GetIndexVersion(),
		ExpectedIndexVersion: version.IndexVersion,
		Files:                st.Files,
		Definitions:          st.Definitions,
		References:           st.References,
	}, nil
}

// Change is one coalesced index mutation batch.
type Change struct {
	// Full reports a whole-workspace reconciliation; Paths is then empty.
	Full bool
	// Paths are the reconciled files, sorted, when Full is false.
	Paths []string
}

type changeSubscriber struct {
	ch     chan Change
	mu     sync.Mutex
	closed bool
	lost   bool
}

// Subscribe registers an in-process consumer of coalesced index mutations. The
// workspace already owns the only filesystem and git watchers, so a frontend
// must not start its own; it subscribes here.
//
// Delivery never blocks the mutation loop. A consumer that falls behind loses
// detail rather than stalling indexing: its next delivery reports Full so it can
// refresh coarsely. Cancel removes the subscription and closes the channel.
func (r *Runtime) Subscribe(buffer int) (<-chan Change, func()) {
	if buffer <= 0 {
		buffer = 64
	}
	sub := &changeSubscriber{ch: make(chan Change, buffer)}
	r.subsMu.Lock()
	r.subs[sub] = struct{}{}
	r.subsMu.Unlock()
	return sub.ch, func() {
		r.subsMu.Lock()
		defer r.subsMu.Unlock()
		if _, ok := r.subs[sub]; !ok {
			return
		}
		delete(r.subs, sub)
		sub.mu.Lock()
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
		sub.mu.Unlock()
	}
}

func (r *Runtime) publish(c Change) {
	r.subsMu.Lock()
	if len(r.subs) == 0 {
		r.subsMu.Unlock()
		return
	}
	subs := make([]*changeSubscriber, 0, len(r.subs))
	for s := range r.subs {
		subs = append(subs, s)
	}
	r.subsMu.Unlock()
	for _, s := range subs {
		s.send(c)
	}
}

func (s *changeSubscriber) send(c Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.lost {
		s.lost = false
		c = Change{Full: true}
	}
	select {
	case s.ch <- c:
	default:
		s.lost = true
	}
}

// LanguageServices returns the headless, disk-backed language-service instance
// for future protocol-neutral operations such as MCP name-based tools. It has
// no client connection or editor document overlay.
func (r *Runtime) LanguageServices() *lsp.Server { return r.core }

// ReconcileFile implements lsp.WorkspaceEvents. It is intentionally
// non-blocking for editor notifications; bursts are coalesced by path.
func (r *Runtime) ReconcileFile(path string) {
	r.send(event{kind: eventPath, path: path})
}

// RemoveFile implements lsp.WorkspaceEvents. Reconciliation stats the path, so
// create/change/delete notifications all share one idempotent code path.
func (r *Runtime) RemoveFile(path string) {
	r.send(event{kind: eventPath, path: path})
}

// watchCoverageChanged schedules a sweep when a gap opens and whenever coverage
// returns to a subtree, to catch changes made while that watch was unavailable.
func (r *Runtime) watchCoverageChanged(bool) {
	r.send(event{kind: eventFull})
}

// SetStdlibRoot updates the shared root used by every LSP session and queues the
// runtime-owned reconciliation. Initialize must not wait for a workspace scan.
func (r *Runtime) SetStdlibRoot(ctx context.Context, path string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if r.closing {
		return errors.New("workspace is shutting down")
	}
	old, changed := r.core.SetStdlibRoot(path)
	if changed {
		r.events <- event{kind: eventStdlib, path: old}
	}
	return nil
}

// Reindex schedules a full incremental reconciliation and waits for a queue
// barrier. Every event accepted before the call is reflected when it returns.
func (r *Runtime) Reindex(ctx context.Context) error {
	done := make(chan error, 1)
	if !r.send(event{kind: eventFull, done: done}) {
		return errors.New("workspace is shutting down")
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Indexes reports whether path is an indexed file or a directory holding one.
// It is a read, so it does not wait behind queued mutations.
func (r *Runtime) Indexes(path string) (bool, error) {
	return r.store.HasPath(path)
}

// ReindexPath reconciles one file and waits for it, schedules a full pass for a
// directory, and prunes a path that no longer exists. Reconciliation stats the
// path itself, so create, change, and delete share one idempotent code path.
func (r *Runtime) ReindexPath(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		return r.Reindex(ctx)
	case err != nil && !os.IsNotExist(err):
		return err
	}
	return r.awaitPath(ctx, path)
}

// awaitPath queues one path mutation and waits for the batch that included it.
func (r *Runtime) awaitPath(ctx context.Context, path string) error {
	done := make(chan error, 1)
	if !r.send(event{kind: eventPath, path: path, done: done}) {
		return errors.New("workspace is shutting down")
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) send(ev event) bool {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if r.closing {
		if ev.done != nil {
			ev.done <- errors.New("workspace is shutting down")
		}
		return false
	}
	r.events <- ev
	return true
}

func (r *Runtime) loop() {
	defer r.loopWG.Done()
	<-r.watcherReady
	if r.hook != nil {
		r.hook()
	}
	r.core.ReindexWorkspace()
	r.publish(Change{Full: true})
	close(r.ready)

	for {
		ev := <-r.events
		paths := make(map[string]struct{})
		oldStdlibRoots := make(map[string]struct{})
		full := false
		stop := false
		var barriers []chan error
		collect := func(e event) {
			switch e.kind {
			case eventPath:
				paths[e.path] = struct{}{}
			case eventFull:
				full = true
			case eventStdlib:
				full = true
				if e.path != "" {
					oldStdlibRoots[e.path] = struct{}{}
				}
			case eventStop:
				stop = true
			}
			if e.done != nil {
				barriers = append(barriers, e.done)
			}
		}
		collect(ev)

		// Absorb whatever is already queued without waiting. An isolated
		// editor save is indexed immediately instead of paying a debounce
		// window; only a burst in flight opens one.
		burst := r.absorb(collect) > 0
		if burst && !stop && !full {
			timer := time.NewTimer(eventDebounce)
		window:
			for {
				select {
				case e := <-r.events:
					collect(e)
					if stop {
						break window
					}
				case <-timer.C:
					break window
				}
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}

		var batchErr error
		if full {
			for root := range oldStdlibRoots {
				r.core.RemoveFilesUnderRoot(root)
			}
			r.core.ReindexWorkspace()
		} else {
			for path := range paths {
				batchErr = errors.Join(batchErr, r.reconcilePath(path))
			}
		}
		r.publishBatch(full, paths)
		for _, done := range barriers {
			done <- batchErr
		}
		if stop {
			return
		}
	}
}

// absorb drains already-queued events without blocking and reports how many it
// took. Zero means the arriving event was isolated.
func (r *Runtime) absorb(collect func(event)) int {
	n := 0
	for {
		select {
		case e := <-r.events:
			collect(e)
			n++
		default:
			return n
		}
	}
}

// publishBatch reports one completed batch to in-process subscribers. The change
// is built only when someone is listening.
func (r *Runtime) publishBatch(full bool, paths map[string]struct{}) {
	r.subsMu.Lock()
	listening := len(r.subs) > 0
	r.subsMu.Unlock()
	if !listening {
		return
	}
	c := Change{Full: full}
	if !full && len(paths) > 0 {
		c.Paths = make([]string, 0, len(paths))
		for path := range paths {
			c.Paths = append(c.Paths, path)
		}
		sort.Strings(c.Paths)
	}
	r.publish(c)
}

// testHookReconcilePath, when set by a test, runs before each path event is
// reconciled.
var testHookReconcilePath func(path string)

func (r *Runtime) reconcilePath(path string) error {
	if testHookReconcilePath != nil {
		testHookReconcilePath(path)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			under, listErr := r.store.ListFilePathsUnder(path)
			if listErr != nil {
				return listErr
			}
			r.core.RemoveFiles(append(under, path))
			return nil
		}
		return err
	}
	if info.IsDir() {
		// A watcher reports a directory when it turns out to be a nested
		// worktree. Files indexed from it before its .git file appeared, as cp -r
		// can do, are removed with one range read of the path index.
		// A worktree that git is still moving counts too: its .git file can
		// be empty when the watcher reports it.
		if path != r.root && parser.GitFile(path).Nested() {
			under, err := r.store.ListFilePathsUnder(path)
			if err != nil {
				return err
			}
			if len(under) > 0 {
				r.core.RemoveFiles(under)
			}
		}
		return nil
	}
	base := filepath.Base(path)
	if base == "mix.lock" || base == "mix.exs" {
		// A nested worktree is a separate checkout the index leaves out, so its
		// manifests must not reindex this workspace. Source files need no check
		// here: the core skips them itself.
		if parser.InLinkedWorktree(r.root, path) {
			return nil
		}
		r.core.ReindexWorkspace()
		return nil
	}
	if parser.IsElixirFile(path) {
		r.core.ReconcileFile(path)
	}
	return nil
}

// Close cancels a reconciliation in flight, stops event sources, drains
// accepted mutations, waits for background index work, checkpoints, and closes
// the store.
//
// The cancel comes first. A warm pass over a large change set can run for
// minutes, and the daemon holds the workspace lock without serving its socket
// until Close returns. A canceled pass leaves every file fully old or fully new,
// and the next start finishes it from the stored mtimes.
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
		r.index.CancelWork()
		close(r.watcherStop)
		r.watcherWG.Wait()
		<-r.watcherReady
		r.watcherMu.RLock()
		watcher := r.watcher
		r.watcherMu.RUnlock()
		if watcher != nil {
			r.closeErr = errors.Join(r.closeErr, watcher.Close())
		}
		close(r.gitStop)
		r.gitWG.Wait()

		done := make(chan error, 1)
		r.sendMu.Lock()
		r.closing = true
		r.events <- event{kind: eventStop, done: done}
		r.sendMu.Unlock()
		r.closeErr = errors.Join(r.closeErr, <-done)
		r.loopWG.Wait()
		r.core.WaitForIndexWork()
		r.closeErr = errors.Join(r.closeErr, r.store.Checkpoint())
		r.closeErr = errors.Join(r.closeErr, r.store.Close())
	})
	return r.closeErr
}

func (r *Runtime) startNativeWatcher() {
	defer r.watcherWG.Done()
	firstAttempt := true
	for {
		started := time.Now()
		watcher, err := startNativeWatch(r.root, WatchCallbacks{
			PathChanged:   r.ReconcileFile,
			FullReconcile: func() { r.send(event{kind: eventFull}) },
			CoverageChanged: func(degraded bool) {
				r.reportCoverage()
				r.watchCoverageChanged(degraded)
			},
		})
		if err == nil {
			r.watcherMu.Lock()
			r.watcher = watcher
			r.watcherMu.Unlock()
			log.Printf("Workspace watcher %s started in %s", watcher.Kind(), time.Since(started).Round(time.Millisecond))
			r.reportWatcherStarted(watcher)
			if firstAttempt {
				close(r.watcherReady)
			} else {
				r.watchCoverageChanged(false)
			}
			return
		}
		r.reportWatchUnavailable(err)
		if firstAttempt {
			close(r.watcherReady)
			firstAttempt = false
		}
		timer := time.NewTimer(watchRetryInterval)
		select {
		case <-timer.C:
		case <-r.watcherStop:
			if !timer.Stop() {
				<-timer.C
			}
			return
		}
	}
}

// gitHeadPollInterval is how often the runtime checks HEAD for a branch switch.
var gitHeadPollInterval = 2 * time.Second

func (r *Runtime) startGitWatch() {
	// In a linked worktree or a submodule, .git is a file that names the git
	// directory, and HEAD is there.
	gitDir, ok := parser.GitDir(r.root)
	if !ok {
		return
	}
	headPath := filepath.Join(gitDir, "HEAD")
	info, err := os.Stat(headPath)
	if err != nil {
		return
	}
	lastMtime := info.ModTime().UnixNano()
	r.gitWG.Add(1)
	go func() {
		defer r.gitWG.Done()
		ticker := time.NewTicker(gitHeadPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-r.gitStop:
				return
			case <-ticker.C:
			}
			info, err := os.Stat(headPath)
			if err != nil {
				continue
			}
			mtime := info.ModTime().UnixNano()
			if mtime != lastMtime {
				lastMtime = mtime
				r.send(event{kind: eventFull})
			}
		}
	}()
}

// openBusyWait bounds how long openStore waits for another process to release
// a locked index. Each attempt also waits out the store's own busy timeout. A
// variable so tests can shrink it.
var openBusyWait = 30 * time.Second

// openStore opens the index. It deletes the index for a rebuild only when that
// is safe and useful: when the file is damaged, or when it was written by
// another index version. Each rebuild is a condition that the first
// reconciliation clears.
//
// A locked index is never deleted. Another process, such as `dexter init` or an
// older Dexter release, can hold it in a write transaction, and deleting the
// files under it loses its work in silence. openStore waits for the lock, and
// fails when it stays, so that the editor shows why. Permission, disk-space,
// and similar errors do not delete either: a rebuild cannot fix them.
func openStore(root string, reporter *notify.Reporter) (*store.Store, error) {
	s, err := openWaitingForLock(root, reporter)
	if err != nil {
		switch store.ClassifyOpenError(err) {
		case store.OpenFailureDamaged:
			reporter.Set(lsp.CondIndexRebuild, notify.Warning, damagedIndexMessage(root, err))
			removeIndexFiles(root)
			if s, err = store.Open(root); err != nil {
				return nil, openFailed(reporter, otherOpenFailureMessage(root, err), err)
			}
		case store.OpenFailureBusy:
			return nil, openFailed(reporter, lockedIndexMessage(root, err), err)
		default:
			return nil, openFailed(reporter, otherOpenFailureMessage(root, err), err)
		}
	}
	if stored := s.GetIndexVersion(); stored != version.IndexVersion && !s.IsEmpty() {
		reporter.Set(lsp.CondIndexRebuild, notify.Warning, versionMismatchMessage(stored, version.IndexVersion))
		if err := s.Close(); err != nil {
			return nil, fmt.Errorf("closing outdated index: %w", err)
		}
		removeIndexFiles(root)
		if s, err = store.Open(root); err != nil {
			return nil, openFailed(reporter, otherOpenFailureMessage(root, err), err)
		}
	}
	return s, nil
}

// openWaitingForLock opens the store and retries while another process holds
// it locked, up to openBusyWait.
func openWaitingForLock(root string, reporter *notify.Reporter) (*store.Store, error) {
	deadline := time.Now().Add(openBusyWait)
	backoff := 100 * time.Millisecond
	waited := false
	for {
		s, err := store.Open(root)
		if err == nil {
			if waited {
				reporter.Clear(lsp.CondIndexUnavailable, "Dexter: the other process released the index; Dexter continues.")
			}
			return s, nil
		}
		if store.ClassifyOpenError(err) != store.OpenFailureBusy || !time.Now().Before(deadline) {
			return nil, err
		}
		if !waited {
			waited = true
			reporter.Set(lsp.CondIndexUnavailable, notify.Error, fmt.Sprintf(
				"Dexter: another process holds the index at %s locked (%v). Dexter waits up to %s for it and does not change the index.",
				store.DBPath(root), err, openBusyWait))
		}
		time.Sleep(backoff)
		if backoff < time.Second {
			backoff *= 2
		}
	}
}

// openIndexError is an index that could not be opened. Its text is the
// message for the user: the daemon exits with it, and a frontend shows the end
// of the daemon log in the editor.
type openIndexError struct {
	message string
	err     error
}

func (e *openIndexError) Error() string { return strings.TrimPrefix(e.message, "Dexter: ") }
func (e *openIndexError) Unwrap() error { return e.err }

func openFailed(reporter *notify.Reporter, message string, err error) error {
	reporter.Set(lsp.CondIndexUnavailable, notify.Error, message)
	return &openIndexError{message: message, err: err}
}

func removeIndexFiles(root string) {
	dbPath := store.DBPath(root)
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(path)
	}
}
