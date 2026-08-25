package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureFresh_HealsFileEditedOutsideCodeTool is the core regression for
// the auto-freshness gate: an agent edits a .go file directly on disk
// (Edit/Write's documented escape hatch, or a plain git checkout) and never
// calls op:"sync". Without ensureFresh, every subsequent read/outline
// answers from the stale pre-edit body with no signal anything is wrong.
func TestEnsureFresh_HealsFileEditedOutsideCodeTool(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	mainGo := filepath.Join(projDir, "main.go")
	newSrc := `package main

// Greet returns a greeting.
func Greet(name string) string {
	return "Howdy, " + name
}

// Farewell says goodbye.
func Farewell(name string) string {
	return Greet(name) + " and goodbye"
}

func main() {
	Farewell("world")
}
`
	if err := os.WriteFile(mainGo, []byte(newSrc), 0644); err != nil {
		t.Fatal(err)
	}

	// No op:"sync" call anywhere -- a plain read must still see "Howdy".
	result, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Greet", Full: true})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "Howdy") {
		t.Errorf("expected auto-healed body to contain the new content (\"Howdy\"), got: %s", text)
	}
	if strings.Contains(text, "Hello, ") {
		t.Errorf("expected the stale pre-edit body (\"Hello, \") to be gone, got: %s", text)
	}
}

// TestEnsureFresh_PrunesFileDeletedOutsideCodeTool confirms a file removed
// from disk (not via op:"delete") gets its stale definitions pruned
// automatically instead of lingering forever in the index.
func TestEnsureFresh_PrunesFileDeletedOutsideCodeTool(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	extraGo := filepath.Join(projDir, "extra.go")
	if err := os.WriteFile(extraGo, []byte(`package main

func Extra() string { return "extra" }
`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "sync", File: "extra.go"}); err != nil {
		t.Fatalf("sync extra.go: %v", err)
	}
	if _, err := db.GetDefinitionByName("Extra", "testproj"); err != nil {
		t.Fatalf("expected Extra to be indexed after sync: %v", err)
	}

	if err := os.Remove(extraGo); err != nil {
		t.Fatal(err)
	}

	// An unrelated op should still trigger the probe and prune the now-gone file.
	if _, _, err := s.handleCode(context.Background(), nil, codeParam{Op: "read", Name: "Greet", Full: true}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := db.GetDefinitionByName("Extra", "testproj"); err == nil {
		t.Error("expected Extra's definition to be pruned after extra.go was deleted from disk, but it still exists")
	}
}

// TestEnsureFresh_SecondCallIsNoOpWhenNothingChanged confirms the common
// steady-state path never re-triggers a heal: once a file's been probed and
// confirmed clean, an unrelated second call over the same unchanged tree
// produces no freshness note.
func TestEnsureFresh_SecondCallIsNoOpWhenNothingChanged(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	if note := s.ensureFresh(); note != "" {
		t.Fatalf("first probe (lays down the fingerprint): expected no note, got %q", note)
	}
	if note := s.ensureFresh(); note != "" {
		t.Fatalf("second probe over an unchanged tree: expected no note, got %q", note)
	}
}
