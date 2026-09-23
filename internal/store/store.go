package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/remoteoss/dexter/internal/beam"
	"github.com/remoteoss/dexter/internal/evidence"
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
				if _, err := conn.Exec(fmt.Sprintf("PRAGMA journal_size_limit = %d", walSizeLimitBytes), nil); err != nil {
					return err
				}
				return conn.RegisterAggregator("dexter_function_fingerprint", func() *fingerprintAggregator {
					return &fingerprintAggregator{}
				}, true)
			},
		})
	})
}

type fingerprintAggregator struct {
	first [sha256.Size]byte
	hash  hash.Hash
	count int
}

func (a *fingerprintAggregator) Step(fingerprint []byte) {
	if len(fingerprint) != sha256.Size {
		return
	}
	a.count++
	if a.count == 1 {
		copy(a.first[:], fingerprint)
		return
	}
	if a.count == 2 {
		a.hash = sha256.New()
		_, _ = a.hash.Write([]byte("dexter:function-clauses:v1\x00"))
		_, _ = a.hash.Write(a.first[:])
	}
	_, _ = a.hash.Write(fingerprint)
}

func (a *fingerprintAggregator) Done() []byte {
	result := make([]byte, sha256.Size)
	if a.hash == nil {
		copy(result, a.first[:])
		return result
	}
	a.hash.Sum(result[:0])
	return result
}

type Store struct {
	db             *sql.DB
	impactSnapshot bool
}

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
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
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

	return openWritableDatabase(DBPath(projectRoot))
}

// OpenTemporary opens a writable index at an explicit path. It is for isolated
// derived indexes and does not create or modify the project's .dexter directory.
func OpenTemporary(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return openWritableDatabase(path)
}

func openWritableDatabase(dbPath string) (*Store, error) {
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

	return &Store{db: db}, nil
}

// OpenImpactSnapshot opens a compact, immutable impact snapshot without running
// normal index migrations or creating workspace files.
func OpenImpactSnapshot(path string) (*Store, error) {
	registerDriver()
	db, err := sql.Open(driverName, path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, impactSnapshot: true}, nil
}

const (
	ImpactSnapshotVersion = 12
	ImpactSourceVersion   = 1
)

const impactCallableKinds = "'def','defp','defmacro','defmacrop','defguard','defguardp','defdelegate','defstruct','defexception','test_root'"

// ExportImpactSnapshot writes the graph and fingerprints needed by impact
// analysis. Navigation references and non-project rows are removed, and file
// paths become project-relative so the artifact is portable across runners.
func (s *Store) ExportImpactSnapshot(path, projectRoot, commit, indexPath string) error {
	return s.exportImpactSnapshot(path, projectRoot, commit, indexPath)
}

// ExportImpactSnapshotWithEvidence includes validated generated provider data.
func (s *Store) ExportImpactSnapshotWithEvidence(path, projectRoot, commit, indexPath string, providers []evidence.LoadedArtifact) error {
	return s.exportImpactSnapshotWithEvidence(context.Background(), path, projectRoot, commit, indexPath, providers, nil)
}

func (s *Store) ExportImpactSnapshotWithEvidenceContext(ctx context.Context, path, projectRoot, commit, indexPath string, providers []evidence.LoadedArtifact) error {
	return s.exportImpactSnapshotWithEvidence(ctx, path, projectRoot, commit, indexPath, providers, nil)
}

// CompiledEvidenceSink receives native compiled evidence while an impact
// snapshot is private and its write transaction is open.
type CompiledEvidenceSink interface {
	AddCompiledEvidence(path string, compiled beam.CompiledEvidence) error
	AddCompiledEdges(edges []parser.CallEdge) error
	AddCompiledOpaque(module, detail string) error
	SetCompiledEvidenceComplete(digest string)
}

// ExportImpactSnapshotWithAugmentContext streams augmentation into the same
// transaction that exports source evidence from the canonical workspace index.
func (s *Store) ExportImpactSnapshotWithAugmentContext(ctx context.Context, path, projectRoot, commit, indexPath string,
	providers []evidence.LoadedArtifact, augment func(CompiledEvidenceSink) error,
) error {
	return s.exportImpactSnapshotWithEvidence(ctx, path, projectRoot, commit, indexPath, providers, augment)
}

// ImpactSnapshotCommit validates the snapshot format and returns its exact
// source revision.
func (s *Store) ImpactSnapshotCommit() (string, error) {
	var versionNumber int
	if err := s.db.QueryRow("SELECT value FROM metadata WHERE key = 'impact_snapshot_version'").Scan(&versionNumber); err != nil {
		return "", err
	}
	if versionNumber != ImpactSnapshotVersion {
		return "", fmt.Errorf("impact snapshot version %d, want %d", versionNumber, ImpactSnapshotVersion)
	}
	var commit string
	if err := s.db.QueryRow("SELECT value FROM metadata WHERE key = 'impact_commit'").Scan(&commit); err != nil {
		return "", err
	}
	return commit, nil
}

// ImpactSnapshotIndexPath returns the repository-relative root indexed by the
// snapshot.
func (s *Store) ImpactSnapshotIndexPath() (string, error) {
	var path string
	if err := s.db.QueryRow("SELECT value FROM metadata WHERE key = 'impact_index_path'").Scan(&path); err != nil {
		return "", err
	}
	return path, nil
}

// MissingImpactEvidenceProviders returns required repository providers that
// were not attached to this snapshot.
func (s *Store) MissingImpactEvidenceProviders() ([]string, error) {
	if !s.impactSnapshot {
		return nil, nil
	}
	rows, err := s.db.Query("SELECT provider FROM impact_evidence_requirements WHERE satisfied = 0 ORDER BY provider")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var providers []string
	for rows.Next() {
		var provider string
		if err := rows.Scan(&provider); err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	return providers, rows.Err()
}

// ImpactEvidenceDigests returns the provider artifacts embedded in a snapshot.
func (s *Store) ImpactEvidenceDigests() (map[string]string, error) {
	if !s.impactSnapshot {
		return nil, nil
	}
	rows, err := s.db.Query("SELECT provider, digest FROM impact_evidence_requirements WHERE satisfied = 1 ORDER BY provider")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	digests := make(map[string]string)
	for rows.Next() {
		var provider, digest string
		if err := rows.Scan(&provider, &digest); err != nil {
			return nil, err
		}
		digests[provider] = digest
	}
	return digests, rows.Err()
}

// ImpactSnapshotStats reports compact artifact row counts.
type ImpactSnapshotStats struct {
	Files      int
	Functions  int
	CallEdges  int
	Unresolved int
}

func (s *Store) GetImpactSnapshotStats() (ImpactSnapshotStats, error) {
	var stats ImpactSnapshotStats
	for _, query := range []struct {
		sql string
		dst *int
	}{
		{"SELECT COUNT(*) FROM files", &stats.Files},
		{"SELECT COUNT(*) FROM impact_functions", &stats.Functions},
		{"SELECT COUNT(*) FROM call_edges", &stats.CallEdges},
		{"SELECT COUNT(*) FROM impact_unresolved", &stats.Unresolved},
	} {
		if err := s.db.QueryRow(query.sql).Scan(query.dst); err != nil {
			return ImpactSnapshotStats{}, err
		}
	}
	return stats, nil
}

func (s *Store) ImpactEdgeKindCounts() (map[string]int, error) {
	rows, err := s.db.Query("SELECT kind, COUNT(*) FROM call_edges GROUP BY kind ORDER BY kind")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[string]int)
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			return nil, err
		}
		counts[kind] = count
	}
	return counts, rows.Err()
}

