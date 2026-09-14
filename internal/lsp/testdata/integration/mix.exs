defmodule DexterIntegrationFixture.MixProject do
  use Mix.Project

  # Every integration scenario lives in one umbrella: a single mix.lock, a single
  # _build, one `mix compile` in CI. Each scenario's dependencies stay in its own
  # child mix.exs so the scenario remains self-describing.
  #
  # A child's directory name must equal its Mix `:app` name, because Dexter maps
  # `<root>/apps/<app>/...` onto `_build/<profile>/lib/<app>/ebin`.
  def project do
    [
      apps_path: "apps",
      version: "0.1.0",
      elixir: "~> 1.18",
      deps: []
    ]
  end
end
