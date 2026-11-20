# Agent guidelines

For a detailed reference of the LSP code flows, use-chain resolution, variable scoping, and rename logic, see `docs/architecture.md`.

## Building and testing

```sh
make build  # build dexter binary
make test   # run all tests
make lint   # run after completing a set of changes
```

Install `golangci-lint` if needed:
```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.4
```

## Conventions

- Our guiding principles for all code written are: We want maximum performance and speed, but without sacrificing
  correctness of results. It's okay to sacrifice some readability in the name of performance, but be mindful of the
  tradeoffs.
- When fixing reported bugs, write the regression test first, verify the failure, and then write the fix.
- When given real code causing bugs, use fake/generic module and function names in tests (e.g. `MyApp.Accounts`, `SharedLib.Worker`) — never use
  real names from the user's codebase, even for regression tests
- When writing code that affects the in-document runtime parsing that is similar to the index parsing (or vice versa),
  see if the code can be unified to reduce deduplication. But we should never do this at the cost of performance.
  Indexing must be kept fast no matter what.
- Keep the CLI commands (`init`, `reindex`, `lookup`, `references`, `stop`, `log`, `prune`) working with no editor open.
  - `reindex`, `lookup`, and `references` are frontends to the workspace daemon, not clients of the LSP. `lsp` and `mcp` are frontends too.
  - `init` is the only command that opens the index itself. It holds the workspace's ownership lock while it builds.
  - The daemon is the only process that opens the index for normal operation. It belongs to the workspace, not to a frontend. `dexter lsp` is a stdio proxy, and nothing may serve an editor in-process.
- Every frontend finds the workspace of a path with `store.ResolveRoot` (`internal/store/project.go`). Do not add another root rule. A folder of repositories (a "container") has no index: the CLI refuses it, and `lsp` and `mcp` send each file or call to the daemon of its own project.
- Parser tests should cover real-world Elixir patterns from large codebases

## Version numbers

- `Version` and `IndexVersion` are in `internal/version/version.go`. `ContractVersion` is in `internal/daemon/endpoint.go`.
- Do not bump them in feature PRs. Version bumps are made together in a separate release PR.
- In a feature PR, note when a parser or schema change makes existing indexes give wrong results, so that the release
  PR bumps `IndexVersion` with `Version`.
- Whenever `IndexVersion` is bumped, bump `ContractVersion` with it: else a running daemon of the older build keeps
  serving the new frontend from the stale index.

What happens on a mismatch: when the daemon opens the index (`openStore` in `internal/workspace/runtime.go`) and the
stored `IndexVersion` is not the current one, it deletes the index files and starts a new, empty index file. The cold
build then fills the new file and stamps the version. This also applies to a 0.7.x index that the open has already
emptied (`dropPreFileIDSchema` drops the old tables), because that index still has a stored version. The new file gets
incremental auto-vacuum, which an existing file cannot change to. Only an index with no stored version and no rows is
kept as it is; the cold build fills it. `store.MarkUpgraded` records the upgrade, so that when an older build rebuilds the
index again, the next start tells the user that two Dexter versions use the project.

## What to check when making changes

| Area changed | Also check |
|---|---|
| `parseUsingBody` / use-chain logic | `lookupInUsingEntry`, `resolveModuleViaUseChainWithOpts`, `findModulesWhoseUsingImports` |
| `resolveBareFunctionModule` | All callers pass `lineNum` — used by `LookupFunctionInFile` to prefer the enclosing module |
| Rename and references sites (`siteCollector` in `function_sites.go`) | References list the same sites. Columns come from `siteColumns`/`siteColumnsAnyArity` in `rename_sites.go`: `findFunctionTokenColumns` skips keyword keys, and sites with `includeKeyword` (import `only:` keys, `defoverridable`, callbacks) use `findAllTokenColumns`. `tokenArity` must count arity as the parser does (`parser.CallArity`) |
| Variable scoping (`variables.go`) | Pin operator handling, `with` multi-clause, stab body rebinds |
| Tokenizer string/heredoc/sigil scanning | `TokenResult.Interp` (the `#{}` token stream), `flushInterpRefs`, `TokenizedFile.ExpressionAtCursor` |
| `ExtractAliases` | Handlers that resolve a name at a position use the scope-aware `ExtractAliasesInScope(lineNum)`. The unscoped `ExtractAliases` is for whole-file work: parts of `Completion`, the `use` readers in `module_members.go`, `CalleesOf`, and rename in a file with one module and no alias in a function body |
| Any new store query | Add an index if the query will run on hot paths (definition, hover, references). Check `EXPLAIN QUERY PLAN` first: index build time is a large part of a cold build |
| Parser output (definitions, refs, arity) | The release's `IndexVersion` (see "Version numbers"). Refs store the arity of each call (-1 when unknown); count it with `parser.CallArity` so the index and rename agree |
| `store.ResolveRoot` / containers | `internal/store/project.go` tests, the CLI (`requireProjectRoot`), the LSP proxy routing (`store.NestedProject`, `internal/daemon/lsp_route.go`), and the MCP `project` argument (`internal/mcp/projects.go`) |
| `internal/beam` ETF tag handling | The ERTS external term format spec. One wrong field width desynchronises every later term in the chunk (`EXPORT_EXT` carries its arity as an integer term, `NEW_FUN_EXT` as a raw byte) |
| `internal/workspace/runtime.go` (mutation queue, readiness, subscribers) | `internal/lsp` write coordination (`IndexCoordinator`), `watch.go` event filtering, failed-watch retries and one-shot coverage reconciliation, `workspace/watch` subscribers, and the `Close` ordering: watchers stop before the queue drains, and the queue drains before the store checkpoints |
| A new failure or degraded state | Report it through the workspace `notify.Reporter` (`IndexCoordinator.Reporter()`): a condition key with `Set`, and `Clear` with a message when it stops. A log line alone is not seen in an editor. Report only on state changes, never per file or per request. See "Telling the user" in `docs/architecture.md` |
| `internal/daemon` (protocol, registries, endpoint) | `ContractVersion`, the reserved method and kind names in `registry.go`, `client.go` multiplexing (one reader, ids matched to callers, notifications interleaved), and `docs/daemon.md`. Socket paths must stay short: `sockaddr_un` is capped near 104 bytes |

