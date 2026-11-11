package lsp

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
)

// Navigation regressions: each test is a small, generic shape of a wrong
// answer found in real projects.

type bbFile struct{ rel, src string }

func bbServer(t *testing.T, files ...bbFile) (*Server, func()) {
	t.Helper()
	server, cleanup := setupTestServer(t)
	for _, f := range files {
		indexFile(t, server.store, server.projectRoot, f.rel, f.src)
	}
	return server, cleanup
}

func bbOpen(t *testing.T, server *Server, f bbFile) string {
	t.Helper()
	uri := "file://" + filepath.Join(server.projectRoot, f.rel)
	server.docs.Set(uri, f.src)
	return uri
}

// bbPos is the 0-based position in the middle of token in the first line that
// holds marker.
func bbPos(t *testing.T, src, marker, token string) (uint32, uint32) {
	t.Helper()
	l, c := position(t, src, marker, token)
	return l, c + uint32(len(token)/2)
}

func bbLines(locs []protocol.Location, root string) []string {
	var out []string
	for _, l := range locs {
		rel, _ := filepath.Rel(root, l.URI.Filename())
		out = append(out, fmt.Sprintf("%s:%d", rel, l.Range.Start.Line+1))
	}
	return out
}

func bbHas(got []string, want string) bool {
	for _, g := range got {
		if g == want {
			return true
		}
	}
	return false
}

// a local capture is a reference.
func TestRegression_LocalCaptureIsAReference(t *testing.T) {
	job := bbFile{"lib/bb_job.ex", `defmodule BB.Job do
  @unique_defaults %{period: 60}

  @doc false
  @spec cast_period(term) :: pos_integer()
  def cast_period(period) when is_integer(period), do: period
  def cast_period(period), do: period

  defp put_unique(changeset, value) do
    case value do
      [_ | _] = opts ->
        unique =
          @unique_defaults
          |> Map.merge(Map.new(opts))
          |> Map.update!(:period, &cast_period/1)

        {changeset, unique}

      true ->
        changeset
    end
  end

  defp to_timestamp({period, unit}) do
    {period, unit}
    |> cast_period()
    |> to_timestamp()
  end

  defp to_timestamp(n), do: n
end
`}
	roles := bbFile{"lib/bb_picks.ex", `defmodule BB.Picks do
  def all(list), do: Enum.map(list, &pick_default/1)

  def pick_default(:a), do: :x
  def pick_default(_), do: :y
end
`}
	caller := bbFile{"lib/bb_picks_user.ex", `defmodule BB.PicksUser do
  def go, do: BB.Picks.pick_default(:a)
end
`}
	server, cleanup := bbServer(t, job, roles, caller)
	defer cleanup()
	uri := bbOpen(t, server, job)
	l, c := bbPos(t, job.src, "def cast_period(period) when", "cast_period")
	got := bbLines(referencesAt(t, server, uri, l, c), server.projectRoot)
	t.Logf("cast_period refs: %v", got)
	for _, want := range []string{"lib/bb_job.ex:15", "lib/bb_job.ex:26"} {
		if !bbHas(got, want) {
			t.Errorf("missing %s", want)
		}
	}
	uri2 := bbOpen(t, server, caller)
	l, c = bbPos(t, caller.src, "BB.Picks.pick_default", "pick_default")
	got = bbLines(referencesAt(t, server, uri2, l, c), server.projectRoot)
	t.Logf("pick_default refs: %v", got)
	if !bbHas(got, "lib/bb_picks.ex:2") {
		t.Errorf("missing the local capture lib/bb_picks.ex:2")
	}
}

var bbChain = []bbFile{
	{"lib/bb_base_worker.ex", `defmodule BB.BaseWorker do
  defmacro __using__(opts) do
    quote do
      @opts unquote(opts)
      def lock_key, do: :owner
    end
  end
end
`},
	{"lib/bb_pro_worker.ex", `defmodule BB.ProWorker do
  defmacro __using__(opts) do
    quote do
      use BB.BaseWorker, unquote(opts)
    end
  end
end
`},
}

