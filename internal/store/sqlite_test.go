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

	// ComputeRootHash is a Phase 1 followup stub.
	if _, err := db.ComputeRootHash(); err != ErrNotImplemented {
		t.Errorf("ComputeRootHash: expected ErrNotImplemented, got %v", err)
	}
}
