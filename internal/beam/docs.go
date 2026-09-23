// Package beam reads selected metadata from BEAM files without starting an
// Erlang VM or loading the compiled module.
package beam

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"
)

const (
	maxBEAMSize        = 64 << 20
	maxDocsChunkSize   = 16 << 20
	maxInflatedETFSize = 64 << 20
)

// Function describes a public function or macro found in a BEAM file.
type Function struct {
	Name   string
	Arity  int
	Params string
	Kind   string
	Hidden bool

	// DocOffset and DocLen locate the documentation prose inside the inflated
	// Docs chunk, so a hover can read one function's docs without decoding the
	// chunk again. They are zero when the function has no prose. zlib output is
	// deterministic, so re-inflating the same chunk reproduces these offsets.
	DocOffset int
	DocLen    int
}

// ReadDocumentedFunctions extracts public functions and macros from an
// Elixir BEAM's Docs chunk. Default arguments are expanded into each callable
// arity. Hidden entries are returned so callers do not reintroduce them from
// the export table.
func ReadDocumentedFunctions(path string) ([]Function, error) {
	docs, err := readChunk(path, "Docs")
	if err != nil {
		return nil, err
	}
	inflated, err := inflateDocsTerm(docs)
	if err != nil {
		return nil, fmt.Errorf("decode Docs chunk: %w", err)
	}
	return parseDocs(inflated)
}

// ReadExports reads the cheap AtU8/ExpT chunks and returns every callable
// export without inflating the much larger Docs term.
func ReadExports(path string) ([]Function, error) {
	return readExportsFromFile(path, false)
}

func readAllExports(path string) ([]Function, error) {
	return readExportsFromFile(path, true)
}

func readExportsFromFile(path string, includeInfrastructure bool) ([]Function, error) {
	// AtU8 has been the only atom table since OTP 20, so the latin1 "Atom" chunk
	// is not worth looking for.
	chunks, err := readChunks(path, "AtU8", "ExpT")
	if err != nil {
		return nil, err
	}
	atomsChunk, err := requiredChunk(chunks, "AtU8")
	if err != nil {
		return nil, err
	}
	exportsChunk, err := requiredChunk(chunks, "ExpT")
	if err != nil {
		return nil, err
	}
	atoms, err := readAtoms(atomsChunk)
	if err != nil {
		return nil, err
	}
	return readExportsWithOptions(exportsChunk, atoms, includeInfrastructure)
}

// ReadDocBody extracts one function's documentation prose from a BEAM's Docs
// chunk. offset and length come from Function.DocOffset and Function.DocLen,
// which are positions in the inflated chunk: zlib is deterministic, so
// re-inflating reproduces them exactly.
//
// Recording the position rather than the text is what lets completion skip the
// prose entirely — most of the chunk — while hover can still show it on demand.
func ReadDocBody(path string, offset, length int) (string, error) {
	if length <= 0 {
		return "", nil
	}
	docs, err := readChunk(path, "Docs")
	if err != nil {
		return "", err
	}
	inflated, err := inflateDocsTerm(docs)
	if err != nil {
		return "", err
	}
	if offset < 0 || offset+length > len(inflated) {
		return "", fmt.Errorf("doc span %d..%d outside %d inflated bytes", offset, offset+length, len(inflated))
	}
	return string(inflated[offset : offset+length]), nil
}