// a local call of a use-injected function is a reference.
func TestRegression_LocalCallOfInjectedFunctionIsAReference(t *testing.T) {
	worker := bbFile{"lib/bb_some_worker.ex", `defmodule BB.SomeWorker do
  use BB.ProWorker

  def perform(_job) do
    lock_key()
  end
end
`}
	test := bbFile{"test/bb_some_worker_test.exs", `defmodule BB.SomeWorkerTest do
  def check, do: BB.SomeWorker.lock_key()
end
`}
	server, cleanup := bbServer(t, append(bbChain, worker, test)...)
	defer cleanup()
	uri := bbOpen(t, server, test)
	l, c := bbPos(t, test.src, "BB.SomeWorker.lock_key", "lock_key")
	got := bbLines(referencesAt(t, server, uri, l, c), server.projectRoot)
	t.Logf("lock_key refs: %v", got)
	for _, want := range []string{"test/bb_some_worker_test.exs:2", "lib/bb_some_worker.ex:5"} {
		if !bbHas(got, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// `use unquote(opt)` takes the value the caller passes.
func TestRegression_UseUnquoteOptionTakesTheCallersValue(t *testing.T) {
	files := []bbFile{
		{"lib/bb_queue_worker.ex", `defmodule BBQueue.Worker do
  defmacro __using__(_opts) do
    quote do
      def new(args, opts \\ []), do: {args, opts}
    end
  end
end
`},
		{"lib/bb_queue_pro_worker.ex", `defmodule BBQueue.ProWorker do
  defmacro __using__(_opts) do
    quote do
      def new(args, opts \\ []), do: {:pro, args, opts}
    end
  end
end
`},
		{"lib/bb_wrap_worker.ex", `defmodule BB.WrapWorker do
  defmacro __using__(opts) do
    queue_module = Keyword.get(opts, :queue_module, BBQueue.Worker)

    quote do
      use unquote(queue_module)
    end
  end
end
`},
		{"lib/bb_wrap_pro_worker.ex", `defmodule BB.WrapProWorker do
  defmacro __using__(_opts) do
    quote do
      use BB.WrapWorker, queue_module: BBQueue.ProWorker
    end
  end
end
`},
		{"lib/bb_emit_worker.ex", `defmodule BB.EmitWorker do
  use BB.WrapProWorker
end
`},
	}
	caller := bbFile{"lib/bb_emit.ex", `defmodule BB.Emit do
  def go, do: BB.EmitWorker.new(%{})
end
`}
	server, cleanup := bbServer(t, append(files, caller)...)
	defer cleanup()
	uri := bbOpen(t, server, caller)
	l, c := bbPos(t, caller.src, "BB.EmitWorker.new", "new")
	got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot)
	t.Logf("new definition: %v", got)
	if bbHas(got, "lib/bb_queue_worker.ex:4") {
		t.Errorf("took the option's default (BBQueue.Worker)")
	}
	ok := bbHas(got, "lib/bb_queue_pro_worker.ex:4") || bbHas(got, "lib/bb_emit_worker.ex:2") ||
		bbHas(got, "lib/bb_wrap_pro_worker.ex:4") || bbHas(got, "lib/bb_wrap_worker.ex:6")
	if !ok {
		t.Errorf("no accepted answer")
	}
}

// extra definitions with the wrong arity.
func TestRegression_DefinitionKeepsTheCallsArity(t *testing.T) {
	engine := bbFile{"lib/bb_engine.ex", `defmodule BB.Engine do
  def insert_job(conf, changeset, opts), do: {conf, changeset, opts}

  def insert_job(conf, multi, name, changeset), do: {conf, multi, name, changeset}
end
`}
	useMod := bbFile{"lib/bb_queue.ex", `defmodule BB.Queue do
  defmacro __using__(_opts) do
    quote do
      def insert(changeset, opts \\ []), do: {changeset, opts}

      def insert(multi, name, changeset, opts \\ []), do: {multi, name, changeset, opts}
    end
  end
end
`}
	used := bbFile{"lib/bb_used.ex", `defmodule BB.Used do
  use BB.Queue
end
`}
	caller := bbFile{"lib/bb_engine_user.ex", `defmodule BB.EngineUser do
  alias BB.Engine

  def go(conf, multi, cs) do
    conf
    |> Engine.insert_job(multi, :name, cs)
  end

  def go2(multi, cs), do: BB.Used.insert(multi, :name, cs)
end
`}
	server, cleanup := bbServer(t, engine, useMod, used, caller)
	defer cleanup()
	uri := bbOpen(t, server, caller)
	l, c := bbPos(t, caller.src, "Engine.insert_job", "insert_job")
	got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot)
	t.Logf("insert_job definition: %v", got)
	if bbHas(got, "lib/bb_engine.ex:2") {
		t.Errorf("piped call of arity 4 also gives insert_job/3")
	}
	l, c = bbPos(t, caller.src, "BB.Used.insert", "insert")
	got = bbLines(definitionAt(t, server, uri, l, c), server.projectRoot)
	t.Logf("Used.insert definition: %v", got)
	if bbHas(got, "lib/bb_queue.ex:4") {
		t.Errorf("call of arity 3 also gives insert/2 (1..2)")
	}
}

// definition on a struct key.
func TestRegression_StructKeyDefinition(t *testing.T) {
	site := bbFile{"lib/bb_site.ex", `defmodule BB.Site do
  use Ecto.Schema

  schema "sites" do
    field :domain, :string
    field :timezone, :string
  end
end
`}
	plain := bbFile{"lib/bb_plain.ex", `defmodule BB.Plain do
  defstruct [:domain, name: nil]
end
`}
	caller := bbFile{"lib/bb_shields.ex", `defmodule BB.Shields do
  alias BB.Site

  def go(d), do: %Site{domain: d}
  def go2(d), do: %BB.Plain{domain: d}
end
`}
	server, cleanup := bbServer(t, site, plain, caller)
	defer cleanup()
	uri := bbOpen(t, server, caller)
	l, c := bbPos(t, caller.src, "%Site{domain:", "domain")
	got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot)
	t.Logf("schema key definition: %v", got)
	if len(got) != 1 || got[0] != "lib/bb_site.ex:5" {
		t.Errorf("schema struct key = %v, want the field line lib/bb_site.ex:5", got)
	}
	l, c = bbPos(t, caller.src, "%BB.Plain{domain:", "domain")
	got = bbLines(definitionAt(t, server, uri, l, c), server.projectRoot)
	t.Logf("defstruct key definition: %v", got)
	if len(got) != 1 || got[0] != "lib/bb_plain.ex:2" {
		t.Errorf("defstruct key = %v, want the defstruct line lib/bb_plain.ex:2", got)
	}
}

