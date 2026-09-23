package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/remoteoss/dexter/internal/beam"
	"github.com/remoteoss/dexter/internal/evidence"
	"github.com/remoteoss/dexter/internal/parser"
)

const impactClauseColumns = 6
const impactCompiledColumns = 5
const impactUnresolvedColumns = 6
const impactEdgeColumns = 3
const impactEdgeEvidenceColumns = 7

var impactCallableKind = map[string]struct{}{
	"def": {}, "defp": {}, "defmacro": {}, "defmacrop": {},
	"defguard": {}, "defguardp": {}, "defdelegate": {},
	"defstruct": {}, "defexception": {}, "test_root": {},
}

func createSnapshotTemp(destination string) (string, error) {
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".impact-snapshot-*.db")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

func removeSQLiteFiles(path string) {
	if path == "" {
		return
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		_ = os.Remove(candidate)
	}
}

func replaceSnapshot(temporary, destination string) error {
	for _, sidecar := range []string{destination + "-wal", destination + "-shm", destination + "-journal"} {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func syncSnapshot(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// ImpactSnapshotFinalizeStats separates one-time table materialization, index
// creation, and publication from parser and row-write time.
type ImpactSnapshotFinalizeStats struct {
	Compact       time.Duration
	CreateIndexes time.Duration
	Commit        time.Duration
	Rename        time.Duration
}

// ImpactSnapshotBuilder writes an immutable compact snapshot directly. It is
// single-writer only; parser workers send their results to its caller.
type ImpactSnapshotBuilder struct {
	db                   *sql.DB
	tx                   *sql.Tx
	temporary            string
	destination          string
	commit               string
	indexPath            string
	nextFileID           int64
	nextSymbol           int64
	symbolIDs            map[parser.FunctionID]int64
	fileArgs             []interface{}
	clauseArgs           []interface{}
	symbolArgs           []interface{}
	callArgs             []interface{}
	edgeEvidenceArgs     []interface{}
	compiledArgs         []interface{}
	unresolvedArgs       []interface{}
	compiledTestFileArgs []interface{}
	fileIDs              map[string]int64
	compiledComplete     bool
	compiledDigest       string
	sourceIncomplete     bool
	config               evidence.RepositoryConfig
	providers            []evidence.LoadedArtifact
}

// NewImpactSnapshotBuilder creates a private artifact next to destination. The
// completed database replaces destination only after all metadata is durable.
func NewImpactSnapshotBuilder(destination, commit, indexPath string) (*ImpactSnapshotBuilder, error) {
	temporary, err := createSnapshotTemp(destination)
	if err != nil {
		return nil, err
	}
	registerDriver()
	db, err := sql.Open(driverName, temporary+"?_journal_mode=OFF&_synchronous=OFF&_locking_mode=EXCLUSIVE")
	if err != nil {
		removeSQLiteFiles(temporary)
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA temp_store = MEMORY; PRAGMA cache_size = -2000000"); err != nil {
		_ = db.Close()
		removeSQLiteFiles(temporary)
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		_ = db.Close()
		removeSQLiteFiles(temporary)
		return nil, err
	}
	if err := createImpactSchemaWithoutIndexes(tx, ""); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		removeSQLiteFiles(temporary)
		return nil, err
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE impact_definition_clauses (
		module TEXT NOT NULL, function TEXT NOT NULL, arity INTEGER NOT NULL,
		fingerprint BLOB NOT NULL, file_id INTEGER NOT NULL, line INTEGER NOT NULL
	)`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		removeSQLiteFiles(temporary)
		return nil, err
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE impact_compiled_functions (
		module TEXT NOT NULL, function TEXT NOT NULL, arity INTEGER NOT NULL,
		fingerprint BLOB NOT NULL, file_id INTEGER NOT NULL,
		PRIMARY KEY (module, function, arity, file_id)
	) WITHOUT ROWID`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		removeSQLiteFiles(temporary)
		return nil, err
	}
	return &ImpactSnapshotBuilder{
		db: db, tx: tx, temporary: temporary, destination: destination,
		commit: commit, indexPath: filepath.ToSlash(indexPath),
		symbolIDs:            make(map[parser.FunctionID]int64),
		fileIDs:              make(map[string]int64),
		fileArgs:             make([]interface{}, 0, 3*defChunkRows),
		clauseArgs:           make([]interface{}, 0, impactClauseColumns*defChunkRows),
		symbolArgs:           make([]interface{}, 0, symbolColumns*symbolChunkRows),
		callArgs:             make([]interface{}, 0, impactEdgeColumns*callChunkRows),
		edgeEvidenceArgs:     make([]interface{}, 0, impactEdgeEvidenceColumns*callChunkRows),
		compiledArgs:         make([]interface{}, 0, impactCompiledColumns*defChunkRows),
		unresolvedArgs:       make([]interface{}, 0, impactUnresolvedColumns*defChunkRows),
		compiledTestFileArgs: make([]interface{}, 0, defChunkRows),
	}, nil
}

func createImpactSchemaWithoutIndexes(exec dbExecer, schema string) error {
	table := func(name string) string { return schema + name }
	_, err := exec.Exec(`
		CREATE TABLE ` + table("files") + ` (id INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE, mtime INTEGER NOT NULL);
		CREATE TABLE ` + table("impact_functions") + ` (
			module TEXT NOT NULL, function TEXT NOT NULL, arity INTEGER NOT NULL, fingerprint BLOB NOT NULL,
			PRIMARY KEY (module, function, arity)
		) WITHOUT ROWID;
		CREATE TABLE ` + table("impact_function_files") + ` (
			file_id INTEGER NOT NULL, module TEXT NOT NULL, function TEXT NOT NULL, arity INTEGER NOT NULL,
			PRIMARY KEY (file_id, module, function, arity)
		) WITHOUT ROWID;
		CREATE TABLE ` + table("call_symbols") + ` (
			id INTEGER PRIMARY KEY, module TEXT NOT NULL, function TEXT NOT NULL, arity INTEGER NOT NULL,
			UNIQUE (module, function, arity)
		);
		CREATE TABLE ` + table("call_edges") + ` (
			caller_id INTEGER NOT NULL, callee_id INTEGER NOT NULL, kind TEXT NOT NULL,
			PRIMARY KEY (caller_id, callee_id)
		) WITHOUT ROWID;
		CREATE TABLE ` + table("call_edge_evidence") + ` (
			caller_id INTEGER NOT NULL, callee_id INTEGER NOT NULL, provider TEXT NOT NULL,
			kind TEXT NOT NULL, file_id INTEGER NOT NULL, digest TEXT NOT NULL, detail TEXT NOT NULL,
			PRIMARY KEY (caller_id, callee_id, provider, kind, file_id, digest, detail)
		) WITHOUT ROWID;
		CREATE TABLE ` + table("metadata") + ` (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		CREATE TABLE ` + table("impact_evidence_requirements") + ` (
			provider TEXT PRIMARY KEY, satisfied INTEGER NOT NULL, digest TEXT NOT NULL
		) WITHOUT ROWID;
		CREATE TABLE ` + table("impact_unresolved") + ` (
			provider TEXT NOT NULL, caller_module TEXT NOT NULL, caller_function TEXT NOT NULL,
			caller_arity INTEGER NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL,
			PRIMARY KEY (provider, caller_module, caller_function, caller_arity, kind, detail)
		) WITHOUT ROWID;
		CREATE TABLE ` + table("impact_compiled_test_files") + ` (
			file_id INTEGER PRIMARY KEY
		) WITHOUT ROWID;
	`)
	return err
}

// SetRepositoryEvidence adds revision-owned framework mappings and provider
// requirements from dexter-impact.json.
func (b *ImpactSnapshotBuilder) SetRepositoryEvidence(config evidence.RepositoryConfig) {
	if len(config.CompiledBuildRoots) > 0 {
		config.RequiredProviders = appendProviderRequirement(config.RequiredProviders, "compiled")
		config.RequiredProviders = appendProviderRequirement(config.RequiredProviders, "compiled_tests")
	}
	b.config = config
}

func appendProviderRequirement(providers []string, required string) []string {
	for _, provider := range providers {
		if provider == required {
			return providers
		}
	}
	return append(providers, required)
}

func (b *ImpactSnapshotBuilder) SetSourceEvidenceIncomplete() {
	b.sourceIncomplete = true
}

// SetProviderEvidence attaches validated, revision-matched generated evidence.
func (b *ImpactSnapshotBuilder) SetProviderEvidence(providers []evidence.LoadedArtifact) {
	b.providers = providers
}

// AddFile adds one project-relative source file and its impact evidence.
func (b *ImpactSnapshotBuilder) AddFile(path string, mtimeNano int64, defs []parser.Definition, calls []parser.CallEdge) error {
	fileID := b.addFile(path, mtimeNano)
	for i := range defs {
		d := &defs[i]
		if d.Function == "" || d.Fingerprint == ([32]byte{}) {
			continue
		}
		if _, ok := impactCallableKind[d.Kind]; !ok {
			continue
		}
		b.clauseArgs = append(b.clauseArgs, d.Module, d.Function, d.Arity, d.Fingerprint[:], fileID, d.Line)
	}
	for _, call := range calls {
		callerID := b.symbolID(call.Caller)
		calleeID := b.symbolID(call.Callee)
		b.addEdge(callerID, calleeID, call.Kind, "source", call.Kind, fileID, "", "")
	}
	if len(b.fileArgs) >= 3*defChunkRows {
		if err := b.flushChunk(&b.fileArgs, "files", "id,path,mtime", 3, defChunkRows); err != nil {
			return err
		}
	}
	if len(b.clauseArgs) >= impactClauseColumns*defChunkRows {
		if err := b.flushChunk(&b.clauseArgs, "impact_definition_clauses", "module,function,arity,fingerprint,file_id,line", impactClauseColumns, defChunkRows); err != nil {
			return err
		}
	}
	if len(b.symbolArgs) >= symbolColumns*symbolChunkRows {
		if err := b.flushChunk(&b.symbolArgs, "call_symbols", "id,module,function,arity", symbolColumns, symbolChunkRows); err != nil {
			return err
		}
	}
	if len(b.callArgs) >= impactEdgeColumns*callChunkRows {
		if err := b.flushChunk(&b.callArgs, "call_edges", "caller_id,callee_id,kind", impactEdgeColumns, callChunkRows); err != nil {
			return err
		}
	}
	if len(b.edgeEvidenceArgs) >= impactEdgeEvidenceColumns*callChunkRows {
		if err := b.flushChunk(&b.edgeEvidenceArgs, "call_edge_evidence", "caller_id,callee_id,provider,kind,file_id,digest,detail", impactEdgeEvidenceColumns, callChunkRows); err != nil {
			return err
		}
	}
	return nil
}

// AddCompiledEvidence streams one native Dbgi module into the compact writer.
func (b *ImpactSnapshotBuilder) AddCompiledEvidence(path string, compiled beam.CompiledEvidence) error {
	fileID := b.addFile(path, 0)
	if err := b.flushChunk(&b.fileArgs, "files", "id,path,mtime", 3, defChunkRows); err != nil {
		return err
	}
	testModule := strings.HasSuffix(filepath.ToSlash(path), "_test.exs")
	root := parser.FunctionID{Module: compiled.Module, Function: "__dexter_test_root__", Arity: 0}
	if testModule {
		fingerprint := sha256.Sum256([]byte("dexter:compiled-test-root:v1\x00" + compiled.Module))
		b.compiledArgs = append(b.compiledArgs, root.Module, root.Function, root.Arity, fingerprint[:], fileID)
		b.compiledTestFileArgs = append(b.compiledTestFileArgs, fileID)
	}
	for _, function := range compiled.Functions {
		b.compiledArgs = append(b.compiledArgs, function.Function.Module, function.Function.Function,
			function.Function.Arity, function.Fingerprint[:], fileID)
		if testModule {
			callerID := b.symbolID(root)
			calleeID := b.symbolID(function.Function)
			b.addEdge(callerID, calleeID, "compiled_test_owner", "compiled", "test_owner", fileID,
				hex.EncodeToString(compiled.Digest[:]), path)
		}
	}
	for _, edge := range compiled.Edges {
		callerID := b.symbolID(edge.Caller)
		calleeID := b.symbolID(edge.Callee)
		b.addEdge(callerID, calleeID, "compiled:"+edge.Kind, "compiled", edge.Kind, fileID,
			hex.EncodeToString(compiled.Digest[:]), path)
	}
	for _, unresolved := range compiled.Unresolved {
		b.symbolID(unresolved.Caller)
		b.unresolvedArgs = append(b.unresolvedArgs, "compiled", unresolved.Caller.Module,
			unresolved.Caller.Function, unresolved.Caller.Arity, unresolved.Kind, "line "+strconv.Itoa(unresolved.Line))
	}
	if err := b.flushChunk(&b.compiledArgs, "impact_compiled_functions", "module,function,arity,fingerprint,file_id", impactCompiledColumns, defChunkRows); err != nil {
		return err
	}
	if err := b.flushChunk(&b.unresolvedArgs, "impact_unresolved", "provider,caller_module,caller_function,caller_arity,kind,detail", impactUnresolvedColumns, defChunkRows); err != nil {
		return err
	}
	if err := b.flushChunk(&b.compiledTestFileArgs, "impact_compiled_test_files", "file_id", 1, defChunkRows); err != nil {
		return err
	}
	if err := b.flushChunk(&b.symbolArgs, "call_symbols", "id,module,function,arity", symbolColumns, symbolChunkRows); err != nil {
		return err
	}
	if err := b.flushChunk(&b.callArgs, "call_edges", "caller_id,callee_id,kind", impactEdgeColumns, callChunkRows); err != nil {
		return err
	}
	return b.flushChunk(&b.edgeEvidenceArgs, "call_edge_evidence", "caller_id,callee_id,provider,kind,file_id,digest,detail", impactEdgeEvidenceColumns, callChunkRows)
}

// SetCompiledEvidenceComplete marks the configured native BEAM inventory as
// fully scanned. Individual opaque modules remain unresolved records.
func (b *ImpactSnapshotBuilder) SetCompiledEvidenceComplete(digest string) {
	b.compiledComplete = true
	b.compiledDigest = digest
}

func (b *ImpactSnapshotBuilder) AddCompiledEdges(edges []parser.CallEdge) error {
	for _, edge := range edges {
		callerID := b.symbolID(edge.Caller)
		calleeID := b.symbolID(edge.Callee)
		b.addEdge(callerID, calleeID, "compiled:"+edge.Kind, "compiled", edge.Kind, 0, "", "")
	}
	if err := b.flushChunk(&b.symbolArgs, "call_symbols", "id,module,function,arity", symbolColumns, symbolChunkRows); err != nil {
		return err
	}
	if err := b.flushChunk(&b.callArgs, "call_edges", "caller_id,callee_id,kind", impactEdgeColumns, callChunkRows); err != nil {
		return err
	}
	return b.flushChunk(&b.edgeEvidenceArgs, "call_edge_evidence", "caller_id,callee_id,provider,kind,file_id,digest,detail", impactEdgeEvidenceColumns, callChunkRows)
}

func (b *ImpactSnapshotBuilder) addEdge(callerID, calleeID int64, graphKind, provider, evidenceKind string, fileID int64, digest, detail string) {
	b.callArgs = append(b.callArgs, callerID, calleeID, graphKind)
	b.edgeEvidenceArgs = append(b.edgeEvidenceArgs, callerID, calleeID, provider, evidenceKind, fileID, digest, detail)
}

func (b *ImpactSnapshotBuilder) AddCompiledOpaque(module, detail string) error {
	caller := parser.FunctionID{Module: module, Function: "__module_metadata__", Arity: 0}
	b.symbolID(caller)
	b.unresolvedArgs = append(b.unresolvedArgs, "compiled", caller.Module, caller.Function, caller.Arity, "opaque_beam", detail)
	if err := b.flushChunk(&b.symbolArgs, "call_symbols", "id,module,function,arity", symbolColumns, symbolChunkRows); err != nil {
		return err
	}
	return b.flushChunk(&b.unresolvedArgs, "impact_unresolved", "provider,caller_module,caller_function,caller_arity,kind,detail", impactUnresolvedColumns, defChunkRows)
}

func (b *ImpactSnapshotBuilder) addFile(path string, mtimeNano int64) int64 {
	path = filepath.ToSlash(path)
	if id, ok := b.fileIDs[path]; ok {
		return id
	}
	b.nextFileID++
	b.fileIDs[path] = b.nextFileID
	b.fileArgs = append(b.fileArgs, b.nextFileID, path, mtimeNano)
	return b.nextFileID
}

func (b *ImpactSnapshotBuilder) symbolID(fn parser.FunctionID) int64 {
	if id, ok := b.symbolIDs[fn]; ok {
		return id
	}
	b.nextSymbol++
	b.symbolIDs[fn] = b.nextSymbol
	b.symbolArgs = append(b.symbolArgs, b.nextSymbol, fn.Module, fn.Function, fn.Arity)
	return b.nextSymbol
}

func (b *ImpactSnapshotBuilder) flush(args *[]interface{}, table, columns string, count int) error {
	if len(*args) == 0 {
		return nil
	}
	rows := len(*args) / count
	query := impactMultiRowInsert(table, columns, count, rows)
	_, err := b.tx.Exec(query, (*args)...)
	*args = (*args)[:0]
	return err
}

func (b *ImpactSnapshotBuilder) flushChunk(args *[]interface{}, table, columns string, count, rows int) error {
	limit := count * rows
	for len(*args) >= limit {
		if _, err := b.tx.Exec(impactMultiRowInsert(table, columns, count, rows), (*args)[:limit]...); err != nil {
			return err
		}
		*args = (*args)[limit:]
	}
	return nil
}

func impactMultiRowInsert(table, columns string, count, rows int) string {
	query := multiRowInsert(table, columns, count, rows)
	if table == "call_edges" {
		return query + " ON CONFLICT(caller_id, callee_id) DO UPDATE SET kind = MIN(kind, excluded.kind)"
	}
	if table == "call_edge_evidence" || table == "impact_unresolved" || table == "impact_compiled_test_files" {
		return strings.Replace(query, "INSERT INTO", "INSERT OR IGNORE INTO", 1)
	}
	return query
}

func (b *ImpactSnapshotBuilder) flushAll() error {
	for _, pending := range []struct {
		args    *[]interface{}
		table   string
		columns string
		count   int
	}{
		{&b.fileArgs, "files", "id,path,mtime", 3},
		{&b.clauseArgs, "impact_definition_clauses", "module,function,arity,fingerprint,file_id,line", impactClauseColumns},
		{&b.symbolArgs, "call_symbols", "id,module,function,arity", symbolColumns},
		{&b.callArgs, "call_edges", "caller_id,callee_id,kind", impactEdgeColumns},
		{&b.edgeEvidenceArgs, "call_edge_evidence", "caller_id,callee_id,provider,kind,file_id,digest,detail", impactEdgeEvidenceColumns},
		{&b.compiledArgs, "impact_compiled_functions", "module,function,arity,fingerprint,file_id", impactCompiledColumns},
		{&b.unresolvedArgs, "impact_unresolved", "provider,caller_module,caller_function,caller_arity,kind,detail", impactUnresolvedColumns},
		{&b.compiledTestFileArgs, "impact_compiled_test_files", "file_id", 1},
	} {
		if err := b.flush(pending.args, pending.table, pending.columns, pending.count); err != nil {
			return err
		}
	}
	return nil
}

// Finish materializes aggregate inventories, creates traversal indexes, writes
// provenance last, and atomically publishes the artifact.
func (b *ImpactSnapshotBuilder) Finish() (ImpactSnapshotFinalizeStats, error) {
	var stats ImpactSnapshotFinalizeStats
	compactStarted := time.Now()
	if err := b.flushAll(); err != nil {
		b.Abort()
		return stats, err
	}
	if _, err := b.tx.Exec(`
		INSERT INTO impact_functions (module, function, arity, fingerprint)
		SELECT d.module, d.function, d.arity,
			dexter_function_fingerprint(d.fingerprint ORDER BY f.path, d.line)
		FROM impact_definition_clauses d JOIN files f ON f.id = d.file_id
		GROUP BY d.module, d.function, d.arity;
		INSERT INTO impact_function_files (file_id, module, function, arity)
		SELECT DISTINCT file_id, module, function, arity FROM impact_definition_clauses;
		DROP TABLE impact_definition_clauses;
	`); err != nil {
		b.Abort()
		return stats, err
	}
	if err := applyRepositoryEvidence(b.tx, "", b.config); err != nil {
		b.Abort()
		return stats, err
	}
	if b.sourceIncomplete {
		if _, err := b.tx.Exec(`INSERT OR IGNORE INTO impact_evidence_requirements (provider, satisfied, digest)
			VALUES ('source_parse', 0, '')`); err != nil {
			b.Abort()
			return stats, err
		}
	}
	if err := applyProviderEvidence(b.tx, "", b.providers); err != nil {
		b.Abort()
		return stats, err
	}
	if _, err := b.tx.Exec(`
		INSERT INTO impact_functions (module, function, arity, fingerprint)
		SELECT module, function, arity, fingerprint FROM impact_compiled_functions
		WHERE true ORDER BY module, function, arity, file_id
		ON CONFLICT(module, function, arity) DO NOTHING;
		INSERT OR IGNORE INTO impact_function_files (file_id, module, function, arity)
		SELECT file_id, module, function, arity FROM impact_compiled_functions;
		DROP TABLE impact_compiled_functions;
	`); err != nil {
		b.Abort()
		return stats, err
	}
	if b.compiledComplete {
		if _, err := b.tx.Exec(`INSERT INTO impact_evidence_requirements (provider, satisfied, digest)
			VALUES ('compiled', 1, ?)
			ON CONFLICT(provider) DO UPDATE SET satisfied = 1, digest = excluded.digest`, b.compiledDigest); err != nil {
			b.Abort()
			return stats, err
		}
		var missingCompiledTests int
		if err := b.tx.QueryRow(`SELECT COUNT(*) FROM files f
			WHERE substr(f.path, -9) = '_test.exs'
			AND NOT EXISTS (SELECT 1 FROM impact_compiled_test_files c WHERE c.file_id = f.id)`).Scan(&missingCompiledTests); err != nil {
			b.Abort()
			return stats, err
		}
		if missingCompiledTests == 0 {
			if _, err := b.tx.Exec(`INSERT INTO impact_evidence_requirements (provider, satisfied, digest)
				VALUES ('compiled_tests', 1, ?)
				ON CONFLICT(provider) DO UPDATE SET satisfied = 1, digest = excluded.digest`, b.compiledDigest); err != nil {
				b.Abort()
				return stats, err
			}
		}
	}
	stats.Compact = time.Since(compactStarted)
	indexStarted := time.Now()
	if _, err := b.tx.Exec(`
		CREATE INDEX idx_call_edges_caller ON call_edges(caller_id, callee_id, kind);
		CREATE INDEX idx_call_edges_callee ON call_edges(callee_id, caller_id, kind);
	`); err != nil {
		b.Abort()
		return stats, err
	}
	stats.CreateIndexes = time.Since(indexStarted)
	commitStarted := time.Now()
	if _, err := b.tx.Exec(`INSERT INTO metadata (key, value) VALUES
		('impact_snapshot_version', ?), ('impact_commit', ?), ('impact_index_path', ?)`,
		ImpactSnapshotVersion, b.commit, b.indexPath); err != nil {
		b.Abort()
		return stats, err
	}
	if err := b.tx.Commit(); err != nil {
		b.Abort()
		return stats, err
	}
	b.tx = nil
	if err := b.db.Close(); err != nil {
		removeSQLiteFiles(b.temporary)
		return stats, err
	}
	b.db = nil
	if err := syncSnapshot(b.temporary); err != nil {
		removeSQLiteFiles(b.temporary)
		return stats, err
	}
	stats.Commit = time.Since(commitStarted)
	renameStarted := time.Now()
	if err := replaceSnapshot(b.temporary, b.destination); err != nil {
		removeSQLiteFiles(b.temporary)
		return stats, err
	}
	stats.Rename = time.Since(renameStarted)
	b.temporary = ""
	return stats, nil
}

// Abort rolls back and removes an incomplete private artifact.
func (b *ImpactSnapshotBuilder) Abort() {
	if b.tx != nil {
		_ = b.tx.Rollback()
		b.tx = nil
	}
	if b.db != nil {
		_ = b.db.Close()
		b.db = nil
	}
	if b.temporary != "" {
		removeSQLiteFiles(b.temporary)
		b.temporary = ""
	}
}

// exportImpactSnapshot writes a compact artifact directly from a normal index.
func (s *Store) exportImpactSnapshot(destination, projectRoot, commit, indexPath string) error {
	return s.exportImpactSnapshotWithEvidence(context.Background(), destination, projectRoot, commit, indexPath, nil, nil)
}

func (s *Store) exportImpactSnapshotWithEvidence(ctx context.Context, destination, projectRoot, commit, indexPath string,
	providers []evidence.LoadedArtifact, augment func(CompiledEvidenceSink) error,
) error {
	temporary, err := createSnapshotTemp(destination)
	if err != nil {
		return err
	}
	defer func() { removeSQLiteFiles(temporary) }()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS impact", temporary); err != nil {
		return err
	}
	attached := true
	defer func() {
		if attached {
			_, _ = conn.ExecContext(ctx, "DETACH DATABASE impact")
		}
	}()
	if _, err := conn.ExecContext(ctx, "PRAGMA impact.journal_mode = OFF"); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := createImpactSchemaWithoutIndexes(tx, "impact."); err != nil {
		return err
	}

	root := filepath.Clean(projectRoot)
	rootLength := len(root)
	separator := string(filepath.Separator)
	projectFile := "substr(f.path, 1, ?) = ? AND substr(f.path, ? + 1, 1) = ?"
	relativePath := "substr(f.path, ?)"
	notDependency := relativePath + " NOT LIKE 'deps/%' AND " + relativePath + " NOT LIKE '%/deps/%'"
	pathArgs := []interface{}{rootLength, root, rootLength, separator, rootLength + 2, rootLength + 2}

	if _, err := tx.Exec(`INSERT INTO impact.files (id, path, mtime)
		SELECT f.id, `+relativePath+`, f.mtime FROM main.files f
		WHERE `+projectFile+` AND `+notDependency, append([]interface{}{rootLength + 2}, pathArgs...)...); err != nil {
		return err
	}
	definitionWhere := projectFile + " AND " + notDependency +
		" AND d.function != '' AND length(d.fingerprint) = 32 AND d.kind IN (" + impactCallableKinds + ")"
	if _, err := tx.Exec(`INSERT INTO impact.impact_functions (module, function, arity, fingerprint)
		SELECT d.module, d.function, d.arity,
			dexter_function_fingerprint(d.fingerprint ORDER BY f.path, d.line)
		FROM main.definitions d JOIN main.files f ON f.id = d.file_id
		WHERE `+definitionWhere+` GROUP BY d.module, d.function, d.arity`, pathArgs...); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO impact.impact_function_files (file_id, module, function, arity)
		SELECT DISTINCT d.file_id, d.module, d.function, d.arity
		FROM main.definitions d JOIN main.files f ON f.id = d.file_id
		WHERE `+definitionWhere, pathArgs...); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO impact.call_symbols (id, module, function, arity)
		SELECT s.id, s.module, s.function, s.arity
		FROM main.call_symbols s JOIN (
			SELECT e.caller_id AS id FROM main.call_edges e JOIN main.files f ON f.id = e.file_id
			WHERE `+projectFile+` AND `+notDependency+`
			UNION
			SELECT e.callee_id AS id FROM main.call_edges e JOIN main.files f ON f.id = e.file_id
			WHERE `+projectFile+` AND `+notDependency+`
		) used ON used.id = s.id`, append(pathArgs, pathArgs...)...); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO impact.call_edges (caller_id, callee_id, kind)
		SELECT e.caller_id, e.callee_id, MIN(e.kind)
		FROM main.call_edges e JOIN main.files f ON f.id = e.file_id
		WHERE `+projectFile+` AND `+notDependency+`
		GROUP BY e.caller_id, e.callee_id`, pathArgs...); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO impact.call_edge_evidence
		(caller_id, callee_id, provider, kind, file_id, digest, detail)
		SELECT e.caller_id, e.callee_id, 'source', e.kind, e.file_id, '', ''
		FROM main.call_edges e JOIN main.files f ON f.id = e.file_id
		WHERE `+projectFile+` AND `+notDependency, pathArgs...); err != nil {
		return err
	}
	config, _, err := evidence.LoadRepositoryConfig(projectRoot)
	if err != nil {
		return err
	}
	if err := applyRepositoryEvidence(tx, "impact.", config); err != nil {
		return err
	}
	if err := applyProviderEvidence(tx, "impact.", providers); err != nil {
		return err
	}
	compiledSink := &transactionCompiledEvidenceSink{tx: tx, schema: "impact."}
	if augment != nil {
		if err := augment(compiledSink); err != nil {
			return err
		}
	}
	if err := compiledSink.finish(); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		CREATE INDEX impact.idx_call_edges_caller ON call_edges(caller_id, callee_id, kind);
		CREATE INDEX impact.idx_call_edges_callee ON call_edges(callee_id, caller_id, kind);
	`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO impact.metadata (key, value) VALUES
		('impact_snapshot_version', ?), ('impact_commit', ?), ('impact_index_path', ?)`,
		ImpactSnapshotVersion, commit, filepath.ToSlash(indexPath)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "DETACH DATABASE impact"); err != nil {
		return err
	}
	attached = false
	if err := syncSnapshot(temporary); err != nil {
		return err
	}
	if err := replaceSnapshot(temporary, destination); err != nil {
		return err
	}
	return nil
}

