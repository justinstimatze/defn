package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

// TestWriteSafetyConformance runs the full writeSafetyCases table. See
// writeSafetyCase's doc comment for what this suite is and why it
// exists as a deliberate table rather than relying on
// FuzzMutationSequence's randomized search to eventually rediscover
// the same shapes.
func TestWriteSafetyConformance(t *testing.T) {
	for _, tc := range writeSafetyCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			runWriteSafetyCase(t, tc)
		})
	}
}

// runWriteSafetyCase is the shared harness every writeSafetyCase runs
// through: write the fixture, ingest+resolve it, dispatch the op
// through the same handleCode entry point a real agent uses, then
// check both defn's own report AND (unconditionally) an independent
// `go build` -- the invariant that matters more than what defn says
// about itself.
func runWriteSafetyCase(t *testing.T, tc writeSafetyCase) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(tc.fixture), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, tc.op)
	text := resultText(t, result)
	failed := result.IsError || strings.Contains(text, "rolled back") || strings.Contains(text, "does not support")

	raw, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}

	switch {
	case tc.wantSuccess && failed:
		t.Fatalf("expected %s to succeed, got: %s", tc.op.Op, text)
	case !tc.wantSuccess && !failed:
		t.Fatalf("expected %s to be refused/rolled back, but it reported success: %s", tc.op.Op, text)
	case !tc.wantSuccess && string(raw) != tc.fixture:
		t.Errorf("expected main.go to be untouched on a refused/rolled-back op, got:\n%s", string(raw))
	}

	// The invariant every case shares regardless of wantSuccess: whatever
	// state the tree is left in, it must build. A refused op leaves the
	// original (already-known-good) fixture in place; a successful op
	// must leave a tree that still compiles. This is the exact check
	// that would have caught every bug this table's negative cases are
	// named after, before it shipped.
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		t.Fatalf("tree does not build after %s (defn reported: %s):\n%s", tc.op.Op, text, out)
	}
}

// writeSafetyCase is one entry in the write-op safety conformance suite:
// a minimal Go fixture that sets up a specific risky shape (a def kind
// or structural feature a write op's fast path has been found to
// mishandle), the exact code(op:...) call to make against it, and
// whether that call should succeed.
//
// This is the deliberately-enumerated, Go-idiomatic equivalent of a
// compiler conformance suite -- the same pattern Go's own toolchain
// uses in go/parser/testdata and cmd/compile/internal/types2/testdata.
// Unlike FuzzMutationSequence (which discovers unknown shapes via
// randomization, gated behind an opt-in -fuzz= flag that a plain
// `go test ./...` never actually searches -- it only replays its tiny
// seed corpus), this suite runs on every normal test invocation, and
// each entry documents a SPECIFIC, already-understood risk rather than
// hoping to rediscover it by chance. New entries belong here whenever
// a new op or a new def-kind interaction turns out to need one --
// treat gaps in this table as the thing that let a bug through, not
// the test coverage as optional polish after the fact.
//
// Every case, regardless of wantSuccess, is checked against the same
// invariant assertBuildStillPasses (mutation_fuzz_test.go) already
// established: never trust defn's own success message, independently
// re-run `go build`. A wantSuccess=false case additionally requires
// the file be left byte-identical to the fixture (nothing partially
// applied) -- the "honest rollback, not silent corruption" contract
// every fix in this suite was built around.
type writeSafetyCase struct {
	name        string
	fixture     string    // full main.go source (package proj, no func main required)
	op          codeParam // dispatched via handleCode, same entry point a real agent uses
	wantSuccess bool      // true: op must succeed and the tree must still build.
	// false: op must be refused/rolled back, and the file must be
	// byte-identical to fixture.
}

