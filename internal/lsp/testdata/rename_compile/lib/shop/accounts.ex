defmodule Shop.Accounts do
  @moduledoc """
  Account functions. Shop.Accounts.fetch/1 is the entry; __MODULE__.fetch(1) works too.
  """
  alias Shop.Accounts.{Token, User}
  alias Shop.Repo, as: Store
  alias __MODULE__.Helpers

  @doc "Fetch a user; see fetch/1 and fetch/2."
  @spec fetch(integer, keyword) :: map
  def fetch(id, opts \\ []) do
    # fetch reads from the store
    Store.get(User, id, opts) |> Helpers.wrap() |> elem(1)
  end

  def fetch_all(ids), do: Enum.map(ids, &fetch/1)
  def fetch_pairs(ids), do: Enum.map(ids, &fetch(&1, []))
  def fetch_self(id), do: {__MODULE__.fetch(id), &__MODULE__.fetch/1}
  def label(id), do: "user #{fetch(id).name} via fetch"
  def sigil_text, do: ~s(fetch is a word)
  def token(id), do: Token.new(id)
  def count, do: count(0)
  def count(n), do: n + 1

  defmodule Helpers do
    @moduledoc false
    def wrap(x), do: {:ok, x}
    def fetch(x), do: x
    def use_own, do: fetch(1)
  end

  def after_nested(id), do: fetch(id) |> Map.get(:name)

  def describe(fetch) do
    {fetch, fetch(2)}
  end
end