// readChunks opens the BEAM once and returns the named chunks it finds, keyed by
// four-byte chunk ID. Each chunk header costs a ReadAt, so asking for AtU8 and
// ExpT in separate passes doubles the syscalls for no benefit; one walk with an
// early exit serves both.
func readChunks(path string, wanted ...string) (map[string][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 12 || info.Size() > maxBEAMSize {
		return nil, fmt.Errorf("invalid BEAM size %d", info.Size())
	}

	var header [12]byte
	if _, err := f.ReadAt(header[:], 0); err != nil {
		return nil, err
	}
	if header[0] == 0x1f && header[1] == 0x8b {
		return readGzipChunks(f, wanted...)
	}
	return readChunksAt(f, info.Size(), header, wanted...)
}

func readGzipChunks(f *os.File, wanted ...string) (map[string][]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	compressed, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = compressed.Close() }()
	data, err := io.ReadAll(io.LimitReader(compressed, maxBEAMSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBEAMSize {
		return nil, fmt.Errorf("inflated BEAM too large: %d", len(data))
	}
	if len(data) < 12 {
		return nil, fmt.Errorf("invalid inflated BEAM size %d", len(data))
	}
	var header [12]byte
	copy(header[:], data[:12])
	return readChunksAt(bytes.NewReader(data), int64(len(data)), header, wanted...)
}

func readChunksAt(reader io.ReaderAt, size int64, header [12]byte, wanted ...string) (map[string][]byte, error) {
	if string(header[:4]) != "FOR1" || string(header[8:]) != "BEAM" {
		return nil, errors.New("invalid BEAM header")
	}
	declared := int64(binary.BigEndian.Uint32(header[4:8])) + 8
	if declared > size {
		return nil, errors.New("truncated BEAM container")
	}

	pending := make(map[string]bool, len(wanted))
	for _, name := range wanted {
		pending[name] = true
	}
	found := make(map[string][]byte, len(pending))

	for offset := int64(12); offset+8 <= declared && len(found) < len(pending); {
		var chunkHeader [8]byte
		if _, err := reader.ReadAt(chunkHeader[:], offset); err != nil {
			return nil, err
		}
		length := int64(binary.BigEndian.Uint32(chunkHeader[4:8]))
		dataOffset := offset + 8
		if length < 0 || dataOffset+length > declared {
			return nil, errors.New("invalid BEAM chunk length")
		}
		name := string(chunkHeader[:4])
		// First occurrence wins, so a malformed file repeating a chunk ID cannot
		// change which bytes a caller sees between two reads.
		if _, done := found[name]; pending[name] && !done {
			if length > maxDocsChunkSize {
				return nil, fmt.Errorf("%s chunk too large: %d", name, length)
			}
			data := make([]byte, length)
			if _, err := reader.ReadAt(data, dataOffset); err != nil {
				return nil, err
			}
			found[name] = data
		}
		offset = dataOffset + ((length + 3) &^ 3)
	}
	return found, nil
}

func readChunk(path, wanted string) ([]byte, error) {
	chunks, err := readChunks(path, wanted)
	if err != nil {
		return nil, err
	}
	return requiredChunk(chunks, wanted)
}

func requiredChunk(chunks map[string][]byte, name string) ([]byte, error) {
	data, ok := chunks[name]
	if !ok {
		return nil, fmt.Errorf("BEAM chunk %q not found", name)
	}
	return data, nil
}

// readAtoms decodes an AtU8 atom table. Dexter targets OTP 24+, which spans two
// layouts of this same-named chunk: through OTP 27 the count is a positive
// int32 and every length is a single byte; from OTP 28 (which lifted the
// 255-byte atom limit) the count is negated and every length is an ERTS tagged
// varint. The sign bit is the only reliable discriminator, matching ERTS
// parse_atom_chunk and beam_lib's extract_module.
func readAtoms(data []byte) ([]string, error) {
	if len(data) < 4 {
		return nil, errors.New("truncated atom table")
	}
	raw := int32(binary.BigEndian.Uint32(data[:4]))
	longCounts := raw < 0
	count := int(raw)
	if longCounts {
		count = -count
	}
	// Every atom costs at least one length byte, so a count larger than the
	// remaining space is corrupt. Check before preallocating.
	if count <= 0 || count > len(data)-4 {
		return nil, fmt.Errorf("invalid atom count %d for %d bytes", raw, len(data))
	}
	atoms := make([]string, count+1) // BEAM atom indexes are one-based.
	offset := 4
	for i := 1; i <= count; i++ {
		var length int
		if longCounts {
			var err error
			if length, offset, err = readTaggedLength(data, offset); err != nil {
				return nil, err
			}
		} else {
			if offset >= len(data) {
				return nil, errors.New("truncated atom table entry")
			}
			length = int(data[offset])
			offset++
		}
		if offset+length > len(data) {
			return nil, errors.New("truncated atom name")
		}
		atoms[i] = string(data[offset : offset+length])
		offset += length
	}
	return atoms, nil
}

// readTaggedLength decodes one ERTS tagged varint used for OTP 28+ atom lengths
// (beamreader_read_tagged). Bits 2-0 are a type tag, which beam_lib also
// ignores here, so only the size selector in bits 3-4 is inspected:
//
//	bit3 clear          length lives in bits 7-4        (0-15)
//	bit3 set, bit4 clear bits 7-5 are the high 3 bits, next byte the low 8 (0-2047)
//
// The general multi-byte form needs lengths of 2048 bytes or more. Atoms are
// capped at 255 codepoints, so at most 1020 UTF-8 bytes are possible and that
// form is rejected instead of misread.
func readTaggedLength(data []byte, offset int) (length, next int, err error) {
	if offset >= len(data) {
		return 0, offset, errors.New("truncated atom length")
	}
	code := data[offset]
	switch {
	case code&0x08 == 0:
		return int(code >> 4), offset + 1, nil
	case code&0x10 == 0:
		if offset+1 >= len(data) {
			return 0, offset, errors.New("truncated atom length")
		}
		return int(code>>5)<<8 | int(data[offset+1]), offset + 2, nil
	default:
		return 0, offset, errors.New("unsupported wide atom length encoding")
	}
}

func readExportsWithOptions(data []byte, atoms []string, includeInfrastructure bool) ([]Function, error) {
	if len(data) < 4 {
		return nil, errors.New("truncated export table")
	}
	count := int(binary.BigEndian.Uint32(data[:4]))
	if count > (len(data)-4)/12 {
		return nil, errors.New("truncated export table entries")
	}
	out := make([]Function, 0, count)
	for offset := 4; count > 0; count, offset = count-1, offset+12 {
		atomIndex := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		arity := int(binary.BigEndian.Uint32(data[offset+4 : offset+8]))
		if atomIndex <= 0 || atomIndex >= len(atoms) || arity > 255 {
			continue
		}
		name := atoms[atomIndex]
		kind := "def"
		if strings.HasPrefix(name, "MACRO-") {
			if arity == 0 {
				continue
			}
			name = strings.TrimPrefix(name, "MACRO-")
			arity-- // The compiler prepends the caller environment to macros.
			kind = "defmacro"
		}
		if !includeInfrastructure && isInfrastructureExport(name) {
			continue
		}
		params := make([]string, arity)
		for i := range params {
			params[i] = fmt.Sprintf("arg%d", i+1)
		}
		out = append(out, Function{Name: name, Arity: arity, Params: strings.Join(params, ","), Kind: kind})
	}
	return out, nil
}

func isInfrastructureExport(name string) bool {
	return name == "module_info" || name == "__info__" || name == "behaviour_info"
}

// inflateDocsTerm strips the ETF version byte and inflates a COMPRESSED_TERM.
// Elixir writes the Docs chunk compressed, and Function.DocOffset refers to
// positions in the inflated form; zlib is deterministic, so re-inflating the
// same chunk reproduces those offsets exactly.
func inflateDocsTerm(chunk []byte) ([]byte, error) {
	if len(chunk) < 2 || chunk[0] != etfVersion {
		return nil, errors.New("invalid ETF header")
	}
	payload := chunk[1:]
	if payload[0] != tagCompressed {
		return payload, nil
	}
	if len(payload) < 6 {
		return nil, errors.New("truncated compressed ETF")
	}
	expected := int64(binary.BigEndian.Uint32(payload[1:5]))
	if expected < 0 || expected > maxInflatedETFSize {
		return nil, fmt.Errorf("inflated ETF too large: %d", expected)
	}
	zr, err := zlib.NewReader(bytes.NewReader(payload[5:]))
	if err != nil {
		return nil, err
	}
	// The header declares the exact inflated size, so allocate it once instead of
	// letting io.ReadAll double its way there — on a real Docs chunk that is the
	// difference between ~400KB and ~1MB of churn.
	inflated := make([]byte, expected)
	_, readErr := io.ReadFull(zr, inflated)
	if readErr != nil {
		_ = zr.Close()
		return nil, readErr
	}
	// Stopping at the declared size would leave the stream partly read, which
	// skips zlib's Adler-32 verification and hides a header that under-declares.
	// One more byte must be EOF.
	var extra [1]byte
	if _, err := zr.Read(extra[:]); err != io.EOF {
		_ = zr.Close()
		if err == nil {
			return nil, fmt.Errorf("inflated ETF larger than declared %d", expected)
		}
		return nil, err
	}
	if err := zr.Close(); err != nil {
		return nil, err
	}
	return inflated, nil
}

// parseDocs walks {:docs_v1, anno, :elixir, format, module_doc, metadata, docs}
// and returns every documented function and macro.
//
// Only the fields completion needs are materialized. The documentation prose
// that makes up the overwhelming majority of the chunk is stepped over, with its
// position recorded rather than copied, which is what keeps this off the
// millisecond scale a generic decode lands on.
func parseDocs(buf []byte) (out []Function, err error) {
	// The reader is bounds-checked throughout, but a mistake in it must not be
	// able to take down the LSP over a corrupt local build artifact.
	defer func() {
		if recovered := recover(); recovered != nil {
			out = nil
			err = fmt.Errorf("invalid Docs term: %v", recovered)
		}
	}()

	r := &etfReader{buf: buf}
	arity, err := r.enterTuple()
	if err != nil {
		return nil, err
	}
	if arity != 7 {
		return nil, fmt.Errorf("docs_v1 tuple arity %d", arity)
	}
	if name, err := r.readAtom(); err != nil {
		return nil, err
	} else if name != "docs_v1" {
		return nil, fmt.Errorf("not a docs_v1 term: %q", name)
	}
	if err := r.skip(); err != nil { // anno
		return nil, err
	}
	if language, err := r.readAtom(); err != nil {
		return nil, err
	} else if language != "elixir" {
		return nil, fmt.Errorf("unsupported docs language %q", language)
	}
	// format, module_doc and chunk-level metadata carry nothing completion needs.
	for range 3 {
		if err := r.skip(); err != nil {
			return nil, err
		}
	}

	count, hasTail, err := r.enterList()
	if err != nil {
		return nil, err
	}
	for i := int64(0); i < count; i++ {
		entry, ok, err := parseDocsEntry(r)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, entry...)
		}
	}
	if hasTail {
		if err := r.skip(); err != nil {
			return nil, err
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].Arity < out[j].Arity
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// parseDocsEntry reads one {{kind, name, arity}, anno, signature, doc, metadata}
// entry and expands it across every callable arity its defaults imply. Entries
// for types and callbacks, or with an unexpected shape, are skipped rather than
// failing the chunk.
func parseDocsEntry(r *etfReader) ([]Function, bool, error) {
	arity, err := r.enterTuple()
	if err != nil {
		return nil, false, err
	}
	if arity != 5 {
		return nil, false, r.skipTerms(int64(arity))
	}

	keyArity, err := r.enterTuple()
	if err != nil {
		return nil, false, err
	}
	if keyArity != 3 {
		if err := r.skipTerms(int64(keyArity)); err != nil {
			return nil, false, err
		}
		return nil, false, r.skipTerms(4)
	}
	kind, err := r.readAtom()
	if err != nil {
		return nil, false, err
	}
	name, err := r.readAtom()
	if err != nil {
		return nil, false, err
	}
	functionArity, err := r.readInt()
	if err != nil {
		return nil, false, err
	}

	if err := r.skip(); err != nil { // anno
		return nil, false, err
	}
	params, err := readSignatureParams(r)
	if err != nil {
		return nil, false, err
	}
	hidden, docOffset, docLen, err := readDoc(r)
	if err != nil {
		return nil, false, err
	}
	defaults, err := readDefaults(r)
	if err != nil {
		return nil, false, err
	}

	if kind != "function" && kind != "macro" {
		return nil, false, nil
	}
	if name == "" || functionArity < 0 || functionArity > 255 {
		return nil, false, nil
	}
	if defaults < 0 || defaults > functionArity {
		defaults = 0
	}
	definitionKind := "def"
	if kind == "macro" {
		definitionKind = "defmacro"
	}

	out := make([]Function, 0, defaults+1)
	for callable := functionArity - defaults; callable <= functionArity; callable++ {
		callParams := make([]string, callable)
		for i := range callParams {
			if i < len(params) {
				callParams[i] = params[i]
			} else {
				callParams[i] = fmt.Sprintf("arg%d", i+1)
			}
		}
		out = append(out, Function{
			Name:      name,
			Arity:     callable,
			Params:    strings.Join(callParams, ","),
			Kind:      definitionKind,
			Hidden:    hidden,
			DocOffset: docOffset,
			DocLen:    docLen,
		})
	}
	return out, true, nil
}

// readSignatureParams consumes the signature list and parses the first signature
// into argument names. Signatures are short, so unlike documentation prose they
// are worth copying.
func readSignatureParams(r *etfReader) ([]string, error) {
	count, hasTail, err := r.enterList()
	if err != nil {
		return nil, err
	}
	var params []string
	for i := int64(0); i < count; i++ {
		if params == nil {
			if tag, err := r.peekTag(); err != nil {
				return nil, err
			} else if tag == tagBinary {
				text, err := r.readBinary()
				if err != nil {
					return nil, err
				}
				params = signatureParams(text)
				continue
			}
		}
		if err := r.skip(); err != nil {
			return nil, err
		}
	}
	if hasTail {
		if err := r.skip(); err != nil {
			return nil, err
		}
	}
	return params, nil
}

// readDoc consumes the documentation field, which is :none, :hidden, a bare
// binary, or a locale map such as %{"en" => prose}. It reports whether the entry
// was marked @doc false and, when there is prose, where it lives.
func readDoc(r *etfReader) (hidden bool, offset, length int, err error) {
	tag, err := r.peekTag()
	if err != nil {
		return false, 0, 0, err
	}
	switch {
	case isAtomTag(tag):
		name, err := r.readAtom()
		if err != nil {
			return false, 0, 0, err
		}
		return name == "hidden", 0, 0, nil

	case tag == tagBinary:
		start, n, err := r.binarySpan()
		if err != nil {
			return false, 0, 0, err
		}
		return false, start, n, nil

	case tag == tagMap:
		count, err := r.enterMap()
		if err != nil {
			return false, 0, 0, err
		}
		for i := int64(0); i < count; i++ {
			if err := r.skip(); err != nil { // locale key
				return false, 0, 0, err
			}
			valueTag, err := r.peekTag()
			if err != nil {
				return false, 0, 0, err
			}
			if valueTag == tagBinary && length == 0 {
				start, n, err := r.binarySpan()
				if err != nil {
					return false, 0, 0, err
				}
				offset, length = start, n
				continue
			}
			if err := r.skip(); err != nil {
				return false, 0, 0, err
			}
		}
		return false, offset, length, nil

	default:
		return false, 0, 0, r.skip()
	}
}

// readDefaults consumes the per-entry metadata map and returns its :defaults
// count: how many trailing arguments carry a default value.
func readDefaults(r *etfReader) (int, error) {
	tag, err := r.peekTag()
	if err != nil {
		return 0, err
	}
	if tag != tagMap {
		return 0, r.skip()
	}
	count, err := r.enterMap()
	if err != nil {
		return 0, err
	}
	defaults := 0
	for i := int64(0); i < count; i++ {
		var key string
		keyTag, err := r.peekTag()
		if err != nil {
			return 0, err
		}
		if isAtomTag(keyTag) {
			if key, err = r.readAtom(); err != nil {
				return 0, err
			}
		} else if err := r.skip(); err != nil {
			return 0, err
		}
		if key == "defaults" {
			if defaults, err = r.readInt(); err != nil {
				return 0, err
			}
			continue
		}
		if err := r.skip(); err != nil {
			return 0, err
		}
	}
	return defaults, nil
}

// signatureParams extracts argument names from one signature string such as
// "create(email, params \\ nil, opts \\ nil)".
func signatureParams(text string) []string {
	open := strings.IndexByte(text, '(')
	if open < 0 {
		return nil
	}
	args, ok := signatureArgs(text[open+1:])
	if !ok {
		return nil
	}
	parts := splitArgs(args)
	for i := range parts {
		parts[i] = paramName(parts[i], i+1)
	}
	return parts
}

func signatureArgs(rest string) (string, bool) {
	depth := 0
	for i, r := range rest {
		switch r {
		case '(':
			depth++
		case ')':
			if depth == 0 {
				return rest[:i], true
			}
			depth--
		}
	}
	return "", false
}

func splitArgs(args string) []string {
	if strings.TrimSpace(args) == "" {
		return nil
	}
	var parts []string
	start := 0
	paren, bracket, brace := 0, 0, 0
	for i, r := range args {
		switch r {
		case '(':
			paren++
		case ')':
			paren--
		case '[':
			bracket++
		case ']':
			bracket--
		case '{':
			brace++
		case '}':
			brace--
		case ',':
			if paren == 0 && bracket == 0 && brace == 0 {
				parts = append(parts, strings.TrimSpace(args[start:i]))
				start = i + 1
			}
		}
	}
	return append(parts, strings.TrimSpace(args[start:]))
}

func paramName(param string, fallback int) string {
	if cut := strings.Index(param, "\\\\"); cut >= 0 {
		param = param[:cut]
	}
	var last string
	for start := 0; start < len(param); {
		for start < len(param) && param[start] != '_' && !unicode.IsLetter(rune(param[start])) {
			start++
		}
		end := start
		for end < len(param) && (param[end] == '_' || unicode.IsLetter(rune(param[end])) || unicode.IsDigit(rune(param[end]))) {
			end++
		}
		if end > start {
			candidate := param[start:end]
			if candidate[0] == '_' || (candidate[0] >= 'a' && candidate[0] <= 'z') {
				last = candidate
			}
		}
		start = end + 1
	}
	if last == "" {
		return fmt.Sprintf("arg%d", fallback)
	}
	return last
}
