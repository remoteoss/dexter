package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/remoteoss/dexter/internal/parser"
)

// synthRows returns the rows of a generated file. version changes every row,
// so a test can tell an old copy of a file from a new one.
func synthRows(i, version int) ([]parser.Definition, []parser.Reference) {
	mod := fmt.Sprintf("MyApp.Mod%d", i)
	defs := []parser.Definition{
		{Module: mod, Kind: "module", Line: 1},
		{Module: mod, Function: fmt.Sprintf("run_v%d", version), Arity: 1, Kind: "def", Line: 2, Params: "id"},
		{Module: mod, Function: "helper", Arity: 0, Kind: "defp", Line: 3 + version},
	}
	refs := []parser.Reference{
		{Module: "SharedLib.Worker", Function: fmt.Sprintf("call%d", i%7), Line: 2, Kind: "call"},
		{Module: "SharedLib.Worker", Function: "perform", Line: 3 + version, Kind: "call"},
		{Module: "MyApp.Accounts", Line: 4, Kind: "alias"},
	}
	return defs, refs
}

func synthPath(dir string, i int) string {
	return filepath.Join(dir, "lib", fmt.Sprintf("mod%d.ex", i))
}

// populate writes files [0, n) at version 0 through one incremental batch.
func populate(t *testing.T, s *Store, dir string, n int) {
	t.Helper()
	b, err := s.BeginBatch()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		defs, refs := synthRows(i, 0)
		if err := b.IndexFileWithMtimeAndRefs(synthPath(dir, i), int64(i+1), defs, refs); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
}

