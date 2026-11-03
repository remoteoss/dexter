package lsp

import (
	"container/list"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_elixir "github.com/tree-sitter/tree-sitter-elixir/bindings/go"
	"go.lsp.dev/uri"

	"golang.org/x/text/unicode/norm"

	"github.com/remoteoss/dexter/internal/parser"
)

// defaultMaxTransient caps how many disk-loaded buffers may live in the
// store concurrently. Editor-owned buffers (added via Set) are never counted
// against this cap.
const defaultMaxTransient = 50

type cachedDoc struct {
	// uri is the spelling of the document URI that the editor sent. The store
	// files the document under its canonical key (see docKey); edits sent
	// back to the editor use this spelling, because an editor can compare
	// URIs as strings.
	uri        string
	text       string
	tree       *refTree
	src        []byte         // source bytes the tree references - must stay alive
	tokens     []parser.Token // cached tokenizer output
	tokSrc     []byte         // source bytes for tokens
	lineStarts []int          // byte offset of each line start (from TokenizeFull)
	interp     []parser.Token // code tokens inside #{} interpolations
	// transient is true for entries loaded from disk via GetOrLoad - i.e.
	// no editor sent didOpen for this URI. These entries are tracked in an
	// LRU and evicted once the transient cap is reached. Editor-owned
	// entries (created via Set) are never transient and never evicted.
	transient bool
	// seq orders editor-owned entries across every store in the process:
	// the higher one was set later. A frontend without an editor uses it to
	// pick the newest unsaved buffer when several editors hold one file.
	seq uint64
	// dirty is true when the editor changed the buffer after its last open
	// or save, at changedAt. A clean buffer can be older than the disk (an
	// editor that has not reloaded yet), so only a dirty one is unsaved work.
	dirty     bool
	changedAt time.Time
}

// docSeq numbers editor-owned entries; see cachedDoc.seq.
var docSeq atomic.Uint64

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
	// closed is set by CloseAll. A closed store ignores writes instead of
	// re-creating the maps or parsing with the freed parser: one daemon serves
	// every attached session, so a request that arrives after a session's
	// shutdown must not be able to take the process down.
	closed bool

	// LRU bookkeeping for transient (disk-loaded) entries only. The list
	// holds URIs in access-order, newest at the front. transientIdx maps
	// URI → its list element for O(1) move/remove.
	transientList *list.List
	transientIdx  map[string]*list.Element
	maxTransient  int

	// registry, when set, receives every editor-owned open and close, so
	// that a rename in one session can see the files that another session
	// of the same workspace has open. See openDocRegistry.
	registry *openDocRegistry
}

// docKey returns the key the store files a document URI under: the file URI
// that uri.File builds from the path the URI names. Editors spell one file in
// more than one way. VS Code percent-encodes characters such as '@', '+', '('
// and ')' and writes a Windows drive as "c%3A"; uri.File, which builds the URI
// of every path Dexter finds in the index, does not. If the store used the raw
// string, a lookup by path could not find a buffer that the editor has open,
// and a rename would write that file on disk under the open buffer.
//
// The key decodes percent escapes, cleans the path, writes a Windows drive
// letter in upper case, and, on macOS, puts the path in Unicode normalization
// form C (APFS and HFS+ ignore the normalization, and an editor can send the
// decomposed form). Elsewhere two normalizations are two files, so the key
// keeps the bytes. On a volume that ignores case (the macOS default), the
// key also folds case, because the editor and the index can spell a file with
// different case; on a volume that keeps case apart, it does not. The
// returned path is the cleaned path without case folding: the index stores
// paths with their own spelling, so store queries use it. A URI that is not a
// file URI is its own key.
func docKey(u string) string {
	key, _ := docKeyPath(u)
	return key
}

// docKeyPath returns the key of u and the clean file path it names. The path
// is empty for a URI that is not a file URI or that cannot be parsed.
func docKeyPath(u string) (key, path string) {
	const scheme = "file://"
	if !strings.HasPrefix(u, scheme) {
		return u, ""
	}
	if rest := u[len(scheme):]; isCanonicalURIPath(rest) {
		// The usual case: nothing to decode, normalize or clean. uri.File
		// would give the same string back, so skip the parse.
		path = filepath.FromSlash(rest)
		if foldsCase(filepath.Dir(path)) {
			return scheme + strings.ToLower(rest), path
		}
		return u, path
	}
	parsed, err := url.ParseRequestURI(u)
	if err != nil || parsed.Scheme != "file" || parsed.Path == "" {
		return u, ""
	}
	p := parsed.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' && isASCIILetter(p[1]) {
		// "/c:/x" names the Windows path "C:/x".
		p = strings.ToUpper(p[1:2]) + p[2:]
	}
	p = filepath.Clean(filepath.FromSlash(p))
	k := p
	if ignoresNormalization {
		k = norm.NFC.String(p)
	}
	if foldsCase(filepath.Dir(p)) {
		k = strings.ToLower(k)
	}
	return string(uri.File(k)), p
}

