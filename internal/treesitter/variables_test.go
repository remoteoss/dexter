package treesitter

import (
	"strings"
	"testing"
)

func TestFindVariableOccurrences_BasicVariable(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    result = transform(data)
    log(result)
    result
  end
end`)

	// Cursor on "result" at line 2, col 4
	occs := FindVariableOccurrences(src, 2, 4)
	if len(occs) != 3 {
		t.Fatalf("expected 3 occurrences of 'result', got %d", len(occs))
	}
	// Line 2: result = transform(data)
	if occs[0].Line != 2 {
		t.Errorf("occ[0] line: expected 2, got %d", occs[0].Line)
	}
	// Line 3: log(result)
	if occs[1].Line != 3 {
		t.Errorf("occ[1] line: expected 3, got %d", occs[1].Line)
	}
	// Line 4: result
	if occs[2].Line != 4 {
		t.Errorf("occ[2] line: expected 4, got %d", occs[2].Line)
	}
}

func TestFindVariableOccurrences_FunctionParam(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    transform(data)
  end
end`)

	// Cursor on "data" parameter at line 1, col 14
	occs := FindVariableOccurrences(src, 1, 14)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of 'data', got %d", len(occs))
	}
}

func TestFindVariableOccurrences_NotOnVariable(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    transform(data)
  end
end`)

	// Cursor on "transform" (function call, not a variable)
	occs := FindVariableOccurrences(src, 2, 4)
	if occs != nil {
		t.Errorf("expected nil for function call, got %d occurrences", len(occs))
	}
}

func TestFindVariableOccurrences_NotOnKeyword(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    data
  end
end`)

	// Cursor on "def" keyword
	occs := FindVariableOccurrences(src, 1, 2)
	if occs != nil {
		t.Errorf("expected nil for keyword, got %d occurrences", len(occs))
	}
}

func TestFindVariableOccurrences_ScopedToFunction(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def first(x) do
    x + 1
  end

  def second(x) do
    x + 2
  end
end`)

	// Cursor on "x" in first function (line 1, col 12)
	occs := FindVariableOccurrences(src, 1, 12)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of 'x' in first/1, got %d", len(occs))
	}
	// Should only find x in first, not second
	for _, occ := range occs {
		if occ.Line >= 5 {
			t.Errorf("found occurrence in second function at line %d", occ.Line)
		}
	}
}

func TestFindVariableOccurrences_ModuleNameNotVariable(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    MyApp.transform(data)
  end
end`)

	// Cursor on "MyApp" (alias, not a variable)
	occs := FindVariableOccurrences(src, 2, 4)
	if occs != nil {
		t.Errorf("expected nil for module alias, got %d occurrences", len(occs))
	}
}

func TestFindVariableOccurrences_ModuleAttribute_BasicRename(t *testing.T) {
	src := []byte(`defmodule MyApp do
  @timeout 5000

  def run do
    Process.sleep(@timeout)
  end
end`)

	// Cursor on "timeout" in the definition "@timeout 5000" (line 1, col 3)
	occs := FindVariableOccurrences(src, 1, 3)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of @timeout (definition + reference), got %d", len(occs))
	}
	if occs[0].Line != 1 {
		t.Errorf("occ[0] line: expected 1 (@timeout 5000), got %d", occs[0].Line)
	}
	if occs[1].Line != 4 {
		t.Errorf("occ[1] line: expected 4 (@timeout reference), got %d", occs[1].Line)
	}
}

func TestFindVariableOccurrences_ModuleAttribute_CursorOnReference(t *testing.T) {
	src := []byte(`defmodule MyApp do
  @timeout 5000

  def run do
    Process.sleep(@timeout)
  end
end`)

	// Cursor on "timeout" in the reference "@timeout" inside the def (line 4, col 20)
	occs := FindVariableOccurrences(src, 4, 20)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences from reference cursor, got %d", len(occs))
	}
}

func TestFindVariableOccurrences_ModuleAttribute_DoesNotMatchPlainVariable(t *testing.T) {
	src := []byte(`defmodule MyApp do
  @timeout 5000

  def run do
    timeout = 1000
    Process.sleep(timeout)
  end
end`)

	// Cursor on "timeout" in "@timeout 5000" — should NOT find the plain variable
	occs := FindVariableOccurrences(src, 1, 3)
	if len(occs) != 1 {
		t.Fatalf("expected 1 occurrence (@timeout def only, no plain variable), got %d: %+v", len(occs), occs)
	}
}

func TestFindVariableOccurrences_CapturedByAnonymousFunction(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(items) do
    prefix = "hello"
    Enum.map(items, fn item -> prefix <> item end)
  end
end`)

	// Cursor on "prefix" at line 2 col 4
	occs := FindVariableOccurrences(src, 2, 4)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of 'prefix' (binding + captured ref), got %d", len(occs))
	}
	if occs[0].Line != 2 {
		t.Errorf("occ[0] line: expected 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 3 {
		t.Errorf("occ[1] line: expected 3, got %d", occs[1].Line)
	}
}

func TestFindVariableOccurrences_ShadowedInAnonymousFunction(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    x = 1
    fn x -> x + 1 end
    x
  end
end`)

	// Cursor on outer "x" at line 2 col 4
	occs := FindVariableOccurrences(src, 2, 4)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of outer 'x' (binding + final ref), got %d", len(occs))
	}
	// Should be line 2 (x = 1) and line 4 (bare x), NOT inside the fn
	if occs[0].Line != 2 {
		t.Errorf("occ[0] line: expected 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 4 {
		t.Errorf("occ[1] line: expected 4, got %d", occs[1].Line)
	}
}

