// Package evidence defines revision-matched graph evidence supplied by a
// repository or an external provider. It contains no project-specific names.
package evidence

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/remoteoss/dexter/internal/parser"
)

const (
	SchemaVersion        = 1
	RepositoryConfigName = "dexter-impact.json"
	maxEvidenceBytes     = 1 << 30
)

type Edge struct {
	Caller parser.FunctionID `json:"caller"`
	Callee parser.FunctionID `json:"callee"`
	Kind   string            `json:"kind"`
}

type Function struct {
	Function    parser.FunctionID `json:"function"`
	File        string            `json:"file"`
	Fingerprint string            `json:"fingerprint"`
}

type TestOwnership struct {
	Root     parser.FunctionID `json:"root"`
	Function parser.FunctionID `json:"function"`
	File     string            `json:"file"`
}

type Unresolved struct {
	Caller parser.FunctionID `json:"caller"`
	Kind   string            `json:"kind"`
	Detail string            `json:"detail,omitempty"`
}

type Artifact struct {
	SchemaVersion        int                 `json:"schema_version"`
	Provider             string              `json:"provider"`
	Revision             string              `json:"revision"`
	Provenance           map[string]string   `json:"provenance,omitempty"`
	Functions            []Function          `json:"functions,omitempty"`
	Edges                []Edge              `json:"edges,omitempty"`
	TestOwnership        []TestOwnership     `json:"test_ownership,omitempty"`
	Unresolved           []Unresolved        `json:"unresolved,omitempty"`
	CompactFunctions     []compactFunction   `json:"compact_functions,omitempty"`
	CompactEdges         []compactEdge       `json:"compact_edges,omitempty"`
	CompactTestOwnership []compactOwnership  `json:"compact_test_ownership,omitempty"`
	CompactUnresolved    []compactUnresolved `json:"compact_unresolved,omitempty"`
}

type compactFunction struct{ Function Function }
type compactEdge struct{ Edge Edge }
type compactOwnership struct{ Ownership TestOwnership }
type compactUnresolved struct{ Unresolved Unresolved }

func (value *compactFunction) UnmarshalJSON(data []byte) error {
	var tuple []json.RawMessage
	if err := json.Unmarshal(data, &tuple); err != nil || len(tuple) != 5 {
		return errors.New("compact function must have 5 fields")
	}
	return decodeCompactFunction(tuple, &value.Function)
}

func decodeCompactFunction(tuple []json.RawMessage, value *Function) error {
	if err := decodeFunctionID(tuple[:3], &value.Function); err != nil {
		return err
	}
	if err := json.Unmarshal(tuple[3], &value.File); err != nil {
		return err
	}
	return json.Unmarshal(tuple[4], &value.Fingerprint)
}

func (value *compactEdge) UnmarshalJSON(data []byte) error {
	var tuple []json.RawMessage
	if err := json.Unmarshal(data, &tuple); err != nil || len(tuple) != 7 {
		return errors.New("compact edge must have 7 fields")
	}
	if err := decodeFunctionID(tuple[:3], &value.Edge.Caller); err != nil {
		return err
	}
	if err := decodeFunctionID(tuple[3:6], &value.Edge.Callee); err != nil {
		return err
	}
	return json.Unmarshal(tuple[6], &value.Edge.Kind)
}

func (value *compactOwnership) UnmarshalJSON(data []byte) error {
	var tuple []json.RawMessage
	if err := json.Unmarshal(data, &tuple); err != nil || len(tuple) != 7 {
		return errors.New("compact test ownership must have 7 fields")
	}
	if err := decodeFunctionID(tuple[:3], &value.Ownership.Root); err != nil {
		return err
	}
	if err := decodeFunctionID(tuple[3:6], &value.Ownership.Function); err != nil {
		return err
	}
	return json.Unmarshal(tuple[6], &value.Ownership.File)
}

func (value *compactUnresolved) UnmarshalJSON(data []byte) error {
	var tuple []json.RawMessage
	if err := json.Unmarshal(data, &tuple); err != nil || len(tuple) != 5 {
		return errors.New("compact unresolved record must have 5 fields")
	}
	if err := decodeFunctionID(tuple[:3], &value.Unresolved.Caller); err != nil {
		return err
	}
	if err := json.Unmarshal(tuple[3], &value.Unresolved.Kind); err != nil {
		return err
	}
	return json.Unmarshal(tuple[4], &value.Unresolved.Detail)
}

