# Integration fixture

One Mix umbrella holding every integration scenario, so CI compiles once and
caches it. Each scenario is a child application; each child's directory name is
its Mix `:app` name, which is what makes Dexter's build-root resolution map
`apps/<app>/…` onto `_build/dev/lib/<app>/ebin`.

## Preparing it

```sh
cd internal/lsp/testdata/integration
mix deps.get && mix compile
```

A cold compile takes about 30 seconds. Tests reach the fixture through
`internal/fixture`, which **skips** when `_build` is missing. CI sets
`DEXTER_REQUIRE_FIXTURE=1`, which turns that skip into a failure: a fixture that
stops compiling has to break the build instead of quietly removing coverage.

## Scenarios

| App | Exercises |
|---|---|
| `app_basic` | Default formatter output, no dependencies |
| `app_with_styler` | A formatting plugin loaded from a child app's `.formatter.exs`, pinned by git ref |
| `app_with_ecto_migration` | `import_deps: [:ecto_sql]` so migration DSL calls keep no parens |
| `dexter_ash_beam_fixture` | Ash/Spark DSL section, entity and option macros, generated code-interface functions, and Ash's own DSL extension modules in `deps/ash` |
| `phoenix_routes` | Router helpers Phoenix generates at compile time, reached through `alias …, as: Routes` |
| `oban_workers` | Functions `Oban.Worker.__using__` injects next to the worker's own `perform/1` |

The umbrella root is also the correct server root for these tests, exactly as a
real umbrella or monorepo checkout would be opened: the index and the build-root
search both start above `apps/`.

## Adding a scenario

1. Create `apps/<name>` with `<name>` as its `:app`, and a `.formatter.exs`.
2. Its dependencies go in its own `mix.exs`, resolved into the single shared
   `mix.lock` at this directory's root. A git dependency must be pinned by `ref:`
   — one lock for every scenario means an unpinned fork drifts to its default
   branch head on the next refresh.
3. Add a constant to `internal/fixture` and index the scenario's files from the
   test with `fixture.Source` / `fixture.DepSource`.

Rules that keep the fixture working:

- **Nothing deliberately broken may live under `apps/*/lib`.** One file that fails
  to compile stops the whole umbrella, which blocks every scenario. Malformed
  Elixir belongs in a test's synthetic builder (`minimalBeam`, `buildDocsTerm`)
  or in a `t.TempDir()` project.
- Dependency *versions* are shared. A scenario cannot pin an older Ecto or Ash
  than another needs; that combination belongs in a separate project, not here.
  Older BEAM **formats** are covered by static fixtures in the Go tests, which is
  where they belong: they need no toolchain at all.
- Bumping a dependency moves every scenario at once, so do it in its own commit
  and re-read the DSL and generated-function assertions afterwards.

CI runs this fixture twice: pinned to the supported toolchain on every pull
request, and weekly against `elixir:latest` (see
`.github/workflows/integration.yml`) so a format change in a new OTP is caught on
a schedule rather than by a user report.
