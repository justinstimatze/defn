package emit

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

	if _, err := EmitWithOpts(db, outDir, Opts{AllowedRemovals: []string{"Dropped"}}); err != nil {
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

	if _, err := EmitWithOpts(db, outDir, Opts{AllowedRemovals: []string{"Dropped"}}); err != nil {
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
	if _, err := EmitWithOpts(db, outDir, Opts{AllowedAdds: []string{"Bar"}}); err != nil {
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
	if _, err := EmitWithOpts(db, outDir, Opts{AllowedAdds: []string{"Gamma"}}); err != nil {
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
	if _, err := EmitWithOpts(db, outDir, Opts{TouchedFiles: []string{"pkg/touched.go"}}); err != nil {
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
	if _, err := EmitWithOpts(db, outDir, Opts{TouchedFiles: []string{"pkg/f.go"}}); err != nil {
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

	merged, ok, _ := mergeDeclsIntoSource(existing, defs, nil, []string{"New"})
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

	merged, ok, _ := mergeDeclsIntoSource(existing, defs, nil, []string{"New"})
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
	merged, ok, _ := mergeDeclsIntoSource(existing, defs, nil, []string{"New"})
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

func TestEmitLogsDiskDriftWarning(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")

	rawStale := "package test\n\nfunc Foo() {}\n"
	if err := db.SetFileSource(mod.ID, "test.go", rawStale); err != nil {
		t.Fatal(err)
	}
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() {}", SourceFile: "test.go",
	})

	outDir := t.TempDir()
	// Disk has content that differs from what SetFileSource stored --
	// simulates a native Edit landing outside defn's DB (e.g. via the
	// #205 sentinel bypass) without a follow-up code(op:"sync").
	diskDrifted := "package test\n\n// Added outside defn.\nfunc Foo() {}\n"
	if err := os.WriteFile(filepath.Join(outDir, "test.go"), []byte(diskDrifted), 0644); err != nil {
		t.Fatal(err)
	}

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	emitErr := Emit(db, outDir)

	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	io.Copy(&buf, r)

	if emitErr != nil {
		t.Fatal(emitErr)
	}
	if !strings.Contains(buf.String(), "disk drift") {
		t.Fatalf("expected disk-drift warning on stderr, got: %q", buf.String())
	}
}

// TestMergeDeclsIntoSource_UnmatchedWantSurfacedNotSilent is the #218
// regression: gemot-2847127 reported op:edit resolving a method to a
// stale pre-sync id, reporting "Updated" success and bumping the graph
// hash, but never writing the new body to disk. Root cause: when a
// requested def's (name, receiver) identity doesn't match any on-disk
// declaration -- e.g. a stale DB row with the wrong receiver -- the
// merge used to splice in everything that DID match and silently drop
// the rest with no signal at all (#163's "skip silently" was silent
// all the way up the stack, not just to the file). ok must still be
// true (Foo matches and gets its update), but unmatched must report
// the def that didn't.
func TestMergeDeclsIntoSource_UnmatchedWantSurfacedNotSilent(t *testing.T) {
	existing := []byte(`package p

func Foo() {}

func Bar() {}
`)
	defs := []store.Definition{
		{Name: "Foo", Kind: "function", Body: "func Foo() { /* updated */ }"},
		// Stale DB row: on disk Bar is a free function, but this def
		// thinks it's a method on *Baz -- simulates a resolved-but-stale
		// id whose identity no longer matches its real on-disk decl.
		{Name: "Bar", Kind: "method", Receiver: "Baz", Body: "func (b *Baz) Bar() { /* should not land */ }"},
	}
	merged, ok, unmatched := mergeDeclsIntoSource(existing, defs, nil, nil)
	if !ok {
		t.Fatalf("expected ok=true (Foo matches an on-disk decl), got false")
	}
	got := string(merged)
	if !strings.Contains(got, "/* updated */") {
		t.Errorf("expected Foo's body to be updated in the merged result:\n%s", got)
	}
	if strings.Contains(got, "should not land") {
		t.Errorf("Bar's stale-identity body must not appear anywhere in the merged result:\n%s", got)
	}
	if len(unmatched) != 1 || unmatched[0] != "Baz.Bar" {
		t.Fatalf("expected unmatched=[%q], got %v", "Baz.Bar", unmatched)
	}
}

// TestWriteFile_UnmatchedWantReturnsWarning is the #218 regression one
// layer up: writeFile must surface mergeDeclsIntoSource's unmatched
// names as a non-empty warning string, not just a server-side stderr
// log line nobody watching the MCP response would ever see.
func TestWriteFile_UnmatchedWantReturnsWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.go")
	existing := []byte(`package p

func Foo() {}

func Bar() {}
`)
	if err := os.WriteFile(path, existing, 0644); err != nil {
		t.Fatal(err)
	}
	defs := []store.Definition{
		{Name: "Foo", Kind: "function", Body: "func Foo() { /* updated */ }"},
		{Name: "Bar", Kind: "method", Receiver: "Baz", Body: "func (b *Baz) Bar() {}"},
	}
	_, warning, err := writeFile(path, "p", "example.com/p", "", nil, defs, nil, nil, nil)
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	if warning == "" {
		t.Fatal("expected a non-empty warning when a requested def couldn't be matched on disk, got \"\"")
	}
	if !strings.Contains(warning, "Baz.Bar") {
		t.Errorf("expected warning to name the unmatched def, got: %s", warning)
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(onDisk), "/* updated */") {
		t.Errorf("expected Foo's matched update to still land on disk despite Bar's mismatch:\n%s", onDisk)
	}
}

// TestEmitProjectFileDiskDriftWarning is the #217/#356 regression:
// emit's project-files loop (go.mod, go.sum, and any go:embed-tracked
// file like schema_sqlite.sql) must never clobber a manually-edited
// tracked project file with defn's own stale DB blob -- #217 initially
// only added a warning for this (still logged here), but two real
// trajectories (2026-08-28/29 bug reports) hit the actual data loss:
// a legitimate go.mod edit (a new dependency, `go mod tidy`) silently
// reverted on the very next unrelated mutation's auto-emit, discarding
// even already-committed changes. Disk now wins on drift and the DB's
// row heals to match, mirroring ensureFresh's treatment of .go files.
func TestEmitProjectFileDiskDriftWarning(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test", "test", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: "func Foo() {}", SourceFile: "test.go",
	})
	if err := db.SetProjectFile("go.mod", "module example.com/test\n\ngo 1.22\n"); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	// Disk has content that differs from the DB's stored project_files
	// row -- simulates a manual edit to a tracked project file made
	// outside defn, with no follow-up full ingest to refresh the row.
	driftedGoMod := "module example.com/test\n\ngo 1.22\n\nrequire foo v1.0.0\n"
	if err := os.WriteFile(filepath.Join(outDir, "go.mod"), []byte(driftedGoMod), 0644); err != nil {
		t.Fatal(err)
	}

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w

	emitErr := Emit(db, outDir)

	w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	io.Copy(&buf, r)

	if emitErr != nil {
		t.Fatal(emitErr)
	}
	if !strings.Contains(buf.String(), "disk drift") {
		t.Fatalf("expected disk-drift warning on stderr for clobbered project file, got: %q", buf.String())
	}

	// #356: disk wins on drift -- the on-disk edit is preserved, not
	// clobbered by the DB's stale blob.
	final, _ := os.ReadFile(filepath.Join(outDir, "go.mod"))
	if !strings.Contains(string(final), "require foo") {
		t.Fatalf("expected on-disk content to survive drift, got:\n%s", final)
	}

	// And the DB's own row heals to match, so a later fresh-tempdir emit
	// carries the corrected content forward instead of re-reverting it.
	healed, err := db.GetProjectFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(healed, "require foo") {
		t.Fatalf("expected DB's project_files row to heal from disk, got:\n%s", healed)
	}
}

func TestEmitOptsSkipGoimportsSkipsTheGoimportsPass(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test/pkg", "pkg", "")
	db.EnsureModule("example.com/test/other", "other", "")

	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body:       "func Foo() { strings.TrimSpace(\"x\") }",
		SourceFile: "pkg.go",
	})

	outDir := t.TempDir()
	if _, err := EmitWithOpts(db, outDir, Opts{
		TouchedFiles:   []string{"pkg.go"},
		GoimportsFiles: []string{"pkg.go"},
		SkipGoimports:  true,
	}); err != nil {
		t.Fatalf("EmitWithOpts: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "pkg", "pkg.go"))
	if err != nil {
		t.Fatalf("file not found: %v", err)
	}
	content := string(data)
	if strings.Contains(content, `"strings"`) {
		t.Fatalf("goimports ran despite SkipGoimports:true -- strings import got added:\n%s", content)
	}
}

// TestEmitExcludesFieldKindFromTopLevelDecls is the regression test for
// the #11 emit-corruption risk: a "field" kind definition's Body (e.g.
// "Port int") is not a standalone top-level declaration -- it only
// exists inside its struct's braces, which the struct's own Body
// already contains as text. Emit must exclude field defs from the
// per-file decl assembly, or it would inject a bare field line as a
// floating top-level statement and produce unparseable Go.
func TestEmitExcludesFieldKindFromTopLevelDecls(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/fields", "fields", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Config", Kind: "type", Exported: true,
		Body: "type Config struct {\n\tPort int\n}", SourceFile: "fields.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Port", Kind: "field", Exported: true,
		Receiver: "Config", Signature: "int", Body: "Port int", SourceFile: "fields.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(outDir, "fields.go"))
	if err != nil {
		t.Fatalf("read emitted file: %v", err)
	}

	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, "fields.go", out, 0); err != nil {
		t.Fatalf("emitted file is not valid Go: %v\n--- content ---\n%s", err, out)
	}
	if strings.Count(string(out), "Port int") != 1 {
		t.Errorf("expected exactly one 'Port int' (inside the struct, not floating), got:\n%s", out)
	}
}

