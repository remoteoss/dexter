# Dexter architecture

Dexter is a fast Elixir LSP server. It indexes module and function definitions from `.ex`/`.exs` files into a SQLite database and serves near-instant lookups over the Language Server Protocol.

## Module structure

- `cmd/main.go` — CLI entrypoint: `init`, `reindex`, `lookup`, `lsp` subcommands
- `internal/parser/` — Regex-based Elixir parser. Extracts defmodule, def, defp, defmacro, defdelegate, defguard, defprotocol, defimpl, @type, @callback. Handles heredocs, module nesting, alias resolution for defdelegate targets.
- `internal/store/` — SQLite layer. Tables: `files` (path + mtime), `definitions` (module, function, kind, line, file_path, delegate_to, delegate_as), `refs` (module, function, line, file_path, kind).
- `internal/lsp/` — LSP server. `server.go` handles all LSP methods. `elixir.go` contains pure functions for cursor expression extraction, alias/import resolution, use-chain parsing. `rename.go` has rename helpers. `hover.go` has hover formatting. `documents.go` is an in-memory open-buffer store.
- `internal/treesitter/` — Tree-sitter integration for scope-aware variable rename and go-to-references.

## LSP feature map

| LSP method | Handler | Notes |
|---|---|---|
| `textDocument/definition` | `Definition` | Variable (tree-sitter) → current module → imports → use chains → Kernel |
| `textDocument/declaration` | `Declaration` | `@impl` → `@callback` via behaviour modules and use chains |
| `textDocument/implementation` | `Implementation` | `@callback` → all implementing `def` across codebase |
| `textDocument/references` | `References` | Variable (tree-sitter) → function (store + injector scan + opt-binding fallback) |
| `textDocument/hover` | `Hover` in `hover.go` | Same resolution order as Definition |
| `textDocument/rename` | `PrepareRename` + `Rename` | Variable (tree-sitter) → `as:` alias → function → module |
| `textDocument/completion` | `Completion` | Module.func, bare func, use-chain injections |
| `textDocument/signatureHelp` | `SignatureHelp` | Resolves call context then looks up function |
| `textDocument/codeAction` | `CodeAction` | Add alias quick-fix |
| `callHierarchy/incomingCalls` | `IncomingCalls` | Uses refs table + bare call scan |

## Core resolution flow

All navigation features share the same bare-function resolution priority via `resolveBareFunctionModule`:

1. **Current file** — `LookupFunctionInFile(filePath, fn, lineNum)`: checks the enclosing module first (respects sibling nested modules via `LookupEnclosingModule`), then other modules in the same file
2. **Explicit imports** — `import SomeMod` declarations in scope (`ExtractAliasesInScope` is scope-aware per defmodule)
3. **Use chains** — `ExtractUsesWithOpts` extracts `use Mod, key: Val` with consumer opts, then `resolveModuleViaUseChainWithOpts` walks the `__using__` chain resolving dynamic `import unquote(var)` bindings with the actual opts
4. **Kernel** — always in scope

For **module references** (e.g. `Foo.bar`), `resolveModuleWithNesting` handles implicit aliases from nested `defmodule` by walking up enclosing parent modules.

## Use-chain resolution

The `__using__` cache (`usingCacheEntry`) stores the parsed result of each module's `defmacro __using__` body:

- **`imports`** — static `import Mod` statements
- **`inlineDefs`** — functions/macros defined directly in `quote do`
- **`transUses`** — `use Mod` inside the body (double-use chains); also a heuristic for `Keyword.put_new/put`
- **`optBindings`** — dynamic `import unquote(var)` where `var` comes from `Keyword.get(opts, :key, Default)`; stores `{optKey, defaultMod, kind}` so consumer opts override the default

