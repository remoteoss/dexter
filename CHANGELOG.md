# Changelog

## [0.7.2] - 2026-09-11

### Changed

- **The index carries each file path once** — definitions and refs repeated the file's absolute path on every row: 122 characters across 3.9 million refs on a large monorepo, plus a second copy inside the path index, which together were more than half the database. Both tables now hold an integer `file_id` into `files`. `idx_refs_function_kind` went with it — no query leads with `function`, so SQLite never chose it, and it cost 80 MB and a share of every rebuild. This is what the index version bump pays for: an index built by an earlier version is rebuilt once, on first start

- **`init` and the server share one cold-build pipeline** — both built an index from nothing and each had its own implementation. `init` parsed on every core and wrote a single bulk transaction with the indexes dropped; the server walked serially, parsed one file at a time and committed a transaction per file against live indexes. On a 10,000-file corpus that was 4.441s against the shared pipeline's 1.088s, for identical rows. The server now reaches the same pipeline, under the empty-index check where its own build already lived

- **Less work for the single SQLite writer** — a cold index is bound by the one writer rather than by parsing: on a 70,000-file monorepo the parse workers use about 12s of CPU across 15 cores while the writer needs 4.6s, so the pipeline ran at the speed of one core. Refs are now deduped in the parse workers, which have the CPU to spare — a `@spec` repeating `String.t()` wrote one identical row per occurrence, 59,792 of them on that monorepo, and every one was invisible to every query. Bulk inserts are batched into multi-row statements, and the two prefix lookups use a range rather than a `LIKE` that SQLite could not turn into an index range

### Added

- **Built-in MCP server** - `dexter mcp` serves the index to AI agents over the Model Context Protocol (stdio, or streamable HTTP with `--listen`), modeled on `gopls mcp`. Ten tools cover workspace overview, fuzzy symbol search, definitions with docs and specs, references (including use-chain injected call sites), module API summaries, file outlines, behaviour/protocol implementations, call hierarchy, incremental reindexing, and workspace-wide rename with the same on-disk semantics as the editor rename. Both headless and attached LSP+MCP modes watch the project tree (fsnotify) so the index stays fresh when agent edits bypass editor events. A running LSP can expose the same tools from its live session via `dexter lsp --mcp-listen=ADDR`, and `dexter mcp --instructions` prints an agent-facing usage guide. The headless server negotiates its workspace root per session through MCP roots, resolved the way the LSP resolves its own (existing `.dexter` index, then `.git`), with one workspace per resolved root in `--listen` mode; an explicit path argument overrides negotiation

### Fixed

- **A `\` line continuation inside a heredoc cut the file short** — a backslash at the end of a line inside a `"""` heredoc drops the newline from the content, but the line it ends is still a line, and a closing `"""` on the next one still closes the heredoc. The scanner consumed the escaped newline and carried on without testing that next line for the delimiter, so the heredoc never closed and the rest of the file was scanned as string content: every definition and reference below it was silently missing, the same failure as the `#{flag?}` and comment-in-interpolation bugs with a third trigger. Interpolation is not needed to hit it — a plain wrapped log message is enough. On a 70,000-file monorepo this recovered 1,409 definitions across 36 files, one of them losing 31 of its 33 functions; on a 4,000-file project, 402

- **A module rename deleted the module when its file path did not change** — the conventional path is the module's last segment, snake_cased, inside the directory the file already sits in, and `camelToSnake` folds an acronym into its capitalised form: `ABTest` and `AbTest` are both `ab_test`, as are `HTTPClient` and `HttpClient`. Normalising that casing is an ordinary rename, and it computed a "new" path equal to the old one, wrote the file there and then removed the old path — removing the file it had just written. The module was gone, with only the reply's edits to the *other* files left pointing at it. A file whose conventional path is unchanged is now left where it is and has its contents rewritten in place, and no rename operation is sent to the client for a path that is not moving

- **A comment inside `#{}` cut the file short** — a comment written in a multi-line interpolation runs to the end of its line, so a `}` or a `"` in its text is inert. The tokenizer read them as real delimiters, closed the interpolation at the brace and the string at the quote, and then scanned the rest of the file as string content: every definition and reference below was lost, the same silent failure as the `#{flag?}` bug. Found while building deep-nesting regression tests, and older than the interpolation work above

