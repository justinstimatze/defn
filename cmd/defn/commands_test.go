package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/justinstimatze/defn/internal/store"
)

// TestIsCorruptDBError_RealSQLiteCorruption exercises isCorruptDBError
// against errors from an actual corrupted SQLite file, not hand-written
// strings -- these patterns were dead for months (Dolt-era text that
// SQLite's driver never produces) until this test's underlying
// investigation (2026-08-05) surfaced modernc.org/sqlite's real error
// text for each corruption shape.
func TestIsCorruptDBError_RealSQLiteCorruption(t *testing.T) {
	scenarios := map[string]func(path string) error{
		"truncate_to_10_bytes": func(path string) error {
			return os.Truncate(path, 10)
		},
		"zero_first_page": func(path string) error {
			f, err := os.OpenFile(path, os.O_WRONLY, 0644)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteAt(make([]byte, 100), 16)
			return err
		},
		"garbage_header": func(path string) error {
			f, err := os.OpenFile(path, os.O_WRONLY, 0644)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteAt([]byte("NOT A SQLITE FILE AT ALL GARBAGE"), 0)
			return err
		},
		"truncate_mid_file": func(path string) error {
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			return os.Truncate(path, info.Size()/2)
		},
	}

	for name, corrupt := range scenarios {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "defn.db")
			db, err := store.OpenSQLite(dbPath)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if _, err := db.EnsureModule("testmod", "testmod", ""); err != nil {
				t.Fatalf("ensure module: %v", err)
			}
			db.Close()

			if err := corrupt(dbPath); err != nil {
				t.Fatalf("corrupt: %v", err)
			}

			_, err = store.OpenSQLite(dbPath)
			if err == nil {
				t.Fatalf("%s: expected an open error against a corrupted file, got none", name)
			}
			if !isCorruptDBError(err) {
				t.Errorf("%s: isCorruptDBError(%v) = false, want true", name, err)
			}
		})
	}
}

// TestResolveIngestDBPath is the #14 regression test: an unset
// DEFN_DB must anchor to the invocation directory (origCwd) when
// modulePath is a scoped subtree, not to modulePath itself -- while
// the common `defn ingest .` case (origCwd == modulePath) and an
// explicit DEFN_DB override are both unaffected.
func TestResolveIngestDBPath(t *testing.T) {
	t.Run("explicit DEFN_DB always wins", func(t *testing.T) {
		t.Setenv("DEFN_DB", "/explicit/path")
		got := resolveIngestDBPath("/repo", "/repo/corpus")
		if got != "/explicit/path" {
			t.Errorf("got %q, want /explicit/path", got)
		}
	})

	t.Run("ingest . (origCwd == modulePath) defaults to relative .defn", func(t *testing.T) {
		t.Setenv("DEFN_DB", "")
		got := resolveIngestDBPath("/repo", "/repo")
		if got != ".defn" {
			t.Errorf("got %q, want .defn", got)
		}
	})

	t.Run("scoped subdirectory anchors the default to the invocation dir", func(t *testing.T) {
		t.Setenv("DEFN_DB", "")
		got := resolveIngestDBPath("/repo", "/repo/corpus")
		want := filepath.Join("/repo", ".defn")
		if got != want {
			t.Errorf("got %q, want %q -- #14: the DB must not relocate into the scoped subtree", got, want)
		}
	})
}

func TestCollectStatus_NoDatabaseDoesNotCreateOne(t *testing.T) {
	dir := t.TempDir()

	r := collectStatus(dir)

	if !r.NoDatabase {
		t.Fatalf("expected NoDatabase=true for a directory with no defn.db, got %+v", r)
	}
	if r.Database != nil {
		t.Fatalf("expected nil Database, got %+v", r.Database)
	}
	if _, err := os.Stat(filepath.Join(dir, "defn.db")); !os.IsNotExist(err) {
		t.Fatalf("collectStatus must not create defn.db as a side effect of a status check; stat err=%v", err)
	}
}

