package beam

import (
	"errors"
)

// ReadModuleAttributes returns the module attributes persisted in a BEAM's Attr
// chunk, keyed by attribute name. Only atom and binary values are kept; compound
// values are skipped, so the result is the flat string view callers can act on.
//
// The chunk is small (404 bytes for a typical module) and stored uncompressed,
// so reading and parsing it measured ~15us — cheap enough to do per module and
// cache, and roughly a fortieth of decoding the same file's Docs chunk.
//
// Attributes are how a compiled module records facts that its source does not
// state. Frameworks that generate code at compile time persist what they
// generated from; Spark, which backs Ash, stores the extension modules that
// supply a module's DSL macros under "extensions". Those macros have no defmacro
// anywhere in source, so this is the only place their provider can be discovered
// without interpreting the framework's code generation.
func ReadModuleAttributes(path string) (map[string][]string, error) {
	raw, err := readChunk(path, "Attr")
	if err != nil {
		return nil, err
	}
	return parseAttributes(raw)
}

// parseAttributes reads the Attr term: a list of {name, [values]} pairs.
func parseAttributes(raw []byte) (map[string][]string, error) {
	if len(raw) < 1 || raw[0] != etfVersion {
		return nil, errors.New("invalid ETF header")
	}
	r := &etfReader{buf: raw[1:]}
	count, hasTail, err := r.enterList()
	if err != nil {
		return nil, err
	}
	// Cap the size hint: a corrupt length could otherwise ask for a huge map
	// even though checkCount already bounds it by the chunk size.
	hint := count
	if hint > 64 {
		hint = 64
	}
	attrs := make(map[string][]string, hint)

	for i := int64(0); i < count; i++ {
		arity, err := r.enterTuple()
		if err != nil {
			return nil, err
		}
		if arity != 2 {
			if err := r.skipTerms(int64(arity)); err != nil {
				return nil, err
			}
			continue
		}
		name, err := r.readAtom()
		if err != nil {
			return nil, err
		}
		values, err := readAttributeValues(r)
		if err != nil {
			return nil, err
		}
		if len(values) > 0 {
			attrs[name] = values
		}
	}
	if hasTail {
		if err := r.skip(); err != nil {
			return nil, err
		}
	}
	return attrs, nil
}

// readAttributeValues consumes one attribute's value list, keeping atoms and
// binaries and skipping anything compound. A value that is not a list at all is
// skipped whole rather than failing the chunk.
func readAttributeValues(r *etfReader) ([]string, error) {
	tag, err := r.peekTag()
	if err != nil {
		return nil, err
	}
	if tag != tagList && tag != tagNil {
		return nil, r.skip()
	}
	count, hasTail, err := r.enterList()
	if err != nil {
		return nil, err
	}
	var values []string
	for i := int64(0); i < count; i++ {
		valueTag, err := r.peekTag()
		if err != nil {
			return nil, err
		}
		switch {
		case isAtomTag(valueTag):
			value, err := r.readAtom()
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		case valueTag == tagBinary:
			value, err := r.readBinary()
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		default:
			if err := r.skip(); err != nil {
				return nil, err
			}
		}
	}
	if hasTail {
		if err := r.skip(); err != nil {
			return nil, err
		}
	}
	return values, nil
}

// StripElixirPrefix converts a BEAM module atom such as "Elixir.Ash.Domain.Dsl"
// into the name Dexter's index uses. Erlang modules carry no prefix and are
// returned unchanged.
func StripElixirPrefix(module string) string {
	const prefix = "Elixir."
	if len(module) > len(prefix) && module[:len(prefix)] == prefix {
		return module[len(prefix):]
	}
	return module
}
