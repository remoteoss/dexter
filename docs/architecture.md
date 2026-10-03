# Dexter architecture

Dexter is a fast Elixir LSP server. It indexes module and function definitions from `.ex`/`.exs` files into a SQLite database and serves near-instant lookups over the Language Server Protocol.

## Module structure

- `cmd/main.go` — CLI entrypoint: `init`, `reindex`, `lookup`, `references`, `lsp`, `version`, and the hidden `daemon` subcommand. `lsp`, `lookup`, `references`, and `reindex` are frontends to the workspace daemon: `lsp` is a raw stdio proxy onto it. Only `init` still opens the store itself, under the same ownership lock the daemon takes, because it needs the workspace to itself.
- `internal/indexer/` — the cold build: walk and stat on all cores, parse on all cores, then one bulk transaction with the indexes dropped. `dexter init` and the LSP server (when it finds an empty index) both call `FullBuild`. `Options.InProcess` marks the server, which shares the database with live readers and so cannot use the connection-wide bulk pragmas.
- `internal/parser/` — Elixir parser backed by a hand-rolled tokenizer (`tokenizer.go`). The tokenizer produces a flat token stream (handling heredocs, sigils, strings, comments as opaque tokens; the code inside a `#{}` interpolation is tokenized as well, into the separate `TokenResult.Interp` stream — see below) and `parser_tokenized.go` walks it to extract defmodule, def, defp, defmacro, defdelegate, defguard, defprotocol, defimpl, @type, @callback, alias, import, use, and Module.function references. Handles module nesting, alias resolution for defdelegate targets, and multi-line expressions natively via bracket depth tracking.
- `internal/beam/` — bounded, bounds-checked readers for the BEAM container, export table, Elixir Docs chunk, Elixir debug info, and persisted module attributes. These readers extract generated callables, their definition lines, and DSL-provider metadata without starting the Erlang VM.
- `internal/store/` — SQLite layer. Tables: `files` (path + mtime), `definitions` (module, function, kind, line, file_path, delegate_to, delegate_as), `refs` (module, function, line, file_path, kind).
- `internal/lsp/` — LSP server. `server.go` handles all LSP methods. `elixir.go` contains pure functions for cursor expression extraction, alias/import/use extraction (tokenizer-based), and use-chain parsing. `rename.go` has rename helpers. `hover.go` has hover formatting. `documents.go` is an in-memory open-buffer store.
- `internal/workspace/` — the protocol-independent owner of one workspace: the store, the shared `lsp.IndexCoordinator`, the headless language-service instance, stdlib discovery, the native and Git watchers, and the single mutation queue every index change enters. `runtime.go` is the lifecycle and the queue; `watch.go` defines the recursive watcher abstraction, with an FSEvents backend on macOS and an fsnotify backend elsewhere or as fallback.
- `internal/daemon/` — the per-workspace daemon and its local transport: endpoint and lock derivation (`endpoint.go`), handshake and framing (`protocol.go`), connection handling and the built-in control methods (`server.go`), the client and the stdio proxies (`client.go`), the adapter registries (`registry.go`), and the per-platform ownership lock. `docs/daemon.md` has the ownership, lifecycle, and extension contract.
- `internal/notify/` — the one path that tells the user about failures, degraded states, and long work. A `Reporter` logs each report and sends it to every attached editor as `window/showMessage` or work-done progress, and keeps the active conditions so it can replay them to an editor that attaches later. `notifytest` has a fake client that records what an editor would show.
- `internal/treesitter/` — Tree-sitter integration for scope-aware variable rename and go-to-references.


## String interpolation (`TokenResult.Interp`)

A string literal stays one token in the main stream, because `@doc` extraction
reads the token text whole and the block-depth tracker must never see a `do` or
an `end` that belongs to an interpolation. The code inside `#{}` is real code
all the same, so the tokenizer records it in a second, byte-ordered stream:
`TokenResult.Interp`, holding only `TokModule`, `TokIdent`, `TokDot` and
`TokAttr` (keywords are dropped on purpose). Nesting recurses, which Elixir
allows: `"outer #{"inner #{x}"}"`. Interpolating (lowercase) sigils are covered;
uppercase sigils are raw text and are not.

