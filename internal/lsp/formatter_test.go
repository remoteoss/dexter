package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/remoteoss/dexter/internal/store"
)

func fixtureMonorepoPath(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "monorepo")
}

func ensureFixtureDeps(t *testing.T, mixRoot string) {
	t.Helper()
	buildDir := filepath.Join(mixRoot, "_build")
	if _, err := os.Stat(buildDir); err == nil {
		return
	}
	t.Logf("compiling fixture deps in %s (first run only)", mixRoot)
	cmd := exec.Command("mix", "deps.get")
	cmd.Dir = mixRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not fetch deps for fixture %s: %v\n%s", mixRoot, err, out)
	}
	cmd = exec.Command("mix", "deps.compile")
	cmd.Dir = mixRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not compile deps for fixture %s: %v\n%s", mixRoot, err, out)
	}
}

func setupTestServerForFixture(t *testing.T, mixRoot string) (*Server, func()) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(s, filepath.Dir(filepath.Dir(mixRoot)))
	if p, err := exec.LookPath("mix"); err == nil {
		server.mixBin = p
	}
	return server, func() {
		_ = s.Close()
	}
}

func TestFormatterServer_WithStylerPlugin(t *testing.T) {
	if _, err := exec.LookPath("mix"); err != nil {
		t.Skip("mix not available in PATH")
	}

	monorepo := fixtureMonorepoPath(t)
	mixRoot := filepath.Join(monorepo, "apps", "app_with_styler")
	ensureFixtureDeps(t, mixRoot)

	server, cleanup := setupTestServerForFixture(t, mixRoot)
	defer cleanup()

	unformatted := "defmodule Test do\n  def hello(x) do\n    x |> to_string()\n  end\nend\n"
	filePath := filepath.Join(mixRoot, "lib", "test.ex")
	docURI := string(uri.File(filePath))
	server.docs.Set(docURI, unformatted)

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits == nil {
		t.Fatal("expected formatting edits from Styler, got nil")
	}
	if !strings.Contains(edits[0].NewText, "to_string(x)") {
		t.Errorf("expected Styler to rewrite pipe, got:\n%s", edits[0].NewText)
	}
	if strings.Contains(edits[0].NewText, "|>") {
		t.Errorf("expected Styler to remove single pipe, got:\n%s", edits[0].NewText)
	}
}

func TestFormatterServer_BasicProject(t *testing.T) {
	if _, err := exec.LookPath("mix"); err != nil {
		t.Skip("mix not available in PATH")
	}

	monorepo := fixtureMonorepoPath(t)
	mixRoot := filepath.Join(monorepo, "apps", "app_basic")
	ensureFixtureDeps(t, mixRoot)

	server, cleanup := setupTestServerForFixture(t, mixRoot)
	defer cleanup()

	unformatted := "defmodule Test do\n  def hello(x) do\n    x |> to_string()\n  end\nend\n"
	filePath := filepath.Join(mixRoot, "lib", "test.ex")
	docURI := string(uri.File(filePath))
	server.docs.Set(docURI, unformatted)

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits != nil {
		if !strings.Contains(edits[0].NewText, "|>") {
			t.Errorf("basic project should NOT rewrite pipes, got:\n%s", edits[0].NewText)
		}
	}
}

func TestFormatterServer_BadIndentation(t *testing.T) {
	if _, err := exec.LookPath("mix"); err != nil {
		t.Skip("mix not available in PATH")
	}

	monorepo := fixtureMonorepoPath(t)
	mixRoot := filepath.Join(monorepo, "apps", "app_basic")
	ensureFixtureDeps(t, mixRoot)

	server, cleanup := setupTestServerForFixture(t, mixRoot)
	defer cleanup()

	unformatted := "defmodule   Test   do\ndef   hello(   ), do:    :world\nend\n"
	filePath := filepath.Join(mixRoot, "lib", "test.ex")
	docURI := string(uri.File(filePath))
	server.docs.Set(docURI, unformatted)

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits == nil {
		t.Fatal("expected formatting edits for badly indented code, got nil")
	}
	if !strings.Contains(edits[0].NewText, "defmodule Test do") {
		t.Errorf("expected formatted output, got:\n%s", edits[0].NewText)
	}
}

