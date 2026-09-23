package impact

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneImpactCacheBoundsAgeAndSize(t *testing.T) {
	cache := t.TempDir()
	old := filepath.Join(cache, "old.db")
	first := filepath.Join(cache, "first.db")
	retained := filepath.Join(cache, "retained.db")
	createSparseFile(t, old, 1)
	createSparseFile(t, first, 600*1024*1024)
	createSparseFile(t, retained, 600*1024*1024)
	oldTime := time.Now().Add(-defaultCacheMaxAge - time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	firstTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(first, firstTime, firstTime); err != nil {
		t.Fatal(err)
	}

	pruneImpactCache(cache, map[string]struct{}{retained: {}})
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("expired snapshot still exists: %v", err)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("oldest snapshot above size budget still exists: %v", err)
	}
	if _, err := os.Stat(retained); err != nil {
		t.Fatalf("active snapshot was removed: %v", err)
	}
}

func createSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
