package mcp

import (
	"strings"
	"testing"
)

// useChainFixture is a two-level use chain: a worker uses a base module whose
// __using__ uses an inner module and injects a defdelegate back to itself, and
// a schema module whose __using__ injects a struct.
var useChainFixture = [][2]string{
	{"lib/shared_lib/inner_worker.ex", `defmodule SharedLib.InnerWorker do
  @callback process(map()) :: term()

  defmacro __using__(opts) do
    quote location: :keep do
      @behaviour SharedLib.InnerWorker
      @opts unquote(opts)

      def new(args, opts \\ []) when is_map(args), do: {args, opts, @opts}
      def perform(%{} = job), do: process(job)
    end
  end
end
`},
	{"lib/shared_lib/base_worker.ex", `defmodule SharedLib.BaseWorker do
  defmacro __using__(opts) do
    quote do
      use SharedLib.InnerWorker, unquote(opts)

      defdelegate backoff(job), to: SharedLib.BaseWorker
    end
  end

  def backoff(_job), do: 10
end
`},
	{"lib/my_app/workers/mailer.ex", `defmodule MyApp.Workers.Mailer do
  use SharedLib.BaseWorker, queue: :default

  def enqueue_many(a, b, c, d), do: {a, b, c, d}
  def process(job), do: job
end
`},
	{"lib/shared_lib/schema.ex", `defmodule SharedLib.Schema do
  defmacro __using__(_opts) do
    quote do
      def __schema__(:source), do: "x"
      defstruct [:id]
    end
  end
end
`},
	{"lib/my_app/thing.ex", "defmodule MyApp.Thing do\n  use SharedLib.Schema\nend\n"},
	{"lib/my_app/point.ex", "defmodule MyApp.Point do\n  defstruct [:x, :y]\nend\n"},
}

func setupUseChainProject(t *testing.T) *testEnv {
	t.Helper()
	e := setupTestEnv(t)
	e.indexFile("mix.exs", "defmodule MyApp.MixProject do\nend\n")
	for _, f := range useChainFixture {
		e.indexFile(f[0], f[1])
	}
	return e
}

// The defs in a __using__ quote are injected into the modules that use it.
// They are not functions of the module that holds the quote, so its API must
// not list them, and a use inside the quote is not a use by that module.
func TestModuleAPITool_QuoteDefsAreNotTheMacroModulesOwn(t *testing.T) {
	e := setupUseChainProject(t)

	inner := e.callTool("dexter_module_api", map[string]any{"module": "SharedLib.InnerWorker"})
	wantContains(t, inner, "__using__(opts)", "process/1")
	wantNotContains(t, inner, "new(args", "perform(job)")

	base := e.callTool("dexter_module_api", map[string]any{"module": "SharedLib.BaseWorker"})
	wantContains(t, base, "backoff(_job) [lib/shared_lib/base_worker.ex:10]")
	wantNotContains(t, base, "Delegates:", "injected by", "new(args", "perform(job)")

	def := e.callTool("dexter_definition", map[string]any{"module": "SharedLib.BaseWorker", "function": "backoff"})
	wantContains(t, def, "lib/shared_lib/base_worker.ex:10")
	wantNotContains(t, def, "base_worker.ex:6", "Delegates to")
}

// What the use chain injects keeps the arity of each head, also through a use
// inside a __using__ body, and a struct injected by a __using__ quote gives the
// module __struct__/0 and __struct__/1. All of it comes from source.
func TestModuleAPITool_InjectedArityAndStructsFromSource(t *testing.T) {
	e := setupUseChainProject(t)

	worker := e.callTool("dexter_module_api", map[string]any{"module": "MyApp.Workers.Mailer"})
	wantContains(t, worker,
		"backoff(job) [lib/shared_lib/base_worker.ex:6]",
		"new(args) [lib/shared_lib/inner_worker.ex:9]",
		"new(args, opts) [lib/shared_lib/inner_worker.ex:9]",
		"perform(job) [lib/shared_lib/inner_worker.ex:10]",
	)

	thing := e.callTool("dexter_module_api", map[string]any{"module": "MyApp.Thing"})
	wantContains(t, thing, "__struct__/0 [lib/shared_lib/schema.ex:5]", "__struct__(kv) [lib/shared_lib/schema.ex:5]",
		"injected by `use SharedLib.Schema`")

	point := e.callTool("dexter_module_api", map[string]any{"module": "MyApp.Point"})
	wantContains(t, point, "__struct__/0 [lib/my_app/point.ex:2]", "__struct__(kv) [lib/my_app/point.ex:2]")
	if i := strings.Index(point, "Injected or generated"); i >= 0 {
		t.Errorf("a struct defined in the module's own source is not injected:\n%s", point)
	}

	schema := e.callTool("dexter_module_api", map[string]any{"module": "SharedLib.Schema"})
	wantNotContains(t, schema, "__struct__", "__schema__")
}

