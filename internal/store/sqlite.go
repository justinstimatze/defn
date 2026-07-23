// SQLite backend implementation of store.Backend.
//
// Category A methods (branch, checkout, commit, merge, diff, log, remotes,
// conflicts) are Dolt-specific and are NOT part of the SQLite backend.
// They live only on *DB and will be removed in Phase 4 when Dolt is retired.
// autoCommit() in mcp/server.go becomes a no-op under SQLite because
// writes persist on tx commit — no working-set-to-branch step exists.
//
// modernc.org/sqlite returns plain strings for TEXT columns — no textCol
// wrapper needed (that's a Dolt-only concern; see textcol_audit_test.go).
//
// FTS5 SearchDefinitions uses a trigram tokenizer over bodies.body and
// definitions.doc. See schema_sqlite.sql for the rationale (camelCase +
// snake_case + dotted paths all indexed as substrings).

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

//go:embed schema_sqlite.sql
var sqliteSchemaSQL string

// ErrNotImplemented marks Backend methods not yet ported to SQLite.
var ErrNotImplemented = errors.New("sqlite: not yet implemented")

const (
	setLitFieldsBatchSize = 500
	setRefsBatchSize      = 1000
	upsertDefsBatchSize   = 500
)

// rowScanner is the common Scan surface of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// SQLiteDB is a store.Backend backed by a local SQLite database file.
// Writes hit disk on transaction commit (WAL mode). Safe for concurrent
// read; writers are serialized by SQLite itself (single-writer model).
type SQLiteDB struct {
	db   *sql.DB
	path string

	closeOnce sync.Once
	closeErr  error
}

// OpenSQLite opens (or creates) a SQLite-backed defn database at path.
// The path should be a filesystem file (e.g. ".defn/defn.db"). WAL +
// NORMAL synchronous mirror the Gate 3 prototype configuration.
func OpenSQLite(path string) (*SQLiteDB, error) {
	if path == "" {
		return nil, errors.New("sqlite: empty path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sqlite: prepare dir %q: %w", dir, err)
		}
	}

	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}

	// SQLite is single-writer; keep the pool modest to avoid busy contention.
	db.SetMaxOpenConns(4)

	if _, err := db.ExecContext(context.Background(), sqliteSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: apply schema: %w", err)
	}

	// Idempotent ALTER TABLE for existing DBs predating #160 (fresh DBs
	// already have these columns from the CREATE TABLE above). Must run
	// before any code touches the new columns.
	if err := migrateAddSummaryColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migrate: %w", err)
	}

	// Backfill FTS if this is an existing DB predating the FTS5 addition
	// (task #137). The CREATE VIRTUAL TABLE IF NOT EXISTS runs above but
	// doesn't populate — triggers only fire on future writes. If the
	// source tables have rows and the FTS table is empty, seed it.
	if err := backfillFTS(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: backfill fts: %w", err)
	}

	sq := &SQLiteDB{db: db, path: path}
	// Backfill def summaries if this is an existing DB predating #151.
	// Same pattern as backfillFTS: skip when already populated. Cost
	// on winze's 2378 defs is ~50-100ms one-shot; amortized on next
	// open it's a no-op.
	if err := sq.backfillDefSummaries(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: backfill def summaries: %w", err)
	}
	return sq, nil
}

// backfillDefSummaries computes MinHash signatures for any def missing
// from def_summaries. Task #151. Skips work when everything's present.
func (s *SQLiteDB) backfillDefSummaries() error {
	ctx := context.Background()
	var missing int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM bodies b
		LEFT JOIN def_summaries ds ON ds.def_id = b.def_id
		WHERE ds.def_id IS NULL`).Scan(&missing); err != nil {
		return fmt.Errorf("count missing summaries: %w", err)
	}
	if missing == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT b.def_id, b.body FROM bodies b
		LEFT JOIN def_summaries ds ON ds.def_id = b.def_id
		WHERE ds.def_id IS NULL`)
	if err != nil {
		return fmt.Errorf("select missing summaries: %w", err)
	}
	defer rows.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO def_summaries(def_id, minhash) VALUES (?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := stmt.ExecContext(ctx, id, ComputeMinHash(body)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func backfillFTS(db *sql.DB) error {
	ctx := context.Background()
	pairs := []struct {
		ftsTable, srcTable, srcRowid, srcCol, col string
	}{
		{"bodies_fts", "bodies", "def_id", "body", "body"},
		{"definitions_fts", "definitions", "id", "COALESCE(doc,'')", "doc"},
	}
	for _, p := range pairs {
		var ftsN, srcN int64
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", p.ftsTable)).Scan(&ftsN); err != nil {
			return fmt.Errorf("count %s: %w", p.ftsTable, err)
		}
		if ftsN > 0 {
			continue
		}
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", p.srcTable)).Scan(&srcN); err != nil {
			return fmt.Errorf("count %s: %w", p.srcTable, err)
		}
		if srcN == 0 {
			continue
		}
		sql := fmt.Sprintf("INSERT INTO %s(rowid, %s) SELECT %s, %s FROM %s",
			p.ftsTable, p.col, p.srcRowid, p.srcCol, p.srcTable)
		if _, err := db.ExecContext(ctx, sql); err != nil {
			return fmt.Errorf("backfill %s (%d rows): %w", p.ftsTable, srcN, err)
		}
	}
	return nil
}

func (s *SQLiteDB) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

func (s *SQLiteDB) Path() string { return s.path }

func (s *SQLiteDB) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *SQLiteDB) Ctx() context.Context { return context.Background() }

func (s *SQLiteDB) CleanTempFiles() {}

func (s *SQLiteDB) Begin() (commit func() error, rollback func(), err error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: begin: %w", err)
	}
	return tx.Commit, func() { _ = tx.Rollback() }, nil
}

// GC runs a WAL checkpoint to fold the -wal file back into the main db.
func (s *SQLiteDB) GC() error {
	_, err := s.db.ExecContext(context.Background(), "PRAGMA wal_checkpoint(PASSIVE)")
	if err != nil {
		return fmt.Errorf("sqlite: wal_checkpoint: %w", err)
	}
	return nil
}

// ComputeRootHash returns a hash of every definition's stored hash + name +
// kind + receiver. Used only for cross-backend equivalence tests; a stable
// hash of the graph state is enough — not intended to match Dolt's noms hash.
func (s *SQLiteDB) ComputeRootHash() (string, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		`SELECT COALESCE(name,''), COALESCE(kind,''), COALESCE(receiver,''), COALESCE(hash,'')
		 FROM definitions ORDER BY id`)
	if err != nil {
		return "", fmt.Errorf("sqlite: compute root hash: %w", err)
	}
	defer rows.Close()
	h := sha256.New()
	var name, kind, recv, hash string
	for rows.Next() {
		if err := rows.Scan(&name, &kind, &recv, &hash); err != nil {
			return "", fmt.Errorf("sqlite: scan for root hash: %w", err)
		}
		fmt.Fprintf(h, "%s|%s|%s|%s\n", name, kind, recv, hash)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *SQLiteDB) EnsureModule(path, name, doc string) (*Module, error) {
	if _, err := s.db.ExecContext(s.Ctx(),
		`INSERT INTO modules(path, name, doc) VALUES(?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET name=excluded.name, doc=excluded.doc`,
		path, name, doc,
	); err != nil {
		return nil, fmt.Errorf("sqlite: ensure module %q: %w", path, err)
	}
	return s.GetModuleByPath(path)
}

func (s *SQLiteDB) GetModuleByPath(path string) (*Module, error) {
	var m Module
	err := s.db.QueryRowContext(s.Ctx(),
		`SELECT id, path, name, COALESCE(doc, '') FROM modules WHERE path = ?`, path,
	).Scan(&m.ID, &m.Path, &m.Name, &m.Doc)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get module %q: %w", path, err)
	}
	return &m, nil
}

func (s *SQLiteDB) ListModules() ([]Module, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		`SELECT id, path, name, COALESCE(doc, '') FROM modules ORDER BY path`,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list modules: %w", err)
	}
	defer rows.Close()
	var mods []Module
	for rows.Next() {
		var m Module
		if err := rows.Scan(&m.ID, &m.Path, &m.Name, &m.Doc); err != nil {
			return nil, fmt.Errorf("sqlite: scan module: %w", err)
		}
		mods = append(mods, m)
	}
	return mods, rows.Err()
}

// sqliteFullDefSelect is the 14-column definition projection that
// scanSQLiteDef expects. Mirrors scanDefRow's column order on the Dolt side.
const sqliteFullDefSelect = `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
	        COALESCE(d.signature,''), COALESCE(b.body, ''), COALESCE(d.doc,''),
	        COALESCE(d.start_line,0), COALESCE(d.end_line,0),
	        COALESCE(d.source_file,''), d.hash`