## Token walking

Use `parser.TokenWalker` for new code that iterates over tokens. It provides:
- Consistent depth tracking with automatic clamping (never goes negative)
- Forward progress guarantees via `EnsureProgress()`
- Module/function detection helpers
- Statement boundary handling

See `internal/parser/token_walker.go` for the API and `token_walk_test.go` for examples.

The consistency tests in `elixir_test.go` (`TestModuleScopeConsistency`, `TestDepthTrackingConsistency`) verify that similar functions handle edge cases the same way. Add new edge cases there when fixing scope-related bugs.

## Common gotchas

- **Nested modules**: `defmodule Inner do` inside `Outer` creates `Outer.Inner` in the store, but the line only says `Inner`. A dotted name nests too: `defmodule Deep.Inner` inside `Outer` is `Outer.Deep.Inner`, and `Deep` is an alias in `Outer` after it, as in Elixir. Use `LookupModulesInFile` / `LookupEnclosingModule` rather than regex-scanning lines for module names.
- **`as:` aliases**: `alias Foo.Bar, as: Baz` — `Baz` ≠ last segment of `Foo.Bar`. Detected in `PrepareRename` by comparing the alias short name to `moduleLastSegment(resolved)`.
- **Import-only sites in rename**: Lines like `import Module, only: [fn: 1]` have the function name as a keyword key. They are flagged `includeKeyword: true`, so `siteColumnsAnyArity` uses `findAllTokenColumns` instead of `findFunctionTokenColumns`.
- **Dynamic use-chain imports**: `import unquote(mod)` goes into `optBindings`, not `imports`. Static `findModulesWhoseUsingImports` won't find these — the fast opt-binding path in the References handler handles them instead.
- **ExUnit.CaseTemplate**: Uses `using opts do` instead of `defmacro __using__`. Only recognised when the module has `use ExUnit.CaseTemplate`. The body may delegate to a helper function (`using_block(opts)`) — `parseHelperQuoteBlock` follows this.
- **Performance**: `findModulesWhoseUsingImports` is expensive (~30-65ms). The References handler skips it when the fast opt-binding check already found injectors.
- **A session is not a process**: one daemon serves every editor and CLI client on a workspace, so nothing on a path shared with daemon-backed sessions may call `os.Exit`. `Server.Exit` closes the stream instead when `manageWorkspace` is false (every daemon session); `Shutdown` and disconnects go through `CloseSession`, which is `sync.Once`-guarded. The embedded `manageWorkspace` path exists for tests and library use only — the CLI never selects it, because one workspace has one owner
- **Session state stays per-session, index state stays shared**: open documents, client capabilities, and position encoding belong to one `Server`; the store, `IndexCoordinator`, watchers, and caches belong to the `workspace.Runtime`. A new field on `Server` is a new per-editor copy unless it deliberately goes on the coordinator.
- **Daemon-backed sessions do not manage the workspace**: with `ManageWorkspace: false`, `Initialize` starts no reindex and no `.git/HEAD` poll — the runtime does both. It still registers `didChangeWatchedFiles`, because the editor's glob covers `deps/` and path dependencies that the native watcher deliberately skips; duplicate events from both sources coalesce by path. Editor notifications are routed through `WorkspaceEvents` into the one mutation queue rather than written to the index directly.
- **A workspace has to look like one**: `init`, `reindex`, `lookup`, and `references` refuse the home directory, a directory with no `mix.exs` or `.git`, and a folder of repositories, unless `-y/--yes` is passed (`requireProjectRoot`). A Dexter database alone does not make a project. So a mistyped command cannot build an index over the home directory. `lsp` warns and serves anyway (the editor decides), and `stop` never checks, so a stray daemon can always be cleaned up. Indexes that an earlier Dexter made above the project are "stray": no session uses them, and `dexter prune` lists or deletes them
