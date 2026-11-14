package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/remoteoss/dexter/internal/pathcase"
)

// oldWorkspaceIdentity is workspaceIdentity before it used
// store.CanonicalDir. The socket and lock of a workspace are keyed by this
// value, so a change would make a new binary miss a daemon that an older
// one started.
func oldWorkspaceIdentity(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		return filepath.Clean(root)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return pathcase.DiskSpelling(resolved)
	}
	return abs
}

func TestWorkspaceIdentityIsUnchanged(t *testing.T) {
	base := t.TempDir() // on macOS below /var, a symlink to /private/var
	project := filepath.Join(base, "Shop")
	if err := os.MkdirAll(filepath.Join(project, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "shop-link")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, project)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		project,
		project + "/",
		filepath.Join(project, "lib", ".."),
		link,
		strings.ToLower(project), // other letter case
		rel,                      // relative
		filepath.Join(base, "missing"),
		"",
	}
	for _, p := range paths {
		if got, want := workspaceIdentity(p), oldWorkspaceIdentity(p); got != want {
			t.Errorf("workspaceIdentity(%q) = %q, want %q as before", p, got, want)
		}
	}
}
