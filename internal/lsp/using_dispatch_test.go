package lsp

import (
	"path/filepath"
	"testing"

	"go.lsp.dev/protocol"
)

// entrypointSrc mirrors the Phoenix-style atom dispatch entrypoint:
//
//	defmacro __using__(which) when is_atom(which), do: apply(__MODULE__, which, [])
//
// Each `def <name> do quote do ... end end` is a separate injection target,
// selected by the atom the consumer passes to `use`.
const dispatchEntrypointSrc = `defmodule MyAppWeb do
  def controller do
    quote do
      import MyApp.ControllerHelpers

      def render_page(conn), do: conn
    end
  end

  def view do
    quote do
      import MyApp.ViewHelpers
    end
  end

  def live_view(opts \\ []) do
    quote do
      import MyApp.LiveHelpers
    end
  end

  defmacro __using__(which) when is_atom(which) do
    apply(__MODULE__, which, [])
  end

  defmacro __using__([{which, opts}]) when is_atom(which) do
    apply(__MODULE__, which, [List.wrap(opts)])
  end
end`

const controllerHelpersSrc = `defmodule MyApp.ControllerHelpers do
  def assign_defaults(conn), do: conn
end`

const viewHelpersSrc = `defmodule MyApp.ViewHelpers do
  def format_money(amount), do: amount
end`

const liveHelpersSrc = `defmodule MyApp.LiveHelpers do
  def assign_socket(socket), do: socket
end`

func setupDispatchServer(t *testing.T) (*Server, func()) {
	t.Helper()
	server, cleanup := setupTestServer(t)
	indexFile(t, server.store, server.projectRoot, "lib/my_app_web.ex", dispatchEntrypointSrc)
	indexFile(t, server.store, server.projectRoot, "lib/controller_helpers.ex", controllerHelpersSrc)
	indexFile(t, server.store, server.projectRoot, "lib/view_helpers.ex", viewHelpersSrc)
	indexFile(t, server.store, server.projectRoot, "lib/live_helpers.ex", liveHelpersSrc)
	return server, cleanup
}

// callerDefinition indexes callerSrc and asks for the definition at line/col.
func callerDefinition(t *testing.T, server *Server, callerSrc string, line, col uint32) []protocol.Location {
	t.Helper()
	callerURI := "file://" + filepath.Join(server.projectRoot, "lib/caller.ex")
	server.docs.Set(callerURI, callerSrc)
	return definitionAt(t, server, callerURI, line, col)
}

func TestDispatch_ResolvesImportFromDispatchedBlock(t *testing.T) {
	server, cleanup := setupDispatchServer(t)
	defer cleanup()

	src := `defmodule MyApp.PageController do
  use MyAppWeb, :controller

  def index(conn) do
    assign_defaults(conn)
  end
end`
	// line 4, col 4 is on `assign_defaults`
	if got := callerDefinition(t, server, src, 4, 4); len(got) == 0 {
		t.Fatal("expected assign_defaults to resolve through `use MyAppWeb, :controller`")
	}
}

func TestDispatch_ResolvesInlineDefFromDispatchedBlock(t *testing.T) {
	server, cleanup := setupDispatchServer(t)
	defer cleanup()

	src := `defmodule MyApp.PageController do
  use MyAppWeb, :controller

  def index(conn) do
    render_page(conn)
  end
end`
	if got := callerDefinition(t, server, src, 4, 4); len(got) == 0 {
		t.Fatal("expected render_page (inline def in the dispatched quote block) to resolve")
	}
}

// The anti-fuzziness test: `:controller` must not pick up `:view`'s imports.
func TestDispatch_DoesNotLeakAcrossDispatchTargets(t *testing.T) {
	server, cleanup := setupDispatchServer(t)
	defer cleanup()

	src := `defmodule MyApp.PageController do
  use MyAppWeb, :controller

  def index(conn) do
    format_money(conn)
  end
end`
	if got := callerDefinition(t, server, src, 4, 4); len(got) != 0 {
		t.Fatalf("format_money is imported only by `:view`; `:controller` must not see it, got %d results", len(got))
	}
}

