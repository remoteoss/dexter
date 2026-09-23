# Function impact selection

Dexter can report test files affected between two exact Git revisions. It only
selects and explains tests; the CI runner remains responsible for execution.

```sh
dexter impact \
  --base "$CI_MERGE_REQUEST_DIFF_BASE_SHA" \
  --head "$CI_COMMIT_SHA" \
  --base-snapshot base-impact.db \
  --selection-root apps/sample_app \
  --format json
```

The index root and selection root are separate. Dexter indexes the full
monorepo so a change under `libs/` can reach application callers, but only test
files under `apps/sample_app` are candidates. Candidate paths are relative to the
selection root.

## Snapshot artifacts

A successful main pipeline should publish an exact-revision snapshot:

```sh
dexter impact snapshot \
  --revision "$CI_COMMIT_SHA" \
  --output impact.db
```

Snapshots contain callable fingerprints, project files, call symbols, and call
edges. They omit navigation references and use project-relative paths. Every
snapshot records its exact commit and format version; Dexter rejects a snapshot
that does not match the requested revision.

Base and head graphs stay independent. Added functions seed the head graph,
removed functions seed the base graph, and modified functions seed both. Dexter
unions the selected files and removes base-only tests from the head inventory.

When no artifact is supplied, Dexter checks its local snapshot cache. For a
clean exact `HEAD`, the workspace daemon reconciles its mutation queue,
validates fingerprint and test-root coverage, and exports the existing normal
index without reparsing source. Other missing revisions use an isolated
detached worktree as a correctness fallback and write the compact impact schema
directly. They do not create a temporary navigation index. If the normal index
predates required impact evidence, Dexter also falls back to an isolated build.

## Cache cleanup

The default cache is `<user-cache>/dexter/impact`. Dexter touches snapshots on
use and prunes after each command:

- snapshots older than 30 days are removed;
- least-recently-used snapshots are removed above 1 GiB;
- snapshots from older format versions are removed;
- snapshots active in the current command are retained.

Use `--cache-dir` to choose another location. Full navigation indexes are never
stored in this cache.

## Conservative selection

Dexter widens selection when a changed function cannot be resolved, traversal
hits a budget, a changed file has no callable evidence, or a test file has no
source root. Unknown-arity calls are traversed explicitly and remain marked as
uncertain evidence. These conditions can add tests but must not remove an
affected test.

Traversal budgets default to `-1` (unlimited). Zero allows seed resolution but
no edge expansion; positive values set explicit depth or node limits.

## Parity roadmap

Source evidence is the first layer. Certification for a large application also requires
revision-matched compiled evidence for generated ExUnit functions, macros,
callbacks and behaviours, dynamic calls, compile-time resources, and dependency
packages. Compiled evidence must carry source, toolchain, dependency, and
configuration provenance before Dexter attaches it to a snapshot.

Project frameworks can contribute relationships through a generic evidence
artifact rather than Dexter hard-coding application hooks. Such evidence must name its
provider and exact revision, declare caller/callee identities or test ownership,
and preserve unresolved records. Dexter can then validate, persist, explain,
and invalidate hook-registry or runtime-only edges using the same rules as BEAM
evidence. Missing or stale provider evidence widens selection.

## Repository and provider evidence

A repository can add static framework relationships in `dexter-impact.json` at
the index root:

```json
{
  "schema_version": 1,
  "required_providers": ["compiled", "compiled_tests", "compiler_trace", "framework_hooks"],
  "compiled_build_roots": ["apps/sample_app/_build/test/lib"],
  "compiled_applications": {
    "sample_app": "apps/sample_app"
  },
  "behaviour_callbacks": [
    {"behaviour": "SharedLib.Worker", "function": "perform", "arity": 1}
  ],
  "mappings": [
    {
      "caller": {"module": "MyApp.Events", "function": "dispatch", "arity": 1},
      "callee": {"module": "SharedLib.Consumer", "function": "handle", "arity": 1},
      "kind": "hook"
    }
  ]
}
```

The file is revision-owned because Dexter reads it from each exact checkout.
Mappings are generic function relationships; Dexter does not interpret provider
or module names. Configured build roots are scanned natively through BEAM `Dbgi`
ETF data. Only modules declared by each application's `.app` file are included;
orphan BEAM files are ignored. `compiled_applications` maps build application
names to repository paths; dependencies without a mapping receive package-scoped
synthetic paths.

Dexter never invokes `mix`, `elixir`, or a project compiler. The repository must
finish ordinary and test compilation, with debug information retained, before
snapshot creation. Compiler tracers and framework providers are also owned by
the repository build. Missing configured BEAM roots, compiled test ownership,
or required provider artifacts widen selection; Dexter does not attempt to
repair them by compiling code itself.

The caller supplies the exact source revision associated with the completed
build. Dexter trusts that association, but hashes the declared BEAM inventory and
checks that no file changes while the snapshot is built. Missing, changing,
duplicate, or unreadable compiled evidence leaves the compiled requirements
unsatisfied.

Compiler traces remain a separate provider. Macro expansion, compile-time
configuration, and compiler resource reads are events that are not completely
recoverable from a finished BEAM. A repository can require that provider and
Dexter will widen selection if its exact-revision artifact is absent.

Generated evidence uses a strict JSON artifact with `schema_version`, `provider`,
and the exact `revision`. It can contain `functions`, `edges`, `test_ownership`,
and `unresolved` call sites. Supply artifacts when publishing a snapshot:

```sh
dexter impact snapshot \
  --revision "$CI_COMMIT_SHA" \
  --evidence compiled.json \
  --evidence framework-hooks.json \
  --output impact.db
```

For a comparison that must build a missing snapshot, use `--base-evidence` and
`--head-evidence`. Published snapshots already contain their evidence and do not
need the original JSON files. Dexter includes provider digests in cache identity,
rejects revision mismatches, and widens selection when a provider declared in
`required_providers` is absent. Unresolved provider callers remain explicit
uncertain graph roots rather than disappearing as missing edges.
