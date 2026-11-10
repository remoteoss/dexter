package version

const Version = "0.7.2"

// IndexVersion is incremented whenever the index schema or parser changes in a
// way that requires a full rebuild. Bump this alongside Version when releasing
// a change that makes existing indexes stale — and bump daemon.ContractVersion
// in the same release, because a running daemon from the older build would
// otherwise keep serving the new frontend from that stale index.
const IndexVersion = 20 // 20: refs store the arity of each call. 19: a nested defmodule is named as Elixir names it (`defmodule Deep.Inner` in Outer is Outer.Deep.Inner and aliases Deep; `__MODULE__.Sub` and `Elixir.Name` are not nested). 18: a nested module is an alias in the module around it, a defimpl protocol name is resolved and not nested, `name[key]` and `x.name(...)` are not imported calls, and an uppercase sigil ends after an escaped closing delimiter. 17: defs in quotes are not the macro module's, defstruct gives __struct__/0 and /1, @derive is stored, a use in a macro quote is quoted_use, `@behaviour Alias` is stored with the alias resolved, `defimpl …, for: Mod` is a reference to Mod, and imported calls obey only:/except:, include def lines and scripts, and the only: keys
