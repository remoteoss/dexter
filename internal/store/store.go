package store

import (
	"database/sql"
	"os"
	"path/filepath"

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
		db.Close()
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
			kind TEXT NOT NULL,
			line INTEGER NOT NULL,
			file_path TEXT NOT NULL,
			delegate_to TEXT NOT NULL DEFAULT '',
			delegate_as TEXT NOT NULL DEFAULT '',
			FOREIGN KEY (file_path) REFERENCES files(path) ON DELETE CASCADE
		);

		CREATE INDEX IF NOT EXISTS idx_definitions_module ON definitions(module);
		CREATE INDEX IF NOT EXISTS idx_definitions_module_function ON definitions(module, function);
		CREATE INDEX IF NOT EXISTS idx_definitions_file_path ON definitions(file_path);
	`)
	return err
}

func (s *Store) IsEmpty() bool {
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM files").Scan(&count)
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
	defer tx.Rollback()

	_, err = tx.Exec("DELETE FROM definitions WHERE file_path = ?", path)
	if err != nil {
		return err
	}

	_, err = tx.Exec("INSERT OR REPLACE INTO files (path, mtime) VALUES (?, ?)", path, info.ModTime().UnixNano())
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare("INSERT INTO definitions (module, function, kind, line, file_path, delegate_to, delegate_as) VALUES (?, ?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, d := range defs {
		_, err := stmt.Exec(d.Module, d.Function, d.Kind, d.Line, d.FilePath, d.DelegateTo, d.DelegateAs)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) RemoveFile(path string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

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
		"SELECT file_path, line, kind, delegate_to, delegate_as FROM definitions WHERE module = ? AND function = ? AND kind NOT IN ('module', 'defprotocol', 'defimpl')",
		module, function,
	)
}

func (s *Store) queryLookup(query string, args ...interface{}) ([]LookupResult, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