// hover on a defdelegate shows its @doc.
func TestRegression_HoverOnDefdelegateShowsItsDoc(t *testing.T) {
	cron := bbFile{"lib/bb_cron.ex", `defmodule BB.Cron do
  @doc "Parse a crontab expression into a cron struct."
  defdelegate parse(input), to: BB.Cron.Expression
end
`}
	expr := bbFile{"lib/bb_cron_expression.ex", `defmodule BB.Cron.Expression do
  @doc false
  def parse(input), do: input
end
`}
	caller := bbFile{"test/bb_cron_test.exs", `defmodule BB.CronTest do
  alias BB.Cron

  def go, do: Cron.parse("* * * * *")
end
`}
	server, cleanup := bbServer(t, cron, expr, caller)
	defer cleanup()
	uri := bbOpen(t, server, caller)
	l, c := bbPos(t, caller.src, "Cron.parse", "parse")
	h := hoverAt(t, server, uri, l, c)
	text := ""
	if h != nil {
		text = h.Contents.Value
	}
	t.Logf("hover: %q", text)
	if !strings.Contains(text, "Parse a crontab expression") {
		t.Errorf("hover does not show the delegate's @doc")
	}
}

// a remote call does not resolve to a def in the module's own
// __using__ quote.
func TestRegression_RemoteCallSkipsTheUsingQuote(t *testing.T) {
	queue := bbFile{"deps/bb_queue/lib/bb_queue.ex", `defmodule BBQ do
  defmacro __using__(_opts) do
    quote do
      def insert!(changeset, opts \\ []), do: BBQ.insert!(__MODULE__, changeset, opts)
    end
  end

  @doc """
  Raises if the job can't be inserted.
  """
  def insert!(name \\ __MODULE__, changeset, opts \\ []), do: {name, changeset, opts}
end
`}
	caller := bbFile{"lib/bb_schedule.ex", `defmodule BB.Schedule do
  def go(cs) do
    cs
    |> BBQ.insert!()
  end
end
`}
	server, cleanup := bbServer(t, queue, caller)
	defer cleanup()
	uri := bbOpen(t, server, caller)
	l, c := bbPos(t, caller.src, "BBQ.insert!", "insert")
	h := hoverAt(t, server, uri, l, c)
	text := ""
	if h != nil {
		text = h.Contents.Value
	}
	t.Logf("hover: %q", text)
	if !strings.Contains(text, "can't be inserted") {
		t.Errorf("hover does not show BBQ.insert!'s doc")
	}
	got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot)
	t.Logf("definition: %v", got)
	if bbHas(got, "deps/bb_queue/lib/bb_queue.ex:4") {
		t.Errorf("definition goes into the __using__ quote")
	}
}

// a use-injected function that implements a callback shows the
// callback's doc.
func TestRegression_InjectedCallbackImplShowsTheCallbackDoc(t *testing.T) {
	repo := bbFile{"deps/bb_repo/lib/bb_repo.ex", `defmodule BBRepo do
  @doc """
  Fetches all entries from the data store matching the given query.
  """
  @callback all(queryable :: term, opts :: keyword) :: [term]

  defmacro __using__(_opts) do
    quote do
      @behaviour BBRepo

      @impl true
      def all(queryable, opts \\ []), do: {queryable, opts}
    end
  end
end
`}
	myRepo := bbFile{"lib/bb_my_repo.ex", `defmodule BB.Repo do
  use BBRepo
end
`}
	caller := bbFile{"lib/bb_reports.ex", `defmodule BB.Reports do
  alias BB.Repo

  def go(q), do: Repo.all(q)
end
`}
	server, cleanup := bbServer(t, repo, myRepo, caller)
	defer cleanup()
	uri := bbOpen(t, server, caller)
	l, c := bbPos(t, caller.src, "Repo.all", "all")
	h := hoverAt(t, server, uri, l, c)
	text := ""
	if h != nil {
		text = h.Contents.Value
	}
	t.Logf("hover: %q", text)
	t.Logf("definition: %v", bbLines(definitionAt(t, server, uri, l, c), server.projectRoot))
	if !strings.Contains(text, "Fetches all entries") {
		t.Errorf("hover does not show the callback's doc")
	}
}

