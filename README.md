# Dexter

<img src="dexter-logo.png" width="200" height="200" alt="Dexter logo" />

A fast, full-featured Elixir LSP that works on large codebases and won't eat all of your resources.

## Features

- **Fast indexing** — cold index completes in ~10s on a 50k-file Elixir monorepo, ~93ms on Oban, ~90ms on the Elixir standard library, using an M1 MacBook Pro. After your first index, incremental indexing makes sure that you never have to reindex the whole codebase again.
- **Go-to-definition** — jump to any module, function, type, or variable definition. Resolves aliases, imports, `defdelegate` chains, `use` injections, and the Elixir stdlib. Handles all definition forms: `def`, `defp`, `defmacro`, `defprotocol`, `defimpl`, `defstruct`, and more
- **Go-to-references** — find all usages of a function or module across the codebase, including through `import`, `use` chains, and `defdelegate`
- **Hover documentation** — `@doc`, `@moduledoc`, `@typedoc`, and `@spec` annotations rendered as Markdown when you hover over a symbol
- **Autocompletion** — modules, functions, types, and variables with full snippet support. Resolves through aliases, imports, `use` injections, and the Elixir stdlib. Works for qualified calls (`MyApp.Repo.|`), bare function calls, and module prefixes
- **Rename** — rename modules, functions, and variables with automatic file renaming when the convention is followed
- **No compilation required** — the index is built by parsing source files directly, not by compiling your project. Dexter works immediately on any codebase, even ones that don't compile
- **Monorepo and umbrella support** — a single index at the repository root covers all apps and shared libraries. Go-to-definition, find references, and rename work cross-project out of the box. Expert, Lexical, and ElixirLS all operate per-project with no cross-app awareness
- **Format on save** — formats `.ex`, `.exs`, and `.heex` files on save via a persistent Elixir process. Near-instant after the first save. Formatter plugins (Styler, Phoenix.LiveView.HTMLFormatter) are loaded from your project's `_build` — no install needed. Syntax errors are surfaced as diagnostics.
- **Elixir stdlib indexing** — jump to `Enum`, `String`, `Mix`, and other bundled modules by indexing your local Elixir installation sources
- **Signature help** — parameter hints as you type function calls
- **Workspace symbols** — search for any module or function across the entire codebase
- **Call hierarchy** — navigate incoming and outgoing calls
- **Code actions** — add missing aliases with a single action
- **Document symbols** — outline view of all functions and modules in the current file
- **Document highlight** — highlight all occurrences of the symbol under the cursor
- **Variable support** — go-to-definition, rename, and completion for local variables via tree-sitter, with correct scoping across `case`, `with`, `for`, and other block constructs
- **Cursor-position-aware resolution** — hovering on `MyApp.Repo` in `MyApp.Repo.all` shows docs for the module, hovering on `all` shows docs for the function
- **Delegate following** — `defdelegate fetch(id), to: MyApp.Repo` jumps to `MyApp.Repo.fetch`, respecting `as:` renames
- **Alias resolution** — `alias MyApp.Handlers.Foo`, `alias MyApp.Handlers.Foo, as: Cool`, `alias MyApp.Handlers.{Foo, Bar}`
- **Import resolution** — bare function calls resolved through `import` declarations
- **Type definitions** — `@type` and `@opaque` are indexed for go-to-definition and hover
- **Folding ranges** — collapse functions and modules in your editor
- **Monorepo-aware formatting** — walks up from the file to find the nearest `.formatter.exs`, so subprojects with their own formatter configs (including nested `subdirectories:` configs) just work
- **Local buffer search** — private function calls resolve without leaving the current file
- **Heredoc awareness** — code examples in `@moduledoc`/`@doc` are skipped
- **Module nesting** — correctly tracks `end` keywords to attribute functions to the right module
- **Git branch switch detection** — automatically reindexes when you switch branches

## Quick start

```sh
# 1. Install dependencies (if you don't already have them)
brew install sqlite
mise use -g go@1.26.1

# 2. Install dexter (requires a tagged release to exist)
mise plugin add dexter git@gitlab.com:remote-com/employ-starbase/dexter.git && mise use -g dexter@latest

# 3. Add .dexter.db to your global .gitignore
echo ".dexter.db*" >> .gitignore

# 4. Configure your editor (see below)
# The LSP server auto-builds the index on first startup — no need to run dexter init manually.
# You can still run it explicitly if you prefer: dexter init ~/code/my-elixir-project
```

## Editor setup

### VS Code / Cursor extension