// Each reference row names the code that holds it: the function, or the test
// block when there is no def. With callees: true, the answer also lists what
// the function calls, including an injected function and a private helper.
func TestReferencesTool_CallersAndCallees(t *testing.T) {
	e := setupUseChainProject(t)
	e.indexFile("lib/my_app/alpha.ex", "defmodule MyApp.Alpha do\n  def old_name(x), do: x\nend\n")
	e.indexFile("lib/my_app/flow.ex", `defmodule MyApp.Flow do
  alias MyApp.Workers.Mailer

  def run(x) do
    job = Mailer.new(%{x: x})
    helper(MyApp.Alpha.old_name(job))
  end

  defp helper(x), do: x
end
`)
	e.indexFile("test/my_app/alpha_test.exs", `defmodule MyApp.AlphaTest do
  use ExUnit.Case

  def helper, do: :ok

  describe "old_name/1" do
    test "returns its argument" do
      assert MyApp.Alpha.old_name(1) == 1
    end
  end
end
`)

	out := e.callTool("dexter_references", map[string]any{"module": "MyApp.Alpha", "function": "old_name"})
	wantContains(t, out,
		"6: helper(MyApp.Alpha.old_name(job))  [in MyApp.Flow.run/1]",
		`8: assert MyApp.Alpha.old_name(1) == 1  [in MyApp.AlphaTest test "returns its argument"]`,
	)
	wantNotContains(t, out, "MyApp.AlphaTest.helper/0")

	out = e.callTool("dexter_references", map[string]any{"module": "MyApp.Flow", "function": "run", "callees": true})
	wantContains(t, out,
		"MyApp.Workers.Mailer.new - defined at lib/shared_lib/inner_worker.ex:9; called at line 5",
		"MyApp.Alpha.old_name - defined at lib/my_app/alpha.ex:2; called at line 6",
		"MyApp.Flow.helper - defined at lib/my_app/flow.ex:9; called at line 6",
	)

	errText := e.callToolExpectError("dexter_references", map[string]any{"module": "MyApp.Flow", "callees": true})
	wantContains(t, errText, "callees needs a function")

	tools := e.callToolExpectError("dexter_call_hierarchy", map[string]any{"module": "MyApp.Flow", "function": "run"})
	wantContains(t, tools, "dexter_call_hierarchy")
}

// @derive makes the protocol generate an implementation for the module: one
// protocol, a tuple with options, a list, and a @derive in a __using__ quote,
// which derives for each module that uses it. The defimpl in the protocol's
// own __deriving__ quote is code for the caller, not an implementation.
// deriveFixture is a project with a protocol, defimpl blocks, and @derive in
// each form, also through a use chain.
var deriveFixture = [][2]string{
	{"lib/my_app/encoder.ex", `defprotocol MyApp.Encoder do
  def encode(value)
end

defimpl MyApp.Encoder, for: Any do
  defmacro __deriving__(module, _struct, _opts) do
    quote do
      defimpl MyApp.Encoder, for: unquote(module) do
        def encode(_), do: "derived"
      end
    end
  end

  def encode(_), do: "any"
end

defimpl MyApp.Encoder, for: Integer do
  def encode(i), do: to_string(i)
end
`},
	{"lib/my_app/structs.ex", `defmodule MyApp.One do
  @derive MyApp.Encoder
  defstruct [:a]
end

defmodule MyApp.Two do
  alias MyApp.Encoder
  @derive [Inspect, {Encoder, only: [:a]}]
  defstruct [:a, :b]
end

defmodule MyApp.Plain do
  defstruct [:a]
end
`},
	{"lib/my_app/schema.ex", `defmodule MyApp.Schema do
  defmacro __using__(_opts) do
    quote do
      @derive MyApp.Encoder
    end
  end
end

defmodule MyApp.Wrapper do
  defmacro __using__(_opts) do
    quote do
      use MyApp.Schema
    end
  end
end
`},
	{"lib/my_app/three.ex", "defmodule MyApp.Three do\n  use MyApp.Schema\n  defstruct [:a]\nend\n"},
	{"lib/my_app/four.ex", "defmodule MyApp.Four do\n  use MyApp.Wrapper\n  defstruct [:a]\nend\n"},
	{"lib/my_app/no_struct.ex", "defmodule MyApp.NoStruct do\n  use MyApp.Schema\n  def a, do: :a\nend\n"},
	{"lib/my_app/branchy.ex", "defmodule MyApp.Branchy do\n  defmacro __using__(opts) do\n    if Keyword.get(opts, :plain) do\n      quote do\n        import MyApp.Branchy, only: []\n      end\n    else\n      quote do\n        @derive MyApp.Encoder\n      end\n    end\n  end\nend\n\ndefmodule MyApp.PlainWrapper do\n  defmacro __using__(_opts) do\n    quote do\n      use MyApp.Branchy, plain: true\n    end\n  end\nend\n"},
	{"lib/my_app/five.ex", "defmodule MyApp.Five do\n  use MyApp.Branchy\n  defstruct [:a]\nend\n"},
	{"lib/my_app/six.ex", "defmodule MyApp.Six do\n  use MyApp.PlainWrapper\n  defstruct [:a]\nend\n"},
	{"lib/my_app/late.ex", "defmodule MyApp.Late do\n  defstruct [:a]\n  @derive MyApp.Encoder\n  def a, do: :a\nend\n"},
}

