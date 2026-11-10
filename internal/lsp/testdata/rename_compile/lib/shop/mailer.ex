defmodule Shop.Mailer do
  @moduledoc false
  use Shop.WorkerBase

  @impl true
  def perform(arg), do: Shop.Worker.run(Shop.Job, arg)

  @impl true
  def go(_x, _opts), do: :ok
end
