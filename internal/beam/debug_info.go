package beam

import (
	"errors"
	"fmt"
	"slices"
)

// maxDebugInfoDepth bounds recursion while stepping over clause bodies. Each
// level of Elixir AST costs two ETF levels (the call tuple and its argument
// list), so this allows a thousand nested expressions: far past any real code,
// and still a bounded Go stack on a corrupt file.
const maxDebugInfoDepth = 2048

// maxPreallocatedSites bounds the definitions slice allocated up front.
const maxPreallocatedSites = 1024

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

	// ModuleLine is the line of the module itself in File: its defmodule, or
	// wherever Module.create was called. It is the map's anno, or its line
	// before Elixir 1.18. Zero if the chunk has none.
	ModuleLine int

	// Lines maps each public function and macro to its line in File. Entries
	// without a usable line are absent.
	Lines map[FunctionKey]int

	// Clauses holds the line of each clause for a public definition whose
	// clauses were made at more than one line, in clause order: a DSL that
	// adds one clause per call, such as `route :get, "/a"`. Definitions with
	// one line are absent; Lines has it.
	Clauses map[FunctionKey][]int
}

// ReadDefinitionLines reads the line of every public definition from an Elixir
// BEAM's debug info, for functions that have no source definition to index
// because a macro generated them.
//
// A generated def carries two locations. Its :line is the line in the module's
// own file that was being compiled when the def was produced: the macro call,
// or wherever a before-compile hook ran. A `file: {path, line}` entry, left by
// `@file` or by `quote location: :keep`, names a more specific place. When that
// path is the module's own source and the line is after the module's own, it
// is where the code asked for the function. Otherwise it is the generator's own
// implementation, in another file or in a macro defined above the module, which
// is not what a reader of the module is looking for, so :line is used instead.
//
// Everything here is standard compiler output. No framework is recognized by
// name: a generator that wants its functions to navigate to the line that
// declared them either expands each def at that line, as Ash's code interfaces
// do, or stamps it with `@file {file, line}`.
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
	key         FunctionKey
	line        int
	keepFile    string
	keepLine    int
	clauseLines []int
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
	var legacyModuleLine int
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
		case "anno":
			if info.ModuleLine, err = readAnnoLine(r); err != nil {
				return DebugInfo{}, err
			}
		case "line":
			// Elixir 1.17 and earlier record the module's line here, as a
			// bare line, and have no anno.
			if legacyModuleLine, err = readAnnoLine(r); err != nil {
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

	if info.ModuleLine == 0 {
		info.ModuleLine = legacyModuleLine
	}

	info.Lines = make(map[FunctionKey]int, len(sites))
	for _, site := range sites {
		line := site.line
		// A location at or before the module's own line cannot be where the
		// module asked for the function. It is a `location: :keep` quote in a
		// macro defined above the module in the same file, so :line, the
		// call, is used.
		stamped := site.keepFile != "" && site.keepLine > 0 &&
			(site.keepFile == info.RelativeFile || site.keepFile == info.File) &&
			(info.ModuleLine == 0 || site.keepLine > info.ModuleLine)
		if stamped {
			line = site.keepLine
		}
		if line > 0 {
			info.Lines[site.key] = line
		}
		// A stamped def names its own line; its clauses' lines are the
		// generator's, so only an unstamped def's clauses are kept.
		if !stamped && len(site.clauseLines) > 1 {
			if info.Clauses == nil {
				info.Clauses = make(map[FunctionKey][]int)
			}
			info.Clauses[site.key] = site.clauseLines
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
	// A corrupt count is bounded only by the chunk size, which can still ask
	// for gigabytes of sites before the walk fails. Real modules have far
	// fewer definitions, so the slice grows past this if it must.
	sites := make([]definitionSite, 0, min(count, maxPreallocatedSites))
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
		if site.clauseLines, err = readClauseLines(r); err != nil {
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

// readClauseLines reads the distinct :line of each {meta, args, guards, body}
// clause, in order, and steps over everything else without allocating. A value
// that is not a list is skipped.
func readClauseLines(r *etfReader) ([]int, error) {
	if tag, err := r.peekTag(); err != nil {
		return nil, err
	} else if tag != tagList && tag != tagNil {
		return nil, r.skip()
	}
	count, hasTail, err := r.enterList()
	if err != nil {
		return nil, err
	}
	var lines []int
	for i := int64(0); i < count; i++ {
		if tag, err := r.peekTag(); err != nil {
			return nil, err
		} else if tag != tagSmallTuple {
			if err := r.skip(); err != nil {
				return nil, err
			}
			continue
		}
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
		var clause definitionSite
		if err := readDefinitionMeta(r, &clause); err != nil {
			return nil, err
		}
		if err := r.skipTerms(3); err != nil { // args, guards, body
			return nil, err
		}
		if clause.line > 0 && !slices.Contains(lines, clause.line) {
			lines = append(lines, clause.line)
		}
	}
	if hasTail {
		if err := r.skip(); err != nil {
			return nil, err
		}
	}
	return lines, nil
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