func TestImplementationsTool_Derive(t *testing.T) {
	e := setupTestEnv(t)
	e.indexFile("mix.exs", "defmodule MyApp.MixProject do\nend\n")
	for _, f := range deriveFixture {
		e.indexFile(f[0], f[1])
	}

	out := e.callTool("dexter_implementations", map[string]any{"module": "MyApp.Encoder"})
	wantContains(t, out,
		"defimpl implementations (2):",
		"lib/my_app/encoder.ex:5",
		"lib/my_app/encoder.ex:17",
		"Derived with @derive (7;",
		"MyApp.One - lib/my_app/structs.ex:2",
		"MyApp.Two - lib/my_app/structs.ex:8",
		"MyApp.Three - lib/my_app/three.ex:2 (`use MyApp.Schema`, whose __using__ has the @derive)",
		"MyApp.Four - lib/my_app/four.ex:2 (`use MyApp.Wrapper`, whose __using__ has the @derive)",
	)
	wantContains(t, out, "MyApp.NoStruct - lib/my_app/no_struct.ex:2 (`use MyApp.Schema`, whose __using__ has the @derive) (unsure: no struct found")
	wantContains(t, out,
		"MyApp.Five - lib/my_app/five.ex:2 (`use MyApp.Branchy`, whose __using__ has the @derive) (unsure: the @derive is in a branch",
		"MyApp.Six - lib/my_app/six.ex:2 (`use MyApp.PlainWrapper`, whose __using__ has the @derive) (unsure: the @derive is in a branch",
	)
	wantNotContains(t, out, "encoder.ex:8", "MyApp.Plain -", "MyApp.Wrapper -", "MyApp.Schema -", "MyApp.Late")

	out = e.callTool("dexter_implementations", map[string]any{"module": "MyApp.Encoder", "function": "encode"})
	wantContains(t, out, "MyApp.Encoder.encode/1 - lib/my_app/encoder.ex:14", "MyApp.Encoder.encode/1 - lib/my_app/encoder.ex:18", "MyApp.One - lib/my_app/structs.ex:2")
	wantNotContains(t, out, "encoder.ex:9")
}

// One row per function with its clause count; a note when the answer is cut;
// a space works as a dot; a snake_case query finds the CamelCase module; the
// module part of a member query can be the end of the module name.
func TestSearchTool_RowsAndQueries(t *testing.T) {
	e := setupUseChainProject(t)
	e.indexFile("lib/my_app/multi.ex", `defmodule MyApp.Multi do
  def humanize(:a), do: "A"
  def humanize(:b), do: "B"
  def humanize(:c), do: "C"
end
`)
	var many strings.Builder
	many.WriteString("defmodule MyApp.Many do\n")
	for i := 0; i < 60; i++ {
		many.WriteString("  def zzfun" + strings.Repeat("x", i) + "(a), do: a\n")
	}
	many.WriteString("end\n")
	e.indexFile("lib/my_app/many.ex", many.String())
	e.indexFile("lib/my_app_web/employee_controller.ex", "defmodule MyAppWeb.EmployeeController do\n  def index(conn, _), do: conn\nend\n")

	out := e.callTool("dexter_search", map[string]any{"query": "humanize"})
	wantContains(t, out, "MyApp.Multi.humanize/1 (def, 3 clauses) - lib/my_app/multi.ex:2")
	if strings.Count(out, "humanize/1") != 1 {
		t.Errorf("one row per function:\n%s", out)
	}

	out = e.callTool("dexter_search", map[string]any{"query": "zzfun"})
	wantContains(t, out, "The first 50 matches are shown; there are more.")

	out = e.callTool("dexter_search", map[string]any{"query": "EmployeeController index"})
	wantContains(t, out, "MyAppWeb.EmployeeController.index/2 (def) - lib/my_app_web/employee_controller.ex:2")

	out = e.callTool("dexter_search", map[string]any{"query": "employee_controller"})
	wantContains(t, out, `these match "EmployeeController"`, "MyAppWeb.EmployeeController (module)")

	out = e.callTool("dexter_search", map[string]any{"query": "Workers.Mailer.ne"})
	wantContains(t, out, "MyApp.Workers.Mailer.new/1 (def, injected by `use SharedLib.BaseWorker`", "MyApp.Workers.Mailer.new/2")

	out = e.callTool("dexter_search", map[string]any{"query": "qqqnothing"})
	wantContains(t, out, "not file names; a space works as a dot")
}
