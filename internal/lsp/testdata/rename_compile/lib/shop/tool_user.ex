defmodule Shop.ToolUser do
  @moduledoc false
  use Shop.Tools

  def a(x), do: tool(x)

  def b(x) do
    x
    |> tool()
  end
end
