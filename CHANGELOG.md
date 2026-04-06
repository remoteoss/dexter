# Changelog

## [0.5.1] - 2026-04-06

### Fixed

- **Formatter `import_deps` resolution** — the persistent formatter now resolves `import_deps` from `.formatter.exs`, reading each dependency's exported `locals_without_parens` so formatting matches `mix format` output
- **Formatter binary protocol corruption** — the Erlang IO server's default Unicode encoding was expanding bytes > 127 in the binary protocol framing to multi-byte UTF-8, silently corrupting formatted file content on the first format request if the server wasn't yet ready; fixed by forcing raw byte mode on stdin/stdout

## [0.5.0] - 2026-04-06

### Added

- **Go-to-references** — `textDocument/references` returns all usages of a module or function across the project, including calls through `__using__`/`import` chains and bare intra-module calls; also finds all bindings and uses of a local variable within its scope
- **Rename** — `textDocument/rename` + `textDocument/prepareRename` rename modules, functions, and variables project-wide; a module rename also renames all submodules (if needed), updates every alias/call/import/use site, and moves the file to its conventional path
- **Near-instant format on save** — `textDocument/formatting` formats the current file on save using the nearest `.formatter.exs`, with full support for formatter plugins like [Styler](https://github.com/remoteoss/elixir-styler); format errors are shown as LSP diagnostics so they appear inline. A persistent BEAM process is kept alive per `.formatter.exs`, eliminating VM startup cost so formatting is near-instant; falls back to `mix format` if the persistent process is unavailable
- **Full workspace symbol search** — `workspace/symbol` fuzzy-searches all indexed symbols by name across the whole project (Cmd-T in VS Code)
- **Go-to-declaration** — `textDocument/declaration` jumps to the `@callback` (or `@macrocallback`) definition for any `@impl`-annotated function; walks `@behaviour` declarations and `use`-chains (including dynamic `use unquote(mod)` patterns resolved via keyword opts) to find the right behaviour module, with a global index fallback for `@impl true`
- **Go-to-implementation** — `textDocument/implementation` jumps from a `@callback` definition to every module that implements it via `@behaviour` or `use`
- **Document symbols** — `textDocument/documentSymbol` returns a fully hierarchical outline of modules, submodules, functions, macros, types, structs, and protocols in the current file
- **Signature help** — `textDocument/signatureHelp` shows function parameter hints (triggered on `(` and `,`), including which argument is active and parameter names extracted from the definition
- **Type definition** — `textDocument/typeDefinition` jumps to the `@type` / `@opaque` declaration for the type under the cursor
- **Folding ranges** — `textDocument/foldingRange` reports foldable regions for `do...end` blocks and heredocs
- **Call hierarchy** — `textDocument/prepareCallHierarchy`, `callHierarchy/incomingCalls`, and `callHierarchy/outgoingCalls` show callers and callees of any function
- **Code actions** — `textDocument/codeAction` offers an "Add alias" quick fix for any unaliased module reference; searches the index when the full module name isn't used, and suggests up to five candidates
- **Document highlight** — `textDocument/documentHighlight` highlights all occurrences of the symbol under the cursor; uses scope-aware tree-sitter variable tracking for local variables, and falls back to token matching for module/function names

### Changed

- **Arity-aware completions** — function completions now emit one entry per callable arity (accounting for default parameters too), so `fun/2` and `fun/3` appear as distinct items
- **Cold indexing performance** — initial indexing is significantly more optimized; `dexter init --profile` added for detailed profiling during startup
- **Go-to-definition and references via use-chain with opts** — dynamic `import unquote(mod)` expressions inside `__using__/1` blocks are now resolved using the keyword opts passed at the `use` call site (e.g. `use MyLib, mod: Mox`)

### Fixed

- **Go-to-definition for nested modules** — a `defmodule Inner do` inside `defmodule Outer do` now creates an implicit alias `Inner → Outer.Inner` within `Outer`'s scope, so qualified calls like `Inner.fun()` resolve correctly
- **Incomplete submodule completions** — submodule segments were missing from completions on large codebases because the raw module row cap was hit before client-side deduplication into immediate segments; the query now uses `SELECT DISTINCT` on segments so the cap applies after dedup
- **Function lookup ordering** — when a name is shared by both a function and a type, the function definition is now returned first

## [0.4.0] - 2026-04-01

### Added

- **Hover documentation** — hovering over a module or function now displays its `@moduledoc`/`@doc` content, rendered as Markdown
- **Autocompletion** — module and function completions with documentation shown inline, including support for already-aliased modules and local functions
- **Elixir standard library support** — hover docs and completions now cover Elixir stdlib modules (e.g. `Enum`, `Map`, `String`) and `@typedoc` content
- **`use` macro support** — go-to-definition and hover now work on `use` statements, including complex multi-part module names like `Remote.Oban.Pro.Worker`
- **`__MODULE__` support** — go-to-definition and hover resolve `__MODULE__` references correctly
- **Zed editor support** — added configuration instructions for Zed

### Changed

- **File watching** — the LSP server now watches for external file changes (e.g. branch switches, `git pull`) and automatically refreshes the index, beyond the existing Git HEAD polling
- **Full reindex on version bumps** — when a new version of Dexter requires schema changes, the index is rebuilt entirely on startup instead of attempting an incremental update
- **CI pipeline** — added GitLab CI with linting and tests

## [0.3.0] - 2026-03-31

### Changed

- CLI commands (`init`, `reindex`, `lookup`, `lsp`) are now implemented with [cobra](https://github.com/spf13/cobra), improving help output and flag handling
- Version string moved to `internal/version/version.go` as a single source of truth; `make release VERSION=x.y.z` now updates that file instead of `server.go`

## [0.1.4] - 2026-03-30

### Fixed

- `defdelegate` lookahead now stops at new statement boundaries, preventing `as:` from a nearby defdelegate being incorrectly captured for an unrelated one
- `defmacro` and other definitions after deeply nested modules are now correctly attributed to the outer module — depth tracking via `do...end` block counting replaces the naive bare-`end` pop heuristic
- Relative nested `defmodule` names (e.g. `defmodule PayslipDownloadResponse do` inside `defmodule MyAppWeb.ApiDocs.Payslips do`) are now indexed as the fully-qualified `MyAppWeb.ApiDocs.Payslips.PayslipDownloadResponse`

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
