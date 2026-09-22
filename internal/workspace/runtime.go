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
}

// Open creates and starts a workspace runtime with native watching enabled.
// Watchers start before the initial reconciliation; their events queue behind
// it so no startup change is lost. Ready is closed once that reconciliation
// completes.
func Open(root string) (*Runtime, error) { return OpenWithOptions(root, Options{}) }

// OpenWithOptions creates and starts a workspace runtime. Open must only be
// called by the process holding the workspace's ownership lock.
func OpenWithOptions(root string, opts Options) (*Runtime, error) {
	s, err := openStore(root)
	if err != nil {
		return nil, err
	}

	index := lsp.NewIndexCoordinator()
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
	if previousStdlibRoot != "" && previousStdlibRoot != stdlibRoot {
		core.RemoveFilesUnderRoot(previousStdlibRoot)
	}
	r := &Runtime{
		root:         root,
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

// Lookup performs the current CLI's name-based lookup against the daemon-owned
// store. Richer operations should be added to the shared language-service API,
// not implemented in protocol adapters.
func (r *Runtime) Lookup(module, function string, followDelegates bool) ([]store.LookupResult, error) {
	if function == "" {
		return r.store.LookupModule(module)
	}
	if followDelegates {
		return r.store.LookupFollowDelegate(module, function)
	}
	return r.store.LookupFunction(module, function)
}

// References performs the store-level references query used by the current
// CLI. The MCP work can extend this through LanguageServices without changing
// daemon ownership.
func (r *Runtime) References(module, function string) ([]store.ReferenceResult, error) {
	return r.store.LookupReferences(module, function)
}

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
	r.core.ReindexWorkspace()
	close(r.ready)
	r.publish(Change{Full: true})

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

func (r *Runtime) reconcilePath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			paths := []string{path}
			stored, listErr := r.store.ListFilePaths()
			if listErr != nil {
				return listErr
			}
			prefix := path + string(os.PathSeparator)
			for _, storedPath := range stored {
				if strings.HasPrefix(storedPath, prefix) {
					paths = append(paths, storedPath)
				}
			}
			r.core.RemoveFiles(paths)
			return nil
		}
		return err
	}
	if info.IsDir() {
		return nil
	}
	base := filepath.Base(path)
	if base == "mix.lock" || base == "mix.exs" {
		r.core.ReindexWorkspace()
		return nil
	}
	if parser.IsElixirFile(path) {
		r.core.ReconcileFile(path)
	}
	return nil
}

// Close stops event sources, drains accepted mutations, waits for background
// index work, checkpoints, and closes the store.
func (r *Runtime) Close() error {
	r.closeOnce.Do(func() {
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
			PathChanged:     r.ReconcileFile,
			FullReconcile:   func() { r.send(event{kind: eventFull}) },
			CoverageChanged: r.watchCoverageChanged,
		})
		if err == nil {
			r.watcherMu.Lock()
			r.watcher = watcher
			r.watcherMu.Unlock()
			log.Printf("Workspace watcher %s started in %s", watcher.Kind(), time.Since(started).Round(time.Millisecond))
			if firstAttempt {
				close(r.watcherReady)
			} else {
				r.watchCoverageChanged(false)
			}
			return
		}
		log.Printf("Warning: native file watching unavailable for %s: %v", r.root, err)
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

func (r *Runtime) startGitWatch() {
	headPath := filepath.Join(r.root, ".git", "HEAD")
	info, err := os.Stat(headPath)
	if err != nil {
		return
	}
	lastMtime := info.ModTime().UnixNano()
	r.gitWG.Add(1)
	go func() {
		defer r.gitWG.Done()
		ticker := time.NewTicker(2 * time.Second)
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

func openStore(root string) (*store.Store, error) {
	s, err := store.Open(root)
	if err != nil {
		log.Printf("Failed to open index at %s (%v); rebuilding derived index", root, err)
		removeIndexFiles(root)
		if s, err = store.Open(root); err != nil {
			return nil, fmt.Errorf("opening index at %s: %w", root, err)
		}
	}
	if stored := s.GetIndexVersion(); stored != version.IndexVersion && !s.IsEmpty() {
		if err := s.Close(); err != nil {
			return nil, fmt.Errorf("closing outdated index: %w", err)
		}
		removeIndexFiles(root)
		if s, err = store.Open(root); err != nil {
			return nil, fmt.Errorf("reopening index at %s: %w", root, err)
		}
	}
	return s, nil
}

func removeIndexFiles(root string) {
	dbPath := store.DBPath(root)
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(path)
	}
}
