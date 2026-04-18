package emit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
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

func TestEmitHandlesProjectRelativeSourceFile(t *testing.T) {
	// Regression: before 2026-04-17, source_file was used verbatim as a
	// byFile key and joined with pkgDir, yielding doubled paths like
	// outDir/cmd/defn/cmd/defn/main.go. Ensure basename is used.
	db := testDB(t)
	root, _ := db.EnsureModule("example.com/test", "test", "")
	sub, _ := db.EnsureModule("example.com/test/cmd/tool", "main", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: root.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() {}", SourceFile: "test.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: sub.ID, Name: "main", Kind: "function",
		Body: "func main() {}", SourceFile: "cmd/tool/main.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(outDir, "cmd/tool/main.go")); err != nil {
		t.Fatalf("expected main.go at cmd/tool/, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "cmd/tool/cmd/tool/main.go")); err == nil {
		t.Fatal("emit produced doubled path cmd/tool/cmd/tool/main.go")
	}
}

func TestEmitSkipsFilesThatWouldLoseDeclarations(t *testing.T) {
	// The database-reconstructed file must not be allowed to clobber an
	// on-disk file if doing so would remove a top-level declaration. This
	// matters because the current ingest doesn't round-trip everything
	// (init functions get renamed to init0/init1, file-level doc comments
	// aren't per-file, etc.). Better to skip with a warning than to
	// destroy user code.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() {}", SourceFile: "test.go",
	})

	// Pre-populate outDir with a file that declares both Foo and an init
	// function. Emit will try to write only Foo; it should refuse because
	// init() would be lost.
	outDir := t.TempDir()
	existing := []byte("package test\n\nfunc init() {}\n\nfunc Foo() {}\n")
	if err := os.WriteFile(filepath.Join(outDir, "test.go"), existing, 0644); err != nil {
		t.Fatal(err)
	}

	if err := Emit(db, outDir); err != nil {
		t.Fatalf("emit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "func init()") {
		t.Fatalf("init() was removed by emit; safety net failed:\n%s", data)
	}
}

func TestEmitWritesNewFileWithoutSafetyCheck(t *testing.T) {
	// When the target path doesn't exist, emit should just write — the
	// safety net only protects against losing existing on-disk content.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/new", "new", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() {}", SourceFile: "new.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "new.go")); err != nil {
		t.Fatalf("expected new.go to be created: %v", err)
	}
}

func TestEmitGeneratedHeader(t *testing.T) {
	db := testDB(t)
	// Use a two-level module path so detectModuleRoot can compute a prefix.
	root, _ := db.EnsureModule("example.com/test", "test", "")
	db.EnsureModule("example.com/test/sub", "sub", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: root.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() {}",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}

	// With root "example.com/test", the root package emits to outDir/test.go
	// (relPath is "." for the root module).
	data, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.HasPrefix(content, "package ") {
		t.Fatal("missing package declaration")
	}
	if !strings.Contains(content, "func Foo() {}") {
		t.Fatalf("missing definition body in:\n%s", content)
	}
}

func TestEmitWithMapTracksLocations(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Alpha", Kind: "function", Exported: true,
		Body: "func Alpha() {}",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Beta", Kind: "function", Exported: true,
		Body: "func Beta() {\n\treturn\n}",
	})

	outDir := t.TempDir()
	locs, err := EmitWithMap(db, outDir)
	if err != nil {
		t.Fatal(err)
	}

	if len(locs) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(locs))
	}

	// Verify both definitions are tracked with valid line numbers.
	for _, loc := range locs {
		if loc.StartLine < 1 {
			t.Fatalf("%s: invalid StartLine %d", loc.DefName, loc.StartLine)
		}
	}
}

func TestEmitImports(t *testing.T) {
	db := testDB(t)
	// Two modules so detectModuleRoot works.
	mod, _ := db.EnsureModule("example.com/test/pkg", "pkg", "")
	db.EnsureModule("example.com/test/other", "other", "")

	db.SetImports(mod.ID, []store.Import{
		{ModuleID: mod.ID, ImportedPath: "fmt"},
		{ModuleID: mod.ID, ImportedPath: "embed", Alias: "_"},
	})
	// Use fmt so goimports doesn't strip it.
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() { fmt.Println() }",
	})

	outDir := t.TempDir()
	Emit(db, outDir)

	data, err := os.ReadFile(filepath.Join(outDir, "pkg", "pkg.go"))
	if err != nil {
		t.Fatalf("file not found: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, `"fmt"`) {
		t.Fatalf("missing fmt import in:\n%s", content)
	}
	// Blank imports should survive goimports.
	if !strings.Contains(content, `_ "embed"`) {
		t.Fatalf("missing blank import in:\n%s", content)
	}
}

func TestEmitProjectFiles(t *testing.T) {
	db := testDB(t)
	db.SetProjectFile("go.mod", "module example.com/test\n\ngo 1.25\n")
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() {}",
	})

	outDir := t.TempDir()
	Emit(db, outDir)

	data, err := os.ReadFile(filepath.Join(outDir, "go.mod"))
	if err != nil {
		t.Fatalf("go.mod not emitted: %v", err)
	}
	if !strings.Contains(string(data), "module example.com/test") {
		t.Fatal("go.mod content wrong")
	}
}

func TestRoundTrip(t *testing.T) {
	// Skip if go build not available (CI environments).
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH")
	}

	testdataDir, err := filepath.Abs("../../testdata/simple")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(testdataDir, "go.mod")); err != nil {
		t.Skipf("testdata not found: %v", err)
	}

	db := testDB(t)

	// Ingest.
	if err := ingest.Ingest(db, testdataDir); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// Resolve.
	if err := resolve.Resolve(db, testdataDir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Verify definitions were stored.
	defs, err := db.FindDefinitions("%")
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) == 0 {
		t.Fatal("no definitions ingested")
	}

	// Check specific definitions.
	greet, err := db.GetDefinitionByName("Greet", "")
	if err != nil {
		t.Fatal("Greet not found")
	}
	if greet.Kind != "function" || !greet.Exported {
		t.Fatalf("Greet: kind=%s exported=%v", greet.Kind, greet.Exported)
	}

	myType, err := db.GetDefinitionByName("MyType", "")
	if err != nil {
		t.Fatal("MyType not found")
	}
	if myType.Kind != "type" {
		t.Fatalf("MyType kind=%s, want type", myType.Kind)
	}

	stringer, err := db.GetDefinitionByName("String", "")
	if err != nil {
		t.Fatal("String method not found")
	}
	if stringer.Kind != "method" || stringer.Receiver != "MyType" {
		t.Fatalf("String: kind=%s receiver=%s", stringer.Kind, stringer.Receiver)
	}

	// Verify references.
	mainDef, err := db.GetDefinitionByName("main", "")
	if err != nil {
		t.Fatal("main not found")
	}
	callees, err := db.GetCallees(mainDef.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(callees) == 0 {
		t.Fatal("main has no callees")
	}

	// Emit.
	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// Verify go.mod emitted.
	if _, err := os.Stat(filepath.Join(outDir, "go.mod")); err != nil {
		t.Fatal("go.mod not emitted")
	}

	// Build the emitted code.
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = outDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build failed:\n%s", out)
	}
}
