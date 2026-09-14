defmodule AppWithStyler.MixProject do
  use Mix.Project

  def project do
    [
      app: :app_with_styler,
      version: "0.1.0",
      elixir: "~> 1.18",
      deps: deps()
    ]
  end

  defp deps do
    [
      # Pinned by ref: the umbrella resolves one lock for every scenario, so an
      # unpinned fork would drift to its default branch head on the next refresh.
      {:styler, "~> 1.4.2", only: [:dev, :test], runtime: false,
       git: "https://github.com/remoteoss/elixir-styler",
       ref: "463bcb55479826a03b22ef9d44a1754718082d67"}
    ]
  end
end
