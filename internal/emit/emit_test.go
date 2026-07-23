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

func TestEmitMergePreservesUntouchedInit(t *testing.T) {
	// init() can't round-trip through defn's schema faithfully (ingest
	// renames it to init0/init1 to side-step name collisions), so a
	// regenerate-from-DB path would emit the renamed variant instead
	// of init(). Byte-range merge sidesteps the round-trip problem:
	// it only touches the byte ranges of decls the DB is actually
	// patching — init()'s bytes are left exactly as they were on disk.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() { _ = 1 }", SourceFile: "test.go",
	})

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
	s := string(data)
	if !strings.Contains(s, "func init()") {
		t.Fatalf("init() lost during merge:\n%s", s)
	}
	if !strings.Contains(s, "_ = 1") {
		t.Fatalf("Foo body not patched:\n%s", s)
	}
}

func TestEmitSafetyNetBlocksRegenerateThatWouldDropOnDiskDecls(t *testing.T) {
	// When merge bails (here: because a newly-created DB def has no
	// on-disk counterpart), emit falls through to regeneration. If
	// regeneration would drop an on-disk decl the schema doesn't
	// represent (init, hand-edited helpers not yet ingested, etc.),
	// safeWriteGoFile must refuse the write. This keeps destructive
	// emits from clobbering user code — the user sees a warning and
	// the file stays intact rather than losing content.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() {}", SourceFile: "test.go",
	})
	// Bar exists in DB but not on disk → merge bails → regenerate runs.
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Bar", Kind: "function", Exported: true,
		Body: "func Bar() int { return 42 }", SourceFile: "test.go",
	})

	outDir := t.TempDir()
	// Disk has init (schema can't round-trip) and Baz (hand-edited,
	// not yet ingested). Both must survive.
	existing := []byte(`package test

func init() {}

func Foo() {}

func Baz() string { return "hand-edited" }
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
	s := string(data)
	if !strings.Contains(s, "func init()") {
		t.Fatalf("init() was destroyed by regenerate — safety net failed:\n%s", s)
	}
	if !strings.Contains(s, "func Baz()") {
		t.Fatalf("hand-edited Baz was destroyed by regenerate — safety net failed:\n%s", s)
	}
	// File should be untouched — matches the "existing" byte-for-byte.
	if string(data) != string(existing) {
		t.Fatalf("file content drifted when safety net should have blocked:\nwant:\n%s\ngot:\n%s",
			existing, data)
	}
}

func TestEmitOptsAllowedRemovalsUnblocksIntentionalDelete(t *testing.T) {
	// Regression coverage for the watch-vs-delete race
	// (project_defn_watch_delete_race memory): when a caller has
	// intentionally deleted a def from the DB and passes its name in
	// Opts.AllowedRemovals, safeWriteGoFile should stop guarding it and
	// let the write land. Without this fix the delete silently persists
	// in the DB but never reaches disk, and watchFiles resurrects it on
	// the next tick.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Keep", Kind: "function", Exported: true,
		Body: "func Keep() {}", SourceFile: "test.go",
	})
	// Note: "Dropped" is intentionally NOT in the DB — that's the state
	// after code(op:"delete") ran. Disk still has it, though.

	outDir := t.TempDir()
	existing := []byte(`package test

func Keep() {}

func Dropped() {}
`)
	if err := os.WriteFile(filepath.Join(outDir, "test.go"), existing, 0644); err != nil {
		t.Fatal(err)
	}

	if err := EmitWithOpts(db, outDir, Opts{AllowedRemovals: []string{"Dropped"}}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if strings.Contains(s, "Dropped") {
		t.Fatalf("Dropped was not removed from disk despite whitelist — safety-net still blocking:\n%s", s)
	}
	if !strings.Contains(s, "func Keep()") {
		t.Fatalf("Keep was destroyed alongside Dropped — whitelist over-broad:\n%s", s)
	}
}

func TestEmitOptsAllowedRemovalsDoesNotWhitelistOtherLosses(t *testing.T) {
	// After the #163 fix, mergeDeclsIntoSource no longer bails on
	// unmatched wants that aren't in AllowedAdds — it silently skips
	// them, leaving on-disk decls untouched. So the safety-net path
	// no longer triggers on "DB has drift" alone. The real data-loss
	// safety (safeWriteGoFile) still runs and refuses if any actual
	// on-disk decl would be dropped without whitelist coverage.
	//
	// Under this contract: DB has Keep + NewInDB (drift), disk has
	// [init, Keep, Dropped], AllowedRemovals=[Dropped]. Expected
	// merge behavior: replace Keep in place, remove Dropped (allowed),
	// leave init untouched (not in wants), skip NewInDB (drift). Net
	// result: [init, Keep]. init survived — the invariant this test
	// really cares about.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Keep", Kind: "function", Exported: true,
		Body: "func Keep() {}", SourceFile: "test.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "NewInDB", Kind: "function", Exported: true,
		Body: "func NewInDB() {}", SourceFile: "test.go",
	})

	outDir := t.TempDir()
	existing := []byte(`package test

func init() {}

func Keep() {}

func Dropped() {}
`)
	if err := os.WriteFile(filepath.Join(outDir, "test.go"), existing, 0644); err != nil {
		t.Fatal(err)
	}

	if err := EmitWithOpts(db, outDir, Opts{AllowedRemovals: []string{"Dropped"}}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, "func init()") {
		t.Fatalf("init() dropped — safety net failed on the real data-loss case:\n%s", got)
	}
	if !strings.Contains(got, "func Keep()") {
		t.Fatalf("Keep dropped unexpectedly:\n%s", got)
	}
	if strings.Contains(got, "func Dropped()") {
		t.Fatalf("Dropped survived despite AllowedRemovals=[Dropped]:\n%s", got)
	}
	if strings.Contains(got, "func NewInDB()") {
		t.Fatalf("NewInDB leaked to disk despite not being in AllowedAdds (drift):\n%s", got)
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

func TestEmitMergeFallsBackToRegenerateForNewDefs(t *testing.T) {
	// After #163: new defs land via Opts.AllowedAdds on the merge
	// path — no regen fallback needed for the common create case.
	// This test now asserts that intent, and also protects the
	// long-lived invariant: a newly-created DB def must reach disk.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")

	seed := "package test\n\nfunc Foo() {}\n"
	if err := db.SetFileSource(mod.ID, "test.go", seed); err != nil {
		t.Fatal(err)
	}

	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() {}", SourceFile: "test.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Bar", Kind: "function", Exported: true,
		Body: "func Bar() int { return 42 }", SourceFile: "test.go",
	})

	outDir := t.TempDir()
	if err := EmitWithOpts(db, outDir, Opts{AllowedAdds: []string{"Bar"}}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "func Bar()") {
		t.Fatalf("newly-created Bar def missing from emitted file:\n%s", s)
	}
	if !strings.Contains(s, "func Foo()") {
		t.Fatalf("Foo dropped during emit:\n%s", s)
	}
}

func TestEmitRegeneratePreservesFilePrefixAndDeclOrder(t *testing.T) {
	// Regression for the silent-data-loss bug reported by calque: when
	// the regenerate path runs (merge falls through because the DB has
	// a def with no on-disk counterpart), it must still preserve the
	// byte prefix before `package X` (build constraints, file-level
	// doc comments not directly attached to package X, free-floating
	// leading comments) and the original on-disk declaration order.
	// Before the fix, this path emitted only mod.Doc — which is empty
	// when ingest never captured the comment (file.Doc only catches
	// comments IMMEDIATELY before `package X`) — and reordered decls
	// alphabetically because GetModuleDefinitions sorts by name.
	//
	// After #163: the merge path handles new defs via AllowedAdds and
	// preserves everything by byte-splice. Test still declares the new
	// def (Gamma) via AllowedAdds so the create-add case succeeds and
	// the prefix + original order all survive naturally.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")

	seed := `//go:build linux

// Package test demonstrates a free-floating file-level comment that
// is separated from the package clause by a blank line and is
// therefore not captured as file.Doc by ingest.

package test

// Zeta runs first in source order but sorts last alphabetically.
func Zeta() {}

// Alpha sorts first alphabetically but appears second in source.
func Alpha() {}
`
	if err := db.SetFileSource(mod.ID, "test.go", seed); err != nil {
		t.Fatal(err)
	}
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Zeta", Kind: "function", Exported: true,
		Body: "func Zeta() {}", SourceFile: "test.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Alpha", Kind: "function", Exported: true,
		Body: "func Alpha() {}", SourceFile: "test.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Gamma", Kind: "function", Exported: true,
		Body: "func Gamma() int { return 7 }", SourceFile: "test.go",
	})

	outDir := t.TempDir()
	if err := EmitWithOpts(db, outDir, Opts{AllowedAdds: []string{"Gamma"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	if !strings.Contains(got, "//go:build linux") {
		t.Fatalf("//go:build constraint was lost:\n%s", got)
	}
	if !strings.Contains(got, "Package test demonstrates a free-floating") {
		t.Fatalf("file-level doc comment (not bound to package X) was lost:\n%s", got)
	}
	zetaIdx := strings.Index(got, "func Zeta")
	alphaIdx := strings.Index(got, "func Alpha")
	gammaIdx := strings.Index(got, "func Gamma")
	if zetaIdx < 0 || alphaIdx < 0 || gammaIdx < 0 {
		t.Fatalf("missing decl in output:\n%s", got)
	}
	if zetaIdx > alphaIdx {
		t.Fatalf("on-disk decl order not preserved: Alpha should appear AFTER Zeta:\n%s", got)
	}
	if gammaIdx < zetaIdx || gammaIdx < alphaIdx {
		t.Fatalf("new def Gamma should appear after the on-disk decls:\n%s", got)
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
		Body:       "const (\n\tRed = iota + 1\n\tGreen\n\tBlue\n\tYellow\n)",
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
}

func TestEmitFiltersBlankEmbedWithoutDirective(t *testing.T) {
	// Imports are stored per-module: every file in the package gets
	// the union. `_ "embed"` is meaningful only in files with a
	// //go:embed directive — emitting it elsewhere injects spurious
	// imports that goimports won't strip (blank imports are kept on
	// purpose for side-effect loaders). Filter it out for files with
	// no //go:embed.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test/pkg", "pkg", "")
	db.EnsureModule("example.com/test/other", "other", "")

	db.SetImports(mod.ID, []store.Import{
		{ModuleID: mod.ID, ImportedPath: "fmt"},
		{ModuleID: mod.ID, ImportedPath: "embed", Alias: "_"},
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() { fmt.Println() }", SourceFile: "pkg.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "pkg", "pkg.go"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, `_ "embed"`) {
		t.Fatalf("spurious `_ \"embed\"` survived emit in a file with no //go:embed directive:\n%s", content)
	}
}

