package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dolthub/dolt/go/store/util/tempfiles"
	_ "github.com/dolthub/driver"
)

// HashBody computes the content hash of a definition body.
func HashBody(body string) string {
	h := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", h)
}

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

// AddRemote adds a named remote pointing to a file path or URL.
func (s *DB) AddRemote(name, url string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_REMOTE('add', ?, ?)", name, url)
	return err
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

// Branch creates a new branch at the current HEAD.
func (s *DB) Branch(name string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_BRANCH(?)", name)
	return err
}

// BranchFrom creates a new branch starting from the given source branch or commit.
func (s *DB) BranchFrom(name, from string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_BRANCH(?, ?)", name, from)
	return err
}

// Checkout switches to a branch.
func (s *DB) Checkout(name string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_CHECKOUT(?)", name)
	return err
}

// CleanTempFiles removes Dolt's accumulated temp files. The embedded Dolt
// storage engine creates UUID-named temp files in /tmp for table persistence
// and manifests. In long-lived processes (defn serve), these leak because
// Clean() is only called on Close(). Call this after write operations.
func (s *DB) CleanTempFiles() {
	tempfiles.MovableTempFileProvider.Clean()
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

// Conflicts returns all unresolved conflicts from the most recent merge.
// Currently focuses on body-level conflicts (the common case); schema and
// metadata conflicts are not surfaced.
func (s *DB) Conflicts() ([]Conflict, error) {
	rows, err := s.queryContext(s.Ctx(), `
		SELECT COALESCE(c.our_def_id, c.their_def_id, c.base_def_id) AS def_id,
		       COALESCE(c.base_body, '') AS base_body,
		       COALESCE(c.our_body, '')  AS our_body,
		       COALESCE(c.their_body,'') AS their_body,
		       d.name, COALESCE(d.receiver, '') AS receiver, d.kind
		FROM dolt_conflicts_bodies c
		LEFT JOIN definitions d
		  ON d.id = COALESCE(c.our_def_id, c.their_def_id, c.base_def_id)
	`)
	if err != nil {
		// Table may not exist when there are no conflicts — treat as empty.
		return nil, nil
	}
	defer rows.Close()

	var out []Conflict
	for rows.Next() {
		var c Conflict
		if err := rows.Scan(&c.DefID, &c.Base, &c.Ours, &c.Theirs, &c.Name, &c.Receiver, &c.Kind); err != nil {
			return nil, fmt.Errorf("scan conflict row: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Ctx returns a background context for database operations.
// All DB methods use this rather than accepting context parameters,
// since the MCP server is single-threaded and cancellation isn't needed.
func (s *DB) Ctx() context.Context {
	return context.Background()
}

// DeleteBranch removes a branch. If force is true, deletes even if unmerged.
func (s *DB) DeleteBranch(name string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := s.execContext(s.Ctx(), "CALL DOLT_BRANCH(?, ?)", flag, name)
	return err
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

// DiffDefinitionsBetween returns definitions that differ between two refs
// (branch names or commit hashes). If to is "" it defaults to WORKING.
func (s *DB) DiffDefinitionsBetween(from, to string) ([]DefDiff, error) {
	if from == "" {
		return nil, fmt.Errorf("DiffDefinitionsBetween: from is required")
	}
	toExpr := "HASHOF(?)"
	qArgs := []any{from, to}
	if to == "" {
		toExpr = "'WORKING'"
		qArgs = []any{from}
	}
	q := fmt.Sprintf(`
		SELECT diff_type,
		       COALESCE(to_name, from_name) AS name,
		       COALESCE(to_kind, from_kind) AS kind,
		       COALESCE(to_receiver, from_receiver, '') AS receiver,
		       COALESCE(from_hash, '') AS from_hash,
		       COALESCE(to_hash, '')   AS to_hash
		FROM dolt_diff_definitions
		WHERE from_commit = HASHOF(?) AND to_commit = %s
	`, toExpr)
	rows, err := s.queryContext(s.Ctx(), q, qArgs...)
	if err != nil {
		return nil, fmt.Errorf("query dolt_diff_definitions: %w", err)
	}
	defer rows.Close()
	var out []DefDiff
	for rows.Next() {
		var d DefDiff
		if err := rows.Scan(&d.DiffType, &d.Name, &d.Kind, &d.Receiver, &d.FromHash, &d.ToHash); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

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

// Fetch fetches from a remote without merging.
func (s *DB) Fetch(remote string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_FETCH(?)", remote)
	return err
}

// FilterDefinitions returns definitions matching optional filters.
// All string filters support SQL LIKE patterns. Empty string means no filter.
// limit caps the number of rows returned; 0 means unlimited.
func (s *DB) FilterDefinitions(name, kind, file string, limit int) ([]Definition, error) {
	q := `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
	        COALESCE(d.signature,''), '', COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
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
	rows, err := s.queryContext(s.Ctx(), q, args...)
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

// GC runs Dolt's garbage collection to compact the noms store.
func (s *DB) GC() error {
	ctx := s.Ctx()
	_, err := s.execContext(ctx, "CALL DOLT_GC()")
	if err != nil {
		return fmt.Errorf("dolt gc: %w", err)
	}
	// DOLT_GC invalidates whatever conn ran it. Swap the pinned conn
	// eagerly so the next read/write lands on a fresh one — otherwise
	// callers would see "this connection was established when..." on
	// queries that have nothing to do with GC. No-op in pool mode.
	if err := s.reacquirePinnedConn(ctx); err != nil {
		return fmt.Errorf("reacquire conn after gc: %w", err)
	}
	s.CleanTempFiles()
	return nil
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

// GetCommentsByPragma returns all comments with the given pragma key.
// pragmaKey supports SQL LIKE patterns (e.g. "go:%").
func (s *DB) GetCommentsByPragma(pragmaKey string) ([]Comment, error) {
	ctx := s.Ctx()
	q := `SELECT c.id, c.def_id, COALESCE(d.name,''), c.source_file, c.line, c.text, c.kind, COALESCE(c.pragma_key,''), COALESCE(c.pragma_value,'')
	      FROM comments c
	      LEFT JOIN definitions d ON c.def_id = d.id
	      WHERE c.pragma_key LIKE ? ORDER BY c.source_file, c.line`
	rows, err := s.queryContext(ctx, q, pragmaKey)
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

// GetCommentsForDef returns all comments associated with a definition.
func (s *DB) GetCommentsForDef(defID int64) ([]Comment, error) {
	ctx := s.Ctx()
	rows, err := s.queryContext(ctx,
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

// GetCurrentBranch returns the active branch name.
func (s *DB) GetCurrentBranch() (string, error) {
	var branch string
	err := s.queryRowContext(s.Ctx(), "SELECT active_branch()").Scan(&branch)
	return branch, err
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

// Ping verifies the database is reachable. Uses the pinned connection
// when present (server mode) so this probe reflects the actual session
// state, falling back to the pool otherwise. Useful for long-running
// clients to detect that a Dolt sql-server has restarted.
//
// On GC invalidation (Dolt killed our conn when it ran DOLT_GC), Ping
// transparently reacquires a fresh pinned conn and retries once, so
// callers see a successful ping whenever the database is actually
// reachable — they don't have to understand the invalidation sentinel.
func (s *DB) Ping(ctx context.Context) error {
	c := s.pinnedConn()
	if c == nil {
		return s.db.PingContext(ctx)
	}
	err := c.PingContext(ctx)
	if err != nil && isGCInvalidation(err) {
		if reErr := s.reacquirePinnedConn(ctx); reErr != nil {
			return reErr
		}
		if c2 := s.pinnedConn(); c2 != nil {
			return c2.PingContext(ctx)
		}
		return s.db.PingContext(ctx)
	}
	return err
}

// GetMeta returns the value for a key, or "" if not set.
func (s *DB) GetMeta(key string) (string, error) {
	ctx := s.Ctx()
	var value string
	err := s.queryRowContext(ctx,
		"SELECT `value` FROM defn_meta WHERE `key` = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return value, nil
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

// GetProjectFile retrieves a project-level file by path.
func (s *DB) GetProjectFile(path string) (string, error) {
	var content string
	err := s.queryRowContext(s.Ctx(),
		"SELECT content FROM project_files WHERE path = ?", path,
	).Scan(&content)
	return content, err
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

// Merge merges a branch into the current branch. If a merge conflict
// occurs, the error is returned but conflict state persists (because
// we enabled dolt_allow_commit_conflicts at session start). Callers can
// inspect Conflicts() and ResolveConflict() to reconcile, or MergeAbort().
func (s *DB) Merge(branchName string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_MERGE(?)", branchName)
	s.CleanTempFiles()
	return err
}

// MergeAbort cancels an in-progress merge and restores the pre-merge state.
func (s *DB) MergeAbort() error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_MERGE('--abort')")
	return err
}

// Path returns the filesystem path of this database.
func (s *DB) Path() string {
	return s.path
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

// Pull pulls from a remote into the current branch.
func (s *DB) Pull(remote, branch string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_PULL(?, ?)", remote, branch)
	return err
}

// Push pushes the current branch to a remote.
func (s *DB) Push(remote, branch string) error {
	_, err := s.execContext(s.Ctx(), "CALL DOLT_PUSH(?, ?)", remote, branch)
	return err
}

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

// QueryLiteralFields searches literal fields. All params are optional filters.
// typeName and fieldValue support SQL LIKE patterns.
// limit caps the number of rows returned; 0 means unlimited.
func (s *DB) QueryLiteralFields(typeName, fieldName, fieldValue string, fieldNames []string, limit int) ([]LiteralField, error) {
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
		placeholders := make([]string, len(fieldNames))
		for i, n := range fieldNames {
			placeholders[i] = "?"
			args = append(args, n)
		}
		q += " AND lf.field_name IN (" + strings.Join(placeholders, ",") + ")"
	}
	if fieldValue != "" {
		q += " AND lf.field_value LIKE ?"
		args = append(args, fieldValue)
	}
	q += " ORDER BY lf.type_name, lf.field_name"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}

	rows, err := s.queryContext(ctx, q, args...)
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

// QueryRefs returns references matching optional filters.
// fromName/toName are definition names (LIKE pattern). kind is exact match.
// limit caps the number of rows returned; 0 means unlimited.
func (s *DB) QueryRefs(fromName, toName, kind string, limit int) ([]Reference, error) {
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
	rows, err := s.queryContext(s.Ctx(), q, args...)
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

// ResolveAll picks a side for every outstanding conflict in bodies and
// definitions. side must be "ours" or "theirs".
func (s *DB) ResolveAll(side string) error {
	if side != "ours" && side != "theirs" {
		return fmt.Errorf("ResolveAll: side must be 'ours' or 'theirs', got %q", side)
	}
	flag := "--" + side
	ctx := s.Ctx()
	if _, err := s.execContext(ctx, "CALL DOLT_CONFLICTS_RESOLVE(?, 'bodies')", flag); err != nil {
		return fmt.Errorf("resolve bodies: %w", err)
	}
	if _, err := s.execContext(ctx, "CALL DOLT_CONFLICTS_RESOLVE(?, 'definitions')", flag); err != nil {
		return fmt.Errorf("resolve definitions: %w", err)
	}
	return nil
}

// ResolveConflict writes body as the resolution for def_id, updates the
// definition's hash, and clears the conflict row. Caller must still Commit
// to finalize the merge.
func (s *DB) ResolveConflict(defID int64, body string) error {
	ctx := s.Ctx()
	if _, err := s.execContext(ctx, "UPDATE bodies SET body = ? WHERE def_id = ?", body, defID); err != nil {
		return fmt.Errorf("update body: %w", err)
	}
	if _, err := s.execContext(ctx, "UPDATE definitions SET hash = ? WHERE id = ?", HashBody(body), defID); err != nil {
		return fmt.Errorf("update hash: %w", err)
	}
	if _, err := s.execContext(ctx,
		"DELETE FROM dolt_conflicts_bodies WHERE our_def_id = ? OR their_def_id = ? OR base_def_id = ?",
		defID, defID, defID); err != nil {
		return fmt.Errorf("clear body conflict: %w", err)
	}
	// Best-effort: any definitions-level conflict row for the same id.
	_, _ = s.execContext(ctx,
		"DELETE FROM dolt_conflicts_definitions WHERE our_id = ? OR their_id = ? OR base_id = ?",
		defID, defID, defID)
	return nil
}

// SearchDefinitions performs full-text search on definition bodies and doc comments.
// Returns definitions ranked by relevance.
func (s *DB) SearchDefinitions(query string) ([]Definition, error) {
	rows, err := s.queryContext(s.Ctx(),
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.test, COALESCE(d.receiver,''),
		        COALESCE(d.signature,''), '', COALESCE(d.doc,''), COALESCE(d.start_line,0), COALESCE(d.end_line,0), COALESCE(d.source_file,''), d.hash
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
	return scanDefinitions(rows)
}

// SetFileComments replaces all comments for a source file.
func (s *DB) SetFileComments(sourceFile string, comments []Comment) error {
	ctx := s.Ctx()
	if _, err := s.execContext(ctx, "DELETE FROM comments WHERE source_file = ?", sourceFile); err != nil {
		return fmt.Errorf("clear comments: %w", err)
	}
	for _, c := range comments {
		if _, err := s.execContext(ctx,
			`INSERT INTO comments (def_id, source_file, line, text, kind, pragma_key, pragma_value)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			c.DefID, sourceFile, c.Line, c.Text, c.Kind, c.PragmaKey, c.PragmaVal,
		); err != nil {
			return fmt.Errorf("insert comment: %w", err)
		}
	}
	return nil
}

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

// SetLiteralFields replaces all literal fields for a definition.
func (s *DB) SetLiteralFields(defID int64, fields []LiteralField) error {
	ctx := s.Ctx()
	if _, err := s.execContext(ctx, "DELETE FROM literal_fields WHERE def_id = ?", defID); err != nil {
		return fmt.Errorf("clear literal_fields: %w", err)
	}
	for _, f := range fields {
		if _, err := s.execContext(ctx,
			`INSERT INTO literal_fields (def_id, type_name, field_name, field_value, line)
			 VALUES (?, ?, ?, ?, ?)`,
			defID, f.TypeName, f.FieldName, f.FieldValue, f.Line,
		); err != nil {
			return fmt.Errorf("insert literal_field: %w", err)
		}
	}
	return nil
}

// SetMeta upserts a key/value pair into the defn_meta table.
func (s *DB) SetMeta(key, value string) error {
	ctx := s.Ctx()
	_, err := s.execContext(ctx,
		"REPLACE INTO defn_meta (`key`, `value`) VALUES (?, ?)", key, value)
	if err != nil {
		return fmt.Errorf("set meta %q: %w", key, err)
	}
	return nil
}

// SetProjectFile stores a project-level file (go.mod, go.sum, etc.).
func (s *DB) SetProjectFile(path, content string) error {
	_, err := s.execContext(s.Ctx(),
		`REPLACE INTO project_files (path, content) VALUES (?, ?)`, path, content)
	return err
}

// SetFileSource stores the raw Go source for a single file. Called by
// ingest with the on-disk bytes, and by emit after writing a merged
// version back to disk so the two stay in sync.
//
// DELETE+INSERT rather than REPLACE INTO to dodge the dolthub/dolt#10882
// FULLTEXT bug in case we add a fulltext index on raw in the future.
func (s *DB) SetFileSource(moduleID int64, sourceFile, raw string) error {
	ctx := s.Ctx()
	hash := HashBody(raw)
	if _, err := s.execContext(ctx,
		`DELETE FROM file_sources WHERE module_id = ? AND source_file = ?`,
		moduleID, sourceFile); err != nil {
		return fmt.Errorf("clear file_sources row: %w", err)
	}
	if _, err := s.execContext(ctx,
		`INSERT INTO file_sources (module_id, source_file, raw, file_hash) VALUES (?, ?, ?, ?)`,
		moduleID, sourceFile, raw, hash); err != nil {
		return fmt.Errorf("insert file_sources: %w", err)
	}
	return nil
}

// GetFileSource returns the raw Go source for (moduleID, sourceFile).
// Returns sql.ErrNoRows if there's no row yet (file has never been ingested).
func (s *DB) GetFileSource(moduleID int64, sourceFile string) (string, error) {
	var raw string
	err := s.queryRowContext(s.Ctx(),
		`SELECT raw FROM file_sources WHERE module_id = ? AND source_file = ?`,
		moduleID, sourceFile).Scan(&raw)
	return raw, err
}

// ListFileSources returns all (source_file, raw) pairs for a module.
func (s *DB) ListFileSources(moduleID int64) (map[string]string, error) {
	rows, err := s.queryContext(s.Ctx(),
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

// Traverse performs a BFS traversal of the reference graph.
// direction: "callers" (who references me) or "callees" (what I reference).
// refKinds: filter by ref kind (empty = all). maxDepth: BFS depth cap (0 = default 10, max 50).
func (s *DB) Traverse(startID int64, direction string, refKinds []string, maxDepth int) ([]TraverseResult, error) {
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

	// Get the start definition's name for path building.
	if d, err := s.GetDefinition(startID); err == nil {
		name := d.Name
		if d.Receiver != "" {
			name = "(" + d.Receiver + ")." + d.Name
		}
		nameOf[startID] = name
	}

	// Build kind filter clause.
	kindClause := ""
	var kindArgs []any
	if len(refKinds) > 0 {
		placeholders := make([]string, len(refKinds))
		for i, k := range refKinds {
			placeholders[i] = "?"
			kindArgs = append(kindArgs, k)
		}
		kindClause = " AND r.kind IN (" + strings.Join(placeholders, ",") + ")"
	}

	var results []TraverseResult
	frontier := []int64{startID}

	for depth := 1; depth <= maxDepth && len(frontier) > 0; depth++ {
		// Build IN clause for current frontier.
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

		rows, err := s.queryContext(ctx, q, args...)
		if err != nil {
			return results, fmt.Errorf("traverse depth %d: %w", depth, err)
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

			// Build path from root.
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

			results = append(results, TraverseResult{
				Definition: d,
				Depth:      depth,
				Path:       path,
			})
			nextFrontier = append(nextFrontier, d.ID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return results, err
		}

		// Safety cap: stop expanding if a level is too wide.
		if len(nextFrontier) > 1000 {
			break
		}
		frontier = nextFrontier
	}

	return results, nil
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
		`UPDATE bodies SET body = ? WHERE def_id = ?`,
		d.Body, existingID,
	); err != nil {
		return 0, fmt.Errorf("update body: %w", err)
	}
	return existingID, nil
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

// execContext runs ExecContext on the pinned connection (embedded) or pool (MySQL).
//
// Writes are NOT retried automatically on GC-invalidation: if the call landed
// on the server before the error came back, retrying would double-apply. We
// surface the error and let the caller decide. Proactive swap in GC() avoids
// this path in practice for defn's own GC.
func (s *DB) execContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if conn := s.pinnedConn(); conn != nil {
		return conn.ExecContext(ctx, query, args...)
	}
	return s.db.ExecContext(ctx, query, args...)
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

// queryContext runs QueryContext on the pinned connection (embedded) or pool (MySQL).
// Reads are safe to retry — if the first attempt hit a GC-invalidated conn,
// no state was mutated. Retries at most once to avoid pathological loops.
func (s *DB) queryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	conn := s.pinnedConn()
	if conn == nil {
		return s.db.QueryContext(ctx, query, args...)
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil && isGCInvalidation(err) {
		if reErr := s.reacquirePinnedConn(ctx); reErr == nil {
			if conn = s.pinnedConn(); conn != nil {
				return conn.QueryContext(ctx, query, args...)
			}
		}
	}
	return rows, err
}

// queryRowContext runs QueryRowContext on the pinned connection (embedded) or pool (MySQL).
// Unlike queryContext, *sql.Row defers error reporting until Scan, so we
// probe for invalidation via a quick ping before issuing the query. The
// extra roundtrip is cheap (micro-sec on embedded) and avoids a surprising
// error buried in Scan().
func (s *DB) queryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	conn := s.pinnedConn()
	if conn == nil {
		return s.db.QueryRowContext(ctx, query, args...)
	}
	// Cheap health probe. If the conn was killed by GC, PingContext surfaces
	// the invalidation error and we can recover before the caller scans.
	if err := conn.PingContext(ctx); err != nil && isGCInvalidation(err) {
		if reErr := s.reacquirePinnedConn(ctx); reErr == nil {
			if c := s.pinnedConn(); c != nil {
				conn = c
			}
		}
	}
	return conn.QueryRowContext(ctx, query, args...)
}

// Comment represents a comment or pragma extracted from Go source.
type Comment struct {
	ID         int64
	DefID      *int64 // nil for file-level comments
	DefName    string // name of associated definition (empty if file-level)
	SourceFile string
	Line       int
	Text       string
	Kind       string // "doc", "line", "block", "pragma"
	PragmaKey  string // e.g. "go:generate", "winze:contested"
	PragmaVal  string // rest of line after pragma directive
}

// Conflict describes one definition whose body differs between the
// current branch (ours) and the branch being merged (theirs), with the
// common-ancestor body (base).
type Conflict struct {
	DefID    int64
	Name     string
	Receiver string
	Kind     string
	Base     string
	Ours     string
	Theirs   string
}

// DB wraps a Dolt connection with code-database operations.
// DB is NOT safe for concurrent use from multiple goroutines.
type DB struct {
	db     *sql.DB
	conn   *sql.Conn // pinned connection for branch-state consistency
	connMu sync.Mutex // guards conn swap across GC-invalidation recovery
	path   string     // filesystem path to database directory
	dbName string     // database name (for USE db/branch)
}

// pinnedConn returns the current pinned connection (may be nil in pool mode).
// Holds the mutex only long enough to read the pointer; the caller uses
// the returned *sql.Conn which is itself internally serialized by database/sql.
func (s *DB) pinnedConn() *sql.Conn {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.conn
}

// isGCInvalidation reports whether err is Dolt's
// "this connection was established when this server performed an online
// garbage collection" — fired on the next query against a conn that was
// alive when DOLT_GC ran.
func isGCInvalidation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "performed an online garbage collection") ||
		strings.Contains(msg, "this connection was established when")
}

// reacquirePinnedConn tears down the current pinned conn and opens a fresh
// one, replaying the session-level setup (database selection, branch pin,
// allow-commit-conflicts). Safe to call from any goroutine.
//
// This is how we recover from DOLT_GC invalidating the pinned conn: after
// GC runs, the old conn is dead; proactively swap before the next query
// surfaces the error.
func (s *DB) reacquirePinnedConn(ctx context.Context) error {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	if s.conn == nil {
		// Pool mode — nothing pinned, nothing to swap.
		return nil
	}

	newConn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire replacement conn: %w", err)
	}

	// Replay open-time session setup. Order matches openEmbedded / openMySQL.
	if s.dbName != "" {
		if _, err := newConn.ExecContext(ctx, fmt.Sprintf("USE `%s`", s.dbName)); err != nil {
			newConn.Close()
			return fmt.Errorf("re-select database: %w", err)
		}
	}
	if branch := strings.TrimSpace(os.Getenv("DEFN_BRANCH")); branch != "" {
		if _, err := newConn.ExecContext(ctx, "CALL DOLT_CHECKOUT(?)", branch); err != nil {
			newConn.Close()
			return fmt.Errorf("re-pin to branch %q: %w", branch, err)
		}
	}
	// Best-effort: conflict persistence is a nice-to-have, not a correctness
	// requirement, so log but don't fail.
	if _, err := newConn.ExecContext(ctx, "SET @@dolt_allow_commit_conflicts = 1"); err != nil {
		fmt.Fprintf(os.Stderr, "defn: warning: could not re-enable dolt_allow_commit_conflicts: %v\n", err)
	}

	old := s.conn
	s.conn = newConn
	old.Close() // old conn was invalidated; closing is a no-op on the server side
	return nil
}

// DefDiff is a single changed definition between two refs. Bodies are
// omitted (callers can fetch via code(op:"read") after a checkout, or
// via SELECT body FROM bodies AS OF HASHOF(...)).
type DefDiff struct {
	DiffType string // "added", "removed", "modified"
	Name     string
	Kind     string
	Receiver string
	FromHash string
	ToHash   string
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

// Import represents an import recorded for a module.
type Import struct {
	ModuleID     int64
	ImportedPath string
	Alias        string
}

// LiteralField represents a field in a composite literal (e.g. Config{Field: "val"}).
type LiteralField struct {
	ID         int64
	DefID      int64  // definition containing the literal
	DefName    string // name of containing definition
	TypeName   string // fully qualified type (e.g. "github.com/foo/bar.Config")
	FieldName  string
	FieldValue string // source text of the value
	Line       int
}

// Module represents a Go package/module in the database.
type Module struct {
	ID   int64
	Path string
	Name string
	Doc  string
}

type Mutation struct {
	Type     string `json:"type"` // signature-change, behavior-change, removal, addition, one-shot-stub
	Name     string `json:"name"`
	Receiver string `json:"receiver,omitempty"`
}

// Reference represents a reference from one definition to another.
type Reference struct {
	FromDef int64
	ToDef   int64
	Kind    string
}

type SimulationResult struct {
	Steps           []SimulationStep `json:"steps"`
	TotalMutations  int              `json:"total_mutations"`
	CombinedCallers int              `json:"combined_callers"`
	CombinedTests   int              `json:"combined_tests"`
	TestDensity     float64          `json:"test_density"`
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

// TraverseResult holds a definition found during graph traversal with its depth and path.
type TraverseResult struct {
	Definition Definition
	Depth      int
	Path       []string // definition names from root to this node
}

//go:embed schema.sql
var schemaSQL string