type transactionCompiledEvidenceSink struct {
	tx               *sql.Tx
	schema           string
	compiledComplete bool
	compiledDigest   string
}

func (s *transactionCompiledEvidenceSink) AddCompiledEvidence(path string, compiled beam.CompiledEvidence) error {
	fileID, err := ensureImpactFile(s.tx, s.schema, filepath.ToSlash(path))
	if err != nil {
		return err
	}
	testModule := strings.HasSuffix(filepath.ToSlash(path), "_test.exs")
	root := parser.FunctionID{Module: compiled.Module, Function: "__dexter_test_root__", Arity: 0}
	if testModule {
		fingerprint := sha256.Sum256([]byte("dexter:compiled-test-root:v1\x00" + compiled.Module))
		if err := ensureImpactFunction(s.tx, s.schema, root, fingerprint[:], fileID); err != nil {
			return err
		}
		if _, err := s.tx.Exec("INSERT OR IGNORE INTO "+s.schema+"impact_compiled_test_files (file_id) VALUES (?)", fileID); err != nil {
			return err
		}
	}
	digest := hex.EncodeToString(compiled.Digest[:])
	for _, function := range compiled.Functions {
		if err := ensureImpactFunction(s.tx, s.schema, function.Function, function.Fingerprint[:], fileID); err != nil {
			return err
		}
		if testModule {
			if err := insertImpactEdge(s.tx, s.schema, root, function.Function, "compiled_test_owner", "compiled", "test_owner", fileID, digest, path); err != nil {
				return err
			}
		}
	}
	for _, edge := range compiled.Edges {
		if err := insertImpactEdge(s.tx, s.schema, edge.Caller, edge.Callee, "compiled:"+edge.Kind, "compiled", edge.Kind, fileID, digest, path); err != nil {
			return err
		}
	}
	for _, unresolved := range compiled.Unresolved {
		if _, err := internImpactSymbol(s.tx, s.schema, unresolved.Caller); err != nil {
			return err
		}
		if _, err := s.tx.Exec("INSERT OR IGNORE INTO "+s.schema+"impact_unresolved "+
			"(provider, caller_module, caller_function, caller_arity, kind, detail) VALUES ('compiled', ?, ?, ?, ?, ?)",
			unresolved.Caller.Module, unresolved.Caller.Function, unresolved.Caller.Arity, unresolved.Kind,
			"line "+strconv.Itoa(unresolved.Line)); err != nil {
			return err
		}
	}
	return nil
}