func scanSQLiteDef(sc rowScanner, d *Definition) error {
	return sc.Scan(
		&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test, &d.Receiver,
		&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash,
	)
}

func scanSQLiteDefinitions(rows *sql.Rows) ([]Definition, error) {
	var defs []Definition
	for rows.Next() {
		var d Definition
		if err := scanSQLiteDef(rows, &d); err != nil {
			return nil, err
		}
		defs = append(defs, d)
	}
	return defs, rows.Err()
}

func (s *SQLiteDB) GetModuleDefinitions(moduleID int64) ([]Definition, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		sqliteFullDefSelect+`
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 WHERE d.module_id = ?
		 ORDER BY d.source_file, d.kind, d.name`, moduleID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: get module definitions: %w", err)
	}
	defer rows.Close()
	return scanSQLiteDefinitions(rows)
}

func (s *SQLiteDB) GetDefinition(id int64) (*Definition, error) {
	d := &Definition{}
	row := s.db.QueryRowContext(s.Ctx(),
		sqliteFullDefSelect+`
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 WHERE d.id = ?`, id)
	if err := scanSQLiteDef(row, d); err != nil {
		return nil, err
	}
	return d, nil
}

// GetDefinitionByName mirrors *DB.GetDefinitionByName: file:line syntax,
// receiver.method parsing, module-fuzzy match, blast-radius tiebreak.
func (s *SQLiteDB) GetDefinitionByName(name, modulePath string) (*Definition, error) {
	if strings.Contains(name, ".") && !strings.Contains(name, "/") {
		dotIdx := strings.LastIndex(name, ".")
		recv := strings.TrimSpace(name[:dotIdx])
		methName := strings.TrimSpace(name[dotIdx+1:])
		recv = strings.TrimPrefix(recv, "(")
		recv = strings.TrimSuffix(recv, ")")
		if methName != "" && recv != "" {
			if d, err := s.GetDefinitionByNameAndReceiver(methName, modulePath, recv); err == nil {
				return d, nil
			}
			if strings.HasPrefix(recv, "*") {
				if d, err := s.GetDefinitionByNameAndReceiver(methName, modulePath, recv[1:]); err == nil {
					return d, nil
				}
			} else {
				if d, err := s.GetDefinitionByNameAndReceiver(methName, modulePath, "*"+recv); err == nil {
					return d, nil
				}
			}
			bareRecv := strings.TrimPrefix(recv, "*")
			prefix := ""
			if strings.HasPrefix(recv, "*") {
				prefix = "*"
			}
			if d, err := s.fuzzyReceiverLookup(methName, modulePath, bareRecv, prefix); err == nil {
				return d, nil
			}
		}
	}

	if parts := strings.SplitN(name, ":", 2); len(parts) == 2 {
		if line, err := strconv.Atoi(parts[1]); err == nil && line > 0 {
			filePath := parts[0]
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
			return s.GetDefinition(defs[0].ID)
		}
	}

	baseQuery := sqliteFullDefSelect + `
	          FROM definitions d
	          LEFT JOIN bodies b ON b.def_id = d.id`

	if modulePath != "" {
		query := baseQuery + " JOIN modules m ON d.module_id = m.id WHERE d.name = ? AND m.path = ?"
		d := &Definition{}
		if err := scanSQLiteDef(s.db.QueryRowContext(s.Ctx(), query, name, modulePath), d); err == nil {
			return d, nil
		}
		query = baseQuery + " JOIN modules m ON d.module_id = m.id WHERE d.name = ? AND m.path LIKE ?" +
			` ORDER BY (SELECT COUNT(*) FROM refs r WHERE r.to_def = d.id) DESC LIMIT 1`
		d = &Definition{}
		if err := scanSQLiteDef(s.db.QueryRowContext(s.Ctx(), query, name, "%"+modulePath+"%"), d); err == nil {
			return d, nil
		}
	}

	query := baseQuery + " WHERE d.name = ?" +
		` ORDER BY (SELECT COUNT(*) FROM refs r
		  JOIN definitions caller ON caller.id = r.from_def AND caller.test = 0
		  WHERE r.to_def = d.id) DESC LIMIT 1`
	d := &Definition{}
	if err := scanSQLiteDef(s.db.QueryRowContext(s.Ctx(), query, name), d); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *SQLiteDB) GetDefinitionByNameAndReceiver(name, modulePath, receiver string) (*Definition, error) {
	d := &Definition{}
	var query string
	var args []any
	if modulePath != "" {
		query = sqliteFullDefSelect + `
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 JOIN modules m ON d.module_id = m.id
		 WHERE d.name = ? AND m.path LIKE ? AND COALESCE(d.receiver,'') = ?`
		args = []any{name, "%" + modulePath + "%", receiver}
	} else {
		query = sqliteFullDefSelect + `
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 WHERE d.name = ? AND COALESCE(d.receiver,'') = ?
		 ORDER BY (SELECT COUNT(*) FROM refs r
		  JOIN definitions caller ON caller.id = r.from_def AND caller.test = 0
		  WHERE r.to_def = d.id) DESC LIMIT 1`
		args = []any{name, receiver}
	}
	if err := scanSQLiteDef(s.db.QueryRowContext(s.Ctx(), query, args...), d); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *SQLiteDB) fuzzyReceiverLookup(name, modulePath, bareRecv, prefix string) (*Definition, error) {
	query := sqliteFullDefSelect + `
	 FROM definitions d
	 LEFT JOIN bodies b ON b.def_id = d.id
	 WHERE d.name = ? AND COALESCE(d.receiver,'') LIKE ?
	 ORDER BY (SELECT COUNT(*) FROM refs r
	   JOIN definitions caller ON caller.id = r.from_def AND caller.test = 0
	   WHERE r.to_def = d.id) DESC LIMIT 1`
	pattern := "%" + bareRecv
	if prefix != "" {
		pattern = prefix + "%" + bareRecv
	}
	d := &Definition{}
	if err := scanSQLiteDef(s.db.QueryRowContext(s.Ctx(), query, name, pattern), d); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *SQLiteDB) FilterDefinitions(name, kind, file string, limit int) ([]Definition, error) {
	q := `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
	        COALESCE(d.signature,''), '', COALESCE(d.doc,''),
	        COALESCE(d.start_line,0), COALESCE(d.end_line,0),
	        COALESCE(d.source_file,''), d.hash
	 FROM definitions d WHERE 1=1`
	var args []any
	if name != "" {
		q += " AND d.name LIKE ?"
		args = append(args, name)
	}
	if kind != "" {
		q += " AND d.kind = ?"
		args = append(args, kind)
	}
	if file != "" {
		q += " AND d.source_file LIKE ?"
		args = append(args, file)
	}
	q += " ORDER BY d.name"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(s.Ctx(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSQLiteDefinitions(rows)
}

func (s *SQLiteDB) FindDefinitions(namePattern string) ([]Definition, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), '', COALESCE(d.doc,''),
		        COALESCE(d.start_line,0), COALESCE(d.end_line,0),
		        COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 WHERE d.name LIKE ? OR COALESCE(d.signature,'') LIKE ?
		 ORDER BY d.name`, namePattern, namePattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSQLiteDefinitions(rows)
}

func (s *SQLiteDB) FindDefinitionsByFile(fileSuffix string, sourceFile string, line int) ([]Definition, error) {
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

	rows, err := s.db.QueryContext(s.Ctx(), query, args...)
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

// CountDefinitions returns the total number of non-test definitions.
// Mirrors *DB.CountDefinitions (used by the ranker to size IDF builds).
func (s *SQLiteDB) CountDefinitions() (int, error) {
	var n int
	if err := s.db.QueryRowContext(s.Ctx(),
		`SELECT COUNT(*) FROM definitions WHERE test = 0`).Scan(&n); err != nil {
		return 0, fmt.Errorf("sqlite: count definitions: %w", err)
	}
	return n, nil
}

// SearchDefinitions runs a trigram FTS5 MATCH over both bodies_fts.body
// and definitions_fts.doc, unioned by definition id and ranked by bm25.
// Trigram tokenization makes `handleEdit`, `handle_edit`, `pkg.Method`,
// and `authentication` all substring-searchable — including winze's
// underscore case that the LIKE-based Phase 1 impl broke.
func (s *SQLiteDB) SearchDefinitions(query string) ([]Definition, error) {
	if query == "" {
		return nil, nil
	}
	// FTS5 MATCH treats certain characters (space, ", parentheses, ':')
	// as query syntax. For a substring-of-identifier search we want the
	// raw needle to be interpreted literally; wrap in double quotes and
	// escape embedded ones. Trigram requires the phrase to be ≥3 chars;
	// shorter needles fall back to LIKE.
	needle := strings.TrimSpace(query)
	if len(needle) < 3 {
		return s.searchDefinitionsLike(query)
	}
	phrase := `"` + strings.ReplaceAll(needle, `"`, `""`) + `"`
	rows, err := s.db.QueryContext(s.Ctx(), `
		WITH matched AS (
		  SELECT rowid AS def_id, MIN(rank) AS rank FROM (
		    SELECT rowid, bm25(bodies_fts) AS rank FROM bodies_fts WHERE bodies_fts MATCH ?
		    UNION ALL
		    SELECT rowid, bm25(definitions_fts) AS rank FROM definitions_fts WHERE definitions_fts MATCH ?
		  )
		  GROUP BY rowid
		)
		SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		       COALESCE(d.signature,''), '', COALESCE(d.doc,''),
		       COALESCE(d.start_line,0), COALESCE(d.end_line,0),
		       COALESCE(d.source_file,''), d.hash
		FROM matched m
		JOIN definitions d ON d.id = m.def_id
		ORDER BY m.rank ASC
		LIMIT 100`, phrase, phrase)
	if err != nil {
		// FTS MATCH can error on rare query shapes even after quoting
		// (odd Unicode, punctuation-only). Fall back to LIKE rather
		// than surface a scary error to the caller.
		return s.searchDefinitionsLike(query)
	}
	defer rows.Close()
	return scanSQLiteDefinitions(rows)
}

// searchDefinitionsLike is the pre-FTS fallback, kept for <3-char needles
// (trigram tokenizer requires ≥3 chars) and rare FTS MATCH errors.
func (s *SQLiteDB) searchDefinitionsLike(query string) ([]Definition, error) {
	like := "%" + query + "%"
	rows, err := s.db.QueryContext(s.Ctx(),
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), '', COALESCE(d.doc,''),
		        COALESCE(d.start_line,0), COALESCE(d.end_line,0),
		        COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 WHERE COALESCE(d.doc,'') LIKE ? OR COALESCE(b.body,'') LIKE ?
		 ORDER BY d.name
		 LIMIT 100`, like, like)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSQLiteDefinitions(rows)
}

func (s *SQLiteDB) SearchBodiesLike(pattern string, limit int) ([]BodyMatch, error) {
	if pattern == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(pattern)
	like := "%" + esc + "%"
	rows, err := s.db.QueryContext(s.Ctx(), `
		SELECT d.name, d.kind, COALESCE(d.receiver, ''),
		       COALESCE(d.source_file, ''), COALESCE(d.start_line, 0),
		       b.body
		FROM bodies b
		JOIN definitions d ON d.id = b.def_id
		WHERE LOWER(b.body) LIKE LOWER(?) ESCAPE '\'
		LIMIT ?`, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BodyMatch
	needle := strings.ToLower(pattern)
	for rows.Next() {
		var m BodyMatch
		var body string
		if err := rows.Scan(&m.Name, &m.Kind, &m.Receiver, &m.SourceFile, &m.Line, &body); err != nil {
			return nil, err
		}
		idx := strings.Index(strings.ToLower(body), needle)
		if idx < 0 {
			continue
		}
		lineOffset := strings.Count(body[:idx], "\n")
		m.Line += lineOffset
		start := idx - 30
		if start < 0 {
			start = 0
		}
		end := idx + len(pattern) + 30
		if end > len(body) {
			end = len(body)
		}
		snip := body[start:end]
		snip = strings.ReplaceAll(snip, "\n", " ")
		if start > 0 {
			snip = "…" + snip
		}
		if end < len(body) {
			snip = snip + "…"
		}
		m.Snippet = snip
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *SQLiteDB) SampleBodies(n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(s.Ctx(),
		`SELECT b.body
		 FROM definitions d
		 JOIN bodies b ON b.def_id = d.id
		 WHERE d.test = 0
		 ORDER BY d.hash
		 LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, n)
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *SQLiteDB) GetBodiesByDefIDs(ids []int64) (map[int64]string, error) {
	if len(ids) == 0 {
		return map[int64]string{}, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	q := fmt.Sprintf("SELECT def_id, body FROM bodies WHERE def_id IN (%s)", placeholders)
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(s.Ctx(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		out[id] = body
	}
	return out, rows.Err()
}

func (s *SQLiteDB) GetUntested() ([]Definition, error) {
	rows, err := s.db.QueryContext(s.Ctx(), `
		SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		       COALESCE(d.signature,''), '', COALESCE(d.doc,''),
		       COALESCE(d.start_line,0), COALESCE(d.end_line,0),
		       COALESCE(d.source_file,''), d.hash
		FROM definitions d
		WHERE d.test = 0 AND d.exported = 1 AND d.kind IN ('function', 'method')
		AND NOT EXISTS (
			SELECT 1 FROM refs r
			JOIN definitions t ON t.id = r.from_def AND t.test = 1
			WHERE r.to_def = d.id
		)
		ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSQLiteDefinitions(rows)
}

func (s *SQLiteDB) UpsertDefinition(d *Definition) (int64, error) {
	d.Hash = HashBody(d.Body)
	ctx := s.Ctx()

	var existingID int64
	var existingHash string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, hash FROM definitions
		 WHERE module_id = ? AND name = ? AND kind = ? AND COALESCE(receiver,'') = COALESCE(?,'') AND test = ?`,
		d.ModuleID, d.Name, d.Kind, d.Receiver, d.Test,
	).Scan(&existingID, &existingHash)

	if err == sql.ErrNoRows {
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO definitions
			 (module_id, name, kind, exported, test, receiver, signature, doc, start_line, end_line, source_file, hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ModuleID, d.Name, d.Kind, d.Exported, d.Test, d.Receiver,
			d.Signature, d.Doc, d.StartLine, d.EndLine, d.SourceFile, d.Hash,
		)
		if err != nil {
			return 0, fmt.Errorf("sqlite: insert definition: %w", err)
		}
		id, _ := res.LastInsertId()
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO bodies (def_id, body) VALUES (?, ?)`, id, d.Body,
		); err != nil {
			return 0, fmt.Errorf("sqlite: insert body: %w", err)
		}
		// #151: precompute minhash. Best-effort — error here shouldn't
		// fail the ingest.
		_ = s.SetDefSummaryMinHash(id, ComputeMinHash(d.Body))
		return id, nil
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: query definition: %w", err)
	}

	if existingHash == d.Hash {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE definitions SET start_line=?, end_line=?, source_file=?
			 WHERE id=? AND (start_line != ? OR end_line != ? OR source_file != ?)`,
			d.StartLine, d.EndLine, d.SourceFile,
			existingID, d.StartLine, d.EndLine, d.SourceFile,
		); err != nil {
			return 0, fmt.Errorf("sqlite: update location: %w", err)
		}
		return existingID, nil
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE definitions
		 SET exported=?, signature=?, doc=?, start_line=?, end_line=?, source_file=?, hash=?
		 WHERE id=?`,
		d.Exported, d.Signature, d.Doc,
		d.StartLine, d.EndLine, d.SourceFile, d.Hash, existingID,
	); err != nil {
		return 0, fmt.Errorf("sqlite: update definition: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE bodies SET body = ? WHERE def_id = ?`, d.Body, existingID,
	); err != nil {
		return 0, fmt.Errorf("sqlite: update body: %w", err)
	}
	// #151: body changed → recompute minhash. Best-effort.
	_ = s.SetDefSummaryMinHash(existingID, ComputeMinHash(d.Body))
	return existingID, nil
}

// UpsertDefinitionsBulk batches N upserts. Same shape as *DB, but SQLite's
// AUTO_INCREMENT semantics differ: multi-row INSERT assigns consecutive
// rowids starting from `last_insert_rowid()`. We use one INSERT per batch
// and derive IDs from LastInsertId + offset (same as the Dolt path).
func (s *SQLiteDB) UpsertDefinitionsBulk(defs []*Definition) ([]int64, error) {
	if len(defs) == 0 {
		return nil, nil
	}
	ctx := s.Ctx()
	ids := make([]int64, len(defs))

	for _, d := range defs {
		d.Hash = HashBody(d.Body)
	}

	type natKey struct {
		modID    int64
		name     string
		kind     string
		receiver string
		test     bool
	}
	keyOf := func(d *Definition) natKey {
		return natKey{d.ModuleID, d.Name, d.Kind, d.Receiver, d.Test}
	}
	type existing struct {
		id   int64
		hash string
	}
	existingByKey := make(map[natKey]existing, len(defs))
	modIDs := make(map[int64]bool)
	for _, d := range defs {
		modIDs[d.ModuleID] = true
	}
	for modID := range modIDs {
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, name, kind, COALESCE(receiver,''), test, hash
			 FROM definitions WHERE module_id = ?`, modID)
		if err != nil {
			return nil, fmt.Errorf("sqlite: UpsertDefinitionsBulk lookup module %d: %w", modID, err)
		}
		for rows.Next() {
			var e existing
			var name, kind, receiver, hash string
			var test bool
			if err := rows.Scan(&e.id, &name, &kind, &receiver, &test, &hash); err != nil {
				rows.Close()
				return nil, fmt.Errorf("sqlite: UpsertDefinitionsBulk scan: %w", err)
			}
			e.hash = hash
			existingByKey[natKey{modID, name, kind, receiver, test}] = e
		}
		rows.Close()
	}

	var toInsert []*Definition
	var toInsertPos []int
	// pendingByKey guards against a caller passing two Definitions with the
	// same natural key in one batch. That happens when the ingest layer
	// enqueues defs from a package variant that shares files with another
	// variant (packages.Load Tests:true can produce overlapping pkg.Syntax
	// under some layouts — FilterPackages catches the common case but not
	// every one). Without this guard the batch INSERT hits the unique
	// constraint on (module_id, name, kind, receiver, test) and the whole
	// flush fails. Last-write-wins semantics: the later Definition value
	// replaces the earlier one in the INSERT, and both input positions
	// receive the same row ID after the insert.
	pendingByKey := make(map[natKey]int) // key → index into toInsert
	type dupPos struct{ inputPos, canonicalToInsertIdx int }
	var dupes []dupPos
	for i, d := range defs {
		if e, ok := existingByKey[keyOf(d)]; ok {
			ids[i] = e.id
			if e.hash == d.Hash {
				if _, err := s.db.ExecContext(ctx,
					`UPDATE definitions SET start_line=?, end_line=?, source_file=?
					 WHERE id=? AND (start_line != ? OR end_line != ? OR source_file != ?)`,
					d.StartLine, d.EndLine, d.SourceFile,
					e.id, d.StartLine, d.EndLine, d.SourceFile,
				); err != nil {
					return nil, fmt.Errorf("sqlite: UpsertDefinitionsBulk location update id=%d: %w", e.id, err)
				}
				continue
			}
			if _, err := s.UpsertDefinition(d); err != nil {
				return nil, err
			}
			continue
		}
		if canonical, ok := pendingByKey[keyOf(d)]; ok {
			// Later occurrence supersedes: overwrite the canonical slot with
			// this Definition, remember the earlier input position needs the
			// canonical row ID copied over.
			dupes = append(dupes, dupPos{inputPos: i, canonicalToInsertIdx: canonical})
			toInsert[canonical] = d
			continue
		}
		pendingByKey[keyOf(d)] = len(toInsert)
		toInsert = append(toInsert, d)
		toInsertPos = append(toInsertPos, i)
	}
	if len(toInsert) == 0 {
		return ids, nil
	}

	for start := 0; start < len(toInsert); start += upsertDefsBatchSize {
		end := start + upsertDefsBatchSize
		if end > len(toInsert) {
			end = len(toInsert)
		}
		chunk := toInsert[start:end]
		placeholders := make([]string, len(chunk))
		defArgs := make([]any, 0, 12*len(chunk))
		for i, d := range chunk {
			placeholders[i] = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
			defArgs = append(defArgs,
				d.ModuleID, d.Name, d.Kind, d.Exported, d.Test, d.Receiver,
				d.Signature, d.Doc, d.StartLine, d.EndLine, d.SourceFile, d.Hash)
		}
		q := `INSERT INTO definitions
		      (module_id, name, kind, exported, test, receiver, signature, doc,
		       start_line, end_line, source_file, hash) VALUES ` +
			strings.Join(placeholders, ",")
		res, err := s.db.ExecContext(ctx, q, defArgs...)
		if err != nil {
			return nil, fmt.Errorf("sqlite: UpsertDefinitionsBulk insert defs (batch %d..%d): %w",
				start, end, err)
		}
		lastID, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("sqlite: UpsertDefinitionsBulk LastInsertId: %w", err)
		}
		// SQLite's last_insert_rowid() returns the LAST rowid of a multi-row
		// INSERT. Rowids are consecutive within a single INSERT statement
		// (autoincrement guarantee), so firstID = lastID - N + 1.
		firstID := lastID - int64(len(chunk)) + 1
		for i := range chunk {
			ids[toInsertPos[start+i]] = firstID + int64(i)
		}

		bodyPlaceholders := make([]string, len(chunk))
		bodyArgs := make([]any, 0, 2*len(chunk))
		for i, d := range chunk {
			bodyPlaceholders[i] = "(?, ?)"
			bodyArgs = append(bodyArgs, firstID+int64(i), d.Body)
		}
		bq := "INSERT INTO bodies (def_id, body) VALUES " + strings.Join(bodyPlaceholders, ",")
		if _, err := s.db.ExecContext(ctx, bq, bodyArgs...); err != nil {
			return nil, fmt.Errorf("sqlite: UpsertDefinitionsBulk insert bodies (batch %d..%d): %w",
				start, end, err)
		}

		// #151: precompute minhashes for the newly-inserted defs. One
		// multi-row INSERT per chunk keeps the per-def overhead low —
		// hashing dominates, but that's ~microseconds per def for
		// typical Go bodies. Best-effort; a failure here shouldn't
		// abort the ingest.
		mhPlaceholders := make([]string, len(chunk))
		mhArgs := make([]any, 0, 2*len(chunk))
		for i, d := range chunk {
			mhPlaceholders[i] = "(?, ?)"
			mhArgs = append(mhArgs, firstID+int64(i), ComputeMinHash(d.Body))
		}
		mhq := "INSERT OR REPLACE INTO def_summaries(def_id, minhash) VALUES " + strings.Join(mhPlaceholders, ",")
		_, _ = s.db.ExecContext(ctx, mhq, mhArgs...)
	}
	// Backfill row IDs for input positions whose natural key was a
	// duplicate of another in the same batch (see pendingByKey above).
	// The canonical position received the freshly-assigned id during the
	// batch loop; duplicates copy from there.
	for _, dp := range dupes {
		ids[dp.inputPos] = ids[toInsertPos[dp.canonicalToInsertIdx]]
	}
	return ids, nil
}

func (s *SQLiteDB) DeleteDefinition(id int64) error {
	ctx := s.Ctx()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM refs WHERE from_def = ? OR to_def = ?", id, id); err != nil {
		return fmt.Errorf("sqlite: delete references for def %d: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM bodies WHERE def_id = ?", id); err != nil {
		return fmt.Errorf("sqlite: delete body for def %d: %w", id, err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM definitions WHERE id = ?", id); err != nil {
		return fmt.Errorf("sqlite: delete definition %d: %w", id, err)
	}
	return nil
}

func (s *SQLiteDB) RenameDefinition(id int64, newName, newBody, newSignature string, exported bool) error {
	hash := HashBody(newBody)
	ctx := s.Ctx()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE definitions
		 SET name = ?, signature = ?, exported = ?, hash = ?
		 WHERE id = ?`,
		newName, newSignature, exported, hash, id,
	); err != nil {
		return fmt.Errorf("sqlite: rename definition: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE bodies SET body = ? WHERE def_id = ?`, newBody, id,
	); err != nil {
		return fmt.Errorf("sqlite: rename body: %w", err)
	}
	return nil
}

func (s *SQLiteDB) PruneStaleDefinitions(liveIDs map[int64]bool) (int, error) {
	if len(liveIDs) == 0 {
		return 0, nil
	}
	rows, err := s.db.QueryContext(s.Ctx(), "SELECT id FROM definitions")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var staleIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("sqlite: scan definition id: %w", err)
		}
		if !liveIDs[id] {
			staleIDs = append(staleIDs, id)
		}
	}
	for _, id := range staleIDs {
		if err := s.DeleteDefinition(id); err != nil {
			return 0, fmt.Errorf("sqlite: prune def %d: %w", id, err)
		}
	}
	return len(staleIDs), nil
}

