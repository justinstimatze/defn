// Package db provides read-only Go access to a defn database.
//
// The database must already exist (created by defn init). This package
// opens it directly using the embedded Dolt engine — no server or CLI
// binary needed.
//
//	import defndb "github.com/justinstimatze/defn/db"
//
//	d, err := defndb.Open(".defn")
//	defer d.Close()
//	defs, err := d.Definitions(defndb.DefinitionFilter{Kind: "type"})
//
// All methods are safe for concurrent use from multiple goroutines.
package db

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/justinstimatze/defn/internal/store"
)

// DB is a read-only handle to a defn database.
type DB struct {
	mu sync.Mutex
	s  *store.DB
}

// Open opens a defn database at the given path (e.g. ".defn").
// The database must already exist (created by defn init).
// Also accepts a MySQL DSN (containing "@") to connect to a running
// dolt sql-server.
func Open(path string) (*DB, error) {
	s, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	return &DB{s: s}, nil
}

// Close releases database resources.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.s.Close()
}

// Query executes a read-only SQL query and returns rows as maps.
// Only SELECT, SHOW, DESCRIBE, EXPLAIN, and WITH (CTE) queries are allowed.
func (db *DB) Query(sql string) ([]map[string]any, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.s.Query(sql)
}

// --- Definitions ---

// Definition represents a Go definition (function, type, var, const, etc.).
type Definition struct {
	ID         int64
	ModuleID   int64
	Name       string
	Kind       string // "function", "method", "type", "interface", "var", "const"
	Exported   bool
	Test       bool
	Receiver   string
	Signature  string
	Doc        string
	StartLine  int
	EndLine    int
	SourceFile string
}

// DefinitionFilter controls which definitions are returned.
// All fields are optional. String fields support SQL LIKE patterns.
type DefinitionFilter struct {
	Name string // LIKE pattern (e.g. "%Handler%")
	Kind string // exact: "function", "method", "type", "interface", "var", "const"
	File string // LIKE pattern on source_file (e.g. "%server.go")
}

// Definitions returns definitions matching the filter.
func (db *DB) Definitions(f DefinitionFilter) ([]Definition, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	defs, err := db.s.FilterDefinitions(f.Name, f.Kind, f.File, 0)
	if err != nil {
		return nil, err
	}
	return convertDefs(defs), nil
}

// DefinitionByID returns a single definition by its ID.
func (db *DB) DefinitionByID(id int64) (*Definition, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	d, err := db.s.GetDefinition(id)
	if err != nil {
		return nil, err
	}
	out := convertDef(*d)
	return &out, nil
}

// Search runs a FULLTEXT search across definition docs and bodies.
func (db *DB) Search(query string) ([]Definition, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	defs, err := db.s.SearchDefinitions(query)
	if err != nil {
		return nil, err
	}
	return convertDefs(defs), nil
}

// --- Literal Fields ---

// LiteralField represents a field in a composite literal (e.g. Config{Field: "val"}).
type LiteralField struct {
	ID         int64
	DefID      int64  // definition containing the literal
	DefName    string // name of containing definition
	TypeName   string // fully qualified (e.g. "github.com/foo/bar.Config")
	FieldName  string
	FieldValue string // source text of the value expression
	Line       int
}

// LiteralFieldFilter controls which literal fields are returned.
// All fields are optional. TypeName and Value support SQL LIKE patterns.
type LiteralFieldFilter struct {
	TypeName   string   // LIKE pattern (e.g. "%Claim%")
	FieldName  string   // exact match (e.g. "Subject") — mutually exclusive with FieldNames
	FieldNames []string // IN match (e.g. []string{"Subject", "Object", "Prov"})
	Value      string   // LIKE pattern on field_value
}

// LiteralFields returns composite literal fields matching the filter.
func (db *DB) LiteralFields(f LiteralFieldFilter) ([]LiteralField, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	fields, err := db.s.QueryLiteralFields(f.TypeName, f.FieldName, f.Value, f.FieldNames, 0)
	if err != nil {
		return nil, err
	}
	out := make([]LiteralField, len(fields))
	for i, sf := range fields {
		out[i] = LiteralField{
			ID: sf.ID, DefID: sf.DefID, DefName: sf.DefName, TypeName: sf.TypeName,
			FieldName: sf.FieldName, FieldValue: sf.FieldValue, Line: sf.Line,
		}
	}
	return out, nil
}

// --- Pragmas ---