func (s *transactionCompiledEvidenceSink) AddCompiledEdges(edges []parser.CallEdge) error {
	for _, edge := range edges {
		if err := insertImpactEdge(s.tx, s.schema, edge.Caller, edge.Callee, "compiled:"+edge.Kind, "compiled", edge.Kind, 0, "", ""); err != nil {
			return err
		}
	}
	return nil
}

func (s *transactionCompiledEvidenceSink) AddCompiledOpaque(module, detail string) error {
	caller := parser.FunctionID{Module: module, Function: "__module_metadata__", Arity: 0}
	if _, err := internImpactSymbol(s.tx, s.schema, caller); err != nil {
		return err
	}
	_, err := s.tx.Exec("INSERT OR IGNORE INTO "+s.schema+"impact_unresolved "+
		"(provider, caller_module, caller_function, caller_arity, kind, detail) VALUES ('compiled', ?, ?, ?, 'opaque_beam', ?)",
		caller.Module, caller.Function, caller.Arity, detail)
	return err
}

func (s *transactionCompiledEvidenceSink) SetCompiledEvidenceComplete(digest string) {
	s.compiledComplete = true
	s.compiledDigest = digest
}

func (s *transactionCompiledEvidenceSink) finish() error {
	if !s.compiledComplete {
		return nil
	}
	if _, err := s.tx.Exec("INSERT INTO "+s.schema+"impact_evidence_requirements (provider, satisfied, digest) VALUES ('compiled', 1, ?) "+
		"ON CONFLICT(provider) DO UPDATE SET satisfied = 1, digest = excluded.digest", s.compiledDigest); err != nil {
		return err
	}
	var missing int
	if err := s.tx.QueryRow("SELECT COUNT(*) FROM " + s.schema + "files f WHERE substr(f.path, -9) = '_test.exs' " +
		"AND NOT EXISTS (SELECT 1 FROM " + s.schema + "impact_compiled_test_files c WHERE c.file_id = f.id)").Scan(&missing); err != nil {
		return err
	}
	if missing != 0 {
		return nil
	}
	_, err := s.tx.Exec("INSERT INTO "+s.schema+"impact_evidence_requirements (provider, satisfied, digest) VALUES ('compiled_tests', 1, ?) "+
		"ON CONFLICT(provider) DO UPDATE SET satisfied = 1, digest = excluded.digest", s.compiledDigest)
	return err
}

