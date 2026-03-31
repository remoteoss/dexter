# Changelog

## [0.1.3] - 2026-03-30

### Fixed

- `dexter init` now defaults to current working directory when no path is given
- `dexter init --force` no longer misinterprets `--force` as a path argument
- `dexter init --force` now removes stale WAL files (`.dexter.db-shm`, `.dexter.db-wal`) that could corrupt the new database

## [0.1.2] - 2026-03-30

### Fixed

- Resolve `__MODULE__` aliases in the LSP buffer context — `alias __MODULE__.Schemas.UserRelationship` now correctly resolves when jumping to definition from an open buffer
- Partial alias resolution in the LSP Definition handler — `Services.AssociateWithTeamV2` now resolves through a `Services` alias to the full module name
- Relative nested `defmodule` — `defmodule PayslipDownloadResponse do` inside `defmodule MyAppWeb.ApiDocs.Payslips do` is now indexed as `MyAppWeb.ApiDocs.Payslips.PayslipDownloadResponse`

### Added

- Go-to-definition for module attributes — pressing the binding on `@endpoint_scopes` jumps to its definition in the current buffer. Reserved Elixir attributes (`@doc`, `@spec`, `@behaviour`, `@callback`, `@impl`, `@derive`, etc.) are excluded.

## [0.1.1] - 2026-03-30

### Fixed

- Resolve `__MODULE__` in alias declarations (e.g. `alias __MODULE__.Services`) so `defdelegate` targets that reference these aliases are correctly followed
- Resolve `__MODULE__` directly in `defdelegate to:` fields (e.g. `to: __MODULE__`)
- Resolve `alias __MODULE__, as: Name` so aliased self-references work in delegate chains
- Resolve partial aliases in `defdelegate to:` (e.g. `to: Services.Foo` where `Services` is an aliased module)

### Changed

- LSP server now auto-builds the index on first startup if no `.dexter.db` exists — no need to run `dexter init` manually
- Project root detection now prefers `.git` over `mix.exs` to correctly locate monorepo roots
- `dexter.followDelegates` LSP initialization option (default: `true`) allows clients to opt out of delegate following

## [0.1.0] - 2026-03-30

Initial release.

- SQLite-backed index of Elixir module and function definitions
- Parallel file parsing using all CPU cores
- Incremental reindex via file mtime tracking
- LSP server (`dexter lsp`) with `textDocument/definition` support
- Alias resolution: `alias A.B.C`, `alias A.B.C, as: D`, `alias A.B.{C, D}`
- Import resolution for bare function calls
- `defdelegate` following with `as:` rename support
- Support for `def`, `defp`, `defmacro`, `defmacrop`, `defguard`, `defguardp`, `defdelegate`, `defprotocol`, `defimpl`, `defstruct`, `defexception`
- Heredoc-aware parsing (skips code examples in `@moduledoc`/`@doc`)
- Module nesting via `end` tracking
- Git HEAD polling for automatic reindex on branch switches
- mise plugin for installation
- CLI commands: `init`, `init --force`, `reindex`, `lookup`, `lookup --strict`, `lookup --no-follow-delegates`