// Pragma represents a comment pragma (e.g. //go:generate, //winze:contested).
type Pragma struct {
	ID         int64
	DefID      *int64 // nil for file-level pragmas
	DefName    string // name of associated definition (empty if file-level)
	SourceFile string
	Line       int
	Text       string // full comment text
	Key        string // e.g. "go:generate", "winze:contested"
	Value      string // rest of line after the key
}

// Pragmas returns pragma comments matching the key pattern.
// keyPattern supports SQL LIKE (e.g. "winze:%" for all winze pragmas).
func (db *DB) Pragmas(keyPattern string) ([]Pragma, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	comments, err := db.s.GetCommentsByPragma(keyPattern)
	if err != nil {
		return nil, err
	}
	out := make([]Pragma, len(comments))
	for i, c := range comments {
		out[i] = Pragma{
			ID: c.ID, DefID: c.DefID, DefName: c.DefName, SourceFile: c.SourceFile,
			Line: c.Line, Text: c.Text, Key: c.PragmaKey, Value: c.PragmaVal,
		}
	}
	return out, nil
}

// --- References ---

// Ref represents a reference edge between two definitions.
type Ref struct {
	FromDef int64
	ToDef   int64
	Kind    string // "call", "type_ref", "field_access", "constructor", "embed", "interface_dispatch", "implements"
}

// RefFilter controls which references are returned.
// All fields are optional. Name fields support SQL LIKE patterns.
type RefFilter struct {
	FromName string // LIKE pattern on source definition name
	ToName   string // LIKE pattern on target definition name
	Kind     string // exact match (e.g. "embed", "call")
}

// Refs returns references matching the filter.
func (db *DB) Refs(f RefFilter) ([]Ref, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	refs, err := db.s.QueryRefs(f.FromName, f.ToName, f.Kind, 0)
	if err != nil {
		return nil, err
	}
	out := make([]Ref, len(refs))
	for i, r := range refs {
		out[i] = Ref{FromDef: r.FromDef, ToDef: r.ToDef, Kind: r.Kind}
	}
	return out, nil
}

// --- Traversal ---

// TraverseResult holds a definition found during graph traversal.
type TraverseResult struct {
	Definition Definition
	Depth      int
	Path       []string // definition names from root to this node
}

// Traverse performs a BFS traversal of the reference graph.
// direction: "callers" (who references me) or "callees" (what I reference).
// refKinds filters by ref kind (nil = all). maxDepth caps BFS depth (0 = default 10, max 50).
func (db *DB) Traverse(name, direction string, refKinds []string, maxDepth int) ([]TraverseResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	d, err := db.s.GetDefinitionByName(name, "")
	if err != nil {
		return nil, err
	}
	results, err := db.s.Traverse(d.ID, direction, refKinds, maxDepth)
	if err != nil {
		return nil, err
	}
	out := make([]TraverseResult, len(results))
	for i, r := range results {
		out[i] = TraverseResult{
			Definition: convertDef(r.Definition),
			Depth:      r.Depth,
			Path:       r.Path,
		}
	}
	return out, nil
}

// --- Staleness ---

// StaleFiles returns paths of .go files under projectDir that have been
// modified more recently than the last ingest. Empty slice means the
// database is in sync with the filesystem (or no ingest timestamp has
// been recorded — e.g. databases created before this feature).
//
// Walks projectDir recursively, skipping .defn/, .git/, vendor/,
// node_modules/, and testdata/ directories.
func (db *DB) StaleFiles(projectDir string) ([]string, error) {
	db.mu.Lock()
	lastIngestStr, err := db.s.GetMeta("last_ingest")
	db.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if lastIngestStr == "" {
		return nil, nil
	}
	lastIngest, err := strconv.ParseInt(lastIngestStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse last_ingest: %w", err)
	}

	var stale []string
	err = filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".defn" || name == ".git" || name == "vendor" ||
				name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // skip unreadable files
		}
		if info.ModTime().Unix() > lastIngest {
			stale = append(stale, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return stale, nil
}

// --- Helpers ---

func convertDef(d store.Definition) Definition {
	return Definition{
		ID: d.ID, ModuleID: d.ModuleID, Name: d.Name, Kind: d.Kind,
		Exported: d.Exported, Test: d.Test, Receiver: d.Receiver,
		Signature: d.Signature, Doc: d.Doc, StartLine: d.StartLine,
		EndLine: d.EndLine, SourceFile: d.SourceFile,
	}
}

func convertDefs(defs []store.Definition) []Definition {
	out := make([]Definition, len(defs))
	for i, d := range defs {
		out[i] = convertDef(d)
	}
	return out
}
