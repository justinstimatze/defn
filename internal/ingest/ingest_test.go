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

// TestIngestStructFields is the regression test for #11: struct fields
// get their own "field" kind definitions, and Type.Field resolves via
// the same receiver.method name-lookup path methods already use.
func TestIngestStructFields(t *testing.T) {
	path := testdataPath("edgecases")
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err != nil {
		t.Skip("testdata/edgecases not found")
	}
	db := testDB(t)
	if err := Ingest(db, path); err != nil {
		t.Fatal(err)
	}

	port, err := db.GetDefinitionByName("Server.Port", "")
	if err != nil {
		t.Fatalf("Server.Port not resolved via Type.Field lookup: %v", err)
	}
	if port.Kind != "field" {
		t.Errorf("Server.Port: kind=%s, want field", port.Kind)
	}
	if port.Receiver != "Server" {
		t.Errorf("Server.Port: receiver=%s, want Server", port.Receiver)
	}
	if port.Name != "Port" {
		t.Errorf("Server.Port: name=%s, want Port", port.Name)
	}
	if !strings.Contains(port.Signature, "int") {
		t.Errorf("Server.Port: signature=%q, want it to contain int", port.Signature)
	}

	// The struct type itself must still resolve to "type", not get
	// shadowed or altered by its own field's definition.
	server, err := db.GetDefinitionByName("Server", "")
	if err != nil {
		t.Fatal("Server type not found")
	}
	if server.Kind != "type" {
		t.Errorf("Server: kind=%s, want type", server.Kind)
	}
}

// TestIngestFunc_InitNamingStableAcrossFilesAndIngestModes guards the
// #241 fix: initCounter was keyed by module alone, so the Nth init()
// encountered anywhere in the module (across every file, in whatever
// order that specific ingest run happened to process them) determined
// its synthetic name. A full-module ingest and a single-file sync
// (IngestFile, which always starts a fresh ingestState per call) would
// then assign DIFFERENT names to the SAME physical init() function --
// and since name is part of UpsertDefinition's natural key, that
// mismatch creates a new row instead of updating the existing one,
// leaving the old name orphaned. Real trajectory (cli-513): mixing
// file-level and module-level `sync` calls during one session
// accumulated six separate, byte-identical copies of one physical
// init() into the emitted file.
//
// Keying initCounter by (module, sourceFile) instead makes the Nth
// init() in a given file always get the same name regardless of which
// other files are ingested alongside it, or whether the file is
// (re-)ingested via full-module Ingest or single-file IngestFile.
func TestIngestFunc_InitNamingStableAcrossFilesAndIngestModes(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "alpha"), 0755)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testproj\n\ngo 1.26\n"), 0644)
	// Two files, each with exactly one init() -- a module-wide counter
	// would number beta's init "init_1" (or higher) purely because
	// alpha's file happened to be processed first in this ingest run.
	os.WriteFile(filepath.Join(dir, "root.go"), []byte(`package testproj

func init() {
	println("root")
}
`), 0644)
	os.WriteFile(filepath.Join(dir, "alpha", "alpha.go"), []byte(`package alpha

func init() {
	println("alpha")
}
`), 0644)

	db := testDB(t)
	if err := Ingest(db, dir); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	rootInit, err := db.GetDefinitionByName("init", "testproj")
	if err != nil || rootInit == nil {
		t.Fatalf("expected root.go's init() to be named bare \"init\", got err=%v", err)
	}
	alphaInit, err := db.GetDefinitionByName("init", "testproj/alpha")
	if err != nil || alphaInit == nil {
		t.Fatalf("expected alpha/alpha.go's init() to ALSO be named bare \"init\" (per-file counter, not module-wide), got err=%v", err)
	}

	// Re-ingest alpha's file alone via the fast single-file path (what
	// `sync file:alpha/alpha.go` does) and confirm it assigns the SAME
	// name as the full ingest did -- no orphaned "init_1" duplicate.
	if _, err := IngestFile(db, dir, filepath.Join(dir, "alpha", "alpha.go")); err != nil {
		t.Fatalf("IngestFile: %v", err)
	}
	defs, err := db.FindDefinitionsByFile("alpha", "alpha/alpha.go", 0)
	if err != nil {
		t.Fatalf("FindDefinitionsByFile: %v", err)
	}
	initCount := 0
	for _, d := range defs {
		if d.Name == "init" || strings.HasPrefix(d.Name, "init_") {
			initCount++
		}
	}
	if initCount != 1 {
		t.Errorf("expected exactly 1 init-shaped definition in alpha/alpha.go after mixing ingest modes, got %d: %+v", initCount, defs)
	}
}
