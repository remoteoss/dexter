defmodule Shop.Worker do
  @moduledoc false
  @callback perform(term) :: :ok
  @callback cancel(term) :: :ok
  @callback go(term, keyword) :: :ok
  @optional_callbacks cancel: 1

  @spec run(module, term) :: :ok
  def run(impl, arg), do: impl.perform(arg)

  def run_all(impls, arg), do: Enum.each(impls, fn impl -> arg |> impl.perform() end)
end
