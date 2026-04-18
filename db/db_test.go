package db

import (
	"context"
	"path/filepath"
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
