package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/remoteoss/dexter/internal/parser"
)

// walSizeLimitBytes caps the on-disk size of the SQLite WAL file. SQLite does
// not shrink the -wal file on its own: after a checkpoint it reuses the frames
// in place, so without a size limit the file stays parked at its all-time
// high-water mark forever (we have seen multi-GB -wal files from a single large
// reindex). PRAGMA journal_size_limit tells SQLite to truncate the WAL back
// down to this size after each checkpoint.
const walSizeLimitBytes = 64 * 1024 * 1024 // 64 MiB

// driverName is a SQLite driver variant registered with a ConnectHook that
// applies journal_size_limit to every pooled connection. database/sql opens
// connections lazily and may create new ones over the lifetime of the pool, so
// a one-shot PRAGMA after Open would not cover later connections; the hook
// guarantees every connection is capped.
const driverName = "sqlite3_dexter"

var registerDriverOnce sync.Once

func registerDriver() {
	registerDriverOnce.Do(func() {
		sql.Register(driverName, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				_, err := conn.Exec(fmt.Sprintf("PRAGMA journal_size_limit = %d", walSizeLimitBytes), nil)
				return err
			},
		})
	})
}

type Store struct {
	db    *sql.DB
	path  string
	inode uint64 // of the database file at open; see SameDatabaseFile

	// gen counts the write transactions that ended, committed or not. A
	// cache that read the index records it before the read, and reads the
	// index again only when it moved. See WriteGeneration.
	gen atomic.Uint64
}

// Path returns the database file this store opened.
func (s *Store) Path() string { return s.path }

// WriteGeneration returns a number that changes after each write to the
// files, definitions, or refs tables. It changes after the write ends, so a
// reader that records it before a read and finds it unchanged later knows
// that no write landed in between.
func (s *Store) WriteGeneration() uint64 { return s.gen.Load() }

func (s *Store) wrote() { s.gen.Add(1) }

// DBPath returns the canonical database path for a project root:
// <root>/.dexter/dexter.db
func DBPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".dexter", "dexter.db")
}

// DBDir returns the directory that holds the database: <root>/.dexter
func DBDir(projectRoot string) string {
	return filepath.Join(projectRoot, ".dexter")
}

// LegacyDBPath returns the pre-migration database path: <root>/.dexter.db
// Used only for detecting and deleting databases created before the
// .dexter/ folder layout.
func LegacyDBPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".dexter.db")
}

