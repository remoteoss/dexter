package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"gitlab.com/remote-com/employ-starbase/dexter/internal/parser"
)

type Store struct {
	db *sql.DB
}

func Open(projectRoot string) (*Store, error) {
	dbPath := filepath.Join(projectRoot, ".dexter.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_foreign_keys=ON")
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

func (s *Store) Close() error {
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS files (
			path TEXT PRIMARY KEY,
			mtime INTEGER NOT NULL
		);

		CREATE TABLE IF NOT EXISTS definitions (
			module TEXT NOT NULL,
			function TEXT NOT NULL DEFAULT '',
			arity INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL,
			line INTEGER NOT NULL,
			file_path TEXT NOT NULL,
			delegate_to TEXT NOT NULL DEFAULT '',
			delegate_as TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (file_path) REFERENCES files(path) ON DELETE CASCADE
		);

		CREATE TABLE IF NOT EXISTS metadata (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	return createDefinitionIndexes(db)
}

type dbExecer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func createDefinitionIndexes(db dbExecer) error {
	_, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_definitions_module ON definitions(module);
		CREATE INDEX IF NOT EXISTS idx_definitions_module_function ON definitions(module, function);
		CREATE INDEX IF NOT EXISTS idx_definitions_file_path ON definitions(file_path);
	`)
	return err
}

func (s *Store) DropDefinitionIndexes() error {
	_, err := s.db.Exec(`
		DROP INDEX IF EXISTS idx_definitions_module;
		DROP INDEX IF EXISTS idx_definitions_module_function;
		DROP INDEX IF EXISTS idx_definitions_file_path;
	`)
	return err
}

func (s *Store) CreateDefinitionIndexes() error {
	return createDefinitionIndexes(s.db)
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

func (s *Store) IsEmpty() bool {
	var count int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM files").Scan(&count)
	return count == 0
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
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("DELETE FROM definitions WHERE file_path = ?", path); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO files (path, mtime) VALUES (?, ?)", path, info.ModTime().UnixNano()); err != nil {
		return err
	}

	stmt, err := tx.Prepare("INSERT INTO definitions (module, function, arity, kind, line, file_path, delegate_to, delegate_as) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()

	for _, d := range defs {
		if _, err := stmt.Exec(d.Module, d.Function, d.Arity, d.Kind, d.Line, d.FilePath, d.DelegateTo, d.DelegateAs); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// Batch wraps multiple IndexFile operations in a single SQLite transaction
// with shared prepared statements.
type Batch struct {
	tx         *sql.Tx
	defStmt    *sql.Stmt
	fileStmt   *sql.Stmt
	delStmt    *sql.Stmt // nil in insert-only mode
	insertOnly bool
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

	defStmt, err := tx.Prepare("INSERT INTO definitions (module, function, arity, kind, line, file_path, delegate_to, delegate_as) VALUES (?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	fileStmt, err := tx.Prepare("INSERT OR REPLACE INTO files (path, mtime) VALUES (?, ?)")
	if err != nil {
		_ = defStmt.Close()
		_ = tx.Rollback()
		return nil, err
	}

	b := &Batch{
		tx:         tx,
		defStmt:    defStmt,
		fileStmt:   fileStmt,
		insertOnly: insertOnly,
	}

	if !insertOnly {
		b.delStmt, err = tx.Prepare("DELETE FROM definitions WHERE file_path = ?")
		if err != nil {
			_ = defStmt.Close()
			_ = fileStmt.Close()
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
	return b.indexFile(path, info.ModTime().UnixNano(), defs)
}

func (b *Batch) IndexFileWithMtime(path string, mtimeNano int64, defs []parser.Definition) error {
	return b.indexFile(path, mtimeNano, defs)
}

func (b *Batch) indexFile(path string, mtimeNano int64, defs []parser.Definition) error {
	if !b.insertOnly {
		if _, err := b.delStmt.Exec(path); err != nil {
			return err
		}
	}

	if _, err := b.fileStmt.Exec(path, mtimeNano); err != nil {
		return err
	}

	for _, d := range defs {
		if _, err := b.defStmt.Exec(d.Module, d.Function, d.Arity, d.Kind, d.Line, d.FilePath, d.DelegateTo, d.DelegateAs); err != nil {
			return err
		}
	}

	return nil
}

func (b *Batch) Commit() error {
	b.closeStmts()
	return b.tx.Commit()
}

func (b *Batch) Rollback() error {
	b.closeStmts()
	return b.tx.Rollback()
}

func (b *Batch) closeStmts() {
	_ = b.defStmt.Close()
	_ = b.fileStmt.Close()
	if b.delStmt != nil {
		_ = b.delStmt.Close()
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
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec("DELETE FROM definitions WHERE file_path = ?", path)
	if err != nil {
		return err
	}
	_, err = tx.Exec("DELETE FROM files WHERE path = ?", path)
	if err != nil {
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
	query := "SELECT module, function, arity, kind, file_path, line FROM definitions WHERE module = ? AND function != ''"
	if publicOnly {
		query += " AND kind IN ('def', 'defmacro', 'defguard', 'defdelegate')"
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
		if err := rows.Scan(&r.Module, &r.Function, &r.Arity, &r.Kind, &r.FilePath, &r.Line); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

type LookupResult struct {
	FilePath   string
	Line       int
	Kind       string
	DelegateTo string
	DelegateAs string
}

func (s *Store) LookupModule(module string) ([]LookupResult, error) {
	return s.queryLookup(
		"SELECT file_path, line, kind, delegate_to, delegate_as FROM definitions WHERE module = ? AND function = '' AND kind IN ('module', 'defprotocol', 'defimpl')",
		module,
	)
}

func (s *Store) LookupFunction(module, function string) ([]LookupResult, error) {
	return s.queryLookup(
		"SELECT file_path, line, kind, delegate_to, delegate_as FROM definitions WHERE module = ? AND function = ? AND kind NOT IN ('module', 'defprotocol', 'defimpl') ORDER BY CASE WHEN kind IN ('type', 'typep', 'opaque') THEN 1 ELSE 0 END, line",
		module, function,
	)
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
		if err := rows.Scan(&r.FilePath, &r.Line, &r.Kind, &r.DelegateTo, &r.DelegateAs); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

func (s *Store) LookupFollowDelegate(module, function string) ([]LookupResult, error) {
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
		targetResults, err := s.LookupFunction(targetModule, targetFunc)
		if err != nil {
			return nil, err
		}
		if len(targetResults) > 0 {
			return targetResults, nil
		}
	}

	return results, nil
}
