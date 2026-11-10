defmodule Ren.Outer do
  @moduledoc false
  defmodule Inner do
    @moduledoc false
    def i, do: :i
  end

  def run, do: {Inner.i(), %{inner: Inner}}
end
