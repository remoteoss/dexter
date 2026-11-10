package lsp

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// Go to implementation on a protocol lists its defimpl blocks and its
// derives; on a protocol function, the function in each defimpl and the
// derives, whose implementation the protocol generates.
func TestImplementation_Protocol(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	protoSrc := `defprotocol MyApp.Size do
  def size(data)
end

defimpl MyApp.Size, for: Map do
  def size(map), do: map_size(map)
end
`
	indexFile(t, server.store, root, "lib/size.ex", protoSrc)
	indexFile(t, server.store, root, "lib/point.ex", `defmodule MyApp.Point do
  @derive [MyApp.Size]
  defstruct [:x]
end
`)
	path := filepath.Join(root, "lib/size.ex")
	docURI := string(uri.File(path))
	server.docs.Set(docURI, protoSrc)
	at := func(line, col uint32) []string {
		locs, err := server.Implementation(context.Background(), &protocol.ImplementationParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
				Position:     protocol.Position{Line: line, Character: col},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, l := range locs {
			got = append(got, filepath.Base(l.URI.Filename())+":"+strconv.Itoa(int(l.Range.Start.Line)+1))
		}
		sort.Strings(got)
		return got
	}
	if got := strings.Join(at(0, 18), " "); got != "point.ex:2 size.ex:5" {
		t.Errorf("on the protocol: %s", got)
	}
	if got := strings.Join(at(1, 7), " "); got != "point.ex:2 size.ex:6" {
		t.Errorf("on the protocol function: %s", got)
	}
}

// Derives that a `use` brings in: from the body of a dispatch atom
// (`use MyAppWeb, :schema`, decided by the atom of each use), from an inline
// `if ..., do: @derive(...)` (decided by the options, so unsure), and not
// from a `use` inside a quote of a macro that is not __using__ (that macro
// is not a module that derives).
func TestProtocolImplementations_DerivesThroughUse(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()
	root := server.projectRoot
	for path, src := range map[string]string{
		"lib/enc.ex": "defprotocol MyApp.Enc do\n  def enc(x)\nend\n",
		"lib/web.ex": `defmodule MyAppWeb do
  def schema do
    quote do
      @derive MyApp.Enc
      defstruct [:a]
    end
  end

  def controller do
    quote do
      import MyApp.Enc
    end
  end

  defmacro __using__(which) when is_atom(which) do
    apply(__MODULE__, which, [])
  end
end
`,
		"lib/thing.ex": "defmodule MyApp.Thing do\n  use MyAppWeb, :schema\nend\n",
		"lib/ctrl.ex":  "defmodule MyApp.Ctrl do\n  use MyAppWeb, :controller\nend\n",
		"lib/opt.ex": `defmodule MyApp.Opt do
  defmacro __using__(opts) do
    json = Keyword.get(opts, :json, false)

    quote do
      if unquote(json), do: @derive(MyApp.Enc)
      defstruct [:a]
    end
  end
end
`,
		"lib/user.ex": "defmodule MyApp.User do\n  use MyApp.Opt, json: true\nend\n",
		"lib/base.ex": `defmodule MyApp.Base do
  defmacro __using__(_) do
    quote do
      @derive MyApp.Enc
      defstruct [:b]
    end
  end
end
`,
		"lib/macros.ex": `defmodule MyApp.Macros do
  defmacro my_schema do
    quote do
      use MyApp.Base
    end
  end
end
`,
		"lib/plain.ex": "defmodule MyApp.Plain do\n  use MyApp.Base\nend\n",
	} {
		indexFile(t, server.store, root, path, src)
	}
	impls, ok := server.ProtocolImplementations("MyApp.Enc")
	if !ok {
		t.Fatal("MyApp.Enc is not a protocol")
	}
	got := map[string]ProtocolImpl{}
	for _, impl := range impls {
		got[impl.Module] = impl
	}
	if impl, ok := got["MyApp.Thing"]; !ok || impl.Unsure {
		t.Errorf("MyApp.Thing (use MyAppWeb, :schema) must derive for sure, got %+v (all %v)", impl, impls)
	}
	if _, ok := got["MyApp.Ctrl"]; ok {
		t.Errorf("MyApp.Ctrl uses the :controller body, which does not derive: %v", impls)
	}
	if impl, ok := got["MyApp.User"]; !ok || !impl.Unsure || !impl.Conditional {
		t.Errorf("MyApp.User derives only when the option says so: want unsure and conditional, got %+v", impl)
	}
	if impl, ok := got["MyApp.Plain"]; !ok || impl.Unsure {
		t.Errorf("MyApp.Plain must derive for sure, got %+v", impl)
	}
	if _, ok := got["MyApp.Macros"]; ok {
		t.Errorf("MyApp.Macros only quotes a use for its callers: %v", impls)
	}
}
