defmodule Shop.DelegatingUser do
  @moduledoc false
  use Shop.Delegating

  def go(id), do: {lookup(id), find_one(id)}
end
