package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"github.com/remoteoss/dexter/internal/parser"
)

type Store struct {
	db *sql.DB
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

	dbPath := DBPath(projectRoot)
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

// SetBulkPragmas configures SQLite for maximum write throughput. Safe only
// when the database is fresh and throwaway-on-crash (e.g. a forced full
// rebuild): synchronous=OFF skips all fsyncs, journal_mode=OFF eliminates
// WAL writes entirely, a large cache keeps pages in RAM during index
// creation, and temp_store=MEMORY keeps sort temporaries off disk.
func (s *Store) SetBulkPragmas() error {
	for _, pragma := range []string{
		"PRAGMA synchronous = OFF",
		"PRAGMA journal_mode = MEMORY",
		"PRAGMA cache_size = -2000000",
		"PRAGMA temp_store = MEMORY",
	} {
		if _, err := s.db.Exec(pragma); err != nil {
			return fmt.Errorf("%s: %w", pragma, err)
		}
	}
	return nil
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
			params TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (file_path) REFERENCES files(path) ON DELETE CASCADE
		);

		CREATE TABLE IF NOT EXISTS refs (
			module TEXT NOT NULL,
			function TEXT NOT NULL DEFAULT '',
			line INTEGER NOT NULL,
			file_path TEXT NOT NULL,
			kind TEXT NOT NULL DEFAULT 'call',
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
	return createIndexes(db)
}

type dbExecer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func createIndexes(db dbExecer) error {
	_, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_definitions_module_function ON definitions(module, function);
		CREATE INDEX IF NOT EXISTS idx_definitions_file_path_line ON definitions(file_path, line);
		CREATE INDEX IF NOT EXISTS idx_refs_module_function ON refs(module, function);
		CREATE INDEX IF NOT EXISTS idx_refs_file_path ON refs(file_path);
		CREATE INDEX IF NOT EXISTS idx_refs_function_kind ON refs(function, kind);
		CREATE INDEX IF NOT EXISTS idx_definitions_delegate_to ON definitions(delegate_to);
	`)
	return err
}

func (s *Store) DropIndexes() error {
	_, err := s.db.Exec(`
		DROP INDEX IF EXISTS idx_definitions_module;
		DROP INDEX IF EXISTS idx_definitions_module_function;
		DROP INDEX IF EXISTS idx_definitions_file_path;
		DROP INDEX IF EXISTS idx_definitions_file_path_line;
		DROP INDEX IF EXISTS idx_refs_module_function;
		DROP INDEX IF EXISTS idx_refs_file_path;
		DROP INDEX IF EXISTS idx_refs_function_kind;
		DROP INDEX IF EXISTS idx_definitions_delegate_to;
	`)
	return err
}

func (s *Store) CreateIndexes() error {
	return createIndexes(s.db)
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
	return s.IndexFileWithRefs(path, defs, nil)
}

func (s *Store) IndexFileWithRefs(path string, defs []parser.Definition, refs []parser.Reference) error {
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
	if _, err := tx.Exec("DELETE FROM refs WHERE file_path = ?", path); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO files (path, mtime) VALUES (?, ?)", path, info.ModTime().UnixNano()); err != nil {
		return err
	}

	defStmt, err := tx.Prepare("INSERT INTO definitions (module, function, arity, kind, line, file_path, delegate_to, delegate_as, params) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer func() { _ = defStmt.Close() }()

	for _, d := range defs {
		if _, err := defStmt.Exec(d.Module, d.Function, d.Arity, d.Kind, d.Line, d.FilePath, d.DelegateTo, d.DelegateAs, d.Params); err != nil {
			return err
		}
	}

	if len(refs) > 0 {
		refStmt, err := tx.Prepare("INSERT INTO refs (module, function, line, file_path, kind) VALUES (?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer func() { _ = refStmt.Close() }()

		for _, r := range refs {
			if _, err := refStmt.Exec(r.Module, r.Function, r.Line, r.FilePath, r.Kind); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// Batch wraps multiple IndexFile operations in a single SQLite transaction
// with shared prepared statements.
type Batch struct {
	tx         *sql.Tx
	defStmt    *sql.Stmt
	refStmt    *sql.Stmt
	fileStmt   *sql.Stmt
	delDefStmt *sql.Stmt // nil in insert-only mode
	delRefStmt *sql.Stmt // nil in insert-only mode
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

	defStmt, err := tx.Prepare("INSERT INTO definitions (module, function, arity, kind, line, file_path, delegate_to, delegate_as, params) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}

	refStmt, err := tx.Prepare("INSERT INTO refs (module, function, line, file_path, kind) VALUES (?, ?, ?, ?, ?)")
	if err != nil {
		_ = defStmt.Close()
		_ = tx.Rollback()
		return nil, err
	}

	fileStmt, err := tx.Prepare("INSERT OR REPLACE INTO files (path, mtime) VALUES (?, ?)")
	if err != nil {
		_ = defStmt.Close()
		_ = refStmt.Close()
		_ = tx.Rollback()
		return nil, err
	}

	b := &Batch{
		tx:         tx,
		defStmt:    defStmt,
		refStmt:    refStmt,
		fileStmt:   fileStmt,
		insertOnly: insertOnly,
	}

	if !insertOnly {
		b.delDefStmt, err = tx.Prepare("DELETE FROM definitions WHERE file_path = ?")
		if err != nil {
			b.closeStmts()
			_ = tx.Rollback()
			return nil, err
		}
		b.delRefStmt, err = tx.Prepare("DELETE FROM refs WHERE file_path = ?")
		if err != nil {
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
	return b.indexFile(path, info.ModTime().UnixNano(), defs, nil)
}

func (b *Batch) IndexFileWithMtime(path string, mtimeNano int64, defs []parser.Definition) error {
	return b.indexFile(path, mtimeNano, defs, nil)
}

func (b *Batch) IndexFileWithMtimeAndRefs(path string, mtimeNano int64, defs []parser.Definition, refs []parser.Reference) error {
	return b.indexFile(path, mtimeNano, defs, refs)
}

func (b *Batch) indexFile(path string, mtimeNano int64, defs []parser.Definition, refs []parser.Reference) error {
	if !b.insertOnly {
		if _, err := b.delDefStmt.Exec(path); err != nil {
			return err
		}
		if _, err := b.delRefStmt.Exec(path); err != nil {
			return err
		}
	}

	if _, err := b.fileStmt.Exec(path, mtimeNano); err != nil {
		return err
	}

	for _, d := range defs {
		if _, err := b.defStmt.Exec(d.Module, d.Function, d.Arity, d.Kind, d.Line, d.FilePath, d.DelegateTo, d.DelegateAs, d.Params); err != nil {
			return err
		}
	}

	for _, r := range refs {
		if _, err := b.refStmt.Exec(r.Module, r.Function, r.Line, r.FilePath, r.Kind); err != nil {
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
	_ = b.refStmt.Close()
	_ = b.fileStmt.Close()
	if b.delDefStmt != nil {
		_ = b.delDefStmt.Close()
	}
	if b.delRefStmt != nil {
		_ = b.delRefStmt.Close()
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

	for _, path := range paths {
		if _, err = tx.Exec("DELETE FROM definitions WHERE file_path = ?", path); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM refs WHERE file_path = ?", path); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM files WHERE path = ?", path); err != nil {
			return err
		}
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
	query := "SELECT module, function, arity, kind, file_path, line, params FROM definitions WHERE module = ? AND function != '' AND kind NOT IN ('callback', 'macrocallback')"
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
		"SELECT file_path, line, kind, arity, delegate_to, delegate_as FROM definitions WHERE module = ? AND function = '' AND kind IN ('module', 'defprotocol', 'defimpl')",
		module,
	)
}

// LookupModulesInFile returns all module names defined in the given file, in line order.
func (s *Store) LookupModulesInFile(filePath string) ([]string, error) {
	rows, err := s.db.Query(
		"SELECT module FROM definitions WHERE file_path = ? AND function = '' AND kind IN ('module', 'defprotocol') ORDER BY line",
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
			"WHERE d.file_path = ? AND d.function = ? AND d.kind NOT IN ('module', 'defprotocol', 'defimpl', 'callback', 'macrocallback') "+
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
		"SELECT file_path, line, kind, arity, delegate_to, delegate_as FROM definitions WHERE module = ? AND function = ? AND kind NOT IN ('module', 'defprotocol', 'defimpl', 'callback', 'macrocallback') ORDER BY CASE WHEN kind IN ('type', 'opaque') THEN 1 ELSE 0 END, line",
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
		"SELECT file_path, line, kind, arity FROM definitions WHERE module = ? AND function = ? AND kind IN ('callback', 'macrocallback') ORDER BY line",
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
			"SELECT file_path, line, kind, arity FROM definitions WHERE function = ? AND arity = ? AND kind IN ('callback', 'macrocallback') ORDER BY module, line",
			function, arity,
		)
	} else {
		rows, err = s.db.Query(
			"SELECT file_path, line, kind, arity FROM definitions WHERE function = ? AND kind IN ('callback', 'macrocallback') ORDER BY module, line",
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
		SELECT module FROM refs WHERE file_path = ? AND kind = 'behaviour'
		UNION
		SELECT r.module FROM refs r
		WHERE r.file_path = ? AND r.kind = 'use'
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
		SELECT DISTINCT d.module, r.file_path
		FROM refs r
		JOIN definitions d ON d.file_path = r.file_path AND d.function = '' AND d.kind IN ('module', 'defprotocol')
			AND d.line = (
				SELECT MAX(d2.line) FROM definitions d2
				WHERE d2.file_path = r.file_path AND d2.function = '' AND d2.kind IN ('module', 'defprotocol') AND d2.line <= r.line
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

	query := "SELECT file_path, line, kind, arity, delegate_to, delegate_as FROM definitions WHERE module IN (" +
		strings.Join(placeholders, ",") +
		") AND function = ? AND kind NOT IN ('module', 'defprotocol', 'defimpl', 'callback', 'macrocallback')"

	if arity >= 0 {
		query += " AND arity = ?"
		args = append(args, arity)
	}

	query += " ORDER BY line"
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
	query := "SELECT file_path, line, kind FROM refs WHERE module = ?"
	args := []interface{}{module}
	query += " AND function = ?"
	args = append(args, function)
	query += " ORDER BY file_path, line"

	rows, err := s.db.Query(query, args...)
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
	return results, rows.Err()
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
	rows, err := s.db.Query(
		"SELECT module, file_path, line, kind FROM refs WHERE module = ? OR module LIKE ? ORDER BY file_path, line",
		prefix, prefix+".%",
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
	rows, err := s.db.Query(
		"SELECT module, file_path, line, kind, arity, delegate_to, delegate_as FROM definitions WHERE function = '' AND (module = ? OR module LIKE ?) AND kind IN ('module', 'defprotocol', 'defimpl') ORDER BY module",
		prefix, prefix+".%",
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
		`SELECT module, function, delegate_as, file_path, line FROM definitions
		 WHERE kind = 'defdelegate' AND delegate_to = ? AND delegate_as = '' AND function = ?
		 UNION ALL
		 SELECT module, function, delegate_as, file_path, line FROM definitions
		 WHERE kind = 'defdelegate' AND delegate_to = ? AND delegate_as = ?`,
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
		pathFilter = " AND file_path NOT LIKE ?"
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
			"SELECT module, function, arity, kind, file_path, line FROM definitions WHERE module LIKE ? AND (function LIKE ? OR module LIKE ?)"+pathFilter+
				" ORDER BY CASE WHEN module = ? THEN 0 WHEN INSTR(module || '.' || function, ?) > 0 THEN 1 ELSE 2 END, module, function LIMIT 50",
			args...,
		)
	} else {
		pattern := "%" + query + "%"
		args := append([]interface{}{pattern, pattern}, extraArgs...)
		args = append(args, query, query, query)
		rows, err = s.db.Query(
			"SELECT module, function, arity, kind, file_path, line FROM definitions WHERE (module LIKE ? OR function LIKE ?)"+pathFilter+
				" ORDER BY CASE WHEN module = ? THEN 0 WHEN INSTR(module, ?) > 0 OR INSTR(function, ?) > 0 THEN 1 ELSE 2 END, module, function LIMIT 50",
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
	rows, err := s.db.Query("SELECT DISTINCT module, file_path FROM definitions WHERE function = '__using__'")
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
		"SELECT module FROM definitions WHERE file_path = ? AND function = '' AND kind IN ('module', 'defprotocol') AND line <= ? ORDER BY line DESC LIMIT 1",
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
		"SELECT module, function, arity, line FROM definitions WHERE file_path = ? AND function != '' AND line <= ? ORDER BY line DESC LIMIT 1",
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
		"SELECT module, function, line FROM refs WHERE file_path = ? AND line >= ? AND line <= ? AND kind = 'call' AND function != ''",
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
		"SELECT line FROM definitions WHERE file_path = ? AND function != '' AND line > ? ORDER BY line LIMIT 1",
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