func TestEmitKeepsBlankEmbedWhenDefHasDirective(t *testing.T) {
	// Counterpart to TestEmitFiltersBlankEmbedWithoutDirective: when
	// a def body carries //go:embed, the blank embed import must
	// survive in that file.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test/pkg", "pkg", "")
	db.EnsureModule("example.com/test/other", "other", "")

	db.SetImports(mod.ID, []store.Import{
		{ModuleID: mod.ID, ImportedPath: "embed", Alias: "_"},
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "data", Kind: "var", Exported: false,
		Body: "//go:embed file.txt\nvar data string", SourceFile: "pkg.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "pkg", "pkg.go"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, `_ "embed"`) {
		t.Fatalf("blank embed import was wrongly filtered from a file with //go:embed:\n%s", content)
	}
}

func TestEmitPackageDocNotDuplicatedAcrossFiles(t *testing.T) {
	// Regression: mod.Doc is stored at module level, and emit used to
	// auto-attach it to the first non-test file iterated from a Go map
	// (non-deterministic). If a different file in the package already
	// carried the doc via prefix preservation, both ended up with it.
	// Fix: scan all files first; if any already carries the doc, skip
	// auto-attach everywhere.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "Package test is the canonical doc.")

	// File A already carries the package doc in its raw source.
	rawA := `// Package test is the canonical doc.
package test

func A() {}
`
	if err := db.SetFileSource(mod.ID, "a.go", rawA); err != nil {
		t.Fatal(err)
	}
	rawB := `package test

func B() {}
`
	if err := db.SetFileSource(mod.ID, "b.go", rawB); err != nil {
		t.Fatal(err)
	}
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "A", Kind: "function", Exported: true,
		Body: "func A() {}", SourceFile: "a.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "B", Kind: "function", Exported: true,
		Body: "func B() {}", SourceFile: "b.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}

	dataA, _ := os.ReadFile(filepath.Join(outDir, "a.go"))
	dataB, _ := os.ReadFile(filepath.Join(outDir, "b.go"))
	a, b := string(dataA), string(dataB)

	if !strings.Contains(a, "Package test is the canonical doc.") {
		t.Fatalf("a.go lost its package doc:\n%s", a)
	}
	if strings.Contains(b, "Package test is the canonical doc.") {
		t.Fatalf("b.go was wrongly given a duplicate of the package doc:\n%s", b)
	}
}

func TestEmitMultiLinePackageDocNotDuplicated(t *testing.T) {
	// Stronger version of TestEmitPackageDocNotDuplicatedAcrossFiles:
	// uses a multi-line package doc (the realistic case for real
	// packages) and proves the parser-backed sourceHasPackageDoc check
	// matches the full doc — not just its first line.
	db := testDB(t)
	doc := "Package multi is the canonical multi-line doc.\n\nIt spans paragraphs and includes blank // lines."
	mod, _ := db.EnsureModule("example.com/multi", "multi", doc)

	rawA := `// Package multi is the canonical multi-line doc.
//
// It spans paragraphs and includes blank // lines.
package multi

func A() {}
`
	if err := db.SetFileSource(mod.ID, "a.go", rawA); err != nil {
		t.Fatal(err)
	}
	rawB := `package multi

func B() {}
`
	if err := db.SetFileSource(mod.ID, "b.go", rawB); err != nil {
		t.Fatal(err)
	}
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "A", Kind: "function", Exported: true,
		Body: "func A() {}", SourceFile: "a.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "B", Kind: "function", Exported: true,
		Body: "func B() {}", SourceFile: "b.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(outDir, "a.go"))
	b, _ := os.ReadFile(filepath.Join(outDir, "b.go"))
	if !strings.Contains(string(a), "It spans paragraphs") {
		t.Fatalf("a.go lost the multi-line package doc:\n%s", a)
	}
	if strings.Contains(string(b), "Package multi is the canonical") {
		t.Fatalf("b.go was given a duplicate of the multi-line package doc:\n%s", b)
	}
}

func TestEmitAttachesPackageDocWhenNoFileCarriesIt(t *testing.T) {
	// Fresh emit to an empty dir with mod.Doc set: no file's existing
	// source carries the doc, so emit attaches it to the alphabetically-
	// first non-test file (deterministic fallback) rather than silently
	// dropping it. b_test.go sorts before z.go but is excluded; z.go
	// sorts after a.go alphabetically so a.go gets it.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "Package test docs.")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Alpha", Kind: "function", Exported: true,
		Body: "func Alpha() {}", SourceFile: "a.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Zeta", Kind: "function", Exported: true,
		Body: "func Zeta() {}", SourceFile: "z.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}
	dataA, _ := os.ReadFile(filepath.Join(outDir, "a.go"))
	dataZ, _ := os.ReadFile(filepath.Join(outDir, "z.go"))
	a, z := string(dataA), string(dataZ)

	if !strings.Contains(a, "Package test docs.") {
		t.Fatalf("a.go should carry the package doc (alphabetically first):\n%s", a)
	}
	if strings.Contains(z, "Package test docs.") {
		t.Fatalf("z.go should NOT carry the package doc (a.go has it):\n%s", z)
	}
}

func TestEmitPrefersDiskWhenFileSourcesStale(t *testing.T) {
	// Regression: a user's built-in Edit lands on disk before defn's
	// file_sources knows about it (built-in tools bypass MCP sync).
	// If file_sources is stale and emit preferred it over disk, the
	// user's edit (e.g. a newly-added package header) would be erased
	// the next time emit ran. Disk-first preserves the user's bytes.
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")

	// file_sources represents the OLD state (no header).
	rawStale := `package test

func Foo() {}
`
	if err := db.SetFileSource(mod.ID, "test.go", rawStale); err != nil {
		t.Fatal(err)
	}
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() {}", SourceFile: "test.go",
	})

	outDir := t.TempDir()
	// Disk has the user's fresh edit: a file-level doc comment NOT
	// bound to `package X` (blank line separates them), so it lives
	// in the prefix and is not captured by file.Doc on ingest.
	diskWithHeader := `//go:build linux

// User-added header that file_sources doesn't know about yet.

package test

func Foo() {}
`
	if err := os.WriteFile(filepath.Join(outDir, "test.go"), []byte(diskWithHeader), 0644); err != nil {
		t.Fatal(err)
	}

	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, "test.go"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)

	if !strings.Contains(got, "//go:build linux") {
		t.Fatalf("user's build tag was erased by stale file_sources:\n%s", got)
	}
	if !strings.Contains(got, "User-added header that file_sources doesn't know about yet") {
		t.Fatalf("user's added header was erased by stale file_sources:\n%s", got)
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

// TestEmitOptsTouchedFilesFiltersModuleFiles covers the #117 scoped
// emit path: TouchedFiles restricts which files get written, leaving
// others untouched on disk. Sibling files in the same module that
// aren't in the touched set must NOT be rewritten.
func TestEmitOptsTouchedFilesFiltersModuleFiles(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test/pkg", "pkg", "")

	// Two defs in two different files within the same module.
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Touched", Kind: "function", Exported: true,
		Body: "func Touched() {}", SourceFile: "pkg/touched.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Untouched", Kind: "function", Exported: true,
		Body: "func Untouched() {}", SourceFile: "pkg/untouched.go",
	})

	outDir := t.TempDir()
	// Full emit first to populate baseline.
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}
	baselineUntouched, err := os.Stat(filepath.Join(outDir, "pkg", "untouched.go"))
	if err != nil {
		t.Fatalf("baseline untouched.go missing: %v", err)
	}
	baselineModTime := baselineUntouched.ModTime()

	// Rewrite Touched's body, then scoped emit only touched.go. untouched.go
	// must NOT be rewritten (mtime unchanged).
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Touched", Kind: "function", Exported: true,
		Body: "func Touched() { /* changed */ }", SourceFile: "pkg/touched.go",
	})
	if err := EmitWithOpts(db, outDir, Opts{TouchedFiles: []string{"pkg/touched.go"}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(outDir, "pkg", "untouched.go"))
	if err != nil {
		t.Fatalf("untouched.go disappeared: %v", err)
	}
	if after.ModTime() != baselineModTime {
		t.Errorf("scoped emit rewrote untouched.go — mtime changed %s → %s",
			baselineModTime, after.ModTime())
	}
	// touched.go must reflect the new body.
	touchedContent, err := os.ReadFile(filepath.Join(outDir, "pkg", "touched.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(touchedContent), "/* changed */") {
		t.Errorf("touched.go missing rewritten body:\n%s", string(touchedContent))
	}
}

// TestEmitScopedAlwaysWritesProjectFiles covers the 8ce7427 followup:
// scoped emit into a fresh empty tempdir must still write go.mod/go.sum,
// otherwise the tree can't build. Earlier #117 skipped project_files on
// scoped to save the write; that broke the ceiling measure path.
func TestEmitScopedAlwaysWritesProjectFiles(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test/pkg", "pkg", "")
	db.SetProjectFile("go.mod", "module example.com/test\n\ngo 1.21\n")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "F", Kind: "function", Exported: true,
		Body: "func F() {}", SourceFile: "pkg/f.go",
	})

	outDir := t.TempDir()
	// Scoped emit into empty dir — must still write go.mod even though it
	// isn't in TouchedFiles.
	if err := EmitWithOpts(db, outDir, Opts{TouchedFiles: []string{"pkg/f.go"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "go.mod")); err != nil {
		t.Fatalf("scoped emit skipped go.mod on fresh tempdir: %v", err)
	}
}