func insertImpactEdge(tx *sql.Tx, schema string, caller, callee parser.FunctionID, graphKind, provider, kind string,
	fileID int64, digest, detail string,
) error {
	callerID, err := internImpactSymbol(tx, schema, caller)
	if err != nil {
		return err
	}
	calleeID, err := internImpactSymbol(tx, schema, callee)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO "+schema+"call_edges (caller_id, callee_id, kind) VALUES (?, ?, ?) "+
		"ON CONFLICT(caller_id, callee_id) DO UPDATE SET kind = MIN(kind, excluded.kind)", callerID, calleeID, graphKind); err != nil {
		return err
	}
	_, err = tx.Exec("INSERT OR IGNORE INTO "+schema+"call_edge_evidence "+
		"(caller_id, callee_id, provider, kind, file_id, digest, detail) VALUES (?, ?, ?, ?, ?, ?, ?)",
		callerID, calleeID, provider, kind, fileID, digest, detail)
	return err
}

func applyRepositoryEvidence(tx *sql.Tx, schema string, config evidence.RepositoryConfig) error {
	if len(config.CompiledBuildRoots) > 0 {
		config.RequiredProviders = appendProviderRequirement(config.RequiredProviders, "compiled")
		config.RequiredProviders = appendProviderRequirement(config.RequiredProviders, "compiled_tests")
	}
	for _, provider := range config.RequiredProviders {
		if _, err := tx.Exec("INSERT INTO "+schema+"impact_evidence_requirements (provider, satisfied, digest) VALUES (?, 0, '')", provider); err != nil {
			return err
		}
	}
	for _, mapping := range config.Mappings {
		callerID, err := internImpactSymbol(tx, schema, mapping.Caller)
		if err != nil {
			return err
		}
		calleeID, err := internImpactSymbol(tx, schema, mapping.Callee)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO "+schema+"call_edges (caller_id, callee_id, kind) VALUES (?, ?, ?) "+
			"ON CONFLICT(caller_id, callee_id) DO UPDATE SET kind = MIN(kind, excluded.kind)",
			callerID, calleeID, "repository:"+mapping.Kind); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT OR IGNORE INTO "+schema+"call_edge_evidence "+
			"(caller_id, callee_id, provider, kind, file_id, digest, detail) VALUES (?, ?, 'repository', ?, 0, '', '')",
			callerID, calleeID, mapping.Kind); err != nil {
			return err
		}
	}
	return nil
}

