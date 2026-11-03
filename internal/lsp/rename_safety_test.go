package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/store"
)

// These tests cover renames that must not write a file on disk under a buffer
// that an editor has open, and must not leave the index or the project half
// changed.

// sessionsAt opens a store at dir and returns n sessions that share it and
// one IndexCoordinator, as the sessions of one daemon do.
func sessionsAt(t *testing.T, dir string, n int) ([]*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	index := NewIndexCoordinator()
	servers := make([]*Server, n)
	for i := range servers {
		servers[i] = NewServerWithOptions(st, dir, ServerOptions{Index: index, ManageWorkspace: true})
		servers[i].renameFileOpsSupported = true
	}
	t.Cleanup(func() {
		for _, s := range servers {
			s.index.backgroundWork.Wait()
			s.CloseSession()
		}
		_ = st.Close()
	})
	return servers, st
}

// vscodeURI spells a file URI the way VS Code (vscode-uri) does: '@', '+',
// '(' and ')' are percent-encoded. uri.File leaves them alone.
func vscodeURI(path string) string {
	r := strings.NewReplacer("@", "%40", "+", "%2B", "(", "%28", ")", "%29")
	return "file://" + r.Replace(filepath.ToSlash(path))
}

func renameParams(docURI string, line, col uint32, newName string) *protocol.RenameParams {
	return &protocol.RenameParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
			Position:     protocol.Position{Line: line, Character: col},
		},
		NewName: newName,
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const (
	safetyDefA   = "defmodule MyApp.Alpha do\n  def foo(x), do: x\nend\n"
	safetyCallB  = "defmodule MyApp.Beta do\n  def run, do: MyApp.Alpha.foo(1)\nend\n"
	safetyUnsavB = "defmodule MyApp.Beta do\n  def run, do: MyApp.Alpha.foo(1)\n  def unsaved, do: :work\nend\n"
)

func TestDocKey_EquivalentSpellings(t *testing.T) {
	cases := [][2]string{
		{"file:///w/elixir%401.17-app/lib/a.ex", "file:///w/elixir@1.17-app/lib/a.ex"},
		{"file:///w/a%2Bb/%28x%29.ex", "file:///w/a+b/(x).ex"},
		{"file:///w/lib/./a.ex", "file:///w/lib/a.ex"},
		{"file:///w//lib/a.ex", "file:///w/lib/a.ex"},
		{"file:///w/lib/x/../a.ex", "file:///w/lib/a.ex"},
		{"file:///c%3A/w/a.ex", "file:///C:/w/a.ex"},
		{"file:///c:/w/a.ex", "file:///C:/w/a.ex"},
		{"file:///w/my%20app/a.ex", string(uri.File("/w/my app/a.ex"))},
	}
	for _, c := range cases {
		if a, b := docKey(c[0]), docKey(c[1]); a != b {
			t.Errorf("docKey(%q) = %q, docKey(%q) = %q; want the same key", c[0], a, c[1], b)
		}
	}
	// Case folds only on a volume that ignores case.
	if same := docKey("file:///w/Lib/a.ex") == docKey("file:///w/lib/a.ex"); same != foldsCase("/w") {
		t.Errorf("docKey folded case: %v, volume ignores case: %v", same, foldsCase("/w"))
	}
	if got := docKey("untitled:Untitled-1"); got != "untitled:Untitled-1" {
		t.Errorf("docKey(untitled) = %q", got)
	}
	// The fast path must agree with uri.File for a plain path.
	if p := "/w/lib/my_app/a_b-c.ex"; docKey("file://"+p) != string(uri.File(p)) {
		t.Errorf("fast path key differs from uri.File for %s", p)
	}
}

func TestDocumentStore_EncodedURIFoundByPath(t *testing.T) {
	ds := NewDocumentStore()
	defer ds.CloseAll()
	path := "/w/elixir@1.17-app/lib/a.ex"
	raw := vscodeURI(path)
	ds.Set(raw, "buffer")
	if text, ok := ds.GetIfOpen(string(uri.File(path))); !ok || text != "buffer" {
		t.Fatalf("GetIfOpen by uri.File(path) = %q, %v; want the buffer", text, ok)
	}
	if got, ok := ds.EditorURI(string(uri.File(path))); !ok || got != raw {
		t.Fatalf("EditorURI = %q, %v; want the editor's spelling %q", got, ok, raw)
	}
	ds.Close(string(uri.File(path)))
	if _, ok := ds.Get(raw); ok {
		t.Fatal("Close under another spelling left the document open")
	}
}

