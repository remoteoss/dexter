package beam

import (
	"errors"
	"fmt"
)

// maxDebugInfoDepth bounds recursion while stepping over clause bodies. Each
// level of Elixir AST costs two ETF levels (the call tuple and its argument
// list), so this allows a thousand nested expressions: far past any real code,
// and still a bounded Go stack on a corrupt file.
const maxDebugInfoDepth = 2048

// FunctionKey names one callable by name and arity.
type FunctionKey struct {
	Name  string
	Arity int
}

// DebugInfo is what ReadDefinitionLines extracts from an Elixir Dbgi chunk.
type DebugInfo struct {
	// File is the absolute source path the module was compiled from, and
	// RelativeFile the same path relative to the compiler's working directory.
	// Either may be empty if the chunk omits it.
	File         string
	RelativeFile string

	// Lines maps each public function and macro to its line in File. Entries
	// without a usable line are absent.
	Lines map[FunctionKey]int
}

// ReadDefinitionLines reads the line of every public definition from an Elixir
// BEAM's debug info, for functions that have no source definition to index
// because a macro generated them.
//
// A generated def carries two locations. Its :line is the line in the module's
// own file that was being compiled when the def was produced: the macro call,
// or wherever a before-compile hook ran. A `file: {path, line}` entry, left by
// `@file` or by `quote location: :keep`, names a more specific place. When that
// path is the module's own source it is where the code asked for the function,
// such as an Ash `define :get_by_slug` line; when it is some other file it is
// the generator's own implementation, which is not what a reader of the module
// is looking for, so :line is used instead.
//
// Everything here is standard compiler output. No framework is recognized by
// name: a generator that wants its functions to navigate to the line that
// declared them stamps them with `@file`, which also fixes their stacktraces.
func ReadDefinitionLines(path string) (DebugInfo, error) {
	chunk, err := readChunk(path, "Dbgi")
	if err != nil {
		return DebugInfo{}, err
	}
	inflated, err := inflateDocsTerm(chunk)
	if err != nil {
		return DebugInfo{}, fmt.Errorf("decode Dbgi chunk: %w", err)
	}
	return parseDebugInfo(inflated)
}

var errNoElixirDebugInfo = errors.New("no Elixir debug info")

// definitionSite is one definition's raw locations, kept until the module's
// source path is known: small maps put definitions ahead of file and
// relative_file, so they cannot be resolved while walking.
type definitionSite struct {
	key      FunctionKey
	line     int
	keepFile string
	keepLine int
}

// parseDebugInfo walks {:debug_info_v1, :elixir_erl, {:elixir_v1, map, specs}}.
// Only three keys of the map are read; clause bodies, which are nearly all of
// the chunk, are stepped over without allocating.
func parseDebugInfo(buf []byte) (info DebugInfo, err error) {
	// Bounds-checked throughout, but as with Docs a reader mistake must not take
	// down the server over a corrupt build artifact.
	defer func() {
		if recovered := recover(); recovered != nil {
			info = DebugInfo{}
			err = fmt.Errorf("invalid Dbgi term: %v", recovered)
		}
	}()

	r := &etfReader{buf: buf, maxDepth: maxDebugInfoDepth}
	if arity, err := r.enterTuple(); err != nil {
		return DebugInfo{}, err
	} else if arity != 3 {
		return DebugInfo{}, fmt.Errorf("debug_info tuple arity %d", arity)
	}
	if version, err := r.readAtom(); err != nil {
		return DebugInfo{}, err
	} else if version != "debug_info_v1" {
		return DebugInfo{}, fmt.Errorf("unsupported debug info version %q", version)
	}
	if backend, err := r.readAtom(); err != nil {
		return DebugInfo{}, err
	} else if backend != "elixir_erl" {
		return DebugInfo{}, fmt.Errorf("%w: backend %q", errNoElixirDebugInfo, backend)
	}
	// Compiling with `debug_info: false` leaves :none here.
	if tag, err := r.peekTag(); err != nil {
		return DebugInfo{}, err
	} else if tag != tagSmallTuple && tag != tagLargeTuple {
		return DebugInfo{}, errNoElixirDebugInfo
	}
	if arity, err := r.enterTuple(); err != nil {
		return DebugInfo{}, err
	} else if arity != 3 {
		return DebugInfo{}, fmt.Errorf("elixir debug info tuple arity %d", arity)
	}
	if format, err := r.readAtom(); err != nil {
		return DebugInfo{}, err
	} else if format != "elixir_v1" {
		return DebugInfo{}, fmt.Errorf("unsupported Elixir debug info %q", format)
	}

	pairs, err := r.enterMap()
	if err != nil {
		return DebugInfo{}, err
	}
	var sites []definitionSite
	for i := int64(0); i < pairs; i++ {
		var key string
		if tag, err := r.peekTag(); err != nil {
			return DebugInfo{}, err
		} else if isAtomTag(tag) {
			if key, err = r.readAtom(); err != nil {
				return DebugInfo{}, err
			}
		} else if err := r.skip(); err != nil {
			return DebugInfo{}, err
		}
		switch key {
		case "definitions":
			if sites, err = readDefinitionSites(r); err != nil {
				return DebugInfo{}, err
			}
		case "file":
			if info.File, err = readOptionalBinary(r); err != nil {
				return DebugInfo{}, err
			}
		case "relative_file":
			if info.RelativeFile, err = readOptionalBinary(r); err != nil {
				return DebugInfo{}, err
			}
		default:
			if err := r.skip(); err != nil {
				return DebugInfo{}, err
			}
		}
	}
	// The specs are not needed, but consuming them proves the term is whole.
	if err := r.skip(); err != nil {
		return DebugInfo{}, err
	}

	info.Lines = make(map[FunctionKey]int, len(sites))
	for _, site := range sites {
		line := site.line
		if site.keepFile != "" && site.keepLine > 0 &&
			(site.keepFile == info.RelativeFile || site.keepFile == info.File) {
			line = site.keepLine
		}
		if line > 0 {
			info.Lines[site.key] = line
		}
	}
	return info, nil
}