Two consumers read it:

- `parseTextFromTokens` drains it in byte order as the main walker passes each
  token (`flushInterpRefs`), so an interpolated reference is resolved with the
  aliases and the enclosing module in force at that point in the file. Both
  streams go through the same `collectModuleRefs`.
- `TokenizedFile.ExpressionAtCursor` (and `FullExpressionAtCursor`) fall back to
  it when the main-stream lookup finds nothing, which is what gives
  go-to-definition, hover and document highlight inside an interpolation. The
  fallback costs one binary search, and only when the cursor is not on an
  expression in the main stream.

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

### Atom dispatch (`use MyAppWeb, :controller`)

The Phoenix entrypoint form dispatches on an atom rather than injecting anything itself:

```elixir
defmacro __using__(which) when is_atom(which), do: apply(__MODULE__, which, [])
def controller do
  quote do ... end
end
```

`usingDispatchParam` recognises this only when `apply/3` targets `__MODULE__` **and** dispatches on that clause's own parameter — a literal function name or another module is not atom dispatch. Runtime parsing is scoped to the indexed module when a file defines multiple modules. When dispatch matches, `parseDispatchBodies` parses each `def name do quote do ... end end` in that module into its own `usingBody`, stored in `usingCacheEntry.dispatch` keyed by function name. Nothing is merged across targets.

Selection happens at lookup time from the literal atom at the `use` site (`UseCall.Which`, or `UseCall.WhichKey` for the `use Mod, live_view: opts` form). `entry.bodyFor(which)` returns the matching body, or nil when the atom names no target — injecting nothing rather than guessing. A `use` that passes no literal atom resolves through the ordinary body, so the feature is purely additive. Completion, injected-alias merging, callback lookup, definitions, and references all select the same body.

Transitive uses retain their complete `UseCall`, including the dispatch atom and keyword options. Visited keys include both module and dispatch target, which permits legitimate same-module chains such as `:api_controller` using `:controller` while still breaking real cycles. Local quoted helpers invoked with `unquote(helper())` are followed recursively.

The token walker cannot evaluate conditionals, so a `use` nested in a compile-time branch (`on_ee do use X end`) is included unconditionally. This over-includes candidate names; it never removes one.

`entry.bodies()` returns the ordinary body plus every dispatch target. `findModulesWhoseUsingImports` uses it, because the references slow path asks which modules *could* inject a name rather than which one a given `use` site selects.

`lookupInUsingEntry(moduleName, fn, consumerOpts, visited)` is the recursive lookup. `lookupThroughUse` calls it with full consumer opts from `ExtractUsesWithOpts`. `ParseKeywordModuleOpts` parses `key: Module` pairs from use call opts strings, with alias resolution.

## Generated functions from BEAM files

Some macros create public functions that have no source definition for Dexter to index. Completion and hover recover those functions from the compiled consumer module while keeping compilation optional:

1. `generatedFunctionsForModule` resolves source-backed modules through the store, derives their Mix build root and application, and stats the expected `Elixir.<Module>.beam` path. When the module itself is generated and therefore absent from the store (for example, Phoenix route helpers), it probes the root application's cached ebin index. It does not glob every dependency ebin directory on the hot path.
2. `loadCompiledFunctionDelta` reads the cheap `AtU8` and `ExpT` chunks first, then subtracts the complete, unbounded set returned by `ModuleFunctionKeys`. The source index wins every conflict, including against a stale BEAM.
3. Only when exports remain does it inflate the `Docs` chunk. The hand-written ETF walker extracts signatures, default-argument arities, hidden flags, and offsets into documentation prose without materialising the full term. Completion reuses the metadata; hover re-inflates lazily for the one requested doc body and memoizes it under the cache entry's own lock, because concurrent hovers each work on a struct copy that still shares that memo and the cache's mutex.
4. The resulting sorted slice is searched by binary search for each completion prefix.