`parseUsingBody` handles three forms:
- `defmacro __using__` — standard form
- `using opts do` — ExUnit.CaseTemplate form (only when `use ExUnit.CaseTemplate` is present)
- Function delegation — when the body calls a local helper like `using_block(opts)`, `parseHelperQuoteBlock` finds the function definition and parses its `quote do` body

`lookupInUsingEntry(moduleName, fn, consumerOpts, visited)` is the recursive lookup. `lookupThroughUse` calls it with full consumer opts from `ExtractUsesWithOpts`. `ParseKeywordModuleOpts` parses `key: Module` pairs from use call opts strings, with alias resolution.

## References — injector scan

References for use-injected functions use two paths, preferring the fast one:

1. **Fast path** — `ExtractUsesWithOpts` on the cursor file → `lookupInUsingEntry` with consumer opts → finds the injecting module in milliseconds. If found, the slow path is skipped entirely.
2. **Slow path** — `findModulesWhoseUsingImports` scans all `__using__` cache entries codebase-wide for modules that statically import `targetModule`. Expensive (~30-65ms). Used for functions from statically-imported modules (e.g. `Ecto.Query` functions).

Call sites are attributed to the **injecting module** in the store (not the defining module), so `LookupReferences(injectorMod, fn)` finds the actual call locations.

## Variable scoping (tree-sitter)

`FindVariableOccurrencesWithTree` handles Elixir's lexical scoping rules:

- **Scope boundaries**: `def`/`defp`/`test` calls; stab clauses (`fn x ->`, case arms) that bind the variable unpinned; `with`/`for` when cursor is in the `do_block` or on a lvalue of `<-`/`=`
- **Pinned variables** (`^x`): references to outer binding, not new bindings — `stabBindsVariable` uses `subtreeContainsUnpinnedIdentifier`
- **Body rebinds** (`fn ^x -> x = nil end`): `stabBodyRebindsVariable` detects assignments; scope is the stab clause; only args are collected (for the pin reference), body is skipped
- **`with` multi-clause**: `collectWithOccurrences` is cursor-position-aware — lhs of clause N collects lhs + subsequent rhs until next rebind; rhs of clause N>0 collects clause N-1's lhs + rhs N forward; do-block collects last binding's lhs + do-block. `cursorNeedsWithScope` in `findEnclosingScope` gates whether the `with` call is a scope boundary.
- **`as:` aliases**: detected in `PrepareRename` (short name ≠ `moduleLastSegment(resolved)`); handled as a file-local text rename, not a codebase-wide module rename

## Rename correctness

`renameFunctionEdits` collects sites from:
1. `LookupFunction` — definition lines
2. `LookupReferences` — indexed call sites (alias/use refs skipped)
3. `FindBareFunctionCalls` — bare intra-module calls in definition files
4. Import-only lines (`import Module, only: [fn: N]`) — flagged `includeKeyword: true`
5. `@spec`/`@callback` lines in definition files

`buildTextEdits` uses `findFunctionTokenColumns` to skip keyword-syntax occurrences (`resource_type: value`) — only `::` type separators pass through. Import-only sites use `findAllTokenColumns` since their keyword keys ARE function names.

## Key design decisions

- **Regex instead of tree-sitter for indexing** — 7.5x faster per file. Tree-sitter is only used when necessary in a
  file already opened by the editor.
- **SQLite for storage** — single file, fast reads, incremental updates via mtime tracking.
- **Parallel indexing** — `init` uses all CPU cores for parsing, single writer for SQLite.
- **Delegate following** — `defdelegate` targets are resolved at index time (including alias resolution and `as:` renames).
- **Git HEAD polling** — watches `.git/HEAD` mtime every 2 seconds to detect branch switches and trigger reindex.
- **Full document sync** — `TextDocumentSyncKindFull`; Elixir files are small enough that incremental sync adds complexity without benefit.
- **Index versioning** — `IndexVersion` in `internal/version/version.go`. Mismatch on startup triggers a forced rebuild. Bump when parser or schema changes would invalidate existing indexes.