func TestEmitZeroDefPhantomModuleDoesNotDeleteRealFile(t *testing.T) {
	// Regression test for task #239: a module row can reach zero defs
	// without defn ever having written real content for it -- e.g. an
	// orphaned module row left behind by a failed code(op:"create")
	// into a new directory (see TestHandleCreateFailedBuildDoesNotOrphanModule
	// in internal/mcp). Guessing a filename from mod.Name and deleting
	// it on the next unscoped emit turned that orphan into real data
	// loss: resolver/passthrough/passthrough.go and four other files
	// were deleted from a real grpc-go checkout this way, despite defn
	// never having ingested a single def from any of them.
	db := testDB(t)
	// A real, legitimately-managed root module establishes a moduleRoot
	// shorter than the phantom's path below -- without this, DetectModuleRoot
	// on a single-module DB collapses moduleRoot to the phantom's own
	// path, relPath goes empty, and the old code's cleanup guard
	// (`relPath != "" && relPath != "."`) never fires at all. That shape
	// doesn't match the real bug: a real project always has other real
	// modules alongside an orphaned one.
	rootMod, err := db.EnsureModule("example.com/test", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertDefinition(&store.Definition{
		ModuleID: rootMod.ID, Name: "Real", Kind: "function", Exported: true,
		Body: "func Real() {}", SourceFile: "test.go",
	}); err != nil {
		t.Fatal(err)
	}
	// No definitions, no file_sources -- exactly the phantom-module
	// shape: EnsureModule ran, nothing else ever did.
	if _, err := db.EnsureModule("example.com/test/pkg", "pkg", ""); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	pkgDir := filepath.Join(outDir, "pkg")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	// A real file defn never touched, sitting at the guessed
	// mod.Name-derived path (pkg.go) the old code would have deleted.
	realContent := []byte("package pkg\n\nfunc RealFunc() int { return 1 }\n")
	realPath := filepath.Join(pkgDir, "pkg.go")
	if err := os.WriteFile(realPath, realContent, 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := EmitWithOpts(db, outDir, Opts{}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	got, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("real file was deleted by the phantom module's zero-defs cleanup: %v", err)
	}
	if string(got) != string(realContent) {
		t.Fatalf("real file was modified:\n%s", got)
	}
}

func TestEmitZeroDefModuleNeverDeletesEvenWithFileSources(t *testing.T) {
	// file_sources being populated used to be treated as proof defn
	// legitimately managed this file, safe to delete once its defs hit
	// zero. Task #239's real root cause disproved that: the incremental
	// ingest fast path can populate file_sources for a file under the
	// WRONG module path (nested-module directories walkGoFiles now skips
	// -- see TestWalkGoFilesSkipsNestedModule) and never reliably capture
	// its definitions. file_sources alone is not proof of anything.
	// emitModule must never delete on zero defs, full stop -- a user who
	// wants a file gone after deleting its last definition can rm it.
	db := testDB(t)
	mod, err := db.EnsureModule("example.com/test/pkg", "pkg", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileSource(mod.ID, "pkg/pkg.go", "package pkg\n"); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	pkgDir := filepath.Join(outDir, "pkg")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	trackedPath := filepath.Join(pkgDir, "pkg.go")
	content := []byte("package pkg\n")
	if err := os.WriteFile(trackedPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := EmitWithOpts(db, outDir, Opts{}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	got, err := os.ReadFile(trackedPath)
	if err != nil {
		t.Fatalf("file was deleted despite the never-delete policy: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("file was modified:\n%s", got)
	}
}

// TestEmitModule_SameBasenameDifferentPackagesDontCollide guards a
// severe data-corruption bug found via a real cli/cli head-to-head-go
// trajectory (cli-2671, 2026-08-09): pkg/cmd/gist/create/create.go and
// pkg/cmd/repo/create/create.go share the basename "create.go". The
// fixture below simulates the corruption with one store.Module and
// two SourceFile subdirectories for simplicity; real ingest never
// actually puts two packages under one Module row (store.Module is
// one row per Go package, keyed on pkg.PkgPath -- corrected
// 2026-08-09, docs/lessons-learned.md, after the opposite claim
// shipped in this comment and the fix commit for a full day). The
// real collision path is emitModule's per-package output directory
// (pkgDir) landing on the same path for two different packages, then
// grouping definitions by filepath.Base(SourceFile) instead of the
// full project-relative path -- so both files' definitions landed in
// ONE map bucket keyed "create.go" regardless of which package(s)
// contributed them. Only one of the two real files ended up written
// (with the OTHER package's definitions merged into it); the sibling
// file was silently skipped. Live symptom: editing repo/create's
// createRun overwrote gist/create's createRun with repo's body and
// imports, corrupting a file the agent never touched or referenced.
func TestEmitModule_SameBasenameDifferentPackagesDontCollide(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("github.com/x/y", "y", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "AlphaCreate", Kind: "function", Exported: true,
		Body: "func AlphaCreate() string {\n\treturn \"alpha\"\n}", SourceFile: "alpha/create.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "BetaCreate", Kind: "function", Exported: true,
		Body: "func BetaCreate() string {\n\treturn \"beta\"\n}", SourceFile: "beta/create.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatalf("emit: %v", err)
	}

	alphaSrc, err := os.ReadFile(filepath.Join(outDir, "alpha", "create.go"))
	if err != nil {
		t.Fatalf("alpha/create.go was never written: %v", err)
	}
	betaSrc, err := os.ReadFile(filepath.Join(outDir, "beta", "create.go"))
	if err != nil {
		t.Fatalf("beta/create.go was never written: %v", err)
	}

	if !strings.Contains(string(alphaSrc), "AlphaCreate") {
		t.Errorf("alpha/create.go missing its own AlphaCreate:\n%s", alphaSrc)
	}
	if strings.Contains(string(alphaSrc), "BetaCreate") {
		t.Errorf("alpha/create.go was corrupted with beta's BetaCreate:\n%s", alphaSrc)
	}
	if !strings.Contains(string(betaSrc), "BetaCreate") {
		t.Errorf("beta/create.go missing its own BetaCreate:\n%s", betaSrc)
	}
	if strings.Contains(string(betaSrc), "AlphaCreate") {
		t.Errorf("beta/create.go was corrupted with alpha's AlphaCreate:\n%s", betaSrc)
	}
}

// TestEmitPreservesInitAcrossSiblingFilesInSamePackage guards a severe
// data-corruption bug found via a real prometheus/prometheus head-to-head
// trajectory (prometheus-19338/17395/19184/19114, 2026-08-10): Go
// explicitly permits multiple func init() per package -- one per file is
// the normal shape for generated code (every protoc-gogo .pb.go file gets
// its own init() registering its enums/types) and for driver/plugin
// registration patterns. definitions' natural key was (module_id, name,
// kind, receiver, test) with NO source_file component, so two sibling
// files in the SAME package each declaring their own (per-file-counter)
// bare "init" collided on that key -- UpsertDefinition treated the
// second file's init() as an UPDATE of the first's row instead of a
// separate definition, silently discarding one file's init() content
// and duplicating the other's onto both files on emit. Live symptom:
// go test on packages the agent never touched panicked with "duplicate
// enum registered" / "Config named ... is already registered", because
// both on-disk files now independently called the same registration
// code at runtime. Fixed by adding source_file to the UNIQUE constraint
// (schema_sqlite.sql) and to UpsertDefinition/UpsertDefinitionsBulk's
// natural key.
func TestEmitPreservesInitAcrossSiblingFilesInSamePackage(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("github.com/x/y/pkgx", "pkgx", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "init", Kind: "function", Exported: false,
		Body: "func init() {\n\tACalls++\n}", SourceFile: "pkgx/a.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "init", Kind: "function", Exported: false,
		Body: "func init() {\n\tBCalls++\n}", SourceFile: "pkgx/b.go",
	})

	defs, err := db.FindDefinitionsByFile("pkgx", "pkgx/a.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "init" {
		t.Fatalf("a.go: expected exactly 1 init def, got %+v", defs)
	}
	aID := defs[0].ID

	defs, err = db.FindDefinitionsByFile("pkgx", "pkgx/b.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "init" {
		t.Fatalf("b.go: expected exactly 1 init def, got %+v", defs)
	}
	bID := defs[0].ID

	if aID == bID {
		t.Fatalf("a.go and b.go's init() collided onto the same definition row (id=%d) -- source_file is not part of the natural key", aID)
	}

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatalf("emit: %v", err)
	}

	aSrc, err := os.ReadFile(filepath.Join(outDir, "pkgx", "a.go"))
	if err != nil {
		t.Fatalf("pkgx/a.go was never written: %v", err)
	}
	bSrc, err := os.ReadFile(filepath.Join(outDir, "pkgx", "b.go"))
	if err != nil {
		t.Fatalf("pkgx/b.go was never written: %v", err)
	}

	if !strings.Contains(string(aSrc), "ACalls++") {
		t.Errorf("pkgx/a.go missing its own init() body:\n%s", aSrc)
	}
	if strings.Contains(string(aSrc), "BCalls++") {
		t.Errorf("pkgx/a.go was corrupted with b.go's init() body:\n%s", aSrc)
	}
	if !strings.Contains(string(bSrc), "BCalls++") {
		t.Errorf("pkgx/b.go missing its own init() body:\n%s", bSrc)
	}
	if strings.Contains(string(bSrc), "ACalls++") {
		t.Errorf("pkgx/b.go was corrupted with a.go's init() body:\n%s", bSrc)
	}

	// Both files must declare exactly one init() each -- not zero (dropped)
	// and not two (duplicated from the other file).
	if n := strings.Count(string(aSrc), "func init()"); n != 1 {
		t.Errorf("pkgx/a.go has %d init() funcs, want 1:\n%s", n, aSrc)
	}
	if n := strings.Count(string(bSrc), "func init()"); n != 1 {
		t.Errorf("pkgx/b.go has %d init() funcs, want 1:\n%s", n, bSrc)
	}
}

func TestEmitRegeneratePathDedupesCollidingLocalImportNames(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("github.com/x/pkgy", "pkgy", "")
	if err := db.SetImports(mod.ID, []store.Import{
		{ModuleID: mod.ID, ImportedPath: "context"},
		{ModuleID: mod.ID, ImportedPath: "github.com/x/ec2/types"},
		{ModuleID: mod.ID, ImportedPath: "github.com/x/msk/types"},
	}); err != nil {
		t.Fatal(err)
	}
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "UseEC2", Kind: "function", Exported: true,
		Body:       "func UseEC2(ctx context.Context) types.Instance {\n\t_ = ctx\n\treturn types.Instance{}\n}",
		SourceFile: "pkgy/ec2.go",
	})

	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatalf("emit: %v", err)
	}

	src, err := os.ReadFile(filepath.Join(outDir, "pkgy", "ec2.go"))
	if err != nil {
		t.Fatalf("pkgy/ec2.go was never written: %v", err)
	}
	s := string(src)

	hasEC2 := strings.Contains(s, "github.com/x/ec2/types")
	hasMSK := strings.Contains(s, "github.com/x/msk/types")
	if hasEC2 && hasMSK {
		t.Fatalf("pkgy/ec2.go got BOTH colliding \"types\" imports -- guaranteed \"redeclared\" compile error:\n%s", s)
	}
	if !hasEC2 && !hasMSK {
		t.Fatalf("pkgy/ec2.go got NEITHER \"types\" import -- UseEC2 references types.Instance and won't compile:\n%s", s)
	}
	if !strings.Contains(s, `"context"`) {
		t.Errorf("pkgy/ec2.go missing unrelated referenced import \"context\" -- filter over-restricted:\n%s", s)
	}
}

// TestEmitDebug_TracesKeepDropAndWriteDecisions verifies the
// DEFN_EMIT_DEBUG=1 instrumentation added 2026-08-17 (see emitDebugf's
// doc comment for the mystery it exists to help debug next time) --
// both as a regression against silently breaking the trace output,
// and as executable documentation of what a scoped emit's debug
// log actually looks like.
func TestEmitDebug_TracesKeepDropAndWriteDecisions(t *testing.T) {
	t.Setenv("DEFN_EMIT_DEBUG", "1")

	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test/pkg", "pkg", "")
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Touched", Kind: "function", Exported: true,
		Body: "func Touched() {}", SourceFile: "pkg/touched.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Untouched", Kind: "function", Exported: true,
		Body: "func Untouched() {}", SourceFile: "pkg/untouched.go",
	})
	outDir := t.TempDir()
	if err := Emit(db, outDir); err != nil {
		t.Fatal(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStderr := os.Stderr
	os.Stderr = w
	_, emitErr := EmitWithOpts(db, outDir, Opts{TouchedFiles: []string{"pkg/touched.go"}})
	w.Close()
	os.Stderr = origStderr
	if emitErr != nil {
		t.Fatal(emitErr)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"[emit-debug] scope: touchedFiles=1",
		"[emit-debug] touchedSet: [pkg/touched.go]",
		"KEEP example.com/test/pkg/pkg/touched.go",
		"DROP example.com/test/pkg/pkg/untouched.go",
		"WROTE",
		"goimports args:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected debug output to contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "WROTE") && strings.Contains(out, "untouched.go)") {
		t.Errorf("debug log claims untouched.go was written, but the scoped emit should never touch it:\n%s", out)
	}
}

