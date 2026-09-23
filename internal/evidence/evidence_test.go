package evidence

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
)

func TestLoadRepositoryConfig(t *testing.T) {
	root := t.TempDir()
	contents := `{
  "schema_version": 1,
  "required_providers": ["compiled", "framework_hooks"],
  "behaviour_callbacks": [{"behaviour":"SharedLib.Worker","function":"perform","arity":1}],
  "mappings": [{
    "caller": {"module": "MyApp.Events", "function": "dispatch", "arity": 1},
    "callee": {"module": "SharedLib.Consumer", "function": "handle", "arity": 1},
    "kind": "hook"
  }]
}`
	if err := os.WriteFile(filepath.Join(root, RepositoryConfigName), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	config, digest, err := LoadRepositoryConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" || len(config.RequiredProviders) != 2 || len(config.Mappings) != 1 || len(config.BehaviourCallbacks) != 1 {
		t.Fatalf("config = %+v, digest = %q", config, digest)
	}
	want := parser.FunctionID{Module: "SharedLib.Consumer", Function: "handle", Arity: 1}
	if config.Mappings[0].Callee != want {
		t.Fatalf("callee = %+v, want %+v", config.Mappings[0].Callee, want)
	}
}

func TestLoadRepositoryConfigMissingIsEmpty(t *testing.T) {
	config, digest, err := LoadRepositoryConfig(t.TempDir())
	if err != nil || digest != "" || config.SchemaVersion != SchemaVersion {
		t.Fatalf("config = %+v, digest = %q, err = %v", config, digest, err)
	}
}

func TestLoadArtifactValidatesRevisionAndRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compiled.json")
	contents := `{
  "schema_version": 1,
  "provider": "compiler_trace",
  "revision": "abc123",
  "functions": [{
    "function": {"module": "MyApp.Generated", "function": "run", "arity": 1},
    "file": "lib/generated.ex",
    "fingerprint": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  }],
  "edges": [{
    "caller": {"module": "MyApp.Generated", "function": "run", "arity": 1},
    "callee": {"module": "SharedLib.Worker", "function": "perform", "arity": 1},
    "kind": "compiled_call"
  }],
  "test_ownership": [{
    "root": {"module": "MyApp.GeneratedTest", "function": "__dexter_test_root__", "arity": 0},
    "function": {"module": "MyApp.GeneratedTest", "function": "test generated", "arity": 1},
    "file": "test/generated_test.exs"
  }],
  "unresolved": [{
    "caller": {"module": "MyApp.Generated", "function": "run", "arity": 1},
    "kind": "dynamic_call",
    "detail": "variable module"
  }]
}`
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact, digest, err := LoadArtifact(path, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" || artifact.Provider != "compiler_trace" || len(artifact.Functions) != 1 || len(artifact.Edges) != 1 || len(artifact.TestOwnership) != 1 || len(artifact.Unresolved) != 1 {
		t.Fatalf("artifact = %+v, digest = %q", artifact, digest)
	}
	if _, _, err := LoadArtifact(path, "different"); err == nil {
		t.Fatal("artifact with the wrong revision was accepted")
	}
}

func TestLoadArtifactRejectsUnsafePathsAndInvalidIdentities(t *testing.T) {
	tests := []string{
		`{"schema_version":1,"provider":"compiler_trace","revision":"abc","functions":[{"function":{"module":"","function":"run","arity":1},"file":"lib/a.ex","fingerprint":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}]}`,
		`{"schema_version":1,"provider":"compiler_trace","revision":"abc","test_ownership":[{"root":{"module":"MyApp.Test","function":"__dexter_test_root__","arity":0},"function":{"module":"MyApp.Test","function":"test x","arity":1},"file":"../outside_test.exs"}]}`,
	}
	for i, contents := range tests {
		path := filepath.Join(t.TempDir(), "invalid.json")
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadArtifact(path, "abc"); err == nil {
			t.Errorf("case %d was accepted", i)
		}
	}
}
