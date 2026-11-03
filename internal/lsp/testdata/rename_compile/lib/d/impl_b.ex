defmodule D.ImplB do
  @moduledoc false
  @behaviour D.Beh

  @impl true
  def go(x, opts), do: {x, opts}

  def go(x), do: x
end
