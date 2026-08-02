package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPingSucceedsOnOpenDB(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if err := d.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on open DB failed: %v", err)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Missing key returns empty, no error.
	got, err := d.GetMeta("winze:missing")
	if err != nil {
		t.Fatalf("GetMeta on missing key: %v", err)
	}
	if got != "" {
		t.Errorf("missing key returned %q, want empty", got)
	}

	// Set then get.
	if err := d.SetMeta("winze:last_cycle", "2026-04-17T12:00:00Z"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	got, err = d.GetMeta("winze:last_cycle")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if got != "2026-04-17T12:00:00Z" {
		t.Errorf("GetMeta returned %q", got)
	}

	// Overwrite.
	if err := d.SetMeta("winze:last_cycle", "2026-04-18T12:00:00Z"); err != nil {
		t.Fatalf("SetMeta overwrite: %v", err)
	}
	got, _ = d.GetMeta("winze:last_cycle")
	if got != "2026-04-18T12:00:00Z" {
		t.Errorf("overwrite produced %q", got)
	}
}

func TestSetMetaRequiresNamespacePrefix(t *testing.T) {
	// Unqualified keys would collide with defn-managed state like
	// last_ingest. SetMeta must refuse them rather than silently
	// clobbering internal metadata.
	dir := t.TempDir()
	d, err := Open(filepath.Join(dir, ".defn"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	err = d.SetMeta("last_ingest", "1234567890")
	if err == nil {
		t.Fatal("SetMeta accepted 'last_ingest' without a namespace prefix")
	}
	if !strings.Contains(err.Error(), "namespace prefix") {
		t.Errorf("error should mention namespace prefix, got: %v", err)
	}

	// Reads of defn-managed keys are unrestricted — external callers
	// can still observe defn's own metadata.
	if _, err := d.GetMeta("last_ingest"); err != nil {
		t.Errorf("GetMeta on defn-managed key failed: %v", err)
	}
}

// TestSyncIngestsProjectDir covers the #winze-migration ask: an embedder
// should be able to build/refresh a .defn database in-process, with no
// defn CLI binary involved. Sync(dir) on a fresh DB should populate
// definitions from the module at dir, queryable immediately after.
func TestSyncIngestsProjectDir(t *testing.T) {
	modDir := t.TempDir()
	src := `package greet

func Hello(name string) string { return "hello " + name }
`
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module example.com/greet\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "greet.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	dbDir := t.TempDir()
	d, err := Open(filepath.Join(dbDir, ".defn"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := d.Sync(modDir); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	defs, err := d.Definitions(DefinitionFilter{Name: "Hello"})
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 def named Hello after Sync, got %d: %+v", len(defs), defs)
	}
	if defs[0].Kind != "function" {
		t.Errorf("Hello kind = %q, want %q", defs[0].Kind, "function")
	}

	// last_ingest meta should be set so a future Sync could offer an
	// incremental path (not yet implemented, but the DB-side signal
	// this relies on must already be correct).
	lastIngest, err := d.GetMeta("last_ingest")
	if err != nil {
		t.Fatalf("GetMeta last_ingest: %v", err)
	}
	if lastIngest == "" {
		t.Error("expected last_ingest meta to be set after Sync")
	}
}
