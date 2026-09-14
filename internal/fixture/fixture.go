// Package fixture locates the integration test fixture: one Mix umbrella whose
// child applications are the scenarios the LSP, parser and BEAM tests run
// against as compiled Elixir artifacts.
//
// Every scenario shares a single mix.lock and a single _build, so CI compiles
// once and caches it. A scenario's directory name is its Mix app name, which is
// what lets Dexter's build-root resolution map a source path onto that
// application's ebin directory. The server root for these scenarios is the
// umbrella root, as it is for a real umbrella or monorepo checkout.
package fixture

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// Scenario applications. The value is both the directory under apps/ and the Mix
// app name, so `_build/dev/lib/<app>/ebin` is where its modules compile to.
const (
	AppBasic         = "app_basic"
	AppStyler        = "app_with_styler"
	AppEctoMigration = "app_with_ecto_migration"
	AppAshDSL        = "dexter_ash_beam_fixture"
	AppPhoenixRoutes = "phoenix_routes"
	AppObanWorkers   = "oban_workers"
)

// compileCommand is repeated in the skip message, the CI failure and the fixture
// README so preparing a checkout never has to be guessed at.
const compileCommand = "mix deps.get && mix compile"

var resolveRoot = sync.OnceValues(func() (string, error) {
	if override := os.Getenv("DEXTER_INTEGRATION_FIXTURE"); override != "" {
		return filepath.Abs(override)
	}
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot locate the fixture: runtime.Caller failed")
	}
	return filepath.Abs(filepath.Join(filepath.Dir(self), "..", "lsp", "testdata", "integration"))
})

// Root returns the absolute path of the compiled umbrella fixture. It skips the
// test when the fixture has not been compiled, unless DEXTER_REQUIRE_FIXTURE is
// set — CI sets it, because a fixture that stops compiling must fail the build
// rather than quietly remove coverage.
//
// Compilation is deliberately not attempted here: packages run concurrently, so
// one of them starting a build would contend with another's for the same _build.
func Root(tb testing.TB) string {
	tb.Helper()
	root, err := resolveRoot()
	if err != nil {
		tb.Fatal(err)
	}
	lib := filepath.Join(root, "_build", "dev", "lib")
	if _, err := os.Stat(lib); err != nil {
		if os.Getenv("DEXTER_REQUIRE_FIXTURE") != "" {
			tb.Fatalf("the integration fixture is not compiled (want %s): run `cd %s && %s`", lib, root, compileCommand)
		}
		tb.Skipf("the integration fixture is not compiled: cd %s && %s", root, compileCommand)
	}
	return root
}

// App returns one scenario application's directory.
func App(tb testing.TB, app string) string {
	tb.Helper()
	return filepath.Join(Root(tb), "apps", app)
}

// Source returns a scenario file's path, relative to the application directory.
func Source(tb testing.TB, app, rel string) string {
	tb.Helper()
	return filepath.Join(App(tb, app), filepath.FromSlash(rel))
}

// Ebin returns the directory a scenario application's modules compile into.
func Ebin(tb testing.TB, app string) string {
	tb.Helper()
	return filepath.Join(Root(tb), "_build", "dev", "lib", app, "ebin")
}

// Beam returns one compiled module of a scenario application. A missing file is
// a broken fixture rather than a soft failure: the scenario exists to supply that
// module's compiled artifact.
func Beam(tb testing.TB, app, module string) string {
	tb.Helper()
	return requireBeam(tb, Ebin(tb, app), module)
}

// Deps returns the umbrella's shared dependency directory, which the parser
// benchmarks use as a corpus of real Elixir source.
func Deps(tb testing.TB) string {
	tb.Helper()
	return filepath.Join(Root(tb), "deps")
}

// DepBeam returns one compiled module of a dependency, such as the DSL extension
// modules Ash and Spark generate their macros from.
func DepBeam(tb testing.TB, dep, module string) string {
	tb.Helper()
	ebin := filepath.Join(Root(tb), "_build", "dev", "lib", dep, "ebin")
	return requireBeam(tb, ebin, module)
}

// DepSource returns a dependency's source file, relative to that dependency.
func DepSource(tb testing.TB, dep, rel string) string {
	tb.Helper()
	return filepath.Join(Deps(tb), dep, filepath.FromSlash(rel))
}

func requireBeam(tb testing.TB, ebin, module string) string {
	tb.Helper()
	path := filepath.Join(ebin, "Elixir."+module+".beam")
	if _, err := os.Stat(path); err != nil {
		tb.Fatalf("compiled module %s is missing from %s: %v", module, ebin, err)
	}
	return path
}
