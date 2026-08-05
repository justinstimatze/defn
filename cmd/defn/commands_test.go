package main

import (
	"os"
	"path/filepath"
	"testing"

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
