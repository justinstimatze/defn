// Open helpers: DSN routing, schema init, and one-time data migrations.
// Split out from store.go to keep that file focused on DB operations.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

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
		if err := execSchemaStmt(ctx, db, stmt); err != nil {
			return err
		}
	}
	return nil
}

func execSchemaStmt(ctx context.Context, db execQuerier, stmt string) error {
	if strings.TrimSpace(stmt) == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, stmt)
	if err == nil {
		return nil
	}
	errMsg := strings.ToLower(err.Error())
	if strings.Contains(errMsg, "already exists") || strings.Contains(errMsg, "duplicate") {
		return nil
	}
	return fmt.Errorf("init schema: %w\nstatement: %s", err, stmt)
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
	return splitSQLStatements(stripSQLComments(s))
}

func stripSQLComments(s string) string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func splitSQLStatements(s string) []string {
	var stmts []string
	for part := range strings.SplitSeq(s, ";") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			stmts = append(stmts, trimmed)
		}
	}
	return stmts
}
