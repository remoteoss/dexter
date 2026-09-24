package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

// The cost of a references query through an injected alias must not grow with
// the files that use the injector without naming the target: on a large
// umbrella nearly every file uses the module that injects `alias MyApp.Repo`,
// and tokenizing each of them once cost gigabytes per query. Only files that
// hold a candidate reference may be read.
func TestReferenceNamesThroughInjectedAliasIgnoresBystanders(t *testing.T) {
	const bystanders = 400
	withoutBystanders := injectedAliasReferenceCost(t, 0)
	withBystanders := injectedAliasReferenceCost(t, bystanders)
	// Each bystander adds its store rows, about a kilobyte, so ~0.5 MB in all.
	// Tokenizing each one adds ~190 KB, ~75 MB in all.
	const budget = 2 << 20
	t.Logf("allocated %d bytes without bystanders, %d with %d", withoutBystanders, withBystanders, bystanders)
	if withBystanders > withoutBystanders+budget {
		t.Fatalf("query allocated %d bytes with %d bystander files and %d without; want growth under %d",
			withBystanders, bystanders, withoutBystanders, budget)
	}
}

func injectedAliasReferenceCost(t *testing.T, bystanders int) uint64 {
	t.Helper()
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/my_app/repo.ex", `defmodule MyApp.Repo do
  defmacro __using__(_) do
    quote do
      alias MyApp.Repo
    end
  end

  def all(queryable), do: queryable
  def insert(changeset), do: changeset
end
`)
	indexFile(t, server.store, server.projectRoot, "lib/my_app/accounts.ex", `defmodule MyApp.Accounts do
  use MyApp.Repo

  def list_users, do: Repo.all(User)
end
`)
	var body strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&body, "  def save_%d(changeset), do: Repo.insert(changeset) |> then(&{:ok, &1, %d})\n", i, i)
	}
	for i := 0; i < bystanders; i++ {
		indexFile(t, server.store, server.projectRoot, fmt.Sprintf("lib/my_app/bystander_%d.ex", i),
			fmt.Sprintf("defmodule MyApp.Bystander%d do\n  use MyApp.Repo\n\n%send\n", i, body.String()))
	}

	accountsPath := filepath.Join(server.projectRoot, "lib/my_app/accounts.ex")
	opts := NameReferenceOptions{FollowDelegates: true, ExcludeStdlib: true}
	query := func() []NameLocation {
		locations, err := server.ReferenceNames("MyApp.Repo", "all", opts)
		if err != nil {
			t.Fatal(err)
		}
		return locations
	}
	// The first query fills the server's lasting caches; the second is the
	// steady-state cost of one query.
	query()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	locations := query()
	runtime.ReadMemStats(&after)

	found := false
	for _, location := range locations {
		if location.FilePath == accountsPath && location.Line == 4 {
			found = true
		}
		if strings.Contains(location.FilePath, "bystander") {
			t.Fatalf("bystander reported as a reference: %+v", location)
		}
	}
	if !found {
		t.Fatalf("Repo.all call in accounts.ex missing from %+v", locations)
	}
	return after.TotalAlloc - before.TotalAlloc
}

// A generated function in a module with source resolves to that module in
// every mode. One in a module that exists only as a BEAM resolves to the
// nearest parent with source, which editors want and a strict lookup must not
// report: the parent is a different module.
func TestLookupNameExactModuleRefusesGeneratedParentFallback(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	indexFile(t, server.store, server.projectRoot, "lib/my_app/router.ex", "defmodule MyApp.Router do\n  use Phoenix.Router\nend\n")
	routerPath := filepath.Join(server.projectRoot, "lib/my_app/router.ex")
	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	if err := os.MkdirAll(ebin, 0o755); err != nil {
		t.Fatal(err)
	}
	for module, export := range map[string]beamExport{
		"MyApp.Router":         {"call", 2},
		"MyApp.Router.Helpers": {"user_path", 2},
	} {
		if err := os.WriteFile(filepath.Join(ebin, "Elixir."+module+".beam"), minimalBeam(export), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		module, function string
		exact            bool
		want             []string
	}{
		{"MyApp.Router", "call", false, []string{routerPath}},
		{"MyApp.Router", "call", true, []string{routerPath}},
		{"MyApp.Router.Helpers", "user_path", false, []string{routerPath}},
		{"MyApp.Router.Helpers", "user_path", true, nil},
	} {
		results, err := server.LookupName(tc.module, tc.function, NameLookupOptions{ExactModule: tc.exact})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, result := range results {
			got = append(got, result.FilePath)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("LookupName(%s.%s, exact=%v) = %v, want %v", tc.module, tc.function, tc.exact, got, tc.want)
		}
	}
}
