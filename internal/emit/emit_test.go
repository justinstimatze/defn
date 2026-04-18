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

func TestEmitPrefersFileSourcesOverDisk(t *testing.T) {
	// Phase C: file_sources.raw is authoritative. When it's populated,
	// emit uses it as the merge base — even if the on-disk file is
	// missing or differs. Proves that ingest→emit roundtrip via the DB
	// preserves content that disk no longer has.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")

	// Seed file_sources with a rich file the DB's definitions table
	// can't fully represent (build tag, package doc, init, plus Foo).
	rawSeed := `//go:build linux

// Package test is rich.
package test

import "fmt"

func init() {
	fmt.Println("hi")
}

func Foo() string { return "OLD" }
`
	if err := db.SetFileSource(mod.ID, "test.go", rawSeed); err != nil {
		t.Fatal(err)
	}
	// Definitions table only knows about Foo — the DB's named-decl view.
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: `func Foo() string { return "NEW" }`, SourceFile: "test.go",
	})

	// Emit to a directory that has NO file on disk. Without file_sources,
	// we'd regenerate and lose everything except Foo. With file_sources,
	// everything is preserved and only Foo's body is swapped.
	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	if !strings.Contains(got, `"NEW"`) {
		t.Fatalf("Foo body not updated:\n%s", got)
	}
	if !strings.Contains(got, "//go:build linux") {
		t.Fatalf("build tag lost:\n%s", got)
	}
	if !strings.Contains(got, "Package test is rich") {
		t.Fatalf("package doc lost:\n%s", got)
	}
	if !strings.Contains(got, "func init()") {
		t.Fatalf("init() lost:\n%s", got)
	}
}

func TestEmitASTMergePreservesUnknownContent(t *testing.T) {
	// Phase A: when the target file already exists and parses, emit
	// should patch changed decl bodies into the existing AST and leave
	// everything else alone — build constraints, package docs, per-file
	// imports, init() functions, floating comments, original decl
	// ordering. All of those are things defn's schema doesn't track
	// faithfully; AST-merge lets Go's parser + format do the roundtrip.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() string { return \"NEW\" }", SourceFile: "test.go",
	})

	outDir := t.TempDir()
	existing := []byte(`//go:build linux

// Package test is an example with content defn doesn't track.
package test

import (
	"fmt"
	_ "embed"
)

// init runs at startup.
func init() {
	fmt.Println("hi")
}

// Foo is the one defn knows about.
func Foo() string { return "OLD" }

// Bar is not in the DB; must be preserved.
func Bar() int { return 42 }
`)
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
	got := string(data)

	// Body update happened.
	if !strings.Contains(got, `"NEW"`) {
		t.Fatalf("Foo body not updated to NEW:\n%s", got)
	}
	// Build constraint preserved.
	if !strings.Contains(got, "//go:build linux") {
		t.Fatalf("//go:build constraint was lost:\n%s", got)
	}
	// Package doc preserved.
	if !strings.Contains(got, "Package test is an example") {
		t.Fatalf("package doc was lost:\n%s", got)
	}
	// init() preserved (not renamed, not deleted).
	if !strings.Contains(got, "func init()") {
		t.Fatalf("init() was lost:\n%s", got)
	}
	// Non-DB decl Bar preserved.
	if !strings.Contains(got, "func Bar()") {
		t.Fatalf("Bar was lost:\n%s", got)
	}
	// Per-file import preserved.
	if !strings.Contains(got, `_ "embed"`) {
		t.Fatalf("blank import was lost:\n%s", got)
	}
}

func TestEmitMergePatchesTypeSpecInPlace(t *testing.T) {
	// Edits to a type body should patch the TypeSpec inside its existing
	// GenDecl, preserving surrounding type decls, comments, and ordering.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "type", Exported: true,
		Body: "type Foo struct {\n\tNewField int\n}", SourceFile: "test.go",
	})

	outDir := t.TempDir()
	existing := []byte(`package test

// Bar is a neighbor that must survive.
type Bar struct {
	X int
}

// Foo gets patched.
type Foo struct {
	OldField string
}

// Baz is another neighbor.
type Baz int
`)
	if err := os.WriteFile(filepath.Join(outDir, "test.go"), existing, 0644); err != nil {
		t.Fatal(err)
	}

	if err := Emit(db, outDir); err != nil {
		t.Fatalf("emit: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "NewField int") {
		t.Fatalf("Foo body not patched:\n%s", s)
	}
	if strings.Contains(s, "OldField string") {
		t.Fatalf("old Foo body still present:\n%s", s)
	}
	if !strings.Contains(s, "type Bar struct") {
		t.Fatalf("Bar was lost:\n%s", s)
	}
	if !strings.Contains(s, "type Baz int") {
		t.Fatalf("Baz was lost:\n%s", s)
	}
	if !strings.Contains(s, "Bar is a neighbor") {
		t.Fatalf("Bar's doc comment was lost:\n%s", s)
	}
}