func TestFormatterServer_DifferentProjectsDifferentResults(t *testing.T) {
	if _, err := exec.LookPath("mix"); err != nil {
		t.Skip("mix not available in PATH")
	}

	monorepo := fixtureMonorepoPath(t)
	stylerRoot := filepath.Join(monorepo, "apps", "app_with_styler")
	basicRoot := filepath.Join(monorepo, "apps", "app_basic")
	ensureFixtureDeps(t, stylerRoot)
	ensureFixtureDeps(t, basicRoot)

	server, cleanup := setupTestServerForFixture(t, stylerRoot)
	defer cleanup()

	input := "defmodule Test do\n  def hello(x) do\n    x |> to_string()\n  end\nend\n"

	stylerFile := filepath.Join(stylerRoot, "lib", "test.ex")
	stylerURI := string(uri.File(stylerFile))
	server.docs.Set(stylerURI, input)

	stylerEdits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(stylerURI)},
	})
	if err != nil {
		t.Fatal(err)
	}

	basicFile := filepath.Join(basicRoot, "lib", "test.ex")
	basicURI := string(uri.File(basicFile))
	server.docs.Set(basicURI, input)

	basicEdits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(basicURI)},
	})
	if err != nil {
		t.Fatal(err)
	}

	if stylerEdits == nil {
		t.Fatal("expected Styler to produce edits")
	}
	stylerResult := stylerEdits[0].NewText

	var basicResult string
	if basicEdits != nil {
		basicResult = basicEdits[0].NewText
	} else {
		basicResult = input
	}

	if stylerResult == basicResult {
		t.Errorf("expected different formatting results between projects.\nstyler: %s\nbasic: %s", stylerResult, basicResult)
	}

	if !strings.Contains(stylerResult, "to_string(x)") {
		t.Errorf("expected Styler to rewrite pipe, got:\n%s", stylerResult)
	}
	if !strings.Contains(basicResult, "|>") {
		t.Errorf("expected basic project to keep pipe, got:\n%s", basicResult)
	}
}

func TestFormatterServer_MigrationDSL(t *testing.T) {
	if _, err := exec.LookPath("mix"); err != nil {
		t.Skip("mix not available in PATH")
	}

	monorepo := fixtureMonorepoPath(t)
	mixRoot := filepath.Join(monorepo, "apps", "app_with_ecto_migration")
	ensureFixtureDeps(t, mixRoot)

	server, cleanup := setupTestServerForFixture(t, mixRoot)
	defer cleanup()

	// Migration DSL functions (add, create) are in locals_without_parens via
	// import_deps: [:fake_ecto_sql]. The formatter must not add parens to them.
	input := `defmodule MyApp.Migrations.CreateWidgets do
  def change do
    create table(:widgets) do
      add :name, :string
      add :count, :integer, default: 0
      timestamps()
    end

    create unique_index(:widgets, [:name])
    create index(:widgets, [:count])
  end
end
`
	filePath := filepath.Join(mixRoot, "priv", "repo", "migrations", "20000101000000_create_widgets.exs")
	docURI := string(uri.File(filePath))
	server.docs.Set(docURI, input)

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}

	var result string
	if edits != nil {
		result = edits[0].NewText
	} else {
		result = input
	}

	for _, unwanted := range []string{"add(", "create("} {
		if strings.Contains(result, unwanted) {
			t.Errorf("formatter added parens to migration DSL call %q (import_deps locals_without_parens not applied):\n%s", unwanted, result)
		}
	}
}