// completion of a use-injected function of a nested module.
func TestRegression_CompletionInjectedIntoNestedModule(t *testing.T) {
	worker := bbFile{"lib/bb_q_worker.ex", `defmodule BBQ2.Worker do
  defmacro __using__(_opts) do
    quote do
      def timeout(_job), do: :infinity
    end
  end
end
`}
	src := `defmodule BBQ2.WorkerTest do
  alias BBQ2.{Job, Worker}

  defmodule BasicWorker do
    use Worker
  end

  def go do
    BasicWorker.time
  end
end
`
	test := bbFile{"test/bb_q_worker_test.exs", src}
	server, cleanup := bbServer(t, worker, test)
	defer cleanup()
	uri := bbOpen(t, server, test)
	l, c := position(t, src, "BasicWorker.time", "BasicWorker.time")
	c += uint32(len("BasicWorker.time"))
	items := completionAt(t, server, uri, l, c)
	var labels []string
	for _, it := range items {
		labels = append(labels, it.Label)
	}
	t.Logf("completion: %v", labels)
	found := false
	for _, it := range items {
		if strings.HasPrefix(it.Label, "timeout") {
			found = true
		}
	}
	if !found {
		t.Errorf("no timeout completion")
	}
	// The same with the full module name: does the use chain through the
	// alias resolve?
	full := strings.Replace(src, "    BasicWorker.time\n", "    BBQ2.WorkerTest.BasicWorker.time\n", 1)
	server.docs.Set(uri, full)
	l, c = position(t, full, "BBQ2.WorkerTest.BasicWorker.time", "BBQ2.WorkerTest.BasicWorker.time")
	c += uint32(len("BBQ2.WorkerTest.BasicWorker.time"))
	labels = nil
	for _, it := range completionAt(t, server, uri, l, c) {
		labels = append(labels, it.Label)
	}
	t.Logf("completion with the full name: %v", labels)
}

// completion of a nested module name.
func TestRegression_CompletionNestedModuleName(t *testing.T) {
	feat := bbFile{"lib/bb_feature.ex", `defmodule BB.Billing.Feature do
  defmodule RevenueGoals do
    def name, do: :revenue
  end

  defmodule Props do
  end
end
`}
	src := `defmodule BB.Goals do
  def go do
    BB.Billing.Feature.Re
  end
end
`
	goals := bbFile{"lib/bb_goals.ex", src}
	server, cleanup := bbServer(t, feat, goals)
	defer cleanup()
	uri := bbOpen(t, server, goals)
	l, c := position(t, src, "BB.Billing.Feature.Re", "BB.Billing.Feature.Re")
	c += uint32(len("BB.Billing.Feature.Re"))
	items := completionAt(t, server, uri, l, c)
	var labels []string
	for _, it := range items {
		labels = append(labels, it.Label)
	}
	t.Logf("completion: %v", labels)
	found := false
	for _, it := range items {
		if it.Label == "RevenueGoals" || strings.HasSuffix(it.Label, ".RevenueGoals") {
			found = true
		}
	}
	if !found {
		t.Errorf("no RevenueGoals completion")
	}
}

// The real shape: the cursor in the middle of the name, and a first
// request before the index has the modules.
func TestRegression_CompletionNestedModuleNameMidWordAndEarly(t *testing.T) {
	feat := bbFile{"lib/bb_feature.ex", `defmodule BB.Billing.Feature do
  defmacro __using__(_opts) do
    quote do
      def check_availability(_team), do: :ok
    end
  end
end

defmodule BB.Billing.Feature.RevenueGoals do
  use BB.Billing.Feature
end

defmodule BB.Billing.Feature.Props do
  use BB.Billing.Feature
end
`}
	src := `defmodule BB.Goals do
  defp check(site) do
    BB.Billing.Feature.RevenueGoals.check_availability(site.team)
  end
end
`
	goals := bbFile{"lib/bb_goals.ex", src}
	server, cleanup := setupTestServer(t)
	defer cleanup()
	uri := bbOpen(t, server, goals)
	l, c := position(t, src, "BB.Billing.Feature.Re", "BB.Billing.Feature.Re")
	c += uint32(len("BB.Billing.Feature.Re"))
	labels := func() []string {
		var out []string
		for _, it := range completionAt(t, server, uri, l, c) {
			out = append(out, it.Label)
		}
		return out
	}
	t.Logf("before the index: %v", labels())
	indexFile(t, server.store, server.projectRoot, feat.rel, feat.src)
	indexFile(t, server.store, server.projectRoot, goals.rel, goals.src)
	got := labels()
	t.Logf("after the index: %v", got)
	found := false
	for _, g := range got {
		if g == "RevenueGoals" || strings.HasSuffix(g, ".RevenueGoals") {
			found = true
		}
	}
	if !found {
		t.Errorf("no RevenueGoals completion")
	}
}

