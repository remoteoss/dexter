defmodule Shop.WorkerBase do
  @moduledoc false
  defmacro __using__(_opts) do
    quote do
      @behaviour Shop.Worker
    end
  end
end
