# Dexter

<img src="dexter-logo.png" width="200" height="200" alt="Dexter logo" />

A fast, full-featured Elixir LSP optimized for large Elixir codebases.

## Table of contents

- [Features](#features)
- [Quick start](#quick-start)
  - [Install via mise or asdf](#install-via-mise-or-asdf)
- [Editor setup](#editor-setup)
  - [VS Code / Cursor](#vs-code--cursor)
    - [Configuration](#configuration)
  - [Neovim (0.11+)](#neovim-011)
    - [Configuring format on save](#configuring-format-on-save)
  - [Neovim (with nvim-lspconfig — \< 0.11)](#neovim-with-nvim-lspconfig---011)
  - [Zed](#zed)
  - [Emacs](#emacs)
    - [Eglot](#eglot)
      - [Emacs version \>= 30](#emacs-version--30)
      - [Emacs version \<= 29](#emacs-version--29)
    - [lsp-mode](#lsp-mode)
  - [Helix](#helix)
- [Why build another LSP?](#why-build-another-lsp)
- [Performance](#performance)
- [CLI usage](#cli-usage)
  - [Index a project](#index-a-project)
  - [Look up definitions](#look-up-definitions)
  - [Find references](#find-references)
  - [Reindexing files manually](#reindexing-files-manually)
- [Hover documentation](#hover-documentation)
  - [Cursor-position-aware resolution](#cursor-position-aware-resolution)
- [Rename](#rename)
  - [Modules](#modules)
  - [Functions](#functions)
  - [Variables](#variables)
- [Lightning-fast formatting](#lightning-fast-formatting)
- [LSP options](#lsp-options)
- [Index database location (.dexter/)](#index-database-location-dexter)
- [Debugging](#debugging)
- [Development (building from source)](#development-building-from-source)
- [Releasing](#releasing)
- [Contributing](#contributing)
- [License](#license)

## Features

- **Fast indexing** — cold index completes in ~11s on a 57k-file Elixir monorepo, ~100ms on Oban, ~300ms on the Elixir standard library (measured on an M1 MacBook Pro). After your first index, incremental indexing makes sure that you never have to reindex the whole codebase again.
- **Go-to-definition** — jump to any module, function, type, or variable definition. Resolves aliases, imports, `defdelegate` chains, `use` injections, and the Elixir stdlib. Handles all definition forms: `def`, `defp`, `defmacro`, `defprotocol`, `defimpl`, `defstruct`, and more.
- **Go-to-references** — find all usages of a function or module across the codebase, including through `import`, `use` chains, and `defdelegate`.
- **Hover documentation** — `@doc`, `@moduledoc`, `@typedoc`, and `@spec` annotations rendered as Markdown when you hover over a symbol.
- **Autocompletion** — modules, functions, types, and variables with full snippet support. Resolves through aliases, imports, `use` injections, and the Elixir stdlib. Works for qualified calls (`MyApp.Accounts.|`), bare function calls, and module prefixes.
- **Rename** — rename modules, functions, and variables with automatic file renaming when the convention is followed.
- **No compilation required** — the index is built by parsing source files directly, not by compiling your project. Dexter works immediately on any codebase, even ones that don't compile.
- **Monorepo and umbrella support** — a single index at the repository root covers all apps and shared libraries. Go-to-definition, find references, and rename work cross-project out of the box.
- **Format on save** — formats `.ex`, `.exs`, and `.heex` files on save via a persistent Elixir process. Near-instant after the first save. Formatter plugins (Styler, Phoenix.LiveView.HTMLFormatter) are loaded from your project's `_build` — no install needed. Syntax errors are surfaced as diagnostics.
- **Elixir stdlib indexing** — jump to `Enum`, `String`, `Mix`, and other bundled modules by indexing your local Elixir installation sources.
- **Signature help** — parameter hints as you type function calls.
- **Workspace symbols** — search for any module or function across the entire codebase.
- **Call hierarchy** — navigate incoming and outgoing calls.
- **Code actions** — add missing aliases with a single action.
- **Document symbols** — outline view of all functions and modules in the current file.
- **Document highlight** — highlight all occurrences of the symbol under the cursor.
- **Variable support** — go-to-definition, rename, and completion for local variables via tree-sitter, with correct scoping across `case`, `with`, `for`, and other block constructs.
- **Git branch switch detection** — automatically reindexes when you switch branches.

<details>
<summary>More features</summary>

- **Delegate following** — `defdelegate fetch_user(id), to: MyApp.Accounts.Finders.FetchUser, as: :find` jumps to `MyApp.Accounts.Finders.FetchUser.find`, respecting `as:` renames.
- **Alias resolution** — `alias MyApp.Handlers.Foo`, `alias MyApp.Handlers.Foo, as: Cool`, `alias MyApp.Handlers.{Foo, Bar}`.
- **Import resolution** — bare function calls resolved through `import` declarations.
- **Type definitions** — `@type` and `@opaque` are indexed for go-to-definition and hover.
- **Folding ranges** — collapse functions and modules in your editor.
- **Monorepo-aware formatting** — walks up from the file to find the nearest `.formatter.exs`, so subprojects with their own formatter configs (including nested `subdirectories:` configs) just work.
- **Heredoc awareness** — code examples in `@moduledoc`/`@doc` are skipped.
- **Module nesting** — correctly tracks `end` keywords to attribute functions to the right module.

</details>

## Quick start

### Install via mise or asdf

```sh
# 1. Install dependencies (if you don't already have them)
brew install sqlite  # or your preferred package manager

# 2. Install dexter (builds from source if no prebuilt binary is available for your platform)
mise plugin add dexter https://github.com/remoteoss/dexter.git && mise use -g dexter@latest
# or, with asdf:
# asdf plugin add dexter https://github.com/remoteoss/dexter.git && asdf install dexter latest && asdf global dexter latest

# 3. Configure your editor (see below)
# The LSP server auto-builds the index on first startup — no need to run dexter init manually.
# You can still run it explicitly if you prefer: dexter init ~/code/my-elixir-project
```

You can also [build from source](#development-building-from-source) directly.

## Editor setup

Dexter works with any editor that supports the Language Server Protocol. Below are setup instructions for the most common ones — if your editor isn't listed, point it at `dexter lsp` over stdio.

### VS Code / Cursor

Install the [Dexter VS Code Extension](https://marketplace.visualstudio.com/items?itemName=remoteoss.dexter-lsp).

Or, if you prefer to install from source: [dexter-vscode](https://github.com/remoteoss/dexter-vscode?tab=readme-ov-file#development).

#### Configuration

If you installed via Mise or ASDF, you're all done!

But if Dexter is not on your `PATH`, set the binary path in your editor settings:

```json
{
  "dexter.binary": "/Users/you/.local/share/mise/shims/dexter"
}
```

To enable format-on-save, update your VS Code/Cursor settings:

```json
// global in your editor
{
  "editor.formatOnSave": true,
}

// or, for Elixir specifically
{
  "[elixir]": {
      "editor.formatOnSave": true,
      // you may need to set Dexter as your default Elixir formatter, depending on your setup
      "editor.defaultFormatter": "remoteoss.dexter-lsp" // "remote-com-oss.dexter-lsp" for Cursor
  },
  "[phoenix-heex]": { "editor.formatOnSave": true }
}
```

### Neovim (0.11+)

Add to your LSP configuration (e.g., `after/plugin/lsp.lua`):

```lua
vim.lsp.config('dexter', {
  cmd = { 'dexter', 'lsp' },
  root_markers = { '.dexter/dexter.db', '.dexter.db', '.git', 'mix.exs' },
  filetypes = { 'elixir', 'eelixir', 'heex' },
  init_options = {
    followDelegates = true,  -- jump through defdelegate to the target function
    -- stdlibPath = "",      -- override Elixir stdlib path (auto-detected)
    -- debug = false,        -- verbose logging to stderr (view with :LspLog)
  },
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

#### Configuring format on save

If you want formatting on save, you'll need to configure a `PreWrite` autocmd. You can do something like this:

```lua
vim.api.nvim_create_autocmd('LspAttach', {
  group = vim.api.nvim_create_augroup('my.lsp', {}),

  callback = function(args)
    local opts = { remap = false }
    local client = assert(vim.lsp.get_client_by_id(args.data.client_id))
    local builtin = require("telescope.builtin")

    -- along with your other config

    if client:supports_method('textDocument/formatting') then
      -- the most important part
      vim.api.nvim_create_autocmd('BufWritePre', {
        buffer = args.buf,
        callback = function()
          vim.lsp.buf.format({ bufnr = args.buf, id = client.id, timeout_ms = 5000 })
        end,
      })
    end
  end
})
```

### Neovim (with nvim-lspconfig — < 0.11)

```lua
local lspconfig = require("lspconfig")
local configs = require("lspconfig.configs")

configs.dexter = {
  default_config = {
    cmd = { "dexter", "lsp" }, -- update this if you don't have Dexter in your PATH
    filetypes = { "elixir", "eelixir", "heex" },
    root_dir = lspconfig.util.root_pattern(".dexter/dexter.db", ".dexter.db", "mix.exs", ".git"),
  },
}

lspconfig.dexter.setup({})
```


### Zed

Zed support is available via a **dev extension** while the update for built-in [extension registry](https://github.com/zed-industries/extensions/pull/5624) and [documentation](https://github.com/zed-industries/zed/pull/53793) is being merged.

To install it:

1. Clone the official extension repo:
   ```bash
   git clone https://github.com/zed-extensions/elixir.git
   cd elixir
   ```
2. In Zed, open the command palette (`Cmd+Shift+P`) and run **zed: install dev extension**
3. Select the cloned `elixir` directory

Once installed, enable it in your Zed `settings.json`:

```json
{
  "languages": {
    "Elixir": {
      "language_servers": ["dexter", "!elixir-ls", "!expert"]
    },
    "EEx": {
      "language_servers": ["dexter", "!elixir-ls", "!expert"]
    },
    "HEEx": {
      "language_servers": ["dexter", "!elixir-ls", "!expert"]
    }
  }
}
```

If you already have Dexter installed via [mise](https://mise.jdx.dev/), the extension will use your local binary from PATH instead of downloading.

To override the binary path manually, add this to your `settings.json`:

```json
{
  "lsp": {
    "dexter": {
      "binary": {
        "path": "/Users/you/.local/share/mise/shims/dexter", // or wherever `which dexter` points to
        "arguments": ["lsp"]
      }
    }
  }
}
```

### Emacs

The emacs instructions assume you're using **use-package**.

#### Eglot

##### Emacs version >= 30

```emacs-lisp
(use-package eglot
  :ensure t

  :config
  (setf (alist-get '(elixir-mode elixir-ts-mode heex-ts-mode)
                   eglot-server-programs
                   nil nil #'equal)
        '("/path/to/dexter" "lsp")) ;; wherever `which dexter` points to

  ;; other config
  )
```

##### Emacs version <= 29

```emacs-lisp
(use-package eglot
  :ensure t

  :config
  (setf (alist-get 'elixir-mode eglot-server-programs)
        '("/path/to/dexter" "lsp")) ;; wherever `which dexter` points to

  ;; other config
  )
```

#### lsp-mode

```emacs-lisp
(use-package lsp-mode
  :ensure t
  :hook ((elixir-mode elixir-ts-mode heex-ts-mode) . lsp-deferred)
  :config
  (add-to-list 'lsp-disabled-clients 'elixir-ls)

  (lsp-register-client
   (make-lsp-client
    :new-connection (lsp-stdio-connection
                     '("/path/to/dexter" "lsp")) ;; wherever `which dexter` points to
    :activation-fn (lsp-activate-on "elixir")
    :server-id 'dexter-elixir)))
```

### Helix

Add to your LSP configuration in `~/.config/helix/languages.toml`:

```toml
[language-server.dexter]
command = "dexter"
args = ["lsp"]

[[language]]
name = "elixir"
language-servers = ["dexter"]
```

## Why build another LSP?

Remote has one of the largest Elixir codebases in existence (at least that we're aware of), now around 57k files. As our codebase has grown, we've had more and more struggles with language servers. We had found that they simply couldn't keep up with such a large codebase. On large codebases like ours, existing LSPs take hours to index, and even after indexing, operations like go-to-definition and go-to-references are still slow. On top of that, changing branches means a whole new round of indexing. The result has been frustration. Many of us on the engineering team had all but given up on the idea of ever having a working LSP.

Dexter is designed with speed and efficiency as core guiding principles. It takes a different approach from other Elixir LSPs, parsing source files directly as text and storing everything in SQLite so lookups are fast. The speed difference is noticeable on codebases of all sizes. Although Dexter isn't fully aware of the compiled state of the code like compilation-based LSPs, some clever parsing and deferring complex macro following to runtime allow it to get very close. In fact, you probably wouldn't even notice this limitation if you weren't reading this.

## Performance

Measured on a 57k-file Elixir monorepo (330k definitions, 2.7M references) on a 32GB M1 MacBook Pro:

| Operation                     | Time  |
| ----------------------------- | ----- |
| Cold first-time index         | ~11s  |
| Lookup (LSP or CLI)           | ~10ms |
| Single file reindex (on save) | ~10ms |
| Full reindex (no changes)     | ~2s   |
| Format on save                | <1ms  |

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
dexter lookup MyApp.Accounts
# => /path/to/lib/my_app/accounts.ex:1

# Find where a function is defined (follows defdelegates by default)
dexter lookup MyApp.Accounts fetch_user
# => /path/to/lib/my_app/accounts/finders/fetch_user.ex:8

# Don't follow defdelegates
dexter lookup --no-follow-delegates MyApp.Accounts fetch_user
# => /path/to/lib/my_app/accounts.ex:5

# Strict mode — exit 1 if exact function not found (no fallback to module)
dexter lookup --strict MyApp.Accounts nonexistent
# => (exit code 1)
```

### Find references

```sh
# Find all usages of a module
dexter references MyApp.Accounts
# => /path/to/lib/my_app_web/user_controller.ex:12
# => /path/to/lib/my_app/auth.ex:8

# Find all usages of a specific function
dexter references MyApp.Accounts fetch_user
# => /path/to/lib/my_app_web/user_controller.ex:45
```

Exits 1 with a message to stderr if no references are found.

### Reindexing files manually

The LSP does this for you automatically, but if for some reason you need to, you can reindex files manually via the CLI.

```sh
# Re-index a single file (~10ms)
dexter reindex /path/to/lib/my_app/accounts.ex

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
def fetch_user(id, opts)
@spec fetch_user(binary(), keyword()) :: {:ok, User.t()} | {:error, term()}

Fetches a user by ID. Options are passed to the underlying query.
```

### Cursor-position-aware resolution

Dexter resolves hover (and go-to-definition) based on which segment of a dotted expression your cursor is on:

| Cursor position                              | Expression                  | Resolves to                  |
| -------------------------------------------- | --------------------------- | ---------------------------- |
| On `Accounts` in `MyApp.Accounts.list_users` | `MyApp.Accounts`            | The `MyApp.Accounts` module  |
| On `list_users` in `MyApp.Accounts.list_users` | `MyApp.Accounts.list_users` | The `list_users` function  |
| On `MyApp` in `MyApp.Accounts.list_users`    | `MyApp`                     | The `MyApp` module           |

## Rename

Dexter supports renaming modules, functions, and variables across the codebase via `textDocument/rename` (F2 in most editors).

### Modules

Place your cursor on any segment of a module name and invoke rename. Dexter highlights just the last segment for editing — the parent namespace is preserved automatically. For example, renaming `Accounts` in `MyApp.Accounts` to `Users` renames the module to `MyApp.Users`.

**What gets updated:**

- The `defmodule` declaration
- All aliases, imports, and uses referencing the module
- All call sites
- All submodules (renaming `MyApp.Foo` also renames `MyApp.Foo.Bar`, `MyApp.Foo.Baz`, etc.)

**File renaming after a module rename:** If the source file follows the Elixir naming convention (module `MyApp.Accounts` → file `accounts.ex`), dexter renames the file alongside the module. For submodules, the containing directory segment is also renamed to match (e.g., renaming `MyApp.Companies` to `MyApp.Clients` moves `lib/companies/services/do_something.ex` → `lib/clients/services/do_something.ex`). After the rename, dexter opens the new file automatically if your editor supports `window/showDocument`.

**When path renaming won't happen:** If the file name doesn't match the snake_case form of the module's last segment — for example, a file named `my_custom_name.ex` that defines `MyApp.Accounts` — the file stays in place and only the contents are updated.

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

## Lightning-fast formatting

Dexter formats files on save via `textDocument/willSaveWaitUntil` using a persistent Elixir process per `.formatter.exs`. This persistent formatter server starts once when you open the first file in a project under a given `.formatter.exs`, so formatting is near-instant.

Plugins ([`Styler`](https://github.com/remoteoss/elixir-styler), `Phoenix.LiveView.HTMLFormatter`, etc.) are loaded from
your project's `_build/dev/lib`. So as long as your formatter plugins are installed and compiled, everything is ready to
go.

If the persistent process can't start, dexter falls back to running `mix format` directly.

**Syntax errors** found by the formatter are surfaced as LSP diagnostics pointing to the exact line and column, with a warning at the hint location (e.g. "the `do` on line 52 does not have a matching `end`"). Diagnostics clear on the next successful format (which again, is nearly instantaneous!).

**Nested `.formatter.exs`:** Dexter walks up from the file to the mix root and uses the nearest `.formatter.exs`. A file in `config/` uses `config/.formatter.exs` if it exists (for projects using `subdirectories:`), falling back to the root config.

**Elixir detection:** The `mix` and `elixir` binaries are derived from the same Elixir install used for stdlib detection, so the correct version is always used regardless of which tool manager you use (mise, asdf, etc.).

## LSP options

Dexter reads `initializationOptions` from your editor configuration:

- **`followDelegates`** (boolean, default: `true`): follow `defdelegate` targets on lookup.
- **`stdlibPath`** (string): override the Elixir stdlib directory to index. Defaults to auto-detection; use this if your install is non-standard.
- **`debug`** (boolean, default: `false`): enable verbose logging to stderr. Logs timing and resolution details for every definition, hover, references, and rename request. Can also be enabled via the `DEXTER_DEBUG=true` environment variable.

## Index database location (.dexter/)

Dexter creates `.dexter/dexter.db` at the root of your project when you start the LSP for the first time. But if you prefer, you can run `dexter init` yourself in the root of your project. Where you place it determines what gets indexed.

The `.dexter/` folder includes its own `.gitignore` (containing `*`), so its contents are automatically ignored by git — no need to update your project's `.gitignore`.

When the LSP server starts, it walks up from the project root looking for `.dexter/dexter.db`, preferring `.git` as the anchor point. This means if you initialised from the monorepo root, the server will find the right database even when Neovim's `rootUri` points to a sub-app (e.g. because `mix.exs` is there).

If you're upgrading from a pre-`.dexter/` version of dexter, any existing `.dexter.db` file at your project root will be automatically deleted and rebuilt into the new `.dexter/` folder on the next `dexter init` or LSP startup. You can also remove any `.dexter.db*` or `.dexter/` entries from your `.gitignore`, as they are no longer needed.

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

## Debugging

If something isn't working as expected, start by forcing a full reindex to rule out a stale/corrupted index. It's
unlikely that this is actually the problem, but it's better to have a clean slate just to be sure.

```sh
dexter init --force ~/code/my-elixir-project
```

If the issue persists, enable debug mode to get verbose logs. You can do this in two ways:

1. Set the `debug` option in your editor's LSP `initializationOptions` (see [LSP options](#lsp-options))
2. Or set the `DEXTER_DEBUG=true` environment variable before launching your editor

Debug mode logs timing and resolution details for every definition, hover, references, and rename request to stderr. In Neovim you can usually view these at `~/.local/state/nvim/lsp.log`. In VS Code, you can see them in Output > Dexter.

When [filing an issue](https://github.com/remoteoss/dexter/issues/new), please include:

- Your Dexter version (`dexter --version`)
- Your Elixir version (`elixir --version`)
- The debug logs from the failing operation
- A minimal code snippet that reproduces the issue, if possible

## Development (building from source)

Requires Go 1.21+, SQLite, and Elixir.

```sh
git clone https://github.com/remoteoss/dexter.git
cd dexter
mise install    # install dependencies
make build      # to build from source
make test       # to test
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

The plugin update step is required to pick up newly tagged releases. Without it, `mise install dexter@latest` will resolve against a stale list.

If the release changes how Elixir files are parsed or what gets stored in the index (e.g. a new definition kind, a change to delegate resolution), also bump `IndexVersion` in `internal/version/version.go`. Dexter will automatically rebuild the index when users upgrade to a binary with a higher `IndexVersion` — no manual `dexter init --force` required.

## Contributing

Dexter is a new project and we're actively expanding its capabilities. If you come across code that causes issues, do let us know so we can support it. Bug reports, pull requests, and feature suggestions are all welcome on [GitHub](https://github.com/remoteoss/dexter). We try to address issues quickly and would love to hear what you'd like to see next.

## License

Dexter is released under the [MIT License](LICENSE).
