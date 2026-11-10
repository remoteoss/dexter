defmodule Shop.Order do
  @moduledoc false
  use Shop.Schema

  def buyer(id), do: fetch(id)
end
