# Dexter architecture

Dexter is a fast Elixir LSP server. It indexes module and function definitions from `.ex`/`.exs` files into a SQLite database and serves near-instant lookups over the Language Server Protocol.

## Module structure

- `cmd/main.go` — CLI entrypoint: `init`, `reindex`, `lookup`, `lsp` subcommands
- `internal/indexer/` — the cold build: walk and stat on all cores, parse on all cores, then one bulk transaction with the indexes dropped. `dexter init` and the LSP server (when it finds an empty index) both call `FullBuild`. `Options.InProcess` marks the server, which shares the database with live readers and so cannot use the connection-wide bulk pragmas.
- `internal/parser/` — Elixir parser backed by a hand-rolled tokenizer (`tokenizer.go`). The tokenizer produces a flat token stream (handling heredocs, sigils, strings, comments as opaque tokens) and `parser_tokenized.go` walks it to extract defmodule, def, defp, defmacro, defdelegate, defguard, defprotocol, defimpl, @type, @callback, alias, import, use, and Module.function references. Handles module nesting, alias resolution for defdelegate targets, and multi-line expressions natively via bracket depth tracking.
- `internal/store/` — SQLite layer. Tables: `files` (path + mtime), `definitions` (module, function, kind, line, file_path, delegate_to, delegate_as), `refs` (module, function, line, file_path, kind).
- `internal/lsp/` — LSP server. `server.go` handles all LSP methods. `elixir.go` contains pure functions for cursor expression extraction, alias/import/use extraction (tokenizer-based), and use-chain parsing. `rename.go` has rename helpers. `hover.go` has hover formatting. `documents.go` is an in-memory open-buffer store.
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

`parseUsingBody` uses the tokenizer to walk the `__using__` body directly on the token stream. This avoids line-joining heuristics and correctly handles heredocs in moduledocs (which previously caused a regression where `bracketDepth` in line-based joining treated `#` inside markdown links as comments, cascading into file-wide line merges). It handles three forms:
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

### Who moves a file

A module rename also moves files whose names follow the module naming convention, and who performs the move depends on whether the editor holds the file:

- **Closed files** — the server writes the new path and deletes the old one. This keeps large renames off the wire.
- **Open files** — the client moves them, through a `rename` resource operation in the reply's `documentChanges`, ordered right after that file's own TextEdits so the edited buffer travels to the new path. The server touches neither path. Moving an open file server-side leaves the editor holding a modified buffer pointing at a deleted path, and the next save recreates the old file with the new module name — two files defining the same module.
- **Open files, client without `resourceOperations: ["rename"]`** — the module is renamed in place and the file keeps its old name. Nothing is deleted underneath a live buffer.

`protocol.WorkspaceEdit` from `go.lsp.dev/protocol` types `documentChanges` as `[]TextDocumentEdit` and cannot carry resource operations, so `internal/lsp/workspace_edit.go` defines the wire types and `renameHandler` answers `textDocument/rename` ahead of the generated dispatcher. A client that understands `documentChanges` ignores `changes` entirely, so once one file moves, every edit in the reply goes through `documentChanges`.

### Grouped aliases

`alias Old.{A, B}` (and the `require`/`import` forms) names the module once, as the prefix, while the index records one reference per member — so a member's full name never appears on the line. `findGroupedAliasEdits` handles both directions: renaming the prefix rewrites the prefix, renaming a member rewrites that member inside the braces. Since every member on the line resolves to the same prefix edit, `applyEdits` drops TextEdits that overlap one already emitted for that line; the on-disk path rewrites the line as it goes and never sees the second match.

## Indexing throughput

The cold index is bound by the single SQLite writer, not by parsing. On a 70k-file monorepo the parse workers burn ~14s of CPU across 15 cores (~1s of wall time) while the writer needs ~4s, so the pipeline runs at the speed of one core. Anything that removes bytes or statements from the writer is worth CPU spent in the parse workers, which have ~15x headroom.

Consequences that are easy to undo by accident:

- **Refs are deduplicated in the parser** (`dedupeRefs`), not in the store. Refs are line-granular, so `@spec f(String.t(), String.t())` produces identical rows; ~60k of them on a large monorepo. Identical rows cannot change a result — no query counts refs, and the References handler dedupes by file+line — so the parse workers drop them before they reach the writer.
- **The bulk path batches inserts** into multi-row `INSERT`s (`multiRowInsert`, 900 bound parameters per statement, which is under even the legacy `SQLITE_MAX_VARIABLE_NUMBER` of 999). Incremental reindex keeps the row-at-a-time path, where a file's `DELETE` must stay ordered ahead of its `INSERT`s.
- **Prefix queries use a range, never `LIKE`.** `LIKE` is case-insensitive by default, so SQLite cannot turn `module LIKE 'Prefix.%'` into an index range and scans all refs. `module >= 'Prefix.' AND module < 'Prefix/'` ('/' is '.'+1) uses `idx_refs_module_function` and turns that scan into a range search: on a 3.9M-row index, 11-14x faster with a warm page cache and ~190x faster cold.
- **`idx_refs_function_kind` was retired.** No query leads with `function`; the two that filter on function/kind both lead with `file_path`. It cost 80 MB and a share of every index rebuild. Check `EXPLAIN QUERY PLAN` before adding an index here — index build time is ~40% of a cold index.

The largest remaining win is interning `file_path`: every ref row stores a ~122-character absolute path, but there are only ~69k distinct paths, so the column and `idx_refs_file_path` together account for well over half the database.

## Key design decisions

- **Tokenizer instead of tree-sitter for indexing** — a hand-rolled tokenizer + walker replaced the original regex-based parser for both file indexing and runtime `__using__` parsing. The tokenizer handles heredocs, sigils, multi-line expressions, and comments as opaque tokens, eliminating fragile line-joining heuristics. Tree-sitter is only used for scope-aware variable operations in files already opened by the editor.
- **SQLite for storage** — single file, fast reads, incremental updates via mtime tracking.
- **Parallel indexing** — the cold build uses all CPU cores for parsing, single writer for SQLite. Both callers share `indexer.FullBuild`; the server used to have a second, serial implementation that parsed one file at a time and committed a transaction per file, which measured ~4x slower on a 10k-file corpus.
- **Two write paths** — `indexer.FullBuild` is insert-only and allocates file ids from a counter, so it is only correct on an empty index and only with no other writer active. Everything else — incremental sweeps included — goes through the per-file path, which deletes a file's rows before reinserting them.
- **`indexWrites` covers every writer, without exception.** A full build holds it for writing; every other writer — a save, a watched-file event, a rename, and the incremental sweep's own walk — holds it for reading. A new writer that skips it can run inside an insert-only build, whose emptiness precondition it then violates. The precondition is process-local: it says nothing about a concurrent `dexter reindex` in a shell.
- **Emptiness is decided under the lock, never sampled.** `Server.fullBuild` tests `IsEmpty` while holding the write lock and reports through its `ran` return value, because one save arriving between a sample and the lock invalidates the answer. `Store.IsEmpty` answers `false` when its own query fails, so a database too broken to count is never taken for an empty one.
- **The sweep's prune re-checks the filesystem.** `pruneMissingFiles` deletes only stored paths that are absent from the walk *and* fail to stat. The walk is one traversal, so a file saved after it passed that directory is legitimately missing from `seen`; the stat is also what stops a walker that yields nothing from deleting the whole index.
- **A cold build is one WAL transaction**, so the `-wal` file grows to roughly the size of the index — a few hundred MiB on a large monorepo — until the post-build checkpoint reclaims it. `wal_autocheckpoint` cannot touch frames belonging to an open transaction, and `InProcess` deliberately leaves `journal_mode` alone, so the server cannot avoid this the way `dexter init` does.
- **A failed cold build must not stamp the index version.** `SetIndexVersion` runs last, so any failure leaves a version the next start rejects. That is the whole recovery path: `cmdLSP` sees the mismatch on a populated index and runs `cmdInit(force)` synchronously, in a process with no live readers. `indexer.ErrUnindexed` — indexes not recreated after the load committed or rolled back — marks writes unavailable for the rest of the live process and must never fall back to the full incremental sweep, whose `DELETE` by `file_id` would scan the full `definitions` and `refs` tables once per file on disk.
- **Delegate following** — `defdelegate` targets are resolved at index time (including alias resolution and `as:` renames). `LookupFollowDelegate` follows chains recursively (up to 5 hops) so `A → B → C` resolves to `C`.
- **Git HEAD polling** — watches `.git/HEAD` mtime every 2 seconds to detect branch switches and trigger reindex.
- **Full document sync** — `TextDocumentSyncKindFull`; Elixir files are small enough that incremental sync adds complexity without benefit.
- **Index versioning** — `IndexVersion` in `internal/version/version.go`. A mismatch on startup triggers a forced rebuild *of a populated index*; an empty one has no stale data to discard, so the live server builds it in the background through `indexer.FullBuild` instead. Bump when parser or schema changes would invalidate existing indexes.
