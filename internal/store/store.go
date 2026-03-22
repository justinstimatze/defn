// Package store manages the SQLite database that holds all code definitions,
// references, and version history.
package store

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed schema.sql
var schemaSQL string

// DB wraps a SQLite connection with code-database operations.
type DB struct {
	db *sql.DB
}

// Open opens or creates a defn database at the given path.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// WAL mode for better concurrent read performance.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable FK: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &DB{db: db}, nil
}

// Close closes the database.
func (s *DB) Close() error {
	return s.db.Close()
}

// Module represents a Go package/module in the database.
type Module struct {
	ID   int64
	Path string
	Name string
}

// Definition represents a single Go definition (function, type, method, etc.).
type Definition struct {
	ID        int64
	ModuleID  int64
	Name      string
	Kind      string
	Exported  bool
	Receiver  string
	Signature string
	Body      string
	Doc       string
	StartLine int
	EndLine   int
	Hash      string
}

// Reference represents a reference from one definition to another.
type Reference struct {
	FromDef int64
	ToDef   int64
	Kind    string
}

// EnsureModule creates or returns an existing module.
func (s *DB) EnsureModule(path, name string) (*Module, error) {
	var m Module
	err := s.db.QueryRow(
		"SELECT id, path, name FROM modules WHERE path = ?", path,
	).Scan(&m.ID, &m.Path, &m.Name)
	if err == sql.ErrNoRows {
		res, err := s.db.Exec(
			"INSERT INTO modules (path, name) VALUES (?, ?)", path, name,
		)
		if err != nil {
			return nil, fmt.Errorf("insert module: %w", err)
		}
		m.ID, _ = res.LastInsertId()
		m.Path = path
		m.Name = name
		return &m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query module: %w", err)
	}
	return &m, nil
}

// HashBody computes the content hash of a definition body.
func HashBody(body string) string {
	h := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", h)
}