// A struct key in `%__MODULE__{...}`, in a module whose nested module comes
// before its defstruct, and through an alias that a `use` injects.
func TestRegression_StructKeyDefinitionShapes(t *testing.T) {
	self := bbFile{"lib/bb_self.ex", `defmodule BB.Self do
  defmodule Helper do
    defstruct [:other]
    def one, do: 1
  end

  defmodule Short, do: def(two, do: 2)
  defmodule Inline, do: defstruct([:domain])

  defmodule Split,
    do: defstruct [:domain]

  defmodule SplitDo, do:
    defstruct [:domain]

  defstruct [:domain, :name]

  def new(d), do: %__MODULE__{domain: d}
end
`}
	aliaser := bbFile{"lib/bb_aliaser.ex", `defmodule BB.Aliaser do
  defmacro __using__(_opts) do
    quote do
      alias BB.Self
    end
  end
end
`}
	caller := bbFile{"lib/bb_self_user.ex", `defmodule BB.SelfUser do
  use BB.Aliaser

  def go(d), do: %Self{domain: d}
end
`}
	server, cleanup := bbServer(t, self, aliaser, caller)
	defer cleanup()
	uri := bbOpen(t, server, self)
	l, c := bbPos(t, self.src, "%__MODULE__{domain:", "domain")
	if got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot); len(got) != 1 || got[0] != "lib/bb_self.ex:16" {
		t.Errorf("%%__MODULE__ key = %v, want lib/bb_self.ex:16 (past the nested modules)", got)
	}
	uri = bbOpen(t, server, caller)
	l, c = bbPos(t, caller.src, "%Self{domain:", "domain")
	if got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot); len(got) != 1 || got[0] != "lib/bb_self.ex:16" {
		t.Errorf("key through a use-injected alias = %v, want lib/bb_self.ex:16", got)
	}
}

// A one-line nested module before a schema does not take the schema's do.
func TestRegression_StructKeyAfterOneLineNestedModule(t *testing.T) {
	site := bbFile{"lib/bb_site2.ex", `defmodule BB.Site2 do
  use Ecto.Schema
  defmodule Short, do: :ok
  schema "sites" do
    field :domain, :string
  end

  def new(d), do: %__MODULE__{domain: d}
end
`}
	server, cleanup := bbServer(t, site)
	defer cleanup()
	uri := bbOpen(t, server, site)
	l, c := bbPos(t, site.src, "%__MODULE__{domain:", "domain")
	if got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot); len(got) != 1 || got[0] != "lib/bb_site2.ex:5" {
		t.Errorf("key after a one-line nested module = %v, want the field line lib/bb_site2.ex:5", got)
	}
}

// A `use unquote(mod)` chain where an outer __using__ changes the options
// before it passes them on: PL.Outer does Keyword.put_new(opts, :base,
// PL.Second) and `use PL.Wrapper, unquote(opts)`; PL.Wrapper does
// Keyword.pop(opts, :base, PL.Base) and `use unquote(mod), unquote(opts)`.
// PL.Second does not use PL.Base.
func TestRegression_UseUnquoteOptionFromAnOuterUsing(t *testing.T) {
	lib := []bbFile{
		{"lib/pl_base.ex", `defmodule PL.Base do
  defmacro __using__(_opts) do
    quote do
      def new(args, opts \\ []), do: {:base, args, opts}
    end
  end
end
`},
		{"lib/pl_second.ex", `defmodule PL.Second do
  defmacro __using__(_opts) do
    quote do
      def new(args, opts \\ []), do: {:second, args, opts}
    end
  end
end
`},
		{"lib/pl_wrapper.ex", `defmodule PL.Wrapper do
  defmacro __using__(opts) do
    {mod, opts} = Keyword.pop(opts, :base, PL.Base)

    quote do
      use unquote(mod), unquote(opts)
    end
  end
end
`},
		{"lib/pl_outer.ex", `defmodule PL.Outer do
  defmacro __using__(opts) do
    opts = Keyword.put_new(opts, :base, PL.Second)

    quote do
      use PL.Wrapper, unquote(opts)
    end
  end
end
`},
		{"lib/pl_forcer.ex", `defmodule PL.Forcer do
  defmacro __using__(opts) do
    opts = Keyword.put(opts, :base, PL.Second)

    quote do
      use PL.Wrapper, unquote(opts)
    end
  end
end
`},
		{"lib/pl_merger.ex", `defmodule PL.Merger do
  defmacro __using__(opts) do
    opts = Keyword.merge([base: PL.Second], opts)

    quote do
      use PL.Wrapper, unquote(opts)
    end
  end
end
`},
		{"lib/pl_picker.ex", `defmodule PL.Picker do
  defmacro __using__(opts) do
    opts = Keyword.put_new(opts, :base, PL.Second.pick())

    quote do
      use PL.Wrapper, unquote(opts)
    end
  end

  def pick, do: PL.Second
end
`},
	}
	workers := []bbFile{
		{"lib/pl_worker.ex", `defmodule PL.Worker do
  use PL.Outer, queue: :default
end
`},
		{"lib/pl_own_worker.ex", `defmodule PL.OwnWorker do
  use PL.Outer, base: PL.Base
end
`},
		{"lib/pl_plain_worker.ex", `defmodule PL.PlainWorker do
  use PL.Wrapper
end
`},
		{"lib/pl_forced_worker.ex", `defmodule PL.ForcedWorker do
  use PL.Forcer, base: PL.Base
end
`},
		{"lib/pl_merged_worker.ex", `defmodule PL.MergedWorker do
  use PL.Merger
end
`},
		{"lib/pl_picked_worker.ex", `defmodule PL.PickedWorker do
  use PL.Picker
end
`},
	}
	caller := bbFile{"lib/pl_caller.ex", `defmodule PL.Caller do
  def a(args), do: args |> PL.Worker.new()
  def b(args), do: args |> PL.OwnWorker.new()
  def c(args), do: args |> PL.PlainWorker.new()
  def d(args), do: args |> PL.PickedWorker.new()
  def e(args), do: args |> PL.ForcedWorker.new()
  def f(args), do: args |> PL.MergedWorker.new()
end
`}
	server, cleanup := bbServer(t, append(append(lib, workers...), caller)...)
	defer cleanup()
	uri := bbOpen(t, server, caller)
	for _, tt := range []struct {
		name, marker string
		want         []string // any of these
		notWant      []string
	}{
		{"the outer put_new", "PL.Worker.new", []string{"lib/pl_second.ex:4"}, []string{"lib/pl_base.ex:4"}},
		{"the caller's option wins over put_new", "PL.OwnWorker.new", []string{"lib/pl_base.ex:4"}, []string{"lib/pl_second.ex:4"}},
		{"no step sets the key: the default", "PL.PlainWorker.new", []string{"lib/pl_base.ex:4"}, []string{"lib/pl_second.ex:4"}},
		{"put wins over the caller's option", "PL.ForcedWorker.new", []string{"lib/pl_second.ex:4"}, []string{"lib/pl_base.ex:4"}},
		{"merge with the options last: put_new", "PL.MergedWorker.new", []string{"lib/pl_second.ex:4"}, []string{"lib/pl_base.ex:4"}},
		{"a value that cannot be known: the use line", "PL.PickedWorker.new", []string{"lib/pl_picked_worker.ex:2"}, []string{"lib/pl_base.ex:4", "lib/pl_second.ex:4"}},
	} {
		l, c := bbPos(t, caller.src, tt.marker, "new")
		got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot)
		t.Logf("%s: %v", tt.name, got)
		for _, bad := range tt.notWant {
			if bbHas(got, bad) {
				t.Errorf("%s: definition = %v, must not give %s", tt.name, got, bad)
			}
		}
		if len(tt.want) > 0 {
			ok := false
			for _, w := range tt.want {
				ok = ok || bbHas(got, w)
			}
			if !ok {
				t.Errorf("%s: definition = %v, want %v", tt.name, got, tt.want)
			}
		}
	}
}

