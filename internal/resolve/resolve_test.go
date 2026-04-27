package resolve

import (
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
