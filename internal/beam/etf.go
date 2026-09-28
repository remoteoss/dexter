package beam

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ETF tags, from the Erlang external term format specification. Only the tags
// that can appear in an Elixir Docs chunk are decoded into values; the rest are
// still skippable so an unexpected term cannot desynchronize the walk.
const (
	tagCompressed    = 80
	tagNewFloat      = 70
	tagBitBinary     = 77
	tagSmallInteger  = 97
	tagInteger       = 98
	tagFloat         = 99
	tagAtom          = 100
	tagSmallTuple    = 104
	tagLargeTuple    = 105
	tagNil           = 106
	tagString        = 107
	tagList          = 108
	tagBinary        = 109
	tagSmallBig      = 110
	tagLargeBig      = 111
	tagNewFun        = 112
	tagExport        = 113
	tagSmallAtom     = 115
	tagMap           = 116
	tagAtomUTF8      = 118
	tagSmallAtomUTF8 = 119
)

// etfVersion is the leading byte of every encoded term.
const etfVersion = 131

func isAtomTag(tag byte) bool {
	return tag == tagAtom || tag == tagAtomUTF8 || tag == tagSmallAtom || tag == tagSmallAtomUTF8
}

// maxETFDepth bounds recursion in skip. A Docs chunk nests at most five levels
// deep, so a corrupt length that implied more is rejected rather than followed.
const maxETFDepth = 64

var (
	errUnsupportedTag = errors.New("unsupported ETF tag")
	errTruncated      = errors.New("truncated ETF term")
	errBadCount       = errors.New("ETF count exceeds remaining bytes")
	errTooDeep        = errors.New("ETF nesting too deep")
)

// etfReader walks an ETF term in place.
//
// It exists because a generic decoder is the wrong tool here. An Elixir Docs
// chunk inflates to ~400KB of which ~97% is documentation prose, and decoding
// it into an `any` graph measured 1.1ms and 49k allocations on a single Ash
// resource — none of which completion needs. This reader instead extracts the
// handful of fields that matter and skips everything else, most importantly by
// stepping over documentation binaries without copying them.
//
// Every read is bounds-checked, so malformed input yields an error rather than a
// panic or an out-of-range slice.
type etfReader struct {
	buf   []byte
	pos   int
	depth int

	// maxDepth overrides maxETFDepth for terms that legitimately nest deeper,
	// such as the clause ASTs in a Dbgi chunk. Zero keeps the default.
	maxDepth int
}

func (r *etfReader) remaining() int { return len(r.buf) - r.pos }

func (r *etfReader) need(n int) error {
	if n < 0 || n > r.remaining() {
		return errTruncated
	}
	return nil
}

func (r *etfReader) u8() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	b := r.buf[r.pos]
	r.pos++
	return b, nil
}

func (r *etfReader) u16() (int, error) {
	if err := r.need(2); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(r.buf[r.pos:]))
	r.pos += 2
	return n, nil
}

// u32 reads a big-endian uint32 as an int64 so a corrupt length cannot wrap
// negative on a 32-bit platform before the bounds check sees it.
func (r *etfReader) u32() (int64, error) {
	if err := r.need(4); err != nil {
		return 0, err
	}
	n := int64(binary.BigEndian.Uint32(r.buf[r.pos:]))
	r.pos += 4
	return n, nil
}

// checkCount rejects a collection length that could not possibly fit in what is
// left. Every term is at least one byte, which makes that a cheap sound bound.
func (r *etfReader) checkCount(count int64, terms int64) error {
	if count < 0 {
		return errBadCount
	}
	if terms > 0 && count*terms > int64(r.remaining()) {
		return errBadCount
	}
	return nil
}

func (r *etfReader) skipBytes(n int64) error {
	if n < 0 || n > int64(r.remaining()) {
		return errTruncated
	}
	r.pos += int(n)
	return nil
}

func (r *etfReader) peekTag() (byte, error) {
	if err := r.need(1); err != nil {
		return 0, err
	}
	return r.buf[r.pos], nil
}