- **Code inside `#{}` was invisible to the index** — a string literal was read as one opaque token, so every module reference and every call written inside an interpolation was missing. `"<#{Config.admin_url(slug)}|#{slug}>"` gave no go-to-definition, no hover and no find-references, and — worse — a module or function rename rewrote every ordinary call site while leaving the interpolated ones pointing at a name that no longer existed. The tokenizer now records the code inside `#{}` in a stream of its own, kept beside the file's tokens rather than mixed into them, so string literals still count as one token for `@doc` extraction and for block-depth tracking, and a `do`/`end`/`fn` inside an interpolation cannot be miscounted. Strings, charlists, heredocs and interpolating (lowercase) sigils such as `~s` and `~r` are covered, nested interpolation included (`"outer #{"inner #{x}"}"`); an uppercase sigil stays raw text, as Elixir reads it. Parsing the whole of a 70,000-file monorepo costs about 2% more for this. A rename also reaches an interpolated call written through an alias injected by `use`, where the short name has no alias line of its own in the file

- **Everything after `#{flag?}` fell out of the index** — a predicate name inside a string interpolation ends in `?`, which also opens a char literal in Elixir (`?a`). The tokenizer read the `?` of `IO.puts("...(DRY RUN: #{dry_run?})...")` the second way, so it swallowed the interpolation's closing brace and scanned the whole rest of the file as string content: every definition and reference below that line was silently missing from the index. One real file lost 150 lines' worth, and a module rename left its `Repo.insert!` calls behind because nothing knew they existed. A `?` straight after a name now ends that name, and a char literal in operand position still tokenizes as one
- **Rename missed call sites reached through an alias injected by `use`** — a module whose `__using__` block declares `alias MyApp.Repo` puts that short name in the scope of every module that uses it, so those files write `Repo.insert(...)` with no alias line of their own. The index holds such a site under the bare `Repo`, which a lookup for `MyApp.Repo` cannot see, and renaming the module rewrote the `use` lines while leaving every one of those calls pointing at a module that no longer existed. On a 1,500-file project the rename went from 694 sites to 1,368, with nothing but comments left mentioning the old name. The injecting module is found from the renamed module's own references — the `alias` line inside a `__using__` body is itself an indexed reference — so a rename that injects nothing pays one small query, and go-to-references now finds these call sites for a function too. Aliases injected through an `ExUnit.CaseTemplate` (`using do` rather than `defmacro __using__`) are included, which is how test files reach theirs. `use` calls are paired with their exact modules, alias state, token order and lexical branches, including blocks that open or close later on the same line
- **Renaming a `with` clause's argument renamed the wrong binding** — a clause's expression is evaluated before that clause's own pattern binds, so the `site` in `{:ok, site} <- authorize(site)` names what the *previous* clause bound. Renaming it reached forward into the second pattern, the later `^site` pin and the `do` block, which all belong to the binding being introduced on that same line. The occurrences of a clause's binding now stop at the next pattern that binds the name anew; a pinned pattern (`{:ok, ^site} <-`) binds nothing, so it and the body still follow the earlier binding, and a pin in the first clause names the outer binding as it should
- **Positions were wrong on any line containing a non-ASCII character** — LSP counts `Position.character` in UTF-16 code units, and Dexter counted UTF-8 bytes. The two agree only while a line stays ASCII, so one accented character, bullet or emoji to the left of the cursor was enough to break go-to-definition, hover, references, completion and signature help. Worse, ranges were *emitted* in bytes too, so an editor applying a rename replaced a span shifted off the identifier and silently corrupted the source. Columns are now converted in both directions, on every client
- **Module rename left the old file behind** — renaming a module from the file that defines it moved that file on disk while the editor still held the buffer, so the next save recreated the old file with the new module name and the project no longer compiled (`cannot define module X because it is currently being defined in ...`). Open files are now moved by the editor, through a `rename` resource operation in the reply, and the server touches neither path
- **Typespec mentions counted as calls** — a module reference written in a typespec, such as `Ecto.Schema.schema()` in `@spec put_meta(Ecto.Schema.schema(), meta)`, names a type but was indexed as a call. It therefore turned up in find-references for the macro of that name, and go-to-definition and hover on it described the macro. Typespec references now carry their own kind: a lookup for a function leaves them out, and a lookup for a type keeps only those. Inside a typespec, definition and hover prefer the type; outside one they prefer the function. Typespec context ends at the declaration's real statement boundary instead of leaking into later expressions. Same-file bare type uses (`@type t :: schema | embedded`) are reported alongside the indexed references, and a type parameter (the `t` in `@type wrapper(t) :: t`) still resolves to its occurrences
- **A type won over a same-named function** — in a module that declares both, such as `Ecto.Schema` with `@type schema` above `defmacro schema/2`, go-to-definition and hover on a bare call landed on the type, because the buffer scan returned whichever came first in the file. Functions now win outside a typespec, types win inside one (`@type`, `@spec`, `@callback` and their continuation lines), and either kind is still found when only it exists
- **Private namesakes offered as definition targets** — go-to-definition on a call reaching another module (an explicit remote call, or a bare call imported through `use`) offered the module's private helper of the same name next to the public one. `Ecto.Schema` exports `defmacro field/3` and keeps a `defp field/4`, so `field :email, :string` in a schema gave two targets. Private definitions are now dropped when the call site sits outside the defining module and a public namesake exists; a module's own private functions, private helpers injected by a `__using__` block, and modules with no public namesake are unaffected
- **Aliases built on other aliases** — `alias SharedLib.Accounts` followed by `alias Accounts.Users`, `alias Accounts.{Tokens, Roles}`, or `require Accounts.Macros, as: M` kept the short prefix (`Accounts.Users`) instead of resolving to the canonical module, so definition, hover and references missed the target. The leading segment of an alias/require line is now expanded through the aliases already in scope, in both the index parser and the open-document parser
- **Phoenix's `use MyAppWeb, :controller` resolved nothing** — a `use` that dispatches on an atom reaches its aliases and imports through a `__using__` that branches on that atom and splices in a quoted helper, so everything a controller, view or LiveView inherits that way was invisible: no completion, no go-to-definition, no hover and no find-references for any of it. The dispatch atom and its options are now carried through nested `use` chains, quoted local helpers such as `unquote(html_helpers())` are followed, and quoted and tuple dispatch atoms are both handled. Dispatch parsing is scoped per module, so each module in a multi-module file resolves its own

