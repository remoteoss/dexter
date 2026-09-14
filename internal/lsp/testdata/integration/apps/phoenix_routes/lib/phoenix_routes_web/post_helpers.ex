defmodule PhoenixRoutesWeb.PostHelpers do
  # Phoenix generates the Helpers module at compile time, so it has no source
  # definition; the alias is how callers normally reach it.
  alias PhoenixRoutesWeb.Router.Helpers, as: Routes

  def show(conn, id), do: Routes.post_path(conn, :show, id)

  def create(conn, params), do: Routes.post_path(conn, :create, params)
end
