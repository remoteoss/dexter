package beam

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestReadDocumentedFunctions(t *testing.T) {
	beamPath := filepath.Join(t.TempDir(), "Elixir.Example.beam")
	writeTestBEAM(t, beamPath, buildDocsTerm(
		docEntry{
			kind:      "function",
			name:      "create",
			arity:     3,
			signature: `create(email, params \\ nil, opts \\ nil)`,
			doc:       "Creates a record.",
			defaults:  2,
		},
		docEntry{
			kind:      "function",
			name:      "written_by_hand",
			arity:     1,
			signature: "written_by_hand(value)",
			doc:       "Not generated.",
		},
	))

	got, err := ReadDocumentedFunctions(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []Function{
		{Name: "create", Arity: 1, Params: "email", Kind: "def"},
		{Name: "create", Arity: 2, Params: "email,params", Kind: "def"},
		{Name: "create", Arity: 3, Params: "email,params,opts", Kind: "def"},
		{Name: "written_by_hand", Arity: 1, Params: "value", Kind: "def"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d functions, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Arity != want[i].Arity ||
			got[i].Params != want[i].Params || got[i].Kind != want[i].Kind || got[i].Hidden {
			t.Fatalf("function %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestReadDocumentedFunctionsHiddenAndMacros(t *testing.T) {
	beamPath := filepath.Join(t.TempDir(), "Elixir.Example.beam")
	writeTestBEAM(t, beamPath, buildDocsTerm(
		docEntry{kind: "function", name: "__struct__", arity: 1, signature: "__struct__(kv)", doc: "hidden"},
		docEntry{kind: "macro", name: "build", arity: 1, signature: "build(opts)", doc: "Builds it."},
		docEntry{kind: "type", name: "t", arity: 0, signature: "t()", doc: "none"},
		docEntry{kind: "callback", name: "handle", arity: 1, signature: "handle(x)", doc: "none"},
	))

	got, err := ReadDocumentedFunctions(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected the hidden function and the macro only, got %v", got)
	}
	if got[0].Name != "__struct__" || !got[0].Hidden {
		t.Errorf("expected __struct__/1 flagged hidden, got %#v", got[0])
	}
	if got[1].Name != "build" || got[1].Kind != "defmacro" || got[1].Hidden {
		t.Errorf("expected a public defmacro, got %#v", got[1])
	}
}

// Documentation prose must be recorded as a position in the inflated chunk, not
// copied into the result. Hover reads it back later by re-inflating, which is
// what makes full docs support possible without paying for it during completion.
func TestReadDocumentedFunctionsRecordsDocSpan(t *testing.T) {
	const prose = "Creates a record with the given email."
	beamPath := filepath.Join(t.TempDir(), "Elixir.Example.beam")
	writeTestBEAM(t, beamPath, buildDocsTerm(
		docEntry{kind: "function", name: "create", arity: 1, signature: "create(email)", doc: prose},
	))

	got, err := ReadDocumentedFunctions(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected one function, got %v", got)
	}
	if got[0].DocLen != len(prose) {
		t.Fatalf("DocLen = %d, want %d", got[0].DocLen, len(prose))
	}

	// Re-inflate independently and slice the recorded span, exactly as a hover
	// handler would.
	chunk, err := readChunk(beamPath, "Docs")
	if err != nil {
		t.Fatal(err)
	}
	inflated, err := inflateDocsTerm(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if string(inflated[got[0].DocOffset:got[0].DocOffset+got[0].DocLen]) != prose {
		t.Errorf("doc span = %q, want %q",
			inflated[got[0].DocOffset:got[0].DocOffset+got[0].DocLen], prose)
	}
}

// A corrupt term must come back as an error rather than a panic or a hang, so a
// damaged local build artifact degrades to the export table instead of taking
// down the language server.
func TestParseDocsMalformed(t *testing.T) {
	tests := []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"truncated tuple", []byte{tagSmallTuple, 7, tagSmallAtomUTF8, 8}},
		{"map with unhashable key shape", []byte{tagMap, 0, 0, 0, 1, tagNil, tagSmallInteger, 1}},
		{"count beyond buffer", []byte{tagList, 0, 0, 0, 100}},
		{"unknown tag", []byte{200}},
		{"atom claiming more bytes than exist", []byte{tagAtomUTF8, 0, 200, 'h', 'i'}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseDocs(tt.buf); err == nil {
				t.Error("expected an error for a malformed Docs term")
			}
		})
	}
}

// Deeply nested terms must be rejected rather than followed into a stack
// overflow, which a corrupt length field could otherwise cause.
func TestParseDocsRejectsDeepNesting(t *testing.T) {
	var w etfTestWriter
	w.smallTuple(7)
	w.atom("docs_v1")
	// The anno field is skipped, so nesting it past the limit exercises the
	// depth guard inside skip rather than the header checks.
	for range maxETFDepth + 10 {
		w.smallTuple(1)
	}
	w.nil()
	if _, err := parseDocs(w.buf); err == nil {
		t.Error("expected deeply nested input to be rejected")
	}
}

// Every strict prefix of a well-formed term must fail. This catches an off-by-one
// in the length accounting that would otherwise let a corrupt chunk be read as
// though it ended early, silently dropping functions.
func TestParseDocsRejectsEveryTruncation(t *testing.T) {
	full := buildDocsTerm(
		docEntry{
			kind: "function", name: "create", arity: 3,
			signature: `create(email, params \\ nil, opts \\ nil)`,
			doc:       "Creates a record.", defaults: 2,
		},
		docEntry{kind: "macro", name: "build", arity: 1, signature: "build(opts)", doc: "hidden"},
	)
	if _, err := parseDocs(full); err != nil {
		t.Fatalf("the untruncated term must parse: %v", err)
	}
	for i := range len(full) {
		if _, err := parseDocs(full[:i]); err == nil {
			t.Fatalf("parseDocs accepted a truncation at %d of %d bytes", i, len(full))
		}
	}
}

func TestReadExports(t *testing.T) {
	beamPath := filepath.Join(t.TempDir(), "Elixir.Example.beam")
	writeTestBEAM(t, beamPath, buildDocsTerm())

	got, err := ReadExports(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []Function{
		{Name: "create", Arity: 3, Params: "arg1,arg2,arg3", Kind: "def"},
		{Name: "written_by_hand", Arity: 1, Params: "arg1", Kind: "def"},
		{Name: "build", Arity: 1, Params: "arg1", Kind: "defmacro"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("export %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

// OTP 28 widened atom names past 255 bytes and re-encoded the AtU8 chunk: the
// count is stored negated and every length becomes a tagged varint instead of a
// raw byte. The chunk kept its name, so parsers must branch on the sign bit.
func TestReadExportsLongAtomTable(t *testing.T) {
	beamPath := filepath.Join(t.TempDir(), "Elixir.Example.beam")
	writeTestBEAMOpts(t, beamPath, testBEAMOptions{
		longAtoms: true,
		atomNames: []string{
			"Elixir.Example",
			"create",
			// 24 bytes: exceeds the 4-bit inline form, needs the 11-bit form.
			"a_rather_long_fn_name_xx",
			"module_info",
			"MACRO-build",
		},
		exports: defaultTestExports,
		docs:    buildDocsTerm(),
	})

	got, err := ReadExports(beamPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []Function{
		{Name: "create", Arity: 3, Params: "arg1,arg2,arg3", Kind: "def"},
		{Name: "a_rather_long_fn_name_xx", Arity: 1, Params: "arg1", Kind: "def"},
		{Name: "build", Arity: 1, Params: "arg1", Kind: "defmacro"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("export %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestReadDocumentedFunctionsFromAshFixture(t *testing.T) {
	path := os.Getenv("DEXTER_ASH_BEAM")
	if path == "" {
		t.Skip("set DEXTER_ASH_BEAM to run against a compiled Ash resource")
	}
	functions, err := ReadDocumentedFunctions(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []Function{
		{Name: "create", Arity: 1, Params: "email", Kind: "def"},
		{Name: "create!", Arity: 4, Params: "email,name,params,opts", Kind: "def"},
		{Name: "get_by_id", Arity: 1, Params: "id", Kind: "def"},
		{Name: "list!", Arity: 0, Params: "", Kind: "def"},
	} {
		if !containsGeneratedFunction(functions, wanted) {
			t.Errorf("missing Ash generated function %#v", wanted)
		}
	}
	// Internal entries must be flagged so callers can drop them.
	var sawHidden bool
	for _, function := range functions {
		if function.Name == "__struct__" {
			sawHidden = function.Hidden
		}
	}
	if !sawHidden {
		t.Error("expected __struct__ to be reported as hidden")
	}
}

func BenchmarkReadDocumentedFunctionsFromAshFixture(b *testing.B) {
	path := os.Getenv("DEXTER_ASH_BEAM")
	if path == "" {
		b.Skip("set DEXTER_ASH_BEAM to benchmark a compiled Ash resource")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := ReadDocumentedFunctions(path); err != nil {
			b.Fatal(err)
		}
	}
}

func containsGeneratedFunction(functions []Function, wanted Function) bool {
	for _, function := range functions {
		if function.Name == wanted.Name && function.Arity == wanted.Arity &&
			function.Params == wanted.Params && function.Kind == wanted.Kind {
			return true
		}
	}
	return false
}

// --- inflate edge cases ------------------------------------------------------

func zlibBytes(t *testing.T, payload string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func compressedChunk(declared uint32, body []byte) []byte {
	chunk := []byte{etfVersion, tagCompressed, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(chunk[2:6], declared)
	return append(chunk, body...)
}

func TestInflateDocsTerm(t *testing.T) {
	body := zlibBytes(t, "hello")

	t.Run("exact declared size", func(t *testing.T) {
		got, err := inflateDocsTerm(compressedChunk(uint32(len("hello")), body))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "hello" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("declares more than the stream holds", func(t *testing.T) {
		// The reader sizes its buffer from this header, so a shortfall must
		// surface rather than hand back a partly zeroed buffer.
		if _, err := inflateDocsTerm(compressedChunk(4096, body)); err == nil {
			t.Error("expected a truncated inflate to fail")
		}
	})

	t.Run("declares less than the stream holds", func(t *testing.T) {
		long := zlibBytes(t, "hello world")
		if _, err := inflateDocsTerm(compressedChunk(uint32(len("hello")), long)); err == nil {
			t.Error("expected an under-declared size to fail rather than truncate")
		}
	})

	t.Run("declared size over the cap", func(t *testing.T) {
		if _, err := inflateDocsTerm(compressedChunk(maxInflatedETFSize+1, body)); err == nil {
			t.Error("expected an oversized inflate to be refused")
		}
	})

	t.Run("uncompressed passthrough", func(t *testing.T) {
		got, err := inflateDocsTerm([]byte{etfVersion, tagNil})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != tagNil {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("missing version byte", func(t *testing.T) {
		if _, err := inflateDocsTerm([]byte{tagNil}); err == nil {
			t.Error("expected a missing ETF version byte to fail")
		}
	})
}

// --- test fixtures -----------------------------------------------------------

var defaultTestExports = [][3]uint32{{2, 3, 1}, {3, 1, 2}, {4, 0, 3}, {5, 2, 4}}

var defaultTestAtoms = []string{"Elixir.Example", "create", "written_by_hand", "module_info", "MACRO-build"}

type testBEAMOptions struct {
	longAtoms bool
	atomNames []string
	exports   [][3]uint32
	docs      []byte
	attrs     []byte
}

func writeTestBEAM(t *testing.T, path string, docs []byte) {
	t.Helper()
	writeTestBEAMOpts(t, path, testBEAMOptions{
		atomNames: defaultTestAtoms,
		exports:   defaultTestExports,
		docs:      docs,
	})
}

func writeTestBEAMOpts(t *testing.T, path string, opts testBEAMOptions) {
	t.Helper()
	var size [4]byte

	var raw bytes.Buffer
	raw.WriteByte(etfVersion)
	if len(opts.docs) > 0 {
		raw.WriteByte(tagCompressed)
		var uncompressed bytes.Buffer
		uncompressed.Write(opts.docs)
		binary.BigEndian.PutUint32(size[:], uint32(uncompressed.Len()))
		raw.Write(size[:])
		var zw bytes.Buffer
		compressor := zlib.NewWriter(&zw)
		if _, err := compressor.Write(uncompressed.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := compressor.Close(); err != nil {
			t.Fatal(err)
		}
		raw.Write(zw.Bytes())
	} else {
		raw.WriteByte(tagNil)
	}

	var atoms bytes.Buffer
	count := uint32(len(opts.atomNames))
	if opts.longAtoms {
		// The negated count is the reader's signal to expect varint lengths.
		count = uint32(-int32(count))
	}
	binary.BigEndian.PutUint32(size[:], count)
	atoms.Write(size[:])
	for _, name := range opts.atomNames {
		if opts.longAtoms {
			atoms.Write(encodeTaggedU(uint32(len(name))))
		} else {
			atoms.WriteByte(byte(len(name)))
		}
		atoms.WriteString(name)
	}

	var exports bytes.Buffer
	binary.BigEndian.PutUint32(size[:], uint32(len(opts.exports)))
	exports.Write(size[:])
	for _, entry := range opts.exports {
		for _, value := range entry {
			binary.BigEndian.PutUint32(size[:], value)
			exports.Write(size[:])
		}
	}

	var chunks bytes.Buffer
	writeTestChunk(&chunks, "AtU8", atoms.Bytes())
	writeTestChunk(&chunks, "ExpT", exports.Bytes())
	if len(opts.attrs) > 0 {
		writeTestChunk(&chunks, "Attr", opts.attrs)
	}
	writeTestChunk(&chunks, "Docs", raw.Bytes())

	var file bytes.Buffer
	file.WriteString("FOR1")
	binary.BigEndian.PutUint32(size[:], uint32(chunks.Len()+4))
	file.Write(size[:])
	file.WriteString("BEAM")
	file.Write(chunks.Bytes())
	if err := os.WriteFile(path, file.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestChunk(chunks *bytes.Buffer, name string, data []byte) {
	chunks.WriteString(name)
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(data)))
	chunks.Write(size[:])
	chunks.Write(data)
	for chunks.Len()%4 != 0 {
		chunks.WriteByte(0)
	}
}

// encodeTaggedU mirrors ERTS beamreader_read_tagged for small TAG_u values:
// 4 bits inline when the value is under 16, otherwise 3+8 bits across 2 bytes.
func encodeTaggedU(value uint32) []byte {
	if value < 16 {
		return []byte{byte(value << 4)}
	}
	if value < 2048 {
		return []byte{byte(value>>8)<<5 | 0x08, byte(value)}
	}
	panic("test atom too long for the 11-bit form")
}

// docEntry describes one entry of the docs list in a synthetic Docs chunk.
type docEntry struct {
	kind      string // function, macro, type or callback
	name      string
	arity     int
	signature string
	doc       string // "none", "hidden", or the documentation prose
	defaults  int
}

// buildDocsTerm encodes a docs_v1 term, mirroring what the Elixir compiler
// writes. Only the tags the reader is expected to meet are produced.
func buildDocsTerm(entries ...docEntry) []byte {
	var w etfTestWriter
	w.smallTuple(7)
	w.atom("docs_v1")
	w.smallInt(1)
	w.atom("elixir")
	w.binary("text/markdown")
	w.atom("none")
	w.mapHeader(0) // chunk-level metadata
	w.listHeader(len(entries))
	for _, entry := range entries {
		w.smallTuple(5)

		w.smallTuple(3)
		w.atom(entry.kind)
		w.atom(entry.name)
		w.smallInt(entry.arity)

		w.smallInt(1) // anno

		w.listHeader(1)
		w.binary(entry.signature)
		w.nil()

		switch entry.doc {
		case "", "none":
			w.atom("none")
		case "hidden":
			w.atom("hidden")
		default:
			w.mapHeader(1)
			w.binary("en")
			w.binary(entry.doc)
		}

		if entry.defaults > 0 {
			w.mapHeader(1)
			w.atom("defaults")
			w.smallInt(entry.defaults)
		} else {
			w.mapHeader(0)
		}
	}
	w.nil()
	return w.buf
}

// etfTestWriter emits the small subset of ETF the fixtures need.
type etfTestWriter struct{ buf []byte }

func (w *etfTestWriter) byte(b byte) { w.buf = append(w.buf, b) }

// version emits the leading ETF version byte every chunk payload starts with.
func (w *etfTestWriter) version() { w.byte(etfVersion) }

func (w *etfTestWriter) smallTuple(arity int) {
	w.byte(tagSmallTuple)
	w.byte(byte(arity))
}

func (w *etfTestWriter) mapHeader(pairs int) {
	w.byte(tagMap)
	w.be32(uint32(pairs))
}

func (w *etfTestWriter) listHeader(count int) {
	w.byte(tagList)
	w.be32(uint32(count))
}

func (w *etfTestWriter) nil() { w.byte(tagNil) }

func (w *etfTestWriter) atom(name string) {
	if len(name) < 256 {
		w.byte(tagSmallAtomUTF8)
		w.byte(byte(len(name)))
	} else {
		w.byte(tagAtomUTF8)
		w.be16(uint16(len(name)))
	}
	w.buf = append(w.buf, name...)
}

func (w *etfTestWriter) binary(text string) {
	w.byte(tagBinary)
	w.be32(uint32(len(text)))
	w.buf = append(w.buf, text...)
}

func (w *etfTestWriter) smallInt(value int) {
	if value >= 0 && value < 256 {
		w.byte(tagSmallInteger)
		w.byte(byte(value))
		return
	}
	w.byte(tagInteger)
	w.be32(uint32(value))
}

func (w *etfTestWriter) be16(value uint16) {
	var field [2]byte
	binary.BigEndian.PutUint16(field[:], value)
	w.buf = append(w.buf, field[:]...)
}

func (w *etfTestWriter) be32(value uint32) {
	var field [4]byte
	binary.BigEndian.PutUint32(field[:], value)
	w.buf = append(w.buf, field[:]...)
}
