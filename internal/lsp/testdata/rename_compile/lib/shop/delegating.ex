defmodule Shop.Delegating do
  @moduledoc false
  defmacro __using__(_opts) do
    quote do
      defdelegate lookup(id), to: Shop.Delegating, as: :find_one
      defdelegate find_one(id), to: Shop.Delegating
    end
  end

  def find_one(id), do: {:found, id}
end
