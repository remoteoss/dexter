package beam

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/remoteoss/dexter/internal/parser"
)

// CompiledFunction is one function recovered from expanded Elixir debug data.
type CompiledFunction struct {
	Function    parser.FunctionID
	Kind        string
	Fingerprint [sha256.Size]byte
}

// CompiledUnresolved preserves one call whose target cannot be proved.
type CompiledUnresolved struct {
	Caller parser.FunctionID
	Kind   string
	Line   int
}

// CompiledEvidence is the bounded static evidence held by one BEAM.
type CompiledEvidence struct {
	Module     string
	Source     string
	Functions  []CompiledFunction
	Edges      []parser.CallEdge
	Unresolved []CompiledUnresolved
	Behaviours []string
	Callbacks  []parser.FunctionID
	Resources  []string
	Digest     [sha256.Size]byte
}

// ReadCompiledEvidence reads expanded definitions and static calls directly
// from the Dbgi ETF term. It does not start an Erlang VM.
func ReadCompiledEvidence(path string) (out CompiledEvidence, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = CompiledEvidence{}
			err = fmt.Errorf("invalid Dbgi term: %v", recovered)
		}
	}()
	chunk, err := readChunk(path, "Dbgi")
	if err != nil {
		return CompiledEvidence{}, err
	}
	inflated, err := inflateDocsTerm(chunk)
	if err != nil {
		return CompiledEvidence{}, fmt.Errorf("decode Dbgi chunk: %w", err)
	}
	reader := &etfReader{buf: inflated}
	term, err := reader.decode()
	if err != nil {
		return CompiledEvidence{}, err
	}
	if reader.remaining() != 0 {
		return CompiledEvidence{}, fmt.Errorf("trailing Dbgi bytes: %d", reader.remaining())
	}
	evidence, err := parseCompiledDebugTerm(term)
	if err != nil {
		return CompiledEvidence{}, err
	}
	evidence.Digest = sha256.Sum256(chunk)
	return evidence, nil
}

