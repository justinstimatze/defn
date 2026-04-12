// Package store manages the Dolt database that holds all code definitions,
// references, and version history. Dolt provides native git semantics
// (branch, merge, diff, commit) on structured data.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/dolthub/driver"
	"github.com/dolthub/dolt/go/store/util/tempfiles"
	_ "github.com/go-sql-driver/mysql"
)

//go:embed schema.sql
var schemaSQL string

// DB wraps a Dolt connection with code-database operations.
// DB is NOT safe for concurrent use from multiple goroutines.
type DB struct {
	db     *sql.DB
	conn   *sql.Conn // pinned connection for branch-state consistency
	path   string    // filesystem path to database directory
	dbName string    // database name (for USE db/branch)
}

// Open opens or creates a defn database.
//
// path can be:
//   - A filesystem path (e.g. ".defn") — uses embedded Dolt driver
//   - A MySQL DSN (contains "@") — connects to a running dolt sql-server
//     e.g. "root@tcp(127.0.0.1:3306)/defn"
func Open(path string) (*DB, error) {
	if strings.Contains(path, "@") {
		return openMySQL(path)
	}
	return openEmbedded(path)
}

// execQuerier is the common subset of *sql.DB and *sql.Conn used by
// schema init + migration helpers so they work for both the MySQL
// (pooled) and embedded Dolt (pinned connection) paths.
type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func openMySQL(dsn string) (*DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to dolt server: %w", err)
	}
	ctx := context.Background()
	if err := prepareSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db, path: dsn}, nil
}

func openEmbedded(path string) (*DB, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}
	parentDir := filepath.Dir(absPath)
	dbName := filepath.Base(absPath)
	dsn := fmt.Sprintf("file://%s?commitname=defn&commitemail=defn@localhost&database=%s",
		filepath.ToSlash(parentDir), dbName)

	db, err := sql.Open("dolt", dsn)
	if err != nil {
		return nil, fmt.Errorf("open dolt: %w", err)
	}
	ctx := context.Background()

	// Pin a single connection for all operations. The embedded Dolt driver
	// tracks branch state per-connection, so using the pool would lose
	// checkout state between calls.
	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("pin connection: %w", err)
	}
	if err := selectEmbeddedDatabase(ctx, conn, dbName); err != nil {
		conn.Close()
		db.Close()
		return nil, err
	}
	if err := prepareSchema(ctx, conn); err != nil {
		conn.Close()
		db.Close()
		return nil, err
	}
	return &DB{db: db, conn: conn, path: absPath, dbName: dbName}, nil
}

func selectEmbeddedDatabase(ctx context.Context, conn *sql.Conn, dbName string) error {
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", dbName)); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("USE `%s`", dbName)); err != nil {
		return fmt.Errorf("use database: %w", err)
	}
	return nil
}

// prepareSchema runs schema.sql (idempotent) and applies any pending
// data migrations. Used by both Open paths.
func prepareSchema(ctx context.Context, db execQuerier) error {
	if err := initSchema(ctx, db); err != nil {
		return err
	}
	if err := migrateReferencesToRefs(ctx, db); err != nil {
		return fmt.Errorf("migrate references table: %w", err)
	}
	return nil
}

func initSchema(ctx context.Context, db execQuerier) error {
	for _, stmt := range splitSQL(schemaSQL) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			errMsg := strings.ToLower(err.Error())
			if strings.Contains(errMsg, "already exists") ||
				strings.Contains(errMsg, "duplicate") {
				continue
			}
			return fmt.Errorf("init schema: %w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

// migrateReferencesToRefs renames the old `references` table to `refs`.
// The rename happened on 2026-04-11 to avoid having to backtick every
// query (`references` is a reserved word in MySQL/Dolt). Databases
// created before that point have data in the old table; this copies
// it into the new one and drops the old table. Runs at Open so existing
// databases keep working without a manual reingest.
func migrateReferencesToRefs(ctx context.Context, db execQuerier) error {
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_name = 'references'",
	).Scan(&count)
	if err != nil || count == 0 {
		return nil // no old table, nothing to do
	}
	if _, err := db.ExecContext(ctx,
		"INSERT IGNORE INTO refs (from_def, to_def, kind) SELECT from_def, to_def, kind FROM `references`",
	); err != nil {
		return fmt.Errorf("copy rows: %w", err)
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE `references`"); err != nil {
		return fmt.Errorf("drop old table: %w", err)
	}
	return nil
}

// splitSQL splits a SQL script by semicolons, filtering out comment-only lines.
func splitSQL(s string) []string {
	// First strip comment lines.
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "--") {
			lines = append(lines, line)
		}
	}
	cleaned := strings.Join(lines, "\n")

	var stmts []string
	for part := range strings.SplitSeq(cleaned, ";") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			stmts = append(stmts, trimmed)
		}
	}
	return stmts
}

// execContext runs ExecContext on the pinned connection (embedded) or pool (MySQL).
func (s *DB) execContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if s.conn != nil {
		return s.conn.ExecContext(ctx, query, args...)
	}
	return s.db.ExecContext(ctx, query, args...)
}

// queryContext runs QueryContext on the pinned connection (embedded) or pool (MySQL).
func (s *DB) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if s.conn != nil {
		return s.conn.QueryContext(ctx, query, args...)
	}
	return s.db.QueryContext(ctx, query, args...)
}

// queryRowContext runs QueryRowContext on the pinned connection (embedded) or pool (MySQL).
func (s *DB) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if s.conn != nil {
		return s.conn.QueryRowContext(ctx, query, args...)
	}
	return s.db.QueryRowContext(ctx, query, args...)
}

// Close closes the database and cleans up Dolt's temporary files.
// Note: temp file cleanup is global (Dolt's MovableTempFileProvider is
// process-wide). Safe because defn uses one embedded DB per process.
func (s *DB) Close() error {
	if s.conn != nil {
		s.conn.Close()
	}
	err := s.db.Close()
	tempfiles.MovableTempFileProvider.Clean()
	return err
}

