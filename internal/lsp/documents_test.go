package lsp

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"go.lsp.dev/uri"
)

func TestDocumentStore_GetOrLoad_DiskFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.ex")
	contents := "defmodule Example do\n  def hello, do: :world\nend\n"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	ds := NewDocumentStore()
	defer ds.CloseAll()

	docURI := string(uri.File(path))

	// Buffer was never opened - Get should miss.
	if _, ok := ds.Get(docURI); ok {
		t.Fatalf("Get returned true for a URI that was never opened")
	}

	// GetOrLoad should populate the store from disk.
	text, ok := ds.GetOrLoad(docURI)
	if !ok {
		t.Fatalf("GetOrLoad returned false for an existing file")
	}
	if text != contents {
		t.Fatalf("GetOrLoad returned wrong text: got %q want %q", text, contents)
	}

	// After loading, Get should also hit.
	if got, ok := ds.Get(docURI); !ok || got != contents {
		t.Fatalf("Get after GetOrLoad: ok=%v text=%q", ok, got)
	}

	// And the entry should be marked transient.
	ds.mu.RLock()
	doc := ds.docs[docURI]
	ds.mu.RUnlock()
	if doc == nil || !doc.transient {
		t.Fatalf("expected disk-loaded entry to be transient, got %+v", doc)
	}
}

func TestDocumentStore_GetOrLoad_MissingFile(t *testing.T) {
	ds := NewDocumentStore()
	defer ds.CloseAll()

	docURI := string(uri.File("/nonexistent/path/does/not/exist.ex"))
	if _, ok := ds.GetOrLoad(docURI); ok {
		t.Fatalf("GetOrLoad returned true for a nonexistent file")
	}
	// And nothing should have been inserted into the store.
	if _, ok := ds.Get(docURI); ok {
		t.Fatalf("missing file should not have been cached")
	}
}

func TestDocumentStore_GetOrLoad_NonFileURI(t *testing.T) {
	ds := NewDocumentStore()
	defer ds.CloseAll()

	// untitled:// and other non-file schemes shouldn't trigger a disk read.
	if _, ok := ds.GetOrLoad("untitled:Untitled-1"); ok {
		t.Fatalf("GetOrLoad returned true for non-file URI")
	}
}

func TestDocumentStore_GetOrLoad_LRUEviction(t *testing.T) {
	dir := t.TempDir()
	ds := NewDocumentStore()
	defer ds.CloseAll()
	ds.maxTransient = 3

	uris := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		path := filepath.Join(dir, "file"+strconv.Itoa(i)+".ex")
		if err := os.WriteFile(path, []byte("defmodule F"+strconv.Itoa(i)+" do\nend\n"), 0644); err != nil {
			t.Fatal(err)
		}
		uris = append(uris, string(uri.File(path)))
	}

	// Load 3 files - fills the cap exactly.
	for i := 0; i < 3; i++ {
		if _, ok := ds.GetOrLoad(uris[i]); !ok {
			t.Fatalf("GetOrLoad(%d) failed", i)
		}
	}
	if got := transientCount(ds); got != 3 {
		t.Fatalf("expected 3 transient entries, got %d", got)
	}

	// Touch file 0 so it becomes most-recently-used.
	if _, ok := ds.GetOrLoad(uris[0]); !ok {
		t.Fatalf("hit on uris[0] failed")
	}

	// Load files 3 and 4. They should evict files 1 and 2 (LRU), not 0.
	for _, i := range []int{3, 4} {
		if _, ok := ds.GetOrLoad(uris[i]); !ok {
			t.Fatalf("GetOrLoad(%d) failed", i)
		}
	}

	if got := transientCount(ds); got != 3 {
		t.Fatalf("expected 3 transient entries after eviction, got %d", got)
	}

	// file 0 should still be present (we bumped it).
	if _, ok := ds.Get(uris[0]); !ok {
		t.Fatalf("uris[0] was incorrectly evicted")
	}
	// files 1 and 2 should be evicted.
	for _, i := range []int{1, 2} {
		if _, ok := ds.Get(uris[i]); ok {
			t.Fatalf("uris[%d] should have been evicted", i)
		}
	}
	// files 3 and 4 are the newest, should remain.
	for _, i := range []int{3, 4} {
		if _, ok := ds.Get(uris[i]); !ok {
			t.Fatalf("uris[%d] should still be present", i)
		}
	}
}