func (s *SQLiteDB) QueryRefs(fromName, toName, kind string, limit int) ([]Reference, error) {
	q := `SELECT r.from_def, r.to_def, r.kind
	      FROM refs r
	      JOIN definitions df ON r.from_def = df.id
	      JOIN definitions dt ON r.to_def = dt.id
	      WHERE 1=1`
	var args []any
	if fromName != "" {
		q += " AND df.name LIKE ?"
		args = append(args, fromName)
	}
	if toName != "" {
		q += " AND dt.name LIKE ?"
		args = append(args, toName)
	}
	if kind != "" {
		q += " AND r.kind = ?"
		args = append(args, kind)
	}
	q += " ORDER BY df.name, dt.name"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(s.Ctx(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []Reference
	for rows.Next() {
		var r Reference
		if err := rows.Scan(&r.FromDef, &r.ToDef, &r.Kind); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

func (s *SQLiteDB) SetReferences(fromDef int64, refs []Reference) error {
	ctx := s.Ctx()

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

	rows, err := s.db.QueryContext(ctx,
		"SELECT to_def, kind FROM refs WHERE from_def = ? ORDER BY to_def, kind", fromDef)
	if err != nil {
		return fmt.Errorf("sqlite: read refs: %w", err)
	}
	var oldSet []refKey
	for rows.Next() {
		var k refKey
		if err := rows.Scan(&k.ToDef, &k.Kind); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: scan ref: %w", err)
		}
		oldSet = append(oldSet, k)
	}
	rows.Close()

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

	if _, err := s.db.ExecContext(ctx, "DELETE FROM refs WHERE from_def = ?", fromDef); err != nil {
		return fmt.Errorf("sqlite: clear refs: %w", err)
	}
	if len(newSet) == 0 {
		return nil
	}
	for start := 0; start < len(newSet); start += setRefsBatchSize {
		end := start + setRefsBatchSize
		if end > len(newSet) {
			end = len(newSet)
		}
		chunk := newSet[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 3*len(chunk))
		for i, r := range chunk {
			placeholders[i] = "(?, ?, ?)"
			args = append(args, fromDef, r.ToDef, r.Kind)
		}
		q := "INSERT OR IGNORE INTO refs (from_def, to_def, kind) VALUES " +
			strings.Join(placeholders, ", ")
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("sqlite: insert refs (batch %d..%d): %w", start, end, err)
		}
	}
	return nil
}

func (s *SQLiteDB) SetManyReferences(refsByDef map[int64][]Reference) error {
	if len(refsByDef) == 0 {
		return nil
	}
	ctx := s.Ctx()

	defIDs := make([]int64, 0, len(refsByDef))
	for id := range refsByDef {
		defIDs = append(defIDs, id)
	}
	for start := 0; start < len(defIDs); start += 500 {
		end := start + 500
		if end > len(defIDs) {
			end = len(defIDs)
		}
		chunk := defIDs[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		q := "DELETE FROM refs WHERE from_def IN (" + placeholders + ")"
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("sqlite: SetManyReferences delete: %w", err)
		}
	}

	type rk struct {
		from int64
		to   int64
		kind string
	}
	seen := make(map[rk]bool)
	var rows []rk
	for fromID, refs := range refsByDef {
		for _, r := range refs {
			k := rk{fromID, r.ToDef, r.Kind}
			if seen[k] {
				continue
			}
			seen[k] = true
			rows = append(rows, k)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	for start := 0; start < len(rows); start += setRefsBatchSize {
		end := start + setRefsBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 3*len(chunk))
		for i, r := range chunk {
			placeholders[i] = "(?, ?, ?)"
			args = append(args, r.from, r.to, r.kind)
		}
		q := "INSERT OR IGNORE INTO refs (from_def, to_def, kind) VALUES " +
			strings.Join(placeholders, ", ")
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("sqlite: SetManyReferences insert: %w", err)
		}
	}
	return nil
}

func (s *SQLiteDB) GetCallers(defID int64) ([]Definition, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		sqliteFullDefSelect+`
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 JOIN refs r ON r.from_def = d.id
		 WHERE r.to_def = ?
		 ORDER BY d.name`, defID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSQLiteDefinitions(rows)
}

func (s *SQLiteDB) GetCallees(defID int64) ([]Definition, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		sqliteFullDefSelect+`
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 JOIN refs r ON r.to_def = d.id
		 WHERE r.from_def = ?
		 ORDER BY d.name`, defID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSQLiteDefinitions(rows)
}

func (s *SQLiteDB) getCallersOfKind(defID int64, kind string) ([]Definition, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		sqliteFullDefSelect+`
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 JOIN refs r ON r.from_def = d.id
		 WHERE r.to_def = ? AND r.kind = ?
		 ORDER BY d.name`, defID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSQLiteDefinitions(rows)
}

func (s *SQLiteDB) GetImpact(defID int64) (*Impact, error) {
	d, err := s.GetDefinition(defID)
	if err != nil {
		return nil, err
	}
	var modulePath string
	if err := s.db.QueryRowContext(s.Ctx(),
		"SELECT path FROM modules WHERE id = ?", d.ModuleID).Scan(&modulePath); err != nil {
		return nil, fmt.Errorf("sqlite: get module path for def %d: %w", defID, err)
	}

	directCallers, err := s.GetCallers(defID)
	if err != nil {
		return nil, err
	}
	ifaceDispatchCallers, err := s.getCallersOfKind(defID, "interface_dispatch")
	if err != nil {
		return nil, err
	}

	// #149: transitive callers via one recursive-CTE round-trip. Was
	// a Go-side BFS with N GetCallers queries (one per node). SQLite
	// 3.30+ CTE does the whole traversal in a single query; UNION
	// dedupes so cycles are naturally handled. On defn-self this
	// takes GetImpact from ~10-30 SQL round-trips to 2 (direct
	// callers + this CTE). On winze it should be more dramatic.
	// Excludes the target itself.
	allCallers, err := s.transitiveCallers(defID)
	if err != nil {
		return nil, err
	}

	var tests []Definition
	for _, c := range allCallers {
		if c.Test {
			tests = append(tests, c)
		}
	}

	// Uncovered = direct non-test callers with no reachable test in
	// the transitive closure. #149: check membership against a set
	// of caller-IDs-with-test-in-their-closure computed via a second
	// CTE, rather than per-direct-caller GetCallers scans.
	coveredByTest := s.coveredCallerSet(directCallers, tests)
	uncovered := 0
	for _, dc := range directCallers {
		if dc.Test {
			continue
		}
		if !coveredByTest[dc.ID] {
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

// transitiveCallers walks the refs graph backwards from `defID` and
// returns every caller in the transitive closure (excluding defID
// itself). One SQL round-trip via a recursive CTE. #149.
func (s *SQLiteDB) transitiveCallers(defID int64) ([]Definition, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		`WITH RECURSIVE reachable(id) AS (
		    SELECT DISTINCT r.from_def FROM refs r WHERE r.to_def = ?
		    UNION
		    SELECT DISTINCT r.from_def FROM refs r
		    JOIN reachable ON r.to_def = reachable.id
		 )
		 SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), '', COALESCE(d.doc,''),
		        COALESCE(d.start_line,0), COALESCE(d.end_line,0),
		        COALESCE(d.source_file,''), d.hash
		 FROM definitions d
		 JOIN reachable ON d.id = reachable.id
		 ORDER BY d.name`, defID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: transitive callers of %d: %w", defID, err)
	}
	defer rows.Close()
	return scanSQLiteDefinitions(rows)
}

// coveredCallerSet returns the set of direct-caller IDs that have a
// test in their own transitive closure. Used by GetImpact's uncovered
// counting. #149: one CTE finds ALL defs that reach any test node;
// intersect with direct-callers in Go. Replaces N per-caller
// GetCallers scans with one bulk query.
func (s *SQLiteDB) coveredCallerSet(directCallers, tests []Definition) map[int64]bool {
	covered := make(map[int64]bool)
	if len(tests) == 0 || len(directCallers) == 0 {
		return covered
	}
	testIDByID := make(map[int64]bool, len(tests))
	for _, t := range tests {
		testIDByID[t.ID] = true
		covered[t.ID] = true // a test def "covers itself"
	}
	// For each direct non-test caller, check if any of its own
	// transitive callers are tests. Fold into one bulk CTE keyed
	// by the direct-caller id set.
	callerIDs := make([]int64, 0, len(directCallers))
	for _, dc := range directCallers {
		if dc.Test {
			continue
		}
		if _, alreadyTest := testIDByID[dc.ID]; alreadyTest {
			covered[dc.ID] = true
			continue
		}
		callerIDs = append(callerIDs, dc.ID)
	}
	if len(callerIDs) == 0 {
		return covered
	}
	placeholders := make([]string, len(callerIDs))
	args := make([]any, len(callerIDs)+len(tests))
	for i, id := range callerIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	// For each caller in the set, check if it has an immediate test
	// caller (matches the old code's one-hop-only look). The old
	// code did NOT recurse deeper — kept that behavior for parity.
	testIDs := make([]string, len(tests))
	for i, t := range tests {
		testIDs[i] = "?"
		args[len(callerIDs)+i] = t.ID
	}
	q := `SELECT DISTINCT r.to_def
	      FROM refs r
	      WHERE r.to_def IN (` + strings.Join(placeholders, ",") + `)
	        AND r.from_def IN (` + strings.Join(testIDs, ",") + `)`
	rows, err := s.db.QueryContext(s.Ctx(), q, args...)
	if err != nil {
		// Falling back to "not covered" on error is safe (over-reports uncovered).
		return covered
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			covered[id] = true
		}
	}
	return covered
}

func (s *SQLiteDB) RefCountsByTarget(targetIDs []int64) (map[int64]int, map[int64]int, error) {
	callers := make(map[int64]int, len(targetIDs))
	tests := make(map[int64]int, len(targetIDs))
	if len(targetIDs) == 0 {
		return callers, tests, nil
	}
	placeholders := make([]string, len(targetIDs))
	args := make([]any, len(targetIDs))
	for i, id := range targetIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT r.to_def, caller.test, COUNT(*)
	      FROM refs r
	      JOIN definitions caller ON caller.id = r.from_def
	      WHERE r.to_def IN (` + strings.Join(placeholders, ",") + `)
	      GROUP BY r.to_def, caller.test`
	rows, err := s.db.QueryContext(s.Ctx(), q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var toDef int64
		var isTest bool
		var count int
		if err := rows.Scan(&toDef, &isTest, &count); err != nil {
			return nil, nil, err
		}
		if isTest {
			tests[toDef] = count
		} else {
			callers[toDef] = count
		}
	}
	return callers, tests, rows.Err()
}

func (s *SQLiteDB) Traverse(startID int64, direction string, refKinds []string, maxDepth int) ([]TraverseResult, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	if maxDepth > 50 {
		maxDepth = 50
	}
	ctx := s.Ctx()
	visited := map[int64]bool{startID: true}
	parent := map[int64]int64{}
	nameOf := map[int64]string{}

	if d, err := s.GetDefinition(startID); err == nil {
		name := d.Name
		if d.Receiver != "" {
			name = "(" + d.Receiver + ")." + d.Name
		}
		nameOf[startID] = name
	}

	kindClause := ""
	var kindArgs []any
	if len(refKinds) > 0 {
		ph := make([]string, len(refKinds))
		for i, k := range refKinds {
			ph[i] = "?"
			kindArgs = append(kindArgs, k)
		}
		kindClause = " AND r.kind IN (" + strings.Join(ph, ",") + ")"
	}

	var results []TraverseResult
	frontier := []int64{startID}

	for depth := 1; depth <= maxDepth && len(frontier) > 0; depth++ {
		placeholders := make([]string, len(frontier))
		var args []any
		for i, id := range frontier {
			placeholders[i] = "?"
			args = append(args, id)
		}
		args = append(args, kindArgs...)

		var q string
		if direction == "callers" {
			q = `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test,
			       COALESCE(d.receiver,''), COALESCE(d.signature,''), '',
			       COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0),
			       COALESCE(d.source_file,''), d.hash, r.to_def
			     FROM definitions d
			     JOIN refs r ON r.from_def = d.id
			     WHERE r.to_def IN (` + strings.Join(placeholders, ",") + `)` + kindClause +
				` ORDER BY d.name`
		} else {
			q = `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test,
			       COALESCE(d.receiver,''), COALESCE(d.signature,''), '',
			       COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0),
			       COALESCE(d.source_file,''), d.hash, r.from_def
			     FROM definitions d
			     JOIN refs r ON r.to_def = d.id
			     WHERE r.from_def IN (` + strings.Join(placeholders, ",") + `)` + kindClause +
				` ORDER BY d.name`
		}

		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return results, fmt.Errorf("sqlite: traverse depth %d: %w", depth, err)
		}
		var nextFrontier []int64
		for rows.Next() {
			var d Definition
			var parentID int64
			if err := rows.Scan(&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test,
				&d.Receiver, &d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine,
				&d.SourceFile, &d.Hash, &parentID); err != nil {
				rows.Close()
				return results, err
			}
			if visited[d.ID] {
				continue
			}
			visited[d.ID] = true
			parent[d.ID] = parentID

			name := d.Name
			if d.Receiver != "" {
				name = "(" + d.Receiver + ")." + d.Name
			}
			nameOf[d.ID] = name

			var path []string
			cur := d.ID
			for {
				path = append([]string{nameOf[cur]}, path...)
				p, ok := parent[cur]
				if !ok || p == startID {
					path = append([]string{nameOf[startID]}, path...)
					break
				}
				cur = p
			}
			results = append(results, TraverseResult{Definition: d, Depth: depth, Path: path})
			nextFrontier = append(nextFrontier, d.ID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return results, err
		}
		if len(nextFrontier) > 1000 {
			break
		}
		frontier = nextFrontier
	}
	return results, nil
}

func (s *SQLiteDB) GetImports(moduleID int64) ([]Import, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
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

func (s *SQLiteDB) SetImports(moduleID int64, imports []Import) error {
	ctx := s.Ctx()
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

	rows, err := s.db.QueryContext(ctx,
		"SELECT imported_path, COALESCE(alias, '') FROM imports WHERE module_id = ? ORDER BY imported_path, alias",
		moduleID)
	if err != nil {
		return fmt.Errorf("sqlite: read imports: %w", err)
	}
	var oldSet []impKey
	for rows.Next() {
		var k impKey
		if err := rows.Scan(&k.Path, &k.Alias); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: scan import: %w", err)
		}
		oldSet = append(oldSet, k)
	}
	rows.Close()

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

	if _, err := s.db.ExecContext(ctx, "DELETE FROM imports WHERE module_id = ?", moduleID); err != nil {
		return fmt.Errorf("sqlite: clear imports: %w", err)
	}
	if len(newSet) == 0 {
		return nil
	}
	for start := 0; start < len(newSet); start += setRefsBatchSize {
		end := start + setRefsBatchSize
		if end > len(newSet) {
			end = len(newSet)
		}
		chunk := newSet[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 3*len(chunk))
		for i, k := range chunk {
			placeholders[i] = "(?, ?, ?)"
			args = append(args, moduleID, k.Path, k.Alias)
		}
		q := "INSERT OR IGNORE INTO imports (module_id, imported_path, alias) VALUES " +
			strings.Join(placeholders, ", ")
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("sqlite: SetImports insert: %w", err)
		}
	}
	return nil
}

func (s *SQLiteDB) QueryLiteralFields(typeName, fieldName, fieldValue string, fieldNames []string, limit int) ([]LiteralField, error) {
	ctx := s.Ctx()
	q := `SELECT lf.id, lf.def_id, COALESCE(d.name,''), lf.type_name, lf.field_name, lf.field_value, lf.line
	      FROM literal_fields lf
	      LEFT JOIN definitions d ON lf.def_id = d.id
	      WHERE 1=1`
	var args []any
	if typeName != "" {
		q += " AND lf.type_name LIKE ?"
		args = append(args, typeName)
	}
	if fieldName != "" {
		q += " AND lf.field_name = ?"
		args = append(args, fieldName)
	}
	if len(fieldNames) > 0 {
		ph := make([]string, len(fieldNames))
		for i, n := range fieldNames {
			ph[i] = "?"
			args = append(args, n)
		}
		q += " AND lf.field_name IN (" + strings.Join(ph, ",") + ")"
	}
	if fieldValue != "" {
		q += " AND lf.field_value LIKE ?"
		args = append(args, fieldValue)
	}
	q += " ORDER BY lf.type_name, lf.field_name"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []LiteralField
	for rows.Next() {
		var f LiteralField
		if err := rows.Scan(&f.ID, &f.DefID, &f.DefName, &f.TypeName, &f.FieldName, &f.FieldValue, &f.Line); err != nil {
			return nil, err
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

func (s *SQLiteDB) SetLiteralFields(defID int64, fields []LiteralField) error {
	ctx := s.Ctx()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM literal_fields WHERE def_id = ?", defID); err != nil {
		return fmt.Errorf("sqlite: clear literal_fields: %w", err)
	}
	if len(fields) == 0 {
		return nil
	}
	for start := 0; start < len(fields); start += setLitFieldsBatchSize {
		end := start + setLitFieldsBatchSize
		if end > len(fields) {
			end = len(fields)
		}
		chunk := fields[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 5*len(chunk))
		for i, f := range chunk {
			placeholders[i] = "(?, ?, ?, ?, ?)"
			args = append(args, defID, f.TypeName, f.FieldName, f.FieldValue, f.Line)
		}
		q := `INSERT INTO literal_fields (def_id, type_name, field_name, field_value, line) VALUES ` +
			strings.Join(placeholders, ", ")
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("sqlite: insert literal_fields: %w", err)
		}
	}
	return nil
}

func (s *SQLiteDB) SetManyLiteralFields(fieldsByDef map[int64][]LiteralField) error {
	if len(fieldsByDef) == 0 {
		return nil
	}
	ctx := s.Ctx()
	defIDs := make([]int64, 0, len(fieldsByDef))
	for id := range fieldsByDef {
		defIDs = append(defIDs, id)
	}
	for start := 0; start < len(defIDs); start += 500 {
		end := start + 500
		if end > len(defIDs) {
			end = len(defIDs)
		}
		chunk := defIDs[start:end]
		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		q := "DELETE FROM literal_fields WHERE def_id IN (" + placeholders + ")"
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("sqlite: SetManyLiteralFields delete: %w", err)
		}
	}

	type row struct {
		defID     int64
		typeName  string
		fieldName string
		value     string
		line      int
	}
	var rows []row
	for defID, fields := range fieldsByDef {
		for _, f := range fields {
			rows = append(rows, row{defID, f.TypeName, f.FieldName, f.FieldValue, f.Line})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	for start := 0; start < len(rows); start += setLitFieldsBatchSize {
		end := start + setLitFieldsBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 5*len(chunk))
		for i, r := range chunk {
			placeholders[i] = "(?, ?, ?, ?, ?)"
			args = append(args, r.defID, r.typeName, r.fieldName, r.value, r.line)
		}
		q := `INSERT INTO literal_fields (def_id, type_name, field_name, field_value, line) VALUES ` +
			strings.Join(placeholders, ", ")
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("sqlite: SetManyLiteralFields insert: %w", err)
		}
	}
	return nil
}

func (s *SQLiteDB) GetCommentsByPragma(pragmaKey string) ([]Comment, error) {
	ctx := s.Ctx()
	q := `SELECT c.id, c.def_id, COALESCE(d.name,''), c.source_file, c.line, c.text, c.kind, COALESCE(c.pragma_key,''), COALESCE(c.pragma_value,'')
	      FROM comments c
	      LEFT JOIN definitions d ON c.def_id = d.id
	      WHERE c.pragma_key LIKE ? ORDER BY c.source_file, c.line`
	rows, err := s.db.QueryContext(ctx, q, pragmaKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Comment
	for rows.Next() {
		var c Comment
		var defID sql.NullInt64
		if err := rows.Scan(&c.ID, &defID, &c.DefName, &c.SourceFile, &c.Line, &c.Text, &c.Kind, &c.PragmaKey, &c.PragmaVal); err != nil {
			return nil, err
		}
		if defID.Valid {
			c.DefID = &defID.Int64
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *SQLiteDB) GetCommentsForDef(defID int64) ([]Comment, error) {
	ctx := s.Ctx()
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.def_id, COALESCE(d.name,''), c.source_file, c.line, c.text, c.kind, COALESCE(c.pragma_key,''), COALESCE(c.pragma_value,'')
		 FROM comments c
		 LEFT JOIN definitions d ON c.def_id = d.id
		 WHERE c.def_id = ? ORDER BY c.line`, defID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Comment
	for rows.Next() {
		var c Comment
		var did sql.NullInt64
		if err := rows.Scan(&c.ID, &did, &c.DefName, &c.SourceFile, &c.Line, &c.Text, &c.Kind, &c.PragmaKey, &c.PragmaVal); err != nil {
			return nil, err
		}
		if did.Valid {
			c.DefID = &did.Int64
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *SQLiteDB) SetFileComments(sourceFile string, comments []Comment) error {
	ctx := s.Ctx()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM comments WHERE source_file = ?", sourceFile); err != nil {
		return fmt.Errorf("sqlite: clear comments: %w", err)
	}
	if len(comments) == 0 {
		return nil
	}
	for start := 0; start < len(comments); start += setRefsBatchSize {
		end := start + setRefsBatchSize
		if end > len(comments) {
			end = len(comments)
		}
		chunk := comments[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 7*len(chunk))
		for i, c := range chunk {
			placeholders[i] = "(?, ?, ?, ?, ?, ?, ?)"
			args = append(args, c.DefID, sourceFile, c.Line, c.Text, c.Kind, c.PragmaKey, c.PragmaVal)
		}
		q := `INSERT INTO comments (def_id, source_file, line, text, kind, pragma_key, pragma_value) VALUES ` +
			strings.Join(placeholders, ", ")
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("sqlite: SetFileComments insert: %w", err)
		}
	}
	return nil
}

func (s *SQLiteDB) SetFileSource(moduleID int64, sourceFile, raw string) error {
	ctx := s.Ctx()
	hash := HashBody(raw)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO file_sources (module_id, source_file, raw, file_hash) VALUES (?, ?, ?, ?)
		 ON CONFLICT(module_id, source_file) DO UPDATE SET raw=excluded.raw, file_hash=excluded.file_hash`,
		moduleID, sourceFile, raw, hash); err != nil {
		return fmt.Errorf("sqlite: upsert file_sources: %w", err)
	}
	return nil
}

func (s *SQLiteDB) GetFileSource(moduleID int64, sourceFile string) (string, error) {
	var raw string
	err := s.db.QueryRowContext(s.Ctx(),
		`SELECT raw FROM file_sources WHERE module_id = ? AND source_file = ?`,
		moduleID, sourceFile).Scan(&raw)
	return raw, err
}

func (s *SQLiteDB) ListFileSources(moduleID int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		`SELECT source_file, raw FROM file_sources WHERE module_id = ?`, moduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var sf, raw string
		if err := rows.Scan(&sf, &raw); err != nil {
			return nil, err
		}
		out[sf] = raw
	}
	return out, rows.Err()
}

func (s *SQLiteDB) DistinctSourceFiles() ([]string, error) {
	rows, err := s.db.QueryContext(s.Ctx(), `SELECT DISTINCT source_file FROM file_sources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sf string
		if err := rows.Scan(&sf); err != nil {
			return nil, err
		}
		out = append(out, sf)
	}
	return out, rows.Err()
}