// optModule is a fixture file with one module.
func optModule(name, body string) bbFile {
	return bbFile{"lib/" + strings.ToLower(strings.ReplaceAll(name, ".", "_")) + ".ex", "defmodule " + name + " do\n" + body + "\nend\n"}
}

// optUsing is a __using__ body: pre runs before the quote.
func optUsing(pre, quote string) string {
	return "  defmacro __using__(opts) do\n" + pre + "\n    quote do\n" + quote + "\n    end\n  end"
}

// The options that use chains pass on, in each form, with the module that
// Elixir gives (each case was compiled to check it). PL.Wrapper reads the
// option with Keyword.pop and a default of PL.Base.
func TestRegression_UseChainOptionForms(t *testing.T) {
	inject := func(tag string) string {
		return "  defmacro __using__(_opts) do\n    quote do\n      def new(args, opts \\\\ []), do: {:" + tag + ", args, opts}\n    end\n  end"
	}
	lib := []bbFile{
		optModule("PL.Base", inject("base")),
		optModule("PL.Second", inject("second")),
		optModule("PL.Third", inject("third")),
		optModule("PL.Empty", "  defmacro __using__(_opts) do\n    quote do\n      def other, do: :ok\n    end\n  end"),
		optModule("PL.Wrapper", optUsing("    {mod, opts} = Keyword.pop(opts, :base, PL.Base)", "      use unquote(mod), unquote(opts)")),
		optModule("PL.Outer", optUsing("    opts = Keyword.put_new(opts, :base, PL.Second)", "      use PL.Wrapper, unquote(opts)")),
		optModule("PL.Piped", optUsing("    opts = opts |> Keyword.put_new(:base, PL.Second)", "      use PL.Wrapper, unquote(opts)")),
		optModule("PL.Piped2", optUsing("    opts =\n      opts\n      |> Keyword.put_new(:base, PL.Second)", "      use PL.Wrapper, unquote(opts)")),
		optModule("PL.InQuote", optUsing("", "      use PL.Wrapper, unquote(Keyword.put_new(opts, :base, PL.Second))")),
		optModule("PL.Renamed", optUsing("    new_opts = Keyword.put_new(opts, :base, PL.Second)", "      use PL.Wrapper, unquote(new_opts)")),
		optModule("PL.OtherVar", optUsing("    other = Keyword.put(opts, :base, PL.Second)\n    _ = other", "      use PL.Wrapper, unquote(opts)")),
		optModule("PL.Mixed", optUsing("", "      use PL.Wrapper, [base: PL.Second] ++ unquote(opts)")),
		optModule("PL.Mixed2", optUsing("", "      use PL.Wrapper, [{:base, PL.Second} | unquote(opts)]")),
		optModule("PL.Mixed3", optUsing("", "      use PL.Wrapper, Keyword.merge(unquote(opts), base: PL.Second)")),
		optModule("PL.Mixed4", optUsing("", "      use PL.Wrapper, [base: PL.Second, unquote_splicing(opts)]")),
		optModule("PL.Top", optUsing("    opts = Keyword.put_new(opts, :base, PL.Third)", "      use PL.Outer, unquote(opts)")),
		optModule("PL.TopPut", optUsing("    opts = Keyword.put(opts, :base, PL.Third)", "      use PL.Outer, unquote(opts)")),
		optModule("PL.Web", "  def worker do\n    quote do\n      use PL.Outer\n    end\n  end\n\n  def plain do\n    quote do\n      use PL.Wrapper\n    end\n  end\n\n  defmacro __using__(which) when is_atom(which) do\n    apply(__MODULE__, which, [])\n  end"),
		optModule("PL.Helper", "  defmacro __using__(opts) do\n    opts = Keyword.put_new(opts, :base, PL.Second)\n    build(opts)\n  end\n\n  defp build(opts) do\n    quote do\n      use PL.Wrapper, unquote(opts)\n    end\n  end"),
		optModule("PL.HelperEdit", "  defmacro __using__(opts) do\n    build(opts)\n  end\n\n  defp build(opts) do\n    opts = Keyword.put_new(opts, :base, PL.Second)\n\n    quote do\n      use PL.Wrapper, unquote(opts)\n    end\n  end"),
		optModule("PL.Opaque", optUsing("    mod = opts[:base] || PL.Base", "      use unquote(mod)")),
		optModule("PL.OpaqueOuter", optUsing("    opts = Keyword.put_new(opts, :base, PL.Second)", "      use PL.Opaque, unquote(opts)")),
		optModule("PL.CallDefault", optUsing("    mod = opts[:base] || default_base()", "      use unquote(mod)")+"\n\n  def default_base, do: PL.Base"),
		optModule("PL.CallDefaultOuter", optUsing("    opts = Keyword.put_new(opts, :base, PL.Second)", "      use PL.CallDefault, unquote(opts)")),
		optModule("PL.SelfRead", optUsing("    opts = Keyword.put_new(opts, :base, PL.Second)\n    base = opts[:base]", "      use unquote(base)")),
		optModule("PL.SelfFetch", optUsing("    opts = Keyword.put_new(opts, :base, PL.Second)\n    base = Keyword.fetch!(opts, :base)", "      use unquote(base)")),
		optModule("PL.PopThenPut", optUsing("    {mod, opts} = Keyword.pop(opts, :base, PL.Base)\n    opts = Keyword.put(opts, :base, PL.Second)", "      use unquote(mod), unquote(opts)")),
		optModule("PL.MergePut", optUsing("    opts = Keyword.merge(opts, base: PL.Second)", "      use PL.Wrapper, unquote(opts)")),
		optModule("PL.MergeML", optUsing("    opts =\n      Keyword.merge(\n        [queue: :x, base: PL.Second],\n        opts\n      )", "      use PL.Wrapper, unquote(opts)")),
		optModule("PL.Deleter", optUsing("    opts = Keyword.delete(opts, :base)", "      use PL.Wrapper, unquote(opts)")),
		optModule("PL.Dropper", optUsing("    opts = Keyword.drop(opts, [:base])", "      use PL.Wrapper, unquote(opts)")),
		optModule("PL.Taker", optUsing("    sub = Keyword.take(opts, [:queue])", "      use PL.Wrapper, unquote(sub)")),
		optModule("PL.GetWrapper", optUsing("    mod = Keyword.get(opts, :base, PL.Base)", "      use unquote(mod)")),
		optModule("PL.GetOuter", optUsing("    opts = Keyword.put_new(opts, :base, PL.Second)", "      use PL.GetWrapper, unquote(opts)")),
		optModule("PL.Aliased", "  alias PL.Second, as: Sec\n"+optUsing("    opts = Keyword.put_new(opts, :base, Sec)", "      use PL.Wrapper, unquote(opts)")),
		optModule("PL.Literal", optUsing("", "      use PL.Wrapper, base: PL.Second")),
		optModule("PL.Unrelated", optUsing("    opts = Keyword.put_new(opts, :queue, :default)", "      use PL.Wrapper, unquote(opts)")),
	}
	workers := []struct{ name, use, want string }{
		{"W.Outer", "use PL.Outer, queue: :default", "second"},
		{"W.Piped", "use PL.Piped", "second"},
		{"W.Piped2", "use PL.Piped2", "second"},
		{"W.InQuote", "use PL.InQuote", "second"},
		{"W.Renamed", "use PL.Renamed", "second"},
		{"W.OtherVar", "use PL.OtherVar", "base"},
		{"W.Mixed", "use PL.Mixed", "second"},
		{"W.Mixed2", "use PL.Mixed2", "second"},
		{"W.Mixed3", "use PL.Mixed3", "second"},
		{"W.Mixed4", "use PL.Mixed4", "second"},
		{"W.MixedOwn", "use PL.Mixed3, base: PL.Third", "second"},
		{"W.Top", "use PL.Top", "third"},
		{"W.TopOwn", "use PL.Top, base: PL.Base", "base"},
		{"W.TopPut", "use PL.TopPut, base: PL.Base", "third"},
		{"W.WebWorker", "use PL.Web, :worker", "second"},
		{"W.WebPlain", "use PL.Web, :plain", "base"},
		{"W.Helper", "use PL.Helper", "second"},
		{"W.HelperOwn", "use PL.Helper, base: PL.Third", "third"},
		{"W.HelperEdit", "use PL.HelperEdit", "second"},
		{"W.Opaque", "use PL.OpaqueOuter", "second"},
		{"W.OpaqueOwn", "use PL.OpaqueOuter, base: PL.Third", "third"},
		{"W.CallDefault", "use PL.CallDefaultOuter", "second"},
		{"W.CallDefaultOwn", "use PL.CallDefault, base: PL.Third", "third"},
		{"W.CallDefaultNone", "use PL.CallDefault", "use"},
		{"W.SelfRead", "use PL.SelfRead", "second"},
		{"W.SelfFetch", "use PL.SelfFetch", "second"},
		{"W.SelfFetchOwn", "use PL.SelfFetch, base: PL.Third", "third"},
		{"W.PopThenPut", "use PL.PopThenPut", "base"},
		{"W.MergePut", "use PL.MergePut, base: PL.Base", "second"},
		{"W.MergeML", "use PL.MergeML", "second"},
		{"W.MergeMLOwn", "use PL.MergeML, base: PL.Third", "third"},
		{"W.Deleter", "use PL.Deleter, base: PL.Second", "base"},
		{"W.Dropper", "use PL.Dropper, base: PL.Second", "base"},
		{"W.Taker", "use PL.Taker, base: PL.Second", "base"},
		{"W.GetOuter", "use PL.GetOuter", "second"},
		{"W.Aliased", "use PL.Aliased", "second"},
		{"W.Literal", "use PL.Literal, base: PL.Third", "second"},
		{"W.Unrelated", "use PL.Unrelated", "base"},
		{"W.Plain", "use PL.Wrapper", "base"},
		{"W.PlainAtom", "use PL.Wrapper, queue: :default, base: PL.Second", "second"},
		{"W.Var", "use PL.Wrapper, base: @base", "use"},
		{"W.MultiLine", "use PL.Outer,\n    queue: :default,\n    base: PL.Third", "third"},
		{"W.Nested", "use PL.Wrapper, base: PL.Second, extra: [base: :nope]", "second"},
	}
	files := append([]bbFile(nil), lib...)
	var caller strings.Builder
	caller.WriteString("defmodule PL.Caller do\n")
	for i, w := range workers {
		files = append(files, optModule(w.name, "  "+w.use+"\n\n  def run(a), do: new(a)"))
		fmt.Fprintf(&caller, "  def f%d(a), do: %s.new(a)\n", i, w.name)
	}
	files = append(files, optModule("W.Empty", "  use PL.Outer, base: PL.Empty"))
	caller.WriteString("  def empty(a), do: W.Empty.new(a)\n")
	// Two modules in one file, each with its own use.
	files = append(files, bbFile{"lib/two.ex", "defmodule Two.A do\n  use PL.Wrapper\nend\n\ndefmodule Two.B do\n  use PL.Outer\nend\n"})
	caller.WriteString("  def two_a(a), do: Two.A.new(a)\n  def two_b(a), do: Two.B.new(a)\nend\n")
	callerFile := bbFile{"lib/pl_caller.ex", caller.String()}
	files = append(files, callerFile)
	server, cleanup := bbServer(t, files...)
	defer cleanup()
	uri := bbOpen(t, server, callerFile)
	check := func(what string, got []string, want, worker string) {
		t.Helper()
		wantLine := "lib/pl_" + want + ".ex:4"
		if want == "use" {
			wantLine = worker + ":2"
		}
		if len(got) != 1 || got[0] != wantLine {
			t.Errorf("%s: definition = %v, want %s", what, got, wantLine)
		}
	}
	for _, w := range workers {
		wf := optModule(w.name, "  "+w.use+"\n\n  def run(a), do: new(a)")
		l, c := bbPos(t, callerFile.src, w.name+".new", "new")
		check(w.name+".new", bbLines(definitionAt(t, server, uri, l, c), server.projectRoot), w.want, wf.rel)
		wuri := bbOpen(t, server, wf)
		bl, bc := bbPos(t, wf.src, "do: new(a)", "new")
		check(w.name+" bare new", bbLines(definitionAt(t, server, wuri, bl, bc), server.projectRoot), w.want, wf.rel)
	}
	// The caller's own option names a module without new: put_new's module
	// is not a fallback.
	l, c := bbPos(t, callerFile.src, "W.Empty.new", "new")
	for _, got := range bbLines(definitionAt(t, server, uri, l, c), server.projectRoot) {
		if strings.HasPrefix(got, "lib/pl_") {
			t.Errorf("W.Empty.new: definition = %s, want none in the chain's other modules", got)
		}
	}
	for marker, want := range map[string]string{"Two.A.new": "base", "Two.B.new": "second"} {
		l, c := bbPos(t, callerFile.src, marker, "new")
		check(marker, bbLines(definitionAt(t, server, uri, l, c), server.projectRoot), want, "")
	}
}
