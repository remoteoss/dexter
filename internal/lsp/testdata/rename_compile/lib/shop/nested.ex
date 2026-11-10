defmodule Shop.Nested do
  @moduledoc false
  defmodule Alpha do
    @moduledoc false
    def foo(x), do: x
    def inside, do: Alpha.foo(0)
  end

  def go, do: Alpha.foo(1)

  defmodule Beta do
    @moduledoc false
    def go, do: Alpha.foo(2)
  end

  defmodule Deep.Inner do
    @moduledoc false
    def peek(x), do: x
    def again, do: Deep.Inner.peek(0)
  end

  def deep, do: Deep.Inner.peek(1)
end