// enterTuple consumes a tuple header and returns its arity, leaving the reader
// at the first element.
func (r *etfReader) enterTuple() (int, error) {
	tag, err := r.u8()
	if err != nil {
		return 0, err
	}
	switch tag {
	case tagSmallTuple:
		n, err := r.u8()
		if err != nil {
			return 0, err
		}
		if err := r.checkCount(int64(n), 1); err != nil {
			return 0, err
		}
		return int(n), nil
	case tagLargeTuple:
		n, err := r.u32()
		if err != nil {
			return 0, err
		}
		if err := r.checkCount(n, 1); err != nil {
			return 0, err
		}
		return int(n), nil
	default:
		return 0, fmt.Errorf("%w: expected tuple, got tag %d", errUnsupportedTag, tag)
	}
}

// enterList consumes a list header and returns its element count, leaving the
// reader at the first element. hasTail reports whether a tail term follows those
// elements: NIL_EXT is an empty list carrying no tail, while LIST_EXT always has
// one (NIL_EXT for a proper list). Callers must skip the tail only when hasTail
// is true, or they will consume the term that follows the list.
func (r *etfReader) enterList() (count int64, hasTail bool, err error) {
	tag, err := r.u8()
	if err != nil {
		return 0, false, err
	}
	if tag == tagNil {
		return 0, false, nil
	}
	if tag != tagList {
		return 0, false, fmt.Errorf("%w: expected list, got tag %d", errUnsupportedTag, tag)
	}
	n, err := r.u32()
	if err != nil {
		return 0, false, err
	}
	if err := r.checkCount(n, 1); err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// enterMap consumes a map header and returns its pair count.
func (r *etfReader) enterMap() (int64, error) {
	tag, err := r.u8()
	if err != nil {
		return 0, err
	}
	if tag != tagMap {
		return 0, fmt.Errorf("%w: expected map, got tag %d", errUnsupportedTag, tag)
	}
	n, err := r.u32()
	if err != nil {
		return 0, err
	}
	if err := r.checkCount(n, 2); err != nil {
		return 0, err
	}
	return n, nil
}

// readAtom reads an atom term as a string.
func (r *etfReader) readAtom() (string, error) {
	tag, err := r.u8()
	if err != nil {
		return "", err
	}
	var length int
	switch tag {
	case tagAtom, tagAtomUTF8:
		length, err = r.u16()
	case tagSmallAtom, tagSmallAtomUTF8:
		var b byte
		if b, err = r.u8(); err != nil {
			return "", err
		}
		length = int(b)
	default:
		return "", fmt.Errorf("%w: expected atom, got tag %d", errUnsupportedTag, tag)
	}
	if err != nil {
		return "", err
	}
	if err := r.need(length); err != nil {
		return "", err
	}
	name := string(r.buf[r.pos : r.pos+length])
	r.pos += length
	return name, nil
}

// readInt reads an integer term. Only the widths a Docs chunk uses are
// accepted; big integers carry no meaningful arity or default count.
func (r *etfReader) readInt() (int, error) {
	tag, err := r.u8()
	if err != nil {
		return 0, err
	}
	switch tag {
	case tagSmallInteger:
		b, err := r.u8()
		return int(b), err
	case tagInteger:
		if err := r.need(4); err != nil {
			return 0, err
		}
		n := int(int32(binary.BigEndian.Uint32(r.buf[r.pos:])))
		r.pos += 4
		return n, nil
	default:
		return 0, fmt.Errorf("%w: expected integer, got tag %d", errUnsupportedTag, tag)
	}
}

// readBinary copies a binary term. Use only for the short strings the result
// actually keeps, such as signatures; documentation bodies must use binarySpan.
func (r *etfReader) readBinary() (string, error) {
	start, length, err := r.binarySpan()
	if err != nil {
		return "", err
	}
	return string(r.buf[start : start+length]), nil
}

// binarySpan returns the offset and length of a binary term's payload without
// copying it, then advances past it. The offset is into the inflated Docs chunk,
// which zlib reproduces byte for byte, so a caller can re-inflate later and read
// exactly this range.
func (r *etfReader) binarySpan() (start, length int, err error) {
	tag, err := r.u8()
	if err != nil {
		return 0, 0, err
	}
	if tag != tagBinary {
		return 0, 0, fmt.Errorf("%w: expected binary, got tag %d", errUnsupportedTag, tag)
	}
	n, err := r.u32()
	if err != nil {
		return 0, 0, err
	}
	if err := r.need(int(n)); err != nil {
		return 0, 0, err
	}
	start = r.pos
	r.pos += int(n)
	return start, int(n), nil
}

// skip advances past one term of any shape without allocating for it.
func (r *etfReader) skip() error {
	limit := r.maxDepth
	if limit == 0 {
		limit = maxETFDepth
	}
	if r.depth >= limit {
		return errTooDeep
	}
	tag, err := r.u8()
	if err != nil {
		return err
	}
	switch tag {
	case tagNil:
		return nil
	case tagSmallInteger:
		return r.skipBytes(1)
	case tagInteger:
		return r.skipBytes(4)
	case tagNewFloat:
		return r.skipBytes(8)
	case tagFloat:
		return r.skipBytes(31)
	case tagAtom, tagAtomUTF8, tagString:
		n, err := r.u16()
		if err != nil {
			return err
		}
		return r.skipBytes(int64(n))
	case tagSmallAtom, tagSmallAtomUTF8:
		n, err := r.u8()
		if err != nil {
			return err
		}
		return r.skipBytes(int64(n))
	case tagBinary:
		n, err := r.u32()
		if err != nil {
			return err
		}
		return r.skipBytes(n)
	case tagBitBinary:
		n, err := r.u32()
		if err != nil {
			return err
		}
		if err := r.skipBytes(1); err != nil {
			return err
		}
		return r.skipBytes(n)
	case tagSmallBig:
		n, err := r.u8()
		if err != nil {
			return err
		}
		return r.skipBytes(int64(n) + 1)
	case tagLargeBig:
		n, err := r.u32()
		if err != nil {
			return err
		}
		return r.skipBytes(n + 1)
	case tagSmallTuple:
		n, err := r.u8()
		if err != nil {
			return err
		}
		return r.skipTerms(int64(n))
	case tagLargeTuple:
		n, err := r.u32()
		if err != nil {
			return err
		}
		return r.skipTerms(n)
	case tagList:
		n, err := r.u32()
		if err != nil {
			return err
		}
		if err := r.checkCount(n, 1); err != nil {
			return err
		}
		if err := r.skipTerms(n); err != nil {
			return err
		}
		return r.skip() // the tail
	case tagMap:
		n, err := r.u32()
		if err != nil {
			return err
		}
		if err := r.checkCount(n, 2); err != nil {
			return err
		}
		return r.skipTerms(n * 2)
	case tagNewFun:
		// Size counts the whole term including the four size bytes themselves.
		n, err := r.u32()
		if err != nil {
			return err
		}
		return r.skipBytes(n - 4)
	case tagExport:
		// An external fun, `fun M:F/A`. Unlike NEW_FUN_EXT, whose Arity is a bare
		// byte, this one encodes the arity as an integer term, so skipping a fixed
		// byte would misalign every term that follows it.
		if _, err := r.readAtom(); err != nil {
			return err
		}
		if _, err := r.readAtom(); err != nil {
			return err
		}
		return r.skip()
	default:
		return fmt.Errorf("%w: %d", errUnsupportedTag, tag)
	}
}

func (r *etfReader) skipTerms(count int64) error {
	if err := r.checkCount(count, 1); err != nil {
		return err
	}
	r.depth++
	defer func() { r.depth-- }()
	for i := int64(0); i < count; i++ {
		if err := r.skip(); err != nil {
			return err
		}
	}
	return nil
}
