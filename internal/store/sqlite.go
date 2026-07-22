// SQLite backend implementation of store.Backend.
//
// Phase 1 milestone 1: lifecycle + module ops + smoke test. Remaining ~50
// methods are stubbed to return ErrNotImplemented and will be filled in as
// Phase 1 progresses. The compile-time assertion `var _ Backend =
// (*SQLiteDB)(nil)` is intentionally NOT added yet — it gates full
// implementation.
//
// Category A methods (branch, checkout, commit, merge, diff, log, remotes,
// conflicts) are Dolt-specific and are NOT part of the SQLite backend.
// They live only on *DB and will be removed in Phase 4 when Dolt is retired.
// autoCommit() in mcp/server.go becomes a no-op under SQLite because
// writes persist on tx commit — no working-set-to-branch step exists.

package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

//go:embed schema_sqlite.sql
var sqliteSchemaSQL string

// ErrNotImplemented marks Backend methods not yet ported to SQLite.
var ErrNotImplemented = errors.New("sqlite: not yet implemented")

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

	return &SQLiteDB{db: db, path: path}, nil
}

// Close shuts down the connection pool. Safe to call multiple times.
func (s *SQLiteDB) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// Path returns the on-disk path passed to OpenSQLite.
func (s *SQLiteDB) Path() string { return s.path }

// Ping verifies the connection is alive.
func (s *SQLiteDB) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Ctx returns a background context. SQLite has no per-connection session
// state we need to plumb (unlike Dolt's branch-pinned conns).
func (s *SQLiteDB) Ctx() context.Context { return context.Background() }

// Begin starts a transaction. Returns commit + rollback closures matching
// the Backend surface. SQLite's single-writer model means only one write
// transaction is active at a time; readers proceed concurrently under WAL.
func (s *SQLiteDB) Begin() (commit func() error, rollback func(), err error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("sqlite: begin: %w", err)
	}
	return tx.Commit, func() { _ = tx.Rollback() }, nil
}

// CleanTempFiles is a no-op on SQLite. WAL files live alongside the main
// db and are managed by the driver.
func (s *SQLiteDB) CleanTempFiles() {}

// GC runs a WAL checkpoint to fold the -wal file back into the main db.
// This is the SQLite analog of Dolt's noms compaction.
func (s *SQLiteDB) GC() error {
	_, err := s.db.ExecContext(context.Background(), "PRAGMA wal_checkpoint(PASSIVE)")
	if err != nil {
		return fmt.Errorf("sqlite: wal_checkpoint: %w", err)
	}
	return nil
}

// ComputeRootHash returns a canonical hash of the database content. Phase
// 1 followup — stubbed for now; callers use this only for cross-backend
// equivalence tests, which don't run yet.
func (s *SQLiteDB) ComputeRootHash() (string, error) {
	return "", ErrNotImplemented
}

// --- Modules ---

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

func (s *SQLiteDB) GetModuleDefinitions(moduleID int64) ([]Definition, error) {
	rows, err := s.db.QueryContext(s.Ctx(),
		`SELECT id, module_id, name, kind, exported, test,
		        COALESCE(receiver, ''), COALESCE(signature, ''), COALESCE(doc, ''),
		        COALESCE(start_line, 0), COALESCE(end_line, 0),
		        COALESCE(source_file, ''), hash
		 FROM definitions WHERE module_id = ? ORDER BY name`, moduleID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: get module definitions: %w", err)
	}
	defer rows.Close()
	var defs []Definition
	for rows.Next() {
		var d Definition
		if err := rows.Scan(
			&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test,
			&d.Receiver, &d.Signature, &d.Doc,
			&d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash,
		); err != nil {
			return nil, fmt.Errorf("sqlite: scan definition: %w", err)
		}
		defs = append(defs, d)
	}
	return defs, rows.Err()
}

// --- Definitions (minimal reads) ---

func (s *SQLiteDB) CountDefinitions() (int, error) {
	var n int
	err := s.db.QueryRowContext(s.Ctx(),
		`SELECT COUNT(*) FROM definitions`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count definitions: %w", err)
	}
	return n, nil
}

// --- Meta (key-value) ---

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
