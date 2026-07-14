package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const accountsSource = `defmodule MyApp.Accounts do
  @moduledoc """
  The accounts context.
  """

  @doc """
  Fetches a user by id.
  """
  @spec fetch_user(integer()) :: {:ok, map()} | {:error, :not_found}
  def fetch_user(id) do
    {:ok, %{id: id}}
  end

  def list_users(opts) do
    opts
  end

  defp validate(id), do: id

  defdelegate create_user(attrs), to: MyApp.Accounts.Creator, as: :create

  @type user_id :: integer()
end
`

const creatorSource = `defmodule MyApp.Accounts.Creator do
  def create(attrs) do
    attrs
  end
end
`

const workerSource = `defmodule MyApp.Worker do
  def run do
    MyApp.Accounts.fetch_user(1)
  end

  def run_all do
    MyApp.Accounts.list_users([])
    MyApp.Accounts.fetch_user(2)
  end
end
`

func setupProject(t *testing.T) *testEnv {
	t.Helper()
	e := setupTestEnv(t)
	e.indexFile("mix.exs", "defmodule MyApp.MixProject do\nend\n")
	e.indexFile("lib/my_app/accounts.ex", accountsSource)
	e.indexFile("lib/my_app/accounts/creator.ex", creatorSource)
	e.indexFile("lib/my_app/worker.ex", workerSource)
	return e
}

func TestWorkspaceTool(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_workspace", nil)
	wantContains(t, out,
		"Project root: "+e.root,
		"mix.exs",
		"definitions",
		"references",
	)
}

func TestSearchTool(t *testing.T) {
	e := setupProject(t)

	out := e.callTool("dexter_search", map[string]any{"query": "fetch_user"})
	wantContains(t, out, "MyApp.Accounts.fetch_user/1", "lib/my_app/accounts.ex")

	out = e.callTool("dexter_search", map[string]any{"query": "zzz_nothing_matches"})
	wantContains(t, out, "No symbols matched")

	errText := e.callToolExpectError("dexter_search", map[string]any{"query": "  "})
	wantContains(t, errText, "query must not be empty")
}

func TestDefinitionTool_Function(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_definition", map[string]any{"module": "MyApp.Accounts", "function": "fetch_user"})
	wantContains(t, out,
		"MyApp.Accounts.fetch_user/1 (def)",
		"lib/my_app/accounts.ex:10",
		"@spec fetch_user(integer())",
		"Fetches a user by id.",
		"def fetch_user(id) do",
	)
}

func TestDefinitionTool_FollowsDelegate(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_definition", map[string]any{"module": "MyApp.Accounts", "function": "create_user"})
	wantContains(t, out,
		"(defdelegate)",
		"Delegates to MyApp.Accounts.Creator.create",
		"lib/my_app/accounts/creator.ex",
	)
}

func TestDefinitionTool_Module(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_definition", map[string]any{"module": "MyApp.Accounts"})
	wantContains(t, out,
		"defmodule MyApp.Accounts - lib/my_app/accounts.ex:1",
		"The accounts context.",
	)
}

func TestDefinitionTool_NotFound(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_definition", map[string]any{"module": "MyApp.Missing"})
	wantContains(t, out, "not in the index")
}

func TestReferencesTool(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_references", map[string]any{"module": "MyApp.Accounts", "function": "fetch_user"})
	wantContains(t, out,
		"reference(s) to MyApp.Accounts.fetch_user",
		"lib/my_app/worker.ex",
		"MyApp.Accounts.fetch_user(1)",
		"MyApp.Accounts.fetch_user(2)",
	)
}

func TestReferencesTool_DelegateFacade(t *testing.T) {
	e := setupProject(t)
	// Calls to the facade MyApp.Accounts.create_user should count as
	// references to the delegate target Creator.create.
	e.indexFile("lib/my_app/caller.ex", `defmodule MyApp.Caller do
  def go(attrs) do
    MyApp.Accounts.create_user(attrs)
  end
end
`)
	out := e.callTool("dexter_references", map[string]any{"module": "MyApp.Accounts.Creator", "function": "create"})
	wantContains(t, out, "lib/my_app/caller.ex")
}

func TestReferencesTool_Truncation(t *testing.T) {
	e := setupProject(t)
	var b strings.Builder
	b.WriteString("defmodule MyApp.Spammy do\n  def go do\n")
	for i := 0; i < maxReferenceLines+20; i++ {
		fmt.Fprintf(&b, "    MyApp.Accounts.list_users(%d)\n", i)
	}
	b.WriteString("  end\nend\n")
	e.indexFile("lib/my_app/spammy.ex", b.String())

	out := e.callTool("dexter_references", map[string]any{"module": "MyApp.Accounts", "function": "list_users"})
	wantContains(t, out, "more reference(s) not shown")
}

