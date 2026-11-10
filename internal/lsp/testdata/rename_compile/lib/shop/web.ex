defmodule Shop.Web do
  @moduledoc false
  import Shop.Accounts,
    only: [
      fetch: 1,
      fetch_all: 1
    ]

  alias Shop.Accounts, as: Acc

  def show(id), do: id |> fetch()
  def list(ids), do: fetch_all(ids)
  def caps, do: {&fetch/1, &Shop.Accounts.fetch/1, &Acc.fetch/2}
  def mixed(id), do: {Acc.fetch(id), Shop.Accounts.Helpers.fetch(id), Acc.count()}
end
