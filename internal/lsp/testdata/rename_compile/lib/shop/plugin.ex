defmodule Shop.Plugin do
  @moduledoc false
  @behaviour DepBehaviour

  @impl true
  def handle(x), do: x
end
