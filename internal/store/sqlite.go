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
	"time"

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

// dbConn is the subset of *sql.DB / *sql.Tx that SQLiteDB's query
// methods actually call. Both types satisfy it with identical method
// sets, which lets Begin() hand back a *SQLiteDB backed by a real
// *sql.Tx instead of the pool -- every existing method keeps working
// completely unchanged, since none of them care which one is
// providing the connection. #214: this is what makes Begin()'s
// returned commit/rollback actually mean something -- previously
// nothing gave callers a way to route writes through the transaction
// it opened, so every write auto-committed via the pool regardless.
type dbConn interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SQLiteDB is a store.Backend backed by a local SQLite database file.
// Writes hit disk on transaction commit (WAL mode). Safe for concurrent
// read; writers are serialized by SQLite itself (single-writer model).
type SQLiteDB struct {
	db   dbConn  // query/exec surface: the pool normally, a *sql.Tx when this is a tx-scoped view returned by Begin()
	pool *sql.DB // only set on the root instance; used for Close/Ping/BeginTx -- nil on a tx-scoped view, which is never Close()'d or re-Begin()'able
	path string

	// txMu serializes Begin()...commit()/rollback() on the root instance.
	// The pool allows up to 4 concurrent connections (SetMaxOpenConns),
	// but a Begin()-returned tx-scoped view is only isolated from OTHER
	// writes if no other connection in the pool writes concurrently --
	// without this, a concurrent write via s.backend (outside the tx)
	// racing an in-flight apply transaction can leave a rolled-back
	// batch's writes silently persisted. Root-instance-only; nil on a
	// tx-scoped view, which never calls Begin() again.
	txMu sync.Mutex

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

	// busy_timeout was 5000ms -- too short for the case that actually
	// hits it in practice: an agent's foreground edit/create racing a
	// large background startup ingest, which can hold/reacquire the
	// write lock in bursts for the whole ingest's duration (minutes on
	// a large repo like prometheus, not milliseconds). Real trajectory
	// (prometheus-12024, 2026-08-10): raw "sqlite: update definition:
	// database is locked (5) (SQLITE_BUSY)" surfaced directly to the
	// agent from two SEQUENTIAL (not even parallel) edit calls while
	// the "[startup ingest in progress]" banner was still attached to
	// every tool result across the entire 638s session. 30s gives a
	// foreground write a real chance to land during a long ingest
	// instead of failing fast and pushing error recovery onto the
	// model.
	// _txlock=immediate: a DEFERRED transaction (the driver default) that
	// reads then writes can fail to upgrade its snapshot with
	// SQLITE_BUSY_SNAPSHOT (extended code 517) when a concurrent writer
	// commits to the WAL in between -- busy_timeout's retry-on-contention
	// does NOT cover this case, since it's a snapshot-isolation conflict,
	// not ordinary lock contention. Confirmed live: reproduced reliably
	// under 8x background-writer contention in
	// TestBeginRollback_ConcurrentWriterCannotObserveRolledBackWrite,
	// gone after this fix under the same stress. Every BeginTx call in
	// this codebase goes through Begin() (write-only, see its doc
	// comment), so acquiring the write lock immediately at BEGIN instead
	// of deferring it has no read-only-transaction downside here.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(30000)&_txlock=immediate"

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

	// #363: rebuild definitions' UNIQUE constraint for DBs predating
	// 7d66258 (fresh DBs already have it from CREATE TABLE above).
	if err := migrateDefinitionsSourceFileUniqueConstraint(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: migrate definitions unique constraint: %w", err)
	}

	// Backfill FTS if this is an existing DB predating the FTS5 addition
	// (task #137). The CREATE VIRTUAL TABLE IF NOT EXISTS runs above but
	// doesn't populate — triggers only fire on future writes. If the
	// source tables have rows and the FTS table is empty, seed it.
	if err := backfillFTS(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: backfill fts: %w", err)
	}

	sq := &SQLiteDB{db: db, pool: db, path: path}
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
		SELECT b.def_id, b.body, COALESCE(d.signature, '') FROM bodies b
		JOIN definitions d ON d.id = b.def_id
		LEFT JOIN def_summaries ds ON ds.def_id = b.def_id
		WHERE ds.def_id IS NULL`)
	if err != nil {
		return fmt.Errorf("select missing summaries: %w", err)
	}
	defer rows.Close()
	tx, err := s.pool.BeginTx(ctx, nil)
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
		var body, signature string
		if err := rows.Scan(&id, &body, &signature); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := stmt.ExecContext(ctx, id, ComputeMinHashForDef(body, signature)); err != nil {
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
		s.closeErr = s.pool.Close()
	})
	return s.closeErr
}

func (s *SQLiteDB) Path() string { return s.path }

func (s *SQLiteDB) Ping(ctx context.Context) error { return s.pool.PingContext(ctx) }

func (s *SQLiteDB) Ctx() context.Context { return context.Background() }

func (s *SQLiteDB) CleanTempFiles() {}

func (s *SQLiteDB) Begin() (tx Backend, commit func() error, rollback func(), err error) {
	s.txMu.Lock()
	t, err := s.pool.BeginTx(context.Background(), nil)
	if err != nil {
		s.txMu.Unlock()
		return nil, nil, nil, fmt.Errorf("sqlite: begin: %w", err)
	}
	txDB := &SQLiteDB{db: t, path: s.path}
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(s.txMu.Unlock) }
	return txDB, func() error { defer unlock(); return t.Commit() }, func() { defer unlock(); _ = t.Rollback() }, nil
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
//
// Struct fields are excluded from every name-only match below (#352).
// A field's bare Name commonly collides with an unrelated top-level
// def -- Go's own "Foo *Foo" self-referencing field idiom (e.g. a
// health-check config's "Upstream *Upstream" field) means a field can
// share its exact Name with the type it points at, in the same file.
// Confirmed live (caddyserver/caddy-7870): a bare, receiverless lookup
// for the type "Upstream" resolved to a field named "Upstream" on an
// unrelated struct instead, because the field had more callers and
// this tiebreak had no kind awareness -- the agent then spent ~20 tool
// calls trying to edit what it believed was the type, always refused
// as "doesn't support struct fields," never realizing the type itself
// was never actually targeted. Fields are only meaningfully addressed
// via their parent type anyway, through GetDefinitionByNameAndReceiver
// (unaffected by this exclusion) -- that path already backs every
// legitimate field operation (see handleFieldRename).
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
		query := baseQuery + " JOIN modules m ON d.module_id = m.id WHERE d.name = ? AND d.kind != 'field' AND m.path = ?"
		d := &Definition{}
		if err := scanSQLiteDef(s.db.QueryRowContext(s.Ctx(), query, name, modulePath), d); err == nil {
			return d, nil
		}
		query = baseQuery + " JOIN modules m ON d.module_id = m.id WHERE d.name = ? AND d.kind != 'field' AND m.path LIKE ?" +
			` ORDER BY (SELECT COUNT(*) FROM refs r WHERE r.to_def = d.id) DESC LIMIT 1`
		d = &Definition{}
		if err := scanSQLiteDef(s.db.QueryRowContext(s.Ctx(), query, name, "%"+modulePath+"%"), d); err == nil {
			return d, nil
		}
	}

	query := baseQuery + " WHERE d.name = ? AND d.kind != 'field'" +
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
		// Exact match only -- unlike GetDefinitionByName's modulePath
		// param (which can be raw user-typed shorthand and deliberately
		// falls back to a fuzzy LIKE match), every current caller here
		// passes an already-resolved, fully-qualified module path
		// (mod.Path from findModule/findModuleByFile, or pkgPath from
		// go/types). A LIKE '%...%' match on that is never useful and
		// is actively harmful: any module path that's a prefix of
		// another (e.g. "zrpc" vs "zrpc/internal") false-collides,
		// which is exactly what made handleCreate's existence check
		// reject a legitimate create as "already exists" in the wrong
		// module.
		query = sqliteFullDefSelect + `
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 JOIN modules m ON d.module_id = m.id
		 WHERE d.name = ? AND m.path = ? AND COALESCE(d.receiver,'') = ?`
		args = []any{name, modulePath, receiver}
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
	// An exact sourceFile already disambiguates uniquely (it's the
	// real repo-relative on-disk path) -- match on it alone rather
	// than ALSO requiring m.path LIKE %fileSuffix%. That module-path
	// filter assumes a module's Go import path is literally a
	// filesystem-path suffix of the file's directory, which breaks
	// for any nested module using semantic import versioning (e.g.
	// etcd's go.etcd.io/etcd/client/pkg/v3/transport does NOT end
	// with ".../client/pkg/transport" -- "v3" sits in the middle) --
	// real definitions were silently dropped for every such file.
	var query string
	var args []any
	if sourceFile != "" {
		query = `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test,
	            COALESCE(d.receiver,''), COALESCE(d.signature,''),
	            COALESCE(d.start_line,0), COALESCE(d.end_line,0),
	            COALESCE(d.source_file,'')
	          FROM definitions d
	          WHERE d.source_file = ?`
		args = []any{sourceFile}
	} else {
		query = `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test,
	            COALESCE(d.receiver,''), COALESCE(d.signature,''),
	            COALESCE(d.start_line,0), COALESCE(d.end_line,0),
	            COALESCE(d.source_file,'')
	          FROM definitions d
	          JOIN modules m ON d.module_id = m.id
	          WHERE m.path LIKE ?`
		args = []any{"%" + fileSuffix + "%"}
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
			&d.Receiver, &d.Signature, &d.StartLine, &d.EndLine, &d.SourceFile); err != nil {
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
		 WHERE module_id = ? AND name = ? AND kind = ? AND COALESCE(receiver,'') = COALESCE(?,'') AND test = ? AND source_file = ?`,
		d.ModuleID, d.Name, d.Kind, d.Receiver, d.Test, d.SourceFile,
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
		_ = s.SetDefSummaryMinHash(id, ComputeMinHashForDef(d.Body, d.Signature))
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
	_ = s.SetDefSummaryMinHash(existingID, ComputeMinHashForDef(d.Body, d.Signature))
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
		modID      int64
		name       string
		kind       string
		receiver   string
		test       bool
		sourceFile string
	}
	keyOf := func(d *Definition) natKey {
		return natKey{d.ModuleID, d.Name, d.Kind, d.Receiver, d.Test, d.SourceFile}
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
			`SELECT id, name, kind, COALESCE(receiver,''), test, COALESCE(source_file,''), hash
			 FROM definitions WHERE module_id = ?`, modID)
		if err != nil {
			return nil, fmt.Errorf("sqlite: UpsertDefinitionsBulk lookup module %d: %w", modID, err)
		}
		for rows.Next() {
			var e existing
			var name, kind, receiver, sourceFile, hash string
			var test bool
			if err := rows.Scan(&e.id, &name, &kind, &receiver, &test, &sourceFile, &hash); err != nil {
				rows.Close()
				return nil, fmt.Errorf("sqlite: UpsertDefinitionsBulk scan: %w", err)
			}
			e.hash = hash
			existingByKey[natKey{modID, name, kind, receiver, test, sourceFile}] = e
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
	// constraint on (module_id, name, kind, receiver, test, source_file)
	// and the whole flush fails. Last-write-wins semantics: the later
	// Definition value replaces the earlier one in the INSERT, and both
	// input positions receive the same row ID after the insert.
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
			mhArgs = append(mhArgs, firstID+int64(i), ComputeMinHashForDef(d.Body, d.Signature))
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

func (s *SQLiteDB) QueryLiteralFields(typeName, fieldName, fieldValue string, fieldNames []string, defIDs []int64, limit int, skipOrderBy, skipDefName bool) ([]LiteralField, error) {
	ctx := s.Ctx()
	// skipDefName drops the LEFT JOIN definitions entirely -- it exists
	// only to populate DefName, which bulk callers that key off DefID
	// discard anyway.
	defNameCol := "COALESCE(d.name,'')"
	from := `FROM literal_fields lf
	      LEFT JOIN definitions d ON lf.def_id = d.id`
	if skipDefName {
		defNameCol = "''"
		from = "FROM literal_fields lf"
	}
	q := fmt.Sprintf(`SELECT lf.id, lf.def_id, %s, lf.type_name, lf.field_name, lf.field_value, lf.line
	      %s
	      WHERE 1=1`, defNameCol, from)
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
	if len(defIDs) > 0 {
		ph := make([]string, len(defIDs))
		for i, id := range defIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		q += " AND lf.def_id IN (" + strings.Join(ph, ",") + ")"
	}
	if fieldValue != "" {
		// A LIKE never uses an index while case_sensitive_like is OFF (the
		// default), so `field_value LIKE 'Foo%'` scans the whole table however
		// many indexes exist. An anchored prefix is equivalent to a range, and
		// a range does use one: measured 36.9ms -> 0.06ms for a 9-row answer
		// out of 132k rows.
		if lo, hi, ok := likePrefixRange(fieldValue); ok {
			q += " AND lf.field_value >= ? AND lf.field_value < ?"
			args = append(args, lo, hi)
		} else {
			q += " AND lf.field_value LIKE ?"
			args = append(args, fieldValue)
		}
	}
	// skipOrderBy: bulk callers that regroup the result themselves pay
	// ~103ms on a 90k-row query for an ordering they discard.
	if !skipOrderBy {
		q += " ORDER BY lf.type_name, lf.field_name"
	}
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]LiteralField, 0, literalFieldsHint(limit))
	for rows.Next() {
		var f LiteralField
		if err := rows.Scan(&f.ID, &f.DefID, &f.DefName, &f.TypeName, &f.FieldName, &f.FieldValue, &f.Line); err != nil {
			return nil, err
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

// literalFieldsHint sizes the result slice. A bounded query knows its ceiling;
// an unbounded one over a large corpus returns tens of thousands of rows, and
// growing from nil re-copies the whole result about seventeen times.
func literalFieldsHint(limit int) int {
	if limit > 0 && limit < 4096 {
		return limit
	}
	return 4096
}

// likePrefixRange converts an anchored-prefix LIKE pattern into the equivalent
// half-open range, so the query planner can use an index on the column.
//
// Only patterns of the form `literal%` qualify: the wildcard must be the single
// trailing character, and the prefix must contain no `%`, `_`, or escape. Any
// other pattern (unanchored `%foo%`, an interior `_`) has no range equivalent
// and is left as a LIKE.
//
// The upper bound increments the prefix's last byte. That is exact for the
// identifier-shaped values this optimises and never under-matches otherwise:
// a byte that cannot be incremented (0xFF) falls back to LIKE rather than
// risking a range that silently drops rows.
func likePrefixRange(pattern string) (lo, hi string, ok bool) {
	if len(pattern) < 2 || !strings.HasSuffix(pattern, "%") {
		return "", "", false
	}
	prefix := pattern[:len(pattern)-1]
	if strings.ContainsAny(prefix, "%_\\") {
		return "", "", false
	}
	if prefix[len(prefix)-1] == 0xFF {
		return "", "", false
	}
	// Increment the last BYTE, not the last rune. SQLite's BINARY collation
	// compares with memcmp, so the byte successor is the correct upper bound;
	// string(rune(b+1)) would UTF-8-encode anything above 0x7F into two bytes
	// and produce a bound that sorts wrong.
	upper := []byte(prefix)
	upper[len(upper)-1]++
	return prefix, string(upper), true
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
	results := make([]map[string]any, 0)
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

// SetDefSummaryMinHash writes/updates just the def_summaries.minhash
// column for defID. Uses ON CONFLICT DO UPDATE targeting only minhash --
// not INSERT OR REPLACE, which does a full row DELETE+INSERT and would
// silently reset one_line/summary_body_hash/summary_model to NULL on
// every call, wiping out a previously-generated #160 summary. Mirrors
// SetDefSummary's own fix for the identical hazard in the other
// direction (see its doc comment).
func (s *SQLiteDB) SetDefSummaryMinHash(defID int64, minhash []byte) error {
	_, err := s.db.ExecContext(s.Ctx(),
		`INSERT INTO def_summaries(def_id, minhash) VALUES (?, ?)
		 ON CONFLICT(def_id) DO UPDATE SET minhash = excluded.minhash`,
		defID, minhash)
	if err != nil {
		return fmt.Errorf("sqlite: set def summary minhash %d: %w", defID, err)
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
// (one_line, summary_body_hash, summary_model, crux) to def_summaries for
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
		`ALTER TABLE def_summaries ADD COLUMN crux TEXT`,
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
	var oneLine, bodyHash, model, crux sql.NullString
	err := s.db.QueryRowContext(s.Ctx(),
		`SELECT COALESCE(one_line,''), COALESCE(summary_body_hash,''), COALESCE(summary_model,''), COALESCE(crux,'')
		 FROM def_summaries WHERE def_id = ?`, defID,
	).Scan(&oneLine, &bodyHash, &model, &crux)
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
		Crux:     crux.String,
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
		`INSERT INTO def_summaries(def_id, one_line, summary_body_hash, summary_model, crux)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(def_id) DO UPDATE SET
		   one_line          = excluded.one_line,
		   summary_body_hash = excluded.summary_body_hash,
		   summary_model     = excluded.summary_model,
		   crux              = excluded.crux`,
		defID, sum.OneLine, sum.BodyHash, sum.Model, sum.Crux)
	if err != nil {
		return fmt.Errorf("sqlite: set def summary %d: %w", defID, err)
	}
	return nil
}

// ListDefsMissingSummary returns the IDs of definitions that do not
// have a model-generated summary in def_summaries.one_line. Task #160
// stage 3a backfill uses this to enqueue every def needing summarization.
//
// Staleness (summary written but the body has since been edited) is
// NOT reported here — handleGetDefinition falls back to full body on
// hash mismatch, and the next mutation re-enqueues via applyEditTerse.
// Callers wanting to force re-summarize (e.g. after a model upgrade)
// should truncate def_summaries.one_line first.
//
// Returns IDs in ascending order so callers can page deterministically
// (limit N, resume from last-seen+1).
func (s *SQLiteDB) ListDefsMissingSummary() ([]int64, error) {
	rows, err := s.db.QueryContext(s.Ctx(), `
		SELECT d.id
		FROM definitions d
		LEFT JOIN def_summaries ds ON ds.def_id = d.id
		WHERE ds.one_line IS NULL OR ds.one_line = ''
		ORDER BY d.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list missing summaries: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite: scan missing summary id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetFileSummary reads the #212 file-level narrative row for
// sourceFile. Returns (nil, nil) when no row exists yet -- the file
// hasn't been overview'd since ingest, or ANTHROPIC_API_KEY wasn't set
// when it was. Missing narrative is normalized to "no summary".
func (s *SQLiteDB) GetFileSummary(sourceFile string) (*FileSummary, error) {
	var narrative, bodyHash, model sql.NullString
	err := s.db.QueryRowContext(s.Ctx(),
		`SELECT COALESCE(narrative,''), COALESCE(summary_body_hash,''), COALESCE(summary_model,'')
		 FROM file_summaries WHERE source_file = ?`, sourceFile,
	).Scan(&narrative, &bodyHash, &model)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("sqlite: get file summary %s: %w", sourceFile, err)
	}
	if !narrative.Valid || narrative.String == "" {
		return nil, nil
	}
	return &FileSummary{
		Narrative: narrative.String,
		BodyHash:  bodyHash.String,
		Model:     model.String,
	}, nil
}

// SetFileSummary writes/updates the #212 file-level narrative row for
// sourceFile. Idempotent -- INSERT OR REPLACE keys off source_file.
func (s *SQLiteDB) SetFileSummary(sourceFile string, moduleID int64, sum *FileSummary) error {
	if sum == nil {
		return nil
	}
	_, err := s.db.ExecContext(s.Ctx(),
		`INSERT INTO file_summaries(source_file, module_id, narrative, summary_body_hash, summary_model)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(source_file) DO UPDATE SET
		   module_id         = excluded.module_id,
		   narrative         = excluded.narrative,
		   summary_body_hash = excluded.summary_body_hash,
		   summary_model     = excluded.summary_model`,
		sourceFile, moduleID, sum.Narrative, sum.BodyHash, sum.Model)
	if err != nil {
		return fmt.Errorf("sqlite: set file summary %s: %w", sourceFile, err)
	}
	return nil
}

// SearchDefSummaries finds def IDs whose one_line summary contains
// pattern (case-insensitive LIKE %pattern%). Rows without a summary
// are skipped. Result is ordered by production caller-count desc as a
// rough default relevance; the caller (context op #197) applies its
// own scoring on top. This is the #160→#195 semantic bridge that lets
// context find defs whose behavior matches the question even when the
// name has zero lexical overlap.
func (s *SQLiteDB) SearchDefSummaries(pattern string) ([]int64, error) {
	if pattern == "" {
		return nil, nil
	}
	// LOWER on both sides for case-insensitive LIKE. Cap at 200 hits
	// so a pathological query doesn't drag the whole DB back.
	rows, err := s.db.QueryContext(s.Ctx(), `
		SELECT ds.def_id
		FROM def_summaries ds
		WHERE ds.one_line IS NOT NULL
		  AND ds.one_line != ''
		  AND LOWER(ds.one_line) LIKE '%' || LOWER(?) || '%'
		ORDER BY ds.def_id ASC
		LIMIT 200`, pattern)
	if err != nil {
		return nil, fmt.Errorf("sqlite: search def summaries: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqlite: scan summary search id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetExplainCache returns the cached #192 explain-QA answer for
// cacheKey, or (nil, nil) if there's no entry yet.
func (s *SQLiteDB) GetExplainCache(cacheKey string) (*ExplainCacheEntry, error) {
	var answer, refs, model string
	err := s.db.QueryRowContext(s.Ctx(),
		`SELECT answer, refs, model FROM explain_cache WHERE cache_key = ?`, cacheKey,
	).Scan(&answer, &refs, &model)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("sqlite: get explain cache %s: %w", cacheKey, err)
	}
	var refList []string
	if refs != "" {
		refList = strings.Split(refs, ",")
	}
	return &ExplainCacheEntry{Answer: answer, Refs: refList, Model: model}, nil
}

// SetExplainCache writes/updates the #192 explain-QA cache row for
// cacheKey. Idempotent -- INSERT OR REPLACE keys off cache_key.
func (s *SQLiteDB) SetExplainCache(cacheKey, question, scope, answer, model string, refs []string) error {
	_, err := s.db.ExecContext(s.Ctx(),
		`INSERT INTO explain_cache(cache_key, question, scope, answer, refs, model, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(cache_key) DO UPDATE SET
		   question   = excluded.question,
		   scope      = excluded.scope,
		   answer     = excluded.answer,
		   refs       = excluded.refs,
		   model      = excluded.model,
		   created_at = excluded.created_at`,
		cacheKey, question, scope, answer, strings.Join(refs, ","), model, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("sqlite: set explain cache %s: %w", cacheKey, err)
	}
	return nil
}

// CountDefinitionsByName returns how many definitions share name,
// regardless of module/package. Excludes struct fields (#352 followup)
// to stay consistent with GetDefinitionByName, which already excludes
// them from bare-name resolution -- without this, a receiverless write
// to a type sharing its name with an unrelated field would resolve
// cleanly via GetDefinitionByName and then get refused as "ambiguous"
// anyway by this count, right after resolving it correctly.
func (s *SQLiteDB) CountDefinitionsByName(name string) (int, error) {
	var n int
	err := s.db.QueryRowContext(s.Ctx(), "SELECT COUNT(*) FROM definitions WHERE name = ? AND kind != 'field'", name).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ListFileSourceNames is the metadata-only sibling of ListFileSources --
// same WHERE clause, but selects source_file alone instead of source_file
// + raw. Callers that only need filenames (e.g. testCoverageHint checking
// for an existing _test.go sibling on every successful write) previously
// paid for the full raw source text of every file in the module on every
// call -- a real, avoidable cost on the hottest path in the system,
// scaling with total file size in the package rather than file count.
func (s *SQLiteDB) ListFileSourceNames(moduleID int64) ([]string, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		`SELECT source_file FROM file_sources WHERE module_id = ?`, moduleID)
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

func (s *SQLiteDB) UpdateDefinitionReceiver(id int64, newReceiver, newBody, newSignature string) error {
	hash := HashBody(newBody)
	ctx := s.Ctx()
	if _, err := s.db.ExecContext(ctx,
		`UPDATE definitions
		 SET receiver = ?, signature = ?, hash = ?
		 WHERE id = ?`,
		newReceiver, newSignature, hash, id,
	); err != nil {
		return fmt.Errorf("sqlite: update definition receiver: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE bodies SET body = ? WHERE def_id = ?`, newBody, id,
	); err != nil {
		return fmt.Errorf("sqlite: update receiver body: %w", err)
	}
	return nil
}

// SetManyExternalInterfaces mirrors SetManyReferences's delete-then-insert
// shape: for every def_id key present in namesByDef, wipe its existing
// def_external_interfaces rows and reinsert the current set. A def_id
// absent from the map is left untouched.
func (s *SQLiteDB) SetManyExternalInterfaces(namesByDef map[int64][]string) error {
	if len(namesByDef) == 0 {
		return nil
	}
	ctx := s.Ctx()

	defIDs := make([]int64, 0, len(namesByDef))
	for id := range namesByDef {
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
		q := "DELETE FROM def_external_interfaces WHERE def_id IN (" + placeholders + ")"
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("sqlite: SetManyExternalInterfaces delete: %w", err)
		}
	}

	type row struct {
		defID int64
		iface string
	}
	seen := make(map[row]bool)
	var rows []row
	for defID, names := range namesByDef {
		for _, name := range names {
			r := row{defID, name}
			if seen[r] {
				continue
			}
			seen[r] = true
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	for start := 0; start < len(rows); start += 500 {
		end := start + 500
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 2*len(chunk))
		for i, r := range chunk {
			placeholders[i] = "(?, ?)"
			args = append(args, r.defID, r.iface)
		}
		q := "INSERT OR IGNORE INTO def_external_interfaces (def_id, iface_name) VALUES " +
			strings.Join(placeholders, ", ")
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("sqlite: SetManyExternalInterfaces insert: %w", err)
		}
	}
	return nil
}

func (s *SQLiteDB) GetExternalInterfaces(defID int64) ([]string, error) {
	ctx := s.Ctx()
	rows, err := s.db.QueryContext(ctx, `SELECT iface_name FROM def_external_interfaces WHERE def_id = ?`, defID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: get external interfaces: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite: scan external interface: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// CountDefinitionsByNameAndReceiver returns how many definitions share
// both name and receiver, regardless of module/package -- the
// receiver-qualified sibling of CountDefinitionsByName, used to detect
// ambiguity for the "(*Recv).Method" name convention (see its own doc
// comment on the Backend interface for why the plain name-only count
// can't see this).
func (s *SQLiteDB) CountDefinitionsByNameAndReceiver(name, receiver string) (int, error) {
	var n int
	err := s.db.QueryRowContext(s.Ctx(), "SELECT COUNT(*) FROM definitions WHERE name = ? AND COALESCE(receiver,'') = ?", name, receiver).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// AllFileHashes returns every known source file's last-ingested content
// hash, keyed by source_file (module-relative path), across ALL modules.
// A source_file that happens to collide across two modules keeps whichever
// row the query visits last -- acceptable here because the caller (the MCP
// freshness probe) only uses this as a cheap "did this file change since
// last ingest" heuristic; the actual re-ingest it triggers re-resolves the
// correct module on its own.
func (s *SQLiteDB) AllFileHashes() (map[string]string, error) {
	rows, err := s.db.QueryContext(s.Ctx(), `SELECT source_file, file_hash FROM file_sources`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var sf, hash string
		if err := rows.Scan(&sf, &hash); err != nil {
			return nil, err
		}
		out[sf] = hash
	}
	return out, rows.Err()
}

// EdgesAmong returns every refs edge (from_def, to_def) where BOTH
// endpoints are in ids -- a bounded, cheap subgraph among a candidate set
// (e.g. a search result pool), not a walk of the whole project's refs
// table. Used by the search/context rankers to seed a personalized
// PageRank re-rank (see internal/rank.GraphRerank) restricted to the
// candidates actually being ranked.
func (s *SQLiteDB) EdgesAmong(ids []int64) ([][2]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)*2)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	in := "(" + strings.Join(placeholders, ",") + ")"
	q := `SELECT from_def, to_def FROM refs WHERE from_def IN ` + in + ` AND to_def IN ` + in
	args = append(args, args...)
	rows, err := s.db.QueryContext(s.Ctx(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]int64
	for rows.Next() {
		var from, to int64
		if err := rows.Scan(&from, &to); err != nil {
			return nil, err
		}
		out = append(out, [2]int64{from, to})
	}
	return out, rows.Err()
}

// definitionsUniqueConstraintHasSourceFile reports whether the on-disk
// definitions table's UNIQUE constraint (a SQLite autoindex, since it's
// declared inline in CREATE TABLE) already includes source_file. False
// for any DB whose definitions table was first created before commit
// 7d66258.
func definitionsUniqueConstraintHasSourceFile(ctx context.Context, db *sql.DB) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA index_list('definitions')`)
	if err != nil {
		return false, err
	}
	var uniqueIdxNames []string
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return false, err
		}
		if unique == 1 {
			uniqueIdxNames = append(uniqueIdxNames, name)
		}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}

	for _, name := range uniqueIdxNames {
		cols, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_info(%q)`, name))
		if err != nil {
			return false, err
		}
		found := false
		count := 0
		for cols.Next() {
			var seqno, cid int
			var colName string
			if err := cols.Scan(&seqno, &cid, &colName); err != nil {
				cols.Close()
				return false, err
			}
			count++
			if colName == "source_file" {
				found = true
			}
		}
		if err := cols.Close(); err != nil {
			return false, err
		}
		if found && count == 6 {
			return true, nil
		}
	}
	return false, nil
}

// migrateDefinitionsSourceFileUniqueConstraint rebuilds the definitions
// table for existing DBs created before source_file joined its UNIQUE
// constraint (commit 7d66258, #157-class fix). Fresh DBs already get the
// 6-column UNIQUE(module_id, name, kind, receiver, test, source_file)
// from CREATE TABLE; a pre-existing DB keeps whatever narrower constraint
// was baked in when its table was first created, forever -- CREATE TABLE
// IF NOT EXISTS is a full no-op once the table exists, so the schema
// text changing underneath it has zero effect on already-created DBs.
//
// #363 (2026-08-29 winze report): an unmigrated DB is exposed to two
// distinct failure modes. First, the exact corruption 7d66258 was meant
// to prevent -- two legitimately distinct definitions that agree on
// module/name/kind/receiver/test but differ only in source_file (e.g.
// two files each with their own func init()) still collide on the old
// 5-column constraint. Second, newer app code now keys its upsert
// lookup on all 6 columns; when that lookup misses (the stored
// source_file no longer matches what's freshly computed for what is
// really the same definition -- e.g. a row predating source_file being
// populated at all), the code falls through to a plain INSERT, which
// then hits the real, narrower on-disk constraint and hard-fails the
// entire sync with a UNIQUE constraint violation.
//
// This follows SQLite's own documented 12-step table-rebuild recipe:
// disable foreign key enforcement for the duration (must happen before
// the transaction starts -- the pragma is a no-op inside one), rebuild
// in a transaction so any failure rolls back cleanly, verify with
// PRAGMA foreign_key_check before committing, then reapply the schema
// DDL to recreate the indexes/triggers that DROP TABLE removes along
// with the old table object. Idempotent: a no-op once the constraint
// already includes source_file.
func migrateDefinitionsSourceFileUniqueConstraint(db *sql.DB) error {
	ctx := context.Background()

	ok, err := definitionsUniqueConstraintHasSourceFile(ctx, db)
	if err != nil {
		return fmt.Errorf("check definitions unique constraint: %w", err)
	}
	if ok {
		return nil
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire dedicated connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	defer conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	rebuild := []string{
		`CREATE TABLE definitions_new (
		    id          INTEGER PRIMARY KEY AUTOINCREMENT,
		    module_id   INTEGER NOT NULL,
		    name        TEXT NOT NULL,
		    kind        TEXT NOT NULL,
		    exported    INTEGER NOT NULL,
		    test        INTEGER NOT NULL DEFAULT 0,
		    receiver    TEXT,
		    signature   TEXT,
		    doc         TEXT,
		    start_line  INTEGER,
		    end_line    INTEGER,
		    source_file TEXT DEFAULT '',
		    hash        TEXT NOT NULL,
		    UNIQUE(module_id, name, kind, receiver, test, source_file),
		    FOREIGN KEY (module_id) REFERENCES modules(id)
		)`,
		`INSERT INTO definitions_new
		    (id, module_id, name, kind, exported, test, receiver, signature,
		     doc, start_line, end_line, source_file, hash)
		 SELECT id, module_id, name, kind, exported, test, receiver, signature,
		        doc, start_line, end_line, source_file, hash
		 FROM definitions`,
		`DROP TABLE definitions`,
		`ALTER TABLE definitions_new RENAME TO definitions`,
	}
	for _, stmt := range rebuild {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate definitions unique constraint: %w", err)
		}
	}

	fkRows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	var violations int
	for fkRows.Next() {
		violations++
	}
	if cerr := fkRows.Close(); cerr != nil {
		return fmt.Errorf("foreign_key_check: %w", cerr)
	}
	if violations > 0 {
		return fmt.Errorf("migrate definitions unique constraint: foreign_key_check found %d violation(s), aborting", violations)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// DROP TABLE removed every index/trigger bound to the old table
	// object (idx_def_name, idx_def_source_file, definitions_ai/ad/au,
	// etc.) -- they're all CREATE ... IF NOT EXISTS, so reapplying the
	// full schema just recreates the missing ones on the new table.
	if _, err := conn.ExecContext(ctx, sqliteSchemaSQL); err != nil {
		return fmt.Errorf("reapply schema after migration: %w", err)
	}
	return nil
}
