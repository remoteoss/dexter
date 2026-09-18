defmodule DexterAshBeamFixture.Accounts do
  use Ash.Domain

  resources do
    resource DexterAshBeamFixture.Accounts.User
  end
end
