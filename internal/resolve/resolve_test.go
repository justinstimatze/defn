package resolve

import (
	"go/parser"
	"os"
	"path/filepath"
	"testing"

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

	rows, err := db.QueryLiteralFields("%Provenance", "Quote", "", nil, 0)
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
