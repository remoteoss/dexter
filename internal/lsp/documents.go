package lsp

import (
	"sync"

	tree_sitter "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_elixir "github.com/tree-sitter/tree-sitter-elixir/bindings/go"
)

type cachedDoc struct {
	text string
	tree *tree_sitter.Tree
	src  []byte // source bytes the tree references — must stay alive
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