// P1: an editor that percent-encodes the project path must still get its open
// buffers edited, and no file it has open may be written on disk.
func TestRename_PercentEncodedURIs_EditsOpenBuffers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "elixir@1.17-app")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	servers, st := sessionsAt(t, dir, 1)
	server := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	aPath := filepath.Join(dir, "lib/alpha.ex")
	bPath := filepath.Join(dir, "lib/beta.ex")
	aURI, bURI := vscodeURI(aPath), vscodeURI(bPath)
	server.docs.Set(aURI, safetyDefA)
	server.docs.Set(bURI, safetyUnsavB)

	edit, err := server.RenameEdit(context.Background(), renameParams(aURI, 1, 6, "bar"))
	if err != nil {
		t.Fatal(err)
	}
	server.index.backgroundWork.Wait()

	if got := readFile(t, aPath); got != safetyDefA {
		t.Errorf("alpha.ex was written on disk under the open buffer:\n%s", got)
	}
	if got := readFile(t, bPath); got != safetyCallB {
		t.Errorf("beta.ex was written on disk under the open buffer:\n%s", got)
	}
	if edit == nil {
		t.Fatal("rename returned no edit")
	}
	for _, u := range []string{aURI, bURI} {
		edits := edit.Changes[protocol.DocumentURI(u)]
		if !hasEdit(edits, "bar") {
			t.Errorf("no edit under the editor's spelling %s; changes: %v", u, edit.Changes)
		}
	}
	if len(edit.Changes) != 2 {
		t.Errorf("want edits for exactly 2 documents, got %v", edit.Changes)
	}
}

// shownMessages records window/showMessage. Other client methods are not
// expected.
type shownMessages struct {
	protocol.Client
	mu   sync.Mutex
	msgs []string
}

func (c *shownMessages) ShowMessage(_ context.Context, params *protocol.ShowMessageParams) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, params.Message)
	return nil
}

func (c *shownMessages) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.msgs...)
}

func openIn(t *testing.T, s *Server, path, text string) {
	t.Helper()
	if err := s.DidOpen(context.Background(), &protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{URI: uri.File(path), Text: text}}); err != nil {
		t.Fatal(err)
	}
}

// P2, editor requester: a file that another session has open is never
// written on disk. Its change goes into the requesting editor's
// WorkspaceEdit, as for the requester's own open files, whether the other
// buffer is clean or has unsaved changes.
func TestRename_FileOpenInAnotherSessionGoesIntoEdit(t *testing.T) {
	for name, bufferB := range map[string]string{"clean": safetyCallB, "unsaved": safetyUnsavB} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			servers, st := sessionsAt(t, dir, 2)
			a, b := servers[0], servers[1]
			shownA, shownB := &shownMessages{}, &shownMessages{}
			a.client, b.client = shownA, shownB
			indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
			indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
			aPath := filepath.Join(dir, "lib/alpha.ex")
			bPath := filepath.Join(dir, "lib/beta.ex")
			openIn(t, b, bPath, bufferB)
			openIn(t, a, aPath, safetyDefA)

			edit, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(aPath)), 1, 6, "bar"))
			if err != nil {
				t.Fatal(err)
			}
			a.index.backgroundWork.Wait()
			if got := readFile(t, bPath); got != safetyCallB {
				t.Errorf("beta.ex was written on disk although another editor has it open:\n%s", got)
			}
			// The edit is against the disk text, which the requesting editor
			// opens to apply it.
			if got := bufferAfterEdits(t, edit, bPath, safetyCallB); !strings.Contains(got, "MyApp.Alpha.bar(1)") {
				t.Errorf("A's WorkspaceEdit does not rename beta.ex; edit: %+v", edit)
			}
			// Neither the disk nor A's buffers have the new text of beta.ex,
			// so the index keeps the old rows until A saves it.
			if refs, _ := st.LookupReferences("MyApp.Alpha", "bar"); len(refs) != 0 {
				t.Errorf("beta.ex was indexed from text that is in no buffer and on no disk: %+v", refs)
			}
			if msgs := shownB.all(); len(msgs) != 0 {
				t.Errorf("the other editor was told something: %q", msgs)
			}
			msgs := shownA.all()
			if len(msgs) != 1 || !strings.Contains(msgs[0], filepath.Join("lib", "beta.ex")+" is also open in another editor") || !strings.Contains(msgs[0], "save it here") {
				t.Errorf("want one notice to A that names beta.ex, got %q", msgs)
			}
		})
	}
}

