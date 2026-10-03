package lsp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.lsp.dev/uri"
)

// The fixture compiles DSLs of every shape navigation has to handle, with the
// real compiler, so a change in what Elixir records fails here. It needs mix,
// so it runs in the integration job.

const compiledDslSource = `defmodule Weird.Dsl do
  defmacro outer(name), do: quote(do: Weird.Dsl.inner(unquote(name)))
  defmacro inner(name), do: quote(do: def(unquote(name)(), do: :inner))

  defmacro flag(name) do
    fun = :"#{name}?"
    quote do: def(unquote(fun)(map), do: Map.get(map, unquote(name), false))
  end

  defmacro later(name), do: quote(do: @later_names(unquote(name)))

  defmacro __before_compile__(env) do
    for name <- Module.get_attribute(env.module, :later_names) do
      quote do: def(unquote(name)(), do: :later)
    end
  end

  defmacro __using__(_) do
    quote do
      import Weird.Dsl
      Module.register_attribute(__MODULE__, :later_names, accumulate: true)
      @before_compile Weird.Dsl
    end
  end

  defmacro pinned(name), do: quote(line: 99, do: def(unquote(name)(), do: :pinned))

  defmacro route(verb, path) do
    quote do: def(match(unquote(verb), unquote(path)), do: {unquote(verb), unquote(path)})
  end

  defmacro plug(_name), do: nil
  defmacro action(_name), do: nil

  defmacro helper(name) do
    quote bind_quoted: [name: name] do
      defmodule Module.concat(__MODULE__, Macro.camelize(Atom.to_string(name))) do
        def run, do: :helper
      end
    end
  end

  # The module name is computed here, so the compiler records no line for
  # the module itself.
  defmacro named_helper(name) do
    module = Module.concat(__CALLER__.module, Macro.camelize(Atom.to_string(name)))

    quote do
      defmodule unquote(module) do
        def run, do: :named
      end
    end
  end

  defmacro keep_route(name, verb) do
    quote bind_quoted: [name: name, verb: verb], location: :keep do
      def unquote(name)(unquote(verb)), do: unquote(verb)
    end
  end

  defmacro with_default(name), do: quote(do: def(unquote(name)(value \\ 1), do: value))

  defmacro elsewhere(name) do
    quote bind_quoted: [name: name] do
      Module.create(
        Module.concat(Weird.Elsewhere, Macro.camelize(Atom.to_string(name))),
        quote(do: def(run, do: :elsewhere)),
        __ENV__
      )
    end
  end

  defmacro delegate(name, to), do: quote(do: defdelegate(unquote(name)(value), to: unquote(to)))

  defmacro many(prefix, count) do
    for i <- 1..count do
      fun = :"#{prefix}_#{i}"
      quote do: def(unquote(fun)(), do: unquote(i))
    end
  end

  defmacro kept_field(name) do
    quote bind_quoted: [name: name], location: :keep do
      def unquote(name)(), do: unquote(name)
    end
  end

  defmacro stamped_field(name) do
    line = __CALLER__.line

    quote bind_quoted: [name: name, line: line] do
      @file {__ENV__.file, line}
      def unquote(name)(), do: unquote(name)
    end
  end
end
`

const compiledUserSource = `defmodule Weird.User do
  use Weird.Dsl

  outer :two_level

  flag :active

  later :deferred

  action :submit
  later :submit

  pinned :pinned_fun

  plug :match

  route :get, "/a"
  route :post, "/b"

  keep_route :handle, :get
  keep_route :handle, :post

  with_default :defaulted

  elsewhere :remote

  delegate :double, Weird.Target

  many :gen, 2000

  kept_field :kept

  stamped_field :stamped

  helper :audit

  named_helper :report

  for name <- [:loop_a, :loop_b] do
    def unquote(name)(), do: unquote(name)
  end

  defmodule Nested do
    use Weird.Dsl
    flag :nested
  end
end
`

const compiledCallerSource = `defmodule Weird.Caller do
  import Weird.User, only: [active?: 1]

  def run do
    Weird.User.two_level()
    Weird.User.active?(%{})
    Weird.User.deferred()
    Weird.User.submit()
    Weird.User.pinned_fun()
    Weird.User.match(:get, "/a")
    Weird.User.handle(:get)
    Weird.User.defaulted(2)
    Weird.Elsewhere.Remote.run()
    Weird.User.double(2)
    Weird.User.gen_1500()
    Weird.User.kept()
    Weird.User.stamped()
    Weird.User.loop_b()
    Weird.User.Audit.run()
    Weird.User.Report.run()
    Weird.User.Nested.nested?(%{})
    active?(%{})
    f = &Weird.User.two_level/0
    f.()
  end
end
`

