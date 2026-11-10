defmodule D.ImplBoth do
  @moduledoc false
  @behaviour D.Beh
  @behaviour D.Other

  @impl D.Beh
  def go(x, opts), do: {x, opts}

  @impl D.Other
  def go(x), do: go(x, [])
end
