defmodule Shop.Job do
  @moduledoc false
  @behaviour Shop.Worker

  @impl true
  @spec perform(term) :: :ok
  def perform(_arg), do: :ok

  @impl Shop.Worker
  def cancel(_arg), do: :ok

  @impl true
  def go(_x, _opts \\ []), do: :ok

  def go(a, b, c), do: {a, b, c}
end
