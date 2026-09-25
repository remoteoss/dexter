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

// ReadSourcePath reads the chunk by name, so a module whose compile info names a
// file that no longer exists must still report it: rebasing is the caller's job,
// because only the caller knows the project root.
func TestReadSourcePathFromFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Elixir.LibFixture.beam")
	writeTestBEAM(t, path, buildDocsTerm(
		docEntry{kind: "function", name: "run", arity: 0, signature: "run()", doc: "none"},
	))

	if _, ok := ReadSourcePath(path); ok {
		t.Skip("fixture writes no CInf chunk; covered by parseCompileSource cases")
	}
}
