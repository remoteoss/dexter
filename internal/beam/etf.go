package beam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
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

// maxETFDepth bounds recursive ETF terms. Docs are shallow, but expanded Elixir
// Dbgi ASTs from generated routers can legitimately exceed 64 levels.
const maxETFDepth = 512

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
}

type etfAtom string
type etfTuple []interface{}
type etfOpaque byte

type etfListValue struct {
	Values []interface{}
	Tail   interface{}
}

type etfMapPair struct {
	Key   interface{}
	Value interface{}
}

type etfMapValue []etfMapPair

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

// decode materializes one bounded ETF term. Hot Docs queries use the selective
// reader above; Dbgi evidence uses this decoder because expanded Elixir ASTs
// require recursive semantic inspection.
func (r *etfReader) decode() (interface{}, error) {
	if r.depth >= maxETFDepth {
		return nil, errTooDeep
	}
	tag, err := r.u8()
	if err != nil {
		return nil, err
	}
	switch tag {
	case tagNil:
		return etfListValue{}, nil
	case tagSmallInteger:
		value, err := r.u8()
		return int(value), err
	case tagInteger:
		if err := r.need(4); err != nil {
			return nil, err
		}
		value := int(int32(binary.BigEndian.Uint32(r.buf[r.pos:])))
		r.pos += 4
		return value, nil
	case tagNewFloat:
		if err := r.need(8); err != nil {
			return nil, err
		}
		value := math.Float64frombits(binary.BigEndian.Uint64(r.buf[r.pos:]))
		r.pos += 8
		return value, nil
	case tagAtom, tagAtomUTF8:
		length, err := r.u16()
		if err != nil {
			return nil, err
		}
		return r.decodeAtomBytes(length)
	case tagSmallAtom, tagSmallAtomUTF8:
		length, err := r.u8()
		if err != nil {
			return nil, err
		}
		return r.decodeAtomBytes(int(length))
	case tagString:
		length, err := r.u16()
		if err != nil {
			return nil, err
		}
		if err := r.need(length); err != nil {
			return nil, err
		}
		value := string(r.buf[r.pos : r.pos+length])
		r.pos += length
		return value, nil
	case tagBinary:
		length, err := r.u32()
		if err != nil || length > int64(r.remaining()) {
			return nil, errTruncated
		}
		value := string(r.buf[r.pos : r.pos+int(length)])
		r.pos += int(length)
		return value, nil
	case tagBitBinary:
		length, err := r.u32()
		if err != nil {
			return nil, err
		}
		bits, err := r.u8()
		if err != nil || length > int64(r.remaining()) {
			return nil, errTruncated
		}
		value := struct {
			Data string
			Bits byte
		}{string(r.buf[r.pos : r.pos+int(length)]), bits}
		r.pos += int(length)
		return value, nil
	case tagSmallTuple, tagLargeTuple:
		r.pos--
		count, err := r.enterTuple()
		if err != nil {
			return nil, err
		}
		values := make(etfTuple, count)
		if err := r.decodeValues(values); err != nil {
			return nil, err
		}
		return values, nil
	case tagList:
		count, err := r.u32()
		if err != nil || r.checkCount(count, 1) != nil {
			return nil, errBadCount
		}
		values := make([]interface{}, int(count))
		if err := r.decodeValues(values); err != nil {
			return nil, err
		}
		tail, err := r.decodeChild()
		if err != nil {
			return nil, err
		}
		return etfListValue{Values: values, Tail: tail}, nil
	case tagMap:
		count, err := r.u32()
		if err != nil || r.checkCount(count, 2) != nil {
			return nil, errBadCount
		}
		pairs := make(etfMapValue, int(count))
		for i := range pairs {
			pairs[i].Key, err = r.decodeChild()
			if err == nil {
				pairs[i].Value, err = r.decodeChild()
			}
			if err != nil {
				return nil, err
			}
		}
		return pairs, nil
	case tagSmallBig, tagLargeBig:
		r.pos--
		start := r.pos
		if err := r.skip(); err != nil {
			return nil, err
		}
		return append([]byte(nil), r.buf[start:r.pos]...), nil
	default:
		r.pos--
		if err := r.skip(); err != nil {
			return nil, err
		}
		return etfOpaque(tag), nil
	}
}

func (r *etfReader) decodeAtomBytes(length int) (interface{}, error) {
	if err := r.need(length); err != nil {
		return nil, err
	}
	value := etfAtom(r.buf[r.pos : r.pos+length])
	r.pos += length
	return value, nil
}

func (r *etfReader) decodeValues(values []interface{}) error {
	for i := range values {
		value, err := r.decodeChild()
		if err != nil {
			return err
		}
		values[i] = value
	}
	return nil
}

func (r *etfReader) decodeChild() (interface{}, error) {
	r.depth++
	defer func() { r.depth-- }()
	return r.decode()
}

// skip advances past one term of any shape without allocating for it.
func (r *etfReader) skip() error {
	if r.depth >= maxETFDepth {
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
