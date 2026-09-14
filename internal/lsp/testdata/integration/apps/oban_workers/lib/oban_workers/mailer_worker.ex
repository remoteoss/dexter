defmodule ObanWorkers.MailerWorker do
  # Oban's __using__ injects new/1, new/2, insert/2 and insert_all/2 alongside
  # the perform/2 this module defines itself, so the compiled exports include
  # functions no source file declares.
  use Oban.Worker, queue: :mailers, max_attempts: 3

  @impl Oban.Worker
  def perform(%Oban.Job{args: %{"to" => to}}) do
    deliver(to)
  end

  defp deliver(_to), do: :ok
end
