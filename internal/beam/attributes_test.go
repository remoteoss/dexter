package beam

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadModuleAttributes(t *testing.T) {
	beamPath := filepath.Join(t.TempDir(), "Elixir.Example.beam")
	writeTestBEAMOpts(t, beamPath, testBEAMOptions{
		atomNames: defaultTestAtoms,
		exports:   defaultTestExports,
		docs:      buildDocsTerm(),
		attrs: buildAttrTerm(
			attrFixture{name: "extensions", atoms: []string{"Elixir.Ash.Domain.Dsl", "Elixir.Other.Dsl"}},
			attrFixture{name: "behaviour", atoms: []string{"Elixir.Ash.Domain"}},
			attrFixture{name: "locale", binaries: []string{"en"}},
			// A compound value must be skipped without desynchronizing the walk,
			// so the attribute that follows it still parses.
			attrFixture{name: "validate_sections", nested: 2, atoms: []string{"Elixir.Later.Dsl"}},
			attrFixture{name: "only_nested", nested: 1},
		),
	})

	got, err := ReadModuleAttributes(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"extensions": {"Elixir.Ash.Domain.Dsl", "Elixir.Other.Dsl"},
		"behaviour":  {"Elixir.Ash.Domain"},
		"locale":     {"en"},
		// The nested tuples are dropped, but the atom after them survives.
		"validate_sections": {"Elixir.Later.Dsl"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attributes mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
	if _, ok := got["only_nested"]; ok {
		t.Error("an attribute with no scalar values should be omitted")
	}
}

func TestReadModuleAttributesMalformed(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"missing version byte", []byte{tagList, 0, 0, 0, 0}},
		{"not a list", []byte{etfVersion, tagMap, 0, 0, 0, 0}},
		{"list count exceeds buffer", []byte{etfVersion, tagList, 0, 0, 0, 50, tagSmallTuple, 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseAttributes(tt.buf); err == nil {
				t.Error("expected malformed attributes to fail")
			}
		})
	}
}

// An entry that is not the {name, values} pair shape must be skipped without
// desynchronizing the walk, so the valid entries around it still parse. Failing
// the whole chunk over one unexpected attribute would cost every other one.
func TestReadModuleAttributesSkipsUnexpectedEntry(t *testing.T) {
	var w etfTestWriter
	w.version()
	w.listHeader(3)
	w.smallTuple(2)
	w.atom("before")
	w.listHeader(1)
	w.atom("Elixir.First.Dsl")
	w.nil()
	w.smallTuple(3) // wrong arity
	w.atom("bogus")
	w.nil()
	w.nil()
	w.smallTuple(2)
	w.atom("after")
	w.listHeader(1)
	w.atom("Elixir.Second.Dsl")
	w.nil()
	w.nil()

	got, err := parseAttributes(w.buf)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"before": {"Elixir.First.Dsl"},
		"after":  {"Elixir.Second.Dsl"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestStripElixirPrefix(t *testing.T) {
	tests := map[string]string{
		"Elixir.Ash.Domain.Dsl": "Ash.Domain.Dsl",
		"Elixir.Ash":            "Ash",
		"Elixir.":               "Elixir.", // too short to be a module name
		"lists":                 "lists",   // Erlang modules carry no prefix
		"":                      "",
	}
	for input, want := range tests {
		if got := StripElixirPrefix(input); got != want {
			t.Errorf("StripElixirPrefix(%q) = %q, want %q", input, got, want)
		}
	}
}

// attrFixture describes one attribute in a synthetic Attr chunk.
type attrFixture struct {
	name     string
	atoms    []string
	binaries []string
	// nested emits this many tuples before the scalar values, exercising the
	// skip path that compound attribute values take.
	nested int
}

// buildAttrTerm encodes the Attr chunk shape: a list of {name, [values]} pairs.
func buildAttrTerm(fixtures ...attrFixture) []byte {
	var w etfTestWriter
	w.version()
	w.listHeader(len(fixtures))
	for _, fixture := range fixtures {
		w.smallTuple(2)
		w.atom(fixture.name)
		total := fixture.nested + len(fixture.atoms) + len(fixture.binaries)
		w.listHeader(total)
		for range fixture.nested {
			w.smallTuple(1)
			w.nil()
		}
		for _, value := range fixture.atoms {
			w.atom(value)
		}
		for _, value := range fixture.binaries {
			w.binary(value)
		}
		w.nil()
	}
	w.nil()
	return w.buf
}

func TestReadModuleAttributesFromAshFixture(t *testing.T) {
	path := os.Getenv("DEXTER_ASH_BEAM")
	if path == "" {
		t.Skip("set DEXTER_ASH_BEAM to run against a compiled Ash resource")
	}
	attrs, err := ReadModuleAttributes(path)
	if err != nil {
		t.Fatal(err)
	}
	// Spark persists the modules that supply a DSL's macros. Ash resources get
	// theirs from Ash.Resource.Dsl, which is where attributes/actions/code_interface
	// come from — none of which exist as a defmacro in any source file.
	if extensions := attrs["extensions"]; len(extensions) == 0 {
		t.Fatalf("expected Spark to persist extensions, got %#v", attrs)
	} else {
		var sawResourceDsl bool
		for _, extension := range extensions {
			if StripElixirPrefix(extension) == "Ash.Resource.Dsl" {
				sawResourceDsl = true
			}
		}
		if !sawResourceDsl {
			t.Errorf("expected Ash.Resource.Dsl among %v", extensions)
		}
	}
}