func TestDispatch_KeywordForm(t *testing.T) {
	server, cleanup := setupDispatchServer(t)
	defer cleanup()

	src := `defmodule MyApp.PageLive do
  use MyAppWeb, live_view: :no_sentry_context

  def mount(socket) do
    assign_socket(socket)
  end
end`
	if got := callerDefinition(t, server, src, 4, 4); len(got) == 0 {
		t.Fatal("expected assign_socket to resolve through `use MyAppWeb, live_view: :no_sentry_context`")
	}
}

// Gate: the atom must name a def that exists in the entrypoint module.
func TestDispatch_UnknownAtomResolvesNothing(t *testing.T) {
	server, cleanup := setupDispatchServer(t)
	defer cleanup()

	src := `defmodule MyApp.PageController do
  use MyAppWeb, :nonexistent

  def index(conn) do
    assign_defaults(conn)
  end
end`
	if got := callerDefinition(t, server, src, 4, 4); len(got) != 0 {
		t.Fatalf("`:nonexistent` names no def; expected no results, got %d", len(got))
	}
}

// Gate: apply/3 must target __MODULE__, not some other module.
func TestDispatch_RequiresSelfModuleApply(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	entrypoint := `defmodule MyAppWeb do
  def controller do
    quote do
      import MyApp.ControllerHelpers
    end
  end

  defmacro __using__(which) when is_atom(which) do
    apply(MyApp.Elsewhere, which, [])
  end
end`
	indexFile(t, server.store, server.projectRoot, "lib/my_app_web.ex", entrypoint)
	indexFile(t, server.store, server.projectRoot, "lib/controller_helpers.ex", controllerHelpersSrc)

	src := `defmodule MyApp.PageController do
  use MyAppWeb, :controller

  def index(conn) do
    assign_defaults(conn)
  end
end`
	if got := callerDefinition(t, server, src, 4, 4); len(got) != 0 {
		t.Fatalf("apply/3 targets another module; expected no results, got %d", len(got))
	}
}

// Gate: the dispatched name must come from the __using__ clause's own parameter.
func TestDispatch_RequiresUsingParameterAsName(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	entrypoint := `defmodule MyAppWeb do
  def controller do
    quote do
      import MyApp.ControllerHelpers
    end
  end

  defmacro __using__(which) when is_atom(which) do
    apply(__MODULE__, :something_else, [])
  end
end`
	indexFile(t, server.store, server.projectRoot, "lib/my_app_web.ex", entrypoint)
	indexFile(t, server.store, server.projectRoot, "lib/controller_helpers.ex", controllerHelpersSrc)

	src := `defmodule MyApp.PageController do
  use MyAppWeb, :controller

  def index(conn) do
    assign_defaults(conn)
  end
end`
	if got := callerDefinition(t, server, src, 4, 4); len(got) != 0 {
		t.Fatalf("apply/3 does not dispatch on the __using__ parameter; expected no results, got %d", len(got))
	}
}

// A plain `use Mod` on a dispatch module injects nothing: there is no atom.
func TestDispatch_PlainUseInjectsNothing(t *testing.T) {
	server, cleanup := setupDispatchServer(t)
	defer cleanup()

	src := `defmodule MyApp.PageController do
  use MyAppWeb

  def index(conn) do
    assign_defaults(conn)
  end
end`
	if got := callerDefinition(t, server, src, 4, 4); len(got) != 0 {
		t.Fatalf("`use MyAppWeb` passes no atom; expected no results, got %d", len(got))
	}
}

// References must reach call sites in modules that got the import through a
// dispatch target, not just through a plain __using__ body.
func TestDispatch_ReferencesThroughDispatchedImport(t *testing.T) {
	server, cleanup := setupDispatchServer(t)
	defer cleanup()

	callerSrc := `defmodule MyApp.PageController do
  use MyAppWeb, :controller

  def index(conn) do
    assign_defaults(conn)
  end
end`
	indexFile(t, server.store, server.projectRoot, "lib/page_controller.ex", callerSrc)

	helpersURI := "file://" + filepath.Join(server.projectRoot, "lib/controller_helpers.ex")
	server.docs.Set(helpersURI, controllerHelpersSrc)

	// line 1, col 6 is on `assign_defaults` in its definition
	locs := referencesAt(t, server, helpersURI, 1, 6)
	found := false
	for _, l := range locs {
		if filepath.Base(string(l.URI)) == "page_controller.ex" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the call site in page_controller.ex, got %d locations", len(locs))
	}
}
