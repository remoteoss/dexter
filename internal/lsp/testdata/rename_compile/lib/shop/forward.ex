defmodule Shop.ForwardImpl do
  @moduledoc false
  def run(x), do: x
end

defmodule Shop.Forward do
  @moduledoc false
  defdelegate run(x), to: Shop.ForwardImpl
end

defmodule Shop.ForwardUser do
  @moduledoc false
  def go, do: Shop.Forward.run(1)
end
