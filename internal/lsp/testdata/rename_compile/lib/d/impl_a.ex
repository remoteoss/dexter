defmodule D.ImplA do
  @moduledoc false
  @behaviour D.Beh

  @impl true
  def go(x, opts \\ []), do: {x, opts}
end
