package mcp

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// compileFixture writes files into a new Mix project, compiles it, and runs
// expr in it. It skips when Elixir is not installed, or in -short mode.
func compileFixture(t *testing.T, files [][2]string, expr string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("compiles a Mix project")
	}
	mix, err := exec.LookPath("mix")
	if err != nil {
		t.Skip("mix is not on PATH")
	}
	dir := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mix.exs", `defmodule CompileCheck.MixProject do
  use Mix.Project
  def project, do: [app: :compile_check, version: "0.1.0", consolidate_protocols: false]
end
`)
	for _, f := range files {
		write(f[0], f[1])
	}
	cmd := exec.Command(mix, "run", "-e", expr)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "MIX_ENV=dev")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mix run: %v\n%s", err, out)
	}
	return string(out)
}

// The derives that implementations lists are the implementations the
// compiler builds for them: every module the compiled protocol has an
// implementation for, except the defimpl targets, is listed as derived.
func TestImplementationsTool_DeriveMatchesCompiler(t *testing.T) {
	compiled := compileFixture(t, deriveFixture, `
		path = :code.lib_dir(:compile_check) |> Path.join("ebin")
		IO.puts("IMPLS " <> Enum.map_join(Protocol.extract_impls(MyApp.Encoder, [path]), " ", &inspect/1))`)
	m := regexp.MustCompile(`IMPLS (.*)`).FindStringSubmatch(compiled)
	if m == nil {
		t.Fatalf("no IMPLS line in:\n%s", compiled)
	}
	var want []string
	for _, impl := range strings.Fields(m[1]) {
		if impl != "Any" && impl != "Integer" { // the defimpl targets
			want = append(want, impl)
		}
	}
	sort.Strings(want)
	if len(want) == 0 {
		t.Fatalf("the compiler reports no derived implementations:\n%s", compiled)
	}
	t.Logf("compiler: %v", want)

	e := setupTestEnv(t)
	e.indexFile("mix.exs", "defmodule MyApp.MixProject do\nend\n")
	for _, f := range deriveFixture {
		e.indexFile(f[0], f[1])
	}
	out := e.callTool("dexter_implementations", map[string]any{"module": "MyApp.Encoder"})
	// A derive that Dexter is sure of must be one the compiler built, and
	// every one the compiler built must be listed: sure, or marked unsure.
	var got, unsure []string
	for _, line := range regexp.MustCompile(`(?m)^  (MyApp\.\S+) - (.*)$`).FindAllStringSubmatch(out, -1) {
		if strings.Contains(line[2], "(unsure:") {
			unsure = append(unsure, line[1])
			continue
		}
		got = append(got, line[1])
	}
	var built []string
	for _, m := range want {
		if !slices.Contains(unsure, m) {
			built = append(built, m)
		}
	}
	want = built
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("derived implementations:\n dexter   %v\n compiler %v\n%s", got, want, out)
	}
}

// moduleAPIFunctions reads the public functions that a dexter_module_api
// answer lists, as name/arity, leaving out macros and callbacks.
func moduleAPIFunctions(out string) []string {
	var got []string
	section := ""
	entry := regexp.MustCompile(`^  ([a-z_][a-zA-Z0-9_?!]*)(?:\(([^)]*)\)|/(\d+))(?: \(macro\))? \[`)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, " ") && strings.HasSuffix(line, ":") {
			section = line
			continue
		}
		m := entry.FindStringSubmatch(line)
		if m == nil || strings.Contains(line, "(macro)") {
			continue
		}
		if section != "Functions:" && section != "Delegates:" && !strings.HasPrefix(section, "Injected or generated") {
			continue
		}
		arity := m[3]
		if arity == "" {
			arity = "0"
			if strings.TrimSpace(m[2]) != "" {
				arity = strconv.Itoa(strings.Count(m[2], ",") + 1)
			}
		}
		got = append(got, m[1]+"/"+arity)
	}
	sort.Strings(got)
	return got
}

// module_api, from source alone, lists the public functions that the compiled
// module exports. behaviour_info/1, which the compiler adds for @callback, is
// left out: only a compiled module has it.
func TestModuleAPITool_MatchesCompiledExports(t *testing.T) {
	modules := []string{"SharedLib.InnerWorker", "SharedLib.BaseWorker", "MyApp.Workers.Mailer", "SharedLib.Schema", "MyApp.Thing", "MyApp.Point"}
	compiled := compileFixture(t, useChainFixture, `
		for m <- [`+strings.Join(modules, ", ")+`] do
		  fs = for {f, a} <- m.__info__(:functions), f != :behaviour_info, do: "#{f}/#{a}"
		  IO.puts("EXPORTS #{inspect(m)} " <> Enum.join(Enum.sort(fs), " "))
		end`)
	want := map[string]string{}
	for _, m := range regexp.MustCompile(`EXPORTS (\S+) ?(.*)`).FindAllStringSubmatch(compiled, -1) {
		want[m[1]] = strings.TrimSpace(m[2])
	}
	if len(want) != len(modules) {
		t.Fatalf("compiled exports: %v\n%s", want, compiled)
	}

	e := setupUseChainProject(t)
	for _, module := range modules {
		out := e.callTool("dexter_module_api", map[string]any{"module": module})
		got := strings.Join(moduleAPIFunctions(out), " ")
		if got != want[module] {
			t.Errorf("%s:\n dexter   %q\n compiler %q\n%s", module, got, want[module], out)
		}
	}
}