const (
	compiledDslRel    = "lib/weird/dsl.ex"
	compiledUserRel   = "lib/weird/user.ex"
	compiledCallerRel = "lib/weird/caller.ex"
)

// compiledFixture is the compiled DSL project and the server that indexed it.
type compiledFixture struct {
	server *Server
	t      *testing.T
}

func newCompiledFixture(t *testing.T, elixircOptions string) *compiledFixture {
	t.Helper()
	if _, err := exec.LookPath("mix"); err != nil {
		t.Skip("mix not available")
	}
	server, cleanup := setupTestServer(t)
	t.Cleanup(cleanup)

	mixExs := `defmodule Weird.MixProject do
  use Mix.Project

  def project, do: [app: :weird, version: "0.1.0", elixir: "~> 1.18", elixirc_options: [` + elixircOptions + `]]
end
`
	if err := os.WriteFile(filepath.Join(server.projectRoot, "mix.exs"), []byte(mixExs), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, source := range map[string]string{
		compiledDslRel:    compiledDslSource,
		compiledUserRel:   compiledUserSource,
		compiledCallerRel: compiledCallerSource,
		"lib/weird/target.ex": `defmodule Weird.Target do
  def double(value), do: value * 2
end
`,
	} {
		indexFile(t, server.store, server.projectRoot, rel, source)
	}
	cmd := exec.Command("mix", "compile")
	cmd.Dir = server.projectRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile the DSL fixture: %v\n%s", err, output)
	}
	return &compiledFixture{server: server, t: t}
}

// saveLater saves and indexes an edit made after the compile.
func (f *compiledFixture) saveLater(rel, source string) {
	f.t.Helper()
	indexFile(f.t, f.server.store, f.server.projectRoot, rel, source)
}

// lineOf returns the 1-based line of the nth occurrence of needle in source.
func lineOf(t *testing.T, source, needle string, nth int) int {
	t.Helper()
	for i, line := range strings.Split(source, "\n") {
		if strings.Contains(line, needle) {
			if nth--; nth == 0 {
				return i + 1
			}
		}
	}
	t.Fatalf("%q not found in source", needle)
	return 0
}

// definitionOf returns the definition lines for the nth occurrence of needle in
// the caller, with the cursor inside the name the needle starts with.
func (f *compiledFixture) definitionOf(needle string, nth int) []string {
	f.t.Helper()
	line := lineOf(f.t, compiledCallerSource, needle, nth)
	text := strings.Split(compiledCallerSource, "\n")[line-1]
	col := strings.Index(text, needle) + 1
	locations := generatedDefinitionAt(f.t, f.server, compiledCallerRel, compiledCallerSource, line-1, col)
	var got []string
	for _, location := range locations {
		rel, _ := filepath.Rel(f.server.projectRoot, uriToPath(location.URI))
		got = append(got, rel+":"+strconv.Itoa(int(location.Range.Start.Line)+1))
	}
	return got
}

func at(rel, source, needle string, t *testing.T) string {
	t.Helper()
	return rel + ":" + strconv.Itoa(lineOf(t, source, needle, 1))
}

func expectDefinition(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("definition = %v, want %v", got, want)
	}
}

