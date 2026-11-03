defmodule Shop.Report do
  @moduledoc false
  alias Shop.Accounts.{
    Token,
    User
  }

  def build(id), do: {Token.new(id), %User{id: id}}
end
