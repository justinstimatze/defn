package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/store"
)

func testDB(t *testing.T) store.Backend {
	t.Helper()
	dir := t.TempDir()
	db, err := store.OpenBackend(filepath.Join(dir, "test.db"))
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

// TestIngestComments_PragmaLinkedToFollowingDef is the winze regression:
// a pragma comment (e.g. //winze:functional) that is the doc comment
// (or the last line of a multi-line doc comment) directly above a
// definition should carry that definition's DefID -- db.Pragmas()
// otherwise returns DefName="" for every def-attached pragma. Covers
// both a bare single-line pragma-as-doc and a pragma trailing prose in
// one doc-comment group, since winze's own repro didn't specify which
// shape their corpus uses.
func TestIngestComments_PragmaLinkedToFollowingDef(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module pragmatest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := `package pragmatest

//winze:functional
type FormedAt struct{ V int }

// EnergyEstimate is a rough figure.
//winze:contested
type EnergyEstimate struct{ V int }
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	db := testDB(t)
	if err := Ingest(db, dir); err != nil {
		t.Fatal(err)
	}

	formedAt, err := db.GetDefinitionByName("FormedAt", "")
	if err != nil {
		t.Fatalf("FormedAt not found: %v", err)
	}
	energyEstimate, err := db.GetDefinitionByName("EnergyEstimate", "")
	if err != nil {
		t.Fatalf("EnergyEstimate not found: %v", err)
	}
	t.Logf("FormedAt id=%d start=%d end=%d", formedAt.ID, formedAt.StartLine, formedAt.EndLine)
	t.Logf("EnergyEstimate id=%d start=%d end=%d", energyEstimate.ID, energyEstimate.StartLine, energyEstimate.EndLine)

	functionalComments, err := db.GetCommentsByPragma("winze:functional")
	if err != nil {
		t.Fatalf("GetCommentsByPragma winze:functional: %v", err)
	}
	if len(functionalComments) != 1 {
		t.Fatalf("expected 1 winze:functional comment, got %d: %+v", len(functionalComments), functionalComments)
	}
	if functionalComments[0].DefID == nil || *functionalComments[0].DefID != formedAt.ID {
		t.Errorf("winze:functional (bare single-line doc) should carry FormedAt's def_id (%d), got %+v", formedAt.ID, functionalComments[0])
	}

	contestedComments, err := db.GetCommentsByPragma("winze:contested")
	if err != nil {
		t.Fatalf("GetCommentsByPragma winze:contested: %v", err)
	}
	if len(contestedComments) != 1 {
		t.Fatalf("expected 1 winze:contested comment, got %d: %+v", len(contestedComments), contestedComments)
	}
	if contestedComments[0].DefID == nil || *contestedComments[0].DefID != energyEstimate.ID {
		t.Errorf("winze:contested (last line of a multi-line doc group) should carry EnergyEstimate's def_id (%d), got %+v", energyEstimate.ID, contestedComments[0])
	}
}

// TestIngestFile_PragmaLinkedToFollowingDef is the #224 regression:
// IngestFile (the incremental single-file sync path used by
// code(op:"sync", file:...)) never called ingestComments at all, so
// pragma/doc-comment-to-def linking only ever happened on a full
// Ingest -- any def added or changed via incremental sync kept
// def_id=NULL for its comments even after #223's full-ingest fix.
func TestIngestFile_PragmaLinkedToFollowingDef(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module pragmafiletest\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := `package pragmafiletest

//winze:functional
type FormedAt struct{ V int }
`
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	db := testDB(t)
	if _, err := IngestFile(db, dir, mainPath); err != nil {
		t.Fatal(err)
	}

	formedAt, err := db.GetDefinitionByName("FormedAt", "")
	if err != nil {
		t.Fatalf("FormedAt not found: %v", err)
	}

	comments, err := db.GetCommentsByPragma("winze:functional")
	if err != nil {
		t.Fatalf("GetCommentsByPragma winze:functional: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("expected 1 winze:functional comment, got %d: %+v", len(comments), comments)
	}
	if comments[0].DefID == nil || *comments[0].DefID != formedAt.ID {
		t.Errorf("winze:functional should carry FormedAt's def_id (%d) after IngestFile, got %+v", formedAt.ID, comments[0])
	}
}