// TestMergeDeclsIntoSource_RemovesFirstGroupedMemberWithoutTouchingSiblings
// is the companion direction: deleting the FIRST member of a grouped
// block used to remove the entire parenthesized GenDecl -- including
// untouched sibling specs never authorized for removal -- because the
// whole-decl shortcut matched on firstSpecName regardless of grouping.
// That got caught by safeWriteGoFile's data-loss check and refused, but
// meant "delete A" was unconditionally blocked whenever A shared a
// group with anything else.
func TestMergeDeclsIntoSource_RemovesFirstGroupedMemberWithoutTouchingSiblings(t *testing.T) {
	existing := []byte(`package p

type (
	A struct{ N int }
	B struct{ S string }
)
`)
	defs := []store.Definition{
		{Name: "B", Kind: "type", Body: "B struct{ S string }"},
	}
	merged, ok, unmatched := mergeDeclsIntoSource(existing, defs, []string{"A"}, nil)
	if !ok {
		t.Fatalf("mergeDeclsIntoSource returned ok=false, unmatched=%v", unmatched)
	}
	got := string(merged)
	if strings.Contains(got, "A struct") {
		t.Errorf("A was not removed:\n%s", got)
	}
	if !strings.Contains(got, "B struct{ S string }") {
		t.Errorf("B was incorrectly removed alongside A (first-member-removes-whole-block bug):\n%s", got)
	}
}

