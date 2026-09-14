defmodule PhoenixRoutes.MixProject do
  use Mix.Project

  def project do
    [
      app: :phoenix_routes,
      version: "0.1.0",
      elixir: "~> 1.18",
      deps: [
        {:phoenix, "~> 1.7"},
        # A route target has to be a plug, which the controller behaviour declares.
        {:plug, "~> 1.16"}
      ]
    ]
  end
end
