defmodule Shop.Tools do
  @moduledoc false
  defmacro __using__(_opts) do
    quote do
      import unquote(__MODULE__)
    end
  end

  @type tool :: term
  @spec tool(tool) :: tool
  def tool(x), do: x
end