// TestMergeDeclsIntoSource_RemovesNonFirstGroupedMember is the
// regression for the grouped-decl delete asymmetry: the removal check
// used to only fire via a whole-GenDecl shortcut keyed on the FIRST
// spec's name (firstSpecName), so deleting a non-first member of a
// grouped type/const/var block matched nothing at all and silently
// left the on-disk decl in place while the DB believed it was gone.
func TestMergeDeclsIntoSource_RemovesNonFirstGroupedMember(t *testing.T) {
	existing := []byte(`package p

type (
	A struct{ N int }
	B struct{ S string }
)
`)
	defs := []store.Definition{
		{Name: "A", Kind: "type", Body: "A struct{ N int }"},
	}
	merged, ok, unmatched := mergeDeclsIntoSource(existing, defs, []string{"B"}, nil)
	if !ok {
		t.Fatalf("mergeDeclsIntoSource returned ok=false, unmatched=%v", unmatched)
	}
	got := string(merged)
	if strings.Contains(got, "B struct") {
		t.Errorf("B was not removed from the grouped block:\n%s", got)
	}
	if !strings.Contains(got, "A struct{ N int }") {
		t.Errorf("A was incorrectly removed alongside B:\n%s", got)
	}
}

// TestMergeDeclsIntoSource_DeletingMultiNameValueSpecRemovesItFromDisk
// covers the sibling half of the same root cause: since the old code
// never resolved a name for a multi-name spec, code(op:"delete") on one
// used to report success (DB row removed) while silently leaving the
// spec's text on disk forever.
func TestMergeDeclsIntoSource_DeletingMultiNameValueSpecRemovesItFromDisk(t *testing.T) {
	existing := []byte(`package p

var (
	agentOnlyFlags, serverOnlyFlags []string
)

func reloadConfig() {}
`)
	// Deleted def is no longer in defs; caller declares it via allowedRemovals.
	defs := []store.Definition{
		{Name: "reloadConfig", Kind: "function", Body: "func reloadConfig() {}"},
	}
	merged, ok, unmatched := mergeDeclsIntoSource(existing, defs, []string{"agentOnlyFlags"}, nil)
	if !ok {
		t.Fatalf("mergeDeclsIntoSource returned ok=false")
	}
	if len(unmatched) != 0 {
		t.Fatalf("expected no unmatched defs, got %v", unmatched)
	}
	got := string(merged)
	if strings.Contains(got, "agentOnlyFlags") {
		t.Errorf("multi-name var spec should have been removed from disk, still present:\n%s", got)
	}
}

