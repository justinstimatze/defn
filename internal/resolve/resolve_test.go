package resolve

import (
	"go/parser"
	"os"
	"path/filepath"
	"testing"

	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/store"
)

// writeFile rewrites a single file inside an existing module dir.
func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

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

// writeModule materializes a tiny module under t.TempDir and returns its root.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/refsbug\n\ngo 1.22\n"
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestResolvePreservesEmbedAndImplements is the regression for the
// "embed/implements refs vanish" bug. A concrete type that implements
// multiple interfaces AND embeds another type used to lose all of those
// edges because SetReferences was REPLACE-style and called multiple times
// for the same fromID across the second (implements) and third (TypeSpec
// collectRefs) passes. After the fix, refs accumulate and flush once.
func TestResolvePreservesEmbedAndImplements(t *testing.T) {
	src := `package refsbug

type Base struct{ X int }

type Reader interface{ Read() int }
type Writer interface{ Write(int) }

// Concrete embeds *Base and implements both Reader and Writer.
type Both struct{ *Base }

func (b *Both) Read() int   { return b.X }
func (b *Both) Write(v int) { b.X = v }
`
	dir := writeModule(t, map[string]string{"main.go": src})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Embed: Both embeds Base.
	embed, err := db.QueryRefs("Both", "Base", "embed", 0)
	if err != nil {
		t.Fatalf("query embed: %v", err)
	}
	if len(embed) == 0 {
		t.Fatalf("expected Both → Base embed ref, got none")
	}

	// Implements: Both should keep edges to BOTH interfaces, not just one.
	impls, err := db.QueryRefs("Both", "", "implements", 0)
	if err != nil {
		t.Fatalf("query implements: %v", err)
	}
	if len(impls) < 2 {
		t.Fatalf("expected Both to implement Reader AND Writer, got %d edges: %+v", len(impls), impls)
	}
}

// TestResolvePreservesValueSpecBothBranches covers the inner-loop wipe
// `var X SomeType = expr` used to hit: pass over s.Values then s.Type
// each called SetReferences for the same fromID, second wiped first.
func TestResolvePreservesValueSpecBothBranches(t *testing.T) {
	src := `package refsbug

type Cfg struct{ Port int }

func NewCfg() *Cfg { return &Cfg{} }

// Both Cfg (type expression) and NewCfg (value expression) should land
// as refs from the var def.
var Default *Cfg = NewCfg()
`
	dir := writeModule(t, map[string]string{"main.go": src})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	hasRef := func(toName string) bool {
		t.Helper()
		rs, err := db.QueryRefs("Default", toName, "", 0)
		if err != nil {
			t.Fatalf("query refs: %v", err)
		}
		return len(rs) > 0
	}

	if !hasRef("Cfg") {
		t.Errorf("expected Default → Cfg ref from type expression, missing")
	}
	if !hasRef("NewCfg") {
		t.Errorf("expected Default → NewCfg ref from value expression, missing")
	}
}

// TestResolveFileRefreshesEmbedAfterEdit reproduces the winze symptom:
// embed refs vanish over time as files are sync'd. After IngestFile +
// ResolveFile, the new embed should appear and an old removed embed
// should disappear.
func TestResolveFileRefreshesEmbedAfterEdit(t *testing.T) {
	v1 := `package refsbug

type Entity struct{ ID string }
type Other struct{ Name string }

// Person originally embeds Entity.
type Person struct {
	*Entity
	Age int
}
`
	dir := writeModule(t, map[string]string{"main.go": v1})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Sanity: Person → Entity embed exists.
	rs, _ := db.QueryRefs("Person", "Entity", "embed", 0)
	if len(rs) == 0 {
		t.Fatalf("setup: expected initial Person → Entity embed")
	}

	// Edit: Person now embeds Other instead of Entity.
	v2 := `package refsbug

type Entity struct{ ID string }
type Other struct{ Name string }

// Person now embeds Other.
type Person struct {
	*Other
	Age int
}
`
	writeFile(t, dir, "main.go", v2)

	if _, err := ingest.IngestFile(db, dir, filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("ingest file: %v", err)
	}
	if err := ResolveFile(db, dir, filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("resolve file: %v", err)
	}

	// Old embed should be gone.
	rs, _ = db.QueryRefs("Person", "Entity", "embed", 0)
	if len(rs) != 0 {
		t.Errorf("expected stale Person → Entity embed to be removed, got %+v", rs)
	}
	// New embed should be present.
	rs, _ = db.QueryRefs("Person", "Other", "embed", 0)
	if len(rs) == 0 {
		t.Errorf("expected fresh Person → Other embed after ResolveFile, missing")
	}
}

