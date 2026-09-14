defmodule PhoenixRoutesWeb.Router do
  use Phoenix.Router

  scope "/", PhoenixRoutesWeb do
    get "/posts", PostController, :index
    get "/posts/:id", PostController, :show
    post "/posts", PostController, :create
  end
end
