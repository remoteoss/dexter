defmodule Shop.Typed do
  @moduledoc false
  @type count :: integer()
  @spec count(count()) :: count()
  def count(x), do: x
end

defmodule Shop.TypedUser do
  @moduledoc false
  @spec go(Shop.Typed.count()) :: Shop.Typed.count()
  def go(x), do: Shop.Typed.count(x)
end