// ReadOpaqueEvidence retains callable exports for Erlang, native, or stripped
// modules. Every export receives the whole-BEAM digest, so any binary change
// conservatively changes every opaque function.
func ReadOpaqueEvidence(path string) (CompiledEvidence, error) {
	exports, err := readAllExports(path)
	if err != nil {
		return CompiledEvidence{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return CompiledEvidence{}, err
	}
	fingerprint := sha256.Sum256(data)
	module := strings.TrimSuffix(filepath.Base(path), ".beam")
	module = moduleName(module)
	functions := make([]CompiledFunction, 0, len(exports))
	metadata := parser.FunctionID{Module: module, Function: "__module_metadata__", Arity: 0}
	edges := make([]parser.CallEdge, 0, len(exports))
	for _, function := range exports {
		identity := parser.FunctionID{Module: module, Function: function.Name, Arity: function.Arity}
		functions = append(functions, CompiledFunction{
			Function: identity,
			Kind:     function.Kind, Fingerprint: fingerprint,
		})
		edges = append(edges, parser.CallEdge{Caller: identity, Callee: metadata, Kind: "opaque"})
	}
	return CompiledEvidence{
		Module: module, Functions: functions, Edges: edges,
		Unresolved: []CompiledUnresolved{{Caller: metadata, Kind: "opaque_beam"}}, Digest: fingerprint,
	}, nil
}

func parseCompiledDebugTerm(term interface{}) (CompiledEvidence, error) {
	outer, ok := term.(etfTuple)
	if !ok || len(outer) != 3 || atomString(outer[0]) != "debug_info_v1" || atomString(outer[1]) != "elixir_erl" {
		return CompiledEvidence{}, fmt.Errorf("unsupported Dbgi envelope")
	}
	payload, ok := outer[2].(etfTuple)
	if !ok || len(payload) != 3 || atomString(payload[0]) != "elixir_v1" {
		return CompiledEvidence{}, fmt.Errorf("unsupported Elixir debug payload")
	}
	info, ok := payload[1].(etfMapValue)
	if !ok {
		return CompiledEvidence{}, fmt.Errorf("elixir debug info is not a map")
	}
	module := moduleName(atomString(mapAtomValue(info, "module")))
	if module == "" {
		return CompiledEvidence{}, fmt.Errorf("elixir debug info has no module")
	}
	source, _ := mapAtomValue(info, "relative_file").(string)
	if source == "" {
		source, _ = mapAtomValue(info, "file").(string)
	}
	definitions, ok := properList(mapAtomValue(info, "definitions"))
	if !ok {
		return CompiledEvidence{}, fmt.Errorf("elixir debug definitions are not a list")
	}
	behaviours := compiledBehaviours(mapAtomValue(info, "attributes"))
	callbacks := compiledCallbacks(payload[2], module)
	resources := compiledResources(mapAtomValue(info, "attributes"))
	locals := make(map[parser.FunctionID]struct{}, len(definitions))
	for _, raw := range definitions {
		function, _, _, ok := compiledDefinition(raw, module)
		if ok {
			locals[function] = struct{}{}
		}
	}
	edges := make(map[parser.CallEdge]struct{})
	unresolved := make(map[CompiledUnresolved]struct{})
	functions := make([]CompiledFunction, 0, len(definitions))
	for _, raw := range definitions {
		function, kind, clauses, ok := compiledDefinition(raw, module)
		if !ok {
			continue
		}
		fingerprint := fingerprintCompiledDefinition(kind, clauses)
		functions = append(functions, CompiledFunction{Function: function, Kind: kind, Fingerprint: fingerprint})
		if clauseList, ok := properList(clauses); ok {
			for _, rawClause := range clauseList {
				clause, ok := rawClause.(etfTuple)
				if !ok || len(clause) != 4 {
					continue
				}
				state := compiledWalkState{caller: function, locals: locals, edges: edges, unresolved: unresolved, bindings: make(map[string][]compiledValue)}
				state.walk(clause[1])
				state.walk(clause[2])
				state.walk(clause[3])
			}
		}
	}
	functions = append(functions, CompiledFunction{
		Function: parser.FunctionID{Module: module, Function: "__module_metadata__", Arity: 0},
		Kind:     "compiled_metadata", Fingerprint: fingerprintCompiledMetadata(info, functions),
	})
	sort.Slice(functions, func(i, j int) bool { return functionIDLess(functions[i].Function, functions[j].Function) })
	edgeList := make([]parser.CallEdge, 0, len(edges))
	for edge := range edges {
		edgeList = append(edgeList, edge)
	}
	sort.Slice(edgeList, func(i, j int) bool {
		a, b := edgeList[i], edgeList[j]
		if a.Caller != b.Caller {
			return functionIDLess(a.Caller, b.Caller)
		}
		if a.Callee != b.Callee {
			return functionIDLess(a.Callee, b.Callee)
		}
		return a.Kind < b.Kind
	})
	unresolvedList := make([]CompiledUnresolved, 0, len(unresolved))
	for record := range unresolved {
		unresolvedList = append(unresolvedList, record)
	}
	sort.Slice(unresolvedList, func(i, j int) bool {
		if unresolvedList[i].Caller != unresolvedList[j].Caller {
			return functionIDLess(unresolvedList[i].Caller, unresolvedList[j].Caller)
		}
		if unresolvedList[i].Kind != unresolvedList[j].Kind {
			return unresolvedList[i].Kind < unresolvedList[j].Kind
		}
		return unresolvedList[i].Line < unresolvedList[j].Line
	})
	return CompiledEvidence{
		Module: module, Source: source, Functions: functions, Edges: edgeList,
		Unresolved: unresolvedList, Behaviours: behaviours, Callbacks: callbacks, Resources: resources,
	}, nil
}

func compiledResources(value interface{}) []string {
	values, ok := properList(value)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	for _, value := range values {
		pair, ok := value.(etfTuple)
		if !ok || len(pair) != 2 || atomString(pair[0]) != "external_resource" {
			continue
		}
		collectCompiledResourcePaths(pair[1], seen)
	}
	result := make([]string, 0, len(seen))
	for path := range seen {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func collectCompiledResourcePaths(value interface{}, result map[string]struct{}) {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			result[typed] = struct{}{}
		}
	case etfListValue:
		for _, child := range typed.Values {
			collectCompiledResourcePaths(child, result)
		}
	}
}

func fingerprintCompiledMetadata(info etfMapValue, functions []CompiledFunction) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("dexter:compiled-module-metadata:v1\x00"))
	for _, key := range []string{"attributes", "struct", "defines_behaviour"} {
		writeHashString(h, 'k', key)
		hashNormalizedTerm(h, mapAtomValue(info, key), false)
	}
	identities := make([]parser.FunctionID, 0, len(functions))
	for _, function := range functions {
		identities = append(identities, function.Function)
	}
	sort.Slice(identities, func(i, j int) bool { return functionIDLess(identities[i], identities[j]) })
	for _, function := range identities {
		writeHashString(h, 'm', function.Module)
		writeHashString(h, 'f', function.Function)
		writeHashString(h, 'a', strconv.Itoa(function.Arity))
	}
	var result [sha256.Size]byte
	h.Sum(result[:0])
	return result
}

