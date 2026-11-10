defmodule Shop.MixProject do
  use Mix.Project

  def project do
    [app: :shop, version: "0.1.0", elixir: "~> 1.14", deps: [{:dep_behaviour, path: "deps/dep_behaviour"}]]
  end
end
