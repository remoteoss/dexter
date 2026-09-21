package beam

import (
	"errors"
)

// ReadSourcePath returns the source file recorded in a BEAM's compile info
// (the CInf chunk).
//
// The chunk is a short keyword list — `[version: ..., options: [...], source:
// ...]` — so this is a handful of terms, far cheaper than the Docs chunk it is
// usually read alongside.
//
// The path is the one the artifact was compiled in, which is not necessarily
// where the artifact lives now: a `_build` copied between checkouts, or any
// Docker/CI build, records a directory that no longer exists. Callers must treat
// the result as a hint and verify it before returning it to an editor.
//
// This matters for modules that have no source file of their own. Spark creates
// entity modules such as `Ash.Resource.Dsl.CodeInterface.Define` with an explicit
// location, so the module's recorded source is the framework file that generated
// it, and the source index has no row to consult.
func ReadSourcePath(path string) (string, bool) {
	raw, err := readChunk(path, "CInf")
	if err != nil {
		return "", false
	}
	return parseCompileSource(raw)
}

// parseCompileSource reads `{version, options, source}` pairs and returns the
// source entry. Any other shape is skipped rather than failing the chunk: a
// module without usable compile info must still yield its generated functions.
func parseCompileSource(raw []byte) (string, bool) {
	if len(raw) < 1 || raw[0] != etfVersion {
		return "", false
	}
	r := &etfReader{buf: raw[1:]}

	count, hasTail, err := r.enterList()
	if err != nil {
		return "", false
	}

	for i := int64(0); i < count; i++ {
		arity, err := r.enterTuple()
		if err != nil {
			return "", false
		}
		if arity != 2 {
			if err := r.skipTerms(int64(arity)); err != nil {
				return "", false
			}
			continue
		}

		key, err := r.readAtom()
		if err != nil {
			return "", false
		}
		if key != "source" {
			if err := r.skip(); err != nil {
				return "", false
			}
			continue
		}

		value, err := r.readStringTerm()
		if err != nil || value == "" {
			return "", false
		}
		return value, true
	}

	// enterList reports a tail for every LIST_EXT, not only improper ones, so the
	// terminator still has to be consumed when the loop runs to completion.
	if hasTail {
		if err := r.skip(); err != nil {
			return "", false
		}
	}
	return "", false
}

// readStringTerm reads a path-shaped term: a binary, a string, or a charlist of
// small integers. The compiler records :source as a charlist, which is stored as
// a compact string term; other producers of the same chunk use a binary.
func (r *etfReader) readStringTerm() (string, error) {
	tag, err := r.peekTag()
	if err != nil {
		return "", err
	}

	switch tag {
	case tagNil:
		_, err := r.u8()
		return "", err

	case tagBinary:
		return r.readBinary()

	case tagString:
		if _, err := r.u8(); err != nil {
			return "", err
		}
		n, err := r.u16()
		if err != nil {
			return "", err
		}
		if err := r.need(n); err != nil {
			return "", err
		}
		s := string(r.buf[r.pos : r.pos+n])
		r.pos += n
		return s, nil

	case tagList:
		count, hasTail, err := r.enterList()
		if err != nil {
			return "", err
		}
		// The chunk size already bounds count; this only keeps a corrupt length
		// from preallocating an absurd buffer.
		if count > maxSourcePathLen {
			return "", errors.New("charlist too long")
		}
		out := make([]byte, 0, count)
		for i := int64(0); i < count; i++ {
			n, err := r.readInt()
			if err != nil {
				return "", err
			}
			if n < 0 || n > 255 {
				return "", errors.New("charlist element out of range")
			}
			out = append(out, byte(n))
		}
		if hasTail {
			if err := r.skip(); err != nil {
				return "", err
			}
		}
		return string(out), nil

	default:
		return "", errors.New("unexpected source path encoding")
	}
}

// maxSourcePathLen bounds a charlist path. Real paths are far shorter; the limit
// exists so a corrupt length cannot drive a large allocation.
const maxSourcePathLen = 4096
