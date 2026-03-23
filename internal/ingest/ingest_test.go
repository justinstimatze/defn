package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testdataPath(name string) string {
	p, _ := filepath.Abs(filepath.Join("../../testdata", name))
	return p
}

func TestIngestEdgeCases(t *testing.T) {
	path := testdataPath("edgecases")
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err != nil {
		t.Skip("testdata/edgecases not found")
	}
	db := testDB(t)
	if err := Ingest(db, path); err != nil {
		t.Fatal(err)
	}

	// Check iota group stored as single definition.
	red, err := db.GetDefinitionByName("Red", "")
	if err != nil {
		t.Fatal("iota group not found as 'Red'")
	}
	if !strings.Contains(red.Body, "Green") || !strings.Contains(red.Body, "Blue") {
		t.Fatal("iota group body should contain all constants")
	}
	if !strings.Contains(red.Body, "iota") {
		t.Fatal("iota group body should contain 'iota'")
	}

	// Check non-iota grouped constants stored individually.
	maxSize, err := db.GetDefinitionByName("MaxSize", "")
	if err != nil {
		t.Fatal("MaxSize not found")
	}
	if strings.Contains(maxSize.Body, "MinSize") {
		t.Fatal("MaxSize body should NOT contain MinSize (individual spec)")
	}

	minSize, err := db.GetDefinitionByName("MinSize", "")
	if err != nil {
		t.Fatal("MinSize not found")
	}
	_ = minSize

	// Check multi-name var stored once under first name.
	xVar, err := db.GetDefinitionByName("x", "")
	if err != nil {
		t.Fatal("multi-name var 'x' not found")
	}
	if !strings.Contains(xVar.Body, "y") {
		t.Fatal("multi-name var body should contain both x and y")
	}
	// y should NOT exist as a separate definition.
	_, err = db.GetDefinitionByName("y", "")
	if err == nil {
		t.Fatal("'y' should not be a separate definition (part of 'var x, y int')")
	}

	// Check multiple init functions preserved with unique names.
	init0, err := db.GetDefinitionByName("init", "")
	if err != nil {
		t.Fatal("init not found")
	}
	if !strings.Contains(init0.Body, "init 1") {
		t.Fatalf("first init body wrong: %s", init0.Body)
	}

	init1, err := db.GetDefinitionByName("init_1", "")
	if err != nil {
		t.Fatal("init_1 not found")
	}
	if !strings.Contains(init1.Body, "init 2") {
		t.Fatalf("second init body wrong: %s", init1.Body)
	}

	// Check init bodies emit as func init() not func init_1().
	if !strings.HasPrefix(strings.TrimSpace(stripDocComment(init1.Body)), "func init()") {
		t.Fatalf("init_1 body should start with 'func init()' not 'func init_1()': %s", init1.Body)
	}

	// Check grouped types stored individually.
	myInt, err := db.GetDefinitionByName("MyInt", "")
	if err != nil {
		t.Fatal("MyInt not found")
	}
	if strings.Contains(myInt.Body, "MyString") {
		t.Fatal("MyInt body should NOT contain MyString (individual spec)")
	}

	// Check method with receiver.
	start, err := db.GetDefinitionByName("Start", "")
	if err != nil {
		t.Fatal("Start method not found")
	}
	if start.Kind != "method" || start.Receiver != "*Server" {
		t.Fatalf("Start: kind=%s receiver=%s", start.Kind, start.Receiver)
	}

	// Check type stored.
	server, err := db.GetDefinitionByName("Server", "")
	if err != nil {
		t.Fatal("Server type not found")
	}
	if server.Kind != "type" {
		t.Fatalf("Server kind=%s, want type", server.Kind)
	}

	// Check imports stored.
	mods, _ := db.ListModules()
	if len(mods) == 0 {
		t.Fatal("no modules")
	}
	imports, _ := db.GetImports(mods[0].ID)
	hasFmt := false
	for _, imp := range imports {
		if imp.ImportedPath == "fmt" {
			hasFmt = true
		}
	}
	if !hasFmt {
		t.Fatal("fmt import not found")
	}
}

func TestIngestPrunesStaleDefinitions(t *testing.T) {
	path := testdataPath("simple")
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err != nil {
		t.Skip("testdata/simple not found")
	}
	db := testDB(t)

	// First ingest.
	if err := Ingest(db, path); err != nil {
		t.Fatal(err)
	}

	// Count definitions.
	defs1, _ := db.FindDefinitions("%")
	count1 := len(defs1)
	if count1 == 0 {
		t.Fatal("no definitions after first ingest")
	}

	// Re-ingest same source — count should be the same (no ghosts).
	if err := Ingest(db, path); err != nil {
		t.Fatal(err)
	}
	defs2, _ := db.FindDefinitions("%")
	if len(defs2) != count1 {
		t.Fatalf("re-ingest changed definition count: %d → %d", count1, len(defs2))
	}
}

func TestIngestProjectFiles(t *testing.T) {
	path := testdataPath("simple")
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err != nil {
		t.Skip("testdata/simple not found")
	}
	db := testDB(t)
	if err := Ingest(db, path); err != nil {
		t.Fatal(err)
	}

	paths, err := db.ListProjectFiles()
	if err != nil {
		t.Fatal(err)
	}

	hasGoMod := false
	for _, p := range paths {
		if p == "go.mod" {
			hasGoMod = true
		}
	}
	if !hasGoMod {
		t.Fatal("go.mod not stored as project file")
	}
}

// stripDocComment removes leading // comment lines from a body.
func stripDocComment(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			return strings.Join(lines[i:], "\n")
		}
	}
	return body
}
