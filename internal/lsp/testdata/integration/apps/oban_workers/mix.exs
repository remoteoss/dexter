defmodule ObanWorkers.MixProject do
  use Mix.Project

  def project do
    [
      app: :oban_workers,
      version: "0.1.0",
      elixir: "~> 1.18",
      deps: [
        {:oban, "~> 2.19"},
        {:jason, "~> 1.4"}
      ]
    ]
  end
end
