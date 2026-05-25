package lsp

import (
	"container/list"
	"os"
	"strings"
	"sync"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_elixir "github.com/tree-sitter/tree-sitter-elixir/bindings/go"
	"go.lsp.dev/protocol"

	"github.com/remoteoss/dexter/internal/parser"
)

// defaultMaxTransient caps how many disk-loaded buffers may live in the
// store concurrently. Editor-owned buffers (added via Set) are never counted
// against this cap.
const defaultMaxTransient = 50

type cachedDoc struct {
	text       string
	tree       *refTree
	src        []byte         // source bytes the tree references - must stay alive
	tokens     []parser.Token // cached tokenizer output
	tokSrc     []byte         // source bytes for tokens
	lineStarts []int          // byte offset of each line start (from TokenizeFull)
	// transient is true for entries loaded from disk via GetOrLoad - i.e.
	// no editor sent didOpen for this URI. These entries are tracked in an
	// LRU and evicted once the transient cap is reached. Editor-owned
	// entries (created via Set) are never transient and never evicted.
	transient bool
}

// refTree wraps a tree-sitter parse tree with refcounting so that
// concurrent handlers walking the tree (RootNode, queries) aren't racing
// with eviction or replacement, which would free the underlying C memory
// via ts_tree_delete and cause a use-after-free that the Go race detector
// cannot observe.
//
// Lifecycle: GetTree increments refs under the store write lock; the
// returned release closure decrements under the same lock and frees the
// tree only once refs==0 AND retired is set. Set/Close/CloseAll/eviction
// don't close the tree directly - they call retireLocked, which marks the
// tree for free and only triggers ts_tree_delete if no handler still
// holds a reference.
type refTree struct {
	tree    *tree_sitter.Tree
	refs    int
	retired bool
}

// retireLocked marks the tree for free. Caller must hold the store write
// lock. If no handler is currently using the tree, frees it immediately;
// otherwise the last release closes it.
func (rt *refTree) retireLocked() {
	if rt == nil || rt.tree == nil {
		return
	}
	rt.retired = true
	if rt.refs == 0 {
		rt.tree.Close()
		rt.tree = nil
	}
}

// DocumentStore tracks the text content of open buffers and caches
// tree-sitter parse trees for each document. All access is serialized
// through a single RWMutex: reads (Get) take RLock, writes and parsing
// (Set, Close, GetTree) take Lock.
//
// In addition to editor-managed buffers (populated by Set on didOpen /
// didChange), DocumentStore can lazily load buffers from disk via
// GetOrLoad. Disk-loaded entries are marked transient and tracked in an
// LRU list so that AI tools that don't drive a didOpen/didClose lifecycle
// (e.g. Claude Code) can still query references/hover/definition without
// causing unbounded memory growth.
type DocumentStore struct {
	mu     sync.RWMutex
	docs   map[string]*cachedDoc
	parser *tree_sitter.Parser

	// LRU bookkeeping for transient (disk-loaded) entries only. The list
	// holds URIs in access-order, newest at the front. transientIdx maps
	// URI → its list element for O(1) move/remove.
	transientList *list.List
	transientIdx  map[string]*list.Element
	maxTransient  int
}

func NewDocumentStore() *DocumentStore {
	p := tree_sitter.NewParser()
	_ = p.SetLanguage(tree_sitter.NewLanguage(tree_sitter_elixir.Language()))
	return &DocumentStore{
		docs:          make(map[string]*cachedDoc),
		parser:        p,
		transientList: list.New(),
		transientIdx:  make(map[string]*list.Element),
		maxTransient:  defaultMaxTransient,
	}
}