func (s *SQLiteDB) PruneStaleFileSources(live map[int64]map[string]bool) (int, error) {
	if len(live) == 0 {
		return 0, nil
	}
	pruned := 0
	for modID, liveSet := range live {
		rows, err := s.db.QueryContext(s.Ctx(),
			"SELECT source_file FROM file_sources WHERE module_id = ?", modID)
		if err != nil {
			return pruned, fmt.Errorf("sqlite: list file_sources: %w", err)
		}
		var stale []string
		for rows.Next() {
			var sf string
			if err := rows.Scan(&sf); err != nil {
				rows.Close()
				return pruned, err
			}
			if !liveSet[sf] {
				stale = append(stale, sf)
			}
		}
		rows.Close()
		for _, sf := range stale {
			if _, err := s.db.ExecContext(s.Ctx(),
				"DELETE FROM file_sources WHERE module_id = ? AND source_file = ?", modID, sf); err != nil {
				return pruned, err
			}
			pruned++
		}
	}
	return pruned, nil
}

func (s *SQLiteDB) DeleteFile(sourceFile string) error {
	ctx := s.Ctx()
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM definitions WHERE source_file = ?`, sourceFile)
	if err != nil {
		return fmt.Errorf("sqlite: list defs in %s: %w", sourceFile, err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.DeleteDefinition(id); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM comments WHERE source_file = ?", sourceFile); err != nil {
		return fmt.Errorf("sqlite: delete comments for %s: %w", sourceFile, err)
	}
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM file_sources WHERE source_file = ?", sourceFile); err != nil {
		return fmt.Errorf("sqlite: delete file_sources for %s: %w", sourceFile, err)
	}
	return nil
}

func (s *SQLiteDB) GetProjectFile(path string) (string, error) {
	var content string
	err := s.db.QueryRowContext(s.Ctx(),
		"SELECT content FROM project_files WHERE path = ?", path).Scan(&content)
	return content, err
}

func (s *SQLiteDB) SetProjectFile(path, content string) error {
	_, err := s.db.ExecContext(s.Ctx(),
		`INSERT INTO project_files (path, content) VALUES (?, ?)
		 ON CONFLICT(path) DO UPDATE SET content=excluded.content`, path, content)
	return err
}

func (s *SQLiteDB) ListProjectFiles() ([]string, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		"SELECT path FROM project_files ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLiteDB) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(s.Ctx(),
		`SELECT "value" FROM defn_meta WHERE "key" = ?`, key,
	).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: get meta %q: %w", key, err)
	}
	return v, nil
}

func (s *SQLiteDB) SetMeta(key, value string) error {
	_, err := s.db.ExecContext(s.Ctx(),
		`INSERT INTO defn_meta("key", "value") VALUES(?, ?)
		 ON CONFLICT("key") DO UPDATE SET "value"=excluded."value"`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("sqlite: set meta %q: %w", key, err)
	}
	return nil
}

func (s *SQLiteDB) InsertUpstreamFingerprint(u UpstreamFingerprint) error {
	_, err := s.db.ExecContext(s.Ctx(), `
		INSERT INTO upstream_fingerprints
		    (module_path, version, def_name, kind, receiver, fingerprint, signature, doc)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(module_path, version, def_name, kind, receiver) DO UPDATE SET
		    fingerprint = excluded.fingerprint,
		    signature   = excluded.signature,
		    doc         = excluded.doc`,
		u.ModulePath, u.Version, u.DefName, u.Kind, u.Receiver,
		u.Fingerprint, u.Signature, u.Doc)
	return err
}

func (s *SQLiteDB) InsertUpstreamFingerprints(rows []UpstreamFingerprint) error {
	if len(rows) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO upstream_fingerprints
	    (module_path, version, def_name, kind, receiver, fingerprint, signature, doc)
	    VALUES `)
	args := make([]any, 0, len(rows)*8)
	for i, r := range rows {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?)")
		args = append(args, r.ModulePath, r.Version, r.DefName, r.Kind,
			r.Receiver, r.Fingerprint, r.Signature, r.Doc)
	}
	sb.WriteString(` ON CONFLICT(module_path, version, def_name, kind, receiver) DO UPDATE SET
	    fingerprint = excluded.fingerprint,
	    signature   = excluded.signature,
	    doc         = excluded.doc`)
	_, err := s.db.ExecContext(s.Ctx(), sb.String(), args...)
	return err
}

