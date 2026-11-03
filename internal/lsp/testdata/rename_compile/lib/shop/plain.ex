defmodule Shop.Plain do
  @moduledoc false
  import Shop.Accounts

  def one(id), do: fetch(id)

  def two(id) do
    id
    |> fetch()
    |> Map.get(:name)
  end

  def cap, do: &fetch/1
  def kw, do: fetch(1, a: 1, b: 2)
  def piped(id), do: id |> fetch

  def in_case(id) do
    case fetch(id) do
      result -> result
    end
  end
  def local, do: fetch(1, 2, 3)

  defp fetch(a, b, c), do: {a, b, c}
end
