package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// A new index has incremental vacuum, so that ReclaimFreePages can give the
// pages that a rebuild frees back in small steps.
func TestNewIndexHasIncrementalVacuum(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
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
	defer func() { _ = s.Close() }()
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
	defer func() { _ = s.Close() }()
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
	defer func() { _ = s.Close() }()
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

// A reclaim that meets a writer waits for it on its own connection: readers
// of the pool are not blocked meanwhile, and the reclaim ends after the
// writer.
func TestReclaimFreePagesWaitsForAWriterOutsideThePool(t *testing.T) {
	oldStep, oldMin, oldPause := reclaimStepPages, reclaimMinBytes, reclaimBusyPause
	reclaimStepPages, reclaimMinBytes, reclaimBusyPause = 64, 0, 50*time.Millisecond
	defer func() { reclaimStepPages, reclaimMinBytes, reclaimBusyPause = oldStep, oldMin, oldPause }()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	fillAndDrop(t, s, 4000)

	// A writer holds the write lock on one of the pool's two connections.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO metadata (key, value) VALUES ('writer', 'x')"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.ReclaimFreePages(context.Background())
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	// A reader gets the pool's other connection at once.
	start := time.Now()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM files").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("a read waited %s while the reclaim waited for the writer", d)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("reclaim after the writer: %v", err)
	}
	var free int
	if err := s.db.QueryRow("PRAGMA freelist_count").Scan(&free); err != nil {
		t.Fatal(err)
	}
	if free != 0 {
		t.Errorf("%d free pages left after the writer ended", free)
	}
}

// A read that needs a new connection of the pool while a write runs does not
// wait for the write lock (WAL readers never do).
func TestNewConnectionDoesNotWaitForAWriter(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	s.db.SetMaxIdleConns(0) // the read below must open a new connection
	defer s.db.SetMaxIdleConns(2)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO metadata (key, value) VALUES ('writer', 'x')"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var n int
	err = s.db.QueryRow("SELECT count(*) FROM files").Scan(&n)
	if d := time.Since(start); err != nil || d > 500*time.Millisecond {
		t.Errorf("read on a new connection during a write: err %v after %s", err, d)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// One step frees reclaimStepPages pages in its transaction, not one:
// incremental_vacuum frees a page for each step of the statement.
func TestReclaimStepFreesAWholeStep(t *testing.T) {
	oldStep := reclaimStepPages
	reclaimStepPages = 64
	defer func() { reclaimStepPages = oldStep }()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	fillAndDrop(t, s, 2000)
	db, err := sql.Open(driverName, s.path+fmt.Sprintf("?_busy_timeout=%d", reclaimBusyMs))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var before, after int
	if err := db.QueryRow("PRAGMA freelist_count").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before < 200 {
		t.Fatalf("only %d free pages to test with", before)
	}
	if err := reclaimStep(context.Background(), db, reclaimStepPages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA freelist_count").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before-after != 64 {
		t.Errorf("one step freed %d pages, want 64", before-after)
	}
}