func applyProviderEvidence(tx *sql.Tx, schema string, providers []evidence.LoadedArtifact) error {
	for _, loaded := range providers {
		artifact := loaded.Artifact
		provenance, err := json.Marshal(artifact.Provenance)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO "+schema+"impact_evidence_requirements (provider, satisfied, digest) VALUES (?, 1, ?) "+
			"ON CONFLICT(provider) DO UPDATE SET satisfied = 1, digest = excluded.digest", artifact.Provider, loaded.Digest); err != nil {
			return err
		}
		for _, function := range artifact.Functions {
			fileID, err := ensureImpactFile(tx, schema, function.File)
			if err != nil {
				return err
			}
			fingerprint, err := hex.DecodeString(function.Fingerprint)
			if err != nil {
				return err
			}
			if err := ensureImpactFunction(tx, schema, function.Function, fingerprint, fileID); err != nil {
				return err
			}
		}
		for _, ownership := range artifact.TestOwnership {
			fileID, err := ensureImpactFile(tx, schema, ownership.File)
			if err != nil {
				return err
			}
			if err := ensureSyntheticImpactFunction(tx, schema, ownership.Root, artifact.Provider, ownership.File, fileID); err != nil {
				return err
			}
			if err := ensureSyntheticImpactFunction(tx, schema, ownership.Function, artifact.Provider, ownership.File, fileID); err != nil {
				return err
			}
			callerID, err := internImpactSymbol(tx, schema, ownership.Root)
			if err != nil {
				return err
			}
			calleeID, err := internImpactSymbol(tx, schema, ownership.Function)
			if err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO "+schema+"call_edges (caller_id, callee_id, kind) VALUES (?, ?, ?) "+
				"ON CONFLICT(caller_id, callee_id) DO UPDATE SET kind = MIN(kind, excluded.kind)",
				callerID, calleeID, "provider:"+artifact.Provider+":test_owner"); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT OR IGNORE INTO "+schema+"call_edge_evidence "+
				"(caller_id, callee_id, provider, kind, file_id, digest, detail) VALUES (?, ?, ?, 'test_owner', ?, ?, ?)",
				callerID, calleeID, artifact.Provider, fileID, loaded.Digest, string(provenance)); err != nil {
				return err
			}
		}
		for _, edge := range artifact.Edges {
			callerID, err := internImpactSymbol(tx, schema, edge.Caller)
			if err != nil {
				return err
			}
			calleeID, err := internImpactSymbol(tx, schema, edge.Callee)
			if err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT INTO "+schema+"call_edges (caller_id, callee_id, kind) VALUES (?, ?, ?) "+
				"ON CONFLICT(caller_id, callee_id) DO UPDATE SET kind = MIN(kind, excluded.kind)",
				callerID, calleeID, "provider:"+artifact.Provider+":"+edge.Kind); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT OR IGNORE INTO "+schema+"call_edge_evidence "+
				"(caller_id, callee_id, provider, kind, file_id, digest, detail) VALUES (?, ?, ?, ?, 0, ?, ?)",
				callerID, calleeID, artifact.Provider, edge.Kind, loaded.Digest, string(provenance)); err != nil {
				return err
			}
		}
		for _, unresolved := range artifact.Unresolved {
			if _, err := internImpactSymbol(tx, schema, unresolved.Caller); err != nil {
				return err
			}
			if _, err := tx.Exec("INSERT OR IGNORE INTO "+schema+"impact_unresolved "+
				"(provider, caller_module, caller_function, caller_arity, kind, detail) VALUES (?, ?, ?, ?, ?, ?)",
				artifact.Provider, unresolved.Caller.Module, unresolved.Caller.Function, unresolved.Caller.Arity,
				unresolved.Kind, unresolved.Detail); err != nil {
				return err
			}
		}
	}
	return nil
}

