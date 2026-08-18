package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

func TestHandleCode_ConcurrentWritesAllLandCorrectly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(filepath.Join(projDir, "pkg"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "pkg", "existing.go"), []byte("package pkg\n\nfunc Real() int {\n\treturn 1\n}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	const n = 8
	var wg sync.WaitGroup
	texts := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, _, _ := s.handleCode(context.Background(), nil, codeParam{
				Op:   "create",
				File: fmt.Sprintf("pkg/gen%d.go", i),
				Body: fmt.Sprintf("func Gen%d() int { return %d }", i, i),
			})
			texts[i] = resultText(t, result)
		}(i)
	}
	wg.Wait()

	for i, text := range texts {
		if !strings.Contains(text, "Created") {
			t.Errorf("goroutine %d: expected success, got: %s", i, text)
		}
	}

	for i := 0; i < n; i++ {
		d, err := db.GetDefinitionByName(fmt.Sprintf("Gen%d", i), "")
		if err != nil || d == nil {
			t.Errorf("Gen%d not found in DB: %v", i, err)
			continue
		}
		if !strings.Contains(d.Body, fmt.Sprintf("return %d", i)) {
			t.Errorf("Gen%d has wrong body (cross-goroutine corruption?): %s", i, d.Body)
		}
	}

	real, err := db.GetDefinitionByName("Real", "")
	if err != nil || real == nil {
		t.Fatalf("Real definition should still exist: %v", err)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed after concurrent creates:\n%s", out)
	}
}

// TestHandleCode_ConcurrentConflictingEditsSerializeCleanly is the
// harder adversarial case for #274: N goroutines editing the SAME
// definition simultaneously, not just N goroutines each touching their
// own independent file. Begin()'s txMu mutex should fully serialize
// these at the transaction level -- this checks that holds under real
// goroutine contention (not just reasoning about the code): no
// crash/hang/deadlock, no torn write (the final body must be exactly
// one of the N candidates, never a mix), and the DB stays internally
// consistent (exactly one row for the def, not a natural-key-collision
// duplicate).
func TestHandleCode_ConcurrentConflictingEditsSerializeCleanly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(projDir, 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nfunc F() int {\n\treturn 0\n}\n\nfunc main() {}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	const n = 8
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				s.handleCode(context.Background(), nil, codeParam{
					Op:      "edit",
					Name:    "F",
					NewBody: fmt.Sprintf("func F() int {\n\treturn %d\n}", i),
				})
			}(i)
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent conflicting edits deadlocked or hung")
	}

	// Exactly one row for F -- a natural-key collision would show up
	// as CountDefinitionsByName > 1.
	n2, err := db.CountDefinitionsByName("F")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 1 {
		t.Fatalf("expected exactly 1 definition named F after concurrent edits, got %d (natural-key collision?)", n2)
	}

	f, err := db.GetDefinitionByName("F", "")
	if err != nil {
		t.Fatal(err)
	}
	// The final body must be exactly one of the N candidates -- never a
	// torn mix of two.
	matched := false
	for i := 0; i < n; i++ {
		if strings.Contains(f.Body, fmt.Sprintf("return %d", i)) {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("F's final body doesn't match any single candidate -- possible torn write: %s", f.Body)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed after concurrent conflicting edits:\n%s", out)
	}
}