// P2, rule 2: a file that must move and that another editor has open, clean
// or with unsaved changes, stops the rename for every requester. That editor
// would save the file back under its old path.
func TestRenameModule_MoveOfFileOpenElsewhereRefused(t *testing.T) {
	type tc struct {
		bufferB   string
		headless  bool
		openHereA bool // the requester also has alpha.ex open
		unsaved   bool // headless: the unsaved-changes refusal comes first
	}
	cases := map[string]tc{
		"editor, clean":            {bufferB: safetyDefA},
		"editor, unsaved":          {bufferB: safetyDefA + "# unsaved\n"},
		"editor, open in both":     {bufferB: safetyDefA, openHereA: true},
		"headless, clean":          {bufferB: safetyDefA, headless: true},
		"headless, clean, fixeol":  {bufferB: strings.TrimSuffix(safetyDefA, "\n"), headless: true},
		"headless, unsaved buffer": {bufferB: safetyDefA + "# unsaved\n", headless: true, unsaved: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			servers, st := sessionsAt(t, dir, 2)
			a, b := servers[0], servers[1]
			indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
			indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
			aPath := filepath.Join(dir, "lib/alpha.ex")
			bPath := filepath.Join(dir, "lib/beta.ex")
			openIn(t, b, aPath, c.bufferB)
			var err error
			if c.headless {
				_, err = a.RenameModuleHeadless(context.Background(), "MyApp.Alpha", "MyApp.Gamma")
			} else {
				openIn(t, a, bPath, safetyCallB)
				if c.openHereA {
					openIn(t, a, aPath, safetyDefA)
				}
				_, err = a.RenameEdit(context.Background(), renameParams(string(uri.File(bPath)), 1, 22, "Gamma"))
			}
			want := filepath.Join("lib", "alpha.ex") + " is open in another editor; close it there, then rename again"
			if c.unsaved {
				want = filepath.Join("lib", "alpha.ex") + " has unsaved changes in an editor"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want a refusal containing %q, got %v", want, err)
			}
			a.index.backgroundWork.Wait()
			if got := readFile(t, aPath); got != safetyDefA {
				t.Errorf("alpha.ex changed:\n%s", got)
			}
			if got := readFile(t, bPath); got != safetyCallB {
				t.Errorf("beta.ex changed:\n%s", got)
			}
			if _, err := os.Stat(filepath.Join(dir, "lib/gamma.ex")); err == nil {
				t.Error("gamma.ex was created")
			}
		})
	}
}

// Rule 2 does not stop the rename when no other editor has the moving file
// open: the requester moves its own open file, or the server moves a closed
// one, as before.
func TestRenameModule_MoveWithoutOtherEditor(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 2)
	a := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	aPath := filepath.Join(dir, "lib/alpha.ex")
	bPath := filepath.Join(dir, "lib/beta.ex")
	openIn(t, a, aPath, safetyDefA)
	edit, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(aPath)), 0, 17, "Gamma"))
	if err != nil {
		t.Fatal(err)
	}
	a.index.backgroundWork.Wait()
	expectClientRename(t, edit, aPath, filepath.Join(dir, "lib/gamma.ex"))
	if got := readFile(t, bPath); !strings.Contains(got, "MyApp.Gamma.foo(1)") {
		t.Errorf("beta.ex not written:\n%s", got)
	}
}

// P2: the end of a session releases its open files, and the files are
// written on disk again like any closed file.
func TestRename_SessionEndReleasesOpenFiles(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 2)
	a, b := servers[0], servers[1]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	aURI := string(uri.File(filepath.Join(dir, "lib/alpha.ex")))
	bPath := filepath.Join(dir, "lib/beta.ex")
	b.docs.Set(vscodeURI(bPath), safetyUnsavB)
	a.docs.Set(aURI, safetyDefA)
	b.CloseSession()

	edit, err := a.RenameEdit(context.Background(), renameParams(aURI, 1, 6, "bar"))
	if err != nil {
		t.Fatal(err)
	}
	a.index.backgroundWork.Wait()
	if len(collectEdits(edit, bPath)) != 0 {
		t.Errorf("beta.ex is in the edit although no editor has it open")
	}
	if got := readFile(t, bPath); !strings.Contains(got, "MyApp.Alpha.bar(1)") {
		t.Errorf("beta.ex not written on disk after the other session ended:\n%s", got)
	}
}