func TestCollectStatus_DetectsOrphanedDoltTree(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "defn", ".dolt", "noms")
	if err := os.MkdirAll(legacy, 0755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("orphaned dolt bytes")
	if err := os.WriteFile(filepath.Join(legacy, "chunk"), payload, 0644); err != nil {
		t.Fatal(err)
	}

	r := collectStatus(dir)

	if r.LegacyDoltBytes != int64(len(payload)) {
		t.Fatalf("expected LegacyDoltBytes=%d, got %d", len(payload), r.LegacyDoltBytes)
	}
	wantPath := filepath.Join(dir, "defn", ".dolt")
	if r.LegacyDoltPath != wantPath {
		t.Fatalf("expected LegacyDoltPath=%q, got %q", wantPath, r.LegacyDoltPath)
	}
}

func TestCollectStatus_NoLegacyDoltTreeStaysZero(t *testing.T) {
	dir := t.TempDir()

	r := collectStatus(dir)

	if r.LegacyDoltBytes != 0 || r.LegacyDoltPath != "" {
		t.Fatalf("expected no legacy dolt fields set, got path=%q bytes=%d", r.LegacyDoltPath, r.LegacyDoltBytes)
	}
}

// TestCountStaleFiles_NotFooledByOldStoredMetaValue is the real-usage
// regression for the same bug via countStaleFiles (backs warnIfStale /
// announceStaleIngest / collectStatus -- i.e. `defn status`, `defn
// ingest`, `defn query` in production). A restored DB whose defn.db
// mtime is newer than both the stored meta value AND the go file's
// mtime must not be reported as stale.
func TestCountStaleFiles_NotFooledByOldStoredMetaValue(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	goFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	oldStored := time.Now().Add(-3 * time.Hour).Unix()
	if err := db.SetMeta("last_ingest", strconv.FormatInt(oldStored, 10)); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if err := os.Chtimes(goFile, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dbPath, "defn.db"), now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	count, _ := countStaleFiles(db, dir, dbPath)
	if count != 0 {
		t.Fatalf("expected 0 stale files when defn.db's own mtime is newer than the go file, got %d", count)
	}
}

// TestCountStaleFiles_StillDetectsGenuinelyStaleFile is the fix's
// boundary check: a go file genuinely touched AFTER defn.db was last
// written must still be reported stale.
func TestCountStaleFiles_StillDetectsGenuinelyStaleFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	goFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := db.SetMeta("last_ingest", strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dbPath, "defn.db"))
	if err != nil {
		t.Fatal(err)
	}
	future := info.ModTime().Add(10 * time.Second)
	if err := os.Chtimes(goFile, future, future); err != nil {
		t.Fatal(err)
	}

	count, sample := countStaleFiles(db, dir, dbPath)
	if count != 1 {
		t.Fatalf("expected 1 stale file, got %d (sample=%q)", count, sample)
	}
}

// TestLastIngestUnix_UsesDBFileMtimeNotStoredMetaValue guards the
// CLI-side sibling of #332: lastIngestUnix used to trust the
// wall-clock NUMBER stored in the last_ingest meta row as the
// freshness threshold. A workflow that copies/restores a .defn/
// directory onto a fresh checkout (bench harness cache restore, or
// any git-clone-then-restore-DB sequence) preserves that old stored
// number while the restored defn.db's own mtime reflects the restore
// time -- comparing fresh checkout mtimes against a stale stored
// number falsely reports every file as modified-since-ingest.
// lastIngestUnix must derive the threshold from defn.db's own
// on-disk mtime, not the value stored inside it.
func TestLastIngestUnix_UsesDBFileMtimeNotStoredMetaValue(t *testing.T) {
	dbPath := t.TempDir()
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	oldStored := time.Now().Add(-3 * time.Hour).Unix()
	if err := db.SetMeta("last_ingest", strconv.FormatInt(oldStored, 10)); err != nil {
		t.Fatal(err)
	}

	recent := time.Now()
	dbFile := filepath.Join(dbPath, "defn.db")
	if err := os.Chtimes(dbFile, recent, recent); err != nil {
		t.Fatal(err)
	}

	got := lastIngestUnix(db, dbPath)
	if got < recent.Add(-2*time.Second).Unix() {
		t.Fatalf("expected lastIngestUnix to reflect defn.db's on-disk mtime (~%d), got %d (stored meta value was %d)", recent.Unix(), got, oldStored)
	}
}