func ensureImpactFile(tx *sql.Tx, schema, path string) (int64, error) {
	var id int64
	err := tx.QueryRow("SELECT id FROM "+schema+"files WHERE path = ?", path).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	result, err := tx.Exec("INSERT INTO "+schema+"files (path, mtime) VALUES (?, 0)", path)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func ensureImpactFunction(tx *sql.Tx, schema string, function parser.FunctionID, fingerprint []byte, fileID int64) error {
	if _, err := tx.Exec("INSERT OR IGNORE INTO "+schema+"impact_functions (module, function, arity, fingerprint) VALUES (?, ?, ?, ?)",
		function.Module, function.Function, function.Arity, fingerprint); err != nil {
		return err
	}
	_, err := tx.Exec("INSERT OR IGNORE INTO "+schema+"impact_function_files (file_id, module, function, arity) VALUES (?, ?, ?, ?)",
		fileID, function.Module, function.Function, function.Arity)
	return err
}

func ensureSyntheticImpactFunction(tx *sql.Tx, schema string, function parser.FunctionID, provider, path string, fileID int64) error {
	fingerprint := sha256.Sum256([]byte("dexter:provider-function:v1\x00" + provider + "\x00" + path + "\x00" +
		function.Module + "\x00" + function.Function + "\x00" + strconv.Itoa(function.Arity)))
	return ensureImpactFunction(tx, schema, function, fingerprint[:], fileID)
}

func internImpactSymbol(tx *sql.Tx, schema string, function parser.FunctionID) (int64, error) {
	var id int64
	err := tx.QueryRow("SELECT id FROM "+schema+"call_symbols WHERE module = ? AND function = ? AND arity = ?",
		function.Module, function.Function, function.Arity).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	result, err := tx.Exec("INSERT INTO "+schema+"call_symbols (module, function, arity) VALUES (?, ?, ?)",
		function.Module, function.Function, function.Arity)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}