// TestEvalStringLiteral covers the BinaryExpr concat collapse used by
// composite-literal field extraction. Fix for winze msg-34edc119: multi-line
// +-concatenated string literals (e.g. Provenance.Quote) used to be stored
// as Go-source-form (`"first " + "second"`), corrupting display, audit, and
// FTS. Mixed chains with identifiers must still fall through to format.Node.
func TestEvalStringLiteral(t *testing.T) {
	cases := []struct {
		name   string
		expr   string
		want   string
		wantOk bool
	}{
		{"bare literal", `"hello"`, "hello", true},
		{"raw string", "`raw \"world\"`", `raw "world"`, true},
		{"two-part concat", `"first " + "second"`, "first second", true},
		{"three-part multi-line", `"a " +` + "\n\t\t\"b \" +\n\t\t\"c\"", "a b c", true},
		{"paren wrap", `("wrapped")`, "wrapped", true},
		{"mixed with ident", `"prefix " + x`, "", false},
		{"non-add op", `"a" - "b"`, "", false},
		{"int literal", `42`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := parser.ParseExpr(tc.expr)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.expr, err)
			}
			got, ok := evalStringLiteral(expr)
			if ok != tc.wantOk {
				t.Fatalf("evalStringLiteral(%q) ok = %v, want %v", tc.expr, ok, tc.wantOk)
			}
			if ok && got != tc.want {
				t.Errorf("evalStringLiteral(%q) = %q, want %q", tc.expr, got, tc.want)
			}
		})
	}
}

