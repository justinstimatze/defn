package db

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestSyncPatternScopesToRootPackage covers winze's #230 ask: an
// embedder whose corpus is a single declarative package shouldn't pay
// full-module type-checking on every Sync. SyncPattern(dir, ".")
// should ingest only the root package, leaving a sibling subpackage
// untouched -- unlike Sync (== SyncPattern(dir, "./...")), which
// ingests everything.
func TestSyncPatternScopesToRootPackage(t *testing.T) {
	modDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module example.com/multi\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "root.go"), []byte(`package multi

func Root() string { return "root" }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(modDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modDir, "sub", "sub.go"), []byte(`package sub

func Leaf() string { return "leaf" }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	dbDir := t.TempDir()
	d, err := Open(filepath.Join(dbDir, ".defn"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := d.SyncPattern(modDir, "."); err != nil {
		t.Fatalf("SyncPattern(\".\"): %v", err)
	}

	root, err := d.Definitions(DefinitionFilter{Name: "Root"})
	if err != nil {
		t.Fatalf("Definitions Root: %v", err)
	}
	if len(root) != 1 {
		t.Fatalf("expected Root to be ingested by SyncPattern(\".\"), got %d matches", len(root))
	}

	leaf, err := d.Definitions(DefinitionFilter{Name: "Leaf"})
	if err != nil {
		t.Fatalf("Definitions Leaf: %v", err)
	}
	if len(leaf) != 0 {
		t.Fatalf("expected Leaf (sub-package) NOT to be ingested by SyncPattern(\".\"), got %d matches", len(leaf))
	}

	// Sync (whole module) picks up the sub-package too.
	if err := d.Sync(modDir); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	leafAfterSync, err := d.Definitions(DefinitionFilter{Name: "Leaf"})
	if err != nil {
		t.Fatalf("Definitions Leaf after Sync: %v", err)
	}
	if len(leafAfterSync) != 1 {
		t.Fatalf("expected Leaf to be ingested after whole-module Sync, got %d matches", len(leafAfterSync))
	}
}

// TestStaleFiles_NotFooledByRestoredDBWithOldMetaMtime guards the
// public-API instance of the #332 bug class: StaleFiles used to
// compare .go file mtimes against the wall-clock NUMBER stored in the
// last_ingest meta row. An external embedder that restores a
// previously-synced .defn/ onto a freshly checked-out tree (the go
// files get the checkout's OWN mtime, unrelated to when Sync last
// ran) would see every file reported as modified-since-ingest even
// though content matches exactly. The threshold must come from
// defn.db's own on-disk mtime instead.
func TestStaleFiles_NotFooledByRestoredDBWithOldMetaMtime(t *testing.T) {
	modDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module example.com/greet\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	goFile := filepath.Join(modDir, "greet.go")
	if err := os.WriteFile(goFile, []byte("package greet\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, ".defn")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := d.Sync(modDir); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	// Simulate a cache-restore ordering: the go file gets a fresh mtime
	// (as a git clone/checkout would stamp it) well AFTER Sync ran, then
	// defn.db is restored in place even later -- the exact sequence #332
	// found in the real bench harness. defn.db's own mtime ends up the
	// most recent thing on disk, so nothing should read as stale.
	now := time.Now()
	if err := os.Chtimes(goFile, now.Add(2*time.Second), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dbPath, "defn.db"), now.Add(5*time.Second), now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}

	stale, err := d.StaleFiles(modDir)
	if err != nil {
		t.Fatalf("StaleFiles: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("expected no stale files (defn.db's own mtime is the most recent thing on disk), got %v", stale)
	}
}

// TestStaleFiles_StillDetectsGenuinelyStaleFile is the fix's boundary
// check: a go file genuinely touched after defn.db was last written
// must still be reported stale.
func TestStaleFiles_StillDetectsGenuinelyStaleFile(t *testing.T) {
	modDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module example.com/greet\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	goFile := filepath.Join(modDir, "greet.go")
	if err := os.WriteFile(goFile, []byte("package greet\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, ".defn")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := d.Sync(modDir); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	info, err := os.Stat(filepath.Join(dbPath, "defn.db"))
	if err != nil {
		t.Fatal(err)
	}
	future := info.ModTime().Add(10 * time.Second)
	if err := os.Chtimes(goFile, future, future); err != nil {
		t.Fatal(err)
	}

	stale, err := d.StaleFiles(modDir)
	if err != nil {
		t.Fatalf("StaleFiles: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("expected 1 genuinely stale file, got %v", stale)
	}
}

// TestStaleFiles_DetectsSameSecondSubSecondModification is the #357
// regression (2026-08-29 winze bug report): StaleFiles truncated both
// the DB's mtime and each .go file's mtime to whole Unix seconds via
// .Unix() before comparing with strict `>`. A file written in the same
// wall-clock second as the last ingest -- entirely plausible for any
// fast automated write-then-verify sequence -- was never reported
// stale, and this isn't a transient race that clears on retry: both
// timestamps are already-fixed past values, so no amount of waiting
// before the next query changes the comparison. Root-caused live via a
// winze_remember-then-winze_recall sequence where defn_meta.last_ingest
// and the just-written file's mtime were numerically identical down to
// the second.
func TestStaleFiles_DetectsSameSecondSubSecondModification(t *testing.T) {
	modDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module example.com/greet\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	goFile := filepath.Join(modDir, "greet.go")
	if err := os.WriteFile(goFile, []byte("package greet\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, ".defn")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	if err := d.Sync(modDir); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	info, err := os.Stat(filepath.Join(dbPath, "defn.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Same whole second as defn.db's mtime, but genuinely later at
	// nanosecond resolution -- exactly the shape StaleFiles's old
	// .Unix()-truncated comparison could never distinguish.
	sameSecondLater := info.ModTime().Add(1 * time.Millisecond)
	if err := os.Chtimes(goFile, sameSecondLater, sameSecondLater); err != nil {
		t.Fatal(err)
	}

	stale, err := d.StaleFiles(modDir)
	if err != nil {
		t.Fatalf("StaleFiles: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("#357: expected the same-second-but-later write to be detected stale, got %v", stale)
	}
}

// TestStaleFiles_NotFooledByReadOnlyReopenBetweenWriteAndCheck guards the
// #362 regression: a genuine edit made after Sync must still be detected
// as stale even if the DB connection gets closed and reopened (read-only,
// no writes) in between -- e.g. a separate "recall" process starting up
// after an earlier "remember" process already wrote and exited. Before
// the #362 fix, including defn.db-wal/defn.db-shm in the freshness
// baseline meant this read-only reopen alone pushed the baseline to
// "now", masking the real edit made just before it.
func TestStaleFiles_NotFooledByReadOnlyReopenBetweenWriteAndCheck(t *testing.T) {
	modDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module example.com/greet\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	goFile := filepath.Join(modDir, "greet.go")
	if err := os.WriteFile(goFile, []byte("package greet\n\nfunc Hello() string { return \"hi\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, ".defn")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := d.Sync(modDir); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A genuine edit after the connection closed -- this is what a
	// subsequent recall should detect as needing re-ingest.
	time.Sleep(2 * time.Millisecond)
	if err := os.WriteFile(goFile, []byte("package greet\n\nfunc Hello() string { return \"hi there\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate a separate read-only process reopening the DB afterward,
	// with no new writes -- this is exactly what advanced defn.db-wal's
	// mtime past the edit under the #362 bug.
	time.Sleep(2 * time.Millisecond)
	d2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d2.Close()
	if _, err := d2.StaleFiles(modDir); err != nil {
		t.Fatalf("warm-up StaleFiles: %v", err)
	}

	stale, err := d2.StaleFiles(modDir)
	if err != nil {
		t.Fatalf("StaleFiles: %v", err)
	}
	if len(stale) != 1 || stale[0] != goFile {
		t.Fatalf("expected [%s] to be reported stale after the read-only reopen, got %v", goFile, stale)
	}
}
