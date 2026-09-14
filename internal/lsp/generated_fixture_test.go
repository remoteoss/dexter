package lsp

import (
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/fixture"
	"github.com/remoteoss/dexter/internal/parser"
)

// Phoenix generates the router's Helpers module at compile time: no source file
// declares PhoenixRoutesWeb.Router.Helpers, so its route helpers exist only in a
// BEAM. This is the real-artifact form of the alias-based synthetic test.
func TestCompletionPhoenixRouteHelpersFromCompiledRouter(t *testing.T) {
	server := newFixtureServer(t)
	indexFixtureFile(t, server, fixture.AppPhoenixRoutes, "lib/phoenix_routes_web/router.ex")

	const probe = `defmodule PhoenixRoutesWeb.Caller do
  alias PhoenixRoutesWeb.Router.Helpers, as: Routes

  def show(conn, id), do: Routes.
end
`
	probeURI := string(uri.File(fixture.Source(t, fixture.AppPhoenixRoutes, "lib/caller.ex")))
	server.docs.Set(probeURI, probe)

	const line = 3
	const typed = "  def show(conn, id), do: Routes."
	items := completionAt(t, server, probeURI, line, uint32(len(typed)))
	if !hasCompletionItem(items, "post_path/3") {
		t.Fatalf("expected the generated route helper post_path/3, got %v", labelsOf(items))
	}
	for _, label := range []string{"post_path/2", "post_path/4", "post_url/3", "static_path/2"} {
		if !hasCompletionItem(items, label) {
			t.Errorf("expected generated helper %s in %v", label, labelsOf(items))
		}
	}
	// One item per arity: a helper offered twice is a dedup regression.
	if got := countItem(items, "post_path/3"); got != 1 {
		t.Errorf("post_path/3 offered %d times", got)
	}
}

// Oban's __using__ injects new/1, new/2, backoff/1 and timeout/1 into every
// worker, while the worker's own perform/1 stays a source definition. Completion
// after the module name lists both sets, and the generated side must contribute
// exactly what the source index has no record of.
func TestCompletionObanWorkerGeneratedFunctions(t *testing.T) {
	server := newFixtureServer(t)
	indexFixtureFile(t, server, fixture.AppObanWorkers, "lib/oban_workers/mailer_worker.ex")

	const prefix = "ObanWorkers.MailerWorker."
	probeURI := string(uri.File(fixture.Source(t, fixture.AppObanWorkers, "lib/caller.ex")))
	server.docs.Set(probeURI, prefix+"\n")

	items := completionAt(t, server, probeURI, 0, uint32(len(prefix)))
	for _, label := range []string{"new/1", "new/2", "backoff/1", "timeout/1"} {
		if !hasCompletionItem(items, label) {
			t.Errorf("expected the Oban-generated %s, got %v", label, labelsOf(items))
		}
	}
	// perform/1 comes from the worker's own source, so it is the index's to offer
	// and the generated delta must not repeat it.
	if got := countItem(items, "perform/1"); got != 1 {
		t.Errorf("perform/1 offered %d times, want exactly the indexed one", got)
	}
}

// countItem returns how many completion items carry this label.
func countItem(items []protocol.CompletionItem, label string) int {
	count := 0
	for _, item := range items {
		if item.Label == label {
			count++
		}
	}
	return count
}

func indexFixtureFile(t *testing.T, server *Server, app, rel string) {
	t.Helper()
	path := fixture.Source(t, app, rel)
	defs, refs, err := parser.ParseFile(path)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if err := server.store.IndexFileWithRefs(path, defs, refs); err != nil {
		t.Fatal(err)
	}
}
