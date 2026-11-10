defmodule Shop.Schema do
  @moduledoc false
  defmacro __using__(_opts) do
    quote do
      import Shop.Accounts, only: [fetch: 1]
    end
  end
end