// isCanonicalURIPath reports whether the path part of a file URI is already
// what uri.File would produce: absolute, clean, and made only of characters
// that URL path escaping leaves alone.
func isCanonicalURIPath(p string) bool {
	if len(p) < 2 || p[0] != '/' || p[len(p)-1] == '/' {
		return false
	}
	for i := 1; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '~':
		case c == '.':
			// "/./", "/../" and a trailing "/." or "/.." need cleaning. A dot
			// that starts a name (".formatter.exs") takes the slow path too;
			// that is rare and still correct.
			if p[i-1] == '/' {
				return false
			}
		case c == '/':
			if p[i-1] == '/' {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// openDocRegistry records, for one workspace, which sessions have which files
// open. Every session keeps its own document store, so without it a rename in
// one editor cannot know that another editor attached to the same daemon has
// a file open, possibly with unsaved changes, and would write that file on
// disk under the other editor's buffer. Only editor-owned documents (didOpen)
// count; buffers loaded from disk for a query do not.
type openDocRegistry struct {
	mu    sync.Mutex
	paths map[string]map[*DocumentStore]struct{} // document key (docKey) → stores that hold it open
}

func newOpenDocRegistry() *openDocRegistry {
	return &openDocRegistry{paths: make(map[string]map[*DocumentStore]struct{})}
}

func (r *openDocRegistry) open(owner *DocumentStore, key string) {
	if r == nil || key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	owners := r.paths[key]
	if owners == nil {
		owners = make(map[*DocumentStore]struct{}, 1)
		r.paths[key] = owners
	}
	owners[owner] = struct{}{}
}

func (r *openDocRegistry) close(owner *DocumentStore, keys ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range keys {
		owners := r.paths[key]
		delete(owners, owner)
		if len(owners) == 0 {
			delete(r.paths, key)
		}
	}
}

// pathKey is the document key of a file path from the index.
func pathKey(path string) string { return docKey(string(uri.File(path))) }

// heldTexts returns the buffer text of path in every session other than
// owner that has it open. The stores are read after the registry lock is
// released: DocumentStore.Set takes its own lock and then the registry's.
func (r *openDocRegistry) heldTexts(owner *DocumentStore, path string) []string {
	if r == nil {
		return nil
	}
	key := pathKey(path)
	r.mu.Lock()
	var others []*DocumentStore
	for o := range r.paths[key] {
		if o != owner {
			others = append(others, o)
		}
	}
	r.mu.Unlock()
	if len(others) == 0 {
		return nil
	}
	texts := make([]string, 0, len(others))
	for _, o := range others {
		if text, ok := o.GetIfOpen(key); ok {
			texts = append(texts, text)
		}
	}
	return texts
}

// openElsewhere returns the paths, of the given ones, that a session other
// than owner has open.
func (r *openDocRegistry) openElsewhere(owner *DocumentStore, paths []string) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.paths) == 0 {
		return nil
	}
	var out []string
	for _, path := range paths {
		for o := range r.paths[pathKey(path)] {
			if o != owner {
				out = append(out, path)
				break
			}
		}
	}
	return out
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
	if ds.closed {
		return
	}
	ds.maxTransient = n
	ds.evictTransientLocked()
}

// Count reports how many buffers the store currently holds, including
// transient disk-loaded entries. It is diagnostic only; the LRU cap governs
// memory, not this number.
func (ds *DocumentStore) Count() int {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return len(ds.docs)
}

// Set stores the editor's text for a document and makes the document
// editor-owned. rawURI keeps the spelling the editor sent; see EditorURI.
func (ds *DocumentStore) Set(rawURI string, text string) {
	key, path := docKeyPath(rawURI)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.closed {
		return
	}
	doc, ok := ds.docs[key]
	if ok {
		doc.tree.retireLocked()
	}
	if (!ok || doc.transient) && path != "" {
		ds.registry.open(ds, key)
	}
	// Editor took ownership of this URI - drop any LRU tracking for it.
	ds.removeFromLRULocked(key)
	ds.docs[key] = &cachedDoc{uri: rawURI, text: text, seq: docSeq.Add(1)}
}

// SetChanged is Set for an edit in the editor: the buffer has unsaved changes
// until MarkSaved.
func (ds *DocumentStore) SetChanged(rawURI string, text string) {
	key, path := docKeyPath(rawURI)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.closed {
		return
	}
	doc, ok := ds.docs[key]
	if ok {
		doc.tree.retireLocked()
	}
	if (!ok || doc.transient) && path != "" {
		ds.registry.open(ds, key)
	}
	ds.removeFromLRULocked(key)
	ds.docs[key] = &cachedDoc{uri: rawURI, text: text, seq: docSeq.Add(1), dirty: true, changedAt: time.Now()}
}