- **`require Mod, as: Name` inside `__using__`** — an alias injected by a `require ... , as:` in a `quote do` block (directly, or through a delegated `using_block/1` helper) was dropped, so modules that `use` the injector could not resolve the short name
- **Grouped aliases were not renamed** — `alias Old.{A, B}` (and the `require`/`import` forms) kept pointing at the old module after a module rename, and in open buffers the shared prefix was rewritten once per member (`Old` → `NewNewNew...`)

- **The formatter's BEAM server warned on every start** — a binary pattern that uses an already-bound variable as its size must pin it (`binary-size(^size)`). Without the pin, Elixir 1.19 and later print a deprecation warning through the server's stderr capture each time it starts, and the message says it is to become an error. Both patterns in `beam_server.exs` are pinned

## [0.7.1] - 2026-06-12

### Added

- **Structural completion snippets** — completions now include snippets for common Elixir forms such as `do`, `defmodule`, `def`, `defp`, `defmacro`, `defstruct`, `defprotocol`, `defimpl`, `defdelegate`, guards, `if`, `case`, `with`, `try`, and ExUnit-style macros; snippet-aware clients also avoid duplicate special-form completions when the same names are imported through `use`. This is an improvement and also fixes some VS Code usability issues do to how VS Code handles completions. Thanks @superhawk610.

### Fixed

- **Index WAL growing unchecked** — SQLite WAL files are capped and checkpointed after reindexing, preventing `.dexter/dexter.db-wal` from growing endlessly. Thanks @flowerett.
- **Top-level script variables** — variables in `.exs` files such as `config/runtime.exs` can now be renamed and highlighted without leaking into nested module or function scopes
- **Document symbols** — local assignments and ordinary function calls no longer appear in the outline, while split-line ExUnit-style macro heads are still shown correctly. Thanks @cjbottaro.
- **Alias-aware import/use/require references** — short aliases in `import`, `use`, and `require` statements now resolve to the full module for references and rename edits
- **Formatter improvements and fixes** — the persistent formatter server now starts Mix's supervisor tree so plugins can call Mix APIs like `Mix.Project.config/0`, and Dexter passes all `.formatter.exs` options through to formatter plugins so plugin-specific settings such as HEEX options are honored. Thanks @superhawk610 and @ogomezba.