// TestEmitSingleModulePreservesSourceFilePath covers #120: single-module
// projects where module.Path == moduleRoot must NOT drop the source_file
// directory prefix. Regression: cli/cli's "command/root.go" was being
// written to outDir/root.go because relPath collapsed to "." and only the
// basename survived.
func TestEmitSingleModulePreservesSourceFilePath(t *testing.T) {
	db := testDB(t)
	// Single module whose Path is itself a subdirectory-shaped path.
	// detectModuleRoot on a single module returns that module's Path as the
	// prefix — so relPath = "", pkgDir = outDir. Pre-fix, basename joining
	// dropped "command/". The #120 fix uses source_file directly under outDir
	// when it has a directory prefix.
	mod, _ := db.EnsureModule("github.com/cli/cli/command", "command", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Root", Kind: "function", Exported: true,
		Body: "func Root() {}", SourceFile: "command/root.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(outDir, "command", "root.go")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("expected %s, not found: %v", wantPath, err)
	}
	// The wrong location (basename only, at outDir root) must NOT exist.
	if _, err := os.Stat(filepath.Join(outDir, "root.go")); err == nil {
		t.Errorf("emit still wrote root.go at outDir root (pre-#120 behavior)")
	}
}

// TestMergeDeclsIntoSource_PreservesFloatingCommentsOnNewDefAdd is the
// regression test for #162. When a new def is added to a file whose
// existing source has floating (blank-line-separated) comments between
// top-level decls, those comments must survive the merge. Prior
// behavior: any unmatched want (i.e., new-def add) forced fall-through
// to full-file regen, which discarded floating comments. Fix: merge
// splices existing decls in place AND appends new-def bodies at end
// of file, so floating-comment byte positions between existing decls
// are outside every replacement range and survive intact.
//
// This reproduces the exact shape hit three times in the #160 arc
// (searchShapedSQLRedirects / outlineCalleeCap / impactCallerCap):
// a floating comment sits above a var/const block, and a new
// unrelated function is added via `code op:create`.
func TestMergeDeclsIntoSource_PreservesFloatingCommentsOnNewDefAdd(t *testing.T) {
	existing := []byte(`package p

// FloatingDocForVarBlock describes what the following block is for,
// separated from it by a blank line so parser.ParseComments leaves it
// in f.Comments rather than attaching it as VarBlock's Doc.
var (
	X = 1
	Y = 2
)

// FloatingDocForConstBlock — same shape, different kind.
const (
	A = "a"
	B = "b"
)

func Existing() int { return X + Y }
`)

	// One existing def (Existing) with an updated body, plus one NEW
	// def (New) with no on-disk counterpart. Before the fix, the New
	// addition triggered regen and dropped both floating comments.
	defs := []store.Definition{
		{Name: "Existing", Kind: "function", Body: "func Existing() int {\n\treturn X + Y + 1\n}"},
		{Name: "New", Kind: "function", Body: "// New was added via code op:create.\nfunc New() string { return \"new\" }"},
	}

	merged, ok := mergeDeclsIntoSource(existing, defs, nil, []string{"New"})
	if !ok {
		t.Fatalf("mergeDeclsIntoSource returned ok=false (expected true — fix should handle new-def add without falling through to regen)")
	}
	got := string(merged)

	// Floating comments must survive.
	for _, want := range []string{
		"FloatingDocForVarBlock describes what the following block is for",
		"FloatingDocForConstBlock — same shape",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("floating comment lost: %q not in merged output", want)
		}
	}

	// Existing def's new body was patched.
	if !strings.Contains(got, "return X + Y + 1") {
		t.Errorf("Existing body not patched: merged does not contain updated body")
	}

	// New def appended.
	if !strings.Contains(got, "func New() string") {
		t.Errorf("New def not appended to merged output")
	}
}