// MarkSaved records that the editor saved the buffer, so it has no unsaved
// changes.
func (ds *DocumentStore) MarkSaved(uri string) {
	key := docKey(uri)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if doc, ok := ds.docs[key]; ok && !doc.transient {
		doc.dirty = false
	}
}

func (ds *DocumentStore) Close(uri string) {
	key := docKey(uri)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	doc, ok := ds.docs[key]
	if ok {
		doc.tree.retireLocked()
		if !doc.transient {
			ds.registry.close(ds, key)
		}
	}
	ds.removeFromLRULocked(key)
	delete(ds.docs, key)
}

// EditorURI returns the URI spelling the editor used for a document it has
// open, given any spelling of that document's URI. ok is false when the
// editor does not have the document open.
func (ds *DocumentStore) EditorURI(uri string) (string, bool) {
	key := docKey(uri)
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	doc, ok := ds.docs[key]
	if !ok || doc.transient {
		return "", false
	}
	return doc.uri, true
}

// CloseAll frees all cached trees and the shared parser. Trees still
// referenced by in-flight handlers stay alive until released; the parser
// itself is safe to close immediately because parse trees are independent
// of the parser once produced.
// CloseAll frees all cached trees and the shared parser. It is idempotent: the
// store is per session, and a daemon session must be able to close twice (a
// shutdown request followed by a disconnect) without freeing the parser twice.
func (ds *DocumentStore) CloseAll() {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.closed {
		return
	}
	ds.closed = true
	var open []string
	for key, doc := range ds.docs {
		doc.tree.retireLocked()
		if !doc.transient {
			open = append(open, key)
		}
	}
	ds.registry.close(ds, open...)
	ds.docs = nil
	ds.transientList = nil
	ds.transientIdx = nil
	ds.parser.Close()
}

func (ds *DocumentStore) Get(uri string) (string, bool) {
	uri = docKey(uri)
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
	uri = docKey(uri)
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	doc, ok := ds.docs[uri]
	if !ok || doc.transient {
		return "", false
	}
	return doc.text, true
}

// UnsavedBuffer returns an editor-owned entry that has changes the editor has
// not saved, with its sequence number (see cachedDoc.seq) and the time of its
// last change.
func (ds *DocumentStore) UnsavedBuffer(uri string) (text string, seq uint64, changedAt time.Time, ok bool) {
	key := docKey(uri)
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	doc, found := ds.docs[key]
	if !found || doc.transient || !doc.dirty {
		return "", 0, time.Time{}, false
	}
	return doc.text, doc.seq, doc.changedAt, true
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
func (ds *DocumentStore) GetOrLoad(rawURI string) (string, bool) {
	uri, path := docKeyPath(rawURI)
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
	// fall back to disk for file:// URIs; docKeyPath gives no path for
	// other schemes (e.g. untitled:).
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
	if ds.closed {
		return "", false
	}

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

	ds.docs[uri] = &cachedDoc{uri: rawURI, text: text, transient: true}
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
	uri = docKey(uri)
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
	uri = docKey(uri)
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
		doc.interp = result.Interp
	}
	return doc.tokens, doc.tokSrc, true
}

// GetTokensFull returns cached tokenizer output: the token stream, the source
// it points into, the line starts for efficient (line, col) → byte offset
// conversion, and the interpolation stream — the code inside #{}, which the
// main stream keeps folded into one string token.
func (ds *DocumentStore) GetTokensFull(uri string) ([]parser.Token, []byte, []int, []parser.Token, bool) {
	uri = docKey(uri)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	doc, ok := ds.docs[uri]
	if !ok {
		return nil, nil, nil, nil, false
	}
	if doc.tokens == nil {
		doc.tokSrc = []byte(doc.text)
		result := parser.TokenizeFull(doc.tokSrc)
		doc.tokens = result.Tokens
		doc.lineStarts = result.LineStarts
		doc.interp = result.Interp
	}
	return doc.tokens, doc.tokSrc, doc.lineStarts, doc.interp, true
}

// GetTokenizedFile returns a cached TokenizedFile for the given URI, or nil
// if the document is not tracked. This is the preferred way to get a
// TokenizedFile from the document store.
func (ds *DocumentStore) GetTokenizedFile(uri string) *TokenizedFile {
	tokens, src, lineStarts, interp, ok := ds.GetTokensFull(uri)
	if !ok {
		return nil
	}
	return NewTokenizedFileFromCache(tokens, src, lineStarts, interp)
}
