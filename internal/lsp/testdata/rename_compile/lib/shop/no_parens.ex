defmodule Shop.NoParens do
  @moduledoc false
  import Shop.Accounts, only: [fetch: 2]
  @items [1]

  # Its own process/1, called without parentheses with each kind of first
  # argument.
  def process(x), do: x
  def list, do: process [1, 2]
  def tuple, do: process {1, 2}
  def attr, do: process @items
  def fun, do: process fn y -> y end
  def bits, do: process <<1, 2>>

  def statement do
    process [1, 2]
  end

  # Its own fetch/1 next to the imported fetch/2: a comma in a bitstring
  # does not separate arguments, and a pipe adds one.
  def own_bits(x), do: fetch(<<x, 1>>)
  def own_piped(x), do: x |> fetch
  def imported(x), do: fetch(<<x, 1>>, [])
  defp fetch(bin), do: bin
end

defmodule Shop.DefLineNoParens do
  @moduledoc false
  import Shop.Accounts, only: [fetch: 1]

  def one(id), do: fetch id
end
