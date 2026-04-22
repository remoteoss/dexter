package lsp

import (
	"sync"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_elixir "github.com/tree-sitter/tree-sitter-elixir/bindings/go"

	"github.com/remoteoss/dexter/internal/parser"
)

type cachedDoc struct {
	text       string
	tree       *tree_sitter.Tree
	src        []byte         // source bytes the tree references — must stay alive
	tokens     []parser.Token // cached tokenizer output
	tokSrc     []byte         // source bytes for tokens
	lineStarts []int          // byte offset of each line start (from TokenizeFull)
}

// DocumentStore tracks the text content of open buffers and caches
// tree-sitter parse trees for each document. All access is serialized
// through a single RWMutex: reads (Get) take RLock, writes and parsing
// (Set, Close, GetTree) take Lock.
type DocumentStore struct {
	mu     sync.RWMutex
	docs   map[string]*cachedDoc
	parser *tree_sitter.Parser
}

func NewDocumentStore() *DocumentStore {
	p := tree_sitter.NewParser()
	_ = p.SetLanguage(tree_sitter.NewLanguage(tree_sitter_elixir.Language()))
	return &DocumentStore{
		docs:   make(map[string]*cachedDoc),
		parser: p,
	}
}

func (ds *DocumentStore) Set(uri string, text string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if doc, ok := ds.docs[uri]; ok && doc.tree != nil {
		doc.tree.Close()
	}
	ds.docs[uri] = &cachedDoc{text: text}
}

func (ds *DocumentStore) Close(uri string) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if doc, ok := ds.docs[uri]; ok && doc.tree != nil {
		doc.tree.Close()
	}
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
