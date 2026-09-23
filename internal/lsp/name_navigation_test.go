package lsp

import (
	"path/filepath"
	"testing"
)

func TestLookupNameFollowsUseChain(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	provider := `defmodule SharedLib.Provider do
  defmacro __using__(_) do
    quote do
      def injected(value), do: value
    end
  end
end`
	consumer := `defmodule MyApp.Consumer do
  use SharedLib.Provider
end`
	providerPath := filepath.Join(server.projectRoot, "lib/provider.ex")
	indexFile(t, server.store, server.projectRoot, "lib/provider.ex", provider)
	indexFile(t, server.store, server.projectRoot, "lib/consumer.ex", consumer)

	results, err := server.LookupName("MyApp.Consumer", "injected", NameLookupOptions{
		FollowDelegates: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].FilePath != providerPath {
		t.Fatalf("use-chain lookup = %#v, want %s", results, providerPath)
	}
}

func TestReferenceNamesFollowsTransitiveUseChain(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	provider := `defmodule SharedLib.Provider do
  defmacro injected(value), do: value
end`
	injector := `defmodule SharedLib.Injector do
  defmacro __using__(_) do
    quote do
      import SharedLib.Provider
    end
  end
end`
	consumer := `defmodule MyApp.Consumer do
  use SharedLib.Injector
  injected(:value)
end`
	indexFile(t, server.store, server.projectRoot, "lib/provider.ex", provider)
	indexFile(t, server.store, server.projectRoot, "lib/injector.ex", injector)
	consumerPath := filepath.Join(server.projectRoot, "lib/consumer.ex")
	indexFile(t, server.store, server.projectRoot, "lib/consumer.ex", consumer)

	results, err := server.ReferenceNames("SharedLib.Provider", "injected", NameReferenceOptions{
		FollowDelegates: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, result := range results {
		if result.FilePath == consumerPath && result.Line == 3 {
			found = true
		}
	}
	if !found {
		t.Fatalf("transitive references = %#v, want %s:3", results, consumerPath)
	}
}

func TestReferenceNamesDeclarationDoesNotFollowDelegate(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	target := `defmodule SharedLib.Target do
  def run(value), do: value
end`
	facade := `defmodule MyApp.Facade do
  defdelegate run(value), to: SharedLib.Target
end`
	indexFile(t, server.store, server.projectRoot, "lib/target.ex", target)
	facadePath := filepath.Join(server.projectRoot, "lib/facade.ex")
	indexFile(t, server.store, server.projectRoot, "lib/facade.ex", facade)

	results, err := server.ReferenceNames("MyApp.Facade", "run", NameReferenceOptions{
		FollowDelegates:    true,
		IncludeDeclaration: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, result := range results {
		if result.IsDeclaration && result.FilePath == facadePath && result.Line == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("delegate declaration = %#v, want %s:2", results, facadePath)
	}
}