func TestEmitMergePreservesGroupedDocComments(t *testing.T) {
	// Regression: AST-surgery (replacing a spec node with one parsed from
	// a foreign fset) orphans the original Doc CommentGroup, leaving the
	// comment floating between unrelated specs. Byte-range splicing
	// preserves each spec's leading doc comment because it only touches
	// the bytes inside s.Pos()..s.End() — comments live outside that.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "B", Kind: "const", Exported: true,
		Body: "B = 99", SourceFile: "test.go",
	})

	outDir := t.TempDir()
	existing := []byte(`package test

const (
	// DocA is the doc for A.
	A = 1
	// DocB is the doc for B.
	B = 2
	// DocC is the doc for C.
	C = 3
)
`)
	if err := os.WriteFile(filepath.Join(outDir, "test.go"), existing, 0644); err != nil {
		t.Fatal(err)
	}

	if err := Emit(db, outDir); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)

	// Each doc comment must immediately precede its own spec — no
	// floating or reordered comments.
	checks := []struct{ before, after string }{
		{"// DocA is the doc for A.", "A = 1"},
		{"// DocB is the doc for B.", "B = 99"},
		{"// DocC is the doc for C.", "C = 3"},
	}
	for _, c := range checks {
		i := strings.Index(s, c.before)
		j := strings.Index(s, c.after)
		if i < 0 || j < 0 {
			t.Fatalf("missing %q or %q in output:\n%s", c.before, c.after, s)
		}
		if i > j {
			t.Fatalf("%q appears after %q (doc comment drifted):\n%s", c.before, c.after, s)
		}
		// And no other spec text should appear between them.
		between := s[i+len(c.before) : j]
		if strings.Contains(between, "=") {
			t.Fatalf("unexpected spec between %q and %q:\n%q\nfull:\n%s",
				c.before, c.after, between, s)
		}
	}
}

func TestEmitMergePatchesIotaConstBlock(t *testing.T) {
	// Iota const blocks ingest as a single definition under the first
	// name, with the whole "const ( A = iota; B; C )" block as the body.
	// Per-spec splicing would cram the whole block into A's byte range,
	// producing a nested const block. The merge must detect a grouped-
	// GenDecl body and replace the enclosing GenDecl whole.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Red", Kind: "const", Exported: true,
		Body: "const (\n\tRed = iota + 1\n\tGreen\n\tBlue\n\tYellow\n)",
		SourceFile: "test.go",
	})

	outDir := t.TempDir()
	existing := []byte(`package test

// Color is a neighboring type that must survive.
type Color int

const (
	Red = iota
	Green
	Blue
)

// Max is another neighbor.
const Max = 100
`)
	if err := os.WriteFile(filepath.Join(outDir, "test.go"), existing, 0644); err != nil {
		t.Fatal(err)
	}

	if err := Emit(db, outDir); err != nil {
		t.Fatalf("emit: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)

	// The iota block was replaced: new members appear, old "= iota"
	// (without "+ 1") does not.
	if !strings.Contains(s, "Red = iota + 1") {
		t.Fatalf("iota block not patched:\n%s", s)
	}
	if !strings.Contains(s, "Yellow") {
		t.Fatalf("new iota member Yellow missing:\n%s", s)
	}
	// No nested const block (would indicate per-spec splicing misfire).
	if strings.Count(s, "const (") > 1 {
		t.Fatalf("nested const block (per-spec splice misfired):\n%s", s)
	}
	// Neighbors survive.
	if !strings.Contains(s, "type Color int") {
		t.Fatalf("Color type lost:\n%s", s)
	}
	if !strings.Contains(s, "const Max = 100") {
		t.Fatalf("Max const lost:\n%s", s)
	}
}

func TestEmitMergePatchesGroupedConstInPlace(t *testing.T) {
	// Editing one const inside a grouped const block should patch only
	// that spec and leave the rest of the block intact.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "B", Kind: "const", Exported: true,
		Body: "B = 99", SourceFile: "test.go",
	})

	outDir := t.TempDir()
	existing := []byte(`package test

const (
	A = 1
	B = 2
	C = 3
)
`)
	if err := os.WriteFile(filepath.Join(outDir, "test.go"), existing, 0644); err != nil {
		t.Fatal(err)
	}

	if err := Emit(db, outDir); err != nil {
		t.Fatalf("emit: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "B = 99") {
		t.Fatalf("B not patched:\n%s", s)
	}
	if strings.Contains(s, "B = 2") {
		t.Fatalf("old B = 2 still present:\n%s", s)
	}
	if !strings.Contains(s, "A = 1") || !strings.Contains(s, "C = 3") {
		t.Fatalf("sibling consts lost:\n%s", s)
	}
	if !strings.Contains(s, "const (") {
		t.Fatalf("grouped const block structure lost:\n%s", s)
	}
}

func TestEmitRefreshesFileSourcesAfterWrite(t *testing.T) {
	// After emit writes a file (and goimports post-processes it), the
	// authoritative raw source stored in file_sources must be updated to
	// match what's on disk. Without this refresh, file_sources drifts
	// from disk on every body edit until the next full re-ingest.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")

	// Seed file_sources with the "old" version and definitions pointing to
	// the "new" body.
	rawSeed := `package test

func Foo() string { return "OLD" }
`
	if err := db.SetFileSource(mod.ID, "test.go", rawSeed); err != nil {
		t.Fatal(err)
	}
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: `func Foo() string { return "NEW" }`, SourceFile: "test.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}

	refreshed, err := db.GetFileSource(mod.ID, "test.go")
	if err != nil {
		t.Fatalf("GetFileSource: %v", err)
	}
	if !strings.Contains(refreshed, `"NEW"`) {
		t.Fatalf("file_sources not refreshed, still contains OLD body:\n%s", refreshed)
	}
	if strings.Contains(refreshed, `"OLD"`) {
		t.Fatalf("file_sources still contains OLD body:\n%s", refreshed)
	}

	onDisk, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != refreshed {
		t.Fatalf("file_sources doesn't match disk:\n-- disk --\n%s\n-- file_sources --\n%s",
			onDisk, refreshed)
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
