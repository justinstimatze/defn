package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinstimatze/defn/internal/ingest"
	"github.com/justinstimatze/defn/internal/resolve"
	"github.com/justinstimatze/defn/internal/store"
)

// TestHandleRename_GenericMethodReceiverSurvivesRoundTrip is the
// end-to-end regression for the ingest-level receiver-storage fix: before
// it, resolveWriteTarget's receiver-qualified lookup for "Swap" on
// "*Pair" (a 2-type-param receiver) would find nothing at all -- the
// stored key was the literal string "*<unknown>" -- so this rename would
// have failed outright with a not-found error, never even reaching a
// build check. Covers both a single type-param receiver (Stack[T]) and a
// multi type-param receiver (Pair[K, V]), and asserts the renamed
// declaration keeps its generic receiver syntax intact on disk.
func TestHandleRename_GenericMethodReceiverSurvivesRoundTrip(t *testing.T) {
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
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte(`package main

type Stack[T any] struct {
	items []T
}

func (s *Stack[T]) Push(v T) {
	s.items = append(s.items, v)
}

type Pair[K comparable, V any] struct {
	Key   K
	Value V
}

func (p *Pair[K, V]) Swap() (V, K) {
	return p.Value, p.Key
}

func main() {
	s := &Stack[int]{}
	s.Push(1)
	p := &Pair[string, int]{}
	_, _ = p.Swap()
}
`), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:       "rename",
		OldName:  "Push",
		Receiver: "Stack",
		NewName:  "PushX",
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || !strings.Contains(text, "Updated 1 callers") {
		t.Fatalf("rename of generic method Push did not succeed with 1 caller updated: %s", text)
	}

	result2, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:       "rename",
		OldName:  "Swap",
		Receiver: "Pair",
		NewName:  "SwapX",
	})
	text2 := resultText(t, result2)
	if strings.Contains(text2, "rolled back") || !strings.Contains(text2, "Updated 1 callers") {
		t.Fatalf("rename of multi-type-param generic method Swap did not succeed with 1 caller updated: %s", text2)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed after renaming generic methods:\n%s", out)
	}

	src, err := os.ReadFile(filepath.Join(projDir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "func (s *Stack[T]) PushX(v T)") {
		t.Errorf("expected renamed PushX with generic receiver intact, got:\n%s", src)
	}
	if !strings.Contains(string(src), "func (p *Pair[K, V]) SwapX()") {
		t.Errorf("expected renamed SwapX with generic receiver intact, got:\n%s", src)
	}
	if strings.Contains(string(src), "func (s *Stack[T]) Push(") {
		t.Errorf("old Push decl was not removed:\n%s", src)
	}
}
