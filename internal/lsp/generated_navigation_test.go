package lsp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/beam"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

type generatedNavigationFixture struct {
	server        *Server
	consumer      string
	extension     string
	consumerURI   string
	extensionPath string
	ebin          string
}

func newGeneratedNavigationFixture(t *testing.T) generatedNavigationFixture {
	t.Helper()
	server, cleanup := setupTestServer(t)
	t.Cleanup(cleanup)

	const consumer = "MyApp.Article"
	const extension = "SharedLib.Dsl"
	const source = `defmodule MyApp.Article do
  use SharedLib.Resource

  code_interface do
    define(:find_article, [])
  end
end
`
	const extensionSource = `defmodule SharedLib.Dsl do
  def source_function, do: :ok
end
`
	indexAndCompile(t, server, consumer, "lib/my_app/article.ex", source)
	indexAndCompile(t, server, extension, "deps/shared_lib/lib/shared_lib/dsl.ex", extensionSource)

	ebin := filepath.Join(server.projectRoot, "_build", "dev", "lib", "my_app", "ebin")
	write := func(module string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ebin, "Elixir."+module+".beam"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(consumer, minimalBeamWithAttrs(
		[]testAttr{{name: "extensions", values: []string{"Elixir." + extension}}},
		"", beamExport{"source_function", 0}, beamExport{"generated_lookup", 1}))
	write(extension, minimalBeam(beamExport{"MACRO-code_interface", 2}))
	write(extension+".CodeInterface.Define", minimalBeam(beamExport{"MACRO-define", 3}))
	bumpDirMtime(t, ebin)

	consumerPath := filepath.Join(server.projectRoot, "lib/my_app/article.ex")
	consumerURI := string(uri.File(consumerPath))
	server.docs.Set(consumerURI, source)
	return generatedNavigationFixture{
		server:        server,
		consumer:      consumer,
		extension:     extension,
		consumerURI:   consumerURI,
		extensionPath: filepath.Join(server.projectRoot, "deps/shared_lib/lib/shared_lib/dsl.ex"),
		ebin:          ebin,
	}
}

func TestDefinitionGeneratedDslMacroFallsBackToSourceBackedProvider(t *testing.T) {
	f := newGeneratedNavigationFixture(t)
	locations, err := f.server.Definition(context.Background(), &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(f.consumerURI)},
			Position:     protocol.Position{Line: 4, Character: 8},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || uriToPath(locations[0].URI) != f.extensionPath {
		t.Fatalf("expected generated macro definition to fall back to %s, got %#v", f.extensionPath, locations)
	}
}

func TestDefinitionGeneratedDslMacroAfterInjectorResolutionMisses(t *testing.T) {
	f := newGeneratedNavigationFixture(t)
	indexFile(t, f.server.store, f.server.projectRoot, "deps/shared_lib/lib/shared_lib/resource.ex", `defmodule SharedLib.Resource do
  defmacro __using__(_opts) do
    quote do
      import SharedLib.Resource
    end
  end
end
`)

	locations, err := f.server.Definition(context.Background(), &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(f.consumerURI)},
			Position:     protocol.Position{Line: 4, Character: 8},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || uriToPath(locations[0].URI) != f.extensionPath {
		t.Fatalf("expected provider fallback after injector lookup missed, got %#v", locations)
	}
}

func TestDefinitionBareGeneratedConsumerFunction(t *testing.T) {
	f := newGeneratedNavigationFixture(t)
	const source = `defmodule MyApp.Article do
  use SharedLib.Resource

  def run, do: generated_lookup(:id)
end
`
	indexFile(t, f.server.store, f.server.projectRoot, "lib/my_app/article.ex", source)
	f.server.docs.Set(f.consumerURI, source)

	locations, err := f.server.Definition(context.Background(), &protocol.DefinitionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(f.consumerURI)},
			Position:     protocol.Position{Line: 3, Character: 26},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || uriToPath(locations[0].URI) != uriToPath(protocol.DocumentURI(f.consumerURI)) {
		t.Fatalf("expected generated consumer function to fall back to its module, got %#v", locations)
	}
}

func TestSignatureHelpGeneratedDslMacro(t *testing.T) {
	f := newGeneratedNavigationFixture(t)
	result, err := f.server.SignatureHelp(context.Background(), &protocol.SignatureHelpParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(f.consumerURI)},
			Position:     protocol.Position{Line: 4, Character: 26},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Signatures) == 0 {
		t.Fatal("expected signature help for generated define macro")
	}
	if got := result.Signatures[0].Label; got != "define(arg1, arg2)" {
		t.Fatalf("signature label = %q, want define(arg1, arg2)", got)
	}
	if result.ActiveParameter != 1 {
		t.Fatalf("active parameter = %d, want 1", result.ActiveParameter)
	}
}

