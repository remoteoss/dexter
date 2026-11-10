defmodule Shop.BareSchemaUser do
  @moduledoc false
  use Shop.Schema

  # The only call of the imported fetch/1 has no parentheses and does not
  # start the line.
  def c(id) do
    value = fetch id
    value
  end
end