// TestMergeDeclsIntoSource_MultiNameValueSpecMatchesUnderFirstName is the
// direct unit-level regression for the prometheus-16766 bug: a var/const
// spec declaring more than one name (var a, b []string) used to bail out
// of the match loop unconditionally, leaving its DB def permanently
// unmatched -- which blocked every subsequent edit to any OTHER decl in
// the same file with a false "could not be matched to an on-disk
// declaration" warning, since the def stayed in mergeDeclsIntoSource's
// unmatched set forever regardless of how many times the file was
// re-synced. ingestValueSpec already stores exactly one def for a
// multi-name spec, keyed by the first non-blank name, with Body holding
// the WHOLE spec's rendered text -- this confirms mergeDeclsIntoSource
// now matches and replaces under that same key.
func TestMergeDeclsIntoSource_MultiNameValueSpecMatchesUnderFirstName(t *testing.T) {
	existing := []byte(`package p

var (
	agentOnlyFlags, serverOnlyFlags []string
)

func reloadConfig(start int) {
	_ = start
}
`)
	defs := []store.Definition{
		{Name: "agentOnlyFlags", Kind: "var", Body: "agentOnlyFlags, serverOnlyFlags []string"},
		{Name: "reloadConfig", Kind: "function", Body: "func reloadConfig(start int) {\n\t_ = start\n\t_ = 1\n}"},
	}
	merged, ok, unmatched := mergeDeclsIntoSource(existing, defs, nil, nil)
	if !ok {
		t.Fatalf("mergeDeclsIntoSource returned ok=false")
	}
	if len(unmatched) != 0 {
		t.Fatalf("expected no unmatched defs, got %v -- the multi-name var spec should match under its first name", unmatched)
	}
	got := string(merged)
	if !strings.Contains(got, "agentOnlyFlags, serverOnlyFlags []string") {
		t.Errorf("multi-name var spec text lost from merged output:\n%s", got)
	}
	if !strings.Contains(got, "_ = 1") {
		t.Errorf("unrelated func edit didn't land:\n%s", got)
	}
}

