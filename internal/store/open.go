// Open helpers: DSN routing, schema init, and one-time data migrations.
// Split out from store.go to keep that file focused on DB operations.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
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

// pingTimeout caps how long we'll wait for a successful MySQL/Dolt
// handshake on initial Open. Silent hangs are the worst UX; 5s is long
// enough for a cold Dolt on slow hardware, short enough that a user
// will notice and retry.
const pingTimeout = 5 * time.Second

// bootRetries / bootBackoff: when TCP reaches the port but the handshake
// fails (the "Dolt is booting" signal), retry a few times with a short
// backoff before giving up. A restart window is typically <3s.
const (
	bootRetries = 3
	bootBackoff = 1 * time.Second
)

func openMySQL(dsn string) (*DB, error) {
	db, err := sql.Open("mysql", injectTimeouts(dsn))
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	if err := pingWithBootRetry(db, dsn); err != nil {
		db.Close()
		return nil, err
	}
	ctx := context.Background()
	if err := prepareSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	s := &DB{db: db, path: dsn}

	// If DEFN_BRANCH is set, pin the session to that branch.
	// We grab a dedicated conn and run CALL DOLT_CHECKOUT — the checkout
	// state is connection-local, so all subsequent ops must go through
	// this conn. Reuses the existing pinned-conn machinery in DB.
	if branch := strings.TrimSpace(os.Getenv("DEFN_BRANCH")); branch != "" {
		conn, err := db.Conn(ctx)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("acquire conn for branch pin: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branch); err != nil {
			conn.Close()
			db.Close()
			return nil, fmt.Errorf("pin session to branch %q: %w", branch, err)
		}
		s.conn = conn
	}
	return s, nil
}

// injectTimeouts adds timeout, readTimeout, and writeTimeout params to
// a MySQL DSN unless already present. Prevents individual queries from
// hanging forever if the server becomes unresponsive after connecting.
func injectTimeouts(dsn string) string {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return dsn // let sql.Open surface the parse error
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = pingTimeout
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 30 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	return cfg.FormatDSN()
}

// pingWithBootRetry issues PingContext with a deadline. If TCP is
// reachable but the handshake fails (driver returns an error fast
// rather than timing out at the TCP layer), we assume the server is
// booting and retry a few times. On terminal failure, returns an
// actionable error pointing at dolt's CLI.
func pingWithBootRetry(db *sql.DB, dsn string) error {
	host := dsnHost(dsn)
	var lastErr error
	for attempt := range bootRetries {
		ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
		err := db.PingContext(ctx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		// Only retry if TCP is reachable (port open but handshake failing).
		// If the port is closed or the dial times out, retrying won't help.
		if host == "" || !tcpReachable(host) {
			break
		}
		if attempt < bootRetries-1 {
			time.Sleep(bootBackoff)
		}
	}
	return fmt.Errorf(
		"connect to dolt server at %s: %w\n  is the server running? try: dolt sql-server status\n  or fall back to embedded: unset DEFN_DSN",
		displayHost(host, dsn), lastErr)
}

// dsnHost extracts host:port from a MySQL DSN (e.g. "127.0.0.1:3307").
// Returns "" if the DSN doesn't use tcp() or can't be parsed.
func dsnHost(dsn string) string {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || cfg.Net != "tcp" {
		return ""
	}
	return cfg.Addr
}

func displayHost(host, dsn string) string {
	if host != "" {
		return host
	}
	return dsn
}

// tcpReachable returns true if we can establish a TCP connection to
// host within a short timeout — distinguishes "server booting, keep
// trying" from "nothing listening, give up".
func tcpReachable(host string) bool {
	conn, err := net.DialTimeout("tcp", host, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func openEmbedded(path string) (*DB, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("abs path: %w", err)
	}

	// We always use the nested layout: <absPath>/defn/.dolt/. Using absPath
	// itself as Dolt's Directory scopes its database scan to a dir we own,
	// which avoids an upstream panic when unrelated sibling .dolt/ dirs
	// in the project root have a corrupt or missing manifest (see
	// https://github.com/dolthub/dolt : NomsBlockStore.Close + empty
	// nbfVers in FatalBehaviorCrash mode).
	const dbName = "defn"
	if err := os.MkdirAll(absPath, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	if err := migrateFlatLayout(absPath, dbName); err != nil {
		return nil, fmt.Errorf("migrate layout: %w", err)
	}

	dsn := fmt.Sprintf("file://%s?commitname=defn&commitemail=defn@localhost",
		filepath.ToSlash(absPath))

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

// migrateFlatLayout rewrites a pre-April-2026 defn database from
// <path>/.dolt/ to <path>/defn/.dolt/ so it's loadable under the
// nested layout (see openEmbedded for why).
func migrateFlatLayout(absPath, dbName string) error {
	flatDolt := filepath.Join(absPath, ".dolt")
	nestedRoot := filepath.Join(absPath, dbName)
	nestedDolt := filepath.Join(nestedRoot, ".dolt")

	if _, err := os.Stat(nestedDolt); err == nil {
		return nil // already migrated
	}
	if _, err := os.Stat(flatDolt); err != nil {
		return nil // no flat layout to migrate
	}
	if err := os.MkdirAll(nestedRoot, 0755); err != nil {
		return err
	}
	if err := os.Rename(flatDolt, nestedDolt); err != nil {
		return fmt.Errorf("rename %s → %s: %w", flatDolt, nestedDolt, err)
	}
	return nil
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
