defmodule Shop.Admin do
  @moduledoc false
  import Shop.Accounts,
    except: [
      token: 1,
      count: 0
    ]

  def token(id), do: {:admin, id}
  def count, do: 0
  def run(id), do: {fetch(id), token(id), count(), fetch_all([id])}
end
