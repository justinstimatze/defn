package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestSQLiteSmoke exercises the Phase 1 milestone-1 surface end-to-end:
// open -> ping -> begin/commit -> module upsert -> read -> meta -> gc -> close.
// If this passes, the driver + schema + basic wiring is proven.
func TestSQLiteSmoke(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "defn.db")

	db, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if got := db.Path(); got != dbPath {
		t.Errorf("Path: got %q, want %q", got, dbPath)
	}

	if err := db.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// Begin -> commit round-trip.
	commit, rollback, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_ = rollback
	if err := commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Module upsert -> read.
	m, err := db.EnsureModule("github.com/foo/bar", "bar", "package doc")
	if err != nil {
		t.Fatalf("EnsureModule: %v", err)
	}
	if m == nil {
		t.Fatal("EnsureModule returned nil module")
	}
	if m.Path != "github.com/foo/bar" || m.Name != "bar" || m.Doc != "package doc" {
		t.Errorf("Module fields: got %+v", m)
	}
	if m.ID == 0 {
		t.Error("expected non-zero module ID")
	}

	// Upsert conflict path — same path, different doc.
	m2, err := db.EnsureModule("github.com/foo/bar", "bar", "updated doc")
	if err != nil {
		t.Fatalf("EnsureModule (upsert): %v", err)
	}
	if m2.ID != m.ID {
		t.Errorf("upsert should keep same ID: got %d, want %d", m2.ID, m.ID)
	}
	if m2.Doc != "updated doc" {
		t.Errorf("doc not updated: got %q", m2.Doc)
	}

	// GetModuleByPath negative case.
	nope, err := db.GetModuleByPath("does/not/exist")
	if err != nil {
		t.Fatalf("GetModuleByPath (missing): %v", err)
	}
	if nope != nil {
		t.Errorf("expected nil for missing module, got %+v", nope)
	}

	// ListModules.
	mods, err := db.ListModules()
	if err != nil {
		t.Fatalf("ListModules: %v", err)
	}
	if len(mods) != 1 {
		t.Errorf("ListModules: got %d, want 1", len(mods))
	}

	// CountDefinitions on empty DB.
	if n, err := db.CountDefinitions(); err != nil || n != 0 {
		t.Errorf("CountDefinitions (empty): got (%d, %v), want (0, nil)", n, err)
	}

	// Meta set/get.
	if err := db.SetMeta("schema_version", "1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if v, err := db.GetMeta("schema_version"); err != nil || v != "1" {
		t.Errorf("GetMeta: got (%q, %v), want (\"1\", nil)", v, err)
	}
	// Meta missing key returns empty + nil.
	if v, err := db.GetMeta("missing"); err != nil || v != "" {
		t.Errorf("GetMeta (missing): got (%q, %v), want (\"\", nil)", v, err)
	}
	// Meta upsert.
	if err := db.SetMeta("schema_version", "2"); err != nil {
		t.Fatalf("SetMeta (upsert): %v", err)
	}
	if v, _ := db.GetMeta("schema_version"); v != "2" {
		t.Errorf("SetMeta upsert did not overwrite: got %q", v)
	}

	// GC — passive checkpoint should always succeed.
	if err := db.GC(); err != nil {
		t.Errorf("GC: %v", err)
	}

	// ComputeRootHash on empty definitions table = hash of empty stream.
	// Not asserting an exact value; just that it's stable + non-error.
	h1, err := db.ComputeRootHash()
	if err != nil {
		t.Fatalf("ComputeRootHash: %v", err)
	}
	h2, err := db.ComputeRootHash()
	if err != nil {
		t.Fatalf("ComputeRootHash (repeat): %v", err)
	}
	if h1 != h2 {
		t.Errorf("ComputeRootHash not stable: %q vs %q", h1, h2)
	}
	if h1 == "" {
		t.Error("ComputeRootHash returned empty string")
	}

	// Simulate: Phase 1 stub returns ErrNotImplemented.
	if _, err := db.Simulate(nil); err != ErrNotImplemented {
		t.Errorf("Simulate: expected ErrNotImplemented, got %v", err)
	}
}

