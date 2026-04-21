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
- Keep the CLI commands (`init`, `reindex`, `lookup`) working independently of the LSP server
- Parser tests should cover real-world Elixir patterns from large codebases
- Version strings (`Version` and `IndexVersion`) live in `internal/version/version.go`

## When to bump IndexVersion

Bump `IndexVersion` (alongside `Version`) whenever a parser change or schema change would make existing indexes produce
wrong results. The LSP server checks this on startup and triggers a forced rebuild on mismatch.

## What to check when making changes

| Area changed | Also check |
|---|---|
| `parseUsingBody` / use-chain logic | `lookupInUsingEntry`, `resolveModuleViaUseChainWithOpts`, `findModulesWhoseUsingImports` |
| `resolveBareFunctionModule` | All callers pass `lineNum` — used by `LookupFunctionInFile` to prefer the enclosing module |
| Rename edits (`buildTextEdits`) | `findFunctionTokenColumns` (skips keyword keys), `includeKeyword` flag on import sites |
| Variable scoping (`variables.go`) | Pin operator handling, `with` multi-clause, stab body rebinds |
| `ExtractAliases` | LSP handlers use `ExtractAliasesInScope(text, lineNum)` — scope-aware. Only `Completion` and `CodeAction` use the unscoped `ExtractAliases` intentionally |
| Any new store query | Add an index if the query will run on hot paths (definition, hover, references) |

## Token walking

Use `parser.TokenWalker` for new code that iterates over tokens. It provides:
- Consistent depth tracking with automatic clamping (never goes negative)
- Forward progress guarantees via `EnsureProgress()`
- Module/function detection helpers
- Statement boundary handling

See `internal/parser/token_walker.go` for the API and `token_walk_test.go` for examples.

The consistency tests in `elixir_test.go` (`TestModuleScopeConsistency`, `TestDepthTrackingConsistency`) verify that similar functions handle edge cases the same way. Add new edge cases there when fixing scope-related bugs.

## Common gotchas

- **Nested modules**: `defmodule Inner do` inside `Outer` creates `Outer.Inner` in the store but the raw line only says `Inner`. Use `LookupModulesInFile` / `LookupEnclosingModule` rather than regex-scanning lines for module names.
- **`as:` aliases**: `alias Foo.Bar, as: Baz` — `Baz` ≠ last segment of `Foo.Bar`. Detected in `PrepareRename` by comparing the alias short name to `moduleLastSegment(resolved)`.
- **Import-only sites in rename**: Lines like `import Module, only: [fn: 1]` have the function name as a keyword key. They're flagged `includeKeyword: true` so `buildTextEdits` uses `findAllTokenColumns` instead of `findFunctionTokenColumns`.
- **Dynamic use-chain imports**: `import unquote(mod)` goes into `optBindings`, not `imports`. Static `findModulesWhoseUsingImports` won't find these — the fast opt-binding path in the References handler handles them instead.
- **ExUnit.CaseTemplate**: Uses `using opts do` instead of `defmacro __using__`. Only recognised when the module has `use ExUnit.CaseTemplate`. The body may delegate to a helper function (`using_block(opts)`) — `parseHelperQuoteBlock` follows this.
- **Performance**: `findModulesWhoseUsingImports` is expensive (~30-65ms). The References handler skips it when the fast opt-binding check already found injectors.