The BEAM readers support both `AtU8` layouts covered by Dexter's OTP 24+ floor: OTP 27 and earlier use one-byte atom lengths, while OTP 28+ negates the atom count and encodes lengths as ERTS tagged integers.

Compilation is never required and source-vs-BEAM mtime is deliberately not a validity gate. A stale compiled module can still describe generated functions absent from the index; if no BEAM exists, the ordinary source-index behavior remains unchanged.

### Cache invalidation

The generated-function cache is a 1,024-entry LRU keyed by module. Invalidation is event-driven wherever a filesystem object exists:

- A compiled result is reused while its BEAM stamp is unchanged.
- A source-backed negative result watches the target ebin directory, whose mtime changes when the BEAM is first compiled.
- Build application names are cached against the `_build/<profile>/lib` directory stamp.
- A store miss also watches the root application's ebin directory for an entirely generated module. It uses a one-second retry TTL as well because a newly indexed source file does not move that directory's mtime.

`DidOpen` prewarms project modules in the background. The completion path remains authoritative and performs the same cached lookup if prewarming has not finished.

With `DEXTER_DEBUG=true`, every completion request logs its total duration and item count. A cold or invalidated compiled-module lookup additionally logs the module, whether it was source-backed, the generated-function count, and its load duration. `cmd/lspprobe -method completion -v` drives that same LSP path repeatedly against a real project, making cold and warm behavior visible without editor-side timestamp inference.

### Generated DSL macros and scope

Spark-generated DSL macros (including Ash sections and entities) neither exist in source nor remain in the consumer's export/import tables after expansion. The consumer BEAM's persisted `extensions` attribute is the record of their providers. `macroProviderAttributes` is the single framework-specific seam for this mapping.

At a cursor inside block path `P`, `dslScopeModules` derives candidates using Spark's naming convention: `<Extension>.<Camel(P)>.<one child segment>`. The prefix is grown one path segment at a time and each step is verified against the provider application's ebin directory, so a segment that derives to no module — a language form such as `defmodule`, `if`, or `for`, wherever it sits in the path — is skipped rather than breaking the chain, and the deepest verified prefix wins. A wrong derivation therefore produces no result rather than an incorrect completion. `treesitter.EnclosingBlockPathWithTree` supplies the lexical block path, and the ebin module listing is cached against the directory mtime so newly compiled entity modules appear without a timer. The cache is capped at 128 directories; it uses arbitrary one-at-a-time eviction at capacity so ordinary reads retain the cheaper read lock instead of mutating an LRU.

The block walk is passed as a thunk and runs only after the compiled consumer reports extension providers. Ordinary modules therefore pay no tree-walk cost. If no scoped entity provider exists, completion falls back to the extension modules themselves for top-level section macros.

Generated-symbol resolution is shared by completion, hover, definition, signature help, references, and call-hierarchy preparation. Definition and call hierarchy cannot point at a source definition for a source-less provider, so they walk the provider's lexical module parents and use the closest module that has an indexed source location. Completion resolve and signature help read the same lazily cached BEAM documentation used by hover.

### Definition lines for generated functions

A generated function has no source definition, but the compiled module's `Dbgi` chunk still records where each def came from. `beam.ReadDefinitionLines` walks `{:debug_info_v1, :elixir_erl, {:elixir_v1, map, specs}}` for the `file`, `relative_file`, and `definitions` keys, and steps over clause bodies without allocating. Clause ASTs can nest very deep, so the ETF reader steps over a term with a count of the terms still to skip instead of recursion: nesting costs no stack, and a count larger than the bytes left is rejected as corrupt.

Each def carries two locations. Its `:line` is the line in the module's own file that was being compiled when the def was produced: the macro call, or wherever a before-compile hook ran. A `file: {path, line}` entry, left by `@file` or `quote location: :keep`, is preferred when `path` is the module's own source, because that is where the code asked for the function. When `path` is another file it is the generator's implementation, and `:line` is used instead. A `file:` line at or before the module's own line is not used either: it is a `location: :keep` quote in a macro defined above the module in the same file. Elixir 1.17 and earlier record the module's line under the map's `line` key instead of `anno`, and both are read. Ash's code interfaces take the second shape: each generated def is expanded at its `define` line, so `:line` is that line and `file` still names `code_interface.ex`, which keeps stacktraces unchanged. No framework is recognized by name.