func decodeFunctionID(tuple []json.RawMessage, function *parser.FunctionID) error {
	if len(tuple) != 3 {
		return errors.New("function identity must have 3 fields")
	}
	if err := json.Unmarshal(tuple[0], &function.Module); err != nil {
		return err
	}
	if err := json.Unmarshal(tuple[1], &function.Function); err != nil {
		return err
	}
	return json.Unmarshal(tuple[2], &function.Arity)
}

type LoadedArtifact struct {
	Artifact Artifact
	Digest   string
}

type RepositoryConfig struct {
	SchemaVersion        int                 `json:"schema_version"`
	RequiredProviders    []string            `json:"required_providers,omitempty"`
	Mappings             []Edge              `json:"mappings,omitempty"`
	CompiledBuildRoots   []string            `json:"compiled_build_roots,omitempty"`
	CompiledApplications map[string]string   `json:"compiled_applications,omitempty"`
	BehaviourCallbacks   []BehaviourCallback `json:"behaviour_callbacks,omitempty"`
}

type BehaviourCallback struct {
	Behaviour string `json:"behaviour"`
	Function  string `json:"function"`
	Arity     int    `json:"arity"`
}

func LoadRepositoryConfig(root string) (RepositoryConfig, string, error) {
	path := filepath.Join(root, RepositoryConfigName)
	data, err := readBounded(path)
	if errors.Is(err, os.ErrNotExist) {
		return RepositoryConfig{SchemaVersion: SchemaVersion}, "", nil
	}
	if err != nil {
		return RepositoryConfig{}, "", err
	}
	var config RepositoryConfig
	if err := decodeStrict(data, &config); err != nil {
		return RepositoryConfig{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if config.SchemaVersion != SchemaVersion {
		return RepositoryConfig{}, "", fmt.Errorf("repository evidence schema %d, want %d", config.SchemaVersion, SchemaVersion)
	}
	seen := make(map[string]struct{}, len(config.RequiredProviders))
	for _, provider := range config.RequiredProviders {
		if !validProvider(provider) {
			return RepositoryConfig{}, "", fmt.Errorf("invalid required provider %q", provider)
		}
		if _, duplicate := seen[provider]; duplicate {
			return RepositoryConfig{}, "", fmt.Errorf("duplicate required provider %q", provider)
		}
		seen[provider] = struct{}{}
	}
	for i := range config.Mappings {
		if err := validateEdge(config.Mappings[i]); err != nil {
			return RepositoryConfig{}, "", fmt.Errorf("mapping %d: %w", i, err)
		}
	}
	for i, root := range config.CompiledBuildRoots {
		clean, err := relativePath(root)
		if err != nil {
			return RepositoryConfig{}, "", fmt.Errorf("compiled build root %d: %w", i, err)
		}
		config.CompiledBuildRoots[i] = clean
	}
	for app, root := range config.CompiledApplications {
		if !validProvider(app) {
			return RepositoryConfig{}, "", fmt.Errorf("invalid compiled application %q", app)
		}
		clean, err := relativePath(root)
		if err != nil {
			return RepositoryConfig{}, "", fmt.Errorf("compiled application %s: %w", app, err)
		}
		config.CompiledApplications[app] = clean
	}
	for i, callback := range config.BehaviourCallbacks {
		if strings.TrimSpace(callback.Behaviour) == "" || strings.TrimSpace(callback.Function) == "" || callback.Arity < 0 {
			return RepositoryConfig{}, "", fmt.Errorf("invalid behaviour callback %d", i)
		}
	}
	sort.Strings(config.RequiredProviders)
	return config, digest(data), nil
}

func LoadArtifact(path, revision string) (Artifact, string, error) {
	data, err := readBounded(path)
	if err != nil {
		return Artifact{}, "", err
	}
	var artifact Artifact
	if err := decodeStrict(data, &artifact); err != nil {
		return Artifact{}, "", fmt.Errorf("decode %s: %w", path, err)
	}
	if artifact.SchemaVersion != SchemaVersion {
		return Artifact{}, "", fmt.Errorf("provider evidence schema %d, want %d", artifact.SchemaVersion, SchemaVersion)
	}
	if !validProvider(artifact.Provider) {
		return Artifact{}, "", fmt.Errorf("invalid provider %q", artifact.Provider)
	}
	if artifact.Provider == "compiled" || artifact.Provider == "compiled_tests" || artifact.Provider == "source_parse" {
		return Artifact{}, "", fmt.Errorf("provider name %q is reserved by Dexter", artifact.Provider)
	}
	if artifact.Revision != revision {
		return Artifact{}, "", fmt.Errorf("provider %s revision %q, want %q", artifact.Provider, artifact.Revision, revision)
	}
	for _, function := range artifact.CompactFunctions {
		artifact.Functions = append(artifact.Functions, function.Function)
	}
	for _, edge := range artifact.CompactEdges {
		artifact.Edges = append(artifact.Edges, edge.Edge)
	}
	for _, ownership := range artifact.CompactTestOwnership {
		artifact.TestOwnership = append(artifact.TestOwnership, ownership.Ownership)
	}
	for _, unresolved := range artifact.CompactUnresolved {
		artifact.Unresolved = append(artifact.Unresolved, unresolved.Unresolved)
	}
	artifact.CompactFunctions = nil
	artifact.CompactEdges = nil
	artifact.CompactTestOwnership = nil
	artifact.CompactUnresolved = nil
	for i, function := range artifact.Functions {
		if err := validateFunctionID(function.Function); err != nil {
			return Artifact{}, "", fmt.Errorf("function %d: %w", i, err)
		}
		if _, err := relativePath(function.File); err != nil {
			return Artifact{}, "", fmt.Errorf("function %d: %w", i, err)
		}
		fingerprint, err := hex.DecodeString(function.Fingerprint)
		if err != nil || len(fingerprint) != sha256.Size {
			return Artifact{}, "", fmt.Errorf("function %d has invalid fingerprint", i)
		}
	}
	for i, edge := range artifact.Edges {
		if err := validateEdge(edge); err != nil {
			return Artifact{}, "", fmt.Errorf("edge %d: %w", i, err)
		}
	}
	for i, ownership := range artifact.TestOwnership {
		if err := validateFunctionID(ownership.Root); err != nil {
			return Artifact{}, "", fmt.Errorf("test ownership %d root: %w", i, err)
		}
		if err := validateFunctionID(ownership.Function); err != nil {
			return Artifact{}, "", fmt.Errorf("test ownership %d function: %w", i, err)
		}
		if _, err := relativePath(ownership.File); err != nil {
			return Artifact{}, "", fmt.Errorf("test ownership %d: %w", i, err)
		}
	}
	for i, unresolved := range artifact.Unresolved {
		if err := validateFunctionID(unresolved.Caller); err != nil {
			return Artifact{}, "", fmt.Errorf("unresolved %d: %w", i, err)
		}
		if strings.TrimSpace(unresolved.Kind) == "" {
			return Artifact{}, "", fmt.Errorf("unresolved %d has empty kind", i)
		}
	}
	return artifact, digest(data), nil
}

func LoadArtifacts(paths []string, revision string) ([]LoadedArtifact, error) {
	loaded := make([]LoadedArtifact, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		artifact, artifactDigest, err := LoadArtifact(path, revision)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[artifact.Provider]; duplicate {
			return nil, fmt.Errorf("duplicate evidence provider %q", artifact.Provider)
		}
		seen[artifact.Provider] = struct{}{}
		loaded = append(loaded, LoadedArtifact{Artifact: artifact, Digest: artifactDigest})
	}
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].Artifact.Provider < loaded[j].Artifact.Provider })
	return loaded, nil
}

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var reader io.Reader = f
	var header [2]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if header == [2]byte{0x1f, 0x8b} {
		compressed, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer func() { _ = compressed.Close() }()
		reader = compressed
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxEvidenceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxEvidenceBytes {
		return nil, fmt.Errorf("evidence exceeds %d bytes", maxEvidenceBytes)
	}
	return data, nil
}

func decodeStrict(data []byte, target interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("multiple JSON values")
	}
	return nil
}

func validateEdge(edge Edge) error {
	if err := validateFunctionID(edge.Caller); err != nil {
		return fmt.Errorf("caller: %w", err)
	}
	if err := validateFunctionID(edge.Callee); err != nil {
		return fmt.Errorf("callee: %w", err)
	}
	if strings.TrimSpace(edge.Kind) == "" {
		return errors.New("empty edge kind")
	}
	return nil
}

func validateFunctionID(function parser.FunctionID) error {
	if strings.TrimSpace(function.Module) == "" || strings.TrimSpace(function.Function) == "" {
		return errors.New("module and function are required")
	}
	if function.Arity < parser.UnknownArity {
		return fmt.Errorf("invalid arity %d", function.Arity)
	}
	return nil
}

func relativePath(path string) (string, error) {
	path = filepath.ToSlash(path)
	if path == "" || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
		return "", fmt.Errorf("unsafe relative path %q", path)
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe relative path %q", path)
	}
	return clean, nil
}

func validProvider(provider string) bool {
	if provider == "" {
		return false
	}
	for _, r := range provider {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