// TestMergeDeclsIntoSource_FloatingCommentSurvivesGroupedSpecReplacement
// isolates the #163 regression: even with AllowedAdds set, floating
// comments above a grouped var(...) or const(...) block get lost
// when there's ALSO a per-spec replacement inside that block. Prior
// #162 test only exercised untouched grouped blocks; this covers the
// case where the merge patches inside the block too.
//
// Failing shape mirrors internal/mcp/server.go: floating comment
// above `var (` block whose specs are individually stored in the DB.
// When a new def is added elsewhere in the file AND the specs get
// replaced (even with identical content), the floating comment above
// the block disappears.
func TestMergeDeclsIntoSource_FloatingCommentSurvivesGroupedSpecReplacement(t *testing.T) {
	existing := []byte(`package p

// FloatingDocAboveVarBlock — this comment is separated from the var
// by a blank line, so parser.ParseComments leaves it in f.Comments
// rather than attaching it to VarBlock's Doc.
var (
	X = 1
	Y = 2
)

func Existing() int { return X + Y }
`)

	// DB has 3 defs — the two grouped-spec vars X and Y, and Existing.
	// Plus one NEW def (New) being added via code op:create.
	defs := []store.Definition{
		{Name: "X", Kind: "var", Body: "X = 1"},
		{Name: "Y", Kind: "var", Body: "Y = 2"},
		{Name: "Existing", Kind: "function", Body: "func Existing() int { return X + Y }"},
		{Name: "New", Kind: "function", Body: "func New() string { return \"new\" }"},
	}

	merged, ok := mergeDeclsIntoSource(existing, defs, nil, []string{"New"})
	if !ok {
		t.Fatalf("mergeDeclsIntoSource returned ok=false")
	}
	got := string(merged)

	if !strings.Contains(got, "FloatingDocAboveVarBlock") {
		t.Errorf("floating comment above var block lost — this is #163\n\nmerged output:\n%s", got)
	}
	if !strings.Contains(got, "func New()") {
		t.Errorf("New def not appended")
	}
}

