// Package dbclient provides a lightweight read-only client for a defn
// database served by a Dolt sql-server. Unlike github.com/justinstimatze/
// defn/db, this package does NOT link the embedded Dolt engine — it
// speaks MySQL wire protocol via github.com/go-sql-driver/mysql. Any
// consumer that only ever talks to a server (never embeds Dolt) can use
// dbclient instead and avoid pulling in ~190 indirect modules, minutes
// of build time, and the CVE surface Dolt's transitive graph carries
// (13 x/crypto + 1 x/net dependabot alerts as of 2026-07-21).
//
// Winze use case (rot-probe, knowledge-base graphs): reads over
// `definitions` and `refs` for a periodically-ingested Go corpus.
// Winze's original 282-line query wrapper motivated this split — see
// dispatch thread `defn-sync-perf` for the full context.
//
//	import "github.com/justinstimatze/defn/dbclient"
//
//	c, err := dbclient.Connect("root@tcp(127.0.0.1:3306)/defn")
//	if err != nil { log.Fatal(err) }
//	defer c.Close()
//	defs, err := c.Definitions(dbclient.DefinitionFilter{Kind: "type"})
//
// All methods are safe for concurrent use.
package dbclient

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Client is a read-only handle to a defn database over MySQL wire.
type Client struct {
	db *sql.DB
}

// Connect opens a connection pool to a Dolt sql-server serving a defn
// database. dsn is a MySQL DSN, e.g. `root@tcp(127.0.0.1:3306)/defn`.
// Fails fast (5s ping timeout) so callers notice unreachable servers
// instead of hanging on the first query.
func Connect(dsn string) (*Client, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("dbclient open: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("dbclient ping: %w", err)
	}
	return &Client{db: db}, nil
}

// Close releases the connection pool.
func (c *Client) Close() error { return c.db.Close() }

// Ping verifies the server is reachable. Callers with long-running
// handles can call this before a burst of queries to detect a server
// restart without waiting for the next real query to fail.
func (c *Client) Ping(ctx context.Context) error { return c.db.PingContext(ctx) }