// readDefinitionSites reads the definitions list, whose entries are
// {{name, arity}, kind, meta, clauses}. Private definitions are skipped:
// generated functions are found through the export table, so only public ones
// can be asked about.
func readDefinitionSites(r *etfReader) ([]definitionSite, error) {
	count, hasTail, err := r.enterList()
	if err != nil {
		return nil, err
	}
	sites := make([]definitionSite, 0, count)
	for i := int64(0); i < count; i++ {
		arity, err := r.enterTuple()
		if err != nil {
			return nil, err
		}
		if arity != 4 {
			if err := r.skipTerms(int64(arity)); err != nil {
				return nil, err
			}
			continue
		}
		if keyArity, err := r.enterTuple(); err != nil {
			return nil, err
		} else if keyArity != 2 {
			return nil, fmt.Errorf("definition key arity %d", keyArity)
		}
		name, err := r.readAtom()
		if err != nil {
			return nil, err
		}
		functionArity, err := r.readInt()
		if err != nil {
			return nil, err
		}
		kind, err := r.readAtom()
		if err != nil {
			return nil, err
		}
		site := definitionSite{key: FunctionKey{Name: name, Arity: functionArity}}
		if err := readDefinitionMeta(r, &site); err != nil {
			return nil, err
		}
		if err := r.skip(); err != nil { // clauses
			return nil, err
		}
		if kind == "def" || kind == "defmacro" {
			sites = append(sites, site)
		}
	}
	if hasTail {
		if err := r.skip(); err != nil {
			return nil, err
		}
	}
	return sites, nil
}

// readDefinitionMeta reads :line and a `file: {path, line}` entry from a
// definition's keyword metadata. The first of each wins, as Keyword.get would.
func readDefinitionMeta(r *etfReader, site *definitionSite) error {
	count, hasTail, err := r.enterList()
	if err != nil {
		return err
	}
	seenLine, seenFile := false, false
	for i := int64(0); i < count; i++ {
		if tag, err := r.peekTag(); err != nil {
			return err
		} else if tag != tagSmallTuple {
			if err := r.skip(); err != nil {
				return err
			}
			continue
		}
		arity, err := r.enterTuple()
		if err != nil {
			return err
		}
		if arity != 2 {
			if err := r.skipTerms(int64(arity)); err != nil {
				return err
			}
			continue
		}
		var key string
		if tag, err := r.peekTag(); err != nil {
			return err
		} else if isAtomTag(tag) {
			if key, err = r.readAtom(); err != nil {
				return err
			}
		} else if err := r.skip(); err != nil {
			return err
		}
		switch {
		case key == "line" && !seenLine:
			seenLine = true
			if site.line, err = readOptionalInt(r); err != nil {
				return err
			}
		case key == "file" && !seenFile:
			seenFile = true
			if site.keepFile, site.keepLine, err = readFileLocation(r); err != nil {
				return err
			}
		default:
			if err := r.skip(); err != nil {
				return err
			}
		}
	}
	if hasTail {
		return r.skip()
	}
	return nil
}

// readFileLocation reads a {path, line} tuple. Any other shape is skipped and
// reported as no location.
func readFileLocation(r *etfReader) (string, int, error) {
	if tag, err := r.peekTag(); err != nil {
		return "", 0, err
	} else if tag != tagSmallTuple {
		return "", 0, r.skip()
	}
	arity, err := r.enterTuple()
	if err != nil {
		return "", 0, err
	}
	if arity != 2 {
		return "", 0, r.skipTerms(int64(arity))
	}
	file, err := readOptionalBinary(r)
	if err != nil {
		return "", 0, err
	}
	line, err := readOptionalInt(r)
	if err != nil {
		return "", 0, err
	}
	return file, line, nil
}

// readOptionalBinary reads a binary, or skips a term of any other type and
// returns "".
func readOptionalBinary(r *etfReader) (string, error) {
	if tag, err := r.peekTag(); err != nil {
		return "", err
	} else if tag != tagBinary {
		return "", r.skip()
	}
	return r.readBinary()
}

// readOptionalInt reads a small or 32-bit integer, or skips a term of any other
// type and returns 0.
func readOptionalInt(r *etfReader) (int, error) {
	if tag, err := r.peekTag(); err != nil {
		return 0, err
	} else if tag != tagSmallInteger && tag != tagInteger {
		return 0, r.skip()
	}
	return r.readInt()
}