// FindProjectRoot walks up from path looking for known dexter/project
// markers. The default markers (in priority order) are:
//
//  1. .dexter/dexter.db — the current database layout
//  2. .dexter.db         — the legacy layout (pre-.dexter/ folder)
//  3. .git               — repository root fallback
//
// Additional markers can be passed via extraMarkers; they are tried after
// the defaults, in the order given. The CLI passes "mix.exs" to fall back
// to the nearest Mix project when no dexter/git marker is found.
//
// The search never climbs above a linked git worktree: one checked out inside
// another checkout (e.g. .claude/worktrees/<name>) is its own project, and
// finding the enclosing checkout's database first would serve the wrong files.
//
// Returns the original path if no marker is found.
func FindProjectRoot(path string, extraMarkers ...string) string {
	markers := append([]string{
		filepath.Join(".dexter", "dexter.db"),
		".dexter.db",
		".git",
	}, extraMarkers...)

	for _, marker := range markers {
		dir := path
		for {
			if validProjectMarker(filepath.Join(dir, marker), marker) {
				return dir
			}
			if parser.IsLinkedWorktree(dir) {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return path
}

func validProjectMarker(path, marker string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	switch marker {
	case ".git":
		// Linked worktrees and submodules use a regular .git file.
		return info.IsDir() || info.Mode().IsRegular()
	default:
		return info.Mode().IsRegular()
	}
}

func Open(projectRoot string) (*Store, error) {
	if err := migrateLegacyLayout(projectRoot); err != nil {
		return nil, fmt.Errorf("migrate legacy layout: %w", err)
	}
	dexterDir := DBDir(projectRoot)
	if err := os.MkdirAll(dexterDir, 0o755); err != nil {
		return nil, fmt.Errorf("create dexter dir: %w", err)
	}
	gitignorePath := filepath.Join(dexterDir, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		_ = os.WriteFile(gitignorePath, []byte("*\n"), 0o644)
	}

	dbPath := DBPath(projectRoot)
	registerDriver()
	db, err := sql.Open(driverName, dbPath+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_foreign_keys=ON")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &Store{db: db, path: dbPath}
	s.recordInode()
	return s, nil
}

// migrateLegacyLayout deletes any pre-.dexter/ folder artifacts so that a
// fresh database will be built at the new location on the next Open. The
// index is a derived cache, so deletion (rather than move) is safe and
// avoids WAL/SHM consistency edge cases.
//
// Returns nil when there is nothing to migrate.
func migrateLegacyLayout(projectRoot string) error {
	legacy := LegacyDBPath(projectRoot)
	if _, err := os.Stat(legacy); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat legacy db %s: %w", legacy, err)
	}
	for _, f := range []string{legacy, legacy + "-shm", legacy + "-wal"} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", f, err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Checkpoint runs a TRUNCATE WAL checkpoint: it flushes committed WAL frames
// into the main database file and then shrinks the -wal file back to zero
// bytes. Call it after a large indexing pass so the WAL does not linger at its
// high-water mark for the lifetime of the process. It is best-effort — if
// another connection is mid-read the checkpoint may complete only partially,
// in which case a later checkpoint (or the journal_size_limit applied on every
// connection) reclaims the rest.
func (s *Store) Checkpoint() error {
	_, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// SetBulkPragmas configures SQLite for maximum write throughput. Safe only
// when the database is fresh and throwaway-on-crash (e.g. a forced full
// rebuild): synchronous=OFF skips all fsyncs, journal_mode=MEMORY keeps the
// rollback journal in RAM so no -wal file is written during the bulk load, a
// large cache keeps pages in RAM during index creation, and temp_store=MEMORY
// keeps sort temporaries off disk.
//
// journal_mode goes first because it is the only one of the four that can fail:
// leaving WAL needs exclusive access, so any other open connection makes it
// return "database is locked". Applying it first means a locked database changes
// nothing at all. With synchronous first, that failure used to leave fsync
// disabled on the connection for the rest of the process — harmless in a CLI
// that exits, not harmless in a server. The LSP does not call this on its live
// pool at all; see indexer.Options.InProcess.
func (s *Store) SetBulkPragmas() error {
	for _, pragma := range []string{
		"PRAGMA journal_mode = MEMORY",
		"PRAGMA synchronous = OFF",
		"PRAGMA cache_size = -2000000",
		"PRAGMA temp_store = MEMORY",
	} {
		if _, err := s.db.Exec(pragma); err != nil {
			return fmt.Errorf("%s: %w", pragma, err)
		}
	}
	return nil
}

// dropPreFileIDSchema removes the pre-file_id tables so migrate can recreate
// them. Definitions and refs used to carry the full absolute file_path on every
// row: 122 characters repeated across 3.9M refs on a large monorepo, plus a
// second copy inside idx_refs_file_path, which together were over half the
// database. Both tables now carry an integer file_id into files(id).
//
// CREATE TABLE IF NOT EXISTS would silently keep the old shape, so the old
// tables are dropped outright. The index is a derived cache and IndexVersion is
// bumped alongside this change, so the server rebuilds from source on the next
// start.
func dropPreFileIDSchema(db *sql.DB) error {
	var sqlText sql.NullString
	err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'refs'").Scan(&sqlText)
	if err == sql.ErrNoRows {
		return nil // fresh database
	}
	if err != nil {
		return err
	}
	if !sqlText.Valid || (strings.Contains(sqlText.String, "file_id") && strings.Contains(sqlText.String, "arity")) {
		return nil // already migrated
	}
	_, err = db.Exec(`
		DROP TABLE IF EXISTS refs;
		DROP TABLE IF EXISTS definitions;
		DROP TABLE IF EXISTS files;
	`)
	return err
}

func migrate(db *sql.DB) error {
	if err := dropPreFileIDSchema(db); err != nil {
		return err
	}
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS files (
			id INTEGER PRIMARY KEY,
			path TEXT NOT NULL UNIQUE,
			mtime INTEGER NOT NULL
		);
	` + createDefinitionsTable("definitions") + createRefsTable("refs") + `
		CREATE TABLE IF NOT EXISTS metadata (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	// idx_refs_function_kind was retired: no query leads with `function`, so
	// SQLite never chose it (the two queries that filter on function/kind both
	// lead with file_path and use idx_refs_file_path). On a 3.9M-row index it
	// cost 80 MB and a share of every index rebuild. Drop it from databases
	// that still carry it; this changes no query plan.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_refs_function_kind`); err != nil {
		return err
	}
	return createIndexes(db)
}

// createDefinitionsTable and createRefsTable return the DDL of the two symbol
// tables under the given name. migrate creates them, and rebuildWithout creates
// a copy with the same shape before it swaps the copy in. One definition keeps
// the two from drifting apart.
func createDefinitionsTable(name string) string {
	return `
		CREATE TABLE IF NOT EXISTS ` + name + ` (
			module TEXT NOT NULL,
			function TEXT NOT NULL DEFAULT '',
			arity INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL,
			line INTEGER NOT NULL,
			file_id INTEGER NOT NULL,
			delegate_to TEXT NOT NULL DEFAULT '',
			delegate_as TEXT NOT NULL DEFAULT '',
			params TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (file_id) REFERENCES files(id) ON DELETE CASCADE
		);
	`
}

func createRefsTable(name string) string {
	return `
		CREATE TABLE IF NOT EXISTS ` + name + ` (
			module TEXT NOT NULL,
			function TEXT NOT NULL DEFAULT '',
			line INTEGER NOT NULL,
			file_id INTEGER NOT NULL,
			kind TEXT NOT NULL DEFAULT 'call',
			-- The arity of a call, or -1: not read, or not a function ref.
			arity INTEGER NOT NULL DEFAULT -1
			-- No FOREIGN KEY here on purpose. With _foreign_keys=ON every insert
			-- costs a parent-key lookup, and refs is the table the cold index
			-- spends most of its writer time on (3.9M rows on a large monorepo).
			-- Every path that removes a file deletes its refs explicitly first,
			-- so the cascade was never the thing keeping them consistent.
		);
	`
}

// fileIDSubquery resolves a path argument to files.id inside a WHERE clause, so
// call sites keep passing paths and only the SQL changes. files.path is UNIQUE,
// so this is one index probe.
const fileIDSubquery = "(SELECT id FROM files WHERE path = ?)"

type dbExecer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func createIndexes(db dbExecer) error {
	_, err := db.Exec(createIndexesSQL)
	return err
}

const createIndexesSQL = `
		CREATE INDEX IF NOT EXISTS idx_definitions_module_function ON definitions(module, function);
		CREATE INDEX IF NOT EXISTS idx_definitions_file_id_line ON definitions(file_id, line);
		CREATE INDEX IF NOT EXISTS idx_refs_module_function ON refs(module, function, file_id, line, kind, arity);
		CREATE INDEX IF NOT EXISTS idx_refs_file_id ON refs(file_id);
		CREATE INDEX IF NOT EXISTS idx_definitions_delegate_to ON definitions(delegate_to);
		-- LookupUsingModules runs on the References slow path. Without this it
		-- scans every definition in the index (481k entries on a large monorepo)
		-- to find a few hundred rows, because idx_definitions_module_function
		-- leads with module and cannot be seeked by function alone. The partial
		-- index holds only the __using__ rows, so the scan becomes a small range.
		CREATE INDEX IF NOT EXISTS idx_definitions_using ON definitions(module, file_id) WHERE function = '__using__';
	`

// dropIndexesSQL removes every index the symbol tables have had, current and
// retired, so that a bulk load writes to tables with no index to update.
const dropIndexesSQL = `
		DROP INDEX IF EXISTS idx_definitions_module;
		DROP INDEX IF EXISTS idx_definitions_module_function;
		DROP INDEX IF EXISTS idx_definitions_file_path;
		DROP INDEX IF EXISTS idx_definitions_file_path_line;
		DROP INDEX IF EXISTS idx_definitions_file_id_line;
		DROP INDEX IF EXISTS idx_refs_module_function;
		DROP INDEX IF EXISTS idx_refs_file_path;
		DROP INDEX IF EXISTS idx_refs_file_id;
		DROP INDEX IF EXISTS idx_refs_function_kind;
		DROP INDEX IF EXISTS idx_definitions_delegate_to;
		DROP INDEX IF EXISTS idx_definitions_using;
	`

func (s *Store) CreateIndexes() error {
	return createIndexes(s.db)
}

// IndexNames returns the names of dexter's own indexes, as the database
// currently has them. The bulk load drops them in its transaction and builds
// them again before it commits, so tests use this to check that readers always
// see them.
func (s *Store) IndexNames() ([]string, error) {
	rows, err := s.db.Query("SELECT name FROM sqlite_master WHERE type = 'index' AND name LIKE 'idx_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// GetIndexVersion returns the index version stored in the database, or 0 if
// none has been recorded yet (e.g. an index created before versioning was added).
func (s *Store) GetIndexVersion() int {
	var value string
	err := s.db.QueryRow("SELECT value FROM metadata WHERE key = 'index_version'").Scan(&value)
	if err != nil {
		return 0
	}
	v, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return v
}

// SetIndexVersion records the index version in the database. Call this after a
// successful full index so that future startups can detect stale indexes.
func (s *Store) SetIndexVersion(v int) error {
	_, err := s.db.Exec("INSERT OR REPLACE INTO metadata (key, value) VALUES ('index_version', ?)", strconv.Itoa(v))
	return err
}

// GetStdlibRoot returns the cached Elixir stdlib lib root, if any.
func (s *Store) GetStdlibRoot() (string, bool) {
	var value string
	err := s.db.QueryRow("SELECT value FROM metadata WHERE key = 'stdlib_root'").Scan(&value)
	if err != nil || value == "" {
		return "", false
	}
	return value, true
}

// SetStdlibRoot persists the detected Elixir stdlib lib root.
func (s *Store) SetStdlibRoot(root string) error {
	_, err := s.db.Exec("INSERT OR REPLACE INTO metadata (key, value) VALUES ('stdlib_root', ?)", root)
	return err
}

// IsEmpty reports whether the index holds no files. It answers false when the
// query itself fails, because every caller uses this to decide whether a
// destructive or insert-only path is safe: a database too broken to count is
// the one case where those paths must not run. migrate() only issues CREATE
// ... IF NOT EXISTS, so it succeeds on a store whose `files` pages are corrupt
// and this is the first place that notices.
func (s *Store) IsEmpty() bool {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM files").Scan(&count)
	return err == nil && count == 0
}

func (s *Store) GetFileMtime(path string) (int64, bool) {
	var mtime int64
	err := s.db.QueryRow("SELECT mtime FROM files WHERE path = ?", path).Scan(&mtime)
	if err != nil {
		return 0, false
	}
	return mtime, true
}

// FileMtimes returns the stored mtime of each of paths that the index has, in
// a few queries rather than one for each path.
func (s *Store) FileMtimes(paths []string) (map[string]int64, error) {
	out := make(map[string]int64, len(paths))
	for start := 0; start < len(paths); start += maxBindVars {
		chunk := paths[start:min(start+maxBindVars, len(paths))]
		args := make([]interface{}, len(chunk))
		for i, p := range chunk {
			args[i] = p
		}
		rows, err := s.db.Query("SELECT path, mtime FROM files WHERE path IN ("+placeholders(len(chunk))+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			var m int64
			if err := rows.Scan(&p, &m); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[p] = m
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) IndexFile(path string, defs []parser.Definition) error {
	return s.IndexFileWithRefs(path, defs, nil)
}

func (s *Store) IndexFileWithRefs(path string, defs []parser.Definition, refs []parser.Reference) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return s.IndexFileWithRefsAt(path, info.ModTime().UnixNano(), defs, refs)
}

// IndexFileWithRefsAt is IndexFileWithRefs with the mtime that the caller
// read before it parsed the file. A save between that stat and the parse then
// leaves the row older than the disk, and the next pass reads it again; a stat
// after the parse would give the old text the new mtime.
func (s *Store) IndexFileWithRefsAt(path string, mtimeNano int64, defs []parser.Definition, refs []parser.Reference) error {
	defer s.wrote()
	return s.indexFileWithMtime(path, mtimeNano, defs, refs)
}

// BufferMtime is the mtime stored for a file indexed from an editor buffer
// rather than from disk. No file on disk has it, so the next reconciliation
// that looks at the file reads it from disk again. A buffer row that carried
// the disk file's mtime would look current forever: if the editor then threw
// the buffer away (undo, reload, close without saving), the index would keep
// text that is on no disk.
const BufferMtime int64 = -1

// IndexBufferWithRefs indexes text that is not on disk, an editor buffer, for
// path. The row gets BufferMtime. The file does not have to exist yet.
func (s *Store) IndexBufferWithRefs(path string, defs []parser.Definition, refs []parser.Reference) error {
	defer s.wrote()
	return s.indexFileWithMtime(path, BufferMtime, defs, refs)
}

func (s *Store) indexFileWithMtime(path string, mtimeNano int64, defs []parser.Definition, refs []parser.Reference) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	fileID, err := upsertFileID(tx, path, mtimeNano)
	if err != nil {
		return err
	}

	if _, err := tx.Exec("DELETE FROM definitions WHERE file_id = ?", fileID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM refs WHERE file_id = ?", fileID); err != nil {
		return err
	}

	defStmt, err := tx.Prepare("INSERT INTO definitions (module, function, arity, kind, line, file_id, delegate_to, delegate_as, params) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer func() { _ = defStmt.Close() }()

	for _, d := range defs {
		if _, err := defStmt.Exec(d.Module, d.Function, d.Arity, d.Kind, d.Line, fileID, d.DelegateTo, d.DelegateAs, d.Params); err != nil {
			return err
		}
	}

	if len(refs) > 0 {
		refStmt, err := tx.Prepare("INSERT INTO refs (" + refInsertColumns + ") VALUES (?, ?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer func() { _ = refStmt.Close() }()

		for _, r := range refs {
			if _, err := refStmt.Exec(r.Module, r.Function, r.Line, fileID, r.Kind, refArity(r)); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// txExecQuerier is the subset of *sql.Tx that upsertFileID needs.
type txExecQuerier interface {
	QueryRow(query string, args ...interface{}) *sql.Row
}

// upsertFileID returns the files.id for path, inserting the row or refreshing
// its mtime as needed. The UPSERT keeps the existing id, which matters because
// definitions and refs point at it: INSERT OR REPLACE would delete the old row
// and hand out a new id, orphaning (or cascading away) every row for the file.
func upsertFileID(tx txExecQuerier, path string, mtimeNano int64) (int64, error) {
	var id int64
	err := tx.QueryRow(
		"INSERT INTO files (path, mtime) VALUES (?, ?) ON CONFLICT(path) DO UPDATE SET mtime = excluded.mtime RETURNING id",
		path, mtimeNano,
	).Scan(&id)
	return id, err
}

// Row counts for the multi-row INSERT statements used by the bulk path. Each
// chunk stays under 900 bound parameters, which is below even the legacy
// SQLITE_MAX_VARIABLE_NUMBER of 999, so the batch size never depends on how
// the driver's SQLite was compiled.
const (
	defColumns   = 9
	refColumns   = 6
	maxBindVars  = 900
	defChunkRows = maxBindVars / defColumns // 100
	refChunkRows = maxBindVars / refColumns // 150
)

// batchMode selects how a Batch writes.
type batchMode uint8

const (
	// batchIncremental replaces each file's rows in the live tables: an upsert
	// of the files row, a DELETE by file id, and buffered INSERTs.
	batchIncremental batchMode = iota
	// batchInsertOnly fills empty tables with their indexes dropped, and
	// builds the indexes at Commit. See BeginBulkInsert.
	batchInsertOnly
	// batchRebuild writes into new, unindexed copies of the symbol tables and
	// swaps them in at Commit. See BeginRebuild.
	batchRebuild
)

// Batch wraps multiple IndexFile operations in a single SQLite transaction
// with shared prepared statements.
type Batch struct {
	store      *Store
	ctx        context.Context
	tx         *sql.Tx
	mode       batchMode
	defStmt    *sql.Stmt
	refStmt    *sql.Stmt
	fileStmt   *sql.Stmt
	delDefStmt *sql.Stmt // incremental mode only
	delRefStmt *sql.Stmt // incremental mode only
	skipStmt   *sql.Stmt // rebuild mode only

	// Multi-row INSERT buffers. A cold index writes ~4.4M rows through one
	// connection, and the writer is the bottleneck of the whole indexing
	// pipeline; batching turns ~4.4M cgo crossings into a few tens of
	// thousands. A large warm change set (a new checkout of a whole tree, for
	// example) writes as many rows through the other modes, so they buffer
	// too. Rows buffered for one file never meet another file's DELETE, which
	// targets only that file's id; a file seen twice in one incremental batch
	// flushes the buffer first, so its DELETE stays ordered ahead of its earlier
	// INSERTs.
	defChunkStmt *sql.Stmt
	refChunkStmt *sql.Stmt
	defArgs      []interface{}
	refArgs      []interface{}

	// written holds the file ids this batch has written, in the incremental and
	// rebuild modes. An incremental batch flushes before a second write of one
	// file; a rebuild rejects it, because its new tables have no index to find
	// the earlier rows by.
	written map[int64]struct{}

	// Bulk-path file id allocation. Insert-only mode assigns ids in Go from a
	// counter and writes files rows with an explicit id, so a cold index never
	// pays a round trip per file to learn what id it just wrote.
	nextFileID int64

	// indexBuild is how long Commit took to build the indexes, in
	// insert-only mode.
	indexBuild time.Duration
}

// multiRowInsert builds "INSERT INTO <table> (<cols>) VALUES (?,..),(?,..)" for
// exactly rows tuples of n placeholders each.
func multiRowInsert(table, columns string, n, rows int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" (")
	b.WriteString(columns)
	b.WriteString(") VALUES ")
	tuple := "(?" + strings.Repeat(",?", n-1) + ")"
	for i := 0; i < rows; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(tuple)
	}
	return b.String()
}

func (s *Store) BeginBatch() (*Batch, error) {
	return s.beginBatch(context.Background(), batchIncremental)
}

// BeginBatchContext starts an incremental batch whose transaction rolls back
// when ctx is canceled. Writes after that fail, and Commit reports the
// cancellation, so none of the batch's files is left half written.
func (s *Store) BeginBatchContext(ctx context.Context) (*Batch, error) {
	return s.beginBatch(ctx, batchIncremental)
}

// BeginBulkInsert starts a batch optimized for inserting into empty tables.
// It skips DELETE statements before each insert, and drops the indexes of the
// symbol tables in its transaction, so that no insert updates an index.
//
// Commit builds the indexes again, each with one sort, before it commits. The
// rows and their indexes become visible to readers together: until the
// commit, a reader sees the database as it was, with its indexes. A reader
// that saw the rows without the indexes would scan the full table on each
// lookup, which takes seconds on a large project. A rollback restores the
// dropped indexes.
func (s *Store) BeginBulkInsert() (*Batch, error) {
	return s.beginBatch(context.Background(), batchInsertOnly)
}

// BeginRebuild starts a batch that rewrites the symbol tables in one
// transaction, for a change set too large to apply row by row to the live
// indexes. Each index of the live tables sorts rows by name, so the rows of one
// file land on pages all over it, and a write of a large share of the index
// touches most of its pages, many times.
//
// Files written to the batch go into new tables that have no indexes. Commit
// then copies in the rows of every file the batch did not write or remove,
// swaps the new tables in for the old ones, and builds each index with one
// sort, as a cold build does. Readers keep their snapshot of the old tables
// until the commit, and a canceled or failed rebuild rolls back to exactly the
// old state. Each path may be written only once.
func (s *Store) BeginRebuild(ctx context.Context) (*Batch, error) {
	return s.beginBatch(ctx, batchRebuild)
}

const (
	defInsertColumns = "module, function, arity, kind, line, file_id, delegate_to, delegate_as, params"
	refInsertColumns = "module, function, line, file_id, kind, arity"
)

func (s *Store) beginBatch(ctx context.Context, mode batchMode) (_ *Batch, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	b := &Batch{store: s, ctx: ctx, tx: tx, mode: mode}
	defer func() {
		if err != nil {
			b.closeStmts()
			_ = tx.Rollback()
		}
	}()

	defTable, refTable := "definitions", "refs"
	if mode != batchIncremental {
		// Take the write lock first. The transaction is deferred, so a DROP
		// ... IF EXISTS of an object that is not there would open it as a
		// read, and the next write would have to upgrade that read to a
		// write. SQLite does not call the busy handler for that upgrade: with
		// another writer active, the batch would fail at once with "database
		// is locked" instead of waiting for busy_timeout. A write statement
		// as the first statement waits like every other writer.
		if _, err = tx.ExecContext(ctx, "DELETE FROM metadata WHERE key = 'rebuild_lock'"); err != nil {
			return nil, err
		}
	}
	if mode == batchInsertOnly {
		if _, err = tx.ExecContext(ctx, dropIndexesSQL); err != nil {
			return nil, err
		}
	}
	if mode == batchRebuild {
		defTable, refTable = "definitions_rebuild", "refs_rebuild"
		// A rolled-back rebuild leaves nothing behind, but a copy from a
		// process that died mid-way would make CREATE ... IF NOT EXISTS keep
		// its rows.
		if _, err = tx.ExecContext(ctx, `
			DROP TABLE IF EXISTS temp.rebuild_skip;
			CREATE TEMP TABLE rebuild_skip (id INTEGER PRIMARY KEY, removed INTEGER NOT NULL DEFAULT 0);
			DROP TABLE IF EXISTS definitions_rebuild;
			DROP TABLE IF EXISTS refs_rebuild;
		`+createDefinitionsTable(defTable)+createRefsTable(refTable)); err != nil {
			return nil, err
		}
		if b.skipStmt, err = tx.Prepare("INSERT OR IGNORE INTO temp.rebuild_skip (id) VALUES (?)"); err != nil {
			return nil, err
		}
	}

	if b.defStmt, err = tx.Prepare("INSERT INTO " + defTable + " (" + defInsertColumns + ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)"); err != nil {
		return nil, err
	}
	if b.refStmt, err = tx.Prepare("INSERT INTO " + refTable + " (" + refInsertColumns + ") VALUES (?, ?, ?, ?, ?, ?)"); err != nil {
		return nil, err
	}
	fileSQL := "INSERT INTO files (path, mtime) VALUES (?, ?) ON CONFLICT(path) DO UPDATE SET mtime = excluded.mtime RETURNING id"
	if mode == batchInsertOnly {
		fileSQL = "INSERT INTO files (id, path, mtime) VALUES (?, ?, ?)"
	}
	if b.fileStmt, err = tx.Prepare(fileSQL); err != nil {
		return nil, err
	}
	if mode == batchIncremental {
		if b.delDefStmt, err = tx.Prepare("DELETE FROM definitions WHERE file_id = ?"); err != nil {
			return nil, err
		}
		if b.delRefStmt, err = tx.Prepare("DELETE FROM refs WHERE file_id = ?"); err != nil {
			return nil, err
		}
	}
	if mode != batchInsertOnly {
		b.written = make(map[int64]struct{})
	}
	if b.defChunkStmt, err = tx.Prepare(multiRowInsert(defTable, defInsertColumns, defColumns, defChunkRows)); err != nil {
		return nil, err
	}
	if b.refChunkStmt, err = tx.Prepare(multiRowInsert(refTable, refInsertColumns, refColumns, refChunkRows)); err != nil {
		return nil, err
	}
	b.defArgs = make([]interface{}, 0, defColumns*defChunkRows)
	b.refArgs = make([]interface{}, 0, refColumns*refChunkRows)
	if mode == batchInsertOnly {
		// Bulk mode is used on a freshly created database, but seed from the
		// table anyway so a non-empty one cannot collide on the primary key.
		if err = tx.QueryRow("SELECT COALESCE(MAX(id), 0) FROM files").Scan(&b.nextFileID); err != nil {
			return nil, err
		}
	}
	return b, nil
}

func (b *Batch) IndexFile(path string, defs []parser.Definition) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	return b.indexFile(path, info.ModTime().UnixNano(), defs, nil)
}

func (b *Batch) IndexFileWithMtime(path string, mtimeNano int64, defs []parser.Definition) error {
	return b.indexFile(path, mtimeNano, defs, nil)
}

func (b *Batch) IndexFileWithMtimeAndRefs(path string, mtimeNano int64, defs []parser.Definition, refs []parser.Reference) error {
	return b.indexFile(path, mtimeNano, defs, refs)
}

func (b *Batch) indexFile(path string, mtimeNano int64, defs []parser.Definition, refs []parser.Reference) error {
	fileID, err := b.fileID(path, mtimeNano)
	if err != nil {
		return err
	}

	switch b.mode {
	case batchIncremental:
		if _, again := b.written[fileID]; again {
			if err := b.flushPending(); err != nil {
				return err
			}
		}
		b.written[fileID] = struct{}{}
		if _, err := b.delDefStmt.Exec(fileID); err != nil {
			return err
		}
		if _, err := b.delRefStmt.Exec(fileID); err != nil {
			return err
		}
	case batchRebuild:
		if _, again := b.written[fileID]; again {
			return fmt.Errorf("rebuild: %s written twice", path)
		}
		b.written[fileID] = struct{}{}
		// The old rows of this file are not copied into the new tables.
		if _, err := b.skipStmt.Exec(fileID); err != nil {
			return err
		}
	}

	// Reset the buffer before checking the error: the chunk boundary is an
	// exact-multiple test, so a buffer left full would never match again and
	// the rest of the batch would silently fall back to flushPending.
	for _, d := range defs {
		b.defArgs = append(b.defArgs, d.Module, d.Function, d.Arity, d.Kind, d.Line, fileID, d.DelegateTo, d.DelegateAs, d.Params)
		if len(b.defArgs) == defColumns*defChunkRows {
			_, err := b.defChunkStmt.Exec(b.defArgs...)
			b.defArgs = b.defArgs[:0]
			if err != nil {
				return err
			}
		}
	}
	for _, r := range refs {
		b.refArgs = append(b.refArgs, r.Module, r.Function, r.Line, fileID, r.Kind, refArity(r))
		if len(b.refArgs) == refColumns*refChunkRows {
			_, err := b.refChunkStmt.Exec(b.refArgs...)
			b.refArgs = b.refArgs[:0]
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// RemoveIDs marks files for removal in a rebuild: their rows are not copied
// into the new tables, and their files rows go at Commit.
func (b *Batch) RemoveIDs(ids []int64) error {
	if b.mode != batchRebuild {
		return errors.New("RemoveIDs needs a rebuild batch")
	}
	for start := 0; start < len(ids); start += maxBindVars {
		chunk := ids[start:min(start+maxBindVars, len(ids))]
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		if _, err := b.tx.ExecContext(b.ctx,
			"INSERT OR REPLACE INTO temp.rebuild_skip (id, removed) VALUES "+strings.TrimSuffix(strings.Repeat("(?,1),", len(chunk)), ","),
			args...); err != nil {
			return err
		}
	}
	return nil
}

// fileID writes the files row for path and returns its id.
//
// The bulk path allocates ids from a counter and inserts them explicitly: a
// cold index writes ~70k files, and asking SQLite to hand back each id would
// add a round trip per file to the one thread that is already the bottleneck.
// The other modes upsert, so an existing file keeps the id that its
// definitions and refs already point at.
func (b *Batch) fileID(path string, mtimeNano int64) (int64, error) {
	if b.mode == batchInsertOnly {
		b.nextFileID++
		if _, err := b.fileStmt.Exec(b.nextFileID, path, mtimeNano); err != nil {
			return 0, err
		}
		return b.nextFileID, nil
	}
	var id int64
	err := b.fileStmt.QueryRow(path, mtimeNano).Scan(&id)
	return id, err
}

// flushPending writes any rows left over from a partly filled chunk, one row at
// a time through the single-row statements.
func (b *Batch) flushPending() error {
	for i := 0; i < len(b.defArgs); i += defColumns {
		if _, err := b.defStmt.Exec(b.defArgs[i : i+defColumns]...); err != nil {
			return err
		}
	}
	b.defArgs = b.defArgs[:0]
	for i := 0; i < len(b.refArgs); i += refColumns {
		if _, err := b.refStmt.Exec(b.refArgs[i : i+refColumns]...); err != nil {
			return err
		}
	}
	b.refArgs = b.refArgs[:0]
	return nil
}

func (b *Batch) Commit() error {
	defer b.store.wrote()
	err := b.flushPending()
	b.closeStmts()
	if err == nil {
		switch b.mode {
		case batchRebuild:
			err = b.finishRebuild()
		case batchInsertOnly:
			start := time.Now()
			_, err = b.tx.ExecContext(b.ctx, createIndexesSQL)
			b.indexBuild = time.Since(start)
		}
	}
	if err != nil {
		_ = b.tx.Rollback()
		return err
	}
	return b.tx.Commit()
}

// IndexBuildTime returns how long Commit took to build the indexes of a bulk
// insert batch. The commit time includes it.
func (b *Batch) IndexBuildTime() time.Duration {
	return b.indexBuild
}

// finishRebuild copies the rows of every file the rebuild did not write or
// remove into the new tables, swaps them in, builds the indexes, and deletes
// the removed files. Every statement runs under the batch context, so a cancel
// interrupts the one in progress.
//
// Dropping a table that only references files (definitions) or nothing (refs)
// does no foreign key work, and the indexes go with their tables.
func (b *Batch) finishRebuild() error {
	const keep = " WHERE file_id NOT IN (SELECT id FROM temp.rebuild_skip)"
	for _, q := range []string{
		"INSERT INTO definitions_rebuild (" + defInsertColumns + ") SELECT " + defInsertColumns + " FROM definitions" + keep,
		"DROP TABLE definitions",
		"ALTER TABLE definitions_rebuild RENAME TO definitions",
		"INSERT INTO refs_rebuild (" + refInsertColumns + ") SELECT " + refInsertColumns + " FROM refs" + keep,
		"DROP TABLE refs",
		"ALTER TABLE refs_rebuild RENAME TO refs",
		createIndexesSQL,
		"DELETE FROM files WHERE id IN (SELECT id FROM temp.rebuild_skip WHERE removed = 1)",
		"DROP TABLE temp.rebuild_skip",
	} {
		if _, err := b.tx.ExecContext(b.ctx, q); err != nil {
			return err
		}
	}
	return nil
}

func (b *Batch) Rollback() error {
	b.closeStmts()
	return b.tx.Rollback()
}

func (b *Batch) closeStmts() {
	for _, st := range []*sql.Stmt{b.defStmt, b.refStmt, b.fileStmt, b.defChunkStmt, b.refChunkStmt, b.delDefStmt, b.delRefStmt, b.skipStmt} {
		if st != nil {
			_ = st.Close()
		}
	}
}

func (s *Store) ListFilePaths() ([]string, error) {
	rows, err := s.db.Query("SELECT path FROM files")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

// ListFilePathsUnder returns the indexed files below the directory dir. Like
// HasPath, it reads only the matching range of the unique path index instead of
// every stored path.
func (s *Store) ListFilePathsUnder(dir string) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT path FROM files WHERE path >= ? AND path < ?",
		dir+string(os.PathSeparator), dir+string(rune(os.PathSeparator+1)),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

// HasPath reports whether path is an indexed file or a directory holding one.
// The range on the unique path index finds a descendant without listing every
// file: sep+1 is the first byte after the separator, so [prefix, upper) spans
// exactly the paths under the directory.
func (s *Store) HasPath(path string) (bool, error) {
	prefix := path + string(os.PathSeparator)
	upper := path + string(rune(os.PathSeparator+1))
	var found int
	err := s.db.QueryRow(
		"SELECT 1 FROM files WHERE path = ? OR (path >= ? AND path < ?) LIMIT 1",
		path, prefix, upper,
	).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) RemoveFile(path string) error {
	return s.RemoveFiles([]string{path})
}

// RemoveFiles removes the given paths and every row that belongs to them.
// Paths that are not indexed are ignored.
func (s *Store) RemoveFiles(paths []string) error {
	return s.RemoveFilesContext(context.Background(), paths)
}

// RemoveFilesContext is RemoveFiles with cancellation. It resolves the paths to
// file ids with one query per chunk and removes the ids as sets; see
// RemoveFileIDs.
func (s *Store) RemoveFilesContext(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	defer s.wrote()
	ids := make([]int64, 0, len(paths))
	for start := 0; start < len(paths); start += maxBindVars {
		chunk := paths[min(start, len(paths)):min(start+maxBindVars, len(paths))]
		args := make([]interface{}, len(chunk))
		for i, p := range chunk {
			args[i] = p
		}
		rows, err := s.db.QueryContext(ctx, "SELECT id FROM files WHERE path IN ("+placeholders(len(chunk))+")", args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
	}
	return s.RemoveFileIDs(ctx, ids)
}

// FileState is what the index records about one file: its row id and the
// modification time the stored symbols were parsed from.
type FileState struct {
	ID    int64
	Mtime int64
}

// FileStates returns every indexed file with its id and stored mtime, read in
// one query. A reconciliation pass compares the walk against it instead of
// issuing one query per file, and removes the files the walk did not see by id.
func (s *Store) FileStates() (map[string]FileState, error) {
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM files").Scan(&n); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT path, id, mtime FROM files")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	states := make(map[string]FileState, n)
	for rows.Next() {
		var path string
		var st FileState
		if err := rows.Scan(&path, &st.ID, &st.Mtime); err != nil {
			return nil, err
		}
		states[path] = st
	}
	return states, rows.Err()
}

// Removal strategy. Deleting a file's rows costs one random B-tree update per
// row in every index on the table: idx_refs_module_function sorts by name, so
// the refs of one file are spread over the whole index. Removing a nested
// checkout of a large monorepo is millions of such updates. Copying the rows
// that stay into a new table and indexing it afresh costs one sequential read of
// the table and one sort per index instead, which is far cheaper once a large
// share of the rows goes. Both are measured on a 280k-file index, where removing
// 223k files took 31s as set-based DELETEs and 15s as a rebuild (and 66s as the
// former per-path DELETEs).
var (
	// rebuildMinFiles keeps small removals on the DELETE path, whatever the
	// share: a rebuild always reads both tables in full. A variable so tests
	// can reach the rebuild with a small index.
	rebuildMinFiles = 4096
	// rebuildShare is the share of indexed files, as 1/rebuildShare, at or
	// above which a removal rebuilds the symbol tables.
	rebuildShare = 4
)

const (
	// deleteGroupIDs is how many files one DELETE transaction removes. Each
	// group commits on its own, so a canceled removal keeps the groups already
	// done, and a group is small enough to commit quickly.
	deleteGroupIDs = 8 * maxBindVars
)

// RemoveFileIDs removes the files with the given ids and all their rows. Each
// file goes in one transaction with its definitions and refs, so no file is
// ever left half removed, also when ctx is canceled part way through.
//
// A removal of a large share of the index rebuilds the symbol tables without
// the removed files (see rebuildWithout); a smaller one deletes by id sets.
func (s *Store) RemoveFileIDs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	defer s.wrote()
	if len(ids) >= rebuildMinFiles {
		var total int
		if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM files").Scan(&total); err != nil {
			return err
		}
		if len(ids)*rebuildShare >= total {
			err := s.rebuildWithout(ctx, ids)
			if err == nil || ctx.Err() != nil {
				return err
			}
			// The DELETE path is slower but needs nothing the rebuild did,
			// so a failed rebuild does not leave the files in the index.
			log.Printf("Warning: index rebuild for %d removed files failed, deleting them by id: %v", len(ids), err)
		}
	}
	for start := 0; start < len(ids); start += deleteGroupIDs {
		if err := s.deleteFileIDs(ctx, ids[start:min(start+deleteGroupIDs, len(ids))]); err != nil {
			return err
		}
	}
	return nil
}

// deleteFileIDs deletes one group of files in one transaction, by id sets of at
// most maxBindVars. Sorting lets each statement walk idx_refs_file_id and the
// definitions index in key order.
func (s *Store) deleteFileIDs(ctx context.Context, ids []int64) error {
	sorted := append([]int64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for start := 0; start < len(sorted); start += maxBindVars {
		chunk := sorted[start:min(start+maxBindVars, len(sorted))]
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		in := "(" + placeholders(len(chunk)) + ")"
		for _, q := range []string{
			"DELETE FROM definitions WHERE file_id IN " + in,
			"DELETE FROM refs WHERE file_id IN " + in,
			"DELETE FROM files WHERE id IN " + in,
		} {
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// rebuildWithout removes the given files by rebuilding the symbol tables
// without them. See BeginRebuild.
func (s *Store) rebuildWithout(ctx context.Context, ids []int64) error {
	b, err := s.BeginRebuild(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = b.Rollback() }() // after Commit it does nothing
	if err := b.RemoveIDs(ids); err != nil {
		_ = b.Rollback()
		return err
	}
	return b.Commit()
}

// placeholders returns n comma-separated "?" markers.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

type CompletionResult struct {
	Module   string
	Function string
	Arity    int
	Kind     string
	FilePath string
	Line     int
	Params   string
}

// SearchModulesBySuffix returns modules whose name ends with the given suffix.
// For example, "Accounts" matches "MyApp.Accounts" and "RandomAPI.Client"
// matches "MyApp.RandomAPI.Client".
// ModuleNamesBySuffix returns up to limit names of modules, protocols, and
// impls that are segment or end in "."+segment, in order. With no kind
// filter the scan reads idx_definitions_module_function alone, which is
// faster than SearchModulesBySuffix on a large index.
func (s *Store) ModuleNamesBySuffix(segment string, limit int) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT DISTINCT module FROM definitions WHERE function = '' AND (module = ? OR module LIKE ?) ORDER BY module LIMIT ?",
		segment, "%."+segment, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SearchModulesBySuffix(segment string) ([]CompletionResult, error) {
	rows, err := s.db.Query(
		"SELECT DISTINCT module FROM definitions WHERE (module = ? OR module LIKE ?) AND function = '' AND kind IN ('module', 'defprotocol') ORDER BY module LIMIT 20",
		segment, "%."+segment,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []CompletionResult
	for rows.Next() {
		var r CompletionResult
		if err := rows.Scan(&r.Module); err != nil {
			return nil, err
		}
		r.Kind = "module"
		results = append(results, r)
	}
	return results, rows.Err()
}

func (s *Store) SearchModules(prefix string) ([]CompletionResult, error) {
	rows, err := s.db.Query(
		"SELECT DISTINCT module FROM definitions WHERE module LIKE ? AND function = '' AND kind IN ('module', 'defprotocol') ORDER BY module LIMIT 100",
		prefix+"%",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []CompletionResult
	for rows.Next() {
		var r CompletionResult
		if err := rows.Scan(&r.Module); err != nil {
			return nil, err
		}
		r.Kind = "module"
		results = append(results, r)
	}
	return results, rows.Err()
}

// SearchSubmoduleSegments returns distinct immediate child segments under parentModule
// that match an optional segment prefix. For example, with parentModule="MyApp" and
// segmentPrefix="S", it returns segments like "Services", "Schema", etc. without
// the LIMIT 100 truncation issue that affects SearchModules + client-side dedup.
func (s *Store) SearchSubmoduleSegments(parentModule string, segmentPrefix string) ([]string, error) {
	// Build prefix: "MyApp.S%"
	likePrefix := parentModule + "." + segmentPrefix + "%"

	rows, err := s.db.Query(
		"SELECT DISTINCT module FROM definitions WHERE module LIKE ? AND function = '' AND kind IN ('module', 'defprotocol') ORDER BY module",
		likePrefix,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	parentDot := parentModule + "."
	seen := make(map[string]bool)
	var segments []string
	for rows.Next() {
		var module string
		if err := rows.Scan(&module); err != nil {
			return nil, err
		}
		segment := strings.TrimPrefix(module, parentDot)
		if dot := strings.IndexByte(segment, '.'); dot >= 0 {
			segment = segment[:dot]
		}
		if !seen[segment] {
			seen[segment] = true
			segments = append(segments, segment)
			if len(segments) >= 100 {
				break
			}
		}
	}
	return segments, rows.Err()
}

func (s *Store) ListModuleFunctions(module string, publicOnly bool) ([]CompletionResult, error) {
	query := "SELECT d.module, d.function, d.arity, d.kind, f.path, d.line, d.params FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module = ? AND d.function != '' AND d.kind NOT IN ('callback', 'macrocallback')"
	if publicOnly {
		query += " AND kind IN ('def', 'defmacro', 'defguard', 'defdelegate', 'defstruct', 'defexception', 'type', 'opaque')"
	}
	query += " GROUP BY function, arity ORDER BY function, arity LIMIT 100"

	rows, err := s.db.Query(query, module)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []CompletionResult
	for rows.Next() {
		var r CompletionResult
		if err := rows.Scan(&r.Module, &r.Function, &r.Arity, &r.Kind, &r.FilePath, &r.Line, &r.Params); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// ListModuleCallbacks returns the @callback and @macrocallback definitions of
// the given behaviour module (these are excluded from ListModuleFunctions).
func (s *Store) ListModuleCallbacks(module string) ([]CompletionResult, error) {
	rows, err := s.db.Query(
		"SELECT d.module, d.function, d.arity, d.kind, f.path, d.line, d.params FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module = ? AND d.kind IN ('callback', 'macrocallback') GROUP BY d.function, d.arity ORDER BY d.function, d.arity LIMIT 100",
		module,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []CompletionResult
	for rows.Next() {
		var r CompletionResult
		if err := rows.Scan(&r.Module, &r.Function, &r.Arity, &r.Kind, &r.FilePath, &r.Line, &r.Params); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// IndexStats summarizes the size of the index.
type IndexStats struct {
	Files       int
	Definitions int
	References  int
}

// Stats returns row counts for the files, definitions, and refs tables.
func (s *Store) Stats() (IndexStats, error) {
	var st IndexStats
	for _, q := range []struct {
		query string
		dst   *int
	}{
		{"SELECT COUNT(*) FROM files", &st.Files},
		{"SELECT COUNT(*) FROM definitions", &st.Definitions},
		{"SELECT COUNT(*) FROM refs", &st.References},
	} {
		if err := s.db.QueryRow(q.query).Scan(q.dst); err != nil {
			return IndexStats{}, err
		}
	}
	return st, nil
}

// FunctionKey identifies a function by name and arity.
type FunctionKey struct {
	Name  string
	Arity int
}

// ModuleFunctionKeys returns every public function, macro, guard, delegate and
// type the index knows for a module.
//
// It is the unbounded counterpart to ListModuleFunctions, which caps at 100 rows
// and joins in file paths and params because it feeds completion. Anything
// diffing the index against another source of truth must use this instead: a
// truncated set makes the rows past the cap look unindexed, and whatever is
// derived from that is wrong. The predicate matches ListModuleFunctions with
// publicOnly so the two agree on what counts as public.
//
// Only the two columns needed are selected and no join is performed, so despite
// being unbounded this is cheaper than the completion query. It is covered by
// idx_definitions_module_function.
func (s *Store) ModuleFunctionKeys(module string) ([]FunctionKey, error) {
	rows, err := s.db.Query(
		"SELECT DISTINCT function, arity FROM definitions WHERE module = ? AND function != '' AND kind IN ('def', 'defmacro', 'defguard', 'defdelegate', 'defstruct', 'defexception', 'type', 'opaque')",
		module,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var keys []FunctionKey
	for rows.Next() {
		var key FunctionKey
		if err := rows.Scan(&key.Name, &key.Arity); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

type LookupResult struct {
	Module     string // populated by bulk queries; empty for single-module lookups
	FilePath   string
	Line       int
	Kind       string
	Arity      int
	DelegateTo string
	DelegateAs string
}

func (s *Store) LookupModule(module string) ([]LookupResult, error) {
	return s.queryLookup(
		"SELECT f.path, d.line, d.kind, d.arity, d.delegate_to, d.delegate_as FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module = ? AND d.function = '' AND d.kind IN ('module', 'defprotocol', 'defimpl')",
		module,
	)
}

// ModuleFile returns the file of module's first definition (the one
// LookupModule lists first) and its indexed mtime. ok is false when the
// index has no such module.
func (s *Store) ModuleFile(module string) (path string, mtime int64, ok bool) {
	err := s.db.QueryRow(
		"SELECT f.path, f.mtime FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module = ? AND d.function = '' AND d.kind IN ('module', 'defprotocol', 'defimpl') LIMIT 1",
		module).Scan(&path, &mtime)
	if err != nil {
		return "", 0, false
	}
	return path, mtime, true
}

// LookupModulesInFile returns all module names defined in the given file, in line order.
func (s *Store) LookupModulesInFile(filePath string) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT module FROM definitions WHERE file_id = "+fileIDSubquery+" AND function = '' AND kind IN ('module', 'defprotocol') ORDER BY line",
		filePath,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var modules []string
	for rows.Next() {
		var mod string
		if err := rows.Scan(&mod); err != nil {
			return nil, err
		}
		modules = append(modules, mod)
	}
	return modules, rows.Err()
}

// LookupFunctionInFile returns the module that defines the given function in the
// specified file. Checks the enclosing module at nearLine first (respecting module
// boundaries), then falls back to any other module in the file.
func (s *Store) LookupFunctionInFile(filePath, function string, nearLine int) (string, bool) {
	// Try the enclosing module first — this is the correct scope for bare calls
	enclosing := s.LookupEnclosingModule(filePath, nearLine)
	if enclosing != "" {
		var count int
		if err := s.db.QueryRow(
			"SELECT COUNT(*) FROM definitions WHERE module = ? AND function = ? AND kind NOT IN ('module', 'defprotocol', 'defimpl', 'callback', 'macrocallback')",
			enclosing, function,
		).Scan(&count); err == nil && count > 0 {
			return enclosing, true
		}
	}

	// Fall back to any module in this file that defines the function (handles
	// calls to parent-scope functions from nested modules)
	var module string
	err := s.db.QueryRow(
		"SELECT d.module FROM definitions d "+
			"WHERE d.file_id = "+fileIDSubquery+" AND d.function = ? AND d.kind NOT IN ('module', 'defprotocol', 'defimpl', 'callback', 'macrocallback') "+
			"LIMIT 1",
		filePath, function,
	).Scan(&module)
	if err != nil {
		return "", false
	}
	return module, true
}

func (s *Store) LookupFunction(module, function string) ([]LookupResult, error) {
	return s.queryLookup(
		"SELECT f.path, d.line, d.kind, d.arity, d.delegate_to, d.delegate_as FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module = ? AND d.function = ? AND d.kind NOT IN ('module', 'defprotocol', 'defimpl', 'callback', 'macrocallback') ORDER BY CASE WHEN d.kind IN ('type', 'opaque') THEN 1 ELSE 0 END, d.line",
		module, function,
	)
}

// LookupPublicFunction returns callable definitions that can be referenced
// outside module. Keep this as an allowlist: a newly indexed private kind must
// not silently become visible through imports, use chains, or Kernel fallback.
func (s *Store) LookupPublicFunction(module, function string) ([]LookupResult, error) {
	return s.queryLookup(
		"SELECT f.path, d.line, d.kind, d.arity, d.delegate_to, d.delegate_as FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module = ? AND d.function = ? AND d.kind IN ('def', 'defmacro', 'defguard', 'defdelegate', 'defstruct', 'defexception', 'type', 'opaque') ORDER BY CASE WHEN d.kind IN ('type', 'opaque') THEN 1 ELSE 0 END, d.line",
		module, function,
	)
}

// CallbackResult holds a @callback or @macrocallback definition with its arity.
type CallbackResult struct {
	FilePath string
	Line     int
	Kind     string
	Arity    int
}

// LookupCallbackDef returns @callback and @macrocallback definitions for a given behaviour module and function name.
func (s *Store) LookupCallbackDef(behaviourModule, function string) ([]CallbackResult, error) {
	rows, err := s.db.Query(
		"SELECT f.path, d.line, d.kind, d.arity FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module = ? AND d.function = ? AND d.kind IN ('callback', 'macrocallback') ORDER BY d.line",
		behaviourModule, function,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []CallbackResult
	for rows.Next() {
		var r CallbackResult
		if err := rows.Scan(&r.FilePath, &r.Line, &r.Kind, &r.Arity); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// LookupCallbackDefGlobal returns all @callback/@macrocallback definitions with the
// given function name across all indexed modules. When arity >= 0 it filters by
// arity; pass -1 to return all arities. Used as a fallback when the behaviour chain
// can't be resolved statically (e.g. dynamic `use unquote(mod)`).
func (s *Store) LookupCallbackDefGlobal(function string, arity int) ([]CallbackResult, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if arity >= 0 {
		rows, err = s.db.Query(
			"SELECT f.path, d.line, d.kind, d.arity FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.function = ? AND d.arity = ? AND d.kind IN ('callback', 'macrocallback') ORDER BY d.module, d.line",
			function, arity,
		)
	} else {
		rows, err = s.db.Query(
			"SELECT f.path, d.line, d.kind, d.arity FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.function = ? AND d.kind IN ('callback', 'macrocallback') ORDER BY d.module, d.line",
			function,
		)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []CallbackResult
	for rows.Next() {
		var r CallbackResult
		if err := rows.Scan(&r.FilePath, &r.Line, &r.Kind, &r.Arity); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// LookupBehavioursForFile returns the fully-qualified names of all behaviour modules
// referenced by the given file. Includes explicit @behaviour declarations and `use`d
// modules that define at least one @callback (since `use` commonly injects @behaviour).
func (s *Store) LookupBehavioursForFile(filePath string) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT module FROM refs WHERE file_id = (SELECT id FROM files WHERE path = ?) AND kind = 'behaviour'
		UNION
		SELECT r.module FROM refs r
		WHERE r.file_id = (SELECT id FROM files WHERE path = ?) AND r.kind = 'use'
			AND EXISTS (SELECT 1 FROM definitions d WHERE d.module = r.module AND d.kind IN ('callback', 'macrocallback'))
		ORDER BY 1`,
		filePath, filePath,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var modules []string
	for rows.Next() {
		var module string
		if err := rows.Scan(&module); err != nil {
			return nil, err
		}
		modules = append(modules, module)
	}
	return modules, rows.Err()
}

// BehaviourImplementorResult holds an implementing module and its source file.
type BehaviourImplementorResult struct {
	Module   string
	FilePath string
}

// LookupBehaviourImplementors returns all modules that declare @behaviour or `use` the given module.
// Uses a correlated subquery to find the nearest enclosing defmodule for each ref,
// avoiding a cross-product when a file defines multiple modules.
func (s *Store) LookupBehaviourImplementors(behaviourModule string) ([]BehaviourImplementorResult, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT d.module, f.path
		FROM refs r
		JOIN files f ON f.id = r.file_id
		JOIN definitions d ON d.file_id = r.file_id AND d.function = '' AND d.kind IN ('module', 'defprotocol')
			AND d.line = (
				SELECT MAX(d2.line) FROM definitions d2
				WHERE d2.file_id = r.file_id AND d2.function = '' AND d2.kind IN ('module', 'defprotocol') AND d2.line <= r.line
			)
		WHERE r.module = ? AND r.kind IN ('behaviour', 'use')
		ORDER BY d.module`,
		behaviourModule,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []BehaviourImplementorResult
	for rows.Next() {
		var r BehaviourImplementorResult
		if err := rows.Scan(&r.Module, &r.FilePath); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// DeriveResult is an `@derive` of a protocol: the module that derives it and
// the line of the attribute.
type DeriveResult struct {
	Module   string
	FilePath string
	Line     int
}

// LookupDerives returns the modules whose source derives protocol with
// `@derive`. The parser stores each as a "derive" reference to the protocol
// with the deriving module in the function column. Covered by
// idx_refs_module_function.
func (s *Store) LookupDerives(protocol string) ([]DeriveResult, error) {
	rows, err := s.db.Query(
		"SELECT r.function, f.path, r.line FROM refs r JOIN files f ON f.id = r.file_id WHERE r.module = ? AND r.function != '' AND r.kind = 'derive' ORDER BY r.function, f.path, r.line",
		protocol)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeriveResult
	for rows.Next() {
		var r DeriveResult
		if err := rows.Scan(&r.Module, &r.FilePath, &r.Line); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UseLines returns the lines of the `use` calls in a file (every module of
// it, and uses inside quotes too). Covered by idx_refs_file_id.
func (s *Store) UseLines(filePath string) (map[int]bool, error) {
	rows, err := s.db.Query("SELECT line FROM refs WHERE file_id = "+fileIDSubquery+" AND kind = 'use'", filePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	lines := make(map[int]bool)
	for rows.Next() {
		var line int
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		lines[line] = true
	}
	return lines, rows.Err()
}

// LookupUseSites returns the `use Module` lines and the module that each is
// in. It is LookupBehaviourImplementors without @behaviour, and with the line.
func (s *Store) LookupUseSites(module string) ([]DeriveResult, error) {
	rows, err := s.db.Query(`
		SELECT d.module, f.path, r.line
		FROM refs r
		JOIN files f ON f.id = r.file_id
		JOIN definitions d ON d.file_id = r.file_id AND d.function = '' AND d.kind IN ('module', 'defprotocol')
			AND d.line = (
				SELECT MAX(d2.line) FROM definitions d2
				WHERE d2.file_id = r.file_id AND d2.function = '' AND d2.kind IN ('module', 'defprotocol') AND d2.line <= r.line
			)
		WHERE r.module = ? AND r.function = '' AND r.kind = 'use'
		ORDER BY d.module, f.path, r.line`,
		module,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeriveResult
	for rows.Next() {
		var r DeriveResult
		if err := rows.Scan(&r.Module, &r.FilePath, &r.Line); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LookupFunctionInModules returns definitions of a function across multiple modules in a single query.
// If arity >= 0, filters by exact arity match.
func (s *Store) LookupFunctionInModules(modules []string, function string, arity int) ([]LookupResult, error) {
	if len(modules) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(modules))
	args := make([]interface{}, 0, len(modules)+2)
	for i, mod := range modules {
		placeholders[i] = "?"
		args = append(args, mod)
	}
	args = append(args, function)

	query := "SELECT f.path, d.line, d.kind, d.arity, d.delegate_to, d.delegate_as FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module IN (" +
		strings.Join(placeholders, ",") +
		") AND d.function = ? AND d.kind NOT IN ('module', 'defprotocol', 'defimpl', 'callback', 'macrocallback')"

	if arity >= 0 {
		query += " AND d.arity = ?"
		args = append(args, arity)
	}

	query += " ORDER BY d.line"
	return s.queryLookup(query, args...)
}

func (s *Store) queryLookup(query string, args ...interface{}) ([]LookupResult, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []LookupResult
	for rows.Next() {
		var r LookupResult
		if err := rows.Scan(&r.FilePath, &r.Line, &r.Kind, &r.Arity, &r.DelegateTo, &r.DelegateAs); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

type ReferenceResult struct {
	FilePath string
	Line     int
	Kind     string
	Function string // set by LookupReferencesInModules only
	// Arity is the call's arity, or -1 when it is not known (set by
	// LookupReferences and LookupReferencesInModules).
	Arity int
}

// refArity is the arity the store keeps for r: -1 for a ref without a
// function, such as an alias or a use.
func refArity(r parser.Reference) int {
	if r.Function == "" {
		return -1
	}
	return r.Arity
}

// LookupReferencesInModules returns the references to function in any of
// modules, in one query.
func (s *Store) LookupReferencesInModules(modules []string, function string) ([]ReferenceResult, error) {
	if len(modules) == 0 {
		return nil, nil
	}
	var results []ReferenceResult
	// SQLite limits the number of parameters of one statement.
	for start := 0; start < len(modules); start += 500 {
		end := min(start+500, len(modules))
		args := make([]any, 0, end-start+1)
		args = append(args, function)
		for _, m := range modules[start:end] {
			args = append(args, m)
		}
		rows, err := s.db.Query(
			"SELECT f.path, r.line, r.kind, r.function, r.arity FROM refs r JOIN files f ON f.id = r.file_id WHERE r.function = ? AND r.module IN (?"+strings.Repeat(",?", end-start-1)+")",
			args...)
		if err != nil {
			return nil, err
		}
		part, err := scanSortedReferences(rows, true)
		if err != nil {
			return nil, err
		}
		results = append(results, part...)
	}
	return results, nil
}

// ModuleArities returns, for each of modules that defines a function (or a
// macro, a guard, a delegate; not a callback or a type) named function, the
// arities it defines, in one query for each 500 modules.
func (s *Store) ModuleArities(function string, modules []string) (map[string]map[int]bool, error) {
	out := make(map[string]map[int]bool)
	for start := 0; start < len(modules); start += 500 {
		end := min(start+500, len(modules))
		args := make([]any, 0, end-start+1)
		args = append(args, function)
		for _, m := range modules[start:end] {
			args = append(args, m)
		}
		rows, err := s.db.Query(
			"SELECT DISTINCT module, arity FROM definitions WHERE function = ? AND kind IN ('def', 'defp', 'defmacro', 'defmacrop', 'defguard', 'defguardp', 'defdelegate') AND module IN (?"+strings.Repeat(",?", end-start-1)+")",
			args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var m string
			var arity int
			if err := rows.Scan(&m, &arity); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if out[m] == nil {
				out[m] = make(map[int]bool)
			}
			out[m][arity] = true
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scanSortedReferences(rows *sql.Rows, withFunction bool) ([]ReferenceResult, error) {
	defer func() { _ = rows.Close() }()
	var results []ReferenceResult
	for rows.Next() {
		var r ReferenceResult
		var err error
		if withFunction {
			err = rows.Scan(&r.FilePath, &r.Line, &r.Kind, &r.Function, &r.Arity)
		} else {
			err = rows.Scan(&r.FilePath, &r.Line, &r.Kind)
			r.Arity = -1
		}
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].FilePath != results[j].FilePath {
			return results[i].FilePath < results[j].FilePath
		}
		return results[i].Line < results[j].Line
	})
	return results, nil
}

func (s *Store) LookupReferences(module, function string) ([]ReferenceResult, error) {
	// idx_refs_module_function covers (module, function, file_id, line, kind),
	// so this reads the index alone — no row lookup into the refs table, which
	// used to cost one random read per hit (7,749 of them for a hot function on
	// a large monorepo). The join to files resolves ids to paths against a
	// table small enough to stay in page cache.
	//
	// Ordering happens in Go: sorting by f.path in SQL would force a temp
	// B-tree, while the result sets here are small enough that a sort costs
	// microseconds.
	rows, err := s.db.Query(
		"SELECT f.path, r.line, r.kind FROM refs r JOIN files f ON f.id = r.file_id WHERE r.module = ? AND r.function = ?",
		module, function)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []ReferenceResult
	for rows.Next() {
		var r ReferenceResult
		r.Arity = -1
		if err := rows.Scan(&r.FilePath, &r.Line, &r.Kind); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].FilePath != results[j].FilePath {
			return results[i].FilePath < results[j].FilePath
		}
		return results[i].Line < results[j].Line
	})
	return results, nil
}

// LookupReferencesWithArity is LookupReferences for the calls of the arities
// given, and the references whose arity is not known. The arity is checked
// in the covering index, so the rows of another arity cost no join.
func (s *Store) LookupReferencesWithArity(module, function string, arities []int) ([]ReferenceResult, error) {
	if len(arities) == 0 {
		return s.LookupReferences(module, function)
	}
	args := make([]any, 0, len(arities)+2)
	args = append(args, module, function)
	for _, a := range arities {
		args = append(args, a)
	}
	rows, err := s.db.Query(
		"SELECT f.path, r.line, r.kind, r.arity FROM refs r JOIN files f ON f.id = r.file_id WHERE r.module = ? AND r.function = ? AND (r.arity = -1 OR r.arity IN (?"+strings.Repeat(",?", len(arities)-1)+"))",
		args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []ReferenceResult
	for rows.Next() {
		var r ReferenceResult
		if err := rows.Scan(&r.FilePath, &r.Line, &r.Kind, &r.Arity); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].FilePath != results[j].FilePath {
			return results[i].FilePath < results[j].FilePath
		}
		return results[i].Line < results[j].Line
	})
	return results, nil
}

// ModuleReferenceResult is like ReferenceResult but also carries the module
// name, used by bulk prefix queries where refs from multiple modules are returned.
type ModuleReferenceResult struct {
	Module   string
	FilePath string
	Line     int
	Kind     string
}

// LookupReferencesByPrefix returns all refs whose module equals prefix or starts
// with prefix + ".". Used for bulk module renames to avoid N+1 queries.
func (s *Store) LookupReferencesByPrefix(prefix string) ([]ModuleReferenceResult, error) {
	// A range beats LIKE here: LIKE is case-insensitive by default, so SQLite
	// cannot convert `module LIKE 'Prefix.%'` into an index range and falls back
	// to a full scan of refs (3.9M rows on a large monorepo). '/' is '.'+1, so
	// [prefix+"." , prefix+"/") is exactly the set of names under the prefix,
	// and idx_refs_module_function serves it. Elixir module names are
	// case-sensitive, so the stricter comparison is also the correct one.
	rows, err := s.db.Query(
		"SELECT r.module, f.path, r.line, r.kind FROM refs r JOIN files f ON f.id = r.file_id WHERE r.module = ? OR (r.module >= ? AND r.module < ?) ORDER BY f.path, r.line",
		prefix, prefix+".", prefix+"/",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []ModuleReferenceResult
	for rows.Next() {
		var r ModuleReferenceResult
		if err := rows.Scan(&r.Module, &r.FilePath, &r.Line, &r.Kind); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// LookupModulesByPrefix returns all module definitions whose name equals prefix
// or starts with prefix + ".". The Module field is populated on each result.
// Used for bulk module renames to replace N per-module LookupModule calls with one.
func (s *Store) LookupModulesByPrefix(prefix string) ([]LookupResult, error) {
	// Range rather than LIKE, for the same reason as LookupReferencesByPrefix.
	rows, err := s.db.Query(
		"SELECT d.module, f.path, d.line, d.kind, d.arity, d.delegate_to, d.delegate_as FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.function = '' AND (d.module = ? OR (d.module >= ? AND d.module < ?)) AND d.kind IN ('module', 'defprotocol', 'defimpl') ORDER BY d.module",
		prefix, prefix+".", prefix+"/",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []LookupResult
	for rows.Next() {
		var r LookupResult
		if err := rows.Scan(&r.Module, &r.FilePath, &r.Line, &r.Kind, &r.Arity, &r.DelegateTo, &r.DelegateAs); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// DelegateEntry represents a defdelegate definition that forwards to a target module.
type DelegateEntry struct {
	Module     string // the delegating module (facade)
	Function   string // the facade function name
	DelegateAs string // the target function name (empty if same as Function)
	FilePath   string
	Line       int
	Arity      int // the facade arity; a head with defaults has a row for each
}

// LookupDelegatesTo returns all defdelegate definitions that forward the given
// function to the target module. Matches both the no-as: case (function name
// equals target) and the as: case (delegate_as equals target function name).
func (s *Store) LookupDelegatesTo(targetModule, targetFunction string) ([]DelegateEntry, error) {
	rows, err := s.db.Query(
		`SELECT d.module, d.function, d.delegate_as, f.path, d.line, d.arity FROM definitions d JOIN files f ON f.id = d.file_id
		 WHERE d.kind = 'defdelegate' AND d.delegate_to = ? AND d.delegate_as = '' AND d.function = ?
		 UNION ALL
		 SELECT d.module, d.function, d.delegate_as, f.path, d.line, d.arity FROM definitions d JOIN files f ON f.id = d.file_id
		 WHERE d.kind = 'defdelegate' AND d.delegate_to = ? AND d.delegate_as = ?`,
		targetModule, targetFunction, targetModule, targetFunction,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []DelegateEntry
	for rows.Next() {
		var r DelegateEntry
		if err := rows.Scan(&r.Module, &r.Function, &r.DelegateAs, &r.FilePath, &r.Line, &r.Arity); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

func (s *Store) SearchSymbols(query string, excludePathPrefix ...string) ([]CompletionResult, error) {
	return s.searchSymbols(query, 50, excludePathPrefix...)
}

func (s *Store) searchSymbols(query string, limit int, excludePathPrefix ...string) ([]CompletionResult, error) {
	var (
		rows *sql.Rows
		err  error
	)

	// Optional path exclusion (e.g. stdlib root) pushed into SQL so the
	// LIMIT applies to user-visible results, not filtered-out stdlib entries.
	pathFilter := ""
	var extraArgs []interface{}
	if len(excludePathPrefix) > 0 && excludePathPrefix[0] != "" {
		pathFilter = " AND f.path NOT LIKE ?"
		extraArgs = append(extraArgs, excludePathPrefix[0]+"%")
	}

	// When the query contains a dot, split at the last dot. The prefix must
	// match the module and the suffix is matched against both the function name
	// and the module (covering "Accounts.fetch_user" → function match and
	// "MyApp.Accounts" → module match). SQLite LIKE is case-insensitive for
	// ASCII so "accounts.fetch_user" and "Accounts.fetch_user" both work.
	// Results are ranked: exact module match first, then case-sensitive
	// substring match (via INSTR), then the rest alphabetically.
	if dotIndex := strings.LastIndex(query, "."); dotIndex != -1 {
		modulePart := "%" + query[:dotIndex] + "%"
		suffixPart := "%" + query[dotIndex+1:] + "%"
		args := append([]interface{}{modulePart, suffixPart, suffixPart}, extraArgs...)
		args = append(args, query, query, limit)
		rows, err = s.db.Query(
			"SELECT d.module, d.function, d.arity, d.kind, f.path, d.line FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module LIKE ? AND (d.function LIKE ? OR d.module LIKE ?)"+pathFilter+
				" ORDER BY CASE WHEN d.module = ? THEN 0 WHEN INSTR(d.module || '.' || d.function, ?) > 0 THEN 1 ELSE 2 END, d.module, d.function, d.arity LIMIT ?",
			args...,
		)
	} else {
		pattern := "%" + query + "%"
		args := append([]interface{}{pattern, pattern}, extraArgs...)
		args = append(args, query, query, query, limit)
		rows, err = s.db.Query(
			"SELECT d.module, d.function, d.arity, d.kind, f.path, d.line FROM definitions d JOIN files f ON f.id = d.file_id WHERE (d.module LIKE ? OR d.function LIKE ?)"+pathFilter+
				" ORDER BY CASE WHEN d.module = ? THEN 0 WHEN INSTR(d.module, ?) > 0 OR INSTR(d.function, ?) > 0 THEN 1 ELSE 2 END, d.module, d.function, d.arity LIMIT ?",
			args...,
		)
	}

	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []CompletionResult
	for rows.Next() {
		var r CompletionResult
		if err := rows.Scan(&r.Module, &r.Function, &r.Arity, &r.Kind, &r.FilePath, &r.Line); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// SymbolMatch is one function (module, name, arity) in one file that a symbol
// search matched, with its first line and its number of clauses.
type SymbolMatch struct {
	CompletionResult
	Clauses int
}

// SearchSymbolsGrouped is SearchSymbols with one row per module, function,
// arity, and file instead of one per clause; more reports that there are
// further matches. It reads the same limit rows as SearchSymbols (one more,
// to know whether there are more), groups them here, and counts the clauses
// of the functions it shows with one indexed query. A GROUP BY in the search
// itself, or reading more rows, made a common query on a large index slower.
func (s *Store) SearchSymbolsGrouped(query string, limit int, excludePathPrefix ...string) (results []SymbolMatch, more bool, err error) {
	rows, err := s.searchSymbols(query, limit+1, excludePathPrefix...)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		rows, more = rows[:limit], true
	}
	type groupKey struct {
		module, function string
		arity            int
		path             string
	}
	index := make(map[groupKey]int)
	for _, r := range rows {
		key := groupKey{r.Module, r.Function, r.Arity, r.FilePath}
		if at, ok := index[key]; ok {
			results[at].Clauses++
			if r.Line < results[at].Line {
				results[at].Line = r.Line
			}
			continue
		}
		index[key] = len(results)
		results = append(results, SymbolMatch{CompletionResult: r, Clauses: 1})
	}
	// The rows are ordered by module, function, and arity, so every function
	// but the last one read has all its clauses here. The last one's clauses
	// can reach past the rows read, so they are counted.
	for i := range results {
		r := &results[i]
		if r.Function == "" {
			continue
		}
		last := rows[len(rows)-1]
		if r.Module != last.Module || r.Function != last.Function || r.Arity != last.Arity {
			continue
		}
		if !more {
			continue
		}
		var n int
		if err := s.db.QueryRow(
			"SELECT COUNT(*) FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module = ? AND d.function = ? AND d.arity = ? AND f.path = ?",
			r.Module, r.Function, r.Arity, r.FilePath,
		).Scan(&n); err == nil && n > 0 {
			r.Clauses = n
		}
	}
	return results, more, nil
}

// ListSubmodules returns all modules whose name starts with prefix + ".".
// Used for cascading module renames (e.g. renaming MyApp.Accounts also catches MyApp.Accounts.User).
func (s *Store) ListSubmodules(prefix string) ([]string, error) {
	// A range, not LIKE: LIKE ignores case and takes `_` as a wildcard, so
	// MyApp.Api_V1 would match MyApp.ApiXV1.Sub.
	rows, err := s.db.Query(
		"SELECT DISTINCT module FROM definitions WHERE module >= ? AND module < ? AND function = '' ORDER BY module",
		prefix+".", prefix+"/",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var modules []string
	for rows.Next() {
		var mod string
		if err := rows.Scan(&mod); err != nil {
			return nil, err
		}
		modules = append(modules, mod)
	}
	return modules, rows.Err()
}

// UsingModule pairs a module name with the file that defines its __using__ macro.
type UsingModule struct {
	Module   string
	FilePath string
}

// LookupUsingModules returns all modules that define a defmacro __using__
// function, along with their file paths. The result set is typically small.
func (s *Store) LookupUsingModules() ([]UsingModule, error) {
	rows, err := s.db.Query("SELECT DISTINCT d.module, f.path FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.function = '__using__'")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var modules []UsingModule
	for rows.Next() {
		var m UsingModule
		if err := rows.Scan(&m.Module, &m.FilePath); err != nil {
			return nil, err
		}
		modules = append(modules, m)
	}
	return modules, rows.Err()
}

const lookupCaseTemplateModulesQuery = `
	WITH sites AS (
		SELECT r.file_id,
			(
			SELECT d2.module
			FROM definitions d2
			WHERE d2.file_id = r.file_id
				AND d2.function = ''
				AND d2.kind IN ('module', 'defprotocol')
				AND d2.line <= r.line
			ORDER BY d2.line DESC
			LIMIT 1
			) AS nearest
		FROM refs r
		WHERE r.module = 'ExUnit.CaseTemplate'
			AND r.function = ''
			AND r.kind = 'use'
	)
	SELECT DISTINCT d.module, f.path
	FROM sites s
	JOIN files f ON f.id = s.file_id
	JOIN definitions d ON d.file_id = s.file_id
		AND d.function = ''
		AND d.kind IN ('module', 'defprotocol')
		AND (
			d.module = s.nearest
			OR s.nearest >= d.module || '.' AND s.nearest < d.module || '/'
		)
	WHERE s.nearest IS NOT NULL
	ORDER BY d.module`

// LookupCaseTemplateModules returns candidate modules enclosing each
// `use ExUnit.CaseTemplate` site. The nearest preceding module and any of its
// defined ancestors are included: after a nested module closes, line-only
// index data cannot tell whether a later use belongs to it or to its parent,
// and the scoped using parser cheaply rejects the wrong candidate. Unrelated
// sibling modules are excluded. One indexed query avoids the previous N+1.
func (s *Store) LookupCaseTemplateModules() ([]UsingModule, error) {
	rows, err := s.db.Query(lookupCaseTemplateModulesQuery)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var modules []UsingModule
	for rows.Next() {
		var m UsingModule
		if err := rows.Scan(&m.Module, &m.FilePath); err != nil {
			return nil, err
		}
		modules = append(modules, m)
	}
	return modules, rows.Err()
}

// LookupEnclosingModule returns the module name for the nearest defmodule at or before
// lineNum in the given file. Returns "" if none is found.
func (s *Store) LookupEnclosingModule(filePath string, lineNum int) string {
	var module string
	err := s.db.QueryRow(
		"SELECT module FROM definitions WHERE file_id = "+fileIDSubquery+" AND function = '' AND kind IN ('module', 'defprotocol') AND line <= ? ORDER BY line DESC LIMIT 1",
		filePath, lineNum,
	).Scan(&module)
	if err != nil {
		return ""
	}
	return module
}

// LookupEnclosingFunction returns the function definition that encloses the
// given line in a file (the nearest def/defp/defmacro at or before lineNum).
func (s *Store) LookupEnclosingFunction(filePath string, lineNum int) (module, function string, arity int, line int, found bool) {
	err := s.db.QueryRow(
		"SELECT module, function, arity, line FROM definitions WHERE file_id = "+fileIDSubquery+" AND function != '' AND line <= ? ORDER BY line DESC LIMIT 1",
		filePath, lineNum,
	).Scan(&module, &function, &arity, &line)
	if err != nil {
		return "", "", 0, 0, false
	}
	return module, function, arity, line, true
}

// OutgoingRef represents a call made from within a function body.
type OutgoingRef struct {
	Module   string
	Function string
	Line     int
}

// LookupRefsInRange returns all call refs in filePath between startLine and
// endLine (inclusive, 1-based). Used for outgoing call hierarchy.
func (s *Store) LookupRefsInRange(filePath string, startLine, endLine int) ([]OutgoingRef, error) {
	rows, err := s.db.Query(
		"SELECT module, function, line FROM refs WHERE file_id = "+fileIDSubquery+" AND line >= ? AND line <= ? AND kind = 'call' AND function != ''",
		filePath, startLine, endLine,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []OutgoingRef
	for rows.Next() {
		var r OutgoingRef
		if err := rows.Scan(&r.Module, &r.Function, &r.Line); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// NextFunctionLine returns the line number of the next function definition
// after startLine in filePath, or 0 if none exists.
func (s *Store) NextFunctionLine(filePath string, startLine int) int {
	var line int
	err := s.db.QueryRow(
		"SELECT line FROM definitions WHERE file_id = "+fileIDSubquery+" AND function != '' AND line > ? ORDER BY line LIMIT 1",
		filePath, startLine,
	).Scan(&line)
	if err != nil {
		return 0
	}
	return line
}

func (s *Store) LookupFollowDelegate(module, function string) ([]LookupResult, error) {
	return s.lookupFollowDelegate(module, function, 0)
}

func (s *Store) lookupFollowDelegate(module, function string, depth int) ([]LookupResult, error) {
	if depth > 5 {
		return nil, nil
	}

	results, err := s.LookupFunction(module, function)
	if err != nil {
		return nil, err
	}

	// If all results are defdelegates, follow them to the target
	allDelegates := len(results) > 0
	for _, r := range results {
		if r.Kind != "defdelegate" || r.DelegateTo == "" {
			allDelegates = false
			break
		}
	}

	if allDelegates {
		targetModule := results[0].DelegateTo
		targetFunc := function
		if results[0].DelegateAs != "" {
			targetFunc = results[0].DelegateAs
		}
		targetResults, err := s.lookupFollowDelegate(targetModule, targetFunc, depth+1)
		if err != nil {
			return nil, err
		}
		if len(targetResults) > 0 {
			return targetResults, nil
		}
	}

	return results, nil
}