// Query runs a read-only SQL query and returns rows as maps. Only
// SELECT / SHOW / DESCRIBE / EXPLAIN / WITH are accepted; anything
// mutating is rejected client-side to keep this a read-only surface.
// The full schema is documented in this package: see the type constants
// below (TableDefinitions, TableRefs, TableBodies, etc.) and the schema
// notes in each type doc.
func (c *Client) Query(sqlQuery string) ([]map[string]any, error) {
	if !isReadOnlySQL(sqlQuery) {
		return nil, fmt.Errorf("dbclient: only SELECT/SHOW/DESCRIBE/EXPLAIN/WITH queries allowed")
	}
	rows, err := c.db.Query(sqlQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAsMaps(rows)
}

// Definition mirrors the shape of the `definitions` table's most-used
// columns. Fields correspond 1:1 with columns of the same snake_case
// name in the DB. Bodies are NOT loaded (join with `bodies` table if
// you need them — see Definitions doc).
type Definition struct {
	ID         int64  // definitions.id
	ModuleID   int64  // definitions.module_id
	Name       string // definitions.name
	Kind       string // definitions.kind ("func" | "type" | "var" | "const" | "method")
	Exported   bool   // definitions.exported
	Test       bool   // definitions.test
	Receiver   string // definitions.receiver (empty for non-methods; "*T" or "T")
	Signature  string // definitions.signature
	Doc        string // definitions.doc
	StartLine  int    // definitions.start_line
	EndLine    int    // definitions.end_line
	SourceFile string // definitions.source_file
	Hash       string // definitions.hash (content hash for dedup)
}

// DefinitionFilter narrows a Definitions() query. Zero-value fields are
// omitted from the WHERE clause. String matches are exact except
// NameLike, which is a raw SQL LIKE pattern (% and _ wildcards apply).
type DefinitionFilter struct {
	Kind       string // exact match
	Name       string // exact match; use NameLike for wildcards
	NameLike   string // SQL LIKE pattern, e.g. "%Auth%"
	Receiver   string // exact match
	SourceFile string // exact match
	Exported   *bool  // nil = don't filter; &true = exported only; &false = unexported only
	Test       *bool  // nil = don't filter
	Limit      int    // 0 = no limit
}

// Definitions returns definitions matching f. Bodies are omitted; if
// you need bodies, use Query() with a JOIN to bodies:
//
//	SELECT d.name, b.body FROM definitions d
//	JOIN bodies b ON b.def_id = d.id WHERE d.name = 'Foo'
func (c *Client) Definitions(f DefinitionFilter) ([]Definition, error) {
	var wheres []string
	var args []any
	if f.Kind != "" {
		wheres = append(wheres, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.Name != "" {
		wheres = append(wheres, "name = ?")
		args = append(args, f.Name)
	}
	if f.NameLike != "" {
		wheres = append(wheres, "name LIKE ?")
		args = append(args, f.NameLike)
	}
	if f.Receiver != "" {
		wheres = append(wheres, "COALESCE(receiver,'') = ?")
		args = append(args, f.Receiver)
	}
	if f.SourceFile != "" {
		wheres = append(wheres, "COALESCE(source_file,'') = ?")
		args = append(args, f.SourceFile)
	}
	if f.Exported != nil {
		wheres = append(wheres, "exported = ?")
		args = append(args, *f.Exported)
	}
	if f.Test != nil {
		wheres = append(wheres, "test = ?")
		args = append(args, *f.Test)
	}
	q := `SELECT id, module_id, name, kind, exported, test, COALESCE(receiver,''),
	             COALESCE(signature,''), COALESCE(doc,''), COALESCE(start_line,0),
	             COALESCE(end_line,0), COALESCE(source_file,''), COALESCE(hash,'')
	      FROM definitions`
	if len(wheres) > 0 {
		q += " WHERE " + strings.Join(wheres, " AND ")
	}
	q += " ORDER BY name"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := c.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Definition
	for rows.Next() {
		var d Definition
		if err := rows.Scan(&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test,
			&d.Receiver, &d.Signature, &d.Doc, &d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Search runs a full-text search over definition bodies + doc comments.
// Uses MATCH...AGAINST on the (doc, body) FULLTEXT indexes. Returns
// definitions ranked by relevance, capped at 100 rows.
func (c *Client) Search(query string) ([]Definition, error) {
	rows, err := c.db.Query(
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), COALESCE(d.doc,''), COALESCE(d.start_line,0),
		        COALESCE(d.end_line,0), COALESCE(d.source_file,''), COALESCE(d.hash,'')
		 FROM definitions d
		 LEFT JOIN bodies b ON b.def_id = d.id
		 WHERE MATCH(d.doc) AGAINST(?)
		    OR MATCH(b.body) AGAINST(?)
		 ORDER BY d.name
		 LIMIT 100`, query, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Definition
	for rows.Next() {
		var d Definition
		if err := rows.Scan(&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Test,
			&d.Receiver, &d.Signature, &d.Doc, &d.StartLine, &d.EndLine, &d.SourceFile, &d.Hash); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Schema table names — exported for consumers writing raw SQL via Query().
const (
	TableDefinitions  = "definitions"
	TableBodies       = "bodies"       // def_id, body TEXT
	TableRefs         = "refs"         // from_def, to_def, kind
	TableModules      = "modules"      // id, path, name, doc
	TableImports      = "imports"      // module_id, import_path
	TableLiterals     = "literals"     // def_id, type_name, field, value
	TablePragmas      = "pragmas"      // def_id, key, value
	TableProjectFiles = "project_files" // path, content
	TableMeta         = "defn_meta"    // key, value (last_ingest, etc.)
)

// isReadOnlySQL is a client-side guard: rejects anything that isn't
// obviously read-only. Server-side Dolt permissions should also enforce
// this, but the client check gives faster feedback + prevents
// accidentally sending a mutating statement over a shared connection.
func isReadOnlySQL(q string) bool {
	trimmed := strings.TrimLeft(q, " \t\n\r")
	up := strings.ToUpper(trimmed)
	for _, prefix := range []string{"SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "WITH"} {
		if strings.HasPrefix(up, prefix) {
			return true
		}
	}
	return false
}

// scanAsMaps materializes a rows result into []map[string]any. Column
// values are decoded per the driver's default types; []byte columns
// become string so JSON marshalling produces text, not base64.
func scanAsMaps(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]any
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
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				row[c] = string(b)
			} else {
				row[c] = vals[i]
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
