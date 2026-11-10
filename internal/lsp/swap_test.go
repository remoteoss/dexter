//go:build darwin || linux

package lsp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.lsp.dev/uri"
	"golang.org/x/sys/unix"
)

// The errors of file systems and kernels that cannot swap go to the
// hard-link fallback; other errors fail the replace.
func TestSwapError(t *testing.T) {
	for _, e := range []error{unix.ENOTSUP, unix.EOPNOTSUPP, unix.EINVAL, unix.ENOSYS, unix.EXDEV} {
		if got := swapError(&os.LinkError{Op: "rename", Err: e}); !errors.Is(got, errSwapUnsupported) {
			t.Errorf("%v: got %v, want the fallback", e, got)
		}
	}
	if got := swapError(unix.EIO); errors.Is(got, errSwapUnsupported) {
		t.Errorf("EIO went to the fallback")
	}
}

// Without a swap (EINVAL on an old overlayfs, for example), the replace uses
// hard-link backups: a rename goes through, and a failed replace is still
// taken back.
func TestRename_HardLinkFallback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("replace fails %v", fail), func(t *testing.T) {
			dir := t.TempDir()
			servers, st := sessionsAt(t, dir, 1)
			a := servers[0]
			indexFile(t, st, dir, "lib/alpha.ex", safetyDefA)
			indexFile(t, st, dir, "lib/beta.ex", safetyCallB)
			origSwap, origReplace, origBackup := renameSwap, renameReplace, renameBackup
			t.Cleanup(func() { renameSwap, renameReplace, renameBackup = origSwap, origReplace, origBackup })
			renameSwap = func(string, string) error { return swapError(unix.EINVAL) }
			var backups atomic.Int32
			renameBackup = func(o, n string) error { backups.Add(1); return origBackup(o, n) }
			renameReplace = func(from, to string) error {
				if fail && strings.HasSuffix(to, "beta.ex") {
					return errors.New("input/output error")
				}
				return origReplace(from, to)
			}
			_, err := a.RenameEdit(context.Background(), renameParams(string(uri.File(filepath.Join(dir, "lib/alpha.ex"))), 1, 6, "bar"))
			a.index.backgroundWork.Wait()
			if n := backups.Load(); n != 2 {
				t.Errorf("%d hard-link backups, want 2", n)
			}
			if fail {
				if err == nil || !strings.Contains(err.Error(), "no file was changed") {
					t.Fatalf("want the all-or-nothing refusal, got %v", err)
				}
				if got := readFile(t, filepath.Join(dir, "lib/alpha.ex")); got != safetyDefA {
					t.Errorf("alpha.ex was not restored:\n%s", got)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := readFile(t, filepath.Join(dir, "lib/beta.ex")); !strings.Contains(got, "Alpha.bar(1)") {
					t.Errorf("beta.ex not renamed:\n%s", got)
				}
			}
			noTempFiles(t, dir)
		})
	}
}