func TestFindVariableOccurrences_InsideAnonymousFunction(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    x = 1
    fn x -> x + 1 end
    x
  end
end`)

	// Cursor on inner "x" (fn parameter) at line 3
	// "    fn x -> x + 1 end" — "x" parameter is at col 7
	occs := FindVariableOccurrences(src, 3, 7)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of inner 'x' (param + body ref), got %d", len(occs))
	}
	// Both should be on line 3
	for _, occ := range occs {
		if occ.Line != 3 {
			t.Errorf("expected occurrence on line 3, got line %d", occ.Line)
		}
	}
}

func TestFindVariableOccurrences_WithBlockScope(t *testing.T) {
	src := []byte(`defmodule M do
  def f() do
    thing = nil
    with {:ok, thing} <- fetch("something") do
      {:ok, thing}
    end
    thing = :something
  end
end`)

	// Cursor on outer "thing" (line 2: "thing = nil")
	occs := FindVariableOccurrences(src, 2, 4)
	// Should find: line 2 (thing = nil) and line 6 (thing = :something)
	// Should NOT find: anything inside the with block
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of outer 'thing', got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 2 {
		t.Errorf("occ[0]: expected line 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 6 {
		t.Errorf("occ[1]: expected line 6, got %d", occs[1].Line)
	}

	// Cursor on inner "thing" (line 4: "{:ok, thing}" in do block)
	innerOccs := FindVariableOccurrences(src, 4, 12)
	// Should find occurrences within the with scope only:
	// line 3 ({:ok, thing} pattern) and line 4 ({:ok, thing} body)
	if len(innerOccs) != 2 {
		t.Fatalf("expected 2 occurrences of inner 'thing', got %d: %+v", len(innerOccs), innerOccs)
	}
	for _, occ := range innerOccs {
		if occ.Line != 3 && occ.Line != 4 {
			t.Errorf("inner occ: expected line 3 or 4, got %d", occ.Line)
		}
	}
}

func TestFindVariableOccurrences_WithBlockExpressionSide(t *testing.T) {
	src := []byte(`defmodule M do
  def f() do
    thing = nil
    with {:ok, thing} <- fetch(thing) do
      thing
    end
    thing = :something
  end
end`)

	// Cursor on outer "thing" (line 2: "thing = nil")
	occs := FindVariableOccurrences(src, 2, 4)
	// Should find: line 2 (thing = nil), line 3 fetch(thing), line 6 (thing = :something)
	// Should NOT find: {:ok, thing} pattern or thing in do block
	if len(occs) != 3 {
		t.Fatalf("expected 3 occurrences of outer 'thing', got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 2 {
		t.Errorf("occ[0]: expected line 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 3 || occs[1].StartCol != 31 {
		t.Errorf("occ[1]: expected line 3 col 31 (fetch(thing)), got line %d col %d", occs[1].Line, occs[1].StartCol)
	}
	if occs[2].Line != 6 {
		t.Errorf("occ[2]: expected line 6, got %d", occs[2].Line)
	}
}

func TestFindVariableOccurrences_WithMultiClauseSequential(t *testing.T) {
	src := []byte(`defmodule M do
  def f() do
    thing = nil
    with {:ok, thing} <- fetch(thing),
         {:ok, other} <- bar(thing) do
      thing
    end
    thing = :something
  end
end`)

	// Cursor on outer "thing" (line 2)
	occs := FindVariableOccurrences(src, 2, 4)
	// Should find: line 2 (thing = nil), line 3 fetch(thing), line 7 (thing = :something)
	// Should NOT find: bar(thing) on line 4 — that refs the rebound thing
	// Should NOT find: thing in pattern or do block
	if len(occs) != 3 {
		t.Fatalf("expected 3 occurrences of outer 'thing', got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 2 {
		t.Errorf("occ[0]: expected line 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 3 {
		t.Errorf("occ[1]: expected line 3 (fetch(thing)), got line %d col %d", occs[1].Line, occs[1].StartCol)
	}
	if occs[2].Line != 7 {
		t.Errorf("occ[2]: expected line 7, got %d", occs[2].Line)
	}
}

func TestFindVariableOccurrences_ForBlockScope(t *testing.T) {
	src := []byte(`defmodule M do
  def f(items) do
    item = "default"
    for item <- items do
      process(item)
    end
    use(item)
  end
end`)

	// Cursor on outer "item" (line 2)
	occs := FindVariableOccurrences(src, 2, 4)
	// Should find: line 2 (item = "default") and line 6 (use(item))
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of outer 'item', got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 2 {
		t.Errorf("occ[0]: expected line 2, got %d", occs[0].Line)
	}
	if occs[1].Line != 6 {
		t.Errorf("occ[1]: expected line 6, got %d", occs[1].Line)
	}
}

func TestFindVariablesInScope(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(user, account) do
    name = user.name
    email = user.email
    na
  end

  def other(x) do
    y = x + 1
  end
end`)

	// Cursor on "na" at line 4, col 5
	vars := FindVariablesInScope(src, 4, 5)
	if vars == nil {
		t.Fatal("expected variables, got nil")
	}

	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	// Should include function params and local variables
	for _, expected := range []string{"user", "account", "name", "email"} {
		if !varSet[expected] {
			t.Errorf("expected variable %q in scope", expected)
		}
	}

	// Should not include variables from other functions
	if varSet["x"] || varSet["y"] {
		t.Error("should not include variables from other function scopes")
	}

	// Should not include function names
	if varSet["process"] || varSet["other"] {
		t.Error("should not include function names")
	}
}

