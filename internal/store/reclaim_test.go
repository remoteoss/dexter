package store

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A new index has incremental vacuum, so that ReclaimFreePages can give the
// pages that a rebuild frees back in small steps.
func TestNewIndexHasIncrementalVacuum(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mode int
	if err := s.db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (incremental)", mode)
	}
}

func TestReclaimFreePagesShrinksTheFile(t *testing.T) {
	oldStep, oldMin := reclaimStepPages, reclaimMinBytes
	reclaimStepPages, reclaimMinBytes = 64, 0
	defer func() { reclaimStepPages, reclaimMinBytes = oldStep, oldMin }()

	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fillAndDrop(t, s, 4000)
	before := fileSize(t, s.path)

	freed, err := s.ReclaimFreePages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var free int
	if err := s.db.QueryRow("PRAGMA freelist_count").Scan(&free); err != nil {
		t.Fatal(err)
	}
	after := fileSize(t, s.path)
	if free != 0 || freed < 4<<20 || after >= before/4 {
		t.Fatalf("freed %d bytes, %d free pages left, file %d -> %d bytes", freed, free, before, after)
	}
	if gen := s.WriteGeneration(); gen != 0 {
		// Moving pages changes no row: caches must not be dropped.
		t.Errorf("write generation = %d after the reclaim, want 0", gen)
	}
}

// Free pages under the threshold stay: the next write uses them.
func TestReclaimFreePagesKeepsAFewFreePages(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec("CREATE TABLE filler (v TEXT); INSERT INTO filler VALUES ('x'); DROP TABLE filler"); err != nil {
		t.Fatal(err)
	}
	if freed, err := s.ReclaimFreePages(context.Background()); err != nil || freed != 0 {
		t.Fatalf("freed %d, err %v; want nothing under the threshold", freed, err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// A canceled reclaim stops at once: the daemon cancels it when it closes.
func TestReclaimFreePagesStopsWhenCanceled(t *testing.T) {
	oldMin := reclaimMinBytes
	reclaimMinBytes = 0
	defer func() { reclaimMinBytes = oldMin }()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fillAndDrop(t, s, 2000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ReclaimFreePages(ctx); err == nil {
		t.Fatal("a canceled reclaim returned no error")
	}
}

// fillAndDrop writes about n*2 KB to a table and drops it, as a rebuild drops
// the old symbol tables.
func fillAndDrop(t *testing.T, s *Store, n int) {
	t.Helper()
	pad := strings.Repeat("x", 2000)
	if _, err := s.db.Exec("CREATE TABLE filler (v TEXT)"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec("INSERT INTO filler VALUES (?)", pad); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE filler"); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}
}