func TestDocumentStore_Set_PromotesTransientToEditorOwned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "promote.ex")
	if err := os.WriteFile(path, []byte("defmodule Promote do\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ds := NewDocumentStore()
	defer ds.CloseAll()
	docURI := string(uri.File(path))

	// Load via disk fallback first.
	if _, ok := ds.GetOrLoad(docURI); !ok {
		t.Fatalf("GetOrLoad failed")
	}
	if transientCount(ds) != 1 {
		t.Fatalf("expected 1 transient entry, got %d", transientCount(ds))
	}

	// Now simulate an editor didOpen with newer content.
	editorText := "defmodule Promote do\n  def edited, do: :ok\nend\n"
	ds.Set(docURI, editorText)

	// The transient bookkeeping should be gone.
	if transientCount(ds) != 0 {
		t.Fatalf("expected 0 transient entries after Set, got %d", transientCount(ds))
	}

	// And the entry should no longer be flagged transient.
	ds.mu.RLock()
	doc := ds.docs[docURI]
	ds.mu.RUnlock()
	if doc == nil {
		t.Fatalf("entry missing after Set")
		return
	}
	if doc.transient {
		t.Fatalf("entry should be editor-owned after Set, still marked transient")
	}
	if doc.text != editorText {
		t.Fatalf("Set did not overwrite text: got %q want %q", doc.text, editorText)
	}
}

func TestDocumentStore_Close_RemovesFromLRU(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "close.ex")
	if err := os.WriteFile(path, []byte("defmodule Close do\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ds := NewDocumentStore()
	defer ds.CloseAll()
	docURI := string(uri.File(path))

	if _, ok := ds.GetOrLoad(docURI); !ok {
		t.Fatalf("GetOrLoad failed")
	}
	if transientCount(ds) != 1 {
		t.Fatalf("expected 1 transient entry, got %d", transientCount(ds))
	}

	ds.Close(docURI)

	if transientCount(ds) != 0 {
		t.Fatalf("expected 0 transient entries after Close, got %d", transientCount(ds))
	}
	if _, ok := ds.Get(docURI); ok {
		t.Fatalf("entry should be gone after Close")
	}
}

func TestDocumentStore_GetOrLoad_EditorOwnedNotEvicted(t *testing.T) {
	dir := t.TempDir()
	ds := NewDocumentStore()
	defer ds.CloseAll()
	ds.maxTransient = 2

	// Editor opens a buffer for "editor.ex".
	editorPath := filepath.Join(dir, "editor.ex")
	if err := os.WriteFile(editorPath, []byte("ignored\n"), 0644); err != nil {
		t.Fatal(err)
	}
	editorURI := string(uri.File(editorPath))
	ds.Set(editorURI, "defmodule Editor do\nend\n")

	// Now load 5 transient files - well over the cap of 2.
	for i := 0; i < 5; i++ {
		path := filepath.Join(dir, "t"+strconv.Itoa(i)+".ex")
		if err := os.WriteFile(path, []byte("defmodule T do\nend\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, ok := ds.GetOrLoad(string(uri.File(path))); !ok {
			t.Fatalf("GetOrLoad t%d failed", i)
		}
	}

	// Editor-owned entry must still be present.
	if _, ok := ds.Get(editorURI); !ok {
		t.Fatalf("editor-owned entry was evicted")
	}
	if transientCount(ds) != 2 {
		t.Fatalf("expected 2 transient entries, got %d", transientCount(ds))
	}
}

func TestDocumentStore_GetTree_DiskLoaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tree.ex")
	contents := "defmodule Tree do\n  def hello, do: :world\nend\n"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	ds := NewDocumentStore()
	defer ds.CloseAll()
	docURI := string(uri.File(path))

	if _, ok := ds.GetOrLoad(docURI); !ok {
		t.Fatalf("GetOrLoad failed")
	}

	tree, src, release, ok := ds.GetTree(docURI)
	if !ok {
		t.Fatalf("GetTree returned ok=false for disk-loaded entry")
	}
	defer release()
	if tree == nil {
		t.Fatalf("GetTree returned nil tree")
		return
	}
	if string(src) != contents {
		t.Fatalf("GetTree src mismatch: got %q want %q", src, contents)
	}
	if tree.RootNode().Kind() != "source" {
		t.Fatalf("expected root node kind 'source', got %q", tree.RootNode().Kind())
	}
}

func TestDocumentStore_GetTokens_DiskLoaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.ex")
	contents := "defmodule Tokens do\nend\n"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	ds := NewDocumentStore()
	defer ds.CloseAll()
	docURI := string(uri.File(path))

	if _, ok := ds.GetOrLoad(docURI); !ok {
		t.Fatalf("GetOrLoad failed")
	}

	tokens, src, lineStarts, ok := ds.GetTokensFull(docURI)
	if !ok {
		t.Fatalf("GetTokensFull returned ok=false for disk-loaded entry")
	}
	if len(tokens) == 0 {
		t.Fatalf("expected non-empty token stream")
	}
	if string(src) != contents {
		t.Fatalf("GetTokensFull src mismatch: got %q want %q", src, contents)
	}
	// "defmodule\n...\nend\n" has 3 newlines, so 4 line starts (incl. line 0).
	if len(lineStarts) < 3 {
		t.Fatalf("expected at least 3 line starts, got %d", len(lineStarts))
	}
}

func TestDocumentStore_GetTree_SetInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invalidate.ex")
	if err := os.WriteFile(path, []byte("defmodule A do\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ds := NewDocumentStore()
	defer ds.CloseAll()
	docURI := string(uri.File(path))

	if _, ok := ds.GetOrLoad(docURI); !ok {
		t.Fatalf("GetOrLoad failed")
	}
	tree1, src1, release1, _ := ds.GetTree(docURI)
	if tree1 == nil {
		t.Fatalf("first GetTree returned nil")
	}
	// Release before Set so the old tree is freed inline (refcount→0) rather
	// than retired-and-deferred; the Set-replaces-tree invariant is the
	// behavior under test, not the deferred-free path.
	release1()

	// Editor opens the buffer with different content - Set must replace the
	// cached tree, otherwise stale parses would leak across the transition
	// from transient to editor-owned.
	editorText := "defmodule B do\n  def x, do: 1\nend\n"
	ds.Set(docURI, editorText)

	tree2, src2, release2, ok := ds.GetTree(docURI)
	if !ok {
		t.Fatalf("GetTree after Set returned ok=false")
	}
	defer release2()
	if tree2 == nil {
		t.Fatalf("GetTree after Set returned nil tree")
	}
	if string(src2) == string(src1) {
		t.Fatalf("expected fresh source after Set, got the original")
	}
	if string(src2) != editorText {
		t.Fatalf("GetTree src mismatch after Set: got %q want %q", src2, editorText)
	}
}

func TestDocumentStore_SetMaxTransient_EvictsImmediately(t *testing.T) {
	dir := t.TempDir()
	ds := NewDocumentStore()
	defer ds.CloseAll()

	for i := 0; i < 4; i++ {
		path := filepath.Join(dir, "f"+strconv.Itoa(i)+".ex")
		if err := os.WriteFile(path, []byte("defmodule F do\nend\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, ok := ds.GetOrLoad(string(uri.File(path))); !ok {
			t.Fatalf("GetOrLoad(%d) failed", i)
		}
	}
	if transientCount(ds) != 4 {
		t.Fatalf("expected 4 transient entries, got %d", transientCount(ds))
	}

	// Lowering the cap should evict the oldest entries immediately.
	ds.SetMaxTransient(2)
	if got := transientCount(ds); got != 2 {
		t.Fatalf("expected 2 transient entries after SetMaxTransient(2), got %d", got)
	}

	// Negative values clamp to 0 and drain everything.
	ds.SetMaxTransient(-1)
	if got := transientCount(ds); got != 0 {
		t.Fatalf("expected 0 transient entries after SetMaxTransient(-1), got %d", got)
	}
}

func TestDocumentStore_GetIfOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "getifopen.ex")
	if err := os.WriteFile(path, []byte("defmodule GetIfOpen do\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ds := NewDocumentStore()
	defer ds.CloseAll()
	docURI := string(uri.File(path))

	// Nothing in the store yet.
	if _, ok := ds.GetIfOpen(docURI); ok {
		t.Fatalf("GetIfOpen returned true for a URI that was never opened or loaded")
	}

	// GetOrLoad adds a transient entry - NOT editor-owned.
	if _, ok := ds.GetOrLoad(docURI); !ok {
		t.Fatalf("GetOrLoad failed")
	}
	if _, ok := ds.GetIfOpen(docURI); ok {
		t.Fatalf("GetIfOpen returned true for a transient (disk-loaded) entry")
	}

	// Set adds an editor-owned entry - IS open.
	editorText := "defmodule GetIfOpen do\n  def open, do: true\nend\n"
	ds.Set(docURI, editorText)
	if text, ok := ds.GetIfOpen(docURI); !ok {
		t.Fatalf("GetIfOpen returned false for an editor-owned entry after Set")
	} else if text != editorText {
		t.Fatalf("GetIfOpen returned wrong text: got %q want %q", text, editorText)
	}

	// Close removes the entry.
	ds.Close(docURI)
	if _, ok := ds.GetIfOpen(docURI); ok {
		t.Fatalf("GetIfOpen returned true after Close")
	}

	// GetOrLoad re-adds it as transient - NOT open.
	if _, ok := ds.GetOrLoad(docURI); !ok {
		t.Fatalf("GetOrLoad after Close failed")
	}
	if _, ok := ds.GetIfOpen(docURI); ok {
		t.Fatalf("GetIfOpen returned true for transient entry after re-load")
	}
}

// transientCount returns the number of transient entries currently
// tracked by the LRU. Used by tests; takes the read lock.
func transientCount(ds *DocumentStore) int {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.transientList.Len()
}

// TestDocumentStore_GetTree_SurvivesEviction is the regression test for
// the use-after-free where cross-URI LRU eviction calls ts_tree_delete on
// a tree another handler is actively walking. With refcounted release,
// the underlying tree must stay alive past eviction until the holder
// calls release().
func TestDocumentStore_GetTree_SurvivesEviction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "victim.ex")
	contents := "defmodule Victim do\n  def hello, do: :world\nend\n"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	ds := NewDocumentStore()
	defer ds.CloseAll()
	docURI := string(uri.File(path))

	if _, ok := ds.GetOrLoad(docURI); !ok {
		t.Fatalf("GetOrLoad failed")
	}

	tree, _, release, ok := ds.GetTree(docURI)
	if !ok {
		t.Fatalf("GetTree returned ok=false")
	}
	if tree == nil {
		t.Fatalf("GetTree returned nil tree")
		return
	}

	// Capture the root node kind so we can re-read it after eviction.
	// Pre-fix, the eviction below would call ts_tree_delete on this tree
	// and the second RootNode() call would read freed C memory.
	rootKindBefore := tree.RootNode().Kind()

	// Force eviction of this URI while we still hold a ref.
	ds.SetMaxTransient(0)
	if got := transientCount(ds); got != 0 {
		t.Fatalf("expected 0 transient entries after SetMaxTransient(0), got %d", got)
	}

	// Walking the tree after eviction must still work - this is the UAF
	// the refcounting prevents.
	rootKindAfter := tree.RootNode().Kind()
	if rootKindAfter != rootKindBefore {
		t.Fatalf("tree root kind changed across eviction: got %q want %q", rootKindAfter, rootKindBefore)
	}
	if rootKindAfter != "source" {
		t.Fatalf("expected root node kind 'source', got %q", rootKindAfter)
	}

	// Release should now actually free the underlying C tree (refcount→0
	// and retired is set). Nothing to assert directly - the absence of a
	// crash on CloseAll later is the implicit check.
	release()
}

// TestDocumentStore_GetTree_ConcurrentEvictionStress runs many goroutines
// that load + walk + release trees against a small transient cap, which
// keeps eviction happening continuously. Designed to catch the UAF race
// when run under -race; pre-fix this reliably crashed on
// ts_tree_delete'd memory.
func TestDocumentStore_GetTree_ConcurrentEvictionStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stress test in -short mode")
	}
	dir := t.TempDir()

	const numURIs = 8
	uris := make([]string, numURIs)
	for i := 0; i < numURIs; i++ {
		path := filepath.Join(dir, "f"+strconv.Itoa(i)+".ex")
		body := "defmodule F" + strconv.Itoa(i) + " do\n  def hello, do: :world\nend\n"
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		uris[i] = string(uri.File(path))
	}

	ds := NewDocumentStore()
	defer ds.CloseAll()
	// Cap well below the working set so every load triggers eviction.
	ds.SetMaxTransient(2)

	const iterations = 200
	const walkers = 4
	var wg sync.WaitGroup
	for w := 0; w < walkers; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				u := uris[(seed+i)%numURIs]
				if _, ok := ds.GetOrLoad(u); !ok {
					continue
				}
				tree, _, release, ok := ds.GetTree(u)
				if !ok {
					continue
				}
				root := tree.RootNode()
				_ = root.Kind()
				_ = root.ChildCount()
				release()
			}
		}(w)
	}
	wg.Wait()
}
