package beam

import (
	"path/filepath"
	"testing"
)

// compileInfoTerm builds a CInf payload: a keyword list shaped like the one the
// compiler writes, with the source entry encoded in the requested form.
func compileInfoTerm(t *testing.T, source func(*etfTestWriter, string)) []byte {
	t.Helper()
	// parseCompileSource takes the chunk payload, which begins with the ETF
	// version byte, exactly as writeTestBEAM lays it out.
	var w etfTestWriter
	w.version()

	w.listHeader(2)

	w.smallTuple(2)
	w.atom("version")
	w.string("10.0.4")

	w.smallTuple(2)
	w.atom("source")
	source(&w, "/build/agent/deps/shared_lib/lib/shared_lib/worker.ex")

	w.nil()
	return w.buf
}

func TestParseCompileSource(t *testing.T) {
	cases := []struct {
		name   string
		source func(*etfTestWriter, string)
	}{
		{
			// What the compiler writes: a printable charlist stored as a string.
			name: "string term",
			source: func(w *etfTestWriter, path string) {
				w.string(path)
			},
		},
		{
			name: "binary term",
			source: func(w *etfTestWriter, path string) {
				w.binary(path)
			},
		},
		{
			name: "charlist term",
			source: func(w *etfTestWriter, path string) {
				w.listHeader(len(path))
				for i := 0; i < len(path); i++ {
					w.smallInt(int(path[i]))
				}
				w.nil()
			},
		},
	}

	const want = "/build/agent/deps/shared_lib/lib/shared_lib/worker.ex"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseCompileSource(compileInfoTerm(t, tc.source))
			if !ok {
				t.Fatalf("parseCompileSource reported no source")
			}
			if got != want {
				t.Errorf("source = %q, want %q", got, want)
			}
		})
	}
}

func TestParseCompileSourceMissing(t *testing.T) {
	// A compile info list without a source entry, and payloads that are not a
	// list at all, must all report "no source" rather than inventing one.
	var noSource etfTestWriter
	noSource.smallTuple(0)

	var notAList etfTestWriter
	notAList.smallInt(1)

	cases := map[string][]byte{
		"empty":      nil,
		"no entry":   noSource.buf,
		"not a list": notAList.buf,
	}
	for name, raw := range cases {
		if got, ok := parseCompileSource(raw); ok {
			t.Errorf("%s: got %q, want no source", name, got)
		}
	}
}

func TestReadSourcePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Elixir.LibFixture.beam")
	writeTestBEAMOpts(t, path, testBEAMOptions{
		atomNames: defaultTestAtoms,
		cinf: compileInfoTerm(t, func(w *etfTestWriter, path string) {
			w.string(path)
		}),
	})

	got, ok := ReadSourcePath(path)
	if !ok || got != "/build/agent/deps/shared_lib/lib/shared_lib/worker.ex" {
		t.Errorf("ReadSourcePath = %q, %v; want the recorded source", got, ok)
	}

	missing := filepath.Join(t.TempDir(), "Elixir.NoCompileInfo.beam")
	writeTestBEAM(t, missing, buildDocsTerm())
	if got, ok := ReadSourcePath(missing); ok {
		t.Errorf("ReadSourcePath without CInf = %q, want no source", got)
	}
}

// Erlang records :source as codepoints. A path with `é` is stored as a string
// term whose byte is 0xE9, and one with `日本` as a list, because 26085 does not
// fit in a byte. Both must come back as UTF-8.
func TestParseCompileSourceUnicode(t *testing.T) {
	const want = "/home/zoë/日本/lib/app.ex"
	codepoints := []rune(want)
	list := compileInfoTerm(t, func(w *etfTestWriter, _ string) {
		w.listHeader(len(codepoints))
		for _, c := range codepoints {
			w.smallInt(int(c))
		}
		w.nil()
	})
	if got, ok := parseCompileSource(list); !ok || got != want {
		t.Errorf("list: source = %q, %v; want %q", got, ok, want)
	}

	const latin = "/home/zoë/lib/app.ex"
	short := compileInfoTerm(t, func(w *etfTestWriter, _ string) {
		var bytes []byte
		for _, c := range latin {
			bytes = append(bytes, byte(c))
		}
		w.string(string(bytes))
	})
	if got, ok := parseCompileSource(short); !ok || got != latin {
		t.Errorf("string term: source = %q, %v; want %q", got, ok, latin)
	}
}