// dump returns every row of the index keyed by path rather than file id, so two
// stores that hold the same content compare equal.
func dump(t *testing.T, s *Store, root string) []string {
	t.Helper()
	var out []string
	collect := func(q string) {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		cols, _ := rows.Columns()
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			parts := make([]string, len(vals))
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				parts[i] = strings.TrimPrefix(fmt.Sprint(v), root)
			}
			out = append(out, strings.Join(parts, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	collect("SELECT 'file', path, mtime FROM files")
	collect("SELECT 'def', f.path, d.module, d.function, d.arity, d.kind, d.line, d.delegate_to, d.delegate_as, d.params FROM definitions d JOIN files f ON f.id = d.file_id")
	collect("SELECT 'ref', f.path, r.module, r.function, r.line, r.kind FROM refs r JOIN files f ON f.id = r.file_id")
	collect("SELECT 'orphan-def', file_id FROM definitions WHERE file_id NOT IN (SELECT id FROM files)")
	collect("SELECT 'orphan-ref', file_id FROM refs WHERE file_id NOT IN (SELECT id FROM files)")
	sort.Strings(out)
	return out
}

func schemaObjects(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT type || ' ' || name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func withRebuildThreshold(t *testing.T, minFiles, share int) {
	t.Helper()
	oldMin, oldShare := rebuildMinFiles, rebuildShare
	rebuildMinFiles, rebuildShare = minFiles, share
	t.Cleanup(func() { rebuildMinFiles, rebuildShare = oldMin, oldShare })
}

// Both removal strategies must leave exactly the same index: the rows of every
// other file, no orphans, and every table and index in place.
func TestRemoveFiles_DeleteAndRebuildAgree(t *testing.T) {
	const n = 2000
	var results [][]string
	for _, mode := range []string{"delete", "rebuild"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "delete" {
				withRebuildThreshold(t, 1<<30, 4)
			} else {
				withRebuildThreshold(t, 1, 4)
			}
			s, dir := setupTestStore(t)
			populate(t, s, dir, n)
			before := schemaObjects(t, s)

			// More than one id chunk, plus a path that was never indexed.
			var remove []string
			for i := 1; i < n; i += 2 {
				remove = append(remove, synthPath(dir, i))
			}
			remove = append(remove, filepath.Join(dir, "lib", "never_indexed.ex"))
			if err := s.RemoveFiles(remove); err != nil {
				t.Fatal(err)
			}

			got := dump(t, s, dir)
			for _, row := range got {
				if strings.HasPrefix(row, "orphan") {
					t.Fatalf("removal left an orphan row: %s", row)
				}
			}
			if after := schemaObjects(t, s); !reflect.DeepEqual(after, before) {
				t.Errorf("schema changed:\nbefore %v\nafter  %v", before, after)
			}
			if r, _ := s.LookupFunction("MyApp.Mod3", "run_v0"); len(r) != 0 {
				t.Error("a removed file's definition is still found")
			}
			if r, _ := s.LookupFunction("MyApp.Mod4", "run_v0"); len(r) != 1 {
				t.Errorf("a kept file's definition: got %d results, want 1", len(r))
			}
			results = append(results, got)
		})
	}
	if len(results) == 2 && !reflect.DeepEqual(results[0], results[1]) {
		t.Error("the delete and rebuild strategies left different rows")
	}
}

// A rebuild batch must produce the same index as the incremental batch that
// it replaces for large change sets: changed files replaced, new files added,
// untouched files copied, and existing files keeping their id.
func TestRebuildMatchesIncremental(t *testing.T) {
	const n = 120
	apply := func(t *testing.T, rebuild bool) ([]string, *Store, string) {
		s, dir := setupTestStore(t)
		populate(t, s, dir, n)
		var b *Batch
		var err error
		if rebuild {
			b, err = s.BeginRebuild(context.Background())
		} else {
			b, err = s.BeginBatch()
		}
		if err != nil {
			t.Fatal(err)
		}
		// Change every other existing file and add as many new ones.
		for i := 0; i < n*2; i += 2 {
			defs, refs := synthRows(i, 1)
			if err := b.IndexFileWithMtimeAndRefs(synthPath(dir, i), int64(1000+i), defs, refs); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.Commit(); err != nil {
			t.Fatal(err)
		}
		return dump(t, s, dir), s, dir
	}

	incremental, _, _ := apply(t, false)
	rebuilt, s, dir := apply(t, true)
	if !reflect.DeepEqual(incremental, rebuilt) {
		t.Fatalf("rebuild and incremental results differ (%d vs %d rows)", len(rebuilt), len(incremental))
	}

	names, err := s.IndexNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 6 {
		t.Errorf("indexes after rebuild: %v", names)
	}
	var id int64
	if err := s.db.QueryRow("SELECT id FROM files WHERE path = ?", synthPath(dir, 1)).Scan(&id); err != nil || id != 2 {
		t.Errorf("an untouched file changed id: %d (%v)", id, err)
	}
	if r, _ := s.LookupFunction("MyApp.Mod2", "run_v1"); len(r) != 1 {
		t.Error("changed file does not have its new rows")
	}
	if r, _ := s.LookupFunction("MyApp.Mod2", "run_v0"); len(r) != 0 {
		t.Error("changed file kept its old rows")
	}
}

func TestRebuildRejectsSecondWriteOfAFile(t *testing.T) {
	s, dir := setupTestStore(t)
	b, err := s.BeginRebuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Rollback() }()
	defs, refs := synthRows(1, 0)
	if err := b.IndexFileWithMtimeAndRefs(synthPath(dir, 1), 1, defs, refs); err != nil {
		t.Fatal(err)
	}
	if err := b.IndexFileWithMtimeAndRefs(synthPath(dir, 1), 2, defs, refs); err == nil {
		t.Fatal("a second write of one path in a rebuild was accepted")
	}
}

// A canceled rebuild rolls back completely: the old rows, tables, and indexes
// stay, and no scratch table is left behind.
func TestRebuildCanceledLeavesIndexUnchanged(t *testing.T) {
	s, dir := setupTestStore(t)
	populate(t, s, dir, 50)
	before := dump(t, s, dir)
	schema := schemaObjects(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	b, err := s.BeginRebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defs, refs := synthRows(1, 9)
	if err := b.IndexFileWithMtimeAndRefs(synthPath(dir, 1), 99, defs, refs); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := b.Commit(); err == nil {
		t.Fatal("commit of a canceled rebuild succeeded")
	}

	if got := dump(t, s, dir); !reflect.DeepEqual(got, before) {
		t.Error("a canceled rebuild changed the index")
	}
	if got := schemaObjects(t, s); !reflect.DeepEqual(got, schema) {
		t.Errorf("a canceled rebuild changed the schema: %v", got)
	}
}

func TestRemoveFilesContextCanceledRemovesNothing(t *testing.T) {
	withRebuildThreshold(t, 1, 4)
	s, dir := setupTestStore(t)
	populate(t, s, dir, 20)
	before := dump(t, s, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.RemoveFilesContext(ctx, []string{synthPath(dir, 1), synthPath(dir, 2)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := dump(t, s, dir); !reflect.DeepEqual(got, before) {
		t.Error("a canceled removal changed the index")
	}
}

// Incremental batches buffer rows across files. A file written twice in one
// batch must still end with only its last version.
func TestIncrementalBatchSecondWriteReplacesBufferedRows(t *testing.T) {
	s, dir := setupTestStore(t)
	b, err := s.BeginBatch()
	if err != nil {
		t.Fatal(err)
	}
	for v := 0; v < 2; v++ {
		defs, refs := synthRows(1, v)
		if err := b.IndexFileWithMtimeAndRefs(synthPath(dir, 1), int64(v+1), defs, refs); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.LookupFunction("MyApp.Mod1", "run_v0"); len(r) != 0 {
		t.Error("the first version's rows survived the second write")
	}
	var defs, refs int
	if err := s.db.QueryRow("SELECT (SELECT COUNT(*) FROM definitions), (SELECT COUNT(*) FROM refs)").Scan(&defs, &refs); err != nil {
		t.Fatal(err)
	}
	if defs != 3 || refs != 3 {
		t.Errorf("rows = %d defs, %d refs; want 3 and 3", defs, refs)
	}
}

func TestFileStates(t *testing.T) {
	s, dir := setupTestStore(t)
	populate(t, s, dir, 3)
	states, err := s.FileStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 3 {
		t.Fatalf("got %d states, want 3", len(states))
	}
	st := states[synthPath(dir, 2)]
	if st.Mtime != 3 || st.ID != 3 {
		t.Errorf("state = %+v, want id 3, mtime 3", st)
	}
}

// holdWriteLock opens a second connection to the same database and holds a
// write transaction on it for d. It returns when the lock is held.
func holdWriteLock(t *testing.T, dir string, d time.Duration) <-chan struct{} {
	t.Helper()
	other, err := sql.Open(driverName, DBPath(dir)+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO metadata (key, value) VALUES ('other_writer', 'x')"); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(d)
		_ = tx.Commit()
		close(released)
	}()
	return released
}

// A rebuild that starts while another connection writes must wait for it, as
// every other writer does, and not fail at once with "database is locked".
func TestRebuildWaitsForConcurrentWriter(t *testing.T) {
	s, dir := setupTestStore(t)
	populate(t, s, dir, 20)
	released := holdWriteLock(t, dir, 300*time.Millisecond)

	b, err := s.BeginRebuild(context.Background())
	if err != nil {
		t.Fatalf("BeginRebuild with a concurrent writer: %v", err)
	}
	select {
	case <-released:
	default:
		t.Error("BeginRebuild got the write lock while another connection held it")
	}
	defs, refs := synthRows(1, 5)
	if err := b.IndexFileWithMtimeAndRefs(synthPath(dir, 1), 50, defs, refs); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.LookupFunction("MyApp.Mod1", "run_v5"); len(r) != 1 {
		t.Error("the rebuild did not write its file")
	}

	// A removal large enough to rebuild also waits and succeeds.
	withRebuildThreshold(t, 1, 4)
	holdWriteLock(t, dir, 300*time.Millisecond)
	var remove []string
	for i := 0; i < 15; i++ {
		remove = append(remove, synthPath(dir, i))
	}
	if err := s.RemoveFiles(remove); err != nil {
		t.Fatalf("removal with a concurrent writer: %v", err)
	}
	if paths, _ := s.ListFilePaths(); len(paths) != 5 {
		t.Errorf("%d files left after the removal, want 5", len(paths))
	}
}