The parser does not index a def inside a `quote` in a macro other than `__using__` (`defmacro route ... do quote do def handle ... end end`) as a function of the macro's module: it is code the macro generates in its caller, and the caller's BEAM records where it was declared. Without that, a consumer that imports the DSL resolved `Consumer.handle` through the import into the macro's body before the BEAM was asked. What `__using__` injects and a quote in a helper function stay indexed, because the use-chain features above resolve them.

`generatedDefinitionResultsFor` is the single entry point that definition, call-hierarchy preparation, and the references declaration share; qualified definition, the references declaration, and `dexter lookup` reach it through `LookupName`. It starts from `generatedDefinitionResults` and moves to the recorded line only when that line belongs to the file being opened: the debug info must have been compiled from that file (the same absolute path, or else the most path components shared from the end, which covers a moved, copied, or symlinked project; when a module has rows in two files, as two umbrella apps can, the most specific match wins, and an equal match trusts neither), and the line must fall after the module's own line. Any other case keeps the module result. As for generated names, source-vs-BEAM mtime is not a gate: Dexter cannot compile the project, so a stale BEAM is the usual state while editing, and the last compile's line is closer than the module line.

`beam.ReadDefinitionLines` also reads the module's own line (the map's `anno`, a line or `{line, column}`) and the line of each clause when a def's clauses were made at different lines (`DebugInfo.Clauses`: a DSL that adds a clause per call, such as `route :get, "/a"`, gets a location per clause).

Lines are checked against the current text, which is the buffer open in the editor if there is one and the file on disk otherwise (`currentSource`). A line at or before the module's line carries no information and is dropped; both lines come from the same compile (the module's line is Dbgi's `anno` of the BEAM that holds the module, which is the parent's for a module a macro nested in it, and the index's only when no BEAM records one), so the comparison holds even when the file has changed since. A line past the end of the current text is dropped too, because a generator can give a def any line (`quote line: 99`) and an edit can remove lines. When nothing is left, which is the case for a def a `@before_compile` hook made at the module line (and for released Ash's code interfaces), the owning module's body is searched for the declaring call: a call, outside heredocs, whose first argument is the function's name as an atom, also trying the name without a trailing `!` or `?`. That is the generic shape of a macro call that declares a name; an atom in a keyword, a typespec, a later argument, or a module attribute does not count, and neither does a call in a sibling module of the same file (found with the tokenizer). When one line matches, it is used. When several do, as an Ash action (`update :publish`) and the code interface that runs it (`define :publish`) do, the macro that made the function is taken to be the one whose calls name the most of the module's generated functions (`define` names every interface; an action names only itself), and a tie gives no answer. The search is not used for a function whose clauses were made at several lines: those came from calls that do not spell its name (`get "/a"` makes a `match/2` clause), so a call that does, such as `plug :match`, is not where they were declared.

A stale BEAM's lines are not corrected. Only edits to the declaring file move them, by the lines added or removed above, and those files (DSL modules, schemas, routers, dependencies) change less often than their callers. Correcting them meant guessing at text the BEAM was compiled from, and every signal tried either gave up often or could send the editor to a line that looked meaningful and was not. The line from the last compile is near, and the next compile makes it exact.