func TestEmitFullSweepSkipsGeneratedFilesButStillCleansOthers(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test/pkg", "pkg", "")

	fnBody := "func Foo() {}"
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: fnBody, SourceFile: "clean.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Bar", Kind: "function", Exported: true,
		Body: "func Bar() {}", SourceFile: "generated.go",
	})

	outDir := t.TempDir()
	pkgDir := filepath.Join(outDir, "pkg")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Pre-seed both files on disk with an unused "strings" import so the
	// AST-merge path has a valid base to patch into (byte-faithful,
	// leaves the import block untouched) -- only goimports would strip
	// the now-unused import.
	cleanSrc := "package pkg\n\nimport \"strings\"\n\n" + fnBody + "\n"
	genSrc := "// Code generated by test. DO NOT EDIT.\npackage pkg\n\nimport \"strings\"\n\nfunc Bar() {}\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "clean.go"), []byte(cleanSrc), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "generated.go"), []byte(genSrc), 0644); err != nil {
		t.Fatal(err)
	}

	// Full/unscoped emit -- GoimportsFiles empty triggers the whole-tree
	// fallback sweep this test targets.
	if _, _, err := emitWithOpts(db, outDir, Opts{}); err != nil {
		t.Fatalf("emitWithOpts: %v", err)
	}

	genData, err := os.ReadFile(filepath.Join(pkgDir, "generated.go"))
	if err != nil {
		t.Fatalf("generated.go missing: %v", err)
	}
	if !strings.Contains(string(genData), `"strings"`) {
		t.Fatalf("generated.go's unused import was stripped -- the unscoped goimports sweep touched a generated file it shouldn't have:\n%s", genData)
	}

	cleanData, err := os.ReadFile(filepath.Join(pkgDir, "clean.go"))
	if err != nil {
		t.Fatalf("clean.go missing: %v", err)
	}
	if strings.Contains(string(cleanData), `"strings"`) {
		t.Fatalf("clean.go's unused import was NOT stripped -- the unscoped sweep should still clean non-generated files:\n%s", cleanData)
	}
}