func TestFindVariablesInScope_CaseClauseBoundary(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(user) do
    claims = user.claims

    case fetch("test") do
      {:ok, company} ->
        company

      _ -> :error
    end
  end
end`)

	// Cursor in the second case clause (line 9: "_ -> :error")
	vars := FindVariablesInScope(src, 9, 14)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	// Should include function params and top-level variables
	if !varSet["user"] {
		t.Error("expected function param 'user'")
	}
	if !varSet["claims"] {
		t.Error("expected top-level variable 'claims'")
	}
	// Should NOT include variables from the other case clause
	if varSet["company"] {
		t.Error("should not include 'company' from a different case clause")
	}
}

func TestFindVariablesInScope_WithVariablesVisible(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(user) do
    with {:ok, thing} = fetch("something") do
      thi
    end
  end
end`)

	// Cursor inside the with's do block (line 3: "thi")
	vars := FindVariablesInScope(src, 3, 9)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	// Should include variables from the with pattern when cursor is inside
	if !varSet["thing"] {
		t.Error("expected 'thing' from with pattern to be visible in do block")
	}
	if !varSet["user"] {
		t.Error("expected function param 'user'")
	}
}

func TestFindVariablesInScope_WithVariablesDontLeak(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(user) do
    with {:ok, thing} <- fetch("something") do
      thing
    end

    us
  end
end`)

	// Cursor AFTER the with block (line 6: "us")
	vars := FindVariablesInScope(src, 6, 6)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	if !varSet["user"] {
		t.Error("expected function param 'user'")
	}
	if varSet["thing"] {
		t.Error("should not include 'thing' — with bindings don't leak to outer scope")
	}
}

func TestFindVariablesInScope_ForVariablesDontLeak(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(list) do
    for item <- list do
      item
    end

    li
  end
end`)

	// Cursor AFTER the for block (line 6: "li")
	vars := FindVariablesInScope(src, 6, 6)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	if !varSet["list"] {
		t.Error("expected function param 'list'")
	}
	if varSet["item"] {
		t.Error("should not include 'item' — for bindings don't leak to outer scope")
	}
}

