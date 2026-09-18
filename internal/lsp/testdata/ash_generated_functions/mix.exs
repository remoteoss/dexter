defmodule DexterAshBeamFixture.MixProject do
  use Mix.Project

  def project do
    [
      app: :dexter_ash_beam_fixture,
      version: "0.1.0",
      elixir: "~> 1.20",
      deps: [
        {:ash, "== 3.33.3"},
        {:spark, "== 2.7.2"}
      ]
    ]
  end
end
