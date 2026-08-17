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

// TestHandleRename_TypeWithMethodsSplitAcrossFiles is the exact shape
// the mutation fuzzer found after being widened to include type-kind
// defs (fuzzgen's split_methods hazard, seed 1): a type's methods
// declared in OTHER files never had their receiver clause updated by a
// type rename at all. GetCallers(typeID) never surfaces them -- a
// method's receiver is a free-text field on the method's own
// Definition row, not a refs-graph edge -- so the old fast path
// (#148-class: skip the real build gate when the ref graph proves
// nothing dispatch-relevant changed) silently shipped a package where
// two other files still declared methods on a type name that no longer
// existed.
func TestHandleRename_TypeWithMethodsSplitAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(filepath.Join(projDir, "splitm"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nimport \"proj/splitm\"\n\nfunc main() {\n\tw := splitm.Widget{N: 1}\n\t_ = w.MethodA()\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "splitm", "types.go"), []byte("package splitm\n\ntype Widget struct {\n\tN int\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "splitm", "a.go"), []byte("package splitm\n\nfunc (w *Widget) MethodA() int {\n\treturn w.N\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "splitm", "b.go"), []byte("package splitm\n\nfunc (w *Widget) MethodB() int {\n\treturn w.N * 2\n}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:      "rename",
		OldName: "Widget",
		NewName: "WidgetR0",
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("rename failed: %s", text)
	}
	if !strings.Contains(text, "Updated 2 method receiver") {
		t.Errorf("expected the report to mention updating both methods' receivers, got: %s", text)
	}

	for _, f := range []string{"a.go", "b.go"} {
		src, err := os.ReadFile(filepath.Join(projDir, "splitm", f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "Widget)") && !strings.Contains(string(src), "WidgetR0)") {
			t.Errorf("%s still has a stale receiver clause after rename:\n%s", f, src)
		}
		if !strings.Contains(string(src), "WidgetR0") {
			t.Errorf("%s missing the renamed receiver:\n%s", f, src)
		}
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed after rename:\n%s", out)
	}
}