// CleanTempFiles removes Dolt's accumulated temp files. The embedded Dolt
// storage engine creates UUID-named temp files in /tmp for table persistence
// and manifests. In long-lived processes (defn serve), these leak because
// Clean() is only called on Close(). Call this after write operations.
func (s *DB) CleanTempFiles() {
	tempfiles.MovableTempFileProvider.Clean()
}

// GC runs Dolt's garbage collection to compact the noms store.
func (s *DB) GC() error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_GC()")
	if err != nil {
		return fmt.Errorf("dolt gc: %w", err)
	}
	s.CleanTempFiles()
	return nil
}

// Path returns the filesystem path of this database.
func (s *DB) Path() string {
	return s.path
}

// Begin starts a transaction. Returns a function to commit or rollback.
func (s *DB) Begin() (commit func() error, rollback func(), err error) {
	_, err = s.execContext(s.Ctx(), "START TRANSACTION")
	if err != nil {
		return nil, nil, fmt.Errorf("begin transaction: %w", err)
	}
	commit = func() error {
		_, err := s.execContext(s.Ctx(), "COMMIT")
		return err
	}
	rollback = func() {
		s.execContext(s.Ctx(), "ROLLBACK")
	}
	return commit, rollback, nil
}

// Ctx returns a background context for database operations.
// All DB methods use this rather than accepting context parameters,
// since the MCP server is single-threaded and cancellation isn't needed.
func (s *DB) Ctx() context.Context {
	return context.Background()
}

// --- Dolt Version Control ---

// Commit stages all changes and creates a Dolt commit.
// Returns nil if there's nothing to commit. If there's nothing new to
// commit but the last commit was an auto-sync, amends it with the
// user's message so labeled commits aren't silently swallowed.
func (s *DB) Commit(message string) error {
	ctx := s.Ctx()
	if _, err := s.execContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
		return fmt.Errorf("dolt add: %w", err)
	}
	if _, err := s.execContext(ctx, "CALL DOLT_COMMIT('-m', ?)", message); err != nil {
		if strings.Contains(err.Error(), "nothing to commit") {
			// If the caller provided a real message and the last commit
			// was an auto-sync, amend it with the user's message.
			if message != "auto-sync" {
				return s.amendLastAutoSync(ctx, message)
			}
			return nil
		}
		return fmt.Errorf("dolt commit: %w", err)
	}
	s.CleanTempFiles()
	return nil
}

// amendLastAutoSync amends the most recent commit if its message is
// "auto-sync", replacing it with the given message. Returns nil if the
// last commit isn't an auto-sync (nothing to amend).
func (s *DB) amendLastAutoSync(ctx context.Context, message string) error {
	var lastMsg string
	if err := s.queryRowContext(ctx,
		"SELECT message FROM dolt_log LIMIT 1",
	).Scan(&lastMsg); err != nil {
		return nil // can't read log — not critical
	}
	if lastMsg != "auto-sync" {
		return nil
	}
	if _, err := s.execContext(ctx,
		"CALL DOLT_COMMIT('--amend', '-m', ?)", message); err != nil {
		return fmt.Errorf("amend commit: %w", err)
	}
	return nil
}

// Branch creates a new branch at the current HEAD.
func (s *DB) Branch(name string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_BRANCH(?)", name)
	return err
}

// Checkout switches to a branch.
func (s *DB) Checkout(name string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_CHECKOUT(?)", name)
	return err
}

// Merge merges a branch into the current branch.
func (s *DB) Merge(branchName string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_MERGE(?)", branchName)
	s.CleanTempFiles()
	return err
}

// AddRemote adds a named remote pointing to a file path or URL.
func (s *DB) AddRemote(name, url string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_REMOTE('add', ?, ?)", name, url)
	return err
}

// Push pushes the current branch to a remote.
func (s *DB) Push(remote, branch string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_PUSH(?, ?)", remote, branch)
	return err
}

// Pull pulls from a remote into the current branch.
func (s *DB) Pull(remote, branch string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_PULL(?, ?)", remote, branch)
	return err
}

// Fetch fetches from a remote without merging.
func (s *DB) Fetch(remote string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_FETCH(?)", remote)
	return err
}

// GetCurrentBranch returns the active branch name.
func (s *DB) GetCurrentBranch() (string, error) {
	var branch string
	err := s.queryRowContext(s.Ctx(), "SELECT active_branch()").Scan(&branch)
	return branch, err
}

