defmodule DexterAshBeamFixture.Accounts.User do
  use Ash.Resource,
    domain: DexterAshBeamFixture.Accounts,
    data_layer: Ash.DataLayer.Ets

  ets do
    private?(true)
  end

  attributes do
    uuid_primary_key(:id)
    attribute(:email, :string, allow_nil?: false, public?: true)
    attribute(:name, :string, public?: true)
  end

  actions do
    defaults([:read, :destroy, create: [:email, :name], update: [:email, :name]])
  end

  code_interface do
    define(:create, args: [:email, {:optional, :name}])
    define(:get_by_id, action: :read, get_by: [:id])
    define(:list, action: :read)
    define(:update)
    define(:destroy)
  end
end
