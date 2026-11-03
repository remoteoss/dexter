package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The use chain answers __schema__ through the import of the wrapper, which
// has a public __schema__ of its own; the compiled Shop.Item has the one that
// its `schema` call generates.
const precedenceWrapperSource = `defmodule Shop.Wrapper do
  defmacro __using__(_opts) do
    quote do
      import Shop.Wrapper

      def new(attrs), do: struct(__MODULE__, attrs)
    end
  end

  def __schema__(:fallback), do: nil

  defmacro schema(source, do: block) do
    quote do
      unquote(block)
      def __schema__(:source), do: unquote(source)
    end
  end
end
`

const precedenceUserSource = `defmodule Shop.Item do
  use Shop.Wrapper

  defstruct [:id]

  schema "items" do
    :ok
  end
end
`

// A function that a `use` injects is answered with its def in the __using__
// quote. When the compiled module records that a later macro call in the
// module declared it, as a schema-style wrapper does for __schema__, that
// call is the answer and the quote's def is extra. A def the `use` itself
// injects (expanded at the `use` line), and any module whose BEAM is not
// loaded, keep the quote's def.
func TestLookupName_InjectedFunctionDeclaredByLaterMacroCall(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a Mix project")
	}
	if _, err := exec.LookPath("mix"); err != nil {
		t.Skip("mix not available")
	}
	server, cleanup := setupTestServer(t)
	t.Cleanup(cleanup)
	root := server.projectRoot
	if err := os.WriteFile(filepath.Join(root, "mix.exs"), []byte(`defmodule Shop.MixProject do
  use Mix.Project
  def project, do: [app: :shop, version: "0.1.0"]
end
`), 0o644); err != nil {
		t.Fatal(err)
	}
	indexFile(t, server.store, root, "lib/shop/wrapper.ex", precedenceWrapperSource)
	indexFile(t, server.store, root, "lib/shop/item.ex", precedenceUserSource)

	where := func(function string) string {
		t.Helper()
		locations, err := server.LookupName("Shop.Item", function, NameLookupOptions{})
		if err != nil || len(locations) == 0 {
			t.Fatalf("LookupName(%s): %v %v", function, locations, err)
		}
		describe := func(l NameLocation) string {
			rel, _ := filepath.Rel(root, l.FilePath)
			return rel + ":" + strconv.Itoa(l.Line)
		}
		out := describe(locations[0])
		for _, also := range locations[0].Also {
			out += " also " + describe(also)
		}
		return out
	}

	// No BEAM: the quote's defs.
	if got := where("__schema__"); got != "lib/shop/wrapper.ex:10" {
		t.Errorf("__schema__ without a BEAM = %s", got)
	}
	if got := where("new"); got != "lib/shop/wrapper.ex:6" {
		t.Errorf("new without a BEAM = %s", got)
	}

	cmd := exec.Command("mix", "compile")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mix compile: %v\n%s", err, out)
	}
	// Compiled: the lookup reads the BEAM itself, so the answer does not
	// depend on whether another request loaded it first.
	if got := where("__schema__"); got != "lib/shop/item.ex:6 also lib/shop/wrapper.ex:10" {
		t.Errorf("__schema__ with the BEAM = %s", got)
	}
	_ = server.ModuleMembers("Shop.Item")
	if got := where("__schema__"); got != "lib/shop/item.ex:6 also lib/shop/wrapper.ex:10" {
		t.Errorf("__schema__ after another request loaded the BEAM = %s", got)
	}
	if got := where("new"); got != "lib/shop/wrapper.ex:6" {
		t.Errorf("new with the BEAM = %s (the use line is where it was expanded, so the quote stays)", got)
	}
	if !strings.Contains(precedenceUserSource, `schema "items" do`) {
		t.Fatal("fixture changed")
	}
}
