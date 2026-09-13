package lsp

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/store"
)

// newTestServerForDir returns a server whose project root is dir, backed by a
// throwaway store.
func newTestServerForDir(t *testing.T, dir string) *Server {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewServer(s, dir)
}

// bumpDirMtime moves a directory's mtime forward. Filesystems vary in timestamp
// granularity, and a test that writes two files in the same tick would otherwise
// see no change.
func bumpDirMtime(t *testing.T, dir string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(dir, future, future); err != nil {
		t.Fatal(err)
	}
}

// Expectations verified against Macro.camelize/1 on Elixir 1.20, since a mismatch
// silently yields no completions rather than an error.
func TestCamelize(t *testing.T) {
	tests := map[string]string{
		"code_interface":      "CodeInterface",
		"attributes":          "Attributes",
		"uuid_v7_primary_key": "UuidV7PrimaryKey",
		"define":              "Define",
		"define_calculation":  "DefineCalculation",
		"_private":            "Private",
		"foo__bar":            "FooBar",
		"":                    "",
		"already":             "Already",
		"2fa":                 "2fa",
		"UPPER":               "UPPER",
		"a_b_c":               "ABC",
	}
	for input, want := range tests {
		if got := camelize(input); got != want {
			t.Errorf("camelize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestModulesUnder(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"Elixir.Foo.Dsl.beam",
		"Elixir.Foo.Dsl.Section.beam",
		"Elixir.Foo.Dsl.Section.Options.beam",
		"Elixir.Foo.Dsl.Section.Entity.beam",
		"Elixir.Foo.Dsl.SectionEntity.beam",
		"Elixir.Foo.Dsl.Other.beam",
		"Elixir.Foo.Unrelated.beam",
		"erlang_module.beam", // not an Elixir module
		"Elixir.Foo.Dsl.beam.bak",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("FOR1"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s := newTestServerForDir(t, dir)

	got := s.modulesUnder(dir, "Foo.Dsl.Section.")
	var names []string
	for _, provider := range got {
		names = append(names, provider.module)
	}
	// Exactly one further segment: Section.Options and Section.Entity are in
	// scope, SectionEntity is a different module, and anything under Section.*.*
	// belongs to a deeper block.
	want := []string{"Foo.Dsl.Section.Entity", "Foo.Dsl.Section.Options"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("modulesUnder = %v, want %v", names, want)
	}
	for _, provider := range got {
		if provider.beamPath != filepath.Join(dir, "Elixir."+provider.module+".beam") {
			t.Errorf("provider %s beam path = %s", provider.module, provider.beamPath)
		}
	}

	if found := s.modulesUnder(dir, "Foo.Dsl.Missing."); len(found) != 0 {
		t.Errorf("expected nothing under an absent prefix, got %v", found)
	}
	if found := s.modulesUnder(filepath.Join(dir, "nope"), "Foo."); len(found) != 0 {
		t.Errorf("expected nothing from an absent directory, got %v", found)
	}
}

// The listing is cached against the directory's mtime, so adding a module is
// picked up without a timer and an unchanged directory is not re-read.
func TestEbinIndexInvalidatesOnDirectoryChange(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("FOR1"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Elixir.Foo.Dsl.One.beam")

	s := newTestServerForDir(t, dir)
	if got := len(s.modulesUnder(dir, "Foo.Dsl.")); got != 1 {
		t.Fatalf("expected 1 module, got %d", got)
	}

	first := s.ebinIndexes.dirs[dir]
	if len(s.modulesUnder(dir, "Foo.Dsl.")) != 1 {
		t.Fatal("cached listing changed")
	}
	if s.ebinIndexes.dirs[dir] != first {
		t.Error("an unchanged directory must not be re-read")
	}

	write("Elixir.Foo.Dsl.Two.beam")
	bumpDirMtime(t, dir)
	if got := len(s.modulesUnder(dir, "Foo.Dsl.")); got != 2 {
		t.Errorf("expected the new module without waiting on a timer, got %d", got)
	}
}

func TestEbinIndexCacheIsBounded(t *testing.T) {
	cache := newEbinIndexCache()
	for i := range maxEbinIndexCacheEntries + 1 {
		dir := strconv.Itoa(i)
		cache.put(dir, &ebinModuleIndex{dir: dir})
	}

	cache.mu.RLock()
	defer cache.mu.RUnlock()
	if len(cache.dirs) != maxEbinIndexCacheEntries {
		t.Errorf("cache grew to %d entries, want %d", len(cache.dirs), maxEbinIndexCacheEntries)
	}
	if cache.dirs[strconv.Itoa(maxEbinIndexCacheEntries)] == nil {
		t.Error("the newly inserted entry was evicted")
	}
}

// Walking the syntax tree costs more than the rest of scope resolution, so a
// module with no DSL extensions must never trigger it. Nearly every module in a
// project is in that position.
func TestDslProvidersInScopeSkipsTreeWalkWithoutExtensions(t *testing.T) {
	server := newTestServerForDir(t, t.TempDir())

	walked := false
	providers := server.dslProvidersInScope("MyApp.PlainModule", func() []string {
		walked = true
		return []string{"defmodule", "code_interface"}
	})
	if len(providers) != 0 {
		t.Errorf("expected no providers, got %v", providers)
	}
	if walked {
		t.Error("the syntax tree must not be walked for a module with no DSL extensions")
	}
}

// Spark names the module that holds a block's macros after the block path, which
// language forms are not part of. A `for` or `if` inside a DSL section used to
// stay on every suffix of the path, match nothing, and drop the section's entity
// macros for the rest of its body.
func TestDslScopeIgnoresLanguageFormsInsideBlocks(t *testing.T) {
	server, cleanup := setupTestServer(t)
	defer cleanup()

	const extension = "Fake.Dsl"
	writeTestModuleBEAM(t, server, extension, "deps/fake/lib/fake/dsl.ex", beamExport{"code_interface", 1})

	// The entity and option modules are generated at compile time, so they have
	// no source; only their BEAMs exist.
	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	for _, module := range []string{
		"Fake.Dsl.CodeInterface",
		"Fake.Dsl.CodeInterface.Define",
		"Fake.Dsl.CodeInterface.Define.Options",
		"Fake.Dsl.CodeInterface.Options",
		"Fake.Dsl.Attributes",
		"Fake.Dsl.Attributes.Attribute",
		"Fake.Dsl.Attributes.Attribute.Options",
	} {
		path := filepath.Join(ebin, "Elixir."+module+".beam")
		if err := os.WriteFile(path, minimalBeam(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bumpDirMtime(t, ebin)

	tests := []struct {
		name      string
		blockPath []string
		want      []string
	}{
		{"section body", []string{"defmodule", "code_interface"},
			[]string{"Fake.Dsl.CodeInterface.Define", "Fake.Dsl.CodeInterface.Options"}},
		{"language form inside a section body", []string{"defmodule", "code_interface", "for"},
			[]string{"Fake.Dsl.CodeInterface.Define", "Fake.Dsl.CodeInterface.Options"}},
		{"language form inside an entity body", []string{"defmodule", "attributes", "attribute", "if"},
			[]string{"Fake.Dsl.Attributes.Attribute.Options"}},
		{"language form between sections", []string{"defmodule", "case", "attributes"},
			[]string{"Fake.Dsl.Attributes.Attribute"}},
		{"entity body", []string{"defmodule", "code_interface", "define"},
			[]string{"Fake.Dsl.CodeInterface.Define.Options"}},
		{"module level", []string{"defmodule"}, nil},
		{"no dsl blocks at all", []string{"defmodule", "def", "with"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var names []string
			for _, provider := range server.dslScopeModules([]string{extension}, tt.blockPath) {
				names = append(names, provider.module)
				if provider.beamPath != filepath.Join(ebin, "Elixir."+provider.module+".beam") {
					t.Errorf("provider %s has beam path %s", provider.module, provider.beamPath)
				}
			}
			if strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Errorf("dslScopeModules(%v) = %v, want %v", tt.blockPath, names, tt.want)
			}
		})
	}
}
