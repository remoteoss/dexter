defmodule Ren.UserCollision do
  @moduledoc false
  alias Ren.Probe.Mcp1
  alias Ren.Other.Mcp

  def run, do: {Mcp1.hello(), Mcp.w()}
end