// SetMaxTransient updates the cap on disk-loaded (transient) entries and
// evicts any excess immediately. A cap of 0 disables transient caching -
// disk-loaded entries are inserted and immediately evicted, so the store
// still serves the read but never retains it. Editor-owned entries are
// never affected. Negative values are clamped to 0.
func (ds *DocumentStore) SetMaxTransient(n int) {
	if n < 0 {
		n = 0
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.maxTransient = n
	ds.evictTransientLocked()
}

func (ds *DocumentStore) Set(uri string, text string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if doc, ok := ds.docs[uri]; ok {
		doc.tree.retireLocked()
	}
	// Editor took ownership of this URI - drop any LRU tracking for it.
	ds.removeFromLRULocked(uri)
	ds.docs[uri] = &cachedDoc{text: text}
}

func (ds *DocumentStore) Close(uri string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if doc, ok := ds.docs[uri]; ok {
		doc.tree.retireLocked()
	}
	ds.removeFromLRULocked(uri)
	delete(ds.docs, uri)
}

// CloseAll frees all cached trees and the shared parser. Trees still
// referenced by in-flight handlers stay alive until released; the parser
// itself is safe to close immediately because parse trees are independent
// of the parser once produced.
func (ds *DocumentStore) CloseAll() {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	for _, doc := range ds.docs {
		doc.tree.retireLocked()
	}
	ds.docs = nil
	ds.transientList = nil
	ds.transientIdx = nil
	ds.parser.Close()
}

func (ds *DocumentStore) Get(uri string) (string, bool) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	doc, ok := ds.docs[uri]
	if !ok {
		return "", false
	}
	return doc.text, true
}

// GetIfOpen returns the text for the given URI, but only if the entry is
// editor-owned (non-transient) — i.e. the editor sent a didOpen. Returns
// ("", false) for transient entries loaded via GetOrLoad and for URIs
// that are not in the store at all. This is an atomic single-lock check
// distinct from calling HasOpen followed by Get, which would be a
// TOCTOU race if Close interleaves between the two RLock acquisitions.
func (ds *DocumentStore) GetIfOpen(uri string) (string, bool) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	doc, ok := ds.docs[uri]
	if !ok || doc.transient {
		return "", false
	}
	return doc.text, true
}

// GetOrLoad returns the text for the given URI, falling back to a disk
// read if no editor has opened the document. Disk-loaded entries are
// marked transient and tracked in an LRU; if the transient population
// exceeds the cap, the least-recently-used transient entry is evicted.
//
// Returns ("", false) if the URI does not resolve to a readable file on
// disk (e.g. non-file:// URIs, missing files, permission errors).
//
// Editor-owned entries (added via Set) are never evicted and are not
// reordered in the LRU - only transient entries participate.
func (ds *DocumentStore) GetOrLoad(uri string) (string, bool) {
	// Fast path: lookup under RLock. We avoid the LRU bump here so
	// repeated hits on editor-owned buffers don't contend on the write
	// lock at all.
	ds.mu.RLock()
	if doc, ok := ds.docs[uri]; ok {
		text := doc.text
		isTransient := doc.transient
		ds.mu.RUnlock()
		if isTransient {
			ds.bumpLRU(uri)
		}
		return text, true
	}
	ds.mu.RUnlock()

	// Miss: read from disk *outside* the write lock so concurrent
	// requests for other URIs aren't blocked behind file I/O. We only
	// fall back to disk for file:// URIs - uri.Filename() panics on
	// other schemes (e.g. untitled:), so guard before calling it.
	if !strings.HasPrefix(uri, "file://") {
		return "", false
	}
	path := uriToPath(protocol.DocumentURI(uri))
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	text := string(data)

	ds.mu.Lock()
	defer ds.mu.Unlock()

	// Re-check: another goroutine may have populated this URI (via Set
	// or a concurrent GetOrLoad) while we were reading from disk. If so,
	// prefer the existing entry - Set wins by definition; a concurrent
	// transient load is equivalent to ours. Bump the LRU on the way out
	// so this access registers as recency-of-use, matching the fast-path
	// behavior above; without this, racing slow-path callers wouldn't
	// keep the entry warm even though they just used it.
	if existing, ok := ds.docs[uri]; ok {
		if existing.transient {
			if elem, ok := ds.transientIdx[uri]; ok {
				ds.transientList.MoveToFront(elem)
			}
		}
		return existing.text, true
	}

	ds.docs[uri] = &cachedDoc{text: text, transient: true}
	ds.transientIdx[uri] = ds.transientList.PushFront(uri)
	ds.evictTransientLocked()
	return text, true
}

