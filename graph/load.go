package graph

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // registers "mysql" driver for server mode
)

// Load loads a graph from a defn database path.
// Tries sql-server first (fast), falls back to subprocess (slower).
// Results are cached for the process lifetime.
func Load(defnPath string) (*Graph, error) {
	absPath, err := filepath.Abs(defnPath)
	if err != nil {
		return nil, err
	}

	return loadOnce(absPath, func() (*Graph, error) {
		g, err := loadFromServer(absPath)
		if err != nil {
			g, err = loadFromSubprocess(absPath)
		}
		return g, err
	})
}

// LoadFromDSN loads a graph from a MySQL DSN (for server mode).
func LoadFromDSN(dsn string) (*Graph, error) {
	return loadOnce(dsn, func() (*Graph, error) {
		db, err := sql.Open("mysql", dsn)
		if err != nil {
			return nil, fmt.Errorf("open: %w", err)
		}
		defer db.Close()
		return queryGraph(db)
	})
}

// LoadFromDB loads a graph from an already-open *sql.DB connection.
// This is the fastest path — no connection setup, no subprocess.
func LoadFromDB(db *sql.DB) (*Graph, error) {
	return queryGraph(db)
}

// LoadMulti loads and merges multiple defn databases into one graph.
// Definitions from different repos coexist — each keeps its original module path.
// Cross-repo duplicates are findable via Duplicates() (same body hash).
func LoadMulti(defnPaths ...string) (*Graph, error) {
	// Load all graphs first — fail early before merging anything.
	graphs := make([]*Graph, 0, len(defnPaths))
	for _, path := range defnPaths {
		g, err := Load(path)
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", path, err)
		}
		graphs = append(graphs, g)
	}

	merged := &Graph{
		modByPath: make(map[string][]int64),
		modByID:   make(map[int64]string),
	}

	var idOffset int64

	for _, g := range graphs {
		// Find the max ID across both defs and modules for offset calculation.
		var maxID int64
		for _, d := range g.defs {
			if d.ID > maxID {
				maxID = d.ID
			}
		}
		for id := range g.modByID {
			if id > maxID {
				maxID = id
			}
		}

		// Copy definitions with offset IDs.
		for _, d := range g.defs {
			copy := *d
			copy.ID += idOffset
			copy.ModuleID += idOffset
			merged.defs = append(merged.defs, &copy)
		}

		// Copy refs with offset IDs.
		for _, r := range g.refs {
			merged.refs = append(merged.refs, Ref{
				FromDef: r.FromDef + idOffset,
				ToDef:   r.ToDef + idOffset,
				Kind:    r.Kind,
			})
		}

		// Copy modules with offset IDs. Append all IDs for each path —
		// merged graphs may have the same path from multiple repos.
		for path, ids := range g.modByPath {
			for _, id := range ids {
				offsetID := id + idOffset
				merged.modByPath[path] = append(merged.modByPath[path], offsetID)
				merged.modByID[offsetID] = path
			}
		}

		idOffset += maxID + 1
	}

	merged.build()
	return merged, nil
}

func loadFromServer(defnPath string) (*Graph, error) {
	// Check for server port file or try default port.
	portFile := filepath.Join(defnPath, "server.port")
	port := "3307"
	if data, err := os.ReadFile(portFile); err == nil {
		port = strings.TrimSpace(string(data))
	}

	dbName := filepath.Base(defnPath)

	// Try connecting — defn user first (from --server init), then root.
	var db *sql.DB
	for _, dsn := range []string{
		fmt.Sprintf("defn@tcp(127.0.0.1:%s)/%s", port, dbName),
		fmt.Sprintf("root@tcp(127.0.0.1:%s)/%s", port, dbName),
	} {
		d, err := sql.Open("mysql", dsn)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = d.PingContext(ctx)
		cancel()
		if err != nil {
			d.Close()
			continue
		}
		db = d
		break
	}
	if db == nil {
		return nil, fmt.Errorf("server not available on port %s", port)
	}
	defer db.Close()

	return queryGraph(db)
}

