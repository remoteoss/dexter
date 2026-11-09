defmodule Shop.Settings do
  @moduledoc false
  def config, do: %{}
end

defmodule Shop.SettingsUser do
  @moduledoc false
  import Shop.Settings

  def get(key) do
    config = config()
    config[key]
  end
end