func compiledBehaviours(value interface{}) []string {
	values, ok := properList(value)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	for _, value := range values {
		pair, ok := value.(etfTuple)
		if !ok || len(pair) != 2 || atomString(pair[0]) != "behaviour" {
			continue
		}
		if behaviour := moduleName(atomString(pair[1])); behaviour != "" {
			seen[behaviour] = struct{}{}
		}
		if nested, ok := properList(pair[1]); ok {
			for _, item := range nested {
				if behaviour := moduleName(atomString(item)); behaviour != "" {
					seen[behaviour] = struct{}{}
				}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for behaviour := range seen {
		result = append(result, behaviour)
	}
	sort.Strings(result)
	return result
}

func compiledCallbacks(value interface{}, module string) []parser.FunctionID {
	forms, ok := properList(value)
	if !ok {
		return nil
	}
	seen := make(map[parser.FunctionID]struct{})
	for _, value := range forms {
		form, ok := value.(etfTuple)
		if !ok || len(form) != 4 || atomString(form[0]) != "attribute" || atomString(form[2]) != "callback" {
			continue
		}
		callback, ok := form[3].(etfTuple)
		if !ok || len(callback) != 2 {
			continue
		}
		nameArity, ok := callback[0].(etfTuple)
		if !ok || len(nameArity) != 2 {
			continue
		}
		name := atomString(nameArity[0])
		arity, ok := nameArity[1].(int)
		if name != "" && ok {
			seen[parser.FunctionID{Module: module, Function: name, Arity: arity}] = struct{}{}
		}
	}
	result := make([]parser.FunctionID, 0, len(seen))
	for callback := range seen {
		result = append(result, callback)
	}
	sort.Slice(result, func(i, j int) bool { return functionIDLess(result[i], result[j]) })
	return result
}

func compiledDefinition(value interface{}, module string) (parser.FunctionID, string, interface{}, bool) {
	definition, ok := value.(etfTuple)
	if !ok || len(definition) != 4 {
		return parser.FunctionID{}, "", nil, false
	}
	nameArity, ok := definition[0].(etfTuple)
	if !ok || len(nameArity) != 2 {
		return parser.FunctionID{}, "", nil, false
	}
	name := atomString(nameArity[0])
	arity, ok := nameArity[1].(int)
	if name == "" || !ok || arity < 0 {
		return parser.FunctionID{}, "", nil, false
	}
	return parser.FunctionID{Module: module, Function: name, Arity: arity}, atomString(definition[1]), definition[3], true
}

type compiledValue struct {
	kind     string
	module   string
	function string
	arity    int
}

type compiledWalkState struct {
	caller     parser.FunctionID
	locals     map[parser.FunctionID]struct{}
	edges      map[parser.CallEdge]struct{}
	unresolved map[CompiledUnresolved]struct{}
	bindings   map[string][]compiledValue
}

func (s *compiledWalkState) walk(value interface{}) {
	if tuple, ok := value.(etfTuple); ok && len(tuple) == 3 {
		name := atomString(tuple[0])
		args, argsOK := properList(tuple[2])
		if name == "=" && argsOK && len(args) == 2 {
			s.walk(args[0])
			s.walk(args[1])
			if key, ok := variableKey(args[0]); ok {
				values, _ := s.resolveMany(args[1], 16)
				s.bindings[key] = values
			}
			return
		}
		if name == "&" && argsOK && len(args) == 1 {
			if target, ok := captureTarget(args[0], s.caller.Module); ok {
				s.edge(target, "capture")
				return
			}
		}
		if name == "%" && argsOK && len(args) == 2 {
			if module := moduleName(atomString(args[0])); module != "" {
				s.edge(parser.FunctionID{Module: module, Function: "__module_metadata__", Arity: 0}, "struct")
			}
			s.walk(args[1])
			return
		}
		if name == "super" && argsOK {
			if targetName := metadataSuperName(tuple[1]); targetName != "" {
				s.edge(parser.FunctionID{Module: s.caller.Module, Function: targetName, Arity: len(args)}, "local")
			} else {
				s.uncertain("super", metadataInt(tuple[1], "line"))
			}
			s.walk(args)
			return
		}
		if dot, ok := tuple[0].(etfTuple); ok && len(dot) == 3 && atomString(dot[0]) == "." && argsOK {
			targets, targetOK := properList(dot[2])
			if targetOK && len(targets) == 2 {
				function := atomString(targets[1])
				if module := moduleName(atomString(targets[0])); module != "" && function != "" {
					if module == "erlang" && function == "apply" && len(args) == 3 {
						s.walkApply(args, metadataInt(tuple[1], "line"))
					} else {
						s.edge(parser.FunctionID{Module: module, Function: function, Arity: len(args)}, "call")
					}
				} else {
					resolved, complete := s.resolveMany(targets[0], 16)
					matched := false
					for _, target := range resolved {
						if target.kind == "atom" && function != "" {
							s.edge(parser.FunctionID{Module: target.module, Function: function, Arity: len(args)}, "inferred_call")
							matched = true
						}
					}
					if !matched || !complete {
						s.uncertain("dynamic_call", metadataInt(tuple[1], "line"))
					}
				}
				if atomString(targets[0]) == "" {
					s.walk(targets[0])
				}
				s.walk(args)
				return
			}
			if targetOK && len(targets) == 1 {
				resolved, complete := s.resolveMany(targets[0], 16)
				matched := false
				for _, target := range resolved {
					if target.kind == "capture" && target.arity == len(args) {
						s.edge(parser.FunctionID{Module: target.module, Function: target.function, Arity: target.arity}, "inferred_capture")
						matched = true
					}
				}
				if !matched || !complete {
					s.uncertain("anonymous_call", metadataInt(tuple[1], "line"))
				}
				s.walk(targets[0])
				s.walk(args)
				return
			}
		}
		if name != "" && argsOK {
			callee := parser.FunctionID{Module: s.caller.Module, Function: name, Arity: len(args)}
			if _, exists := s.locals[callee]; exists {
				s.edge(callee, "local")
			}
			s.walk(args)
			return
		}
		if _, variable := variableKey(tuple); variable {
			return
		}
	}
	switch typed := value.(type) {
	case etfOpaque:
		s.uncertain("unsupported_term", 0)
	case etfAtom:
		if module := moduleName(string(typed)); strings.HasPrefix(string(typed), "Elixir.") && module != "" {
			s.edge(parser.FunctionID{Module: module, Function: "__module_metadata__", Arity: 0}, "module_value")
		}
	case etfTuple:
		for _, child := range typed {
			s.walk(child)
		}
	case etfListValue:
		s.walk(typed.Values)
		if !emptyList(typed.Tail) {
			s.walk(typed.Tail)
		}
	case []interface{}:
		for _, child := range typed {
			s.walk(child)
		}
	case etfMapValue:
		for _, pair := range typed {
			s.walk(pair.Key)
			s.walk(pair.Value)
		}
	}
}

func (s *compiledWalkState) walkApply(args []interface{}, line int) {
	modules, modulesComplete := s.resolveMany(args[0], 16)
	functions, functionsComplete := s.resolveMany(args[1], 16)
	lists, listsComplete := s.resolveMany(args[2], 16)
	matched := false
	for _, module := range modules {
		for _, function := range functions {
			for _, list := range lists {
				if module.kind == "atom" && function.kind == "atom" && list.kind == "list" {
					s.edge(parser.FunctionID{Module: module.module, Function: function.function, Arity: list.arity}, "apply")
					matched = true
				}
			}
		}
	}
	if !matched || !modulesComplete || !functionsComplete || !listsComplete {
		s.uncertain("apply", line)
	}
}

func (s *compiledWalkState) resolveMany(value interface{}, limit int) ([]compiledValue, bool) {
	if limit <= 0 {
		return nil, false
	}
	if atom := atomString(value); atom != "" {
		return []compiledValue{{kind: "atom", module: moduleName(atom), function: atom}}, true
	}
	if key, ok := variableKey(value); ok {
		values, exists := s.bindings[key]
		return values, exists
	}
	if values, ok := properList(value); ok {
		return []compiledValue{{kind: "list", arity: len(values)}}, true
	}
	if tuple, ok := value.(etfTuple); ok && len(tuple) == 3 && atomString(tuple[0]) == "&" {
		if args, ok := properList(tuple[2]); ok && len(args) == 1 {
			if target, ok := captureTarget(args[0], s.caller.Module); ok {
				return []compiledValue{{kind: "capture", module: target.Module, function: target.Function, arity: target.Arity}}, true
			}
		}
	}
	if tuple, ok := value.(etfTuple); ok && len(tuple) == 3 && (atomString(tuple[0]) == "%" || atomString(tuple[0]) == "%{}") {
		return []compiledValue{{kind: "map"}}, true
	}
	if tuple, ok := value.(etfTuple); ok && len(tuple) == 3 {
		name := atomString(tuple[0])
		args, argsOK := properList(tuple[2])
		if argsOK && name == "__block__" && len(args) > 0 {
			return s.resolveMany(args[len(args)-1], limit)
		}
		if argsOK && (name == "case" || name == "cond" || name == "if") {
			var bodies []interface{}
			collectArrowBodies(tuple[2], &bodies)
			result := make([]compiledValue, 0, len(bodies))
			complete := len(bodies) > 0
			seen := make(map[compiledValue]struct{})
			for _, body := range bodies {
				values, bodyComplete := s.resolveMany(body, limit-len(result))
				complete = complete && bodyComplete
				for _, candidate := range values {
					if _, duplicate := seen[candidate]; duplicate {
						continue
					}
					seen[candidate] = struct{}{}
					result = append(result, candidate)
					if len(result) >= limit {
						return result, false
					}
				}
			}
			return result, complete
		}
	}
	return nil, false
}

func collectArrowBodies(value interface{}, bodies *[]interface{}) {
	if tuple, ok := value.(etfTuple); ok && len(tuple) == 3 && atomString(tuple[0]) == "->" {
		if args, ok := properList(tuple[2]); ok && len(args) == 2 {
			*bodies = append(*bodies, args[1])
			return
		}
	}
	switch typed := value.(type) {
	case etfTuple:
		for _, child := range typed {
			collectArrowBodies(child, bodies)
		}
	case etfListValue:
		for _, child := range typed.Values {
			collectArrowBodies(child, bodies)
		}
	case etfMapValue:
		for _, pair := range typed {
			collectArrowBodies(pair.Value, bodies)
		}
	}
}

func (s *compiledWalkState) edge(callee parser.FunctionID, kind string) {
	s.edges[parser.CallEdge{Caller: s.caller, Callee: callee, Kind: kind}] = struct{}{}
}

func (s *compiledWalkState) uncertain(kind string, line int) {
	s.unresolved[CompiledUnresolved{Caller: s.caller, Kind: kind, Line: line}] = struct{}{}
}

func variableKey(value interface{}) (string, bool) {
	tuple, ok := value.(etfTuple)
	if !ok || len(tuple) != 3 || atomString(tuple[0]) == "" {
		return "", false
	}
	if _, args := properList(tuple[2]); args {
		return "", false
	}
	context := atomString(tuple[2])
	if context == "" {
		return "", false
	}
	version := metadataInt(tuple[1], "version")
	return atomString(tuple[0]) + "\x00" + context + "\x00" + strconv.Itoa(version), true
}

func captureTarget(value interface{}, localModule string) (parser.FunctionID, bool) {
	slash, ok := value.(etfTuple)
	if !ok || len(slash) != 3 || atomString(slash[0]) != "/" {
		return parser.FunctionID{}, false
	}
	args, ok := properList(slash[2])
	if !ok || len(args) != 2 {
		return parser.FunctionID{}, false
	}
	arity, ok := args[1].(int)
	if !ok {
		return parser.FunctionID{}, false
	}
	call, ok := args[0].(etfTuple)
	if !ok || len(call) != 3 {
		return parser.FunctionID{}, false
	}
	if dot, ok := call[0].(etfTuple); ok && len(dot) == 3 && atomString(dot[0]) == "." {
		targets, ok := properList(dot[2])
		if ok && len(targets) == 2 {
			module, function := moduleName(atomString(targets[0])), atomString(targets[1])
			return parser.FunctionID{Module: module, Function: function, Arity: arity}, module != "" && function != ""
		}
	}
	function := atomString(call[0])
	return parser.FunctionID{Module: localModule, Function: function, Arity: arity}, function != ""
}

func metadataInt(value interface{}, key string) int {
	values, ok := properList(value)
	if !ok {
		return 0
	}
	for _, value := range values {
		pair, ok := value.(etfTuple)
		if ok && len(pair) == 2 && atomString(pair[0]) == key {
			result, _ := pair[1].(int)
			return result
		}
	}
	return 0
}

func metadataSuperName(value interface{}) string {
	values, ok := properList(value)
	if !ok {
		return ""
	}
	for _, value := range values {
		pair, ok := value.(etfTuple)
		if !ok || len(pair) != 2 || atomString(pair[0]) != "super" {
			continue
		}
		super, ok := pair[1].(etfTuple)
		if ok && len(super) == 2 {
			return atomString(super[1])
		}
	}
	return ""
}

func fingerprintCompiledDefinition(kind string, clauses interface{}) [sha256.Size]byte {
	h := sha256.New()
	_, _ = h.Write([]byte("dexter:compiled-function:v1\x00" + kind + "\x00"))
	hashNormalizedTerm(h, clauses, false)
	var result [sha256.Size]byte
	h.Sum(result[:0])
	return result
}

var positionMetadata = map[string]struct{}{
	"line": {}, "column": {}, "closing": {}, "do": {}, "end": {}, "end_of_expression": {}, "file": {}, "keep": {},
}

func hashNormalizedTerm(h hash.Hash, value interface{}, metadata bool) {
	switch typed := value.(type) {
	case nil:
		_, _ = h.Write([]byte{'n'})
	case etfAtom:
		writeHashString(h, 'a', string(typed))
	case string:
		writeHashString(h, 's', typed)
	case int:
		writeHashString(h, 'i', strconv.Itoa(typed))
	case float64:
		writeHashString(h, 'f', strconv.FormatFloat(typed, 'g', -1, 64))
	case []byte:
		_, _ = h.Write([]byte{'b'})
		_, _ = h.Write(typed)
	case etfTuple:
		_, _ = h.Write([]byte{'t'})
		for i, child := range typed {
			hashNormalizedTerm(h, child, len(typed) == 3 && i == 1)
		}
	case etfListValue:
		_, _ = h.Write([]byte{'l'})
		for _, child := range typed.Values {
			if metadata {
				if pair, ok := child.(etfTuple); ok && len(pair) == 2 {
					if _, skip := positionMetadata[atomString(pair[0])]; skip {
						continue
					}
				}
			}
			hashNormalizedTerm(h, child, false)
		}
		hashNormalizedTerm(h, typed.Tail, false)
	case etfMapValue:
		_, _ = h.Write([]byte{'m'})
		for _, pair := range typed {
			hashNormalizedTerm(h, pair.Key, false)
			hashNormalizedTerm(h, pair.Value, false)
		}
	default:
		writeHashString(h, 'u', fmt.Sprint(typed))
	}
}

func writeHashString(h hash.Hash, marker byte, value string) {
	_, _ = h.Write([]byte{marker})
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(value))
}

func mapAtomValue(value etfMapValue, key string) interface{} {
	for _, pair := range value {
		if atomString(pair.Key) == key {
			return pair.Value
		}
	}
	return nil
}

func properList(value interface{}) ([]interface{}, bool) {
	list, ok := value.(etfListValue)
	return list.Values, ok && (list.Tail == nil || emptyList(list.Tail))
}

func emptyList(value interface{}) bool {
	list, ok := value.(etfListValue)
	return ok && len(list.Values) == 0 && list.Tail == nil
}

func atomString(value interface{}) string {
	atom, _ := value.(etfAtom)
	return string(atom)
}

func moduleName(value string) string {
	return strings.TrimPrefix(value, "Elixir.")
}

func functionIDLess(a, b parser.FunctionID) bool {
	if a.Module != b.Module {
		return a.Module < b.Module
	}
	if a.Function != b.Function {
		return a.Function < b.Function
	}
	return a.Arity < b.Arity
}