func (s *SQLiteDB) FindUpstreamMatch(modulePath, defName, kind, receiver, fingerprint string) (*UpstreamFingerprint, error) {
	row := s.db.QueryRowContext(s.Ctx(), `
		SELECT module_path, version, def_name, kind, receiver, fingerprint,
		       COALESCE(signature, ''), COALESCE(doc, '')
		FROM upstream_fingerprints
		WHERE module_path = ? AND def_name = ? AND kind = ? AND receiver = ? AND fingerprint = ?
		LIMIT 1`,
		modulePath, defName, kind, receiver, fingerprint)
	var u UpstreamFingerprint
	err := row.Scan(&u.ModulePath, &u.Version, &u.DefName, &u.Kind,
		&u.Receiver, &u.Fingerprint, &u.Signature, &u.Doc)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *SQLiteDB) FindUpstreamVersions(modulePath, defName, kind, receiver string) ([]UpstreamFingerprint, error) {
	rows, err := s.db.QueryContext(s.Ctx(), `
		SELECT module_path, version, def_name, kind, receiver, fingerprint,
		       COALESCE(signature, ''), COALESCE(doc, '')
		FROM upstream_fingerprints
		WHERE module_path = ? AND def_name = ? AND kind = ? AND receiver = ?
		ORDER BY version`,
		modulePath, defName, kind, receiver)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamFingerprint
	for rows.Next() {
		var u UpstreamFingerprint
		if err := rows.Scan(&u.ModulePath, &u.Version, &u.DefName, &u.Kind,
			&u.Receiver, &u.Fingerprint, &u.Signature, &u.Doc); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *SQLiteDB) CountUpstreamFingerprints() (int, error) {
	var n int
	if err := s.db.QueryRowContext(s.Ctx(),
		`SELECT COUNT(*) FROM upstream_fingerprints`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Query is the read-only op:query surface. SQLite doesn't parse SHOW/DESCRIBE
// (those are MySQL) — we accept SELECT, WITH (CTE), EXPLAIN, and PRAGMA.
func (s *SQLiteDB) Query(query string) ([]map[string]any, error) {
	normalized := strings.TrimSpace(strings.ToUpper(query))
	if !strings.HasPrefix(normalized, "SELECT") &&
		!strings.HasPrefix(normalized, "WITH") &&
		!strings.HasPrefix(normalized, "EXPLAIN") &&
		!strings.HasPrefix(normalized, "PRAGMA") {
		return nil, fmt.Errorf("only SELECT, WITH (CTE), EXPLAIN, and PRAGMA queries are allowed")
	}
	rows, err := s.db.QueryContext(s.Ctx(), query)
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
			// modernc.org/sqlite returns []byte for TEXT under generic Scan.
			// Coerce to string so JSON output is readable.
			if b, ok := vals[i].([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = vals[i]
			}
		}
		results = append(results, row)
	}
	return results, rows.Err()
}

// Simulate is Dolt-branch-based. Under SQLite it would need a SAVEPOINT-per-
// mutation harness on a single dedicated conn. Not wired for Phase 1 — the
// op:simulate MCP tool is rarely used and can degrade gracefully.
func (s *SQLiteDB) Simulate(mutations []Mutation) (*SimulationResult, error) {
	return nil, ErrNotImplemented
}

// SetDefSummaryMinHash stores a precomputed MinHash signature for defID.
// Idempotent — INSERT OR REPLACE keys off def_id. Called at UpsertDefinition
// time (below) and by the backfill pass on OpenSQLite.
func (s *SQLiteDB) SetDefSummaryMinHash(defID int64, minhash []byte) error {
	_, err := s.db.ExecContext(s.Ctx(),
		`INSERT OR REPLACE INTO def_summaries(def_id, minhash) VALUES (?, ?)`,
		defID, minhash)
	if err != nil {
		return fmt.Errorf("sqlite: set def summary %d: %w", defID, err)
	}
	return nil
}

// AllDefSummaryMinHashes loads every stored MinHash keyed by def_id.
// Used by the `similar` op's O(N) Jaccard scan.
func (s *SQLiteDB) AllDefSummaryMinHashes() (map[int64][]byte, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		`SELECT def_id, minhash FROM def_summaries WHERE minhash IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: all def summaries: %w", err)
	}
	defer rows.Close()
	out := make(map[int64][]byte)
	for rows.Next() {
		var id int64
		var mh []byte
		if err := rows.Scan(&id, &mh); err != nil {
			return nil, fmt.Errorf("sqlite: scan def summary: %w", err)
		}
		out[id] = mh
	}
	return out, rows.Err()
}

// migrateAddSummaryColumns idempotently adds the #160 columns
// (one_line, summary_body_hash, summary_model) to def_summaries for
// existing DBs. Fresh DBs already have them from CREATE TABLE; this
// only matters when opening a DB created before this change.
//
// SQLite has no ALTER TABLE ... ADD COLUMN IF NOT EXISTS, so we swallow
// the "duplicate column name" error on each call. Any other error is
// fatal.
func migrateAddSummaryColumns(db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE def_summaries ADD COLUMN one_line TEXT`,
		`ALTER TABLE def_summaries ADD COLUMN summary_body_hash TEXT`,
		`ALTER TABLE def_summaries ADD COLUMN summary_model TEXT`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("migrate summary columns: %w", err)
		}
	}
	return nil
}

