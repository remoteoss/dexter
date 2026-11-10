defmodule Ren.UserGroup do
  @moduledoc false
  alias Ren.Probe.{Mcp1, Other}
  alias Ren.Probe.{Deep.Mod, Mcp1.Child}

  def run, do: {Mcp1.hello(), Other.y(), Mod.z(), Child.x()}
end
