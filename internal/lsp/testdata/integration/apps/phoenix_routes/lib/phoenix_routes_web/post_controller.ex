defmodule PhoenixRoutesWeb.PostController do
  # Phoenix checks at compile time that a route target is a plug, so init/1 has to
  # exist. The actions are dispatched at runtime and the fixture never calls them.
  @behaviour Plug

  @impl true
  def init(opts), do: opts

  @impl true
  def call(conn, _opts), do: conn

  def index(conn, _params), do: conn

  def show(conn, _params), do: conn

  def create(conn, _params), do: conn
end