// bumpLRU moves a transient URI to the front of the LRU list. Called on
// every hit against a transient entry so the eviction order tracks
// recency-of-use rather than recency-of-load.
func (ds *DocumentStore) bumpLRU(uri string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if elem, ok := ds.transientIdx[uri]; ok {
		ds.transientList.MoveToFront(elem)
	}
}

// removeFromLRULocked removes a URI from LRU tracking. Caller must hold
// the write lock. Safe to call for URIs that aren't tracked.
func (ds *DocumentStore) removeFromLRULocked(uri string) {
	if elem, ok := ds.transientIdx[uri]; ok {
		ds.transientList.Remove(elem)
		delete(ds.transientIdx, uri)
	}
}

// evictTransientLocked drops the least-recently-used transient entry
// while the transient population exceeds the cap. Caller must hold the
// write lock.
func (ds *DocumentStore) evictTransientLocked() {
	for ds.transientList.Len() > ds.maxTransient {
		elem := ds.transientList.Back()
		if elem == nil {
			return
		}
		victim := elem.Value.(string)
		ds.transientList.Remove(elem)
		delete(ds.transientIdx, victim)
		if doc, ok := ds.docs[victim]; ok {
			doc.tree.retireLocked()
			delete(ds.docs, victim)
		}
	}
}

// GetTree returns a cached tree-sitter parse tree and its source bytes for
// the given URI. Parses on first access and caches the result.
//
// The returned release closure MUST be called exactly once when the caller
// is done with the tree (typically via defer). It increments the tree's
// refcount under the store lock for the duration of the caller's use,
// keeping the underlying C memory alive even if Set/Close/CloseAll or LRU
// eviction concurrently retires the tree. Without this, a concurrent
// GetOrLoad for one URI could evict and free the tree of another URI
// while a handler is still walking it - a use-after-free on C memory that
// the Go race detector cannot catch.
//
// Callers must not close the returned tree directly.
//
// When ok is false, release is nil and must not be called.
func (ds *DocumentStore) GetTree(uri string) (*tree_sitter.Tree, []byte, func(), bool) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	doc, ok := ds.docs[uri]
	if !ok {
		return nil, nil, nil, false
	}
	if doc.tree == nil {
		doc.src = []byte(doc.text)
		doc.tree = &refTree{tree: ds.parser.Parse(doc.src, nil)}
	}
	rt := doc.tree
	rt.refs++
	release := func() {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		rt.refs--
		if rt.refs == 0 && rt.retired && rt.tree != nil {
			rt.tree.Close()
			rt.tree = nil
		}
	}
	return rt.tree, doc.src, release, true
}

// GetTokens returns cached tokenizer output and source bytes for the given URI.
// Tokenizes on first access and caches the result. The cache is invalidated on
// the next Set() call.
func (ds *DocumentStore) GetTokens(uri string) ([]parser.Token, []byte, bool) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	doc, ok := ds.docs[uri]
	if !ok {
		return nil, nil, false
	}
	if doc.tokens == nil {
		doc.tokSrc = []byte(doc.text)
		result := parser.TokenizeFull(doc.tokSrc)
		doc.tokens = result.Tokens
		doc.lineStarts = result.LineStarts
	}
	return doc.tokens, doc.tokSrc, true
}

// GetTokensFull returns cached tokenizer output including line starts for
// efficient (line, col) → byte offset conversion.
func (ds *DocumentStore) GetTokensFull(uri string) ([]parser.Token, []byte, []int, bool) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	doc, ok := ds.docs[uri]
	if !ok {
		return nil, nil, nil, false
	}
	if doc.tokens == nil {
		doc.tokSrc = []byte(doc.text)
		result := parser.TokenizeFull(doc.tokSrc)
		doc.tokens = result.Tokens
		doc.lineStarts = result.LineStarts
	}
	return doc.tokens, doc.tokSrc, doc.lineStarts, true
}

// GetTokenizedFile returns a cached TokenizedFile for the given URI, or nil
// if the document is not tracked. This is the preferred way to get a
// TokenizedFile from the document store.
func (ds *DocumentStore) GetTokenizedFile(uri string) *TokenizedFile {
	tokens, src, lineStarts, ok := ds.GetTokensFull(uri)
	if !ok {
		return nil
	}
	return NewTokenizedFileFromCache(tokens, src, lineStarts)
}
