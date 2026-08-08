package store

import (
	"context"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
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
	tx, commit, rollback, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	_ = tx
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
		{"handleEdit", "handleEdit"},         // full identifier
		{"handle", "handleEdit"},             // camelCase prefix (unicode61 misses this)
		{"Edit", "handleEdit"},               // camelCase suffix
		{"handle_snake", "handle_snake"},     // underscore literal (Chunk C bug)
		{"snake", "handle_snake"},            // substring across snake_case
		{"pkg.Method", "PkgMethod"},          // dotted path in doc comment
		{"authentication", "Authenticate"},   // doc comment substring
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
	if _, err := dbRaw.db.ExecContext(context.Background(), "DELETE FROM bodies_fts"); err != nil {
		t.Fatalf("wipe bodies_fts: %v", err)
	}
	if _, err := dbRaw.db.ExecContext(context.Background(), "DELETE FROM definitions_fts"); err != nil {
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

// TestUpsertDefinitionsBulk_WithinBatchDuplicate exercises the guard
// added in this change: when the caller passes two Definitions with
// the same natural key (module_id, name, kind, receiver, test) in one
// batch, the bulk INSERT must NOT hit the unique constraint. The
// caller-visible contract is last-write-wins, and both input positions
// receive the same row ID.
//
// The pre-fix failure was "duplicate unique key given: [modID,Name,
// kind,,0]" from the SQLite driver — surfaced whenever the ingest
// layer's package variants overlapped and enqueued shared files' defs
// twice within one flushDefs call.
func TestUpsertDefinitionsBulk_WithinBatchDuplicate(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenSQLite(filepath.Join(dir, "defn.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mod, err := db.EnsureModule("example.com/dup", "dup", "")
	if err != nil {
		t.Fatalf("EnsureModule: %v", err)
	}

	// Two Definitions sharing the natural key (mod.ID, "Target", "type",
	// "", false). Different bodies so we can verify last-write-wins.
	first := &Definition{
		ModuleID: mod.ID, Name: "Target", Kind: "type",
		Body: "type Target struct{ A int }",
	}
	second := &Definition{
		ModuleID: mod.ID, Name: "Target", Kind: "type",
		Body: "type Target struct{ B int }", // last-write-wins expects this
	}
	ids, err := db.UpsertDefinitionsBulk([]*Definition{first, second})
	if err != nil {
		t.Fatalf("UpsertDefinitionsBulk with within-batch duplicate: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids len: got %d, want 2", len(ids))
	}
	if ids[0] != ids[1] {
		t.Errorf("duplicate natural key must yield same row id: got ids[0]=%d ids[1]=%d",
			ids[0], ids[1])
	}
	if ids[0] == 0 {
		t.Errorf("row id must be assigned, got 0")
	}
	got, err := db.GetDefinition(ids[0])
	if err != nil || got == nil {
		t.Fatalf("GetDefinition(%d): %v (nil=%v)", ids[0], err, got == nil)
	}
	if got.Body != second.Body {
		t.Errorf("last-write-wins violated: body=%q, want %q", got.Body, second.Body)
	}
}

// TestListDefsMissingSummary covers the three shapes handleResummarize
// needs to distinguish: (1) no def_summaries row at all, (2) row exists
// but one_line is NULL (from #151 minhash-only backfill), (3) row
// exists with an empty one_line string. All three count as "missing"
// — otherwise handleResummarize would skip defs the read path treats
// as having no summary. Sorted-ascending contract lets callers page.
func TestListDefsMissingSummary(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenSQLite(filepath.Join(dir, "defn.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mod, err := db.EnsureModule("example.com/miss", "miss", "")
	if err != nil {
		t.Fatalf("EnsureModule: %v", err)
	}

	// Three defs, all missing summaries at start.
	var ids [3]int64
	for i, name := range []string{"A", "B", "C"} {
		d := &Definition{
			ModuleID: mod.ID, Name: name, Kind: "function",
			Body: "func " + name + "() {}",
		}
		id, err := db.UpsertDefinition(d)
		if err != nil {
			t.Fatalf("UpsertDefinition %s: %v", name, err)
		}
		ids[i] = id
	}

	got, err := db.ListDefsMissingSummary()
	if err != nil {
		t.Fatalf("ListDefsMissingSummary (initial): %v", err)
	}
	if len(got) != 3 {
		t.Errorf("initial: got %d missing, want 3 (ids=%v)", len(got), got)
	}
	// Sorted-ascending contract.
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("not sorted ascending: %v", got)
		}
	}

	// Fill def A with a real summary → drops from missing.
	if err := db.SetDefSummary(ids[0], &DefSummary{
		OneLine: "returns nothing", BodyHash: "h", Model: "test",
	}); err != nil {
		t.Fatalf("SetDefSummary A: %v", err)
	}
	got, _ = db.ListDefsMissingSummary()
	if len(got) != 2 || got[0] == ids[0] {
		t.Errorf("after filling A: got %v, want 2 ids excluding %d", got, ids[0])
	}

	// Empty one_line still counts as missing (edge case: a failed
	// generation that persisted an empty result would leave defs
	// invisible to future backfill without this guard).
	if err := db.SetDefSummary(ids[1], &DefSummary{
		OneLine: "", BodyHash: "h", Model: "test",
	}); err != nil {
		t.Fatalf("SetDefSummary B (empty): %v", err)
	}
	got, _ = db.ListDefsMissingSummary()
	if len(got) != 2 {
		t.Errorf("after empty-B: got %d missing, want 2 (empty one_line must still count as missing): %v", len(got), got)
	}
}

// TestQueryLiteralFields_SkipOrderByAndSkipDefName locks in the
// opt-out contract for the two bulk-query performance flags: zero
// value (false) must preserve the original behavior (DefName joined,
// results ordered by type_name/field_name); true must actually skip
// each one, not just be silently ignored.
func TestQueryLiteralFields_SkipOrderByAndSkipDefName(t *testing.T) {
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
	def := &Definition{ModuleID: mod.ID, Name: "Config", Kind: "var", Exported: true, Body: "var Config = T{}", Hash: HashBody("var Config = T{}")}
	defID, err := db.UpsertDefinition(def)
	if err != nil {
		t.Fatalf("UpsertDefinition: %v", err)
	}

	// Insertion order deliberately violates (type_name, field_name)
	// alphabetical order, so a present-vs-absent ORDER BY is distinguishable.
	fields := []LiteralField{
		{TypeName: "Zeta", FieldName: "B", FieldValue: "z-b", Line: 3},
		{TypeName: "Alpha", FieldName: "A", FieldValue: "a-a", Line: 1},
		{TypeName: "Alpha", FieldName: "Z", FieldValue: "a-z", Line: 2},
	}
	if err := db.SetLiteralFields(defID, fields); err != nil {
		t.Fatalf("SetLiteralFields: %v", err)
	}

	// Default (both false): DefName populated, results sorted by
	// (type_name, field_name) -- Alpha/A, Alpha/Z, Zeta/B.
	got, err := db.QueryLiteralFields("", "", "", nil, nil, 0, false, false)
	if err != nil {
		t.Fatalf("QueryLiteralFields default: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(got))
	}
	for _, f := range got {
		if f.DefName != "Config" {
			t.Errorf("expected DefName %q joined by default, got %q", "Config", f.DefName)
		}
	}
	wantOrder := []string{"a-a", "a-z", "z-b"}
	for i, f := range got {
		if f.FieldValue != wantOrder[i] {
			t.Errorf("default order[%d] = %q, want %q (sorted by type_name,field_name): got order %v", i, f.FieldValue, wantOrder[i], fieldValues(got))
		}
	}

	// skipDefName: DefName must come back empty even though the
	// definition genuinely exists (proves the join was skipped, not
	// coincidentally empty).
	gotNoDefName, err := db.QueryLiteralFields("", "", "", nil, nil, 0, false, true)
	if err != nil {
		t.Fatalf("QueryLiteralFields skipDefName: %v", err)
	}
	for _, f := range gotNoDefName {
		if f.DefName != "" {
			t.Errorf("skipDefName=true: expected DefName empty, got %q", f.DefName)
		}
	}

	// skipOrderBy: first row should match insertion order (Zeta/B
	// first), not the sorted order -- proves ORDER BY was actually
	// omitted rather than silently still applied.
	gotNoOrder, err := db.QueryLiteralFields("", "", "", nil, nil, 0, true, false)
	if err != nil {
		t.Fatalf("QueryLiteralFields skipOrderBy: %v", err)
	}
	if len(gotNoOrder) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(gotNoOrder))
	}
	if gotNoOrder[0].FieldValue != "z-b" {
		t.Errorf("skipOrderBy=true: expected first row in insertion order (z-b), got %v", fieldValues(gotNoOrder))
	}
}

func fieldValues(fs []LiteralField) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.FieldValue
	}
	return out
}

// TestQueryLiteralFields_DefIDsFilter locks in winze's #230 ask: a
// def-set predicate that pushes membership filtering to SQL instead
// of a caller fetching every matching field and filtering by def_id
// in Go afterward.
func TestQueryLiteralFields_DefIDsFilter(t *testing.T) {
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
	defA := &Definition{ModuleID: mod.ID, Name: "ClaimA", Kind: "var", Exported: true, Body: "var ClaimA = T{}", Hash: HashBody("var ClaimA = T{}")}
	idA, err := db.UpsertDefinition(defA)
	if err != nil {
		t.Fatalf("UpsertDefinition A: %v", err)
	}
	defB := &Definition{ModuleID: mod.ID, Name: "ClaimB", Kind: "var", Exported: true, Body: "var ClaimB = T{}", Hash: HashBody("var ClaimB = T{}")}
	idB, err := db.UpsertDefinition(defB)
	if err != nil {
		t.Fatalf("UpsertDefinition B: %v", err)
	}

	if err := db.SetLiteralFields(idA, []LiteralField{{TypeName: "Claim", FieldName: "Subject", FieldValue: "alice", Line: 1}}); err != nil {
		t.Fatalf("SetLiteralFields A: %v", err)
	}
	if err := db.SetLiteralFields(idB, []LiteralField{{TypeName: "Claim", FieldName: "Subject", FieldValue: "bob", Line: 1}}); err != nil {
		t.Fatalf("SetLiteralFields B: %v", err)
	}

	// No DefIDs: both fields come back.
	all, err := db.QueryLiteralFields("", "", "", nil, nil, 0, false, false)
	if err != nil {
		t.Fatalf("QueryLiteralFields (no filter): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 fields with no DefIDs filter, got %d", len(all))
	}

	// DefIDs scoped to A only: only alice's field comes back, pushed to
	// SQL rather than filtered in Go.
	scoped, err := db.QueryLiteralFields("", "", "", nil, []int64{idA}, 0, false, false)
	if err != nil {
		t.Fatalf("QueryLiteralFields (DefIDs=A): %v", err)
	}
	if len(scoped) != 1 {
		t.Fatalf("expected 1 field scoped to def A, got %d", len(scoped))
	}
	if scoped[0].FieldValue != "alice" {
		t.Errorf("expected alice's field, got %q", scoped[0].FieldValue)
	}
	if scoped[0].DefID != idA {
		t.Errorf("expected DefID %d, got %d", idA, scoped[0].DefID)
	}

	// DefIDs scoped to a def with no literal fields: empty result, not
	// an error.
	none, err := db.QueryLiteralFields("", "", "", nil, []int64{99999}, 0, false, false)
	if err != nil {
		t.Fatalf("QueryLiteralFields (DefIDs=nonexistent): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("expected 0 fields for a nonexistent def ID, got %d", len(none))
	}
}

func TestGetDefinitionByNameAndReceiver_ExactModuleMatchOnly(t *testing.T) {
	// Regression test for a real bug found in a head-to-head-go
	// trajectory: creating a package-level var alias in "zrpc" was
	// rejected as "already exists" because a same-named function
	// already existed in the UNRELATED module "zrpc/internal" -- the
	// SQL used `m.path LIKE '%' || modulePath || '%'`, so "zrpc" as a
	// substring of "zrpc/internal" false-collided. Every real caller
	// (handleCreate's existence check, resolveEditTarget, the resolve
	// package's def-ID lookups) passes an already-resolved, exact
	// module path -- none of them want or expect fuzzy substring
	// matching, unlike GetDefinitionByName's modulePath param (which
	// can be raw user-typed shorthand and deliberately falls back to a
	// fuzzy match after trying exact first).
	db, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	inner, err := db.EnsureModule("example.com/pkg/inner", "inner", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpsertDefinition(&Definition{
		ModuleID: inner.ID, Name: "Widget", Kind: "function", Exported: true,
		Body: "func Widget() {}", SourceFile: "inner.go",
	}); err != nil {
		t.Fatal(err)
	}

	outer, err := db.EnsureModule("example.com/pkg", "pkg", "")
	if err != nil {
		t.Fatal(err)
	}

	// "example.com/pkg" is a prefix of "example.com/pkg/inner" -- the
	// bug's exact shape. A lookup scoped to the outer module must NOT
	// see the inner module's Widget.
	if _, err := db.GetDefinitionByNameAndReceiver("Widget", outer.Path, ""); err == nil {
		t.Fatalf("GetDefinitionByNameAndReceiver found %q in module %q via prefix collision with %q -- exact match should have found nothing", "Widget", outer.Path, inner.Path)
	}

	// Sanity: it's still found when scoped to the module it's actually in.
	if d, err := db.GetDefinitionByNameAndReceiver("Widget", inner.Path, ""); err != nil {
		t.Fatalf("expected to find Widget in its real module %q: %v", inner.Path, err)
	} else if d.ModuleID != inner.ID {
		t.Fatalf("found Widget in module_id=%d, want %d", d.ModuleID, inner.ID)
	}
}
