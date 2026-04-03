# Dexter

Dexter is a fast Elixir go-to-definition engine that runs as an LSP server. It indexes module and function definitions from `.ex`/`.exs` files into a SQLite database and serves near-instant lookups over the Language Server Protocol.

## Architecture

- `cmd/main.go` — CLI entrypoint with `init`, `reindex`, `lookup`, and `lsp` subcommands
- `internal/parser/` — Regex-based Elixir parser that extracts definitions (defmodule, def, defp, defmacro, defdelegate, defguard, defprotocol, defimpl, etc.). Handles heredocs, module nesting, and alias resolution for defdelegate targets.
- `internal/store/` — SQLite storage layer with tables for files (path + mtime) and definitions (module, function, kind, line, file_path, delegate_to, delegate_as). Supports lookup by module, function, and delegate following.
- `internal/lsp/` — LSP server implementation. `server.go` handles lifecycle and document sync. `elixir.go` contains pure functions for cursor expression extraction, alias/import resolution, and local buffer function search. `documents.go` is an in-memory store for open buffer contents.

## Building

```sh
go build -o dexter ./cmd/
```

## Testing

```sh
go test ./...
```

Tests include unit tests for the parser, store, and LSP elixir analysis functions, plus integration tests that scaffold a fake Elixir project, run `dexter init`, and verify lookups.

## Linting

Run `make lint` after completing a set of changes and before marking work as done:

```sh
make lint
```

Install `golangci-lint` via Go modules if you don't yet have it:

```sh
go install github.com/golangci/golangci-lint/cmd/golangci-lint@v2.9.0
```

## Key design decisions

- **Regex over tree-sitter** — 7.5x faster per file. The regex parser handles heredocs, module nesting, and all def forms. Edge cases are fixed as they come up.
- **SQLite for storage** — single file, fast reads, incremental updates via mtime tracking.
- **Parallel indexing** — `init` uses all CPU cores for parsing, single writer for SQLite.
- **Delegate following** — `defdelegate` targets are resolved at index time (including alias resolution and `as:` renames). Followed by default on lookup.
- **Git HEAD polling** — the LSP server watches `.git/HEAD` mtime every 2 seconds to detect branch switches and trigger reindex.
- **Full document sync** — the LSP uses `TextDocumentSyncKindFull` since Elixir files are small.
- **Index versioning** — `internal/version/version.go` has an `IndexVersion` integer alongside `Version`. When the LSP server or `reindex` command starts, it checks the version stored in the `metadata` table against `IndexVersion`. A mismatch triggers an automatic forced rebuild. Bump `IndexVersion` (alongside `Version`) whenever a parser change or schema change would make existing indexes produce wrong results.

## Performance

Any changes that touch the parser or indexing pipeline should be profiled against a real Elixir codebase to verify they don't increase cold indexing time. Use the built-in profiling flag:

```sh
dexter init --force --profile /path/to/elixir/project
```

`--force` ensures a full cold index (no mtime cache hits). `--profile` prints a timing breakdown for each indexing phase to stdout. Compare results before and after your change on the same codebase. A large Elixir project (e.g. several thousand files) gives the most signal.

## Conventions

- Keep the CLI commands (`init`, `reindex`, `lookup`) working independently of the LSP server
- Parser tests should cover real-world Elixir patterns from large codebases
- Integration tests scaffold a fake Elixir project with `mix.exs` and verify end-to-end behavior
- Always use fake/generic module names in tests (e.g. `MyApp.Accounts`, `SharedLib.Worker`), never real module names from the user's codebase, even when writing regression tests for bugs found in real code
- Version strings (`Version` and `IndexVersion`) live in `internal/version/version.go`