func TestSignatureHelpGeneratedDslMacroWithoutParens(t *testing.T) {
	f := newGeneratedNavigationFixture(t)
	const source = `defmodule MyApp.Article do
  use SharedLib.Resource

  code_interface do
    define :find_article, get_by: [:id]
  end
end
`
	f.server.docs.Set(f.consumerURI, source)
	result, err := f.server.SignatureHelp(context.Background(), &protocol.SignatureHelpParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(f.consumerURI)},
			Position:     protocol.Position{Line: 4, Character: uint32(len("    define :find_article, get_by: [:id]"))},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Signatures) == 0 {
		t.Fatal("expected signature help for generated no-paren define macro")
	}
	if got := result.Signatures[0].Label; got != "define(arg1, arg2)" {
		t.Fatalf("signature label = %q, want define(arg1, arg2)", got)
	}
	if result.ActiveParameter != 1 {
		t.Fatalf("active parameter = %d, want 1", result.ActiveParameter)
	}
}

func TestGeneratedCompletionResolveUsesBeamDocs(t *testing.T) {
	f := newGeneratedNavigationFixture(t)
	provider := f.extension + ".CodeInterface.Define"
	beamPath := filepath.Join(f.ebin, "Elixir."+provider+".beam")
	const prose = "Defines a generated code interface."
	if err := os.WriteFile(beamPath, minimalBeamWithDocs(prose, beamExport{"MACRO-define", 3}), 0o644); err != nil {
		t.Fatal(err)
	}
	f.server.generatedCache.put(provider, generatedFunctionCacheEntry{
		beamPath:  beamPath,
		beamStamp: statFileStamp(beamPath),
		functions: []beam.Function{{Name: "define", Arity: 2, Params: "name,opts", Kind: "defmacro", DocLen: len(prose)}},
	})

	items := completionAt(t, f.server, f.consumerURI, 4, uint32(len("    def")))
	var item *protocol.CompletionItem
	for i := range items {
		if items[i].Label == "define/2" {
			item = &items[i]
			break
		}
	}
	if item == nil {
		t.Fatalf("expected generated define completion, got %v", labelsOf(items))
	}
	if item.Data == nil {
		t.Fatal("generated completion must carry resolve data")
	}
	resolved, err := f.server.CompletionResolve(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	doc, ok := resolved.Documentation.(protocol.MarkupContent)
	if !ok || !strings.Contains(doc.Value, prose) {
		t.Fatalf("resolved documentation = %#v, want BEAM prose", resolved.Documentation)
	}
}

func TestPrepareCallHierarchyGeneratedConsumerFunction(t *testing.T) {
	f := newGeneratedNavigationFixture(t)
	callerPath := filepath.Join(f.server.projectRoot, "lib/my_app/caller.ex")
	callerSource := `defmodule MyApp.Caller do
  def run, do: MyApp.Article.generated_lookup(:id)
end
`
	indexFile(t, f.server.store, f.server.projectRoot, "lib/my_app/caller.ex", callerSource)
	callerURI := string(uri.File(callerPath))
	f.server.docs.Set(callerURI, callerSource)

	items, err := f.server.PrepareCallHierarchy(context.Background(), &protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(callerURI)},
			Position:     protocol.Position{Line: 1, Character: 38},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "MyApp.Article.generated_lookup/1" {
		t.Fatalf("expected generated call hierarchy item, got %#v", items)
	}
	incoming, err := f.server.IncomingCalls(context.Background(), &protocol.CallHierarchyIncomingCallsParams{Item: items[0]})
	if err != nil {
		t.Fatal(err)
	}
	if len(incoming) != 1 || incoming[0].From.Name != "MyApp.Caller.run/0" {
		t.Fatalf("expected caller of generated function, got %#v", incoming)
	}
}

func TestPrepareCallHierarchyGeneratedDslMacro(t *testing.T) {
	f := newGeneratedNavigationFixture(t)
	items, err := f.server.PrepareCallHierarchy(context.Background(), &protocol.CallHierarchyPrepareParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(f.consumerURI)},
			Position:     protocol.Position{Line: 4, Character: 8},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name != "SharedLib.Dsl.CodeInterface.Define.define/2" {
		t.Fatalf("expected generated DSL call hierarchy item, got %#v", items)
	}
	if uriToPath(items[0].URI) != f.extensionPath {
		t.Fatalf("expected call hierarchy to use source-backed provider, got %s", items[0].URI)
	}
}

func TestReferencesGeneratedDslMacroThroughInjector(t *testing.T) {
	f := newGeneratedNavigationFixture(t)
	second := `defmodule MyApp.Comment do
  use SharedLib.Resource

  code_interface do
    define(:find_comment, [])
  end
end
`
	indexAndCompile(t, f.server, "MyApp.Comment", "lib/my_app/comment.ex", second)
	if err := os.WriteFile(filepath.Join(f.ebin, "Elixir.MyApp.Comment.beam"), minimalBeamWithAttrs(
		[]testAttr{{name: "extensions", values: []string{"Elixir." + f.extension}}}, "", beamExport{"source_function", 0}), 0o644); err != nil {
		t.Fatal(err)
	}
	bumpDirMtime(t, f.ebin)

	// The parser conservatively attributes bare calls to every use module. A
	// different DSL can therefore produce the same call name under the same
	// injector key; provider-scope validation must keep it out of these results.
	const otherExtension = "OtherLib.Dsl"
	other := `defmodule MyApp.OtherArticle do
  use SharedLib.Resource

  code_interface do
    define(:find_other, [])
  end
end
`
	indexAndCompile(t, f.server, "MyApp.OtherArticle", "lib/my_app/other_article.ex", other)
	indexAndCompile(t, f.server, otherExtension, "deps/other_lib/lib/other_lib/dsl.ex", `defmodule OtherLib.Dsl do
  def source_function, do: :ok
end
`)
	if err := os.WriteFile(filepath.Join(f.ebin, "Elixir.MyApp.OtherArticle.beam"), minimalBeamWithAttrs(
		[]testAttr{{name: "extensions", values: []string{"Elixir." + otherExtension}}}, "", beamExport{"source_function", 0}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.ebin, "Elixir."+otherExtension+".beam"), minimalBeam(beamExport{"MACRO-code_interface", 2}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.ebin, "Elixir."+otherExtension+".CodeInterface.Define.beam"), minimalBeam(beamExport{"MACRO-define", 3}), 0o644); err != nil {
		t.Fatal(err)
	}
	bumpDirMtime(t, f.ebin)

	locations, err := f.server.References(context.Background(), &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(f.consumerURI)},
			Position:     protocol.Position{Line: 4, Character: 8},
		},
		Context: protocol.ReferenceContext{IncludeDeclaration: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, location := range locations {
		seen[filepath.Base(uriToPath(location.URI))] = true
	}
	if !seen["article.ex"] || !seen["comment.ex"] {
		t.Fatalf("expected both DSL call sites, got %#v", locations)
	}
	if !seen["dsl.ex"] {
		t.Fatalf("expected the source-backed provider declaration, got %#v", locations)
	}
	if seen["other_article.ex"] {
		t.Fatalf("reference from a different generated provider leaked in: %#v", locations)
	}
}

func TestReferencesBareGeneratedConsumerFunctionThroughInjector(t *testing.T) {
	f := newGeneratedNavigationFixture(t)
	const source = `defmodule MyApp.Article do
  use SharedLib.Resource

  def run do
    generated_lookup(:id)
  end
end
`
	indexFile(t, f.server.store, f.server.projectRoot, "lib/my_app/article.ex", source)
	f.server.docs.Set(f.consumerURI, source)
	indexed, err := f.server.store.LookupReferences("SharedLib.Resource", "generated_lookup")
	if err != nil || len(indexed) != 1 {
		t.Fatalf("test setup expected one injector reference, got %v, %v", indexed, err)
	}
	provider, _, found := f.server.generatedSymbolInScope(f.consumer, func() []string { return []string{"defmodule", "def"} }, "generated_lookup")
	if !found || provider.module != f.consumer {
		t.Fatalf("test setup expected generated consumer provider, got %#v, %v", provider, found)
	}

	locations, err := f.server.References(context.Background(), &protocol.ReferenceParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(f.consumerURI)},
			Position:     protocol.Position{Line: 4, Character: 12},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || uriToPath(locations[0].URI) != uriToPath(protocol.DocumentURI(f.consumerURI)) || locations[0].Range.Start.Line != 4 {
		t.Fatalf("expected the bare generated consumer call, got %#v", locations)
	}
}