func TestModuleAPITool(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_module_api", map[string]any{"module": "MyApp.Accounts"})
	wantContains(t, out,
		"module MyApp.Accounts - lib/my_app/accounts.ex:1",
		"The accounts context.",
		"Functions:",
		"fetch_user(id)",
		"Fetches a user by id.",
		"Delegates:",
		"create_user(attrs)",
		"→ MyApp.Accounts.Creator.create",
		"Types:",
		"user_id/0",
		"Submodules",
		"Creator",
	)
	wantNotContains(t, out, "validate")

	out = e.callTool("dexter_module_api", map[string]any{"module": "MyApp.Accounts", "include_private": true})
	wantContains(t, out, "validate")
}

func TestFileOutlineTool(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_file_outline", map[string]any{"file": "lib/my_app/accounts.ex"})
	wantContains(t, out,
		"defmodule MyApp.Accounts (line 1)",
		"def fetch_user/1",
		"defp validate/1",
		"defdelegate create_user/1",
		"→ MyApp.Accounts.Creator.create",
		"@type user_id/0",
	)

	out = e.callTool("dexter_file_outline", map[string]any{"file": "lib/nope.ex"})
	wantContains(t, out, "File not found")
}

func TestFileOutlineTool_NestedModules(t *testing.T) {
	e := setupProject(t)
	e.indexFile("lib/my_app/outer.ex", `defmodule MyApp.Outer do
  def outer_fun, do: :ok

  defmodule Inner do
    def inner_fun, do: :ok
  end
end
`)
	out := e.callTool("dexter_file_outline", map[string]any{"file": "lib/my_app/outer.ex"})
	wantContains(t, out,
		"defmodule MyApp.Outer (line 1)",
		"defmodule MyApp.Outer.Inner (line 4)",
		"def inner_fun/0",
	)
}

func TestImplementationsTool_Behaviour(t *testing.T) {
	e := setupProject(t)
	e.indexFile("lib/my_app/notifier.ex", `defmodule MyApp.Notifier do
  @callback deliver(map()) :: :ok | {:error, term()}
end
`)
	e.indexFile("lib/my_app/email_notifier.ex", `defmodule MyApp.EmailNotifier do
  @behaviour MyApp.Notifier

  @impl true
  def deliver(msg) do
    :ok
  end
end
`)
	out := e.callTool("dexter_implementations", map[string]any{"module": "MyApp.Notifier"})
	wantContains(t, out,
		"Modules implementing behaviour MyApp.Notifier",
		"MyApp.EmailNotifier",
		"@callback deliver/1",
	)

	out = e.callTool("dexter_implementations", map[string]any{"module": "MyApp.Notifier", "function": "deliver"})
	wantContains(t, out,
		"Implementations of callback MyApp.Notifier.deliver",
		"MyApp.EmailNotifier.deliver/1",
		"lib/my_app/email_notifier.ex",
	)
}

func TestImplementationsTool_Protocol(t *testing.T) {
	e := setupProject(t)
	e.indexFile("lib/my_app/size.ex", `defprotocol MyApp.Size do
  def size(data)
end
`)
	e.indexFile("lib/my_app/size_impls.ex", `defimpl MyApp.Size, for: BitString do
  def size(binary), do: byte_size(binary)
end

defimpl MyApp.Size, for: Map do
  def size(map), do: map_size(map)
end
`)
	out := e.callTool("dexter_implementations", map[string]any{"module": "MyApp.Size"})
	wantContains(t, out,
		"MyApp.Size is a protocol",
		"Implementations (2)",
		"lib/my_app/size_impls.ex:1",
		"lib/my_app/size_impls.ex:5",
	)
}

func TestCallHierarchyTool(t *testing.T) {
	e := setupProject(t)
	out := e.callTool("dexter_call_hierarchy", map[string]any{"module": "MyApp.Accounts", "function": "fetch_user"})
	wantContains(t, out,
		"Call hierarchy for MyApp.Accounts.fetch_user",
		"Incoming (callers)",
		"MyApp.Worker.run/0",
		"lib/my_app/worker.ex",
	)

	out = e.callTool("dexter_call_hierarchy", map[string]any{"module": "MyApp.Worker", "function": "run", "direction": "outgoing"})
	wantContains(t, out, "Outgoing (callees)", "MyApp.Accounts.fetch_user")
	wantNotContains(t, out, "Incoming")

	errText := e.callToolExpectError("dexter_call_hierarchy", map[string]any{"module": "MyApp.Worker", "function": "run", "direction": "sideways"})
	wantContains(t, errText, "direction must be")
}

func TestReindexTool(t *testing.T) {
	e := setupProject(t)

	// Write a new file WITHOUT indexing it: the tool must pick it up.
	path := filepath.Join(e.root, "lib/my_app/fresh.ex")
	if err := os.WriteFile(path, []byte("defmodule MyApp.Fresh do\n  def new_fun, do: :ok\nend\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := e.callTool("dexter_reindex", nil)
	wantContains(t, out, "Reindexed 1 file(s)")

	out = e.callTool("dexter_search", map[string]any{"query": "new_fun"})
	wantContains(t, out, "MyApp.Fresh.new_fun/0")
}
