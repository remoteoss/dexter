package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/daemon"
	"github.com/remoteoss/dexter/internal/lsptest"
)

func TestLSP_ColdStartBuildsInServer(t *testing.T) {
	binary := buildDexter(t)
	root := scaffoldProject(t)

	var stderr bytes.Buffer
	client, err := lsptest.Start(binary, root, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	path := filepath.Join(root, "lib/my_app/workers/direct_worker.ex")
	line, char := lsptest.FindT(t, path, "get", 1)
	deadline := time.Now().Add(5 * time.Second)
	for {
		locations, err := client.Definition(path, line, char)
		if err != nil {
			t.Fatal(err)
		}
		if got := lsptest.Lines(root, locations); reflect.DeepEqual(got, []string{"lib/my_app/repo.ex:2"}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("definition never became available after cold LSP startup")
		}
		time.Sleep(10 * time.Millisecond)
	}

	client.Close()
	// The workspace — and therefore the cold build — lives in the daemon, so its
	// log carries what the server process used to print on its own stderr. The
	// proxy's stderr is included too: if a rebuild ever moves back into the
	// frontend, the mismatch assertion below still catches it there.
	logs := stderr.String() + daemonLogs(t, root)
	if strings.Contains(logs, "Index version mismatch") {
		t.Errorf("cold LSP startup rebuilt through cmdInit before serving:\n%s", logs)
	}
	if !strings.Contains(logs, "No index found, building from scratch") {
		t.Errorf("cold LSP startup did not use the server's background build:\n%s", logs)
	}
}

// TestLSP_RootFlagServesWorkspaceFromAnotherDirectory starts the server from a
// directory outside the project and names the workspace with --root, the shape
// an agent or an editor wrapper uses when it does not run from the project.
func TestLSP_RootFlagServesWorkspaceFromAnotherDirectory(t *testing.T) {
	binary := buildDexter(t)
	root := scaffoldProject(t)
	runDexter(t, binary, root, "init", "--root", root)
	client := lsptest.StartInT(t, binary, root, t.TempDir())

	path := filepath.Join(root, "lib/my_app/workers/direct_worker.ex")
	line, char := lsptest.FindT(t, path, "get", 1)
	got := lsptest.Lines(root, client.Definition(path, line, char))
	want := []string{"lib/my_app/repo.ex:2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("definition from outside the project = %v, want %v", got, want)
	}
}

// TestLSP_WarnsOnNonProjectRoot covers the editor side of the not-a-project
// guard: an editor is authoritative about what the user opened, so the LSP
// warns and serves rather than refusing, and the warning reaches the server log
// an editor collects.
func TestLSP_WarnsOnNonProjectRoot(t *testing.T) {
	binary := buildDexter(t)
	root := t.TempDir()
	var stderr safeBuffer
	client, err := lsptest.Start(binary, root, &stderr)
	if err != nil {
		t.Fatalf("lsp refused a directory that is not a project: %v", err)
	}
	defer client.Close()

	// The warning is written by the child before the handshake completes, but
	// the parent's stderr copy goroutine may not have landed it yet when Start
	// returns, so wait for it rather than sampling once.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stderr.String(), "does not look like an Elixir project") {
		if time.Now().After(deadline) {
			t.Fatalf("serving a non-project root logged no warning:\n%s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// safeBuffer is a bytes.Buffer the test may read while os/exec's stderr copy
// goroutine writes it; bytes.Buffer itself is not safe for that.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// daemonLogs returns what the workspace daemon logged for root. The daemon exits
// on its idle timeout but its log stays at the endpoint, so this is readable
// after the client that started it is gone.
func daemonLogs(t *testing.T, root string) string {
	t.Helper()
	endpoint, err := daemon.ResolveEndpoint(root)
	if err != nil {
		t.Fatalf("resolving the daemon endpoint for %s: %v", root, err)
	}
	data, err := os.ReadFile(endpoint.Log)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("reading the daemon log %s: %v", endpoint.Log, err)
	}
	return string(data)
}

// These tests drive a real dexter LSP server over stdio through internal/lsptest
// and assert on the wire results. They cover the paths that unit tests cannot
// reach on their own: cursor resolution, alias and use-chain resolution, and the
// store queries behind them, all in one request.

// writeFiles adds files to an existing scaffold root.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// startIndexedServer scaffolds a project, indexes it, and returns a driven
// server plus the project root.
func startIndexedServer(t *testing.T, extra map[string]string) (*lsptest.T, string) {
	t.Helper()
	binary := buildDexter(t)
	root := scaffoldProject(t)
	if extra != nil {
		writeFiles(t, root, extra)
	}
	runDexter(t, binary, root, "init", "--root", root)
	return lsptest.StartT(t, binary, root), root
}

func TestLSP_ReferencesResolveThroughEveryAliasForm(t *testing.T) {
	client, root := startIndexedServer(t, nil)

	cases := []struct {
		name   string
		file   string
		needle string
		nth    int
		want   []string
	}{
		{
			name:   "fully qualified call",
			file:   "lib/my_app/repo.ex",
			needle: "get",
			nth:    1, // def get(schema, id)
			want:   []string{"lib/my_app/workers/direct_worker.ex:3"},
		},
		{
			name:   "plain alias",
			file:   "lib/my_app/handlers/webhooks.ex",
			needle: "process_event",
			nth:    1,
			want:   []string{"lib/my_app/workers/webhook_worker.ex:5"},
		},
		{
			name:   "alias with as:",
			file:   "lib/my_app/serializer/date.ex",
			needle: "format",
			nth:    1,
			want:   []string{"lib/my_app/values/timesheet.ex:6"},
		},
		{
			name:   "grouped alias member",
			file:   "lib/my_app/companies/value/company.ex",
			needle: "build",
			nth:    1,
			want:   []string{"lib/my_app/values/report.ex:5", "lib/my_app/values/timesheet.ex:7"},
		},
		{
			name:   "imported function called bare",
			file:   "lib/my_app/helpers/formatting.ex",
			needle: "format_currency",
			nth:    1,
			want:   []string{"lib/my_app/views/money_view.ex:5"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.file)
			line, char := lsptest.FindT(t, path, tc.needle, tc.nth)
			got := lsptest.Lines(root, client.References(path, line, char, false))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("references = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLSP_DefinitionResolvesThroughAliases(t *testing.T) {
	client, root := startIndexedServer(t, nil)

	cases := []struct {
		name   string
		file   string
		needle string
		nth    int
		want   []string
	}{
		// Every head of a pattern-matched function is a definition.
		{"through a plain alias", "lib/my_app/workers/webhook_worker.ex", "process_event", 1, []string{
			"lib/my_app/handlers/webhooks.ex:10", "lib/my_app/handlers/webhooks.ex:2", "lib/my_app/handlers/webhooks.ex:6"}},
		{"through an as: alias", "lib/my_app/values/timesheet.ex", "format", 1, []string{"lib/my_app/serializer/date.ex:2"}},
		{"fully qualified", "lib/my_app/workers/direct_worker.ex", "get", 1, []string{"lib/my_app/repo.ex:2"}},
		{"imported bare call", "lib/my_app/views/money_view.ex", "format_date", 1, []string{"lib/my_app/helpers/formatting.ex:6"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.file)
			line, char := lsptest.FindT(t, path, tc.needle, tc.nth)
			got := lsptest.Lines(root, client.Definition(path, line, char))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("definition = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLSP_HoverReportsTheDefiningModule(t *testing.T) {
	client, root := startIndexedServer(t, nil)

	path := filepath.Join(root, "lib/my_app/workers/webhook_worker.ex")
	line, char := lsptest.FindT(t, path, "process_event", 1)
	got := client.Hover(path, line, char)
	if got == "" {
		t.Fatal("hover returned nothing for an aliased call")
	}
	// Hover renders the resolved definition's signature.
	if want := "def process_event"; !strings.Contains(got, want) {
		t.Errorf("hover = %q, want it to contain %q", got, want)
	}
}

// useChainFiles is a minimal `use` chain: a web module whose __using__ imports a
// helper, and two consumers that call the injected function bare. The call sites
// name neither the helper nor the function's own module, so only the injector
// scan can attribute them.
var useChainFiles = map[string]string{
	"lib/shared_lib/helpers.ex": `defmodule SharedLib.Helpers do
  def render_title(assigns) do
    :ok
  end
end
`,
	"lib/shared_lib/web.ex": `defmodule SharedLib.Web do
  defmacro __using__(_opts) do
    quote do
      import SharedLib.Helpers
    end
  end
end
`,
	"lib/my_app/pages/home_page.ex": `defmodule MyApp.Pages.HomePage do
  use SharedLib.Web

  def render(assigns) do
    render_title(assigns)
  end
end
`,
	"lib/my_app/pages/about_page.ex": `defmodule MyApp.Pages.AboutPage do
  use SharedLib.Web

  def render(assigns) do
    render_title(assigns)
  end
end
`,
}

func TestLSP_ReferencesThroughUseChain(t *testing.T) {
	client, root := startIndexedServer(t, useChainFiles)

	path := filepath.Join(root, "lib/shared_lib/helpers.ex")
	line, char := lsptest.FindT(t, path, "render_title", 1)

	got := lsptest.Lines(root, client.References(path, line, char, false))
	want := []string{
		"lib/my_app/pages/about_page.ex:5",
		"lib/my_app/pages/home_page.ex:5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("references through use chain = %v, want %v", got, want)
	}
}

func TestLSP_ReferencesOnUsingMacroFindsUseSites(t *testing.T) {
	client, root := startIndexedServer(t, useChainFiles)

	path := filepath.Join(root, "lib/shared_lib/web.ex")
	line, char := lsptest.FindT(t, path, "__using__", 1)

	got := lsptest.Lines(root, client.References(path, line, char, false))
	want := []string{
		"lib/my_app/pages/about_page.ex:2",
		"lib/my_app/pages/home_page.ex:2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("references on __using__ = %v, want the use sites %v", got, want)
	}
}

// TestLSP_ReferencesOnModuleFindsEveryUseSite covers the shape that looked wrong
// on a real codebase: a case-template module `use`d by many files should report
// every one of those use sites, not a handful.
func TestLSP_ReferencesOnModuleFindsEveryUseSite(t *testing.T) {
	client, root := startIndexedServer(t, useChainFiles)

	path := filepath.Join(root, "lib/shared_lib/web.ex")
	line, char := lsptest.FindT(t, path, "SharedLib.Web", 1)

	got := lsptest.Lines(root, client.References(path, line, char, false))
	want := []string{
		"lib/my_app/pages/about_page.ex:2",
		"lib/my_app/pages/home_page.ex:2",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("references on the module = %v, want the use sites %v", got, want)
	}
}
