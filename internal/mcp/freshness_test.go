package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
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

	if note := s.ensureFresh(nil); note != "" {
		t.Fatalf("first probe (lays down the fingerprint): expected no note, got %q", note)
	}
	if note := s.ensureFresh(nil); note != "" {
		t.Fatalf("second probe over an unchanged tree: expected no note, got %q", note)
	}
}

// TestEnsureFresh_HealInvalidatesBodyServedCacheSoRereadShowsNewContent is
// the regression for a real bug a code review caught: ensureFresh heals the
// DB but, without invalidating respCache, a session that already read the
// pre-heal body via full:true would keep getting served the "already read,
// hasn't changed" stub -- silently hiding the very drift the heal just
// fixed. Sequence: read Greet full:true (marks it served), edit its file on
// disk, read Greet again (bare) in the SAME handleCode call that triggers
// the heal -- must show the NEW body, not the stale-cache stub.
func TestEnsureFresh_HealInvalidatesBodyServedCacheSoRereadShowsNewContent(t *testing.T) {
	db, projDir := setupTestDB(t)
	defer db.Close()
	s := &server{backend: db, projectDir: projDir, respCache: newRespCache()}
	s.ready.Store(true)
	req := &sdkmcp.CallToolRequest{Session: &sdkmcp.ServerSession{}}

	ctx := context.Background()

	first, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "Greet", Full: true})
	if err != nil {
		t.Fatalf("initial full read: %v", err)
	}
	if !strings.Contains(resultText(t, first), "Hello, ") {
		t.Fatalf("expected the original body, got: %s", resultText(t, first))
	}

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

	second, _, err := s.handleCode(ctx, req, codeParam{Op: "read", Name: "Greet"})
	if err != nil {
		t.Fatalf("re-read after heal: %v", err)
	}
	text := resultText(t, second)
	if strings.Contains(text, "already read") || strings.Contains(text, "hasn't changed") {
		t.Fatalf("expected the stale bodyServed stub to be invalidated by the heal, got: %s", text)
	}
	if !strings.Contains(text, "Howdy") {
		t.Errorf("expected the re-read to show the healed body (\"Howdy\"), got: %s", text)
	}
}

// TestEnsureFresh_TransientStatErrorDoesNotPruneRealDefinitions is the
// regression for a real bug a code review caught: ensureFresh treated ANY
// os.Stat error on a known file as "the file was deleted" and permanently
// pruned its definitions -- but a permission error (an AV/backup scan, a
// transient EACCES) means "couldn't check", not "gone". Simulated here by
// stripping the containing directory's execute bit, which makes stat on
// files inside it fail with EACCES while the file itself is untouched.
func TestEnsureFresh_TransientStatErrorDoesNotPruneRealDefinitions(t *testing.T) {
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

	if err := os.Chmod(projDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(projDir, 0o755) })

	note := s.ensureFresh(nil)

	if err := os.Chmod(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(note, "removed") {
		t.Errorf("expected no files reported removed for a transient stat error, got note: %q", note)
	}
	if _, err := db.GetDefinitionByName("Extra", "testproj"); err != nil {
		t.Errorf("expected Extra's definition to survive a transient stat error, but it was pruned: %v", err)
	}
}
