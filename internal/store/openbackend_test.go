package store

import (
	"path/filepath"
	"testing"
)

// TestOpenBackendSQLite exercises the Phase 2 env-flag routing for the
// SQLite backend: DEFN_BACKEND=sqlite → OpenSQLite at <path>/defn.db.
func TestOpenBackendSQLite(t *testing.T) {
	t.Setenv("DEFN_BACKEND", "sqlite")
	dir := t.TempDir()

	b, err := OpenBackend(dir)
	if err != nil {
		t.Fatalf("OpenBackend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	sqliteDB, ok := b.(*SQLiteDB)
	if !ok {
		t.Fatalf("OpenBackend returned %T, want *SQLiteDB", b)
	}
	wantPath := filepath.Join(dir, "defn.db")
	if got := sqliteDB.Path(); got != wantPath {
		t.Errorf("Path: got %q, want %q", got, wantPath)
	}

	// End-to-end smoke: EnsureModule → GetModuleByPath round-trip.
	if _, err := b.EnsureModule("example.com/foo", "foo", ""); err != nil {
		t.Fatalf("EnsureModule: %v", err)
	}
	if m, err := b.GetModuleByPath("example.com/foo"); err != nil || m == nil {
		t.Fatalf("GetModuleByPath: got (%v, %v)", m, err)
	}
}

// TestOpenBackendUnknown rejects an unrecognized backend value with a
// helpful error rather than silently falling back.
func TestOpenBackendUnknown(t *testing.T) {
	t.Setenv("DEFN_BACKEND", "bogus")
	if _, err := OpenBackend(t.TempDir()); err == nil {
		t.Fatal("OpenBackend(bogus): expected error, got nil")
	}
}
