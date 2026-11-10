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
	if got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot); len(got) != 1 || got[0] != "lib/bb_self.ex:9" {
		t.Errorf("%%__MODULE__ key = %v, want lib/bb_self.ex:9 (past the nested modules)", got)
	}
	uri = bbOpen(t, server, caller)
	l, c = bbPos(t, caller.src, "%Self{domain:", "domain")
	if got := bbLines(definitionAt(t, server, uri, l, c), server.projectRoot); len(got) != 1 || got[0] != "lib/bb_self.ex:9" {
		t.Errorf("key through a use-injected alias = %v, want lib/bb_self.ex:9", got)
	}
}
