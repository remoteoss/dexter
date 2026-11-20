# Ash generated-function fixture

This minimal Ash/Spark app exercises public functions that an
`Ash.Resource` code interface generates, and the DSL macros that Spark
extensions supply. Compile it with the toolchain in the repository's
`.tool-versions`:

```sh
cd internal/lsp/testdata/ash_generated_functions
mise exec -- mix deps.get
mise exec -- mix compile
```

The tests that use it skip until it is compiled:
`TestCompletion_AshGeneratedFunctionsIntegration` in `server_test.go`, and the
tests that call `ashFixture` in `dsl_macro_test.go`. To use a compiled copy
somewhere else, set `DEXTER_ASH_FIXTURE` to its directory. Run them from the
repository root:

```sh
mise exec -- go test ./internal/lsp -run 'AshGeneratedFunctionsIntegration|SparkDSLMacro|NestedDSLMacroScope|DslScopeResolvesInnermostBlock|HoverGeneratedFunctionDocs'
```