func queryGraph(db *sql.DB) (*Graph, error) {
	g := &Graph{
		modByPath: make(map[string][]int64),
		modByID:   make(map[int64]string),
	}

	// Load modules.
	rows, err := db.Query("SELECT id, path FROM modules")
	if err != nil {
		return nil, fmt.Errorf("query modules: %w", err)
	}
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return nil, err
		}
		g.modByPath[path] = append(g.modByPath[path], id)
		g.modByID[id] = path
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read modules: %w", err)
	}

	// Load definitions.
	rows, err = db.Query(`SELECT id, name, kind, COALESCE(receiver,''), COALESCE(signature,''),
		COALESCE(source_file,''), module_id, test, exported, COALESCE(hash,'') FROM definitions`)
	if err != nil {
		return nil, fmt.Errorf("query definitions: %w", err)
	}
	for rows.Next() {
		d := &Def{}
		if err := rows.Scan(&d.ID, &d.Name, &d.Kind, &d.Receiver, &d.Signature,
			&d.SourceFile, &d.ModuleID, &d.Test, &d.Exported, &d.Hash); err != nil {
			rows.Close()
			return nil, err
		}
		g.defs = append(g.defs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read definitions: %w", err)
	}

	// Load references.
	rows, err = db.Query("SELECT from_def, to_def, COALESCE(kind,'') FROM `references`")
	if err != nil {
		return nil, fmt.Errorf("query references: %w", err)
	}
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.FromDef, &r.ToDef, &r.Kind); err != nil {
			rows.Close()
			return nil, err
		}
		g.refs = append(g.refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read references: %w", err)
	}

	g.build()
	return g, nil
}

func loadFromSubprocess(defnPath string) (*Graph, error) {
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		return nil, fmt.Errorf("dolt not found: %w", err)
	}

	dbDir := defnPath
	g := &Graph{
		modByPath: make(map[string][]int64),
		modByID:   make(map[int64]string),
	}

	// Query modules.
	out, err := doltQuery(doltBin, dbDir, "SELECT id, path FROM modules")
	if err != nil {
		return nil, fmt.Errorf("query modules: %w", err)
	}
	for _, row := range out {
		id := jsonInt64(row["id"])
		path := jsonString(row["path"])
		g.modByPath[path] = append(g.modByPath[path], id)
		g.modByID[id] = path
	}

	// Query definitions.
	out, err = doltQuery(doltBin, dbDir, `SELECT id, name, kind, COALESCE(receiver,'') as receiver,
		COALESCE(signature,'') as signature, COALESCE(source_file,'') as source_file,
		module_id, test, exported, COALESCE(hash,'') as hash FROM definitions`)
	if err != nil {
		return nil, fmt.Errorf("query definitions: %w", err)
	}
	for _, row := range out {
		d := &Def{
			ID:         jsonInt64(row["id"]),
			Name:       jsonString(row["name"]),
			Kind:       jsonString(row["kind"]),
			Receiver:   jsonString(row["receiver"]),
			Signature:  jsonString(row["signature"]),
			SourceFile: jsonString(row["source_file"]),
			ModuleID:   jsonInt64(row["module_id"]),
			Test:       jsonBool(row["test"]),
			Exported:   jsonBool(row["exported"]),
			Hash:       jsonString(row["hash"]),
		}
		g.defs = append(g.defs, d)
	}

	// Query references.
	out, err = doltQuery(doltBin, dbDir, "SELECT from_def, to_def, COALESCE(kind,'') as kind FROM `references`")
	if err != nil {
		return nil, fmt.Errorf("query references: %w", err)
	}
	for _, row := range out {
		g.refs = append(g.refs, Ref{
			FromDef: jsonInt64(row["from_def"]),
			ToDef:   jsonInt64(row["to_def"]),
			Kind:    jsonString(row["kind"]),
		})
	}

	g.build()
	return g, nil
}

func doltQuery(doltBin, dbDir, query string) ([]map[string]any, error) {
	cmd := exec.Command(doltBin, "sql", "-q", query, "-r", "json")
	cmd.Dir = dbDir
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("dolt sql: %s", strings.TrimSpace(msg))
	}
	out := stdout.String()
	// Strip any non-JSON prefix (warnings, debug output).
	if idx := strings.Index(out, "{"); idx > 0 {
		out = out[idx:]
	}
	var result struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, fmt.Errorf("parse dolt output: %w (raw: %s)", err, out[:min(len(out), 200)])
	}
	return result.Rows, nil
}

func jsonString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func jsonInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}

func jsonBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	case int:
		return b != 0
	default:
		return false
	}
}