// TestSearchDefinitions_FTS5Trigram locks in the tokenizer contract for
// task #137: camelCase / snake_case / dotted-path / substring queries all
// match, and a subsequent body edit is reflected via the sync triggers.
// Regression guard for the underscore-guard hack we removed from
// handleSearch — trigram FTS handles `_` as content, not a wildcard.
func TestSearchDefinitions_FTS5Trigram(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenSQLite(filepath.Join(dir, "defn.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mod, err := db.EnsureModule("example.com/pkg", "pkg", "")
	if err != nil {
		t.Fatalf("EnsureModule: %v", err)
	}

	// Seed a small def set that exercises Go's naming idioms.
	seed := []struct {
		name, body string
	}{
		{"handleEdit", "func handleEdit() { doStuff() }"},
		{"handle_snake", "func handle_snake() { snakeStuff() }"},
		{"PkgMethod", "// pkg.Method dispatches\nfunc PkgMethod() error { return nil }"},
		{"Authenticate", "// authentication handler for the API\nfunc Authenticate() {}"},
		{"CamelCaseIdentifier", "func CamelCaseIdentifier() {}"},
	}
	for _, s := range seed {
		d := &Definition{
			ModuleID: mod.ID, Name: s.name, Kind: "function",
			Exported: true, Body: s.body, Hash: HashBody(s.body),
		}
		if _, err := db.UpsertDefinition(d); err != nil {
			t.Fatalf("UpsertDefinition %s: %v", s.name, err)
		}
	}

	cases := []struct {
		query   string
		wantHit string // one name we expect in the result set
	}{
		{"handleEdit", "handleEdit"},        // full identifier
		{"handle", "handleEdit"},            // camelCase prefix (unicode61 misses this)
		{"Edit", "handleEdit"},              // camelCase suffix
		{"handle_snake", "handle_snake"},    // underscore literal (Chunk C bug)
		{"snake", "handle_snake"},           // substring across snake_case
		{"pkg.Method", "PkgMethod"},         // dotted path in doc comment
		{"authentication", "Authenticate"},  // doc comment substring
		{"CamelCase", "CamelCaseIdentifier"}, // camelCase middle
	}
	for _, tc := range cases {
		defs, err := db.SearchDefinitions(tc.query)
		if err != nil {
			t.Errorf("SearchDefinitions(%q): %v", tc.query, err)
			continue
		}
		found := false
		names := make([]string, len(defs))
		for i, d := range defs {
			names[i] = d.Name
			if d.Name == tc.wantHit {
				found = true
			}
		}
		if !found {
			t.Errorf("SearchDefinitions(%q): want hit %q, got %v", tc.query, tc.wantHit, names)
		}
	}

	// Body update propagates through the FTS trigger: an edit that adds
	// a distinctive token should be searchable immediately.
	target, err := db.GetDefinitionByName("handleEdit", "example.com/pkg")
	if err != nil || target == nil {
		t.Fatalf("lookup handleEdit: %v (nil=%v)", err, target == nil)
	}
	target.Body = "func handleEdit() { veryDistinctiveMarker() }"
	target.Hash = HashBody(target.Body)
	if _, err := db.UpsertDefinition(target); err != nil {
		t.Fatalf("update handleEdit body: %v", err)
	}
	defs, err := db.SearchDefinitions("veryDistinctiveMarker")
	if err != nil {
		t.Fatalf("SearchDefinitions veryDistinctiveMarker: %v", err)
	}
	if len(defs) == 0 {
		t.Error("body update did not propagate through FTS trigger (search for new token returned 0)")
	}

	// Sub-trigram query (2 chars) must not error — falls back to LIKE.
	if _, err := db.SearchDefinitions("Ed"); err != nil {
		t.Errorf("short-query LIKE fallback errored: %v", err)
	}
}

// TestSearchDefinitions_FTSBackfill covers the migration path: a DB
// populated BEFORE the FTS triggers existed must have its FTS index
// backfilled on the next OpenSQLite. Guards against silent search-
// misses on upgrade.
func TestSearchDefinitions_FTSBackfill(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "defn.db")

	// First open: create schema, seed defs, close.
	db, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	mod, err := db.EnsureModule("example.com/mig", "mig", "")
	if err != nil {
		t.Fatalf("EnsureModule: %v", err)
	}
	d := &Definition{
		ModuleID: mod.ID, Name: "Preexisting", Kind: "function",
		Exported: true, Body: "func Preexisting() { backfillMarker() }",
	}
	d.Hash = HashBody(d.Body)
	if _, err := db.UpsertDefinition(d); err != nil {
		t.Fatalf("UpsertDefinition: %v", err)
	}

	// Sanity: search works on first open (trigger fired).
	defs, err := db.SearchDefinitions("backfillMarker")
	if err != nil || len(defs) == 0 {
		t.Fatalf("first-open search: err=%v defs=%d", err, len(defs))
	}
	_ = db.Close()

	// Simulate an "old DB" state by wiping the FTS tables directly
	// (bypassing triggers). Re-opening should backfill.
	dbRaw, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	if _, err := dbRaw.db.Exec("DELETE FROM bodies_fts"); err != nil {
		t.Fatalf("wipe bodies_fts: %v", err)
	}
	if _, err := dbRaw.db.Exec("DELETE FROM definitions_fts"); err != nil {
		t.Fatalf("wipe definitions_fts: %v", err)
	}
	_ = dbRaw.Close()

	// Third open: backfill should repopulate bodies_fts from bodies.
	db2, err := OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("third open (after wipe): %v", err)
	}
	defer db2.Close()
	defs, err = db2.SearchDefinitions("backfillMarker")
	if err != nil {
		t.Fatalf("post-backfill search: %v", err)
	}
	if len(defs) == 0 {
		t.Error("backfill did not populate FTS (search for existing body returned 0)")
	}
}