// Headless requester (MCP, CLI): closed files and files open without unsaved
// changes are written on disk; a file with unsaved changes in an editor stops
// the rename before any change.
func TestRenameHeadless(t *testing.T) {
	setup := func(t *testing.T) (core, editor *Server, st *store.Store, dir string) {
		dir = t.TempDir()
		servers, st := sessionsAt(t, dir, 2)
		indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
		indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
		return servers[0], servers[1], st, dir
	}

	t.Run("closed file is written", func(t *testing.T) {
		core, _, st, dir := setup(t)
		if _, err := core.RenameFunctionHeadless(context.Background(), "MyApp.Alpha", "foo", "bar"); err != nil {
			t.Fatal(err)
		}
		core.index.backgroundWork.Wait()
		if got := readFile(t, filepath.Join(dir, "lib/beta.ex")); !strings.Contains(got, "MyApp.Alpha.bar(1)") {
			t.Errorf("beta.ex not written:\n%s", got)
		}
		if defs, _ := st.LookupFunction("MyApp.Alpha", "bar"); len(defs) == 0 {
			t.Error("the index does not follow the disk")
		}
	})

	t.Run("clean buffer is written on disk", func(t *testing.T) {
		core, editor, st, dir := setup(t)
		bPath := filepath.Join(dir, "lib/beta.ex")
		openIn(t, editor, bPath, safetyCallB)
		indexed, err := core.RenameFunctionHeadless(context.Background(), "MyApp.Alpha", "foo", "bar")
		if err != nil {
			t.Fatal(err)
		}
		<-indexed
		if got := readFile(t, bPath); !strings.Contains(got, "MyApp.Alpha.bar(1)") {
			t.Errorf("beta.ex not written:\n%s", got)
		}
		// The editor's buffer changes when the editor reloads it.
		if text, _ := editor.docs.Get(string(uri.File(bPath))); text != safetyCallB {
			t.Errorf("the editor's buffer was changed by the server:\n%s", text)
		}
		if mtime, ok := st.GetFileMtime(bPath); !ok || mtime == store.BufferMtime {
			t.Errorf("beta.ex rows do not come from the disk (mtime %d)", mtime)
		}
	})

	t.Run("unsaved buffer stops the rename", func(t *testing.T) {
		core, editor, _, dir := setup(t)
		bPath := filepath.Join(dir, "lib/beta.ex")
		openIn(t, editor, bPath, safetyUnsavB)
		_, err := core.RenameFunctionHeadless(context.Background(), "MyApp.Alpha", "foo", "bar")
		if err == nil || !strings.Contains(err.Error(), filepath.Join("lib", "beta.ex")+" has unsaved changes in an editor") {
			t.Fatalf("want an error that names lib/beta.ex, got %v", err)
		}
		core.index.backgroundWork.Wait()
		if got := readFile(t, filepath.Join(dir, "lib/alpha.ex")); got != safetyDefA {
			t.Errorf("alpha.ex changed although the rename stopped:\n%s", got)
		}
		if got := readFile(t, bPath); got != safetyCallB {
			t.Errorf("beta.ex changed although the rename stopped:\n%s", got)
		}
	})

	t.Run("unsaved buffer stops a module rename", func(t *testing.T) {
		core, editor, _, dir := setup(t)
		bPath := filepath.Join(dir, "lib/beta.ex")
		openIn(t, editor, bPath, safetyUnsavB)
		_, err := core.RenameModuleHeadless(context.Background(), "MyApp.Alpha", "MyApp.Gamma")
		if err == nil || !strings.Contains(err.Error(), "unsaved changes") {
			t.Fatalf("want a refusal, got %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "lib/alpha.ex")); err != nil {
			t.Errorf("alpha.ex was moved although the rename stopped: %v", err)
		}
	})
}

// P3: rows indexed from a buffer must not look current once the buffer is
// thrown away.
func TestRename_DiscardedBufferIndexFollowsDisk(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	server := servers[0]
	indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
	indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
	aPath := filepath.Join(dir, "lib/alpha.ex")
	bPath := filepath.Join(dir, "lib/beta.ex")
	aURI, bURI := string(uri.File(aPath)), string(uri.File(bPath))
	ctx := context.Background()
	for u, text := range map[string]string{aURI: safetyDefA, bURI: safetyCallB} {
		if err := server.DidOpen(ctx, &protocol.DidOpenTextDocumentParams{TextDocument: protocol.TextDocumentItem{URI: protocol.DocumentURI(u), Text: text}}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := server.RenameEdit(ctx, renameParams(aURI, 1, 6, "bar")); err != nil {
		t.Fatal(err)
	}
	server.index.backgroundWork.Wait()
	if defs, _ := st.LookupFunction("MyApp.Alpha", "bar"); len(defs) == 0 {
		t.Fatal("the index does not have the renamed buffer text")
	}

	// The user undoes the rename in alpha.ex without saving, and closes
	// beta.ex without saving. Neither file changed on disk.
	if err := server.DidClose(ctx, &protocol.DidCloseTextDocumentParams{TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(bURI)}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		refs, _ := st.LookupReferences("MyApp.Alpha", "foo")
		for _, r := range refs {
			if r.FilePath == bPath {
				return true
			}
		}
		return false
	})

	// A reconciliation pass reads alpha.ex from disk again.
	_ = server.ReindexWorkspace()
	if defs, _ := st.LookupFunction("MyApp.Alpha", "foo"); len(defs) == 0 {
		t.Error("after a pass, the index does not have foo, which is on disk")
	}
	if defs, _ := st.LookupFunction("MyApp.Alpha", "bar"); len(defs) != 0 {
		t.Error("after a pass, the index still has bar, which is on no disk")
	}
}

const (
	facadeImpl   = "defmodule MyApp.Impl do\n  def foo(x), do: x\nend\n"
	facadeFile   = "defmodule MyApp.Facade do\n  defdelegate foo(x), to: MyApp.Impl\nend\n"
	facadeCaller = "defmodule MyApp.User do\n  def run, do: MyApp.Impl.foo(1)\nend\n"
)

// P7: a facade that cannot be written stops the rename before any file
// changes.
func TestRename_UnwritableFacadeRefusesRename(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only file")
	}
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	server := servers[0]
	indexFile(t, st, dir, "lib/impl.ex", facadeImpl)
	indexFile(t, st, dir, "lib/facade.ex", facadeFile)
	indexFile(t, st, dir, "lib/user.ex", facadeCaller)
	facadePath := filepath.Join(dir, "lib/facade.ex")
	if err := os.Chmod(facadePath, 0444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(facadePath, 0644) })

	_, err := server.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/impl.ex"))), 1, 6, "bar"))
	if err == nil || !strings.Contains(err.Error(), "cannot write 1 of the files it must change: "+filepath.Join("lib", "facade.ex")+" (permission denied)") {
		t.Fatalf("want an error that names %s, got %v", facadePath, err)
	}
	server.index.backgroundWork.Wait()
	for path, want := range map[string]string{
		filepath.Join(dir, "lib/impl.ex"): facadeImpl,
		facadePath:                        facadeFile,
		filepath.Join(dir, "lib/user.ex"): facadeCaller,
	} {
		if got := readFile(t, path); got != want {
			t.Errorf("%s changed although the rename was refused:\n%s", path, got)
		}
	}
}

