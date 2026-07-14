# Dexter: Elixir code intelligence

Dexter maintains a SQLite index of every module, function, and call site in
this Elixir workspace, built by parsing source directly (no compilation
required, so it works even when the project doesn't compile). Prefer these
tools over grep for navigating Elixir code: they resolve aliases,
`defdelegate` chains, `use`-chain injected imports, and the Elixir stdlib.

## Reading code

1. `dexter_workspace`: orient yourself with the project layout, index size,
   and stdlib status.
2. `dexter_search`: fuzzy-find symbols when you don't know the defining module.
3. `dexter_definition` / `dexter_module_api`: inspect a symbol or a whole
   module's API (docs, specs, signatures).
4. `dexter_references` / `dexter_call_hierarchy`: find callers and callees.
5. `dexter_implementations`: behaviour implementors or protocol `defimpl`s.
6. `dexter_file_outline`: what a specific file defines.

## Editing code

- `dexter_rename_symbol` computes a workspace-wide rename and returns a
  unified diff plus any file renames. **It writes nothing.** Apply the diff,
  perform the listed file renames (module renames move files that follow the
  naming convention), then call `dexter_reindex`.
- After you create, edit, or delete Elixir files by any means, call
  `dexter_reindex` (fast, incremental) before trusting references, renames, or
  call hierarchies again. Git branch switches are picked up automatically.

## Elixir specifics worth knowing

- **Modules are not files.** One file can define many modules and a module can
  live anywhere. Use `dexter_file_outline` for a file's contents and
  `dexter_definition` for a module's location.
- **Fully-qualified names only.** Pass `MyApp.Accounts`, not an alias like
  `Accounts`. Function names are given without arity.
- **`defdelegate` is followed.** `dexter_definition` shows both the facade and
  the real implementation; `dexter_references` includes calls to facades.
- **`use` injects code.** Functions imported via a module's `__using__` macro
  are attributed to the injecting module; `dexter_references` includes those
  call sites. Functions *defined inside* a `quote do` block may not be in the
  index. If a lookup comes up empty, check the module's `__using__` macro.
- **Behaviours vs protocols.** `@behaviour`/`@callback` and
  `defprotocol`/`defimpl` are different mechanisms; `dexter_implementations`
  handles both and tells you which it found.
- The index covers `deps/` and the Elixir stdlib for definitions, so you can
  jump into library code too.