A module compiled without debug info still has its Docs chunk, whose per-entry anno (`beam.Function.Line`) is often the same `:line`, but not always: a generator can give the def one line and its docs another, as Ash's code interfaces do. It is used when there is no Dbgi, and the `CInf` chunk's `:source` (`beam.ReadSourcePath`) decides which file it belongs to. The `CInf` source also covers a generated module with no source row of its own, such as a Spark entity module: its recorded file is usually the generator's own file. A recorded path inside the project is used as it is. Otherwise it is rebased: first each tail of the recorded path under the project root, longest first, which covers a moved or copied checkout and an umbrella's `apps/<app>/lib`, then the Mix layouts for the application named below the last `lib/` (`lib/<app>`, `deps/<app>/lib/<app>`, or `deps/<app>`). The application comes from the recorded path, not the BEAM's, because Spark creates Ash's entity modules in ash's ebin from spark's source. A recorded path outside the project is the last choice: it is right for a `path:` dependency, but a `_build` copied from another worktree records that worktree, which usually still exists. If no file is found, the lexical parent stays the answer. `CInf` records the path as codepoints, which are decoded to UTF-8. A precise line is the function's own definition, so `LookupName` returns it even for a strict (`ExactModule`) lookup; without one, the existing module results stand. `LookupName` on the name of a module that has no source row returns the file from `CInf` at the module's recorded line, or at its first function's line when the module line is zero, as it is for a module a macro made with `defmodule unquote(name)` and a name it computed, so go-to-definition on a module that `Module.create` made works too. A bare call that no source resolves also checks the BEAMs of the file's imported modules for a generated function of that name.

The debug info and compile info are read on the first definition request that needs them and memoized on the module's generated-function cache entry, so the BEAM stamp invalidates them with everything else. A module compiled without debug info, or an Erlang module, is memoized as empty.

For references, the parser records bare injected calls under the direct `use` module because the generated provider is unavailable while source is indexed. At lookup time, generated-symbol resolution queries those injector rows and validates every candidate against the compiled provider active at that candidate's block path. This keeps same-named macros from another DSL or another section out of the result. Statement-level injected calls with arguments are indexed even when they omit both parentheses and a `do` block, as in `authorize_if always()`.

## References — injector scan

References for use-injected functions use two paths, preferring the fast one:

1. **Fast path** — `ExtractUsesWithOpts` on the cursor file → `lookupInUsingEntry` with consumer opts → finds the injecting module in milliseconds. If found, the slow path is skipped entirely.
2. **Slow path** — `findModulesWhoseUsingImports` scans all `__using__` cache entries codebase-wide for modules that statically import `targetModule`. Expensive (~30-65ms). Used for functions from statically-imported modules (e.g. `Ecto.Query` functions).

Call sites are attributed to the **injecting module** in the store (not the defining module), so `LookupReferences(injectorMod, fn)` finds the actual call locations.

An explicit definition in the consumer shadows a same-named injected function. `resolveBareFunctionModuleWithOrigin` preserves whether the winning definition came from the cursor file, preventing the fast path from widening an override to unrelated consumers. When multiple `use` declarations can inject the name, they are checked in reverse source order—the later declaration wins, matching `lookupThroughUse` and completion precedence.

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

## Telling the user

A language server that fails silently looks broken: an editor writes the server's stderr to a log file that nobody reads. So every condition that stops Dexter from working, or makes it work with less, goes through `notify.Reporter`, which logs it as before and also shows it in the editor.

- **One reporter for each workspace.** It lives on the `lsp.IndexCoordinator`, like the store, so the daemon's headless service (which runs the index builds) and every editor session share it. A session attaches its client in `initialized`, not in `initialize`, because a server must not send `window/workDoneProgress/create` before the initialize response. It detaches when the session closes.
- **Conditions, not events.** `Set(key, severity, message)` makes a condition active: Error when Dexter does not work, Warning when it works with less, Info for a normal state change such as a first build. Setting an active key again with the same severity only updates its text, so a count that changes does not send a message each time. `Clear(key, message)` says that the condition stopped. `Notify` is for an event that does not continue.
- **Replay on attach.** The daemon serves several editors. An editor that attaches while a rebuild runs, or while the watcher is degraded, receives the active conditions in the order they started, and the work in progress as a new progress begin.
- **Progress for long work.** A cold build and a rebuild show work-done progress where the client capability `window.workDoneProgress` allows it; their start and end are also conditions, so a client without progress support still sees them. An incremental pass shows progress only after 1,000 changed files, with a message at the start and the end as the fallback.
- **Aggregate, never per file.** Files that cannot be read, parsed, or written go into one set on the coordinator; the user sees one message ("3 files could not be indexed: <first path> and 2 more") and one more when the set is empty again. Recording a success costs one atomic load when no file has failed, so the walk and the watchers pay nothing in the usual case.
- **Delivery never blocks.** Each attached editor has its own queue and goroutine. A slow editor loses progress reports first, and nothing on a request path or in the mutation loop waits for an editor.
- **A frontend that cannot start still speaks LSP.** When `dexter lsp` cannot reach a daemon (a contract mismatch in either direction, a root spelling mismatch, a workspace held by `dexter init`, a daemon that does not start), it reads the editor's `initialize` request, sends `window/showMessage` with the explanation and the fix, answers `initialize` with a JSON-RPC error (-32603) that carries the same text, and exits.
- **The CLI states the index too.** `lookup`, `references`, and `reindex` results carry the active index conditions (`notes`), and the CLI prints a warning as `note:` and an error as `error:` on stderr. `--quiet` hides warnings, never errors. `workspace/status` returns every active condition.

