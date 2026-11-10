defmodule Ren.UserAlias do
  @moduledoc false
  alias Ren.Probe.Mcp1
  alias Ren.Probe.Mcp1.Child

  def run, do: {Mcp1.hello(), Mcp1.Child.x(), Child.x()}
end