func TestFormatterServer_UmbrellaStylerPlugin(t *testing.T) {
	if _, err := exec.LookPath("mix"); err != nil {
		t.Skip("mix not available in PATH")
	}

	// Ensure the Styler fixture is compiled so we have beam files to reuse
	monorepo := fixtureMonorepoPath(t)
	stylerFixture := filepath.Join(monorepo, "apps", "app_with_styler")
	ensureFixtureDeps(t, stylerFixture)

	// Create an umbrella-like temp directory where _build is only at the
	// root, not in the child app — this is how real umbrella apps work.
	umbrellaRoot := t.TempDir()
	childApp := filepath.Join(umbrellaRoot, "apps", "child_app")
	if err := os.MkdirAll(filepath.Join(childApp, "lib"), 0755); err != nil {
		t.Fatal(err)
	}

	// Symlink _build from the existing fixture to the umbrella root
	if err := os.Symlink(
		filepath.Join(stylerFixture, "_build"),
		filepath.Join(umbrellaRoot, "_build"),
	); err != nil {
		t.Fatal(err)
	}

	// Write a minimal mix.exs so findMixRoot stops at the child app
	if err := os.WriteFile(
		filepath.Join(childApp, "mix.exs"),
		[]byte("defmodule ChildApp.MixProject do\n  use Mix.Project\n  def project, do: [app: :child_app, version: \"0.1.0\"]\nend\n"),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	// Write .formatter.exs with Styler plugin
	if err := os.WriteFile(
		filepath.Join(childApp, ".formatter.exs"),
		[]byte("[plugins: [Styler], inputs: [\"{lib,test}/**/*.{ex,exs}\"]]\n"),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	// Set up server with the umbrella root as projectRoot
	storeDir := t.TempDir()
	s, err := store.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	server := NewServer(s, umbrellaRoot)
	if p, err := exec.LookPath("mix"); err == nil {
		server.mixBin = p
	}

	unformatted := "defmodule Test do\n  def hello(x) do\n    x |> to_string()\n  end\nend\n"
	filePath := filepath.Join(childApp, "lib", "test.ex")
	docURI := string(uri.File(filePath))
	server.docs.Set(docURI, unformatted)

	edits, err := server.Formatting(context.Background(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(docURI)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if edits == nil {
		t.Fatal("expected formatting edits from Styler in umbrella child app, got nil")
	}
	if !strings.Contains(edits[0].NewText, "to_string(x)") {
		t.Errorf("expected Styler to rewrite pipe in umbrella child app, got:\n%s", edits[0].NewText)
	}
	if strings.Contains(edits[0].NewText, "|>") {
		t.Errorf("expected Styler to remove single pipe in umbrella child app, got:\n%s", edits[0].NewText)
	}
}

func TestComputeMinimalEdits(t *testing.T) {
	t.Run("identical text returns nil", func(t *testing.T) {
		edits := computeMinimalEdits("hello\nworld\n", "hello\nworld\n")
		if edits != nil {
			t.Errorf("expected nil, got %v", edits)
		}
	})

	t.Run("single line change in middle", func(t *testing.T) {
		original := "line1\nline2\nline3\n"
		formatted := "line1\nline2_changed\nline3\n"
		edits := computeMinimalEdits(original, formatted)
		if len(edits) != 1 {
			t.Fatalf("expected 1 edit, got %d", len(edits))
		}
		// Should only cover line 1 (0-indexed), not the whole document
		if edits[0].Range.Start.Line != 1 {
			t.Errorf("expected start line 1, got %d", edits[0].Range.Start.Line)
		}
		if edits[0].Range.End.Line != 2 {
			t.Errorf("expected end line 2, got %d", edits[0].Range.End.Line)
		}
		if edits[0].NewText != "line2_changed\n" {
			t.Errorf("unexpected new text: %q", edits[0].NewText)
		}
	})

	t.Run("change at beginning preserves suffix", func(t *testing.T) {
		original := "  bad_indent\nline2\nline3\n"
		formatted := "good_indent\nline2\nline3\n"
		edits := computeMinimalEdits(original, formatted)
		if len(edits) != 1 {
			t.Fatalf("expected 1 edit, got %d", len(edits))
		}
		if edits[0].Range.Start.Line != 0 {
			t.Errorf("expected start line 0, got %d", edits[0].Range.Start.Line)
		}
		if edits[0].Range.End.Line != 1 {
			t.Errorf("expected end line 1, got %d", edits[0].Range.End.Line)
		}
	})

	t.Run("line insertion", func(t *testing.T) {
		original := "line1\nline3\n"
		formatted := "line1\nline2\nline3\n"
		edits := computeMinimalEdits(original, formatted)
		if len(edits) != 1 {
			t.Fatalf("expected 1 edit, got %d", len(edits))
		}
		// Insert at line 1 with zero-width old range
		if edits[0].Range.Start.Line != 1 || edits[0].Range.End.Line != 1 {
			t.Errorf("expected insert at line 1, got %d-%d", edits[0].Range.Start.Line, edits[0].Range.End.Line)
		}
		if edits[0].NewText != "line2\n" {
			t.Errorf("unexpected new text: %q", edits[0].NewText)
		}
	})

	t.Run("line deletion", func(t *testing.T) {
		original := "line1\nline2\nline3\n"
		formatted := "line1\nline3\n"
		edits := computeMinimalEdits(original, formatted)
		if len(edits) != 1 {
			t.Fatalf("expected 1 edit, got %d", len(edits))
		}
		if edits[0].Range.Start.Line != 1 || edits[0].Range.End.Line != 2 {
			t.Errorf("expected delete line 1-2, got %d-%d", edits[0].Range.Start.Line, edits[0].Range.End.Line)
		}
		if edits[0].NewText != "" {
			t.Errorf("expected empty new text, got: %q", edits[0].NewText)
		}
	})

	t.Run("full document change still works", func(t *testing.T) {
		original := "aaa\nbbb\n"
		formatted := "xxx\nyyy\n"
		edits := computeMinimalEdits(original, formatted)
		if len(edits) != 1 {
			t.Fatalf("expected 1 edit, got %d", len(edits))
		}
		// No common prefix or suffix, but the trailing "" from SplitAfter matches
		if edits[0].NewText != "xxx\nyyy\n" {
			t.Errorf("unexpected new text: %q", edits[0].NewText)
		}
	})
}