func TestMergeDeclsIntoSource_UngroupedThreeNameConstDoesNotBlockUnrelatedEdit(t *testing.T) {
	existing := []byte(`package caddy

const phOpen, phClose, phEscape = '{', '}', '\\'

func readFileIntoBuffer(path string) (string, error) {
	return path, nil
}
`)
	// ingestValueSpec's own convention for an ungrouped (standalone) spec:
	// Body renders the WHOLE GenDecl, keyword included -- unlike a
	// grouped spec's Body, which is just the spec's own text.
	constBody := `const phOpen, phClose, phEscape = '{', '}', '\\'`
	defs := []store.Definition{
		{Name: "phOpen", Kind: "const", Body: constBody},
		{Name: "readFileIntoBuffer", Kind: "function", Body: "func readFileIntoBuffer(path string) (string, error) {\n\treturn path, io.EOF\n}"},
	}
	merged, ok, unmatched := mergeDeclsIntoSource(existing, defs, nil, nil)
	if !ok {
		t.Fatalf("mergeDeclsIntoSource returned ok=false")
	}
	if len(unmatched) != 0 {
		t.Fatalf("expected no unmatched defs, got %v -- the ungrouped 3-name const spec should match under its first name (phOpen)", unmatched)
	}
	got := string(merged)
	if !strings.Contains(got, "phOpen, phClose, phEscape") {
		t.Errorf("ungrouped multi-name const text lost from merged output:\n%s", got)
	}
	if !strings.Contains(got, "io.EOF") {
		t.Errorf("unrelated func edit didn't land:\n%s", got)
	}
}