// TestMergeDeclsIntoSource_OrphanDefTriggersRegenDropsComment is the
// #163 root cause. The failing scenario in the real workflow:
//
//   - DB accumulates an "orphan" def (recorded via UpsertDefinition but
//     the emit that would have written it to disk failed/rolled back
//     for some earlier op).
//   - A LATER code(op:"create") for a DIFFERENT new def declares
//     AllowedAdds=[newName] — orphan name isn't in there.
//   - mergeDeclsIntoSource sees the orphan as an unmatched want with
//     no AllowedAdds entry → returns false → writeFile falls to regen
//     → regen drops floating comments.
//
// The fix: mergeDeclsIntoSource should treat unmatched-and-not-allowed
// as "the caller doesn't own this def; leave it alone" rather than
// bailing. Skip the orphan (don't try to add or remove it) and let
// the merge succeed with the caller's actual intent — the disk file
// stays consistent with what the user asked for.
func TestMergeDeclsIntoSource_OrphanDefTriggersRegenDropsComment(t *testing.T) {
	existing := []byte(`package p

// FloatingDoc above the var block.
var (
	X = 1
)

func Existing() int { return X }
`)

	// DB has 3 defs — X and Existing (both on disk) PLUS Orphan
	// (a def someone earlier upsert'd but never got written to disk).
	// Now a NEW def is being added via code op:create.
	defs := []store.Definition{
		{Name: "X", Kind: "var", Body: "X = 1"},
		{Name: "Existing", Kind: "function", Body: "func Existing() int { return X }"},
		{Name: "Orphan", Kind: "function", Body: "func Orphan() {}"},
		{Name: "New", Kind: "function", Body: "func New() {}"},
	}
	// AllowedAdds only declares the CURRENT caller's intent (New);
	// Orphan is drift and shouldn't be added.
	merged, ok := mergeDeclsIntoSource(existing, defs, nil, []string{"New"})
	if !ok {
		t.Fatalf("mergeDeclsIntoSource returned ok=false when orphan Present — this is #163: fix should skip orphan, not bail")
	}
	got := string(merged)
	if !strings.Contains(got, "FloatingDoc above the var block") {
		t.Errorf("floating comment lost when orphan def in DB\n\nmerged:\n%s", got)
	}
	if !strings.Contains(got, "func New()") {
		t.Errorf("New def not appended")
	}
	// Orphan MUST NOT appear — it wasn't allowed-add, and disk didn't have it.
	if strings.Contains(got, "func Orphan()") {
		t.Errorf("Orphan def leaked into disk despite not being in AllowedAdds")
	}
}