func TestFindVariablesInScope_IfVariablesDontLeak(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(flag) do
    if flag do
      result = compute()
      result
    end

    fl
  end
end`)

	// Cursor AFTER the if block (line 7: "fl")
	vars := FindVariablesInScope(src, 7, 6)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	if !varSet["flag"] {
		t.Error("expected function param 'flag'")
	}
	if varSet["result"] {
		t.Error("should not include 'result' — if bindings don't leak to outer scope")
	}
}

func TestFindVariablesInScope_OnlyAboveCursor(t *testing.T) {
	src := []byte(`defmodule MyModule do
  def process(user) do
    name = user.name
    na
    email = user.email
  end
end`)

	// Cursor on line 3 ("na") — should see name and user, but NOT email
	vars := FindVariablesInScope(src, 3, 6)
	varSet := make(map[string]bool)
	for _, v := range vars {
		varSet[v] = true
	}

	if !varSet["user"] {
		t.Error("expected function param 'user'")
	}
	if !varSet["name"] {
		t.Error("expected 'name' defined above cursor")
	}
	if varSet["email"] {
		t.Error("should not include 'email' defined below cursor")
	}
}

func TestFindVariablesInScope_OutsideFunction(t *testing.T) {
	src := []byte(`defmodule MyModule do
  @attr "hello"
  at
end`)

	// Cursor at module level, not inside a function
	vars := FindVariablesInScope(src, 2, 4)
	if vars != nil {
		t.Errorf("expected nil for cursor outside function, got %v", vars)
	}
}

func TestFindVariableOccurrences_FullWorkerFile(t *testing.T) {
	// Full file structure matching the real worker pattern that fails
	src := []byte(`defmodule MyApp.Workers.SendEmailWorker do
  @moduledoc """
  Worker to send an email.
  """

  use MyApp.Worker,
    owner: :payments,
    queue: :default,
    max_attempts: 4,
    unique: [
      period: {1, :minutes},
      keys: [:resource_slug, :resource_type],
      states: [:available, :scheduled, :executing, :retryable]
    ]

  alias MyApp.Mailer.PaymentEmail
  alias MyApp.Resources
  alias MyApp.Resources.Resource
  alias MyApp.Remittances
  alias MyApp.Remittances.Schemas.Remittance

  args_schema do
    field :resource_slug, :uuid, required: true
    field :resource_type, :string, required: true
    field :payment_intent_id, :string, required: true
    field :transfer_amount, :map, required: true
  end

  @spec enqueue!(Resource.t() | Remittance.t(), String.t(), Money.t()) :: :ok
  def enqueue!(resource, payment_intent_id, transfer_amount) do
    %{
      resource_slug: resource.slug,
      resource_type: resource_type(resource),
      payment_intent_id: payment_intent_id,
      transfer_amount: %{amount: transfer_amount.amount, currency: transfer_amount.currency}
    }
    |> __MODULE__.new()
    |> MyApp.Oban.safe_insert()

    :ok
  end

  defp resource_type(%Resource{}), do: "resource"
  defp resource_type(%Remittance{}), do: "remittance"

  @impl true
  def process(%Job{
        args: %__MODULE__{
          resource_slug: resource_slug,
          resource_type: resource_type,
          payment_intent_id: payment_intent_id,
          transfer_amount: transfer_amount
        }
      }) do
    with {:ok, resource} <- fetch_resource_by_slug(resource_slug, resource_type) do
      transfer_amount = Money.new(transfer_amount["currency"], transfer_amount["amount"])
      {:ok, _} = PaymentEmail.deliver(resource, payment_intent_id, transfer_amount)

      :ok
    end
  end

  defp fetch_resource_by_slug(slug, "resource") do
    Resources.get_resource_by_slug(slug)
  end

  defp fetch_resource_by_slug(slug, "remittance") do
    Remittances.get_remittance_by(%{slug: slug})
  end

  @impl true
  defdelegate backoff(job), to: MyApp.Oban.EmailWorker
end`)

	root, cleanup := parseElixir(src)
	if root == nil {
		t.Fatal("failed to parse")
	}
	defer cleanup()

	// Find the actual line for "transfer_amount = Money.new" in this test source
	lines := strings.Split(string(src), "\n")
	transferLine := -1
	for i, line := range lines {
		if strings.Contains(line, "transfer_amount = Money.new") {
			transferLine = i
			break
		}
	}
	if transferLine < 0 {
		t.Fatal("could not find transfer_amount = Money.new line")
	}
	t.Logf("transfer_amount rebind is at line %d: %q", transferLine, lines[transferLine])

	occs := FindVariableOccurrences(src, uint(transferLine), 6)
	t.Logf("transfer_amount from line %d col 6: %d occs: %+v", transferLine, len(occs), occs)
	if occs == nil {
		t.Fatal("expected variable occurrences for 'transfer_amount', got nil")
	}
	if len(occs) < 2 {
		t.Fatalf("expected multiple occurrences of 'transfer_amount', got %d: %+v", len(occs), occs)
	}
}

func TestFindVariableOccurrences_BareZeroArityCallNotVariable(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def run() do
    result = do_work()
    validate
  end

  defp do_work, do: :ok
  defp validate, do: :ok
end`)

	// Cursor on "validate" (line 3) — a zero-arity function call without parens.
	// Not defined as a variable anywhere in the scope, so returns nil to let
	// function reference lookup handle it.
	occs := FindVariableOccurrences(src, 3, 4)
	if occs != nil {
		t.Errorf("expected nil for bare function call 'validate', got %d: %+v", len(occs), occs)
	}
}