## [0.7.0] - 2026-05-28

### Added

- **Erlang go-to-definition and hover docs** — `textDocument/definition` and `textDocument/hover` now resolve Erlang modules and functions (e.g. `:lists.map/2`), jumping to the `.erl` source and rendering EDoc-style documentation alongside the existing Elixir support (#50)
- **Claude Code plugin and marketplace** — the repo is now a Claude Code marketplace, so Dexter can be installed as an Elixir LSP with `claude plugin marketplace add remoteoss/dexter` followed by `claude plugin install dexter-lsp@dexter`; the plugin wires up `dexter lsp` for `.ex`, `.exs`, and `.heex` files and exposes `followDelegates` and `debug` as configurable options (#59)
- **Lazy document loading for headless LSP clients** — the document store now falls back to reading files from disk for clients that don't drive the full `didOpen`/`didChange`/`didClose` lifecycle (e.g. Claude Code), so definitions, hovers, references, completions, and the other read-only handlers all work without an editor buffer; disk-loaded entries are tracked in an LRU cache (default cap 50, configurable via the new `maxTransientDocuments` init option) and never evict editor-owned buffers (#62)
- **Bracket-pair folding** — `textDocument/foldingRange` now emits ranges for multi-line maps/structs (`{...}`), lists (`[...]`), parenthesized expressions (`(...)`), and binaries (`<<...>>`), in addition to the existing `do/fn..end` and heredoc support; brackets inside strings, comments, sigils, char literals, and atoms are skipped automatically (#56)

### Changed

- **Single consolidated BEAM server** — the per-`.formatter.exs` formatter process was reworked into a hardened persistent BEAM server (`beam_server.exs`) with synchronized stderr capture, automatic restart when `.formatter.exs` changes outside `DidSave`, and invalidation on formatter watcher events (#50, #55)
- **Cached tokenized documents** — tokenized Elixir documents are now reused across LSP handlers instead of being re-tokenized for completions, alias/import/use resolution, buffer-function lookups, and alias-block checks; completion context detection is token-aware so completions correctly ignore strings, comments, and heredocs while still handling Erlang atom modules and dotted prefixes (#50)
- **CI lints the entire codebase** — linting now runs over the whole codebase on every pipeline rather than only the diffed files, so latent issues are caught going forward (#64)

### Fixed

- **Sigil formatting** — the persistent formatter now wires plugin-advertised sigils into `Code.format_string!/2`, so `~H` and other plugin sigils (e.g. `Phoenix.LiveView.HTMLFormatter`) are formatted inside `.ex`/`.exs` files; whole-file plugin formatting stays scoped to plugins that match the file extension (#55)
- **Umbrella formatter config resolution** — child apps without their own `.formatter.exs` now search up to the LSP project root and use the umbrella root config (#50, closes #35, #34)
- **Alias merging for unopened files** — alias merging previously returned `nil` for any file the editor hadn't explicitly opened, breaking `lookupThroughUse`/`lookupThroughUseOf` for headless clients; these now resolve via the lazy document store (#62)

## [0.6.0] - 2026-04-22

### Added

- **Tokenizer-based parser** — replaced the regex-over-joined-lines parser with a single-pass tokenizer and token walker; geomean throughput on real `.ex` files improved from 84 MB/s to 231 MB/s (#22). But most importantly, this change fixes a ton of edge-case bugs in parsing due to improved consistency.
- **Homebrew tap support** — Dexter can now be installed via `brew install remoteoss/tap/dexter`; the release workflow automatically updates the formula on each tagged release (#14)

### Changed

- **Index database moved to `.dexter/` folder** — the SQLite index now lives at `<project>/.dexter/dexter.db` instead of `<project>/.dexter.db`; existing databases are migrated automatically on next startup (legacy files are deleted and a fresh index is built); update your `.gitignore` to include `.dexter/` instead of `.dexter.db` (#46)
- **Removed periodic reindexing** — eliminated the 30-second full-project reindex that caused CPU spikes (~25%) and unnecessary file I/O; the index now stays current via editor file-watcher events and startup scans (#16)

### Fixed

The tokenizer change in #22 fixed a ton of edge-case bugs. Here are some of them:

- **Heredoc comment misparse** — `#` inside heredoc markdown links was misread as a comment, which cascaded into line merges that swallowed entire `defmacro __using__` bodies and broke use-chain resolution in some modules (#22)
- **Multi-line bracket expressions** — missed references and incorrect line numbers when expressions spanned multiple lines (#22)
- **`require` alias registration** — `require Module, as: Name` now registers aliases for go-to-definition (#22)
- **Multi-hop `defdelegate` chains** — chains like A → B → C now resolve to the final target instead of stopping at the intermediate delegate (recursive up to depth 5) (#22)
- **Multi-line alias blocks** — `alias Parent.{ Child, Other }` blocks now resolve correctly in go-to-definition, hover, references, and completion (#22)
- **Multi-line `use` opts** — `use Module, opts` spanning multiple lines now parses the opts correctly (#22)

## [0.5.3] - 2026-04-09

### Added

- **Open-source release** — Dexter is now available under the MIT license on GitHub, with updated install scripts for mise and asdf

### Fixed

- **Aliases injected via `use`** — `use` macros that inject `alias` declarations (e.g. `alias MyApp.Repo` inside `__using__`) now propagate those aliases to the consumer module, so go-to-definition, hover, completions, references, rename, signature help, and all other LSP features correctly resolve the aliased modules; also follows transitive `use` chains and helper `quote do` blocks
- **Module depth tracking** — module nesting depth is now tracked by counting `do..end` and `fn..end` blocks instead of relying on indentation, fixing incorrect module scope attribution when anonymous functions or other nested blocks were present; heredoc content and string literals are now properly excluded from block detection
- **Formatter `_build` lookup in umbrella apps** — the persistent formatter process now walks up from the app's mix root to the umbrella root to find `_build` and `deps`, so formatter plugins and `import_deps` resolve correctly in umbrella projects
- **Folding ranges** — folding range detection now strips strings and comments before checking for block boundaries, preventing false ranges from string content like `"foo do"`

## [0.5.2] - 2026-04-07

### Added

- **Completion snippets with parameter names** — function completions now insert tab-stop snippets with real parameter names extracted from the definition during indexing (e.g. `fun(user, opts)` instead of `fun(arg1, arg2)`); falls back to positional names for complex patterns like destructured maps

### Fixed

- **Completion with pipes** — pipe context (`|>`) detection omits the first argument from the snippet since it is already supplied by the pipe
- **Elixir/OTP version mismatch** — stdlib resolution now calls `mise where elixir` / `asdf where elixir` with the project root, so Dexter picks up the version pinned in `.tool-versions` / `mise.toml` rather than the globally latest install; eliminates the "requires a more recent Erlang/OTP" crash when a project pins an Elixir build that targets a specific OTP; a one-time editor error notification is shown if the mismatch is still detected at runtime

## [0.5.1] - 2026-04-06

### Fixed

- **Formatter `import_deps` resolution** — the persistent formatter now resolves `import_deps` from `.formatter.exs`, reading each dependency's exported `locals_without_parens` so formatting matches `mix format` output
- **Formatter binary protocol corruption** — the Erlang IO server's default Unicode encoding was expanding bytes > 127 in the binary protocol framing to multi-byte UTF-8, silently corrupting formatted file content on the first format request if the server wasn't yet ready; fixed by forcing raw byte mode on stdin/stdout

## [0.5.0] - 2026-04-06

### Added

- **Go-to-references** — `textDocument/references` returns all usages of a module or function across the project, including calls through `__using__`/`import` chains and bare intra-module calls; also finds all bindings and uses of a local variable within its scope
- **Rename** — `textDocument/rename` + `textDocument/prepareRename` rename modules, functions, and variables project-wide; a module rename also renames all submodules (if needed), updates every alias/call/import/use site, and moves the file to its conventional path
- **Near-instant format on save** — `textDocument/formatting` formats the current file on save using the nearest `.formatter.exs`, with full support for formatter plugins like [Styler](https://github.com/remoteoss/elixir-styler); format errors are shown as LSP diagnostics so they appear inline. A persistent BEAM process is kept alive per `.formatter.exs`, eliminating VM startup cost so formatting is near-instant; falls back to `mix format` if the persistent process is unavailable
- **Full workspace symbol search** — `workspace/symbol` fuzzy-searches all indexed symbols by name across the whole project (Cmd-T in VS Code)
- **Go-to-declaration** — `textDocument/declaration` jumps to the `@callback` (or `@macrocallback`) definition for any `@impl`-annotated function; walks `@behaviour` declarations and `use`-chains (including dynamic `use unquote(mod)` patterns resolved via keyword opts) to find the right behaviour module, with a global index fallback for `@impl true`
- **Go-to-implementation** — `textDocument/implementation` jumps from a `@callback` definition to every module that implements it via `@behaviour` or `use`
- **Document symbols** — `textDocument/documentSymbol` returns a fully hierarchical outline of modules, submodules, functions, macros, types, structs, and protocols in the current file
- **Signature help** — `textDocument/signatureHelp` shows function parameter hints (triggered on `(` and `,`), including which argument is active and parameter names extracted from the definition
- **Type definition** — `textDocument/typeDefinition` jumps to the `@type` / `@opaque` declaration for the type under the cursor
- **Folding ranges** — `textDocument/foldingRange` reports foldable regions for `do...end` blocks and heredocs
- **Call hierarchy** — `textDocument/prepareCallHierarchy`, `callHierarchy/incomingCalls`, and `callHierarchy/outgoingCalls` show callers and callees of any function
- **Code actions** — `textDocument/codeAction` offers an "Add alias" quick fix for any unaliased module reference; searches the index when the full module name isn't used, and suggests up to five candidates
- **Document highlight** — `textDocument/documentHighlight` highlights all occurrences of the symbol under the cursor; uses scope-aware tree-sitter variable tracking for local variables, and falls back to token matching for module/function names

### Changed

- **Arity-aware completions** — function completions now emit one entry per callable arity (accounting for default parameters too), so `fun/2` and `fun/3` appear as distinct items
- **Cold indexing performance** — initial indexing is significantly more optimized; `dexter init --profile` added for detailed profiling during startup
- **Go-to-definition and references via use-chain with opts** — dynamic `import unquote(mod)` expressions inside `__using__/1` blocks are now resolved using the keyword opts passed at the `use` call site (e.g. `use MyLib, mod: Mox`)

### Fixed

- **Go-to-definition for nested modules** — a `defmodule Inner do` inside `defmodule Outer do` now creates an implicit alias `Inner → Outer.Inner` within `Outer`'s scope, so qualified calls like `Inner.fun()` resolve correctly
- **Incomplete submodule completions** — submodule segments were missing from completions on large codebases because the raw module row cap was hit before client-side deduplication into immediate segments; the query now uses `SELECT DISTINCT` on segments so the cap applies after dedup
- **Function lookup ordering** — when a name is shared by both a function and a type, the function definition is now returned first

## [0.4.0] - 2026-04-01

### Added

- **Hover documentation** — hovering over a module or function now displays its `@moduledoc`/`@doc` content, rendered as Markdown
- **Autocompletion** — module and function completions with documentation shown inline, including support for already-aliased modules and local functions
- **Elixir standard library support** — hover docs and completions now cover Elixir stdlib modules (e.g. `Enum`, `Map`, `String`) and `@typedoc` content
- **`use` macro support** — go-to-definition and hover now work on `use` statements, including complex multi-part module names like `Remote.Oban.Pro.Worker`
- **`__MODULE__` support** — go-to-definition and hover resolve `__MODULE__` references correctly
- **Zed editor support** — added configuration instructions for Zed

### Changed

- **File watching** — the LSP server now watches for external file changes (e.g. branch switches, `git pull`) and automatically refreshes the index, beyond the existing Git HEAD polling
- **Full reindex on version bumps** — when a new version of Dexter requires schema changes, the index is rebuilt entirely on startup instead of attempting an incremental update
- **CI pipeline** — added GitHub Actions CI with linting and tests

## [0.3.0] - 2026-03-31

### Changed

- CLI commands (`init`, `reindex`, `lookup`, `lsp`) are now implemented with [cobra](https://github.com/spf13/cobra), improving help output and flag handling
- Version string moved to `internal/version/version.go` as a single source of truth; `make release VERSION=x.y.z` now updates that file instead of `server.go`

## [0.1.4] - 2026-03-30

### Fixed

- `defdelegate` lookahead now stops at new statement boundaries, preventing `as:` from a nearby defdelegate being incorrectly captured for an unrelated one
- `defmacro` and other definitions after deeply nested modules are now correctly attributed to the outer module — depth tracking via `do...end` block counting replaces the naive bare-`end` pop heuristic
- Relative nested `defmodule` names (e.g. `defmodule PayslipDownloadResponse do` inside `defmodule MyAppWeb.ApiDocs.Payslips do`) are now indexed as the fully-qualified `MyAppWeb.ApiDocs.Payslips.PayslipDownloadResponse`

## [0.1.3] - 2026-03-30

### Fixed

- `dexter init` now defaults to current working directory when no path is given
- `dexter init --force` no longer misinterprets `--force` as a path argument
- `dexter init --force` now removes stale WAL files (`.dexter.db-shm`, `.dexter.db-wal`) that could corrupt the new database

## [0.1.2] - 2026-03-30

### Fixed

- Resolve `__MODULE__` aliases in the LSP buffer context — `alias __MODULE__.Schemas.UserRelationship` now correctly resolves when jumping to definition from an open buffer
- Partial alias resolution in the LSP Definition handler — `Services.AssociateWithTeamV2` now resolves through a `Services` alias to the full module name
- Relative nested `defmodule` — `defmodule PayslipDownloadResponse do` inside `defmodule MyAppWeb.ApiDocs.Payslips do` is now indexed as `MyAppWeb.ApiDocs.Payslips.PayslipDownloadResponse`

### Added

- Go-to-definition for module attributes — pressing the binding on `@endpoint_scopes` jumps to its definition in the current buffer. Reserved Elixir attributes (`@doc`, `@spec`, `@behaviour`, `@callback`, `@impl`, `@derive`, etc.) are excluded.

## [0.1.1] - 2026-03-30

### Fixed

- Resolve `__MODULE__` in alias declarations (e.g. `alias __MODULE__.Services`) so `defdelegate` targets that reference these aliases are correctly followed
- Resolve `__MODULE__` directly in `defdelegate to:` fields (e.g. `to: __MODULE__`)
- Resolve `alias __MODULE__, as: Name` so aliased self-references work in delegate chains
- Resolve partial aliases in `defdelegate to:` (e.g. `to: Services.Foo` where `Services` is an aliased module)

### Changed

- LSP server now auto-builds the index on first startup if no `.dexter.db` exists — no need to run `dexter init` manually
- Project root detection now prefers `.git` over `mix.exs` to correctly locate monorepo roots
- `dexter.followDelegates` LSP initialization option (default: `true`) allows clients to opt out of delegate following

## [0.1.0] - 2026-03-30

Initial release.

- SQLite-backed index of Elixir module and function definitions
- Parallel file parsing using all CPU cores
- Incremental reindex via file mtime tracking
- LSP server (`dexter lsp`) with `textDocument/definition` support
- Alias resolution: `alias A.B.C`, `alias A.B.C, as: D`, `alias A.B.{C, D}`
- Import resolution for bare function calls
- `defdelegate` following with `as:` rename support
- Support for `def`, `defp`, `defmacro`, `defmacrop`, `defguard`, `defguardp`, `defdelegate`, `defprotocol`, `defimpl`, `defstruct`, `defexception`
- Heredoc-aware parsing (skips code examples in `@moduledoc`/`@doc`)
- Module nesting via `end` tracking
- Git HEAD polling for automatic reindex on branch switches
- mise plugin for installation
- CLI commands: `init`, `init --force`, `reindex`, `lookup`, `lookup --strict`, `lookup --no-follow-delegates`
