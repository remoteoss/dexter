# Dexter: Elixir code intelligence

Dexter indexes every module, function, and call site in this Elixir workspace
by parsing source directly (no compilation needed). Use these tools instead of
grep or reading whole files whenever you navigate or ask questions about
Elixir code: they resolve aliases, imports, defdelegate chains, use-chain
injection, and the Elixir stdlib, which text search cannot.

Which tool for which question:

- Locate a symbol by name fragment: `dexter_search`
- Where or what is Module.function: `dexter_definition`
- Understand a module before reading its source: `dexter_module_api`
- Who calls or uses something: `dexter_references` or `dexter_call_hierarchy`
- Implementations of a behaviour or protocol: `dexter_implementations`
- What a specific file defines: `dexter_file_outline`
- Project layout and index freshness: `dexter_workspace`

The index updates automatically: file changes are watched (fsnotify) and git
branch switches are detected. If a lookup ever seems stale, `dexter_reindex`
forces an immediate incremental update.

Elixir specifics: modules are not tied to files (use `dexter_file_outline` for
a file, `dexter_definition` for a module); pass fully-qualified module names,
not aliases; function names take no arity; functions defined inside a
`__using__` quote block may not be indexed, so an empty lookup can mean
macro-generated code.
