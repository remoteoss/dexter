package parser

import (
	"os"
	"path/filepath"
	"testing"
)

var benchFiles []struct {
	name string
	data []byte
}

func loadBenchFiles(b *testing.B) {
	b.Helper()
	if benchFiles != nil {
		return
	}
	testdata := filepath.Join("..", "lsp", "testdata", "monorepo", "apps", "app_with_ecto_migration", "deps")
	candidates := []string{
		filepath.Join(testdata, "ecto", "lib", "ecto", "changeset.ex"),
		filepath.Join(testdata, "db_connection", "lib", "db_connection.ex"),
		filepath.Join(testdata, "ecto", "lib", "ecto", "repo.ex"),
		filepath.Join(testdata, "ecto", "lib", "ecto", "query.ex"),
		filepath.Join(testdata, "ecto_sql", "lib", "ecto", "adapters", "sql.ex"),
	}
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		benchFiles = append(benchFiles, struct {
			name string
			data []byte
		}{filepath.Base(path), data})
	}
	if len(benchFiles) == 0 {
		b.Skip("no benchmark files found")
	}
}

func BenchmarkParseText(b *testing.B) {
	loadBenchFiles(b)
	for _, f := range benchFiles {
		b.Run(f.name, func(b *testing.B) {
			text := string(f.data)
			b.SetBytes(int64(len(f.data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _, _ = ParseText("bench.ex", text)
			}
		})
	}
}

func BenchmarkTokenize(b *testing.B) {
	loadBenchFiles(b)
	for _, f := range benchFiles {
		b.Run(f.name, func(b *testing.B) {
			b.SetBytes(int64(len(f.data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				Tokenize(f.data)
			}
		})
	}
}

func BenchmarkParseTextAllFiles(b *testing.B) {
	testdata := filepath.Join("..", "lsp", "testdata")
	var allFiles []struct {
		name string
		data []byte
	}
	_ = filepath.Walk(testdata, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".ex" && ext != ".exs" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(testdata, path)
		allFiles = append(allFiles, struct {
			name string
			data []byte
		}{rel, data})
		return nil
	})
	if len(allFiles) == 0 {
		b.Skip("no test files found")
	}

	var totalBytes int64
	for _, f := range allFiles {
		totalBytes += int64(len(f.data))
	}

	b.Run("all_testdata", func(b *testing.B) {
		b.SetBytes(totalBytes)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, f := range allFiles {
				_, _, _ = ParseText(f.name, string(f.data))
			}
		}
	})
}
