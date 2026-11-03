defmodule D.User do
  @moduledoc false
  alias D.{ImplA, ImplB, ImplBoth}

  def run do
    {ImplA.go(1), ImplA.go(1, []), 1 |> ImplA.go(), &ImplA.go/1,
     ImplB.go(1), ImplB.go(1, []), &ImplB.go/1, 1 |> ImplB.go(),
     ImplBoth.go(1), ImplBoth.go(1, []), &ImplBoth.go/1, &ImplBoth.go/2}
  end
end
