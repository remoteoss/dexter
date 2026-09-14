import Config

# The Ash scenario compiles against these, and its generated functions are what
# the DSL and generated-function tests assert on. Umbrella config is shared by
# every child, so keep it keyed by app name.
config :ash, default_string_length_count: :codepoints

config :dexter_ash_beam_fixture,
  ash_domains: [DexterAshBeamFixture.Accounts]