// P7: a facade written on disk is indexed again.
func TestRename_FacadeReindexedAfterWrite(t *testing.T) {
	dir := t.TempDir()
	servers, st := sessionsAt(t, dir, 1)
	server := servers[0]
	indexFile(t, st, dir, "lib/impl.ex", facadeImpl)
	indexFile(t, st, dir, "lib/facade.ex", facadeFile)
	facadePath := filepath.Join(dir, "lib/facade.ex")

	if _, err := server.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/impl.ex"))), 1, 6, "bar")); err != nil {
		t.Fatal(err)
	}
	server.index.backgroundWork.Wait()
	if got := readFile(t, facadePath); !strings.Contains(got, "as: :bar") {
		t.Fatalf("facade not updated:\n%s", got)
	}
	dels, err := st.LookupDelegatesTo("MyApp.Impl", "bar")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range dels {
		found = found || d.FilePath == facadePath
	}
	if !found {
		t.Errorf("the index does not know that the facade now delegates to MyApp.Impl.bar: %+v", dels)
	}
}

// A failed index query must stop a module rename: without it, the check for a
// taken destination path is skipped, and a move can overwrite a file.
func TestRenameModule_StoreErrorAborts(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st, dir)
	indexFile(t, st, dir, "lib/old_name.ex", "defmodule MyApp.OldName do\nend\n")
	_ = st.Close() // every later query fails

	if _, err := server.renameModuleEdits(context.Background(), "MyApp.OldName", "MyApp.NewName", requesterEditor, nil); err == nil {
		t.Fatal("a module rename went on although the index could not be queried")
	}
	if _, err := os.Stat(filepath.Join(dir, "lib/old_name.ex")); err != nil {
		t.Errorf("the source file was moved or removed: %v", err)
	}
}