```sh
git clone git@gitlab.com:remote-com/employ-starbase/dexter-vscode.git
cd dexter-vscode
make install   # or `make install-vscode` for VS Code
```

You'll need to restart Cursor after installing the new extension.

### Neovim (0.11+)

Add to your LSP configuration (e.g., `after/plugin/lsp.lua`):

```lua
vim.lsp.config('dexter', {
  cmd = { 'dexter', 'lsp' },
  root_markers = { '.dexter.db', '.git', 'mix.exs' },
  filetypes = { 'elixir', 'eelixir', 'heex' },
  init_options = {
    followDelegates = true,  -- jump through defdelegate to the target function
    -- stdlibPath = "",      -- override Elixir stdlib path (auto-detected)
    -- debug = false,        -- verbose logging to stderr (view with :LspLog)
  },
})

vim.lsp.enable 'dexter'
```

That's it. Go-to-definition (`gd`, `<C-]>`, or whatever you have mapped to `vim.lsp.buf.definition()`) will now use dexter alongside any other attached LSP servers. Formatting happens automatically on save — no `BufWritePre` autocommand needed.

If you want a dedicated binding just for dexter:

```lua
vim.keymap.set("n", "<leader>va", function()
  vim.lsp.buf.definition({ filter = function(client) return client.name == "dexter" end })
end)
```

### Neovim (with nvim-lspconfig - NeoVim < 0.11)

```lua
local lspconfig = require("lspconfig")
local configs = require("lspconfig.configs")

configs.dexter = {
  default_config = {
    cmd = { "dexter", "lsp" },
    filetypes = { "elixir", "eelixir", "heex" },
    root_dir = lspconfig.util.root_pattern(".dexter.db", "mix.exs", ".git"),
  },
}

lspconfig.dexter.setup({})
```

### Zed

