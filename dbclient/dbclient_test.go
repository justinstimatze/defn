package dbclient

import (
	"strings"
	"testing"
)

func TestIsReadOnlySQL(t *testing.T) {
	cases := []struct {
		q  string
		ok bool
	}{
		{"SELECT 1", true},
		{"  select foo from bar", true},
		{"WITH t AS (SELECT 1) SELECT * FROM t", true},
		{"SHOW TABLES", true},
		{"DESCRIBE definitions", true},
		{"DESC definitions", true},
		{"EXPLAIN SELECT 1", true},
		{"INSERT INTO foo VALUES (1)", false},
		{"UPDATE foo SET x = 1", false},
		{"DELETE FROM foo", false},
		{"DROP TABLE foo", false},
		{"CREATE TABLE foo (x INT)", false},
		{"ALTER TABLE foo ADD COLUMN y INT", false},
		{"CALL DOLT_COMMIT('m')", false},
	}
	for _, c := range cases {
		if got := isReadOnlySQL(c.q); got != c.ok {
			t.Errorf("isReadOnlySQL(%q) = %v, want %v", c.q, got, c.ok)
		}
	}
}

// This isn't a full round-trip integration test — those would need a
// running dolt sql-server, which the standard test env doesn't provide.
// Kept as a smoke test that the API compiles and the packaging story
// (no Dolt drag) holds under refactor. See go.mod diff for the drag
// guarantee: dbclient depends only on database/sql + mysql driver.
func TestPackageCompiles(t *testing.T) {
	// Force-reference every exported type/const so a rename in the future
	// breaks this test loudly instead of silently orphaning consumers.
	var (
		_ *Client
		_ = DefinitionFilter{Kind: "type"}
		_ = Definition{}
		_ = TableDefinitions
		_ = TableBodies
		_ = TableRefs
		_ = TableModules
		_ = TableImports
		_ = TableLiterals
		_ = TablePragmas
		_ = TableProjectFiles
		_ = TableMeta
	)
	// Placate the linter about the guarded reference above.
	if strings.HasPrefix(TableDefinitions, "definitions") != true {
		t.Fatal("TableDefinitions changed shape")
	}
}
