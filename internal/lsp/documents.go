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
	tree       *tree_sitter.Tree
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
// evicts any excess immediately. A cap of 0 disables transient caching —
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
	if doc, ok := ds.docs[uri]; ok && doc.tree != nil {
		doc.tree.Close()
	}
	// Editor took ownership of this URI - drop any LRU tracking for it.
	ds.removeFromLRULocked(uri)
	ds.docs[uri] = &cachedDoc{text: text}
}

func (ds *DocumentStore) Close(uri string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if doc, ok := ds.docs[uri]; ok && doc.tree != nil {
		doc.tree.Close()
	}
	ds.removeFromLRULocked(uri)
	delete(ds.docs, uri)
}

// CloseAll frees all cached trees and the shared parser.
func (ds *DocumentStore) CloseAll() {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	for _, doc := range ds.docs {
		if doc.tree != nil {
			doc.tree.Close()
		}
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
	// transient load is equivalent to ours.
	if existing, ok := ds.docs[uri]; ok {
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
			if doc.tree != nil {
				doc.tree.Close()
			}
			delete(ds.docs, victim)
		}
	}
}

// GetTree returns a cached tree-sitter parse tree and its source bytes for
// the given URI. Parses on first access and caches the result. The tree is
// invalidated on the next Set() call. Callers must not close the returned tree.
func (ds *DocumentStore) GetTree(uri string) (*tree_sitter.Tree, []byte, bool) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	doc, ok := ds.docs[uri]
	if !ok {
		return nil, nil, false
	}
	if doc.tree == nil {
		doc.src = []byte(doc.text)
		doc.tree = ds.parser.Parse(doc.src, nil)
	}
	return doc.tree, doc.src, true
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