var writeSafetyCases = []writeSafetyCase{
	{
		name: "struct_field_delete_refused",
		fixture: `package proj

type Opts struct {
	Count bool
	Other int
}
`,
		op:          codeParam{Op: "delete", Name: "Count", Receiver: "Opts", Force: true},
		wantSuccess: false,
	},
	{
		name: "struct_field_patch_refused",
		fixture: `package proj

type Opts struct {
	Count bool
}
`,
		op:          codeParam{Op: "patch", Name: "Count", Receiver: "Opts", OldName: "Count", NewName: "CountX"},
		wantSuccess: false,
	},
	{
		name: "struct_field_move_refused",
		fixture: `package proj

type Opts struct {
	Count bool
}
`,
		op:          codeParam{Op: "move", Name: "Count", Receiver: "Opts", Module: "proj"},
		wantSuccess: false,
	},
	{
		name: "method_rename_breaks_interface_rolled_back",
		fixture: `package proj

type Reader interface {
	Bar() int
}

type Foo struct{}

func (f Foo) Bar() int { return 1 }

func use(r Reader) int {
	return r.Bar()
}
`,
		op:          codeParam{Op: "rename", OldName: "Bar", NewName: "Baz", Receiver: "Foo"},
		wantSuccess: false,
	},
	{
		name: "method_rename_breaks_embedded_interface_rolled_back",
		fixture: `package proj

type BaseReader interface {
	Bar() int
}

type Reader interface {
	BaseReader
}

type Foo struct{}

func (f Foo) Bar() int { return 1 }

func use(r Reader) int {
	return r.Bar()
}
`,
		op:          codeParam{Op: "rename", OldName: "Bar", NewName: "Baz", Receiver: "Foo"},
		wantSuccess: false,
	},
	{
		name: "method_rename_breaks_stdlib_interface_rolled_back",
		fixture: `package proj

import "io"

type Foo struct{}

func (f Foo) Read(p []byte) (int, error) { return 0, nil }

func use() io.Reader {
	return Foo{}
}
`,
		op:          codeParam{Op: "rename", OldName: "Read", NewName: "ReadX", Receiver: "Foo"},
		wantSuccess: false,
	},
	{
		name: "replace_hunk_signature_type_change_rolled_back",
		fixture: `package proj

func double(x int) int {
	return x * 2
}

func use() int {
	return double(5)
}
`,
		op:          codeParam{Op: "replace-hunk", Name: "double", Old: "x int) int {\n\treturn x * 2", New: "x string) int {\n\treturn len(x) * 2"},
		wantSuccess: false,
	},
	{
		name: "replace_slice_signature_kind_type_change_rolled_back",
		fixture: `package proj

func double(x int) int {
	return x * 2
}

func use() int {
	return double(5)
}
`,
		op:          codeParam{Op: "replace-slice", Name: "double", Slice: "signature", New: "func double(x string) int"},
		wantSuccess: false,
	},
	{
		name: "edit_interface_method_removal_rolled_back",
		fixture: `package proj

type Reader interface {
	Bar() int
	Qux() string
}

func use(r Reader) string {
	return r.Qux()
}
`,
		op:          codeParam{Op: "edit", Name: "Reader", NewBody: "type Reader interface {\n\tBar() int\n}"},
		wantSuccess: false,
	},
	{
		name: "edit_struct_field_removal_rolled_back",
		fixture: `package proj

type Opts struct {
	Count int
}

func use() Opts {
	return Opts{Count: 5}
}
`,
		op:          codeParam{Op: "edit", Name: "Opts", NewBody: "type Opts struct {\n}"},
		wantSuccess: false,
	},
	{
		name: "replace_hunk_interface_method_removal_rolled_back",
		fixture: `package proj

type Reader interface {
	Bar() int
	Qux() string
}

func use(r Reader) string {
	return r.Qux()
}
`,
		op:          codeParam{Op: "replace-hunk", Name: "Reader", Old: "Bar() int\n\tQux() string", New: "Bar() int"},
		wantSuccess: false,
	},
	{
		name: "plain_method_rename_succeeds",
		fixture: `package proj

type Foo struct{}

func (f Foo) Bar() int { return 1 }

func use() int {
	f := Foo{}
	return f.Bar()
}
`,
		op:          codeParam{Op: "rename", OldName: "Bar", NewName: "Baz", Receiver: "Foo"},
		wantSuccess: true,
	},
	{
		name: "plain_body_only_edit_succeeds",
		fixture: `package proj

func Greet(name string) string {
	return "Hello, " + name
}
`,
		op:          codeParam{Op: "edit", Name: "Greet", NewBody: "func Greet(name string) string {\n\treturn \"Hi, \" + name\n}"},
		wantSuccess: true,
	},
	{
		name: "promoted_method_rename_via_embedding_succeeds",
		fixture: `package proj

type Inner struct{}

func (i Inner) Bar() int { return 1 }

type Outer struct {
	Inner
}

func usePromoted(o Outer) int {
	return o.Bar()
}
`,
		op:          codeParam{Op: "rename", OldName: "Bar", NewName: "Baz", Receiver: "Inner"},
		wantSuccess: true,
	},
	{
		name: "struct_field_rename_succeeds",
		fixture: `package proj

type Opts struct {
	Count bool
}

func readSelector(o Opts) bool {
	return o.Count
}
`,
		op:          codeParam{Op: "rename", OldName: "Count", NewName: "CountOnly", Receiver: "Opts"},
		wantSuccess: true,
	},
	{
		name: "unrelated_method_rename_not_flagged_by_stdlib_allowlist_succeeds",
		fixture: `package proj

type Foo struct{}

func (f Foo) Compute(x int) int { return x + 1 }

func use() int {
	f := Foo{}
	return f.Compute(5)
}
`,
		op:          codeParam{Op: "rename", OldName: "Compute", NewName: "ComputeX", Receiver: "Foo"},
		wantSuccess: true,
	},
	{
		// Regression for a bug the mutation fuzzer found after being
		// widened to include type-kind defs: renaming a TYPE never
		// updated its methods' receiver clauses at all, since a method's
		// receiver is a free-text field on the method's OWN Definition
		// row, not a refs-graph edge GetCallers would surface. defn
		// reported success while leaving "func (w *Widget) MethodA()"
		// on disk pointing at a type name that no longer existed.
		name: "type_rename_updates_method_receiver_succeeds",
		fixture: `package proj

type Widget struct {
	N int
}

func (w *Widget) Double() int {
	return w.N * 2
}

func use() int {
	w := Widget{N: 1}
	return w.Double()
}
`,
		op:          codeParam{Op: "rename", OldName: "Widget", NewName: "WidgetV2"},
		wantSuccess: true,
	},
	{
		// Regression the mutation fuzzer found (corpus entry
		// 04f64e9f0f97f1e0) after the interface_satisfaction hazard was
		// added: resolve()'s FuncDecl reference pass only ever scanned
		// d.Body, never d.Type (the parameter/return type list) -- a
		// type used ONLY in a signature (never instantiated inside the
		// body) was invisible to GetCallers. Renaming Greeter here used
		// to report "Updated 0 callers" and leave UseGreeter's parameter
		// type referencing the now-undefined old name.
		name: "type_rename_updates_signature_only_reference_succeeds",
		fixture: `package proj

type Greeter interface {
	Greet() string
}

func UseGreeter(g Greeter) string {
	return g.Greet()
}
`,
		op:          codeParam{Op: "rename", OldName: "Greeter", NewName: "GreeterV2"},
		wantSuccess: true,
	},
}