What is reported, and what stays in the log only:

| Condition | Key | Severity | Clears with a message |
|---|---|---|---|
| First build of an empty index (once for each workspace: an index with no Elixir files stays empty, and each full pass over it would be "cold") | `index.build` | Info, with progress | yes ("index built") |
| Index written by another index version, or damaged (`SQLITE_CORRUPT`, `SQLITE_NOTADB`): deleted and rebuilt | `index.rebuild` | Warning, with progress | yes ("rebuild is complete") |
| Index locked by another process (`SQLITE_BUSY`, `SQLITE_LOCKED`): never deleted; the daemon waits up to 30 s, then fails to start, and the editor shows the end of the daemon log | `index.unavailable` | Error | yes, when the lock goes during the wait |
| Index that cannot be opened for another cause (permissions, full disk, open-file limit): not deleted, because a rebuild does not fix it | `index.unavailable` | Error | no (the daemon does not start) |
| Bulk build left the index without its SQL indexes | `index.unavailable` | Error | no (restart needed) |
| Fast full build failed, so files are indexed one by one | `index.fallback` | Warning | through the build end message |
| Files that could not be indexed | `index.files` | Warning | yes |
| Root is the home directory, or does not look like a project | `root` | Warning | no |
| Native file watching cannot start | `watcher` | Warning | yes |
| macOS FSEvents unavailable, so fsnotify runs | `watcher.fallback` | Warning | no |
| Directories that cannot be watched | `watcher.coverage` | Warning | yes |
| The workspace has no Elixir standard library (read from the root that all sessions share, not from what one session found) | `stdlib` | Warning | yes |
| `mix` not found for one session | (this editor only) | Warning | — |
| `mix format` cannot run, or Elixir/OTP mismatch, in one Mix project | `formatter:<mix root>`, `formatter.otp:<root>` | Warning, Error | yes ("formatting works again in <mix root>") |
| A rename that could not change some files | (this editor only) | Error | — |

A syntax error in the user's code is not a formatter failure: it is a diagnostic. WAL checkpoint warnings, fsnotify transient errors, the per-directory watch errors (they are in the aggregate), BEAM formatter restarts that fall back to `mix format`, and requests that waited for the first build stay in the log.

## Key design decisions