Install the [dexter-zed](https://gitlab.com/remote-com/employ-starbase/dexter-zed) extension:

1. Clone the extension: `git clone git@gitlab.com:remote-com/employ-starbase/dexter-zed.git`
2. In Zed, open the command palette (`Cmd+Shift+P`) and run **"zed: install dev extension"**
3. Select the `dexter-zed/` directory

Configure the binary path in Zed's `settings.json`:

```json
{
  "lsp": {
    "dexter": {
      "binary": {
        "path": "/Users/you/.local/share/mise/shims/dexter",
        "arguments": ["lsp"],
      }
    }
  }
}
```

### Cursor/VSCode

Install the [dexter-vscode](https://gitlab.com/remote-com/employ-starbase/dexter-vscode) extension, then optionally set the binary path if dexter is not on your PATH:

```sh
git clone git@gitlab.com:remote-com/employ-starbase/dexter-vscode.git
cd dexter-vscode
make install   # for Cursor, or make install-vscode for VSCode
```

```json
{
  "dexter.binary": "/Users/you/.local/share/mise/shims/dexter"
}
```

To enable format-on-save, add this to your VS Code settings:

```json
{
  "[elixir]": { "editor.formatOnSave": true },
  "[phoenix-heex]": { "editor.formatOnSave": true }
}
```

## CLI usage

The CLI commands are available for scripting and manual use.

### Index a project

```sh
# First time — indexes all .ex/.exs files (including deps/ and the Elixir standard library)
dexter init ~/code/my-elixir-project

# Re-init from scratch (deletes existing index)
dexter init --force ~/code/my-elixir-project

# Print timing breakdown for each indexing phase (walk, parse, store)
dexter init --profile ~/code/my-elixir-project
```

Dexter auto-detects your Elixir installation. If it can't find it (e.g. a non-standard install, it's not in your `PATH`, etc.), set:

```sh
export DEXTER_ELIXIR_LIB_ROOT="/path/to/elixir/lib"
```

### Look up definitions

```sh
# Find where a module is defined
dexter lookup MyApp.Repo
# => /path/to/lib/my_app/repo.ex:1

# Find where a function is defined (follows defdelegates by default)
dexter lookup MyApp.Repo get
# => /path/to/lib/my_app/repo.ex:15

# Don't follow defdelegates
dexter lookup --no-follow-delegates MyApp.Accounts fetch
# => /path/to/lib/my_app/accounts.ex:5

# Strict mode — exit 1 if exact function not found (no fallback to module)
dexter lookup --strict MyApp.Repo nonexistent
# => (exit code 1)
```

### Find references

```sh
# Find all usages of a module
dexter references MyApp.Repo
# => /path/to/lib/my_app/accounts.ex:12
# => /path/to/lib/my_app/posts.ex:8

# Find all usages of a specific function
dexter references MyApp.Repo get
# => /path/to/lib/my_app/accounts.ex:45
```

Exits 1 with a message to stderr if no references are found.

### Keep the index up to date

```sh
# Re-index a single file (~10ms)
dexter reindex /path/to/lib/my_app/repo.ex

# Re-index the whole project (only re-parses changed files)
dexter reindex ~/code/my-elixir-project
```

When running as an LSP server, dexter automatically:
- Reindexes files on save (`textDocument/didSave`)
- Runs an incremental reindex on startup
- Watches `.git/HEAD` for branch switches and reindexes when detected

## Hover documentation

Dexter serves hover docs (`textDocument/hover`) for functions, modules, and types. When you hover over a symbol, it looks up the definition in the index and reads the `@doc`, `@moduledoc`, `@typedoc`, or `@spec` annotations from the source file.

The hover response shows the function signature (with `@spec` if present), followed by the doc string:

```
defp do_something(arg1, arg2)
@spec do_something(binary(), map()) :: {:ok, map()} | {:error, term()}

Does something with arg1 and arg2.
```

### Cursor-position-aware resolution

Dexter resolves hover (and go-to-definition) based on which segment of a dotted expression your cursor is on:

| Cursor position | Expression | Resolves to |
|-----------------|------------|-------------|
| On `Repo` in `MyApp.Repo.all` | `MyApp.Repo` | The `MyApp.Repo` module |
| On `all` in `MyApp.Repo.all` | `MyApp.Repo.all` | The `all` function |
| On `MyApp` in `MyApp.Repo.all` | `MyApp` | The `MyApp` module |

## Rename

Dexter supports `textDocument/rename` (F2 in most editors) for three kinds of symbols:

### Modules

Place your cursor on any segment of a module name and invoke rename. Dexter highlights just the last segment for editing — the parent namespace is preserved automatically. For example, renaming `Repo` in `MyApp.Repo` to `Repository` renames the module to `MyApp.Repository`.

**What gets updated:**
- The `defmodule` declaration
- All aliases, imports, and uses referencing the module
- All call sites
- All submodules (renaming `MyApp.Foo` also renames `MyApp.Foo.Bar`, `MyApp.Foo.Baz`, etc.)

**File renaming:** If the source file follows the Elixir naming convention (module `MyApp.SomeRepo` → file `some_repo.ex`), dexter renames the file alongside the module. For submodules, the containing directory segment is also renamed to match (e.g., renaming `MyApp.Companies` to `MyApp.Clients` moves `lib/companies/services/do_something.ex` → `lib/clients/services/do_something.ex`). After the rename, dexter opens the new file automatically if your editor supports `window/showDocument`.

**When path renaming won't happen:** If the file name doesn't match the snake_case form of the module's last segment — for example, a file named `my_custom_name.ex` that defines `MyModule.SomeRepo` — the file stays in place and only the contents are updated.

Files not open in the editor are written directly to disk; open buffers receive edits via the LSP workspace edit response.

### Functions

Place your cursor on a function name (qualified or bare) and invoke rename. Dexter updates:
- All `def`/`defp`/`defmacro`/`defguard`/etc. clauses
- `@spec` and `@callback` annotations
- Direct calls and pipe calls (`|> function_name`)
- `import Module, only: [function_name: ...]` lines
- Transitive call sites via `__using__` chains

Renaming is blocked for functions defined in stdlib or deps.

### Variables

Place your cursor on a local variable and invoke rename. Dexter uses tree-sitter to find all occurrences within the
enclosing function scope and renames them in a single edit. This is file-local only.

Go-to-definition also works for variables — it jumps to the first occurrence (pattern match or assignment) in scope.

## LSP options

Dexter reads `initializationOptions` from your editor configuration:

- **`followDelegates`** (boolean, default: `true`): follow `defdelegate` targets on lookup.
- **`stdlibPath`** (string): override the Elixir stdlib directory to index. Defaults to auto-detection; use this if your install is non-standard.
- **`debug`** (boolean, default: `false`): enable verbose logging to stderr. Logs timing and resolution details for every definition, hover, references, and rename request. Can also be enabled via the `DEXTER_DEBUG=true` environment variable.

## Lightning-fast Formatting

Dexter formats files on save via `textDocument/willSaveWaitUntil` using a persistent Elixir process per `.formatter.exs`. This persistent formatter server starts once when you open the first file in a project under a given `.formatter.exs`, so formatting is near-instant.

Plugins ([`Styler`](https://github.com/remoteoss/elixir-styler), `Phoenix.LiveView.HTMLFormatter`, etc.) are loaded from
your project's `_build/dev/lib`. So as long as your formatter plugins are installed and compiled, everything is ready to
go.

If the persistent process can't start, dexter falls back to running `mix format` directly.

**Syntax errors** found by the formatter are surfaced as LSP diagnostics pointing to the exact line and column, with a warning at the hint location (e.g. "the `do` on line 52 does not have a matching `end`"). Diagnostics clear on the next successful format (which again, is nearly instantaneous!).

**Nested `.formatter.exs`:** Dexter walks up from the file to the mix root and uses the nearest `.formatter.exs`. A file in `config/` uses `config/.formatter.exs` if it exists (for projects using `subdirectories:`), falling back to the root config.

**Elixir detection:** The `mix` and `elixir` binaries are derived from the same Elixir install used for stdlib detection, so the correct version is always used regardless of which tool manager you use (mise, asdf, etc.).

## Index location (.dexter.db)

Dexter creates `.dexter.db` at the root of your project. Where you place it determines what gets indexed.

**Monorepo root (recommended if using an Elixir monorepo or umbrella structure)** — Put the index at the root of your repository, next to `.git`. This indexes everything: all apps, all shared libraries, and all deps. Go-to-definition works across the entire codebase.

```sh
cd ~/code/my-monorepo   # where .git lives
dexter init .
```

**Single app** — Put the index inside a specific Mix project. Go-to-definition works within that app and its deps, but not across other apps in the monorepo.

```sh
cd ~/code/my-monorepo/apps/my_app
dexter init .
```

When the LSP server starts, it walks up from the project root looking for `.dexter.db`, preferring `.git` as the anchor point. This means if you initialised from the monorepo root, the server will find the right database even when Neovim's `rootUri` points to a sub-app (e.g. because `mix.exs` is there).

If no `.dexter.db` exists anywhere, the LSP server builds the index automatically on first startup.

## How it works

1. **Parsing** — `.ex`/`.exs` files are scanned line-by-line with regex for definition declarations. The parser tracks module nesting, heredoc boundaries, and aliases for `defdelegate` resolution.

2. **Storage** — Definitions are stored in SQLite (`.dexter.db`) with indexes on module name and module+function for fast lookups.

3. **LSP server** — `dexter lsp` speaks JSON-RPC over stdio. On `textDocument/definition`, it parses the cursor context, resolves aliases and imports from the open buffer, and queries the index.

4. **Incremental updates** — File mtimes are tracked. Reindex only re-parses files that changed.

5. **Persistent formatter** — A long-running Elixir process per `.formatter.exs` handles formatting via a binary protocol. Plugins are loaded from `_build` at startup. The process restarts automatically when `.formatter.exs` changes.

## Performance

Measured on a 55k-file Elixir monorepo (337k definitions, 2.7M references):

| Operation | Time |
|-----------|------|
| Full init | ~11s |
| Lookup (LSP or CLI) | ~10ms |
| Single file reindex (on save) | ~10ms |
| Full reindex (no changes) | ~2s |
| Format on save | <1ms |

## Why?

Other Elixir LSPs compile your project to build their understanding of the code. This works on small codebases but falls apart at scale — they exhaust memory, index forever, and still produce stale results. A persistent Elixir process for formatting instead of spawning subprocesses means format-on-save is nearly instantaneous. A SQLite-backed index built from source parsing means lookups are ~10ms regardless of codebase size.

## Development

Requires Go 1.21+, SQLite, and Elixir.

```sh
git clone git@gitlab.com:remote-com/employ-starbase/dexter.git
cd dexter
mise install
make build
make test
```

## Releasing

```sh
# 1. Create a release branch with the version bump
make release VERSION=0.2.0

# 2. Push the branch and merge it into main

# 3. Tag and push the tag
make tag VERSION=0.2.0
```

This updates the version in `internal/version/version.go` on a release branch. After merging to main, `make tag` creates and pushes the git tag. Users can then upgrade via mise:

```sh
mise plugin update dexter && mise install dexter@latest
```

The plugin update step is required to pick up newly tagged releases — without it, `mise install dexter@latest` will resolve against a stale list.

If the release changes how Elixir files are parsed or what gets stored in the index (e.g. a new definition kind, a change to delegate resolution), also bump `IndexVersion` in `internal/version/version.go`. Dexter will automatically rebuild the index when users upgrade to a binary with a higher `IndexVersion` — no manual `dexter init --force` required.
