package store

import (
	"fmt"
	"os"
	"testing"

	"github.com/remoteoss/dexter/internal/parser"
)

// TestConnectionsHaveJournalSizeLimit verifies the ConnectHook applies
// PRAGMA journal_size_limit to pooled connections. Without it, SQLite never
// truncates the -wal file after a checkpoint and it grows without bound.
func TestConnectionsHaveJournalSizeLimit(t *testing.T) {
	s, _ := setupTestStore(t)
	defer func() { _ = s.Close() }()

	var limit int64
	if err := s.db.QueryRow("PRAGMA journal_size_limit").Scan(&limit); err != nil {
		t.Fatal(err)
	}
	if limit != walSizeLimitBytes {
		t.Fatalf("journal_size_limit = %d, want %d", limit, walSizeLimitBytes)
	}
}

// TestCheckpointTruncatesWAL verifies that Checkpoint() collapses a populated
// WAL back to zero bytes. This is the post-reindex reclaim that stops the
// multi-GB -wal files we saw in the wild.
func TestCheckpointTruncatesWAL(t *testing.T) {
	s, dir := setupTestStore(t)
	defer func() { _ = s.Close() }()

	// Write enough rows to push the WAL well past its empty size.
	batch, err := s.BeginBatch()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		path := fmt.Sprintf("%s/lib/f%d.ex", dir, i)
		defs := []parser.Definition{{
			Module:   fmt.Sprintf("MyApp.Mod%d", i),
			Function: "run",
			Arity:    1,
			Kind:     "def",
			Line:     1,
			FilePath: path,
		}}
		if err := batch.IndexFileWithMtime(path, int64(i), defs); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	walPath := DBPath(dir) + "-wal"
	if info, err := os.Stat(walPath); err != nil || info.Size() == 0 {
		t.Fatalf("expected a non-empty WAL before checkpoint (size err=%v)", err)
	}

	if err := s.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	// TRUNCATE checkpoint with no concurrent readers shrinks the file to 0.
	info, err := os.Stat(walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return // removed entirely is also fine
		}
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("WAL not truncated after Checkpoint: %d bytes", info.Size())
	}

	// Sanity: data survived the checkpoint (it was flushed into the main DB).
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM definitions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3000 {
		t.Fatalf("expected 3000 definitions to survive checkpoint, got %d", count)
	}
}