func TestEmitScopedGoimportsSkipsGeneratedFileNotInTouchedSet(t *testing.T) {
	db := testDB(t)
	mod, _ := db.EnsureModule("example.com/test/pkg", "pkg", "")

	fnBody := "func Foo() {}"
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Foo", Kind: "function", Exported: true,
		Body: fnBody, SourceFile: "clean.go",
	})
	db.UpsertDefinition(&store.Definition{
		ModuleID: mod.ID, Name: "Bar", Kind: "function", Exported: true,
		Body: "func Bar() {}", SourceFile: "generated.go",
	})

	outDir := t.TempDir()
	pkgDir := filepath.Join(outDir, "pkg")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	cleanSrc := "package pkg\n\nimport \"strings\"\n\n" + fnBody + "\n"
	genSrc := "// Code generated by test. DO NOT EDIT.\npackage pkg\n\nimport \"strings\"\n\nfunc Bar() {}\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "clean.go"), []byte(cleanSrc), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "generated.go"), []byte(genSrc), 0644); err != nil {
		t.Fatal(err)
	}

	// Scoped emit (GoimportsFiles non-empty) with the generated file
	// named in the list, as handleTestByName's directory-prefix scoping
	// does when a sibling package sits in a subdirectory (#304 followup:
	// promql test scoping to "promql" also swept "promql/parser"'s
	// generated_parser.y.go). generated.go is NOT in TouchedFiles --
	// nothing this pass actually edited it.
	if _, _, err := emitWithOpts(db, outDir, Opts{
		TouchedFiles:   []string{"pkg/clean.go"},
		GoimportsFiles: []string{"pkg/clean.go", "pkg/generated.go"},
	}); err != nil {
		t.Fatalf("emitWithOpts: %v", err)
	}

	genData, err := os.ReadFile(filepath.Join(pkgDir, "generated.go"))
	if err != nil {
		t.Fatalf("generated.go missing: %v", err)
	}
	if !strings.Contains(string(genData), `"strings"`) {
		t.Fatalf("generated.go's unused import was stripped -- the scoped goimports pass touched a generated file it shouldn't have:\n%s", genData)
	}
}

// TestSafeWriteGoFile_ConcurrentWritesToSameFileNeverProduceUnparseableContent
// is a direct repro for a real prometheus-19017 bench trajectory: the
// agent issued two parallel MCP tool calls (op:"test", test:"...") and
// (op:"test", name:"getMMappedFile") in the same turn, both scoped to
// the same package -- handleTestByName/handleTest unconditionally
// re-emit their scope's files before running `go test`, so both calls
// raced to write the same on-disk file via safeWriteGoFile's plain
// os.WriteFile, which is NOT atomic (open+O_TRUNC+write+close). A
// follow-up op:"read" showed the edited function's stored body missing
// its trailing closing brace, and the next emit's safety check failed
// with a parse error blaming an unrelated, adjacent function -- exactly
// the symptom of one writer's os.WriteFile getting truncated mid-flight
// by another concurrent writer's O_TRUNC open on the same path.
func TestSafeWriteGoFile_ConcurrentWritesToSameFileNeverProduceUnparseableContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "race.go")

	if err := os.WriteFile(path, []byte("package race\n\nfunc F() int { return 0 }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const n = 24
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var b strings.Builder
			b.WriteString("package race\n\nfunc F() int {\n")
			for j := 0; j < 300+i*11; j++ {
				fmt.Fprintf(&b, "\t_ = %d\n", j)
			}
			b.WriteString("\treturn 0\n}\n")
			_, _, err := safeWriteGoFile(path, []byte(b.String()), nil)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: safeWriteGoFile error: %v", i, err)
		}
	}

	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := parser.ParseFile(token.NewFileSet(), "", final, 0); perr != nil {
		t.Fatalf("concurrent writes to the same file produced unparseable content (non-atomic write race in safeWriteGoFile): %v\n\n--- final content (%d bytes) ---\n%s", perr, len(final), final)
	}
}