func TestDefinition_GeneratedFunctionsFromCompiler(t *testing.T) {
	f := newCompiledFixture(t, "")
	user := func(needle string) string { return at(compiledUserRel, compiledUserSource, needle, t) }

	for _, tc := range []struct {
		name, needle string
		nth          int
		want         []string
	}{
		{"macro that calls a macro", "two_level(", 1, []string{user("outer :two_level")}},
		{"predicate from an atom", "active?(%{})", 1, []string{user("flag :active")}},
		{"@before_compile hook", "deferred(", 1, []string{user("later :deferred")}},
		{"declaring call next to a same-named call", "submit(", 1, []string{user("later :submit")}},
		{"line past the end of the file", "pinned_fun(", 1, []string{user("pinned :pinned_fun")}},
		{"one clause per call", "match(", 1, []string{user(`route :get`), user(`route :post`)}},
		{"one location: :keep clause per call", "handle(", 1, []string{user("keep_route :handle, :get"), user("keep_route :handle, :post")}},
		{"default arguments", "defaulted(", 1, []string{user("with_default :defaulted")}},
		{"module from Module.create", "run()", 1, []string{user("elsewhere :remote")}},
		{"generated defdelegate", "double(", 1, []string{user("delegate :double")}},
		{"one of many generated", "gen_1500(", 1, []string{user("many :gen")}},
		{"location: :keep", "kept(", 1, []string{user("kept_field :kept")}},
		{"@file stamp", "stamped(", 1, []string{user("stamped_field :stamped")}},
		{"comprehension", "loop_b(", 1, []string{user("def unquote(name)()")}},
		{"nested module", "nested?(", 1, []string{user("flag :nested")}},
		{"function of a module a macro nested", "run()", 2, []string{user("helper :audit")}},
		{"name of a module a macro nested", "Audit.run", 1, []string{user("helper :audit")}},
		{"function of a module a macro named", "run()", 3, []string{user("named_helper :report")}},
		{"name of a module a macro named", "Report.run", 1, []string{user("named_helper :report")}},
		{"bare call through import", "active?(%{})", 2, []string{user("flag :active")}},
		{"capture", "two_level/0", 1, []string{user("outer :two_level")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectDefinition(t, f.definitionOf(tc.needle, tc.nth), tc.want...)
		})
	}

	t.Run("module that exists only as a BEAM", func(t *testing.T) {
		locations, err := f.server.LookupName("Weird.Elsewhere.Remote", "", NameLookupOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(f.server.projectRoot, compiledUserRel)
		if len(locations) != 1 || locations[0].FilePath != want || locations[0].Line != lineOf(t, compiledUserSource, "elsewhere :remote", 1) {
			t.Errorf("module location = %#v, want the elsewhere call in %s", locations, want)
		}
	})
}

// Dexter cannot compile, so the declaring file can have edits the BEAM has not
// seen. The line from the last compile is still the answer: it is near, and
// the next compile makes it exact. A line the file no longer has is never
// returned, and the search for a declaring call reads the current text.
func TestDefinition_GeneratedFunctionsFromCompilerWithStaleSource(t *testing.T) {
	f := newCompiledFixture(t, "")
	compiled := func(needle string) string { return at(compiledUserRel, compiledUserSource, needle, t) }

	t.Run("lines added above, saved", func(t *testing.T) {
		edited := "# one\n# two\n# three\n" + compiledUserSource
		f.saveLater(compiledUserRel, edited)
		t.Cleanup(func() { indexFile(t, f.server.store, f.server.projectRoot, compiledUserRel, compiledUserSource) })
		expectDefinition(t, f.definitionOf("two_level(", 1), compiled("outer :two_level"))
		expectDefinition(t, f.definitionOf("match(", 1), compiled("route :get"), compiled("route :post"))
	})

	// The index then has the module below the recorded lines. The module
	// line they are compared with comes from the same compile as they do,
	// also for a module a macro nested in it.
	t.Run("many lines added above, saved", func(t *testing.T) {
		edited := strings.Repeat("# header\n", 40) + compiledUserSource
		f.saveLater(compiledUserRel, edited)
		t.Cleanup(func() { indexFile(t, f.server.store, f.server.projectRoot, compiledUserRel, compiledUserSource) })
		expectDefinition(t, f.definitionOf("two_level(", 1), compiled("outer :two_level"))
		expectDefinition(t, f.definitionOf("run()", 2), compiled("helper :audit"))
	})

	t.Run("lines added in an unsaved buffer", func(t *testing.T) {
		edited := strings.Replace(compiledUserSource, "  outer :two_level\n", "  # one\n  # two\n  outer :two_level\n", 1)
		userURI := string(uri.File(filepath.Join(f.server.projectRoot, compiledUserRel)))
		f.server.docs.Set(userURI, edited)
		t.Cleanup(func() { f.server.docs.Close(userURI) })
		expectDefinition(t, f.definitionOf("two_level(", 1), compiled("outer :two_level"))
		// The BEAM records only the module line here, so the declaring call
		// is searched for, in the text as it is now.
		expectDefinition(t, f.definitionOf("deferred(", 1), at(compiledUserRel, edited, "later :deferred", t))
	})

	t.Run("file shorter than the recorded line", func(t *testing.T) {
		edited := "defmodule Weird.User do\n  use Weird.Dsl\n  plug :match\n  outer :two_level\nend\n"
		f.saveLater(compiledUserRel, edited)
		t.Cleanup(func() { indexFile(t, f.server.store, f.server.projectRoot, compiledUserRel, compiledUserSource) })
		expectDefinition(t, f.definitionOf("gen_1500(", 1), compiledUserRel+":1")
		// The route clauses are gone; `plug :match` spells the name but is not
		// where a clause was declared.
		expectDefinition(t, f.definitionOf("match(", 1), compiledUserRel+":1")
		expectDefinition(t, f.definitionOf("two_level(", 1), compiled("outer :two_level"))
	})
}

// A project compiled without debug info still has its Docs and compile info
// chunks, and its source.
func TestDefinition_GeneratedFunctionsFromCompilerWithoutDebugInfo(t *testing.T) {
	f := newCompiledFixture(t, "debug_info: false")
	user := func(needle string) string { return at(compiledUserRel, compiledUserSource, needle, t) }
	expectDefinition(t, f.definitionOf("two_level(", 1), user("outer :two_level"))
	expectDefinition(t, f.definitionOf("deferred(", 1), user("later :deferred"))
}
