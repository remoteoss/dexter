defmodule AppWithEctoMigration.MixProject do
  use Mix.Project

  def project do
    [
      app: :app_with_ecto_migration,
      version: "0.1.0",
      elixir: "~> 1.18",
      deps: deps()
    ]
  end

  defp deps do
    [
      {:ecto_sql, "~> 3.0"}
    ]
  end
end