- **Tokenizer instead of tree-sitter for indexing** — a hand-rolled tokenizer + walker replaced the original regex-based parser for both file indexing and runtime `__using__` parsing. The tokenizer handles heredocs, sigils, multi-line expressions, and comments as opaque tokens, eliminating fragile line-joining heuristics. Tree-sitter is only used for scope-aware variable operations in files already opened by the editor.
- **SQLite for storage** — single file, fast reads, incremental updates via mtime tracking.
- **Parallel indexing** — the cold build uses all CPU cores for parsing, single writer for SQLite. Both callers share `indexer.FullBuild`; the server used to have a second, serial implementation that parsed one file at a time and committed a transaction per file, which measured ~4x slower on a 10k-file corpus.
- **Two write paths** — `indexer.FullBuild` is insert-only and allocates file ids from a counter, so it is only correct on an empty index and only with no other writer active. Everything else — incremental sweeps included — goes through the per-file path, which deletes a file's rows before reinserting them.
- **`IndexCoordinator.writes` covers every writer, without exception.** A full build holds it for writing; every other writer — a save, a watched-file event, a rename, and the incremental sweep's own walk — holds it for reading. A new writer that skips it can run inside an insert-only build, whose emptiness precondition it then violates. The coordinator belongs to the workspace rather than to a session, so every LSP session attached to one store shares it, and the daemon's ownership lock extends the precondition across processes: `dexter reindex` in a shell now enters the same queue instead of opening a second writer the lock could not see.
- **Emptiness is decided under the lock, never sampled.** `Server.fullBuild` tests `IsEmpty` while holding the write lock and reports through its `ran` return value, because one save arriving between a sample and the lock invalidates the answer. `Store.IsEmpty` answers `false` when its own query fails, so a database too broken to count is never taken for an empty one.
- **The sweep's prune re-checks the filesystem.** `pruneMissingFiles` deletes only stored paths that are absent from the walk *and* fail to stat. The walk is one traversal, so a file saved after it passed that directory is legitimately missing from `seen`; the stat is also what stops a walker that yields nothing from deleting the whole index.
- **A cold build is one WAL transaction**, so the `-wal` file grows to roughly the size of the index — a few hundred MiB on a large monorepo — until the post-build checkpoint reclaims it. `wal_autocheckpoint` cannot touch frames belonging to an open transaction, and `InProcess` deliberately leaves `journal_mode` alone, so the server cannot avoid this the way `dexter init` does.
- **A failed cold build must not stamp the index version.** `SetIndexVersion` runs last, so any failure leaves a version the next start rejects. That is the whole recovery path: the workspace runtime's `openStore` sees the mismatch on a populated index, drops the derived index, and reopens — in the one process holding the ownership lock, so there are no live readers and nothing else can be writing. An index that does not open is deleted only when `store.ClassifyOpenError` says that it is damaged. A locked index is never deleted: `dexter init` and older releases write it with a rollback journal, and deleting the files under such a writer loses its work in silence. `indexer.ErrUnindexed` — indexes not recreated after the load committed or rolled back — marks writes unavailable for the rest of the live process and must never fall back to the full incremental sweep, whose `DELETE` by `file_id` would scan the full `definitions` and `refs` tables once per file on disk.
- **Delegate following** — `defdelegate` targets are resolved at index time (including alias resolution and `as:` renames). `LookupFollowDelegate` follows chains recursively (up to 5 hops) so `A → B → C` resolves to `C`.
- **Git HEAD polling** — watches `.git/HEAD` mtime every 2 seconds to detect branch switches and trigger reindex. The workspace runtime owns this poll; a daemon-backed LSP session does not start its own.
- **Full document sync** — `TextDocumentSyncKindFull`; Elixir files are small enough that incremental sync adds complexity without benefit.
- **One workspace, one owner** — a per-workspace daemon holds an advisory kernel lock (`flock`, `LockFileEx`) for its lifetime and is the only process that opens the index or watches the tree. The kernel releases the lock when the process dies, so a crash leaves no stale lock and no PID file to reconcile; the lock file may outlive the process and is never unlinked, because unlinking a lock another process holds would admit a second writer. Identity — the symlink-resolved root, in the case the file system stores it (macOS `F_GETPATH`) — decides which daemon owns the physical workspace, while the indexed root keeps the starter's spelling so stored paths still match the URIs an editor sends. A frontend using another spelling is rejected during the handshake.
- **Frontends are thin** — an editor connection is a raw byte proxy after a one-line handshake, so the daemon hop costs a copy rather than a JSON parse, and a CLI call is one multiplexed control request. Protocol adapters register (`daemon.RegisterFrontend`, `daemon.RegisterMethod`) instead of editing dispatch, and reach workspace state through `workspace.Runtime` rather than opening a store, starting a watcher, or running a second LSP lifecycle.
- **Index versioning** — `IndexVersion` in `internal/version/version.go`. A mismatch on startup triggers a forced rebuild *of a populated index*; an empty one has no stale data to discard, so the live server builds it in the background through `indexer.FullBuild` instead. Bump when parser or schema changes would invalidate existing indexes.