func TestFindTokenOccurrences_Function(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    result = process(data)
    # process is called here
    "process string"
  end
end`)

	occs := FindTokenOccurrences(src, "process")
	// Should find: def process (line 1) and process(data) call (line 2)
	// Should NOT find: comment (line 3) or string (line 4)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of 'process', got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 1 {
		t.Errorf("first occurrence should be line 1 (def), got %d", occs[0].Line)
	}
	if occs[1].Line != 2 {
		t.Errorf("second occurrence should be line 2 (call), got %d", occs[1].Line)
	}
}

func TestFindTokenOccurrences_Module(t *testing.T) {
	src := []byte(`defmodule MyApp.Accounts do
  alias MyApp.Repo

  def list do
    Repo.all(User)
  end
end`)

	occs := FindTokenOccurrences(src, "Repo")
	// alias MyApp.Repo (line 1) and Repo.all (line 4)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of 'Repo', got %d: %+v", len(occs), occs)
	}
}

func TestFindVariableOccurrences_DestructuredStructParams(t *testing.T) {
	// Mirrors the pattern from a real Oban worker with %Job{args: %__MODULE__{...}} destructuring
	src := []byte(`defmodule MyApp.Workers.EmailWorker do
  use MyApp.Worker

  def process(%Job{
        args: %__MODULE__{
          resource_slug: resource_slug,
          resource_type: resource_type,
          payment_intent_id: payment_intent_id,
          transfer_amount: transfer_amount
        }
      }) do
    with {:ok, resource} <- fetch_resource(resource_slug, resource_type) do
      transfer_amount = Money.new(transfer_amount["currency"], transfer_amount["amount"])
      {:ok, _} = deliver(resource, payment_intent_id, transfer_amount)
      :ok
    end
  end

  defp fetch_resource(slug, type) do
    {:ok, %{slug: slug, type: type}}
  end
end`)

	// Cursor on "resource_slug" in the with expression (line 11)
	// "    with {:ok, resource} <- fetch_resource(resource_slug, resource_type) do"
	occs := FindVariableOccurrences(src, 11, 45)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of 'resource_slug', got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 5 {
		t.Errorf("occ[0]: expected line 5 (struct pattern), got line %d", occs[0].Line)
	}
	if occs[1].Line != 11 {
		t.Errorf("occ[1]: expected line 11 (with expression), got line %d", occs[1].Line)
	}

	// Cursor on "resource" in the with pattern (line 11)
	// Should be scoped to the with block only
	occs = FindVariableOccurrences(src, 11, 17)
	if len(occs) != 2 {
		t.Fatalf("expected 2 occurrences of 'resource' (pattern + usage), got %d: %+v", len(occs), occs)
	}
	if occs[0].Line != 11 {
		t.Errorf("occ[0]: expected line 11 ({:ok, resource}), got line %d", occs[0].Line)
	}
	if occs[1].Line != 13 {
		t.Errorf("occ[1]: expected line 13 (deliver(resource, ...)), got line %d", occs[1].Line)
	}

	// Cursor on "transfer_amount" inside the with do block (line 12)
	// Should find ALL occurrences in the process function since with doesn't rebind it
	occs = FindVariableOccurrences(src, 12, 6)
	if len(occs) != 5 {
		t.Fatalf("expected 5 occurrences of 'transfer_amount', got %d: %+v", len(occs), occs)
	}

	// Cursor on "fetch_resource" (a function call, NOT a variable)
	occs = FindVariableOccurrences(src, 11, 30)
	if occs != nil {
		t.Errorf("expected nil for function call 'fetch_resource', got %d: %+v", len(occs), occs)
	}

	// Cursor on "deliver" inside with do block (a function call, NOT a variable)
	occs = FindVariableOccurrences(src, 13, 19)
	if occs != nil {
		t.Errorf("expected nil for function call 'deliver', got %d: %+v", len(occs), occs)
	}
}

func TestFindVariableOccurrences_PinnedVariable(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def run do
    slug = "test_slug"
    expect(Repo, :get_by_slug, fn ^slug -> {:ok, %{}} end)
    slug
  end
end`)

	// Cursor on "slug" at line 2, col 4 (the assignment)
	occs := FindVariableOccurrences(src, 2, 4)
	if len(occs) == 0 {
		t.Fatal("expected occurrences of 'slug'")
	}

	// Should find: assignment (line 2), pinned reference (line 3), usage (line 4)
	lines := make(map[uint]bool)
	for _, occ := range occs {
		lines[occ.Line] = true
	}
	if !lines[2] {
		t.Error("expected occurrence on line 2 (assignment)")
	}
	if !lines[3] {
		t.Error("expected occurrence on line 3 (pinned ^slug in fn)")
	}
	if !lines[4] {
		t.Error("expected occurrence on line 4 (usage)")
	}
}

func TestFindVariableOccurrences_PinnedDoesNotCreateNewBinding(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def run do
    x = 1
    Enum.find([1, 2, 3], fn ^x -> true; _ -> false end)
    x + 1
  end
end`)

	// Cursor on "x" at line 2, col 4 (the assignment)
	occs := FindVariableOccurrences(src, 2, 4)

	// The fn ^x -> ... clause uses pin, so x is NOT rebound.
	// All three lines should be included.
	if len(occs) < 3 {
		t.Errorf("expected at least 3 occurrences (assign + pin + usage), got %d", len(occs))
	}
}

func TestFindVariableOccurrences_PinnedThenReboundInBody(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def run do
    slug = "outer"
    expect(Mock, :fn, fn ^slug, %{}, ^user ->
      slug = nil
      {:ok, slug}
    end)
    slug
  end
end`)

	// Cursor on outer "slug" at line 2, col 4
	occs := FindVariableOccurrences(src, 2, 4)

	lines := make(map[uint]bool)
	for _, occ := range occs {
		lines[occ.Line] = true
	}

	// Outer assignment and pin reference should be collected
	if !lines[2] {
		t.Error("expected occurrence on line 2 (outer assignment)")
	}
	if !lines[3] {
		t.Error("expected occurrence on line 3 (pinned ^slug in fn params)")
	}
	// Final usage after the expect should be collected
	if !lines[7] {
		t.Error("expected occurrence on line 7 (usage after expect)")
	}
	// Inside the fn body: slug = nil and {:ok, slug} should NOT be collected
	// (they refer to the local rebinding, not the outer variable)
	if lines[4] {
		t.Error("slug = nil (line 4) is a body rebind — should NOT be collected")
	}
	if lines[5] {
		t.Error("{:ok, slug} (line 5) follows a body rebind — should NOT be collected")
	}
}

