defmodule Shop.Repo do
  @moduledoc false
  def get(schema, id, _opts), do: struct(schema, id: id, name: "user #{id}")
end
