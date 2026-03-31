# Dexter

A fast Elixir go-to-definition engine that runs as an LSP server. Built for large codebases where traditional Elixir LSP servers are too slow.

Dexter indexes every module and function definition in your project into a local SQLite database, then serves instant go-to-definition responses over the Language Server Protocol. It understands aliases, imports, `defdelegate`, nested modules, and heredocs.

Dexter is designed to run **alongside** your existing Elixir LSP, not replace it. Use dexter for fast navigation and your full LSP for diagnostics, completions, and refactoring.

## Quick start

```sh
# 1. Install dependencies
brew install sqlite
mise use -g go@1.26.1

# 2. Install dexter
mise plugin add dexter git@gitlab.com:remote-com/employ-starbase/dexter.git
mise install dexter@latest
mise use -g dexter@latest

# 3. Index your project (one-time, ~8s for a large codebase)
# Run from your monorepo root — dexter will place .dexter.db next to your .git directory
cd ~/code/my-elixir-project
dexter init .

# 4. Add .dexter.db to your .gitignore
echo ".dexter.db" >> .gitignore

# 5. Configure your editor (see below)
# When using the LSP server, dexter auto-builds the index on first startup if it doesn't exist
```

### VS Code / Cursor extension

```sh
git clone git@gitlab.com:remote-com/employ-starbase/dexter-vscode.git
cd dexter-vscode
make install-vscode   # or make install-cursor
```

## Editor setup

### Neovim (0.11+)

Add to your LSP configuration (e.g., `after/plugin/lsp.lua`):

```lua
vim.lsp.config('dexter', {
  cmd = { 'dexter', 'lsp' },
  root_markers = { '.dexter.db', '.git', 'mix.exs' },
  filetypes = { 'elixir', 'eelixir' },
})

vim.lsp.enable 'dexter'
```

That's it. Go-to-definition (`gd`, `<C-]>`, or whatever you have mapped to `vim.lsp.buf.definition()`) will now use dexter alongside any other attached LSP servers.

If you want a dedicated binding just for dexter:

```lua
vim.keymap.set("n", "<leader>va", function()
  vim.lsp.buf.definition({ filter = function(client) return client.name == "dexter" end })
end)
```

### Neovim (with nvim-lspconfig)

```lua
local lspconfig = require("lspconfig")
local configs = require("lspconfig.configs")

configs.dexter = {
  default_config = {
    cmd = { "dexter", "lsp" },
    filetypes = { "elixir", "eelixir" },
    root_dir = lspconfig.util.root_pattern(".dexter.db", "mix.exs", ".git"),
  },
}

lspconfig.dexter.setup({})
```

### VS Code / Cursor

Install the [dexter-vscode](https://gitlab.com/remote-com/employ-starbase/dexter-vscode) extension, then optionally set the binary path if dexter is not on your PATH:

```json
{
  "dexter.binary": "/Users/you/.local/share/mise/shims/dexter"
}
```

## Features

- **Alias resolution** — `alias MyApp.Handlers.Foo`, `alias MyApp.Handlers.Foo, as: Cool`, `alias MyApp.Handlers.{Foo, Bar}`
- **Import resolution** — bare function calls resolved through `import` declarations
- **Delegate following** — `defdelegate fetch(id), to: MyApp.Repo` jumps to `MyApp.Repo.fetch`, respecting `as:` renames
- **Local buffer search** — private function calls resolve without leaving the current file
- **All def forms** — `def`, `defp`, `defmacro`, `defmacrop`, `defguard`, `defguardp`, `defdelegate`, `defprotocol`, `defimpl`, `defstruct`, `defexception`
- **Heredoc awareness** — code examples in `@moduledoc`/`@doc` are skipped
- **Module nesting** — correctly tracks `end` keywords to attribute functions to the right module
- **Git branch detection** — automatically reindexes when you switch branches
- **Parallel indexing** — uses all CPU cores for initial index

## CLI usage

The CLI commands are available for scripting and manual use.

### Index a project

```sh
# First time — indexes all .ex/.exs files (including deps/)
dexter init ~/code/my-elixir-project

# Re-init from scratch (deletes existing index)
dexter init --force ~/code/my-elixir-project
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

## How it works

1. **Parsing** — `.ex`/`.exs` files are scanned line-by-line with regex for definition declarations. The parser tracks module nesting, heredoc boundaries, and aliases for `defdelegate` resolution.

2. **Storage** — Definitions are stored in SQLite (`.dexter.db`) with indexes on module name and module+function for fast lookups.

3. **LSP server** — `dexter lsp` speaks JSON-RPC over stdio. On `textDocument/definition`, it parses the cursor context, resolves aliases and imports from the open buffer, and queries the index.

4. **Incremental updates** — File mtimes are tracked. Reindex only re-parses files that changed.

## Performance

Measured on a 57k-file Elixir monorepo (2.5M lines, 340k+ definitions):

| Operation | Time |
|-----------|------|
| Full init | ~8s |
| Lookup (LSP or CLI) | ~10ms |
| Single file reindex (on save) | ~10ms |
| Full reindex (no changes) | ~2s |

## Why?

Elixir LSP servers (ElixirLS, Lexical, etc.) can struggle with very large monorepos. Ctags works but doesn't understand Elixir module namespacing, so `Foo` often resolves to the wrong module. Dexter sits in between — it's Elixir-aware but doesn't try to be a full LSP. Just fast, correct go-to-definition.

## Development

Requires Go 1.21+ and Xcode command line tools (for SQLite via CGo).

```sh
git clone git@gitlab.com:remote-com/employ-starbase/dexter.git
cd dexter
mise install
make build
make test
```

## Releasing

```sh
make release VERSION=0.2.0
```

This updates the version in `internal/lsp/server.go`, commits, tags, and pushes. Users can then upgrade via mise:

```sh
mise install dexter@latest
```