func TestFindVariableOccurrences_RenameInsideStabBodyRebind(t *testing.T) {
	// Cursor on inner `slug` (the rebind inside fn body) should NOT rename outer slug.
	src := []byte(`defmodule MyApp do
  test "example", %{user: user} do
    slug = Ecto.UUID.generate()

    expect(Mock, :fn, fn ^slug, %{}, ^user ->
      slug = nil
      {:ok, slug}
    end)

    slug
  end
end`)

	// Cursor on the inner `slug = nil` at line 5, col 6
	occs := FindVariableOccurrences(src, 5, 6)

	lines := make(map[uint]bool)
	for _, occ := range occs {
		lines[occ.Line] = true
	}

	// Should find: the rebind (line 5) and usage after it (line 6)
	if !lines[5] {
		t.Error("expected occurrence on line 5 (inner rebind)")
	}
	if !lines[6] {
		t.Error("expected occurrence on line 6 (usage after rebind)")
	}
	// Should NOT rename the outer slug or the pin reference
	if lines[2] {
		t.Error("outer slug assignment (line 2) should NOT be renamed")
	}
	if lines[4] {
		t.Error("^slug pin (line 4) should NOT be renamed")
	}
	if lines[9] {
		t.Error("outer slug usage (line 9) should NOT be renamed")
	}
}

func TestFindVariableOccurrences_MixedPinnedAndUnpinnedArgs(t *testing.T) {
	// fn with mixed args: `^pinned` (outer ref) and `other` (new binding).
	// Renaming `other` from inside the fn should be scoped to the fn.
	// Renaming `pinned` from outside should include the ^pinned pin reference.
	src := []byte(`defmodule MyApp do
  def run do
    pinned = "value"
    other = "outer"

    Enum.each(list, fn ^pinned, other ->
      IO.puts(other)
    end)

    other
  end
end`)

	// Renaming `other` from the usage inside the fn (line 6, col 14 — on `other` in `IO.puts(other)`)
	innerOccs := FindVariableOccurrences(src, 6, 14)
	innerLines := make(map[uint]bool)
	for _, occ := range innerOccs {
		innerLines[occ.Line] = true
	}
	// Should find the fn param `other` (line 5) and usage (line 6)
	if !innerLines[5] && !innerLines[6] {
		t.Errorf("expected inner `other` in fn scope, got lines %v", innerLines)
	}
	// Should NOT find the outer `other`
	if innerLines[3] {
		t.Error("outer `other` assignment (line 3) should NOT be included")
	}
	if innerLines[9] {
		t.Error("outer `other` usage (line 9) should NOT be included")
	}

	// Renaming `pinned` from outside (line 2, col 4) — should include ^pinned pin
	outerOccs := FindVariableOccurrences(src, 2, 4)
	outerLines := make(map[uint]bool)
	for _, occ := range outerOccs {
		outerLines[occ.Line] = true
	}
	if !outerLines[2] {
		t.Error("expected outer `pinned` assignment (line 2)")
	}
	if !outerLines[5] {
		t.Error("expected ^pinned pin reference (line 5) to be included")
	}
}

func TestFindVariableOccurrences_WithRightSideIsOuterScope(t *testing.T) {
	// Right side of <- in `with` is evaluated in the outer scope.
	// Renaming `slug` on the right side should NOT rename the left-side binding.
	src := []byte(`defmodule MyApp do
  def run do
    slug = nil
    with {:ok, slug} <- fetch_something(slug) do
      use(slug)
    end
  end
end`)

	// Cursor on right-side `slug` in `fetch_something(slug)` (line 3, col 40)
	// "    with {:ok, slug} <- fetch_something(slug) do"
	//      0              15                  40
	occs := FindVariableOccurrences(src, 3, 40)
	lines := make(map[uint]bool)
	for _, occ := range occs {
		lines[occ.Line] = true
	}
	// Right-side slug and the outer assignment should be included
	if !lines[2] {
		t.Error("expected outer slug assignment (line 2)")
	}
	if !lines[3] {
		t.Error("expected right-side slug in with (line 3)")
	}
	// Left-side slug (new binding) should NOT be included
	for _, occ := range occs {
		if occ.Line == 3 && occ.StartCol == 11 { // col 11 = left-side slug position
			t.Error("left-side slug (new binding) should NOT be renamed with outer slug")
		}
	}
	// do_block slug (new binding's scope) should NOT be included
	if lines[4] {
		t.Error("do_block slug (line 4) uses new binding — should NOT be included")
	}
}

func TestFindVariableOccurrences_WithLeftSideDoesNotIncludeRightSide(t *testing.T) {
	// Cursor on left-side `slug` in `{:ok, slug} <- fetch_something(slug)`.
	// Renaming should include the new binding (left side) and do_block usages,
	// but NOT the right-side slug which comes from the outer scope.
	src := []byte(`defmodule MyApp do
  def run do
    slug = nil
    with {:ok, slug} <- fetch_something(slug) do
      slug = :ok
      {:ok, slug}
    end
  end
end`)

	// Cursor on left-side `slug` in `{:ok, slug}` (line 3, col 15)
	// "    with {:ok, slug} <- fetch_something(slug) do"
	//      0             15
	occs := FindVariableOccurrences(src, 3, 15)
	lines := make(map[uint]bool)
	for _, occ := range occs {
		lines[occ.Line] = true
	}
	// Left-side binding and do_block usages should be included
	if !lines[3] {
		t.Error("expected left-side slug occurrence (line 3)")
	}
	if !lines[4] {
		t.Error("expected do_block slug rebind (line 4)")
	}
	if !lines[5] {
		t.Error("expected do_block slug usage (line 5)")
	}
	// Right-side slug and outer slug should NOT be included
	if lines[2] {
		t.Error("outer slug assignment (line 2) should NOT be renamed")
	}
	// Verify there's only one occurrence on line 3 (the left-side, not the right-side)
	line3count := 0
	for _, occ := range occs {
		if occ.Line == 3 {
			line3count++
		}
	}
	if line3count != 1 {
		t.Errorf("expected exactly 1 occurrence on line 3 (left side only), got %d", line3count)
	}
}