// ListBranches returns all branch names.
func (s *DB) ListBranches() ([]string, error) {
	rows, err := s.queryContext(s.Ctx(), "SELECT name FROM dolt_branches ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

// Log returns recent commits.
func (s *DB) Log(limit int) ([]map[string]any, error) {
	rows, err := s.queryContext(s.Ctx(),
		"SELECT commit_hash, committer, message, date FROM dolt_log LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []map[string]any
	for rows.Next() {
		var hash, committer, message, date string
		if err := rows.Scan(&hash, &committer, &message, &date); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"hash": hash, "committer": committer, "message": message, "date": date,
		})
	}
	return results, rows.Err()
}

// Diff returns changes in the working set (uncommitted changes).
func (s *DB) Diff() ([]map[string]any, error) {
	rows, err := s.queryContext(s.Ctx(),
		`SELECT table_name, staged, status FROM dolt_status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var results []map[string]any
	for rows.Next() {
		var table, status string
		var staged bool
		if err := rows.Scan(&table, &staged, &status); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"table": table, "status": status, "staged": staged,
		})
	}
	return results, rows.Err()
}

// DiffDefinitions returns definitions that changed since the last commit.
func (s *DB) DiffDefinitions() ([]map[string]any, error) {
	rows, err := s.queryContext(s.Ctx(),
		`SELECT diff_type, from_name, from_kind, to_name, to_kind, from_hash, to_hash
		 FROM dolt_diff_definitions
		 WHERE from_commit = HASHOF('HEAD') AND to_commit = 'WORKING'`)
	if err != nil {
		// Table might not have changes.
		return nil, nil
	}
	defer rows.Close()
	var results []map[string]any
	for rows.Next() {
		var diffType string
		var fromName, fromKind, toName, toKind, fromHash, toHash sql.NullString
		if err := rows.Scan(&diffType, &fromName, &fromKind, &toName, &toKind, &fromHash, &toHash); err != nil {
			return nil, err
		}
		results = append(results, map[string]any{
			"diff_type": diffType,
			"from_name": fromName.String, "from_kind": fromKind.String,
			"to_name": toName.String, "to_kind": toKind.String,
			"from_hash": fromHash.String, "to_hash": toHash.String,
		})
	}
	return results, rows.Err()
}

// --- Types ---

// Module represents a Go package/module in the database.
type Module struct {
	ID   int64
	Path string
	Name string
	Doc  string
}

// Definition represents a single Go definition (function, type, method, etc.).
type Definition struct {
	ID         int64
	ModuleID   int64
	Name       string
	Kind       string
	Exported   bool
	Test       bool
	Receiver   string
	Signature  string
	Body       string
	Doc        string
	StartLine  int
	EndLine    int
	SourceFile string
	Hash       string
}

// Reference represents a reference from one definition to another.
type Reference struct {
	FromDef int64
	ToDef   int64
	Kind    string
}

// Import represents an import recorded for a module.
type Import struct {
	ModuleID     int64
	ImportedPath string
	Alias        string
}

// --- Module CRUD ---

// EnsureModule creates or returns an existing module.
func (s *DB) EnsureModule(path, name, doc string) (*Module, error) {
	var m Module
	err := s.queryRowContext(s.Ctx(),
		"SELECT id, path, name, COALESCE(doc,'') FROM modules WHERE path = ?", path,
	).Scan(&m.ID, &m.Path, &m.Name, &m.Doc)
	if err == sql.ErrNoRows {
		res, err := s.execContext(s.Ctx(),
			"INSERT INTO modules (path, name, doc) VALUES (?, ?, ?)", path, name, doc)
		if err != nil {
			return nil, fmt.Errorf("insert module: %w", err)
		}
		m.ID, _ = res.LastInsertId()
		m.Path = path
		m.Name = name
		m.Doc = doc
		return &m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query module: %w", err)
	}
	// Update doc if changed.
	if m.Doc != doc && doc != "" {
		s.execContext(s.Ctx(), "UPDATE modules SET doc = ? WHERE id = ?", doc, m.ID)
		m.Doc = doc
	}
	return &m, nil
}

// GetModuleByPath returns a module by its path.
func (s *DB) GetModuleByPath(path string) (*Module, error) {
	var m Module
	err := s.queryRowContext(s.Ctx(),
		"SELECT id, path, name, COALESCE(doc,'') FROM modules WHERE path = ?", path,
	).Scan(&m.ID, &m.Path, &m.Name, &m.Doc)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListModules returns all modules.
func (s *DB) ListModules() ([]Module, error) {
	rows, err := s.queryContext(s.Ctx(), "SELECT id, path, name, COALESCE(doc,'') FROM modules ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var modules []Module
	for rows.Next() {
		var m Module
		if err := rows.Scan(&m.ID, &m.Path, &m.Name, &m.Doc); err != nil {
			return nil, err
		}
		modules = append(modules, m)
	}
	return modules, rows.Err()
}

// --- Definition CRUD ---

// HashBody computes the content hash of a definition body.
func HashBody(body string) string {
	h := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", h)
}

// UpsertDefinition inserts or updates a definition, returning its ID.
// If the hash is unchanged, it's a no-op.
func (s *DB) UpsertDefinition(d *Definition) (int64, error) {
	d.Hash = HashBody(d.Body)
	ctx := s.Ctx()

	var existingID int64
	var existingHash string
	err := s.queryRowContext(ctx,
		`SELECT id, hash FROM definitions
		 WHERE module_id = ? AND name = ? AND kind = ? AND COALESCE(receiver,'') = COALESCE(?,'') AND test = ?`,
		d.ModuleID, d.Name, d.Kind, d.Receiver, d.Test,
	).Scan(&existingID, &existingHash)

	if err == sql.ErrNoRows {
		res, err := s.execContext(ctx,
			`INSERT INTO definitions
			 (module_id, name, kind, exported, test, receiver, signature, doc, start_line, end_line, source_file, hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ModuleID, d.Name, d.Kind, d.Exported, d.Test, d.Receiver,
			d.Signature, d.Doc, d.StartLine, d.EndLine, d.SourceFile, d.Hash,
		)
		if err != nil {
			return 0, fmt.Errorf("insert definition: %w", err)
		}
		id, _ := res.LastInsertId()
		if _, err := s.execContext(ctx,
			`INSERT INTO bodies (def_id, body) VALUES (?, ?)`, id, d.Body,
		); err != nil {
			return 0, fmt.Errorf("insert body: %w", err)
		}
		return id, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query definition: %w", err)
	}

	if existingHash == d.Hash {
		// Body unchanged — only update location fields if they actually differ.
		if _, err := s.execContext(ctx,
			`UPDATE definitions SET start_line=?, end_line=?, source_file=?
			 WHERE id=? AND (start_line != ? OR end_line != ? OR source_file != ?)`,
			d.StartLine, d.EndLine, d.SourceFile,
			existingID, d.StartLine, d.EndLine, d.SourceFile,
		); err != nil {
			return 0, fmt.Errorf("update location: %w", err)
		}
		return existingID, nil
	}

	if _, err := s.execContext(ctx,
		`UPDATE definitions
		 SET exported=?, signature=?, doc=?, start_line=?, end_line=?, source_file=?, hash=?
		 WHERE id=?`,
		d.Exported, d.Signature, d.Doc,
		d.StartLine, d.EndLine, d.SourceFile, d.Hash, existingID,
	); err != nil {
		return 0, fmt.Errorf("update definition: %w", err)
	}
	if _, err := s.execContext(ctx,
		`REPLACE INTO bodies (def_id, body) VALUES (?, ?)`,
		existingID, d.Body,
	); err != nil {
		return 0, fmt.Errorf("update body: %w", err)
	}
	return existingID, nil
}

// DeleteDefinition removes a definition and associated data.
func (s *DB) DeleteDefinition(id int64) error {
	ctx := s.Ctx()
	if _, err := s.execContext(ctx, "DELETE FROM refs WHERE from_def = ? OR to_def = ?", id, id); err != nil {
		return fmt.Errorf("delete references for def %d: %w", id, err)
	}
	if _, err := s.execContext(ctx, "DELETE FROM bodies WHERE def_id = ?", id); err != nil {
		return fmt.Errorf("delete body for def %d: %w", id, err)
	}
	if _, err := s.execContext(ctx, "DELETE FROM definitions WHERE id = ?", id); err != nil {
		return fmt.Errorf("delete definition %d: %w", id, err)
	}
	return nil
}

// PruneStaleDefinitions removes definitions not in the given set of IDs.
func (s *DB) PruneStaleDefinitions(liveIDs map[int64]bool) (int, error) {
	if len(liveIDs) == 0 {
		return 0, nil
	}
	rows, err := s.queryContext(s.Ctx(), "SELECT id FROM definitions")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var staleIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan definition id: %w", err)
		}
		if !liveIDs[id] {
			staleIDs = append(staleIDs, id)
		}
	}
	for _, id := range staleIDs {
		if err := s.DeleteDefinition(id); err != nil {
			return 0, fmt.Errorf("prune def %d: %w", id, err)
		}
	}
	return len(staleIDs), nil
}