// UpsertDefinition inserts or updates a definition, returning its ID.
// If the hash is unchanged, it's a no-op.
func (s *DB) UpsertDefinition(d *Definition) (int64, error) {
	d.Hash = HashBody(d.Body)

	var existingID int64
	var existingHash string
	err := s.db.QueryRow(
		`SELECT id, hash FROM definitions
		 WHERE module_id = ? AND name = ? AND kind = ? AND COALESCE(receiver,'') = COALESCE(?,'')`,
		d.ModuleID, d.Name, d.Kind, d.Receiver,
	).Scan(&existingID, &existingHash)

	if err == sql.ErrNoRows {
		res, err := s.db.Exec(
			`INSERT INTO definitions
			 (module_id, name, kind, exported, receiver, signature, body, doc, start_line, end_line, hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ModuleID, d.Name, d.Kind, d.Exported, d.Receiver,
			d.Signature, d.Body, d.Doc, d.StartLine, d.EndLine, d.Hash,
		)
		if err != nil {
			return 0, fmt.Errorf("insert definition: %w", err)
		}
		id, _ := res.LastInsertId()
		return id, nil
	}
	if err != nil {
		return 0, fmt.Errorf("query definition: %w", err)
	}

	// Content unchanged — skip.
	if existingHash == d.Hash {
		return existingID, nil
	}

	_, err = s.db.Exec(
		`UPDATE definitions
		 SET exported=?, signature=?, body=?, doc=?, start_line=?, end_line=?,
		     hash=?, modified_at=datetime('now')
		 WHERE id=?`,
		d.Exported, d.Signature, d.Body, d.Doc,
		d.StartLine, d.EndLine, d.Hash, existingID,
	)
	if err != nil {
		return 0, fmt.Errorf("update definition: %w", err)
	}
	return existingID, nil
}

// SetReferences replaces all references from a given definition.
func (s *DB) SetReferences(fromDef int64, refs []Reference) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM references WHERE from_def = ?", fromDef); err != nil {
		return fmt.Errorf("clear refs: %w", err)
	}
	for _, r := range refs {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO references (from_def, to_def, kind) VALUES (?, ?, ?)",
			fromDef, r.ToDef, r.Kind,
		); err != nil {
			return fmt.Errorf("insert ref: %w", err)
		}
	}
	return tx.Commit()
}

// GetDefinition returns a definition by ID.
func (s *DB) GetDefinition(id int64) (*Definition, error) {
	d := &Definition{}
	err := s.db.QueryRow(
		`SELECT id, module_id, name, kind, exported, receiver, signature, body, doc,
		        start_line, end_line, hash
		 FROM definitions WHERE id = ?`, id,
	).Scan(&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Receiver,
		&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.Hash)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// FindDefinitions searches definitions by name pattern (SQL LIKE).
func (s *DB) FindDefinitions(namePattern string) ([]Definition, error) {
	rows, err := s.db.Query(
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.receiver,
		        d.signature, d.body, d.doc, d.start_line, d.end_line, d.hash
		 FROM definitions d
		 WHERE d.name LIKE ?
		 ORDER BY d.name`, namePattern,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitions(rows)
}

// GetDefinitionByName returns a definition by exact name and optional module path.
func (s *DB) GetDefinitionByName(name, modulePath string) (*Definition, error) {
	query := `SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.receiver,
	                 d.signature, d.body, d.doc, d.start_line, d.end_line, d.hash
	          FROM definitions d`
	var args []any

	if modulePath != "" {
		query += " JOIN modules m ON d.module_id = m.id WHERE d.name = ? AND m.path = ?"
		args = append(args, name, modulePath)
	} else {
		query += " WHERE d.name = ?"
		args = append(args, name)
	}

	d := &Definition{}
	err := s.db.QueryRow(query, args...).Scan(
		&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Receiver,
		&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.Hash,
	)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// GetCallers returns all definitions that reference the given definition.
func (s *DB) GetCallers(defID int64) ([]Definition, error) {
	rows, err := s.db.Query(
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.receiver,
		        d.signature, d.body, d.doc, d.start_line, d.end_line, d.hash
		 FROM definitions d
		 JOIN references r ON r.from_def = d.id
		 WHERE r.to_def = ?
		 ORDER BY d.name`, defID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitions(rows)
}

// GetCallees returns all definitions referenced by the given definition.
func (s *DB) GetCallees(defID int64) ([]Definition, error) {
	rows, err := s.db.Query(
		`SELECT d.id, d.module_id, d.name, d.kind, d.exported, d.receiver,
		        d.signature, d.body, d.doc, d.start_line, d.end_line, d.hash
		 FROM definitions d
		 JOIN references r ON r.to_def = d.id
		 WHERE r.from_def = ?
		 ORDER BY d.name`, defID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitions(rows)
}

// GetModuleDefinitions returns all definitions in a module.
func (s *DB) GetModuleDefinitions(moduleID int64) ([]Definition, error) {
	rows, err := s.db.Query(
		`SELECT id, module_id, name, kind, exported, receiver,
		        signature, body, doc, start_line, end_line, hash
		 FROM definitions
		 WHERE module_id = ?
		 ORDER BY kind, name`, moduleID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitions(rows)
}

// ListModules returns all modules.
func (s *DB) ListModules() ([]Module, error) {
	rows, err := s.db.Query("SELECT id, path, name FROM modules ORDER BY path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var modules []Module
	for rows.Next() {
		var m Module
		if err := rows.Scan(&m.ID, &m.Path, &m.Name); err != nil {
			return nil, err
		}
		modules = append(modules, m)
	}
	return modules, rows.Err()
}

// ComputeRootHash computes a merkle-like root hash of all definitions.
func (s *DB) ComputeRootHash() (string, error) {
	rows, err := s.db.Query("SELECT hash FROM definitions ORDER BY id")
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

// Query executes a raw SQL query and returns results as maps.
func (s *DB) Query(query string) ([]map[string]any, error) {
	rows, err := s.db.Query(query)
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

func scanDefinitions(rows *sql.Rows) ([]Definition, error) {
	var defs []Definition
	for rows.Next() {
		var d Definition
		if err := rows.Scan(
			&d.ID, &d.ModuleID, &d.Name, &d.Kind, &d.Exported, &d.Receiver,
			&d.Signature, &d.Body, &d.Doc, &d.StartLine, &d.EndLine, &d.Hash,
		); err != nil {
			return nil, err
		}
		defs = append(defs, d)
	}
	return defs, rows.Err()
}