func TestFindVariableOccurrences_WithRightSideOfEquals(t *testing.T) {
	// Right side of `=` in `with x = expr do` is outer scope;
	// left side is the new binding.
	src := []byte(`defmodule MyApp do
  def run do
    x = 1
    with y = x * 2 do
      use(x)
      use(y)
    end
    x
  end
end`)

	// Cursor on `x` in the right side `x * 2` (line 3, col 13)
	// "    with y = x * 2 do"
	//      0        9 13
	occs := FindVariableOccurrences(src, 3, 13)
	lines := make(map[uint]bool)
	for _, occ := range occs {
		lines[occ.Line] = true
	}
	if !lines[2] {
		t.Error("expected outer x assignment (line 2)")
	}
	if !lines[3] {
		t.Error("expected x in with right side (line 3)")
	}
	// do_block x should NOT be renamed (it references the outer x, true, but
	// the scope boundary prevents conflating with the with-expression x)
	// NOTE: the outer x IS visible in the do_block, but this test mainly checks
	// the left-side y is not mistakenly included
	if lines[6] {
		t.Error("y (line 6) should NOT be included in x rename")
	}
}

func TestFindVariableOccurrences_WithMultiClause(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def run do
    slug = nil
    with {:ok, slug} <- fetch_slug(slug),
         {:ok, slug} <- transform_slug(slug) do
      use(slug)
    end
  end
end`)

	// "    slug = nil"                             → line 2
	// "    with {:ok, slug} <- fetch_slug(slug),"  → line 3
	// "         {:ok, slug} <- transform_slug(slug) do" → line 4
	// "      use(slug)"                            → line 5

	// Case 1: outer slug (line 2, col 4)
	// Should include: outer slug + clause 0 rhs (fetch_slug(slug))
	// Should NOT include: clause 0 lhs, clause 1 rhs, clause 1 lhs, do_block
	t.Run("outer slug", func(t *testing.T) {
		occs := FindVariableOccurrences(src, 2, 4)
		lines := make(map[uint]bool)
		for _, occ := range occs {
			lines[occ.Line] = true
		}
		if !lines[2] {
			t.Error("expected outer slug (line 2)")
		}
		if !lines[3] {
			t.Error("expected clause 0 rhs fetch_slug(slug) (line 3)")
		}
		if lines[4] {
			t.Error("clause 1 rhs should NOT be included (uses clause 0 binding)")
		}
		if lines[5] {
			t.Error("do_block should NOT be included")
		}
	})

	// Case 2: clause 0 lhs slug (line 3, col 15)
	// "    with {:ok, slug} <- fetch_slug(slug),"
	//           at col 15 = left-side slug
	// Should include: clause 0 lhs + clause 1 rhs (transform_slug(slug))
	// Should NOT include: outer slug, clause 0 rhs, clause 1 lhs, do_block
	t.Run("clause 0 lhs", func(t *testing.T) {
		occs := FindVariableOccurrences(src, 3, 15)
		lines := make(map[uint]bool)
		for _, occ := range occs {
			lines[occ.Line] = true
		}
		if !lines[3] {
			t.Error("expected clause 0 lhs (line 3)")
		}
		if !lines[4] {
			t.Error("expected clause 1 rhs transform_slug(slug) (line 4)")
		}
		if lines[2] {
			t.Error("outer slug (line 2) should NOT be included")
		}
		if lines[5] {
			t.Error("do_block should NOT be included (clause 1 lhs rebinds)")
		}
		// Ensure only one occurrence on line 3 (lhs only, not rhs)
		line3count := 0
		for _, occ := range occs {
			if occ.Line == 3 {
				line3count++
			}
		}
		if line3count != 1 {
			t.Errorf("expected 1 occurrence on line 3 (lhs only), got %d", line3count)
		}
	})

	// Case 3: clause 1 lhs slug (line 4, col 15)
	// "         {:ok, slug} <- transform_slug(slug) do"
	//                at col 15 = left-side slug (with 9 spaces indent)
	// Should include: clause 1 lhs + do_block use(slug)
	// Should NOT include: outer slug, clause 0 anything, clause 1 rhs
	t.Run("clause 1 lhs", func(t *testing.T) {
		occs := FindVariableOccurrences(src, 4, 15)
		lines := make(map[uint]bool)
		for _, occ := range occs {
			lines[occ.Line] = true
		}
		if !lines[4] {
			t.Error("expected clause 1 lhs (line 4)")
		}
		if !lines[5] {
			t.Error("expected do_block use(slug) (line 5)")
		}
		if lines[2] || lines[3] {
			t.Error("outer slug and clause 0 should NOT be included")
		}
		// Only the lhs occurrence on line 4, not the rhs
		line4count := 0
		for _, occ := range occs {
			if occ.Line == 4 {
				line4count++
			}
		}
		if line4count != 1 {
			t.Errorf("expected 1 occurrence on line 4 (lhs only), got %d", line4count)
		}
	})

	// Case 4: do_block slug (line 5, col 10)
	// Should include: clause 1 lhs + do_block
	// Same as Case 3 (both reference clause 1's binding)
	t.Run("do_block slug", func(t *testing.T) {
		occs := FindVariableOccurrences(src, 5, 10)
		lines := make(map[uint]bool)
		for _, occ := range occs {
			lines[occ.Line] = true
		}
		if !lines[4] {
			t.Error("expected clause 1 lhs (line 4)")
		}
		if !lines[5] {
			t.Error("expected do_block slug (line 5)")
		}
		if lines[2] || lines[3] {
			t.Error("outer slug and clause 0 should NOT be included")
		}
	})
}

func TestFindVariableOccurrences_PinnedWithNoBodyRebind(t *testing.T) {
	// fn ^x -> use(x) end — no rebind in body, x is a closure reference.
	// Renaming outer x should include both the pin and the closure reference.
	src := []byte(`defmodule MyApp do
  def run do
    x = 1
    Enum.each(list, fn ^x ->
      IO.puts(x)
    end)
    x
  end
end`)

	// Cursor on outer x at line 2
	occs := FindVariableOccurrences(src, 2, 4)
	lines := make(map[uint]bool)
	for _, occ := range occs {
		lines[occ.Line] = true
	}
	if !lines[2] {
		t.Error("expected outer x assignment (line 2)")
	}
	if !lines[3] {
		t.Error("expected ^x pin (line 3)")
	}
	if !lines[4] {
		t.Error("expected x closure reference in fn body (line 4)")
	}
	if !lines[6] {
		t.Error("expected x usage after fn (line 6)")
	}
}

func TestFindVariableOccurrences_MultipleClausesWithPins(t *testing.T) {
	// case with pinned and non-pinned clauses
	src := []byte(`defmodule MyApp do
  def run(expected) do
    result = fetch()
    case result do
      ^expected -> :matched
      other -> :no_match
    end
  end
end`)

	// Cursor on `expected` at line 1, col 11 (the param name: `  def run(expected)`)
	occs := FindVariableOccurrences(src, 1, 11)
	lines := make(map[uint]bool)
	for _, occ := range occs {
		lines[occ.Line] = true
	}
	if !lines[1] {
		t.Error("expected param `expected` (line 1)")
	}
	if !lines[4] {
		t.Error("expected ^expected pin in case (line 4)")
	}
}

func TestFindVariableOccurrences_BodyRebindMidFunction(t *testing.T) {
	// fn ^slug -> first_use(slug); slug = new_val; second_use(slug) end
	// Cursor on inner slug (after rebind) should scope to fn body only.
	// Cursor on first_use(slug) — before the rebind — is ambiguous but
	// in practice treated as outer scope (no rebind before cursor).
	src := []byte(`defmodule MyApp do
  def run do
    slug = "outer"
    expect(Mock, :call, fn ^slug ->
      result = do_thing(slug)
      slug = "inner"
      other(slug)
    end)
    slug
  end
end`)

	// Cursor on `slug = "inner"` (line 5, col 6) — inner rebind
	innerOccs := FindVariableOccurrences(src, 5, 6)
	innerLines := make(map[uint]bool)
	for _, occ := range innerOccs {
		innerLines[occ.Line] = true
	}
	if !innerLines[5] {
		t.Error("expected inner rebind (line 5)")
	}
	if !innerLines[6] {
		t.Error("expected inner usage after rebind (line 6)")
	}
	if innerLines[2] {
		t.Error("outer slug (line 2) should NOT be included")
	}
	if innerLines[3] {
		t.Error("^slug pin (line 3) should NOT be included")
	}
}

func TestFindTokenOccurrences_SkipsAtoms(t *testing.T) {
	src := []byte(`defmodule MyApp do
  def process(data) do
    Keyword.get(data, :process, nil)
  end
end`)

	occs := FindTokenOccurrences(src, "process")
	// Should find: def process (line 1)
	// Should NOT find: :process atom (line 2) — tree-sitter parses atoms differently
	if len(occs) != 1 {
		t.Fatalf("expected 1 occurrence of 'process' (not atom), got %d: %+v", len(occs), occs)
	}
}

func TestFindVariableOccurrences_DefpLine(t *testing.T) {
	src := []byte(`defmodule MyApp.Worker do
  def enqueue(resource) do
    %{
      resource_type: resource_type(resource)
    }
  end

  defp resource_type(%{type: t}), do: t
end`)

	// Line 7 is "defp resource_type(%{type: t}), do: t"
	// Col 7 is on the 'r' in 'resource_type'
	occs := FindVariableOccurrences(src, 7, 7)
	if occs != nil {
		t.Errorf("expected nil on defp line, got %d occurrences", len(occs))
		for _, occ := range occs {
			t.Logf("  Line %d, col %d-%d", occ.Line, occ.StartCol, occ.EndCol)
		}
	}
}