// --- Definition Reads ---

// FindDefinitionsByFile finds definitions in a module matching a file path suffix.
// If line > 0, returns only the definition containing that line.
// The fileSuffix is matched against module paths (e.g. "internal/mcp" matches
// "github.com/justinstimatze/defn/internal/mcp").
// If sourceFile is non-empty, results are further filtered to definitions
// whose source_file matches exactly (e.g. "internal/mcp/server.go").
func (s *DB) FindDefinitionsByFile(fileSuffix string, sourceFile string, line int) ([]Definition, error) {
	query := `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test,
	            COALESCE(d.receiver,''), COALESCE(d.signature,''),
	            COALESCE(d.start_line,0), COALESCE(d.end_line,0)
	          FROM definitions d
	          JOIN modules m ON d.module_id = m.id
	          WHERE m.path LIKE ?`
	args := []any{"%" + fileSuffix + "%"}

	if sourceFile != "" {
		query += " AND d.source_file = ?"
		args = append(args, sourceFile)
	}

	if line > 0 {
		query += " AND d.start_line <= ? AND d.end_line >= ? AND d.start_line > 0"
		args = append(args, line, line)
	}
	query += " ORDER BY d.start_line"

	rows, err := s.queryContext(s.Ctx(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var defs []Definition
	for rows.Next() {
		var d Definition
		if err := rows.Scan(&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test,
			&d.Receiver, &d.Signature, &d.StartLine, &d.EndLine); err != nil {
			return nil, err
		}
		defs = append(defs, d)
	}
	return defs, rows.Err()
}

// GetDefinition returns a definition by ID, including its body.
func (s *DB) GetDefinition(id int64) (*Definition, error) {
	d := &Definition{}
	err := s.queryRowContext(s.Ctx(),
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        d.signature, COALESCE(b.body, ''), COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 WHERE d.id = ?`, id,
	).Scan(&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test, &d.Receiver,
		&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// SearchDefinitions performs full-text search on definition bodies and doc comments.
// Returns definitions ranked by relevance.
func (s *DB) SearchDefinitions(query string) ([]Definition, error) {
	// Search doc comments and bodies via LIKE.
	// Dolt's FULLTEXT indexes don't work with MATCH AGAINST in embedded mode,
	// so we fall back to LIKE. Acceptable for typical project sizes (<10K defs).
	likePattern := "%" + query + "%"
	rows, err := s.queryContext(s.Ctx(),
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), '', COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 WHERE d.doc LIKE ?
		    OR d.id IN (SELECT def_id FROM bodies WHERE body LIKE ?)
		 ORDER BY d.name
		 LIMIT 100`, likePattern, likePattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitions(rows)
}

// FindDefinitions searches by name pattern (SQL LIKE). No bodies.
func (s *DB) FindDefinitions(namePattern string) ([]Definition, error) {
	rows, err := s.queryContext(s.Ctx(),
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), '', COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 WHERE d.name LIKE ? OR COALESCE(d.signature,'') LIKE ?
		 ORDER BY d.name`, namePattern, namePattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitions(rows)
}

// GetDefinitionByName returns a definition by exact name and optional module path.
// Module path supports fuzzy matching — "gin" matches "github.com/gin-gonic/gin".
// When multiple definitions match the same name, returns the one with the most
// callers (highest blast radius) to pick the most relevant one.
//
// Also accepts file:line syntax (e.g. "internal/mcp/server.go:272") to find
// the definition at a specific file location.
func (s *DB) GetDefinitionByName(name, modulePath string) (*Definition, error) {
	// Check for receiver.method syntax (e.g. "Context.Render", "(*Context).Render").
	if strings.Contains(name, ".") && !strings.Contains(name, "/") {
		// Parse "Context.Render", "(*Context).Render", "*Context.Render"
		dotIdx := strings.LastIndex(name, ".")
		recv := strings.TrimSpace(name[:dotIdx])
		methName := strings.TrimSpace(name[dotIdx+1:])
		// Normalize receiver: strip parens from "(*Context)" → "*Context"
		recv = strings.TrimPrefix(recv, "(")
		recv = strings.TrimSuffix(recv, ")")
		if methName != "" && recv != "" {
			// Exact receiver match.
			if d, err := s.GetDefinitionByNameAndReceiver(methName, modulePath, recv); err == nil {
				return d, nil
			}
			// Try with/without * prefix.
			if strings.HasPrefix(recv, "*") {
				if d, err := s.GetDefinitionByNameAndReceiver(methName, modulePath, recv[1:]); err == nil {
					return d, nil
				}
			} else {
				if d, err := s.GetDefinitionByNameAndReceiver(methName, modulePath, "*"+recv); err == nil {
					return d, nil
				}
			}
			// Suffix match: *Router matches *DefaultRouter, *Echo matches *echo.Echo
			bareRecv := strings.TrimPrefix(recv, "*")
			prefix := ""
			if strings.HasPrefix(recv, "*") {
				prefix = "*"
			}
			if d, err := s.fuzzyReceiverLookup(methName, modulePath, bareRecv, prefix); err == nil {
				return d, nil
			}
			// Last resort: name-only lookup (ignores receiver, picks highest blast radius).
			// This handles cases where diff says method but defn stores as standalone func.
		}
	}

	// Check for file:line syntax (e.g. "internal/mcp/server.go:272").
	if parts := strings.SplitN(name, ":", 2); len(parts) == 2 {
		if line, err := strconv.Atoi(parts[1]); err == nil && line > 0 {
			filePath := parts[0]
			// Strip filename to get package directory for module matching.
			dir := filePath
			if idx := strings.LastIndex(dir, "/"); idx >= 0 {
				dir = dir[:idx]
			} else {
				dir = strings.TrimSuffix(dir, "_test.go")
				dir = strings.TrimSuffix(dir, ".go")
			}
			defs, err := s.FindDefinitionsByFile(dir, filePath, line)
			if err != nil {
				return nil, err
			}
			if len(defs) == 0 {
				return nil, fmt.Errorf("no definition at %s", name)
			}
			// Return the full definition with body.
			return s.GetDefinition(defs[0].ID)
		}
	}

	baseQuery := `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
	                 COALESCE(d.signature,''), COALESCE(b.body, ''), COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
	          FROM definitions d
	          LEFT JOIN bodies b ON b.def_id = d.id`
	var args []any

	if modulePath != "" {
		// Try exact match first.
		query := baseQuery + " JOIN modules m ON d.module_id = m.id WHERE d.name = ? AND m.path = ?"
		d := &Definition{}
		err := s.queryRowContext(s.Ctx(), query, name, modulePath).Scan(
			&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test, &d.Receiver,
			&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash,
		)
		if err == nil {
			return d, nil
		}
		// Fuzzy match: module path contains the search term.
		query = baseQuery + " JOIN modules m ON d.module_id = m.id WHERE d.name = ? AND m.path LIKE ?" +
			` ORDER BY (SELECT COUNT(*) FROM refs r WHERE r.to_def = d.id) DESC LIMIT 1`
		d = &Definition{}
		err = s.queryRowContext(s.Ctx(), query, name, "%"+modulePath+"%").Scan(
			&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test, &d.Receiver,
			&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash,
		)
		if err == nil {
			return d, nil
		}
		// Fall through to name-only lookup if module didn't match anything.
	}

	// Name-only lookup: pick the definition with the most NON-TEST callers.
	// This prefers production code (e.g., (*Context).Render with 16 production callers)
	// over interface implementations (e.g., render.BSON.Render called mainly by tests).
	query := baseQuery + " WHERE d.name = ?" +
		` ORDER BY (SELECT COUNT(*) FROM refs r
		  JOIN definitions caller ON caller.id = r.from_def AND caller.test = FALSE
		  WHERE r.to_def = d.id) DESC LIMIT 1`
	args = append(args, name)

	d := &Definition{}
	err := s.queryRowContext(s.Ctx(), query, args...).Scan(
		&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test, &d.Receiver,
		&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash,
	)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// GetDefinitionByNameAndReceiver returns a definition by name, optional module, and receiver.
func (s *DB) GetDefinitionByNameAndReceiver(name, modulePath, receiver string) (*Definition, error) {
	d := &Definition{}
	var query string
	var args []any
	if modulePath != "" {
		query = `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), COALESCE(b.body, ''), COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 JOIN modules m ON d.module_id = m.id
		 WHERE d.name = ? AND m.path LIKE ? AND COALESCE(d.receiver,'') = ?`
		args = []any{name, "%" + modulePath + "%", receiver}
	} else {
		query = `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), COALESCE(b.body, ''), COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 WHERE d.name = ? AND COALESCE(d.receiver,'') = ?
		 ORDER BY (SELECT COUNT(*) FROM refs r
		  JOIN definitions caller ON caller.id = r.from_def AND caller.test = FALSE
		  WHERE r.to_def = d.id) DESC LIMIT 1`
		args = []any{name, receiver}
	}
	err := s.queryRowContext(s.Ctx(), query, args...).Scan(
		&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test, &d.Receiver,
		&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// fuzzyReceiverLookup finds a method where the stored receiver ends with the given suffix.
// e.g. bareRecv="Router" matches stored "*DefaultRouter", prefix="*" adds the pointer.
func (s *DB) fuzzyReceiverLookup(name, modulePath, bareRecv, prefix string) (*Definition, error) {
	query := `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
	        COALESCE(d.signature,''), COALESCE(b.body, ''), COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
	 FROM definitions d
	 LEFT JOIN bodies b ON b.def_id = d.id
	 WHERE d.name = ? AND COALESCE(d.receiver,'') LIKE ?
	 ORDER BY (SELECT COUNT(*) FROM refs r
	   JOIN definitions caller ON caller.id = r.from_def AND caller.test = FALSE
	   WHERE r.to_def = d.id) DESC LIMIT 1`
	pattern := "%" + bareRecv
	if prefix != "" {
		pattern = prefix + "%" + bareRecv
	}
	d := &Definition{}
	err := s.queryRowContext(s.Ctx(), query, name, pattern).Scan(
		&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test, &d.Receiver,
		&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// Simulate creates a throwaway branch, applies mutations, queries impact, and discards.
// All operations happen on a single connection to maintain branch context.
func (s *DB) Simulate(mutations []Mutation) (*SimulationResult, error) {
	ctx := s.Ctx()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("get connection: %w", err)
	}
	defer conn.Close()

	branchName := fmt.Sprintf("sim_%x", time.Now().UnixNano())

	// Create and checkout simulation branch.
	if _, err := conn.ExecContext(ctx, "CALL DOLT_BRANCH(?)", branchName); err != nil {
		return nil, fmt.Errorf("create sim branch: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branchName); err != nil {
		return nil, fmt.Errorf("checkout sim branch: %w", err)
	}

	// Ensure we clean up: checkout main and delete branch.
	defer func() {
		conn.ExecContext(ctx, "CALL DOLT_CHECKOUT('main')")
		conn.ExecContext(ctx, "CALL DOLT_BRANCH('-D', ?)", branchName)
		s.CleanTempFiles()
	}()

	result := &SimulationResult{
		Steps: make([]SimulationStep, 0, len(mutations)),
	}
	allCallers := map[string]bool{}
	allTests := map[string]bool{}

	for _, m := range mutations {
		step := SimulationStep{Mutation: m}

		// Find the definition.
		d, err := s.GetDefinitionByName(m.Name, "")
		if err != nil {
			step.Error = fmt.Sprintf("definition %q not found", m.Name)
			result.Steps = append(result.Steps, step)
			continue
		}

		// Apply mutation on the sim branch.
		switch m.Type {
		case "signature-change":
			// Change hash + signature → callers break.
			_, err = conn.ExecContext(ctx,
				"UPDATE definitions SET hash = ?, signature = CONCAT(signature, ' (changed)') WHERE id = ?",
				fmt.Sprintf("sim_%x", time.Now().UnixNano()), d.ID)
		case "behavior-change":
			// Change hash only → tests may break.
			_, err = conn.ExecContext(ctx,
				"UPDATE definitions SET hash = ? WHERE id = ?",
				fmt.Sprintf("sim_%x", time.Now().UnixNano()), d.ID)
		case "removal":
			// Delete → all references break.
			err = s.DeleteDefinition(d.ID)
		case "addition", "one-shot-stub":
			// No mutation needed — just report what references exist.
		default:
			step.Error = fmt.Sprintf("unknown mutation type %q", m.Type)
			result.Steps = append(result.Steps, step)
			continue
		}
		if err != nil {
			step.Error = fmt.Sprintf("apply mutation: %v", err)
			result.Steps = append(result.Steps, step)
			continue
		}

		// Query impact for this definition.
		impact, err := s.GetImpact(d.ID)
		if err != nil && m.Type != "removal" {
			step.Error = fmt.Sprintf("get impact: %v", err)
			result.Steps = append(result.Steps, step)
			continue
		}

		if impact != nil {
			var prodCallers, testCallers int
			for _, c := range impact.DirectCallers {
				key := c.Receiver + "." + c.Name
				allCallers[key] = true
				if c.Test {
					testCallers++
				} else {
					prodCallers++
				}
			}
			for _, t := range impact.Tests {
				allTests[t.Name] = true
			}
			step.ProductionCallers = prodCallers
			step.TestCallers = testCallers
			step.TransitiveCallers = impact.TransitiveCount
			step.TestCoverage = len(impact.Tests)
			step.UncoveredCallers = impact.UncoveredBy
		} else if m.Type == "removal" {
			// For removal, we already collected callers before deleting.
			// Re-query from the original (callers still exist, just orphaned).
		}

		result.Steps = append(result.Steps, step)
	}

	// Compute totals.
	totalProd := 0
	for _, step := range result.Steps {
		totalProd += step.ProductionCallers
	}
	result.TotalMutations = len(mutations)
	result.CombinedCallers = len(allCallers)
	result.CombinedTests = len(allTests)
	if result.CombinedCallers > 0 {
		result.TestDensity = float64(result.CombinedTests) / float64(result.CombinedCallers)
	}

	return result, nil
}

type Mutation struct {
	Type     string `json:"type"` // signature-change, behavior-change, removal, addition, one-shot-stub
	Name     string `json:"name"`
	Receiver string `json:"receiver,omitempty"`
}

type SimulationStep struct {
	Mutation          Mutation `json:"mutation"`
	ProductionCallers int      `json:"production_callers"`
	TestCallers       int      `json:"test_callers"`
	TransitiveCallers int      `json:"transitive_callers"`
	TestCoverage      int      `json:"test_coverage"`
	UncoveredCallers  int      `json:"uncovered_callers"`
	Error             string   `json:"error,omitempty"`
}

type SimulationResult struct {
	Steps           []SimulationStep `json:"steps"`
	TotalMutations  int              `json:"total_mutations"`
	CombinedCallers int              `json:"combined_callers"`
	CombinedTests   int              `json:"combined_tests"`
	TestDensity     float64          `json:"test_density"`
}

// GetCallers returns all definitions that reference the given definition.
func (s *DB) GetCallers(defID int64) ([]Definition, error) {
	rows, err := s.queryContext(s.Ctx(),
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), COALESCE(b.body, ''), COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 JOIN refs r ON r.from_def = d.id
		 WHERE r.to_def = ?
		 ORDER BY d.name`, defID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitions(rows)
}

// GetCallees returns all definitions referenced by the given definition.
func (s *DB) GetCallees(defID int64) ([]Definition, error) {
	rows, err := s.queryContext(s.Ctx(),
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), COALESCE(b.body, ''), COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 JOIN refs r ON r.to_def = d.id
		 WHERE r.from_def = ?
		 ORDER BY d.name`, defID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitions(rows)
}

// GetModuleDefinitions returns all definitions in a module, including bodies.
func (s *DB) GetModuleDefinitions(moduleID int64) ([]Definition, error) {
	rows, err := s.queryContext(s.Ctx(),
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), COALESCE(b.body, ''), COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 WHERE d.module_id = ?
		 ORDER BY d.source_file, d.kind, d.name`, moduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitions(rows)
}

// --- References ---

// SetReferences replaces all references from a given definition.
func (s *DB) SetReferences(fromDef int64, refs []Reference) error {
	ctx := s.Ctx()

	// Build sorted set of new refs for comparison.
	type refKey struct {
		ToDef int64
		Kind  string
	}
	newSet := make([]refKey, 0, len(refs))
	seen := make(map[refKey]bool, len(refs))
	for _, r := range refs {
		k := refKey{r.ToDef, r.Kind}
		if !seen[k] {
			seen[k] = true
			newSet = append(newSet, k)
		}
	}
	sort.Slice(newSet, func(i, j int) bool {
		if newSet[i].ToDef != newSet[j].ToDef {
			return newSet[i].ToDef < newSet[j].ToDef
		}
		return newSet[i].Kind < newSet[j].Kind
	})

	// Read existing refs and compare.
	rows, err := s.queryContext(ctx,
		"SELECT to_def, kind FROM refs WHERE from_def = ? ORDER BY to_def, kind",
		fromDef)
	if err != nil {
		return fmt.Errorf("read refs: %w", err)
	}
	var oldSet []refKey
	for rows.Next() {
		var k refKey
		if err := rows.Scan(&k.ToDef, &k.Kind); err != nil {
			rows.Close()
			return fmt.Errorf("scan ref: %w", err)
		}
		oldSet = append(oldSet, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read refs: %w", err)
	}

	// Skip write if unchanged.
	if len(oldSet) == len(newSet) {
		match := true
		for i := range oldSet {
			if oldSet[i] != newSet[i] {
				match = false
				break
			}
		}
		if match {
			return nil
		}
	}

	if _, err := s.execContext(ctx, "DELETE FROM refs WHERE from_def = ?", fromDef); err != nil {
		return fmt.Errorf("clear refs: %w", err)
	}
	for _, r := range refs {
		if _, err := s.execContext(ctx,
			"INSERT IGNORE INTO refs (from_def, to_def, kind) VALUES (?, ?, ?)",
			fromDef, r.ToDef, r.Kind,
		); err != nil {
			return fmt.Errorf("insert ref: %w", err)
		}
	}
	return nil
}

// --- Imports ---

// SetImports replaces all imports for a module.
func (s *DB) SetImports(moduleID int64, imports []Import) error {
	ctx := s.Ctx()

	// Build sorted set of new imports for comparison.
	type impKey struct {
		Path  string
		Alias string
	}
	newSet := make([]impKey, 0, len(imports))
	seen := make(map[impKey]bool, len(imports))
	for _, imp := range imports {
		k := impKey{imp.ImportedPath, imp.Alias}
		if !seen[k] {
			seen[k] = true
			newSet = append(newSet, k)
		}
	}
	sort.Slice(newSet, func(i, j int) bool {
		if newSet[i].Path != newSet[j].Path {
			return newSet[i].Path < newSet[j].Path
		}
		return newSet[i].Alias < newSet[j].Alias
	})

	// Read existing imports and compare.
	rows, err := s.queryContext(ctx,
		"SELECT imported_path, COALESCE(alias, '') FROM imports WHERE module_id = ? ORDER BY imported_path, alias",
		moduleID)
	if err != nil {
		return fmt.Errorf("read imports: %w", err)
	}
	var oldSet []impKey
	for rows.Next() {
		var k impKey
		if err := rows.Scan(&k.Path, &k.Alias); err != nil {
			rows.Close()
			return fmt.Errorf("scan import: %w", err)
		}
		oldSet = append(oldSet, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read imports: %w", err)
	}

	// Skip write if unchanged.
	if len(oldSet) == len(newSet) {
		match := true
		for i := range oldSet {
			if oldSet[i] != newSet[i] {
				match = false
				break
			}
		}
		if match {
			return nil
		}
	}

	if _, err := s.execContext(ctx, "DELETE FROM imports WHERE module_id = ?", moduleID); err != nil {
		return fmt.Errorf("clear imports: %w", err)
	}
	for _, imp := range imports {
		if _, err := s.execContext(ctx,
			"INSERT IGNORE INTO imports (module_id, imported_path, alias) VALUES (?, ?, ?)",
			moduleID, imp.ImportedPath, imp.Alias,
		); err != nil {
			return fmt.Errorf("insert import: %w", err)
		}
	}
	return nil
}

// GetImports returns all imports for a module.
func (s *DB) GetImports(moduleID int64) ([]Import, error) {
	rows, err := s.queryContext(s.Ctx(),
		"SELECT module_id, imported_path, COALESCE(alias, '') FROM imports WHERE module_id = ? ORDER BY imported_path",
		moduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var imports []Import
	for rows.Next() {
		var imp Import
		if err := rows.Scan(&imp.ModuleID, &imp.ImportedPath, &imp.Alias); err != nil {
			return nil, err
		}
		imports = append(imports, imp)
	}
	return imports, rows.Err()
}

// --- Project Files ---

// SetProjectFile stores a project-level file (go.mod, go.sum, etc.).
func (s *DB) SetProjectFile(path, content string) error {
	_, err := s.execContext(s.Ctx(),
		`REPLACE INTO project_files (path, content) VALUES (?, ?)`, path, content)
	return err
}

// GetProjectFile retrieves a project-level file by path.
func (s *DB) GetProjectFile(path string) (string, error) {
	var content string
	err := s.queryRowContext(s.Ctx(),
		"SELECT content FROM project_files WHERE path = ?", path,
	).Scan(&content)
	return content, err
}

// ListProjectFiles returns all project file paths.
func (s *DB) ListProjectFiles() ([]string, error) {
	rows, err := s.queryContext(s.Ctx(), "SELECT path FROM project_files ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// --- Query ---

// Query executes a read-only SQL query and returns results as maps.
func (s *DB) Query(query string) ([]map[string]any, error) {
	normalized := strings.TrimSpace(strings.ToUpper(query))
	if !strings.HasPrefix(normalized, "SELECT") &&
		!strings.HasPrefix(normalized, "SHOW") &&
		!strings.HasPrefix(normalized, "DESCRIBE") &&
		!strings.HasPrefix(normalized, "EXPLAIN") &&
		!strings.HasPrefix(normalized, "WITH") {
		return nil, fmt.Errorf("only SELECT, SHOW, DESCRIBE, EXPLAIN, and WITH (CTE) queries are allowed")
	}
	// The Dolt driver rejects multi-statement queries at the protocol level,
	// so no additional semicolon checking is needed here.
	rows, err := s.queryContext(s.Ctx(), query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var results []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = vals[i]
		}
		results = append(results, row)
	}
	return results, rows.Err()
}

// --- Analysis ---

// ComputeRootHash computes a merkle-like root hash of all definitions.
func (s *DB) ComputeRootHash() (string, error) {
	rows, err := s.queryContext(s.Ctx(), "SELECT hash FROM definitions ORDER BY id")
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return "", err
		}
		hashes = append(hashes, h)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	sort.Strings(hashes)
	combined := sha256.Sum256([]byte(strings.Join(hashes, "")))
	return fmt.Sprintf("%x", combined), nil
}

// Impact computes the blast radius of a definition.
type Impact struct {
	Definition               Definition
	Module                   string
	DirectCallers            []Definition
	InterfaceDispatchCallers []Definition // callers via interface dispatch (subset of DirectCallers)
	TransitiveCount          int
	Tests                    []Definition
	UncoveredBy              int
}

// GetImpact computes the full impact analysis for a definition.
func (s *DB) GetImpact(defID int64) (*Impact, error) {
	d, err := s.GetDefinition(defID)
	if err != nil {
		return nil, err
	}

	var modulePath string
	if err := s.queryRowContext(s.Ctx(), "SELECT path FROM modules WHERE id = ?", d.ModuleID).Scan(&modulePath); err != nil {
		return nil, fmt.Errorf("get module path for def %d: %w", defID, err)
	}

	directCallers, err := s.GetCallers(defID)
	if err != nil {
		return nil, err
	}

	// If this is a method on a concrete type, also include callers of
	// any interface method it satisfies (interface dispatch).
	var ifaceDispatchCallers []Definition
	if d.Receiver != "" {
		ifaceDispatchCallers = s.getInterfaceDispatchCallers(d)
		for _, c := range ifaceDispatchCallers {
			found := false
			for _, existing := range directCallers {
				if existing.ID == c.ID {
					found = true
					break
				}
			}
			if !found {
				directCallers = append(directCallers, c)
			}
		}
	}

	// Transitive callers via BFS.
	visited := map[int64]bool{defID: true}
	queue := []int64{defID}
	var allCallers []Definition
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		callers, _ := s.GetCallers(current)
		for _, c := range callers {
			if !visited[c.ID] {
				visited[c.ID] = true
				queue = append(queue, c.ID)
				allCallers = append(allCallers, c)
			}
		}
	}

	var tests []Definition
	for _, c := range allCallers {
		if c.Test {
			tests = append(tests, c)
		}
	}

	uncovered := 0
	for _, dc := range directCallers {
		if dc.Test {
			continue
		}
		hasCoveringTest := false
		for _, t := range tests {
			if t.ID == dc.ID {
				hasCoveringTest = true
				break
			}
		}
		if !hasCoveringTest {
			dcCallers, _ := s.GetCallers(dc.ID)
			for _, dcc := range dcCallers {
				if dcc.Test {
					hasCoveringTest = true
					break
				}
			}
		}
		if !hasCoveringTest {
			uncovered++
		}
	}

	return &Impact{
		Definition:               *d,
		Module:                   modulePath,
		DirectCallers:            directCallers,
		InterfaceDispatchCallers: ifaceDispatchCallers,
		TransitiveCount:          len(allCallers),
		Tests:                    tests,
		UncoveredBy:              uncovered,
	}, nil
}

// GetUntested returns definitions that have no test in their direct callers.
// getInterfaceDispatchCallers finds callers of interface methods that this
// concrete method satisfies. E.g., if *responseWriter satisfies ResponseWriter,
// callers of ResponseWriter.WriteHeader are also callers of responseWriter.WriteHeader.
func (s *DB) getInterfaceDispatchCallers(d *Definition) []Definition {
	// Find the concrete type (strip * from receiver).
	concreteType := strings.TrimPrefix(d.Receiver, "*")

	// Find interfaces this type implements (via "implements" edges).
	rows, err := s.queryContext(s.Ctx(),
		`SELECT d2.name FROM definitions d1
		 JOIN refs r ON r.from_def = d1.id
		 JOIN definitions d2 ON d2.id = r.to_def
		 WHERE d1.name = ? AND r.kind = 'implements'`,
		concreteType)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var ifaces []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		ifaces = append(ifaces, name)
	}

	// For each interface, find the same-named method and get its callers.
	var allCallers []Definition
	for _, iface := range ifaces {
		// Interface methods don't exist as separate definitions in defn.
		// But callers reference the interface type. Find callers that
		// use this method name via the interface.
		ifaceDef, err := s.GetDefinitionByName(iface, "")
		if err != nil {
			continue
		}
		callers, err := s.GetCallers(ifaceDef.ID)
		if err != nil {
			continue
		}
		allCallers = append(allCallers, callers...)
	}
	return allCallers
}

func (s *DB) GetUntested() ([]Definition, error) {
	rows, err := s.queryContext(s.Ctx(), `
		SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		       COALESCE(d.signature,''), '', COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
		FROM definitions d
		WHERE d.test = FALSE AND d.exported = TRUE AND d.kind IN ('function', 'method')
		AND NOT EXISTS (
			SELECT 1 FROM refs r
			JOIN definitions t ON t.id = r.from_def AND t.test = TRUE
			WHERE r.to_def = d.id
		)
		ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitions(rows)
}

// --- Helpers ---

func scanDefinitions(rows *sql.Rows) ([]Definition, error) {
	var defs []Definition
	for rows.Next() {
		var d Definition
		if err := rows.Scan(
			&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test, &d.Receiver,
			&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash,
		); err != nil {
			return nil, err
		}
		defs = append(defs, d)
	}
	return defs, rows.Err()
}