// ValidateImpactSource verifies that the normal index was built with all source
// evidence required to export an impact snapshot.
func (s *Store) ValidateImpactSource() error {
	var sourceVersion int
	if err := s.db.QueryRow("SELECT value FROM metadata WHERE key = 'impact_source_version'").Scan(&sourceVersion); err != nil {
		return fmt.Errorf("impact source version unavailable: %w", err)
	}
	if sourceVersion != ImpactSourceVersion {
		return fmt.Errorf("impact source version %d, want %d", sourceVersion, ImpactSourceVersion)
	}
	var missing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM definitions
		WHERE function != '' AND kind IN (` + impactCallableKinds + `) AND length(fingerprint) != 32`).Scan(&missing); err != nil {
		return err
	}
	if missing > 0 {
		return fmt.Errorf("%d callable definitions have no impact fingerprint", missing)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM files f
		WHERE substr(f.path, -9) = '_test.exs'
		AND EXISTS (SELECT 1 FROM definitions m WHERE m.file_id = f.id AND m.kind = 'module')
		AND NOT EXISTS (SELECT 1 FROM definitions r WHERE r.file_id = f.id AND r.kind = 'test_root')`).Scan(&missing); err != nil {
		return err
	}
	if missing > 0 {
		return fmt.Errorf("%d test files have no source test root", missing)
	}
	return nil
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
	if !sqlText.Valid || strings.Contains(sqlText.String, "file_id") {
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

		CREATE TABLE IF NOT EXISTS definitions (
			module TEXT NOT NULL,
			function TEXT NOT NULL DEFAULT '',
			arity INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL,
			line INTEGER NOT NULL,
			file_id INTEGER NOT NULL,
			delegate_to TEXT NOT NULL DEFAULT '',
			delegate_as TEXT NOT NULL DEFAULT '',
			params TEXT NOT NULL DEFAULT '',
			fingerprint BLOB NOT NULL DEFAULT X'',
			FOREIGN KEY (file_id) REFERENCES files(id) ON DELETE CASCADE
		);

		CREATE TABLE IF NOT EXISTS refs (
			module TEXT NOT NULL,
			function TEXT NOT NULL DEFAULT '',
			line INTEGER NOT NULL,
			file_id INTEGER NOT NULL,
			kind TEXT NOT NULL DEFAULT 'call'
			-- No FOREIGN KEY here on purpose. With _foreign_keys=ON every insert
			-- costs a parent-key lookup, and refs is the table the cold index
			-- spends most of its writer time on (3.9M rows on a large monorepo).
			-- Every path that removes a file deletes its refs explicitly first,
			-- so the cascade was never the thing keeping them consistent.
		);

		CREATE TABLE IF NOT EXISTS call_symbols (
			id INTEGER PRIMARY KEY,
			module TEXT NOT NULL,
			function TEXT NOT NULL,
			arity INTEGER NOT NULL,
			UNIQUE (module, function, arity)
		);

		CREATE TABLE IF NOT EXISTS call_edges (
			file_id INTEGER NOT NULL,
			caller_id INTEGER NOT NULL,
			callee_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			PRIMARY KEY (file_id, caller_id, callee_id, kind)
		) WITHOUT ROWID;

		CREATE TABLE IF NOT EXISTS metadata (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	if err := ensureDefinitionFingerprintColumn(db); err != nil {
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

func ensureDefinitionFingerprintColumn(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(definitions)")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if name == "fingerprint" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.Exec("ALTER TABLE definitions ADD COLUMN fingerprint BLOB NOT NULL DEFAULT X''")
	return err
}

// fileIDSubquery resolves a path argument to files.id inside a WHERE clause, so
// call sites keep passing paths and only the SQL changes. files.path is UNIQUE,
// so this is one index probe.
const fileIDSubquery = "(SELECT id FROM files WHERE path = ?)"

type dbExecer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func createIndexes(db dbExecer) error {
	_, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_definitions_module_function ON definitions(module, function);
		CREATE INDEX IF NOT EXISTS idx_definitions_file_id_line ON definitions(file_id, line);
		CREATE INDEX IF NOT EXISTS idx_refs_module_function ON refs(module, function, file_id, line, kind);
		CREATE INDEX IF NOT EXISTS idx_refs_file_id ON refs(file_id);
		CREATE INDEX IF NOT EXISTS idx_call_edges_caller ON call_edges(caller_id, callee_id, kind);
		CREATE INDEX IF NOT EXISTS idx_call_edges_callee ON call_edges(callee_id, caller_id, kind);
		CREATE INDEX IF NOT EXISTS idx_definitions_delegate_to ON definitions(delegate_to);
		-- LookupUsingModules runs on the References slow path. Without this it
		-- scans every definition in the index (481k entries on a large monorepo)
		-- to find a few hundred rows, because idx_definitions_module_function
		-- leads with module and cannot be seeked by function alone. The partial
		-- index holds only the __using__ rows, so the scan becomes a small range.
		CREATE INDEX IF NOT EXISTS idx_definitions_using ON definitions(module, file_id) WHERE function = '__using__';
	`)
	return err
}

func (s *Store) DropIndexes() error {
	_, err := s.db.Exec(`
		DROP INDEX IF EXISTS idx_definitions_module;
		DROP INDEX IF EXISTS idx_definitions_module_function;
		DROP INDEX IF EXISTS idx_definitions_file_path;
		DROP INDEX IF EXISTS idx_definitions_file_path_line;
		DROP INDEX IF EXISTS idx_definitions_file_id_line;
		DROP INDEX IF EXISTS idx_refs_module_function;
		DROP INDEX IF EXISTS idx_refs_file_path;
		DROP INDEX IF EXISTS idx_refs_file_id;
		DROP INDEX IF EXISTS idx_call_edges_caller;
		DROP INDEX IF EXISTS idx_call_edges_callee;
		DROP INDEX IF EXISTS idx_refs_function_kind;
		DROP INDEX IF EXISTS idx_definitions_delegate_to;
		DROP INDEX IF EXISTS idx_definitions_using;
	`)
	return err
}

func (s *Store) CreateIndexes() error {
	return createIndexes(s.db)
}

// IndexNames returns the names of dexter's own indexes, as the database
// currently has them. The bulk load drops them before writing and recreates
// them after, so tests use this to check that they came back.
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

// SetImpactSourceVersion records the parser evidence generation in a normal
// index. Snapshot export rejects older or missing generations.
func (s *Store) SetImpactSourceVersion(v int) error {
	_, err := s.db.Exec("INSERT OR REPLACE INTO metadata (key, value) VALUES ('impact_source_version', ?)", strconv.Itoa(v))
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

func (s *Store) IndexFile(path string, defs []parser.Definition) error {
	return s.IndexFileWithRefs(path, defs, nil)
}

func (s *Store) IndexFileWithRefs(path string, defs []parser.Definition, refs []parser.Reference) error {
	return s.IndexFileWithRefsAndCalls(path, defs, refs, nil)
}

func (s *Store) IndexFileWithRefsAndCalls(path string, defs []parser.Definition, refs []parser.Reference, calls []parser.CallEdge) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	fileID, err := upsertFileID(tx, path, info.ModTime().UnixNano())
	if err != nil {
		return err
	}
	staleSymbols, err := callSymbolIDsForFile(tx, fileID)
	if err != nil {
		return err
	}

	if _, err := tx.Exec("DELETE FROM definitions WHERE file_id = ?", fileID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM refs WHERE file_id = ?", fileID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM call_edges WHERE file_id = ?", fileID); err != nil {
		return err
	}

	defStmt, err := tx.Prepare("INSERT INTO definitions (module, function, arity, kind, line, file_id, delegate_to, delegate_as, params, fingerprint) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer func() { _ = defStmt.Close() }()

	for _, d := range defs {
		if _, err := defStmt.Exec(d.Module, d.Function, d.Arity, d.Kind, d.Line, fileID, d.DelegateTo, d.DelegateAs, d.Params, d.Fingerprint[:]); err != nil {
			return err
		}
	}

	if len(refs) > 0 {
		refStmt, err := tx.Prepare("INSERT INTO refs (module, function, line, file_id, kind) VALUES (?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer func() { _ = refStmt.Close() }()

		for _, r := range refs {
			if _, err := refStmt.Exec(r.Module, r.Function, r.Line, fileID, r.Kind); err != nil {
				return err
			}
		}
	}

	if len(calls) > 0 {
		callStmt, err := tx.Prepare("INSERT INTO call_edges (file_id, caller_id, callee_id, kind) VALUES (?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer func() { _ = callStmt.Close() }()

		symbols := make(map[parser.FunctionID]int64)
		for _, call := range calls {
			callerID, err := internCallSymbol(tx, symbols, call.Caller)
			if err != nil {
				return err
			}
			calleeID, err := internCallSymbol(tx, symbols, call.Callee)
			if err != nil {
				return err
			}
			if _, err := callStmt.Exec(fileID, callerID, calleeID, call.Kind); err != nil {
				return err
			}
		}
	}
	if err := cleanupCallSymbols(tx, staleSymbols); err != nil {
		return err
	}

	return tx.Commit()
}

func internCallSymbol(tx *sql.Tx, cache map[parser.FunctionID]int64, fn parser.FunctionID) (int64, error) {
	if id, ok := cache[fn]; ok {
		return id, nil
	}
	var id int64
	err := tx.QueryRow("SELECT id FROM call_symbols WHERE module = ? AND function = ? AND arity = ?", fn.Module, fn.Function, fn.Arity).Scan(&id)
	if err == sql.ErrNoRows {
		result, insertErr := tx.Exec("INSERT INTO call_symbols (module, function, arity) VALUES (?, ?, ?)", fn.Module, fn.Function, fn.Arity)
		if insertErr != nil {
			return 0, insertErr
		}
		id, err = result.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	cache[fn] = id
	return id, nil
}

func callSymbolIDsForFile(tx *sql.Tx, fileID int64) (map[int64]struct{}, error) {
	rows, err := tx.Query("SELECT caller_id FROM call_edges WHERE file_id = ? UNION ALL SELECT callee_id FROM call_edges WHERE file_id = ?", fileID, fileID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

func cleanupCallSymbols(tx *sql.Tx, ids map[int64]struct{}) error {
	for id := range ids {
		if _, err := tx.Exec(`DELETE FROM call_symbols WHERE id = ?
			AND NOT EXISTS (SELECT 1 FROM call_edges WHERE caller_id = ? LIMIT 1)
			AND NOT EXISTS (SELECT 1 FROM call_edges WHERE callee_id = ? LIMIT 1)`, id, id, id); err != nil {
			return err
		}
	}
	return nil
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
	defColumns      = 10
	refColumns      = 5
	symbolColumns   = 4
	callColumns     = 4
	maxBindVars     = 900
	defChunkRows    = maxBindVars / defColumns    // 100
	refChunkRows    = maxBindVars / refColumns    // 180
	symbolChunkRows = maxBindVars / symbolColumns // 225
	callChunkRows   = maxBindVars / callColumns   // 225
)

// Batch wraps multiple IndexFile operations in a single SQLite transaction
// with shared prepared statements.
type Batch struct {
	tx          *sql.Tx
	defStmt     *sql.Stmt
	refStmt     *sql.Stmt
	symbolStmt  *sql.Stmt
	symbolQuery *sql.Stmt
	callStmt    *sql.Stmt
	fileStmt    *sql.Stmt
	delDefStmt  *sql.Stmt // nil in insert-only mode
	delRefStmt  *sql.Stmt // nil in insert-only mode
	delCallStmt *sql.Stmt // nil in insert-only mode
	insertOnly  bool

	// Multi-row INSERT buffers, used in insert-only mode only. A cold index
	// writes ~4.4M rows through one connection, and the writer is the
	// bottleneck of the whole indexing pipeline; batching turns ~4.4M cgo
	// crossings into a few tens of thousands. Incremental reindexing keeps the
	// row-at-a-time path, where a file's DELETE must stay ordered ahead of its
	// INSERTs and the row count is far too small to matter.
	defChunkStmt    *sql.Stmt
	refChunkStmt    *sql.Stmt
	symbolChunkStmt *sql.Stmt
	callChunkStmt   *sql.Stmt
	defArgs         []interface{}
	refArgs         []interface{}
	symbolArgs      []interface{}
	callArgs        []interface{}

	// Bulk-path file id allocation. Insert-only mode assigns ids in Go from a
	// counter and writes files rows with an explicit id, so a cold index never
	// pays a round trip per file to learn what id it just wrote.
	nextFileID     int64
	nextSymbolID   int64
	symbolIDs      map[parser.FunctionID]int64
	staleSymbolIDs map[int64]struct{}
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
	return s.beginBatch(false)
}

// BeginBulkInsert starts a batch optimized for inserting into an empty table.
// It skips DELETE statements before each insert. Callers should drop indexes
// before calling this and recreate them after Commit.
func (s *Store) BeginBulkInsert() (*Batch, error) {
	return s.beginBatch(true)
}

func (s *Store) beginBatch(insertOnly bool) (*Batch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}

	defStmt, err := tx.Prepare("INSERT INTO definitions (module, function, arity, kind, line, file_id, delegate_to, delegate_as, params, fingerprint) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	refStmt, err := tx.Prepare("INSERT INTO refs (module, function, line, file_id, kind) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		_ = defStmt.Close()
		_ = tx.Rollback()
		return nil, err
	}
	symbolStmt, err := tx.Prepare("INSERT INTO call_symbols (id, module, function, arity) VALUES (?, ?, ?, ?)")
	if err != nil {
		_ = defStmt.Close()
		_ = refStmt.Close()
		_ = tx.Rollback()
		return nil, err
	}
	symbolQuery, err := tx.Prepare("SELECT id FROM call_symbols WHERE module = ? AND function = ? AND arity = ?")
	if err != nil {
		_ = defStmt.Close()
		_ = refStmt.Close()
		_ = symbolStmt.Close()
		_ = tx.Rollback()
		return nil, err
	}
	callStmt, err := tx.Prepare("INSERT INTO call_edges (file_id, caller_id, callee_id, kind) VALUES (?, ?, ?, ?)")
	if err != nil {
		_ = defStmt.Close()
		_ = refStmt.Close()
		_ = symbolStmt.Close()
		_ = symbolQuery.Close()
		_ = tx.Rollback()
		return nil, err
	}

	fileSQL := "INSERT INTO files (path, mtime) VALUES (?, ?) ON CONFLICT(path) DO UPDATE SET mtime = excluded.mtime RETURNING id"
	if insertOnly {
		fileSQL = "INSERT INTO files (id, path, mtime) VALUES (?, ?, ?)"
	}
	fileStmt, err := tx.Prepare(fileSQL)
	if err != nil {
		_ = defStmt.Close()
		_ = refStmt.Close()
		_ = callStmt.Close()
		_ = symbolStmt.Close()
		_ = symbolQuery.Close()
		_ = tx.Rollback()
		return nil, err
	}

	b := &Batch{
		tx:             tx,
		defStmt:        defStmt,
		refStmt:        refStmt,
		symbolStmt:     symbolStmt,
		symbolQuery:    symbolQuery,
		callStmt:       callStmt,
		fileStmt:       fileStmt,
		insertOnly:     insertOnly,
		symbolIDs:      make(map[parser.FunctionID]int64),
		staleSymbolIDs: make(map[int64]struct{}),
	}
	if err := tx.QueryRow("SELECT COALESCE(MAX(id), 0) FROM call_symbols").Scan(&b.nextSymbolID); err != nil {
		b.closeStmts()
		_ = tx.Rollback()
		return nil, err
	}

	if !insertOnly {
		b.delDefStmt, err = tx.Prepare("DELETE FROM definitions WHERE file_id = ?")
		if err != nil {
			b.closeStmts()
			_ = tx.Rollback()
			return nil, err
		}
		b.delRefStmt, err = tx.Prepare("DELETE FROM refs WHERE file_id = ?")
		if err != nil {
			b.closeStmts()
			_ = tx.Rollback()
			return nil, err
		}
		b.delCallStmt, err = tx.Prepare("DELETE FROM call_edges WHERE file_id = ?")
		if err != nil {
			b.closeStmts()
			_ = tx.Rollback()
			return nil, err
		}
	} else {
		b.defChunkStmt, err = tx.Prepare(multiRowInsert(
			"definitions",
			"module, function, arity, kind, line, file_id, delegate_to, delegate_as, params, fingerprint",
			defColumns, defChunkRows))
		if err != nil {
			b.closeStmts()
			_ = tx.Rollback()
			return nil, err
		}
		b.refChunkStmt, err = tx.Prepare(multiRowInsert(
			"refs", "module, function, line, file_id, kind", refColumns, refChunkRows))
		if err != nil {
			b.closeStmts()
			_ = tx.Rollback()
			return nil, err
		}
		b.callChunkStmt, err = tx.Prepare(multiRowInsert(
			"call_edges", "file_id, caller_id, callee_id, kind", callColumns, callChunkRows))
		if err != nil {
			b.closeStmts()
			_ = tx.Rollback()
			return nil, err
		}
		b.symbolChunkStmt, err = tx.Prepare(multiRowInsert(
			"call_symbols", "id, module, function, arity", symbolColumns, symbolChunkRows))
		if err != nil {
			b.closeStmts()
			_ = tx.Rollback()
			return nil, err
		}
		b.defArgs = make([]interface{}, 0, defColumns*defChunkRows)
		b.refArgs = make([]interface{}, 0, refColumns*refChunkRows)
		b.symbolArgs = make([]interface{}, 0, symbolColumns*symbolChunkRows)
		b.callArgs = make([]interface{}, 0, callColumns*callChunkRows)

		// Bulk mode is used on a freshly created database, but seed from the
		// table anyway so a non-empty one cannot collide on the primary key.
		if err := tx.QueryRow("SELECT COALESCE(MAX(id), 0) FROM files").Scan(&b.nextFileID); err != nil {
			b.closeStmts()
			_ = tx.Rollback()
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
	return b.indexFile(path, info.ModTime().UnixNano(), defs, nil, nil)
}

func (b *Batch) IndexFileWithMtime(path string, mtimeNano int64, defs []parser.Definition) error {
	return b.indexFile(path, mtimeNano, defs, nil, nil)
}

func (b *Batch) IndexFileWithMtimeAndRefs(path string, mtimeNano int64, defs []parser.Definition, refs []parser.Reference) error {
	return b.indexFile(path, mtimeNano, defs, refs, nil)
}

func (b *Batch) IndexFileWithMtimeRefsAndCalls(path string, mtimeNano int64, defs []parser.Definition, refs []parser.Reference, calls []parser.CallEdge) error {
	return b.indexFile(path, mtimeNano, defs, refs, calls)
}

func (b *Batch) indexFile(path string, mtimeNano int64, defs []parser.Definition, refs []parser.Reference, calls []parser.CallEdge) error {
	fileID, err := b.fileID(path, mtimeNano)
	if err != nil {
		return err
	}

	if !b.insertOnly {
		stale, err := callSymbolIDsForFile(b.tx, fileID)
		if err != nil {
			return err
		}
		for id := range stale {
			b.staleSymbolIDs[id] = struct{}{}
		}
		if _, err := b.delDefStmt.Exec(fileID); err != nil {
			return err
		}
		if _, err := b.delRefStmt.Exec(fileID); err != nil {
			return err
		}
		if _, err := b.delCallStmt.Exec(fileID); err != nil {
			return err
		}
	}

	if b.insertOnly {
		// Reset the buffer before checking the error: the chunk boundary is an
		// exact-multiple test, so a buffer left full would never match again and
		// the rest of the batch would silently fall back to flushPending.
		for i := range defs {
			d := &defs[i]
			b.defArgs = append(b.defArgs, d.Module, d.Function, d.Arity, d.Kind, d.Line, fileID, d.DelegateTo, d.DelegateAs, d.Params, d.Fingerprint[:])
			if len(b.defArgs) == defColumns*defChunkRows {
				_, err := b.defChunkStmt.Exec(b.defArgs...)
				b.defArgs = b.defArgs[:0]
				if err != nil {
					return err
				}
			}
		}
		for _, r := range refs {
			b.refArgs = append(b.refArgs, r.Module, r.Function, r.Line, fileID, r.Kind)
			if len(b.refArgs) == refColumns*refChunkRows {
				_, err := b.refChunkStmt.Exec(b.refArgs...)
				b.refArgs = b.refArgs[:0]
				if err != nil {
					return err
				}
			}
		}
		for _, call := range calls {
			callerID, err := b.callSymbolID(call.Caller)
			if err != nil {
				return err
			}
			calleeID, err := b.callSymbolID(call.Callee)
			if err != nil {
				return err
			}
			b.callArgs = append(b.callArgs, fileID, callerID, calleeID, call.Kind)
			if len(b.callArgs) == callColumns*callChunkRows {
				_, err := b.callChunkStmt.Exec(b.callArgs...)
				b.callArgs = b.callArgs[:0]
				if err != nil {
					return err
				}
			}
		}
		return nil
	}

	for i := range defs {
		d := &defs[i]
		if _, err := b.defStmt.Exec(d.Module, d.Function, d.Arity, d.Kind, d.Line, fileID, d.DelegateTo, d.DelegateAs, d.Params, d.Fingerprint[:]); err != nil {
			return err
		}
	}

	for _, r := range refs {
		if _, err := b.refStmt.Exec(r.Module, r.Function, r.Line, fileID, r.Kind); err != nil {
			return err
		}
	}
	for _, call := range calls {
		callerID, err := b.callSymbolID(call.Caller)
		if err != nil {
			return err
		}
		calleeID, err := b.callSymbolID(call.Callee)
		if err != nil {
			return err
		}
		if _, err := b.callStmt.Exec(fileID, callerID, calleeID, call.Kind); err != nil {
			return err
		}
	}

	return nil
}

func (b *Batch) callSymbolID(fn parser.FunctionID) (int64, error) {
	if id, ok := b.symbolIDs[fn]; ok {
		return id, nil
	}
	if !b.insertOnly {
		var id int64
		err := b.symbolQuery.QueryRow(fn.Module, fn.Function, fn.Arity).Scan(&id)
		if err == nil {
			b.symbolIDs[fn] = id
			return id, nil
		}
		if err != sql.ErrNoRows {
			return 0, err
		}
	}

	b.nextSymbolID++
	id := b.nextSymbolID
	b.symbolIDs[fn] = id
	if b.insertOnly {
		b.symbolArgs = append(b.symbolArgs, id, fn.Module, fn.Function, fn.Arity)
		if len(b.symbolArgs) == symbolColumns*symbolChunkRows {
			_, err := b.symbolChunkStmt.Exec(b.symbolArgs...)
			b.symbolArgs = b.symbolArgs[:0]
			if err != nil {
				return 0, err
			}
		}
		return id, nil
	}
	if _, err := b.symbolStmt.Exec(id, fn.Module, fn.Function, fn.Arity); err != nil {
		return 0, err
	}
	return id, nil
}

// fileID writes the files row for path and returns its id.
//
// The bulk path allocates ids from a counter and inserts them explicitly: a
// cold index writes ~70k files, and asking SQLite to hand back each id would
// add a round trip per file to the one thread that is already the bottleneck.
// The incremental path upserts, so an existing file keeps the id that its
// definitions and refs already point at.
func (b *Batch) fileID(path string, mtimeNano int64) (int64, error) {
	if b.insertOnly {
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
	for i := 0; i < len(b.symbolArgs); i += symbolColumns {
		if _, err := b.symbolStmt.Exec(b.symbolArgs[i : i+symbolColumns]...); err != nil {
			return err
		}
	}
	b.symbolArgs = b.symbolArgs[:0]
	for i := 0; i < len(b.callArgs); i += callColumns {
		if _, err := b.callStmt.Exec(b.callArgs[i : i+callColumns]...); err != nil {
			return err
		}
	}
	b.callArgs = b.callArgs[:0]
	return nil
}

func (b *Batch) Commit() error {
	if err := b.flushPending(); err != nil {
		b.closeStmts()
		_ = b.tx.Rollback()
		return err
	}
	if err := cleanupCallSymbols(b.tx, b.staleSymbolIDs); err != nil {
		b.closeStmts()
		_ = b.tx.Rollback()
		return err
	}
	b.closeStmts()
	return b.tx.Commit()
}

func (b *Batch) Rollback() error {
	b.closeStmts()
	return b.tx.Rollback()
}

func (b *Batch) closeStmts() {
	_ = b.defStmt.Close()
	_ = b.refStmt.Close()
	_ = b.symbolStmt.Close()
	_ = b.symbolQuery.Close()
	_ = b.callStmt.Close()
	_ = b.fileStmt.Close()
	if b.defChunkStmt != nil {
		_ = b.defChunkStmt.Close()
	}
	if b.refChunkStmt != nil {
		_ = b.refChunkStmt.Close()
	}
	if b.symbolChunkStmt != nil {
		_ = b.symbolChunkStmt.Close()
	}
	if b.callChunkStmt != nil {
		_ = b.callChunkStmt.Close()
	}
	if b.delDefStmt != nil {
		_ = b.delDefStmt.Close()
	}
	if b.delRefStmt != nil {
		_ = b.delRefStmt.Close()
	}
	if b.delCallStmt != nil {
		_ = b.delCallStmt.Close()
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

func (s *Store) RemoveFile(path string) error {
	return s.RemoveFiles([]string{path})
}

func (s *Store) RemoveFiles(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	staleSymbols := make(map[int64]struct{})
	for _, path := range paths {
		var fileID int64
		lookupErr := tx.QueryRow("SELECT id FROM files WHERE path = ?", path).Scan(&fileID)
		if lookupErr != nil && lookupErr != sql.ErrNoRows {
			return lookupErr
		}
		if lookupErr == nil {
			ids, err := callSymbolIDsForFile(tx, fileID)
			if err != nil {
				return err
			}
			for id := range ids {
				staleSymbols[id] = struct{}{}
			}
		}
		if _, err = tx.Exec("DELETE FROM definitions WHERE file_id = "+fileIDSubquery, path); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM refs WHERE file_id = "+fileIDSubquery, path); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM call_edges WHERE file_id = "+fileIDSubquery, path); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM files WHERE path = ?", path); err != nil {
			return err
		}
	}
	if err := cleanupCallSymbols(tx, staleSymbols); err != nil {
		return err
	}
	// A workspace can become empty after its last file is removed. Clear the
	// file-independent symbol pool too, so a later insert-only cold build still
	// starts from a genuinely empty derived index.
	if _, err = tx.Exec("DELETE FROM call_symbols WHERE NOT EXISTS (SELECT 1 FROM files LIMIT 1)"); err != nil {
		return err
	}
	return tx.Commit()
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
		query += " AND kind IN ('def', 'defmacro', 'defguard', 'defdelegate', 'type', 'opaque')"
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

// IndexStats summarizes the size of the index.
type IndexStats struct {
	Files       int
	Definitions int
	References  int
	CallEdges   int
}

// FunctionRecord is one source definition row used by offline index comparison.
// Repeated clauses remain separate and are returned in deterministic source order.
type FunctionRecord struct {
	Function    parser.FunctionID
	FilePath    string
	Kind        string
	Line        int
	Fingerprint [32]byte
}

// FunctionFingerprint is one aggregate fingerprint per callable identity.
type FunctionFingerprint struct {
	Function    parser.FunctionID
	Fingerprint [32]byte
}

// ListFunctionFingerprints aggregates repeated clauses in deterministic source
// order inside SQLite, so impact analysis does not retain every definition row.
func (s *Store) ListFunctionFingerprints() ([]FunctionFingerprint, error) {
	query := `SELECT d.module, d.function, d.arity,
		dexter_function_fingerprint(d.fingerprint ORDER BY f.path, d.line)
		FROM definitions d JOIN files f ON f.id = d.file_id
		WHERE d.function != '' AND length(d.fingerprint) = 32 AND d.kind IN (` + impactCallableKinds + `)
		GROUP BY d.module, d.function, d.arity
		ORDER BY d.module, d.function, d.arity`
	if s.impactSnapshot {
		query = `SELECT module, function, arity, fingerprint FROM impact_functions
			ORDER BY module, function, arity`
	}
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []FunctionFingerprint
	for rows.Next() {
		var entry FunctionFingerprint
		var fingerprint []byte
		if err := rows.Scan(&entry.Function.Module, &entry.Function.Function, &entry.Function.Arity, &fingerprint); err != nil {
			return nil, err
		}
		copy(entry.Fingerprint[:], fingerprint)
		result = append(result, entry)
	}
	return result, rows.Err()
}

// ListTestFunctionRecords returns callable ownership rows only for source tests.
func (s *Store) ListTestFunctionRecords() ([]FunctionRecord, error) {
	if s.impactSnapshot {
		return s.listImpactFunctionRecords(true)
	}
	return s.listFunctionRecords(` AND substr(f.path, -9) = '_test.exs' AND d.function = '__dexter_test_root__'`, nil)
}

// ListFunctionsInFiles returns callable identities owned by the supplied paths.
func (s *Store) ListFunctionsInFiles(paths []string) ([]parser.FunctionID, error) {
	seen := make(map[parser.FunctionID]struct{})
	var result []parser.FunctionID
	for start := 0; start < len(paths); start += maxBindVars {
		end := min(start+maxBindVars, len(paths))
		args := make([]interface{}, end-start)
		for i, path := range paths[start:end] {
			args[i] = path
		}
		query := `SELECT DISTINCT d.module, d.function, d.arity
			FROM definitions d JOIN files f ON f.id = d.file_id
			WHERE d.function != '' AND d.kind IN (` + impactCallableKinds + `) AND f.path IN (?` + strings.Repeat(",?", end-start-1) + `)`
		if s.impactSnapshot {
			query = `SELECT DISTINCT d.module, d.function, d.arity
				FROM impact_function_files d JOIN files f ON f.id = d.file_id
				WHERE f.path IN (?` + strings.Repeat(",?", end-start-1) + `)`
		}
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var function parser.FunctionID
			if err := rows.Scan(&function.Module, &function.Function, &function.Arity); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if _, ok := seen[function]; !ok {
				seen[function] = struct{}{}
				result = append(result, function)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	sort.Slice(result, func(i, j int) bool { return functionIDLess(result[i], result[j]) })
	return result, nil
}

// ListCallableFiles returns supplied paths that own at least one callable.
func (s *Store) ListCallableFiles(paths []string) ([]string, error) {
	seen := make(map[string]struct{})
	var result []string
	for start := 0; start < len(paths); start += maxBindVars {
		end := min(start+maxBindVars, len(paths))
		args := make([]interface{}, end-start)
		for i, path := range paths[start:end] {
			args[i] = path
		}
		query := `SELECT DISTINCT f.path FROM definitions d JOIN files f ON f.id = d.file_id
			WHERE d.function != '' AND d.kind IN (` + impactCallableKinds + `) AND f.path IN (?` + strings.Repeat(",?", end-start-1) + `)`
		if s.impactSnapshot {
			query = `SELECT DISTINCT f.path FROM impact_function_files d JOIN files f ON f.id = d.file_id
				WHERE f.path IN (?` + strings.Repeat(",?", end-start-1) + `)`
		}
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if _, ok := seen[path]; !ok {
				seen[path] = struct{}{}
				result = append(result, path)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	sort.Strings(result)
	return result, nil
}

// ListFunctionRecords returns all fingerprinted callable source definitions.
func (s *Store) ListFunctionRecords() ([]FunctionRecord, error) {
	if s.impactSnapshot {
		return s.listImpactFunctionRecords(false)
	}
	return s.listFunctionRecords("", nil)
}

func (s *Store) listImpactFunctionRecords(testsOnly bool) ([]FunctionRecord, error) {
	query := `SELECT d.module, d.function, d.arity, f.path, p.fingerprint
		FROM impact_function_files d
		JOIN files f ON f.id = d.file_id
		JOIN impact_functions p ON p.module = d.module AND p.function = d.function AND p.arity = d.arity`
	if testsOnly {
		query += ` WHERE substr(f.path, -9) = '_test.exs' AND d.function = '__dexter_test_root__'`
	}
	query += ` ORDER BY f.path, d.module, d.function, d.arity`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []FunctionRecord
	for rows.Next() {
		var record FunctionRecord
		var fingerprint []byte
		if err := rows.Scan(&record.Function.Module, &record.Function.Function, &record.Function.Arity, &record.FilePath, &fingerprint); err != nil {
			return nil, err
		}
		copy(record.Fingerprint[:], fingerprint)
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) listFunctionRecords(extraWhere string, args []interface{}) ([]FunctionRecord, error) {
	rows, err := s.db.Query(`SELECT d.module, d.function, d.arity, f.path, d.kind, d.line, d.fingerprint
		FROM definitions d JOIN files f ON f.id = d.file_id
		WHERE d.function != '' AND length(d.fingerprint) = 32 AND d.kind IN (`+impactCallableKinds+`)`+extraWhere+`
		ORDER BY f.path, d.line, d.module, d.function, d.arity`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var records []FunctionRecord
	for rows.Next() {
		var record FunctionRecord
		var fingerprint []byte
		if err := rows.Scan(&record.Function.Module, &record.Function.Function, &record.Function.Arity, &record.FilePath, &record.Kind, &record.Line, &fingerprint); err != nil {
			return nil, err
		}
		copy(record.Fingerprint[:], fingerprint)
		records = append(records, record)
	}
	return records, rows.Err()
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
		{"SELECT COUNT(*) FROM call_edges", &st.CallEdges},
	} {
		if err := s.db.QueryRow(q.query).Scan(q.dst); err != nil {
			return IndexStats{}, err
		}
	}
	return st, nil
}

// CallResult is one adjacent function and the relationship that connects it.
type CallResult struct {
	Function parser.FunctionID
	Kind     string
}

// CallSymbol is one interned function identity in the call graph.
type CallSymbol struct {
	ID       int64
	Function parser.FunctionID
}

// CallRelation connects one requested frontier symbol to an adjacent symbol.
type CallRelation struct {
	FrontierID int64
	Adjacent   CallSymbol
	Kind       string
}

// ImpactUnresolved is one provider call site whose target was not proven.
type ImpactUnresolved struct {
	Provider string
	Caller   parser.FunctionID
	Kind     string
	Detail   string
}

// ListImpactUnresolved returns generated evidence that must remain uncertain.
func (s *Store) ListImpactUnresolved() ([]ImpactUnresolved, error) {
	if !s.impactSnapshot {
		return nil, nil
	}
	rows, err := s.db.Query(`SELECT provider, caller_module, caller_function, caller_arity, kind, detail
		FROM impact_unresolved ORDER BY provider, caller_module, caller_function, caller_arity, kind, detail`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []ImpactUnresolved
	for rows.Next() {
		var unresolved ImpactUnresolved
		if err := rows.Scan(&unresolved.Provider, &unresolved.Caller.Module, &unresolved.Caller.Function,
			&unresolved.Caller.Arity, &unresolved.Kind, &unresolved.Detail); err != nil {
			return nil, err
		}
		result = append(result, unresolved)
	}
	return result, rows.Err()
}

const callSymbolLookupChunk = maxBindVars / 3

// ResolveCallSymbols resolves function identities in bounded batches. Missing
// identities are omitted from the result.
func (s *Store) ResolveCallSymbols(functions []parser.FunctionID) ([]CallSymbol, error) {
	seen := make(map[int64]struct{}, len(functions))
	result := make([]CallSymbol, 0, len(functions))
	for start := 0; start < len(functions); start += callSymbolLookupChunk {
		end := min(start+callSymbolLookupChunk, len(functions))
		query, args := resolveCallSymbolsQuery(functions[start:end])
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var symbol CallSymbol
			if err := rows.Scan(&symbol.ID, &symbol.Function.Module, &symbol.Function.Function, &symbol.Function.Arity); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if _, ok := seen[symbol.ID]; !ok {
				seen[symbol.ID] = struct{}{}
				result = append(result, symbol)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	sort.Slice(result, func(i, j int) bool {
		return functionIDLess(result[i].Function, result[j].Function)
	})
	return result, nil
}

func resolveCallSymbolsQuery(functions []parser.FunctionID) (string, []interface{}) {
	values := make([]string, len(functions))
	args := make([]interface{}, 0, len(functions)*3)
	for i, fn := range functions {
		values[i] = "(?,?,?)"
		args = append(args, fn.Module, fn.Function, fn.Arity)
	}
	return `WITH requested(module, function, arity) AS (VALUES ` + strings.Join(values, ",") + `)
		SELECT s.id, s.module, s.function, s.arity
		FROM requested r
		JOIN call_symbols s ON s.module = r.module AND s.function = r.function AND s.arity = r.arity`, args
}

// LookupCalleeFrontier returns all callees adjacent to the supplied caller IDs.
func (s *Store) LookupCalleeFrontier(ids []int64) ([]CallRelation, error) {
	return s.lookupCallFrontier(ids, true)
}

// LookupCallerFrontier returns all callers adjacent to the supplied callee IDs.
func (s *Store) LookupCallerFrontier(ids []int64) ([]CallRelation, error) {
	return s.lookupCallFrontier(ids, false)
}

func (s *Store) lookupCallFrontier(ids []int64, forward bool) ([]CallRelation, error) {
	var result []CallRelation
	for start := 0; start < len(ids); start += maxBindVars {
		end := min(start+maxBindVars, len(ids))
		query := callFrontierQuery(end-start, forward)
		args := make([]interface{}, end-start)
		for i, id := range ids[start:end] {
			args[i] = id
		}
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var relation CallRelation
			if err := rows.Scan(&relation.FrontierID, &relation.Adjacent.ID, &relation.Adjacent.Function.Module, &relation.Adjacent.Function.Function, &relation.Adjacent.Function.Arity, &relation.Kind); err != nil {
				_ = rows.Close()
				return nil, err
			}
			result = append(result, relation)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}
	return result, nil
}

func callFrontierQuery(count int, forward bool) string {
	frontierColumn, adjacentColumn, index := "caller_id", "callee_id", "idx_call_edges_caller"
	if !forward {
		frontierColumn, adjacentColumn, index = "callee_id", "caller_id", "idx_call_edges_callee"
	}
	return "SELECT e." + frontierColumn + ", s.id, s.module, s.function, s.arity, e.kind " +
		"FROM call_edges e INDEXED BY " + index + " JOIN call_symbols s ON s.id = e." + adjacentColumn +
		" WHERE e." + frontierColumn + " IN (?" + strings.Repeat(",?", count-1) + ")"
}

func functionIDLess(a, b parser.FunctionID) bool {
	if a.Module != b.Module {
		return a.Module < b.Module
	}
	if a.Function != b.Function {
		return a.Function < b.Function
	}
	return a.Arity < b.Arity
}

// LookupCallees returns the functions called by caller.
func (s *Store) LookupCallees(caller parser.FunctionID) ([]CallResult, error) {
	id, ok, err := s.lookupCallSymbolID(caller)
	if err != nil || !ok {
		return nil, err
	}
	return s.lookupAdjacentCalls(
		"SELECT s.module, s.function, s.arity, e.kind FROM call_edges e JOIN call_symbols s ON s.id = e.callee_id WHERE e.caller_id = ?",
		id,
	)
}

// LookupCallers returns the functions that call callee.
func (s *Store) LookupCallers(callee parser.FunctionID) ([]CallResult, error) {
	id, ok, err := s.lookupCallSymbolID(callee)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return s.lookupAdjacentCalls(
		"SELECT s.module, s.function, s.arity, e.kind FROM call_edges e JOIN call_symbols s ON s.id = e.caller_id WHERE e.callee_id = ?",
		id,
	)
}

func (s *Store) lookupCallSymbolID(fn parser.FunctionID) (int64, bool, error) {
	var id int64
	err := s.db.QueryRow("SELECT id FROM call_symbols WHERE module = ? AND function = ? AND arity = ?", fn.Module, fn.Function, fn.Arity).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return id, err == nil, err
}

func (s *Store) lookupAdjacentCalls(query string, ids ...int64) ([]CallResult, error) {
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	seen := make(map[CallResult]struct{})
	var results []CallResult
	for rows.Next() {
		var result CallResult
		if err := rows.Scan(&result.Function.Module, &result.Function.Function, &result.Function.Arity, &result.Kind); err != nil {
			return nil, err
		}
		if _, ok := seen[result]; ok {
			continue
		}
		seen[result] = struct{}{}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.Function.Module != b.Function.Module {
			return a.Function.Module < b.Function.Module
		}
		if a.Function.Function != b.Function.Function {
			return a.Function.Function < b.Function.Function
		}
		if a.Function.Arity != b.Function.Arity {
			return a.Function.Arity < b.Function.Arity
		}
		return a.Kind < b.Kind
	})
	return results, nil
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
		"SELECT DISTINCT function, arity FROM definitions WHERE module = ? AND function != '' AND kind IN ('def', 'defmacro', 'defguard', 'defdelegate', 'type', 'opaque')",
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
		"SELECT f.path, d.line, d.kind, d.arity, d.delegate_to, d.delegate_as FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module = ? AND d.function = ? AND d.kind IN ('def', 'defmacro', 'defguard', 'defdelegate', 'type', 'opaque') ORDER BY CASE WHEN d.kind IN ('type', 'opaque') THEN 1 ELSE 0 END, d.line",
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
}

// LookupDelegatesTo returns all defdelegate definitions that forward the given
// function to the target module. Matches both the no-as: case (function name
// equals target) and the as: case (delegate_as equals target function name).
func (s *Store) LookupDelegatesTo(targetModule, targetFunction string) ([]DelegateEntry, error) {
	rows, err := s.db.Query(
		`SELECT d.module, d.function, d.delegate_as, f.path, d.line FROM definitions d JOIN files f ON f.id = d.file_id
		 WHERE d.kind = 'defdelegate' AND d.delegate_to = ? AND d.delegate_as = '' AND d.function = ?
		 UNION ALL
		 SELECT d.module, d.function, d.delegate_as, f.path, d.line FROM definitions d JOIN files f ON f.id = d.file_id
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
		if err := rows.Scan(&r.Module, &r.Function, &r.DelegateAs, &r.FilePath, &r.Line); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

func (s *Store) SearchSymbols(query string, excludePathPrefix ...string) ([]CompletionResult, error) {
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
		args = append(args, query, query)
		rows, err = s.db.Query(
			"SELECT d.module, d.function, d.arity, d.kind, f.path, d.line FROM definitions d JOIN files f ON f.id = d.file_id WHERE d.module LIKE ? AND (d.function LIKE ? OR d.module LIKE ?)"+pathFilter+
				" ORDER BY CASE WHEN d.module = ? THEN 0 WHEN INSTR(d.module || '.' || d.function, ?) > 0 THEN 1 ELSE 2 END, d.module, d.function LIMIT 50",
			args...,
		)
	} else {
		pattern := "%" + query + "%"
		args := append([]interface{}{pattern, pattern}, extraArgs...)
		args = append(args, query, query, query)
		rows, err = s.db.Query(
			"SELECT d.module, d.function, d.arity, d.kind, f.path, d.line FROM definitions d JOIN files f ON f.id = d.file_id WHERE (d.module LIKE ? OR d.function LIKE ?)"+pathFilter+
				" ORDER BY CASE WHEN d.module = ? THEN 0 WHEN INSTR(d.module, ?) > 0 OR INSTR(d.function, ?) > 0 THEN 1 ELSE 2 END, d.module, d.function LIMIT 50",
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

// ListSubmodules returns all modules whose name starts with prefix + ".".
// Used for cascading module renames (e.g. renaming MyApp.Accounts also catches MyApp.Accounts.User).
func (s *Store) ListSubmodules(prefix string) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT DISTINCT module FROM definitions WHERE module LIKE ? AND function = '' ORDER BY module",
		prefix+".%",
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
