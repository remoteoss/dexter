import Config

config :ash, default_string_length_count: :codepoints

config :dexter_ash_beam_fixture,
  ash_domains: [DexterAshBeamFixture.Accounts]