// TestResolveCollapsesBinaryExprLiterals is the end-to-end regression:
// composite literals with +-concatenated string fields must be stored as
// prose in literal_fields, not as Go-source-form.
func TestResolveCollapsesBinaryExprLiterals(t *testing.T) {
	src := `package refsbug

type Provenance struct {
	Quote string
}

var Sample = Provenance{
	Quote: "first line " +
		"second line " +
		"third line",
}
`
	dir := writeModule(t, map[string]string{"main.go": src})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rows, err := db.QueryLiteralFields("%Provenance", "Quote", "", nil, nil, 0, false, false)
	if err != nil {
		t.Fatalf("query literal fields: %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("expected at least one Provenance.Quote literal, got none")
	}
	want := "first line second line third line"
	if rows[0].FieldValue != want {
		t.Errorf("Quote stored as %q; want %q (BinaryExpr collapse regression)", rows[0].FieldValue, want)
	}
}

func TestResolveFileRefreshesCallRefsFromTestFile(t *testing.T) {
	// Regression probe for a friction point found in a real head-to-head-go
	// trajectory: after a projection-op edit (replace-hunk) rewrote a
	// _test.go function's call target, code(op:"delete") on the OLD
	// target still refused with "callers still reference this def" --
	// even after an explicit code(op:"sync", file:<the test file>). Both
	// paths funnel through ResolveFile, which hardcodes Tests: false.
	// This reproduces that in isolation: does ResolveFile actually pick
	// up a call-ref change made inside a _test.go file?
	src := `package refsbug

func A() int { return 1 }
func B() int { return 2 }
`
	testSrc := `package refsbug

func UseA() int { return A() }
`
	dir := writeModule(t, map[string]string{"main.go": src, "main_test.go": testSrc})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rs, _ := db.QueryRefs("UseA", "A", "call", 0)
	if len(rs) == 0 {
		t.Fatalf("setup: expected initial UseA -> A call ref")
	}

	// Edit: UseA now calls B instead of A -- the same shape as the
	// real trajectory's replace-hunk rewriting a test's call target.
	testSrc2 := `package refsbug

func UseA() int { return B() }
`
	writeFile(t, dir, "main_test.go", testSrc2)

	if _, err := ingest.IngestFile(db, dir, filepath.Join(dir, "main_test.go")); err != nil {
		t.Fatalf("ingest file: %v", err)
	}
	if err := ResolveFile(db, dir, filepath.Join(dir, "main_test.go")); err != nil {
		t.Fatalf("resolve file: %v", err)
	}

	rs, _ = db.QueryRefs("UseA", "A", "call", 0)
	if len(rs) != 0 {
		t.Errorf("ResolveFile left a stale UseA -> A call ref after the test file's call target changed: %+v -- this is exactly what makes code(op:\"delete\") on A wrongly refuse, and what an explicit code(op:\"sync\", file:<test file>) fails to fix, since handleSync's single-file path calls this same ResolveFile", rs)
	}
}

// TestResolveCrossPackageInterfaceSatisfaction is the regression for a
// real bench trajectory: an agent's fix to grpc-go's (*lbPicker).Pick
// (package grpclb) deadlocked a real test, but defn's own op:"test"
// said "No tests cover Pick. Nothing to run." Root cause: pass 2 of
// resolve() only paired concrete types and interfaces declared in the
// SAME package's own scope, so it never noticed grpclb.lbPicker
// implements balancer.Picker -- a different package, the completely
// standard Go idiom of declaring an interface where it's consumed and
// implementing it elsewhere. Calls dispatched through the interface got
// zero caller edges. This test mirrors that shape with three packages:
// the interface, the implementer (a different package), and a caller
// that only ever touches the interface type, never the concrete one.
func TestResolveCrossPackageInterfaceSatisfaction(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"iface/iface.go": `package iface

type Picker interface{ Pick() int }
`,
		"impl/impl.go": `package impl

import "example.com/refsbug/iface"

type LBPicker struct{ n int }

func (p *LBPicker) Pick() int { return p.n }

func New() iface.Picker { return &LBPicker{} }
`,
		"main.go": `package refsbug

import (
	"example.com/refsbug/iface"
	"example.com/refsbug/impl"
)

func dispatch(p iface.Picker) int {
	return p.Pick()
}

func run() int {
	var p iface.Picker = &impl.LBPicker{}
	return dispatch(p)
}
`,
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	pick, err := db.GetDefinitionByNameAndReceiver("Pick", "", "*LBPicker")
	if err != nil {
		t.Fatalf("lookup (*LBPicker).Pick: %v", err)
	}

	refs, err := db.QueryRefs("dispatch", "", "interface_dispatch", 0)
	if err != nil {
		t.Fatalf("query interface_dispatch refs: %v", err)
	}
	found := false
	for _, r := range refs {
		if r.ToDef == pick.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected dispatch -> (*LBPicker).Pick interface_dispatch ref (cross-package), got %d refs: %+v", len(refs), refs)
	}
}

// TestResolveCrossPackageInterfaceDispatchCountsAsTestCoverage is the
// end-to-end version of the bug: a test that only ever reaches a
// concrete method through a cross-package interface must show up in
// GetImpact(method).Tests, matching what op:"test" actually consults.
// Before the fix, this method's Tests list was empty -- "no tests
// cover this" -- for a method a real test did exercise.
func TestResolveCrossPackageInterfaceDispatchCountsAsTestCoverage(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"iface/iface.go": `package iface

type Picker interface{ Pick() int }
`,
		"impl/impl.go": `package impl

import "example.com/refsbug/iface"

type LBPicker struct{ n int }

func (p *LBPicker) Pick() int { return p.n }

func New() iface.Picker { return &LBPicker{} }
`,
		// Decoy: an unrelated type with a same-named "Pick" method and
		// several direct callers, so it wins any name-only, module-fuzzy
		// "most references" tiebreak. Without the fix, the interface
		// method's declaration-site object incorrectly binds to whichever
		// same-named def wins that tiebreak -- this decoy makes that
		// failure mode deterministic instead of accidentally passing
		// because the fixture only had one "Pick" to find.
		"decoy/decoy.go": `package decoy

type Widget struct{}

func (w *Widget) Pick() int { return -1 }

func A() int { return (&Widget{}).Pick() }
func B() int { return (&Widget{}).Pick() }
func C() int { return (&Widget{}).Pick() }
`,
		"main.go": `package refsbug

import "example.com/refsbug/iface"

func dispatch(p iface.Picker) int {
	return p.Pick()
}
`,
		"main_test.go": `package refsbug

import (
	"testing"

	"example.com/refsbug/impl"
)

func TestDispatch(t *testing.T) {
	if dispatch(impl.New()) != 0 {
		t.Fail()
	}
}
`,
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	pick, err := db.GetDefinitionByNameAndReceiver("Pick", "", "*LBPicker")
	if err != nil {
		t.Fatalf("lookup (*LBPicker).Pick: %v", err)
	}

	impact, err := db.GetImpact(pick.ID)
	if err != nil {
		t.Fatalf("GetImpact: %v", err)
	}
	if len(impact.Tests) == 0 {
		t.Fatalf("expected TestDispatch to cover (*LBPicker).Pick via interface dispatch, got zero tests in impact: %+v", impact)
	}
}

func TestResolveFileCapturesCrossPackageCallRef(t *testing.T) {
	// Regression for a bug found via real head-to-head-go trajectories:
	// impact(name:"Emit") reported 0 production callers even though
	// cmdEmit and handleTest call emit.Emit directly. Root cause:
	// ResolveFile loads ONLY the touched file's own package (NeedDeps
	// intentionally omitted for speed), so pass1's objToDef only has
	// entries for defs declared in that one package. A call from the
	// touched file to a func in ANY other package always missed
	// objToDef and collectRefs silently dropped the ref -- and since
	// SetManyReferences deletes-then-reinserts the touched def's whole
	// ref set, this didn't just fail to ADD cross-package refs, it
	// ERASED previously-correct ones on every edit through this path
	// (used by code(op:"sync", file:) and after nearly every write op
	// via autoResolveFile). Fixed by collectRefs falling back to a
	// DB-backed lookup (lookupDefID/lookupTypeDefID, same as pass1's
	// "from" side) when an Ident's object isn't in objToDef.
	dir := writeModule(t, map[string]string{
		"sub/sub.go": "package sub\n\nfunc Target() int { return 1 }\nfunc Other() int { return 2 }\n",
		"main.go":    "package refsbug\n\nimport \"example.com/refsbug/sub\"\n\nfunc Caller() int { return sub.Target() }\n",
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	rs, _ := db.QueryRefs("Caller", "Target", "call", 0)
	if len(rs) == 0 {
		t.Fatalf("setup: expected initial Caller -> Target cross-package call ref")
	}

	// Edit main.go (the caller's OWN file) to call a different
	// cross-package function -- same shape as a real code(op:"edit").
	writeFile(t, dir, "main.go", "package refsbug\n\nimport \"example.com/refsbug/sub\"\n\nfunc Caller() int { return sub.Other() }\n")
	if _, err := ingest.IngestFile(db, dir, filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("ingest file: %v", err)
	}
	if err := ResolveFile(db, dir, filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("resolve file: %v", err)
	}

	rs, _ = db.QueryRefs("Caller", "Other", "call", 0)
	if len(rs) == 0 {
		t.Errorf("ResolveFile (the scoped single-package path used after every code(op:\"edit\")) failed to capture the new cross-package Caller -> Other call ref")
	}
	rs, _ = db.QueryRefs("Caller", "Target", "call", 0)
	if len(rs) != 0 {
		t.Errorf("ResolveFile left a stale Caller -> Target ref after the call target changed: %+v", rs)
	}
}

// TestResolveModule_PreservesCrossModuleInterfaceDispatch guards a severe
// bug found digging prometheus-batch trajectories (2026-08-10): ifaceMethodToImpls
// is rebuilt from scratch on every resolve() call, and pass 2's population loop
// (unlike ifacesByPkg's collection loop, which is unconditional) is gated by
// onlyModule -- packages.Load always loads the whole project ("./..."), but
// ResolveModule/ResolveFile's onlyModule filter skips processing any package
// OTHER than the scoped one when building the concrete-type/interface pairing.
// If the interface's implementer lives in a DIFFERENT module than the one
// being partially resolved, ifaceMethodToImpls never gets an entry for it in
// THAT call -- so collectRefs finds nothing for the caller's dispatch call
// site, and SetManyReferences (a full delete+reinsert per fromID) silently
// WIPES a previously-correct interface_dispatch ref a prior full Resolve had
// computed. Live symptom: op:"impact"/op:"traverse" on any store.Backend
// method reported near-zero callers despite dozens of real cross-package call
// sites through the interface, because internal/mcp (the caller module) had
// been through many incremental per-file/per-module resolves since the last
// full one.
func TestResolveModule_PreservesCrossModuleInterfaceDispatch(t *testing.T) {
	// Mirrors the real defn shape: interface (Backend) and its sole
	// implementer (SQLiteDB) declared in the SAME package (store) -- no
	// import needed for pass 2's "own package's interfaces" branch to
	// pair them. The caller (mcp) lives in a DIFFERENT module and only
	// ever touches the interface type.
	// dispatch also calls a sibling function (helper) so it has at least
	// one OTHER ref -- a real-world function calling s.backend.X() always
	// has other refs too (formatting, sibling calls). That matters here:
	// collectRefs only appends to defRefs[fromID] when len(refs) > 0, so a
	// function whose ONLY possible ref is the (in-this-pass-unresolvable)
	// interface dispatch call never becomes a key in defRefs at all, and
	// SetManyReferences leaves untouched IDs alone -- accidentally
	// sidestepping the bug. helper() ensures dispatch is a real entry
	// that DOES get its ref set replaced by this scoped resolve.
	dir := writeModule(t, map[string]string{
		"store/store.go": `package store

type Backend interface{ Pick() int }

type SQLiteDB struct{ n int }

func (p *SQLiteDB) Pick() int { return p.n }
`,
		"mcp/mcp.go": `package mcp

import "example.com/refsbug/store"

type Server struct{ backend store.Backend }

func helper() int { return 1 }

func (s *Server) dispatch() int {
	return s.backend.Pick() + helper()
}
`,
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("initial full Resolve: %v", err)
	}

	pick, err := db.GetDefinitionByNameAndReceiver("Pick", "", "*SQLiteDB")
	if err != nil {
		t.Fatalf("lookup (*SQLiteDB).Pick: %v", err)
	}

	hasDispatchRef := func() bool {
		refs, err := db.QueryRefs("dispatch", "", "interface_dispatch", 0)
		if err != nil {
			t.Fatalf("query interface_dispatch refs: %v", err)
		}
		for _, r := range refs {
			if r.ToDef == pick.ID {
				return true
			}
		}
		return false
	}

	if !hasDispatchRef() {
		t.Fatalf("expected dispatch -> (*SQLiteDB).Pick interface_dispatch ref after the initial full Resolve")
	}

	// Simulate the real-world pattern: an edit to the CALLER's own module
	// (mcp, containing dispatch) triggers a scoped ResolveModule for just
	// that module -- the implementer (store.SQLiteDB) lives in a DIFFERENT
	// module and is never re-processed by pass 2 in this call.
	if err := ResolveModule(db, dir, "example.com/refsbug/mcp"); err != nil {
		t.Fatalf("ResolveModule: %v", err)
	}

	if !hasDispatchRef() {
		t.Fatalf("ResolveModule scoped to the CALLER's own module silently wiped the dispatch -> (*SQLiteDB).Pick interface_dispatch ref computed by the prior full Resolve")
	}
}

// TestResolveFile_PreservesCrossModuleInterfaceDispatch is the real-world
// counterpart to TestResolveModule_PreservesCrossModuleInterfaceDispatch:
// autoResolveFile (called after nearly every code(op:"edit")/op:"create") uses
// ResolveFile, not ResolveModule. ResolveFile loads only the touched file's
// own package (NeedDeps intentionally omitted for speed -- see its doc
// comment), so it structurally cannot see an interface's implementer at all
// when that implementer lives in a different package. Even after fixing
// resolve()'s onlyModule-gated pass 2, a ResolveFile call has no data to
// rebuild the dispatch edge with -- so it must PRESERVE the existing
// interface_dispatch ref a prior full Resolve established, the same way its
// own doc comment already accepts for incoming cross-package refs.
func TestResolveFile_PreservesCrossModuleInterfaceDispatch(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"store/store.go": `package store

type Backend interface{ Pick() int }

type SQLiteDB struct{ n int }

func (p *SQLiteDB) Pick() int { return p.n }
`,
		"mcp/mcp.go": `package mcp

import "example.com/refsbug/store"

type Server struct{ backend store.Backend }

func helper() int { return 1 }

func (s *Server) dispatch() int {
	return s.backend.Pick() + helper()
}
`,
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("initial full Resolve: %v", err)
	}

	pick, err := db.GetDefinitionByNameAndReceiver("Pick", "", "*SQLiteDB")
	if err != nil {
		t.Fatalf("lookup (*SQLiteDB).Pick: %v", err)
	}

	hasDispatchRef := func() bool {
		refs, err := db.QueryRefs("dispatch", "", "interface_dispatch", 0)
		if err != nil {
			t.Fatalf("query interface_dispatch refs: %v", err)
		}
		for _, r := range refs {
			if r.ToDef == pick.ID {
				return true
			}
		}
		return false
	}

	if !hasDispatchRef() {
		t.Fatalf("expected dispatch -> (*SQLiteDB).Pick interface_dispatch ref after the initial full Resolve")
	}

	// Simulate the real edit path: touch the caller's OWN file (an
	// unrelated body change, same shape as any code(op:"edit")) and
	// re-resolve via ResolveFile -- what autoResolveFile actually calls.
	writeFile(t, dir, "mcp/mcp.go", `package mcp

import "example.com/refsbug/store"

type Server struct{ backend store.Backend }

func helper() int { return 2 }

func (s *Server) dispatch() int {
	return s.backend.Pick() + helper()
}
`)
	if _, err := ingest.IngestFile(db, dir, filepath.Join(dir, "mcp", "mcp.go")); err != nil {
		t.Fatalf("ingest file: %v", err)
	}
	if err := ResolveFile(db, dir, filepath.Join(dir, "mcp", "mcp.go")); err != nil {
		t.Fatalf("ResolveFile: %v", err)
	}

	if !hasDispatchRef() {
		t.Fatalf("ResolveFile (the path used after every real code(op:\"edit\")) silently wiped the dispatch -> (*SQLiteDB).Pick interface_dispatch ref computed by the prior full Resolve")
	}
}

// TestResolvePackages_LoadAllInterfaceDispatch checks whether the exact
// loading path the live server uses (ingestAndResolve -> goload.LoadAll ->
// resolve.ResolvePackages) computes cross-package interface_dispatch refs
// correctly -- goload.LoadAll deliberately omits packages.NeedDeps (unlike
// resolve()'s own internal fallback loader used by plain Resolve/
// ResolveModule), and that difference is unverified against pass 2's
// interface-satisfaction check.
func TestResolvePackages_LoadAllInterfaceDispatch(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"store/store.go": `package store

type Backend interface{ Pick() int }

type SQLiteDB struct{ n int }

func (p *SQLiteDB) Pick() int { return p.n }
`,
		"mcp/mcp.go": `package mcp

import "example.com/refsbug/store"

type Server struct{ backend store.Backend }

func helper() int { return 1 }

func (s *Server) dispatch() int {
	return s.backend.Pick() + helper()
}
`,
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	pkgs, err := goload.LoadAll(dir)
	if err != nil {
		t.Fatalf("goload.LoadAll: %v", err)
	}
	if err := ResolvePackages(db, pkgs, dir); err != nil {
		t.Fatalf("ResolvePackages: %v", err)
	}

	pick, err := db.GetDefinitionByNameAndReceiver("Pick", "", "*SQLiteDB")
	if err != nil {
		t.Fatalf("lookup (*SQLiteDB).Pick: %v", err)
	}
	refs, err := db.QueryRefs("dispatch", "", "interface_dispatch", 0)
	if err != nil {
		t.Fatalf("query interface_dispatch refs: %v", err)
	}
	found := false
	for _, r := range refs {
		if r.ToDef == pick.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ResolvePackages via goload.LoadAll (the live server's actual full-resolve path) failed to compute dispatch -> (*SQLiteDB).Pick, got %d interface_dispatch refs: %+v", len(refs), refs)
	}
}

// TestResolve_InterfaceDispatchSurvivesTestVariantPreference is the real
// root cause behind the two ResolveModule/ResolveFile bugs fixed above --
// found by diagnosing why defn's own live dogfooding session still showed
// zero interface-dispatch callers for store.Backend methods even after
// those fixes and a full re-sync. Direct diagnostic against defn's own repo
// (a standalone go/packages program) proved: internal/store.Backend.GetImpact's
// method Object as seen from internal/store's OWN type-checking session has a
// DIFFERENT pointer than the Object internal/mcp's call site resolves via
// info.Uses -- identical String() representation, different addresses.
//
// Root cause: packages.Load(Tests:true) produces a separate "test variant"
// *packages.Package for any package with its own _test.go files (bundling
// test + non-test files into one type-checking session, distinct from the
// plain variant). goload.FilterPackages deliberately PREFERS the test
// variant when iterating a package directly ("superset of files") -- but a
// package that IMPORTS it normally (never a test variant, per Go's own
// import rules) gets Objects from the PLAIN variant's session. Two
// structurally-identical *types.Func for "the same" method then have
// different pointers. ifaceMethodToImpls, keyed by types.Object, silently
// never matches across this boundary -- breaking cross-package interface
// dispatch tracking for EVERY package with its own tests, i.e. nearly every
// real package. This fixture reproduces it precisely: store/ has a _test.go
// file (unlike the fixtures above, which never triggered FilterPackages'
// preference at all and so never exercised this specific mechanism).
func TestResolve_InterfaceDispatchSurvivesTestVariantPreference(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"store/store.go": `package store

type Backend interface{ Pick() int }

type SQLiteDB struct{ n int }

func (p *SQLiteDB) Pick() int { return p.n }
`,
		"store/store_test.go": `package store

import "testing"

func TestSomething(t *testing.T) {
	var b Backend = &SQLiteDB{}
	_ = b.Pick()
}
`,
		"mcp/mcp.go": `package mcp

import "example.com/refsbug/store"

type Server struct{ backend store.Backend }

func helper() int { return 1 }

func (s *Server) dispatch() int {
	return s.backend.Pick() + helper()
}
`,
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// The exact real-world path: goload.LoadAll (Tests:true, triggers
	// FilterPackages' test-variant preference for store/) + ResolvePackages.
	pkgs, err := goload.LoadAll(dir)
	if err != nil {
		t.Fatalf("goload.LoadAll: %v", err)
	}
	if err := ResolvePackages(db, pkgs, dir); err != nil {
		t.Fatalf("ResolvePackages: %v", err)
	}

	pick, err := db.GetDefinitionByNameAndReceiver("Pick", "", "*SQLiteDB")
	if err != nil {
		t.Fatalf("lookup (*SQLiteDB).Pick: %v", err)
	}
	refs, err := db.QueryRefs("dispatch", "", "interface_dispatch", 0)
	if err != nil {
		t.Fatalf("query interface_dispatch refs: %v", err)
	}
	found := false
	for _, r := range refs {
		if r.ToDef == pick.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("mcp.dispatch -> (*SQLiteDB).Pick interface_dispatch ref missing -- the caller package (mcp) imports store's PLAIN variant while pass 2 iterated store's TEST variant (preferred by FilterPackages since store/ has its own _test.go file), and a types.Object-keyed ifaceMethodToImpls map can never bridge the two. Got %d interface_dispatch refs: %+v", len(refs), refs)
	}
}

// TestResolveTracksStructFieldReferences is the regression for the
// "rename struct field updates 0 callers" bug: struct field objects
// never entered objToDef (obj.Parent() is nil for a field, same as a
// param/local var, so isPackageLevelOrMethod filtered them out), so
// collectRefs could never resolve a selector expression (ro.Count) or
// keyed composite literal (T{Count: ...}) back to the field's def ID.
// GetCallers on the field then always reported zero, so code(op:"rename")
// on a struct field silently failed to propagate to any call site --
// confirmed via a real bench trajectory (etcd RangeOptions.Count ->
// CountOnly) where this forced ~15 extra manual search/read/edit calls
// to hand-propagate a rename the tool was supposed to do atomically.
func TestResolveTracksStructFieldReferences(t *testing.T) {
	src := `package fieldrefs

type Opts struct {
	Count bool
}

func readSelector(o Opts) bool {
	return o.Count
}

func buildLiteral() Opts {
	return Opts{Count: true}
}
`
	dir := writeModule(t, map[string]string{"main.go": src})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	field, err := db.GetDefinitionByNameAndReceiver("Count", "example.com/refsbug", "Opts")
	if err != nil {
		t.Fatalf("get field def: %v", err)
	}

	callers, err := db.GetCallers(field.ID)
	if err != nil {
		t.Fatalf("get callers: %v", err)
	}
	names := map[string]bool{}
	for _, c := range callers {
		names[c.Name] = true
	}
	if !names["readSelector"] {
		t.Errorf("expected readSelector (o.Count selector) to be a caller of Opts.Count, got: %+v", names)
	}
	if !names["buildLiteral"] {
		t.Errorf("expected buildLiteral (Opts{Count: ...} keyed literal) to be a caller of Opts.Count, got: %+v", names)
	}
}

// TestResolveDisambiguatesSameNamedFuncAcrossFiles is the regression for
// the "file goes missing after init edit" bug (grpc-go dialoptions.go /
// pickfirst.go trajectory). Go allows multiple files in one package to
// each declare their own func init() -- a common, valid pattern. Before
// the fix, lookupFuncDefID resolved callers by bare name only
// (defIndex.byName), so a reference made from inside one file's init()
// could be attributed to the WRONG file's init() definition (whichever
// one happened to win the name collision), corrupting the ref graph for
// an ordinary, valid multi-init() package. The fix adds a
// file-qualified lookup (defIndex.byNameFile) tried first, falling back
// to the ambiguous bare-name lookup only when no file-scoped match
// exists.
func TestResolveDisambiguatesSameNamedFuncAcrossFiles(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"a.go": `package multiinit

var registry func()

func Target() {}

func init() {
	registry = Target
}
`,
		"b.go": `package multiinit

var unrelated bool

func init() {
	unrelated = true
}
`,
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	target, err := db.GetDefinitionByName("Target", "example.com/refsbug")
	if err != nil {
		t.Fatalf("get target def: %v", err)
	}

	callers, err := db.GetCallers(target.ID)
	if err != nil {
		t.Fatalf("get callers: %v", err)
	}

	var initFromA bool
	for _, c := range callers {
		if c.Name == "init" && c.SourceFile == "a.go" {
			initFromA = true
		}
	}
	if !initFromA {
		t.Errorf("expected init() in a.go (which references Target) to be attributed as a caller, got: %+v", callers)
	}
	if len(callers) != 1 {
		t.Errorf("expected exactly 1 caller (a.go's init, not b.go's unrelated init), got %d: %+v", len(callers), callers)
	}
}

// TestResolveTracksCrossPackageStructFieldReferences is the second half of
// the struct-field-ref regression (see
// TestResolveTracksStructFieldReferences for the same-package case).
// Real-world bench trajectory: renaming go.etcd.io/etcd's
// RangeOptions.Count -> CountOnly updated same-package callers fine but
// left a keyed composite literal in a DIFFERENT package
// (mvcc.RangeOptions{Count: ...} inside etcdserver/txn) untouched --
// because mvcc has _test.go files, FilterPackages prefers its test
// variant for objToDef, and a field declared there gets a DIFFERENT
// types.Object identity than what an ordinary (non-test) importer's own
// TypesInfo resolves the same field access to. Object-identity lookups
// alone can never see this; only a DB-backed lookup (lookupFieldDefID)
// can. This fixture mirrors that exact shape: package a has a _test.go
// file (forcing FilterPackages to prefer a's test variant), package b
// has no tests and references a.Opts.Count both via a keyed composite
// literal and a plain selector.
func TestResolveTracksCrossPackageStructFieldReferences(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"a/a.go": `package a

type Opts struct {
	Count bool
}
`,
		"a/a_test.go": `package a

import "testing"

func TestNothing(t *testing.T) {}
`,
		"b/b.go": `package b

import "example.com/refsbug/a"

func buildLiteral() a.Opts {
	return a.Opts{Count: true}
}

func readSelector(o a.Opts) bool {
	return o.Count
}
`,
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	field, err := db.GetDefinitionByNameAndReceiver("Count", "example.com/refsbug/a", "Opts")
	if err != nil {
		t.Fatalf("get field def: %v", err)
	}

	callers, err := db.GetCallers(field.ID)
	if err != nil {
		t.Fatalf("get callers: %v", err)
	}
	names := map[string]bool{}
	for _, c := range callers {
		names[c.Name] = true
	}
	if !names["buildLiteral"] {
		t.Errorf("expected cross-package buildLiteral (a.Opts{Count: ...} keyed literal) to be a caller of a.Opts.Count, got: %+v", names)
	}
	if !names["readSelector"] {
		t.Errorf("expected cross-package readSelector (o.Count selector) to be a caller of a.Opts.Count, got: %+v", names)
	}
}

// TestResolveExternalInterfaceSatisfaction is the resolve-level regression
// for widening ifacesByPkg to external (stdlib/third-party) packages: a
// type satisfying io.ReaderAt with no local interface declared anywhere
// used to be entirely invisible to interface-satisfaction tracking (the
// "implements" ref-graph edge needs a defn ID on both sides, and io.ReaderAt
// has none -- it was never ingested). def_external_interfaces is the
// ID-less sidecar that closes that gap: this checks the concrete method's
// own def row gets "io.ReaderAt" recorded via GetExternalInterfaces.
func TestResolveExternalInterfaceSatisfaction(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"main.go": `package extifacebug

import "io"

type File struct{ n int }

func (f *File) ReadAt(p []byte, off int64) (int, error) { return 0, nil }

func use() io.ReaderAt { return &File{} }
`,
	})

	db := testDB(t)
	if err := ingest.Ingest(db, dir); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := Resolve(db, dir); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	readAt, err := db.GetDefinitionByNameAndReceiver("ReadAt", "", "*File")
	if err != nil {
		t.Fatalf("lookup (*File).ReadAt: %v", err)
	}

	extIfaces, err := db.GetExternalInterfaces(readAt.ID)
	if err != nil {
		t.Fatalf("GetExternalInterfaces: %v", err)
	}
	found := false
	for _, name := range extIfaces {
		if name == "io.ReaderAt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected (*File).ReadAt to be recorded as satisfying io.ReaderAt, got: %v", extIfaces)
	}
}