// GetDefSummary reads the #160 semantic-summary row for defID.
// Returns (nil, nil) when no row exists yet — the fire-and-forget
// worker hasn't populated one, or the schema was populated before
// #160 landed. Missing one_line is normalized to empty string.
func (s *SQLiteDB) GetDefSummary(defID int64) (*DefSummary, error) {
	var oneLine, bodyHash, model sql.NullString
	err := s.db.QueryRowContext(s.Ctx(),
		`SELECT COALESCE(one_line,''), COALESCE(summary_body_hash,''), COALESCE(summary_model,'')
		 FROM def_summaries WHERE def_id = ?`, defID,
	).Scan(&oneLine, &bodyHash, &model)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("sqlite: get def summary %d: %w", defID, err)
	}
	if !oneLine.Valid || oneLine.String == "" {
		return nil, nil
	}
	return &DefSummary{
		OneLine:  oneLine.String,
		BodyHash: bodyHash.String,
		Model:    model.String,
	}, nil
}

// SetDefSummary writes/updates the #160 semantic-summary row for
// defID. Idempotent — INSERT OR REPLACE keys off def_id and preserves
// the existing minhash column (BUT only if the row already exists;
// SQLite's ON CONFLICT DO UPDATE guarantees this). If no row exists
// yet, we fall back to a two-statement upsert path so we don't
// clobber a not-yet-computed minhash with NULL.
func (s *SQLiteDB) SetDefSummary(defID int64, sum *DefSummary) error {
	if sum == nil {
		return nil
	}
	// ON CONFLICT DO UPDATE ensures we don't lose the minhash column
	// when the row already exists from the #151 backfill pass.
	_, err := s.db.ExecContext(s.Ctx(),
		`INSERT INTO def_summaries(def_id, one_line, summary_body_hash, summary_model)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(def_id) DO UPDATE SET
		   one_line          = excluded.one_line,
		   summary_body_hash = excluded.summary_body_hash,
		   summary_model     = excluded.summary_model`,
		defID, sum.OneLine, sum.BodyHash, sum.Model)
	if err != nil {
		return fmt.Errorf("sqlite: set def summary %d: %w", defID, err)
	}
	return nil
}
