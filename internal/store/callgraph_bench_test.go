package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
)

func BenchmarkIncrementalCallGraphIndex(b *testing.B) {
	const count = 100
	dir := b.TempDir()
	path := filepath.Join(dir, "worker.ex")
	if err := os.WriteFile(path, []byte("defmodule MyApp.Worker do\nend\n"), 0o644); err != nil {
		b.Fatal(err)
	}

	defs := make([]parser.Definition, 0, count+1)
	refs := make([]parser.Reference, 0, count)
	calls := make([]parser.CallEdge, 0, count)
	caller := parser.FunctionID{Module: "MyApp.Worker", Function: "run", Arity: 1}
	defs = append(defs, parser.Definition{Module: caller.Module, Function: caller.Function, Arity: caller.Arity, Kind: "def", FilePath: path})
	for i := 0; i < count; i++ {
		function := fmt.Sprintf("call_%d", i)
		callee := parser.FunctionID{Module: "SharedLib.Service", Function: function, Arity: 1}
		refs = append(refs, parser.Reference{Module: callee.Module, Function: function, FilePath: path, Kind: "call"})
		calls = append(calls, parser.CallEdge{Caller: caller, Callee: callee, Kind: "call"})
	}

	for _, benchmark := range []struct {
		name  string
		calls []parser.CallEdge
	}{
		{name: "without_calls"},
		{name: "with_100_calls", calls: calls},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			s, err := Open(filepath.Join(dir, benchmark.name))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = s.Close() })
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.IndexFileWithRefsAndCalls(path, defs, refs, benchmark.calls); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkBulkCallGraphIndex(b *testing.B) {
	const (
		fileCount    = 1000
		callsPerFile = 10
	)

	for _, includeCalls := range []bool{false, true} {
		name := "without_calls"
		if includeCalls {
			name = "with_calls"
		}
		b.Run(name, func(b *testing.B) {
			var databaseBytes int64
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				root := filepath.Join(b.TempDir(), fmt.Sprintf("index-%d", iteration))
				s, err := Open(root)
				if err != nil {
					b.Fatal(err)
				}
				if err := s.DropIndexes(); err != nil {
					b.Fatal(err)
				}
				batch, err := s.BeginBulkInsert()
				if err != nil {
					b.Fatal(err)
				}
				for fileIndex := 0; fileIndex < fileCount; fileIndex++ {
					module := fmt.Sprintf("MyApp.Worker%d", fileIndex)
					service := fmt.Sprintf("SharedLib.Service%d", fileIndex)
					path := filepath.Join(root, "lib", fmt.Sprintf("worker_%d.ex", fileIndex))
					caller := parser.FunctionID{Module: module, Function: "run", Arity: 1}
					defs := []parser.Definition{{Module: module, Function: "run", Arity: 1, Kind: "def", FilePath: path}}
					refs := make([]parser.Reference, 0, callsPerFile)
					calls := make([]parser.CallEdge, 0, callsPerFile)
					for callIndex := 0; callIndex < callsPerFile; callIndex++ {
						function := fmt.Sprintf("call_%d", callIndex)
						callee := parser.FunctionID{Module: service, Function: function, Arity: 1}
						refs = append(refs, parser.Reference{Module: callee.Module, Function: function, FilePath: path, Kind: "call"})
						if includeCalls {
							calls = append(calls, parser.CallEdge{Caller: caller, Callee: callee, Kind: "call"})
						}
					}
					if err := batch.IndexFileWithMtimeRefsAndCalls(path, int64(fileIndex), defs, refs, calls); err != nil {
						b.Fatal(err)
					}
				}
				if err := batch.Commit(); err != nil {
					b.Fatal(err)
				}
				if err := s.CreateIndexes(); err != nil {
					b.Fatal(err)
				}
				if err := s.Checkpoint(); err != nil {
					b.Fatal(err)
				}
				if err := s.Close(); err != nil {
					b.Fatal(err)
				}
				info, err := os.Stat(DBPath(root))
				if err != nil {
					b.Fatal(err)
				}
				databaseBytes = info.Size()
			}
			b.ReportMetric(float64(databaseBytes), "db-bytes")
			b.ReportMetric(float64(databaseBytes)/fileCount, "db-bytes/file")
		})
	}
}
