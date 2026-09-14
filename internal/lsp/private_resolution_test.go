package lsp

import (
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// A bare call cannot name another module's private function, so cross-module
// resolution must not match one. Kernel is where this bites: it has a private
// `defp define/4`, and because Kernel is treated as always in scope, every bare
// `define` inside an Ash code_interface block used to jump into the Elixir
// standard library.
func TestDefinitionBareNameIgnoresKernelPrivateFunction(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/kernel.ex", `defmodule Kernel do
  defp helper_thing(a, b, c, d), do: {a, b, c, d}

  def public_thing(a), do: a
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/caller.ex", `defmodule MyApp.Caller do
  def go do
    helper_thing(1)
    public_thing(1)
  end
end
`)
	callerURI := string(uri.File(filepath.Join(server.projectRoot, "lib", "caller.ex")))

	if locs := definitionAt(t, server, callerURI, 2, 6); len(locs) != 0 {
		t.Errorf("a private Kernel function is not in scope, got %v", locationStrings(locs))
	}
	// The Kernel fallback itself must keep working for public definitions.
	locs := definitionAt(t, server, callerURI, 3, 6)
	if len(locs) != 1 || !strings.HasSuffix(string(locs[0].URI), "kernel.ex") {
		t.Errorf("expected the public Kernel definition, got %v", locationStrings(locs))
	}
}

// `import` brings in public functions and macros only, so a private definition in
// an imported module must not capture a bare name either.
func TestDefinitionBareNameIgnoresImportedPrivateFunction(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/shared_lib/helpers.ex", `defmodule SharedLib.Helpers do
  defp secret(value), do: value

  def open(value), do: value
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/importer.ex", `defmodule MyApp.Importer do
  import SharedLib.Helpers

  def go do
    secret(1)
    open(1)
  end
end
`)
	callerURI := string(uri.File(filepath.Join(server.projectRoot, "lib", "importer.ex")))

	if locs := definitionAt(t, server, callerURI, 4, 6); len(locs) != 0 {
		t.Errorf("an imported module's private function is not in scope, got %v", locationStrings(locs))
	}
	locs := definitionAt(t, server, callerURI, 5, 6)
	if len(locs) != 1 || !strings.HasSuffix(string(locs[0].URI), "helpers.ex") {
		t.Errorf("expected the imported public definition, got %v", locationStrings(locs))
	}
}

// Use-chain lookup has a delegate-following path separate from module
// resolution. It must apply the same public allowlist to imports found inside a
// __using__ body.
func TestLookupInUsingEntryIgnoresImportedPrivateFunction(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/shared_lib/helpers.ex", `defmodule SharedLib.Helpers do
  defp secret(value), do: value
  def open(value), do: value
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/shared_lib/injector.ex", `defmodule SharedLib.Injector do
  defmacro __using__(_opts) do
    quote do
      import SharedLib.Helpers
    end
  end
end
`)

	if results := server.lookupInUsingEntry("SharedLib.Injector", "secret", nil, map[string]bool{}); len(results) != 0 {
		t.Errorf("a private function imported by __using__ is not in scope, got %+v", results)
	}
	if results := server.lookupInUsingEntry("SharedLib.Injector", "open", nil, map[string]bool{}); len(results) != 1 {
		t.Errorf("expected the public function imported by __using__, got %+v", results)
	}
}

func locationStrings(locs []protocol.Location) []string {
	out := make([]string, 0, len(locs))
	for _, loc := range locs {
		out = append(out, string(loc.URI))
	}
	return out
}
