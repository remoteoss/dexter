defmodule Shop.Facade do
  @moduledoc false
  defdelegate find(id), to: Shop.Accounts, as: :fetch
  defdelegate fetch_all(ids), to: Shop.Accounts
end
