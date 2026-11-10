defmodule Shop.Shape do
  @moduledoc false
  defprotocol Area do
    def area(x)
  end

  defmodule Square do
    @moduledoc false
    defstruct side: 1

    defimpl Area, for: __MODULE__ do
      def area(s), do: s.side * s.side
    end
  end

  def total(x), do: Area.area(x)
end
