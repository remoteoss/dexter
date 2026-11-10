defmodule Ren.UserAncestorKept do
  @moduledoc false
  alias Ren.Probe

  def run, do: {Probe.Mcp1.hello(), Probe.Mcp1.Child.x(), Probe.Other.y()}
end
