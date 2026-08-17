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

// TestHandleMove_DoesNotCorruptUnrelatedSameNamedDecl guards the
// collision risk in the fix above: AllowedRemovals/AllowedAdds match by
// bare identity name with no file or module qualifier, so without
// scoping the emit to just the two files this move actually touches, an
// unrelated package's own same-named top-level func (a common Go shape
// -- New, String, Close...) would get silently spliced out the instant
// a full unscoped emit visited its file.
func TestHandleMove_DoesNotCorruptUnrelatedSameNamedDecl(t *testing.T) {
	s, projDir, _ := setupMoveTestProject(t)

	// A third package with its own unrelated top-level "Bar" -- same
	// identity string as the def being moved.
	os.MkdirAll(filepath.Join(projDir, "third"), 0755)
	os.WriteFile(filepath.Join(projDir, "third", "third.go"), []byte("package third\n\nfunc Bar() string {\n\treturn \"unrelated\"\n}\n"), 0644)
	if err := ingest.Ingest(s.backend, projDir); err != nil {
		t.Fatal("re-ingest:", err)
	}
	if err := resolve.Resolve(s.backend, projDir); err != nil {
		t.Fatal("re-resolve:", err)
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:     "move",
		Name:   "Bar",
		Module: "other",
		File:   "sub/sub.go", // disambiguate: two "Bar"s now exist project-wide
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("move failed: %s", text)
	}

	thirdSrc, err := os.ReadFile(filepath.Join(projDir, "third", "third.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(thirdSrc), "func Bar() string") {
		t.Errorf("unrelated third.Bar was corrupted by the move:\n%s", thirdSrc)
	}

	goBuild(t, projDir)
}

// TestHandleMove_RefusesStructField covers the same unsupportedFieldOp
// gate every other write op goes through -- move never had explicit
// test coverage that this handler actually calls it.
func TestHandleMove_RefusesStructField(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(filepath.Join(projDir, "other"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\ntype Config struct {\n\tPort int\n}\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "other", "other.go"), []byte("package other\n\nfunc Baz() {}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:       "move",
		Name:     "Port",
		Receiver: "Config",
		Module:   "other",
	})
	if !result.IsError {
		t.Fatalf("expected struct field move to be refused, got success: %s", resultText(t, result))
	}
}

// TestHandleMove_RelocatesToTargetModuleDirectory is the core
// regression: move used to change ModuleID in the DB without ever
// updating SourceFile, so emit kept re-splicing the moved decl back
// into its OLD file (still declaring the OLD package) and never wrote
// it under the new module's directory at all -- a move that reported
// success while the source tree stayed byte-identical.
func TestHandleMove_RelocatesToTargetModuleDirectory(t *testing.T) {
	s, projDir, _ := setupMoveTestProject(t)

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:     "move",
		Name:   "Bar",
		Module: "other",
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") || strings.Contains(text, "WARNING") {
		t.Fatalf("move reported failure/warning: %s", text)
	}

	subSrc, err := os.ReadFile(filepath.Join(projDir, "sub", "sub.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(subSrc), "func Bar") {
		t.Errorf("sub/sub.go still contains Bar after move:\n%s", subSrc)
	}
	if !strings.Contains(string(subSrc), "func Foo") {
		t.Errorf("sub/sub.go lost unrelated Foo after move:\n%s", subSrc)
	}

	relocated := filepath.Join(projDir, "other", "sub.go")
	newSrc, err := os.ReadFile(relocated)
	if err != nil {
		t.Fatalf("expected %s to exist after move: %v", relocated, err)
	}
	if !strings.Contains(string(newSrc), "package other") {
		t.Errorf("relocated file has wrong package clause:\n%s", newSrc)
	}
	if !strings.Contains(string(newSrc), "func Bar() int") {
		t.Errorf("relocated file missing Bar's body:\n%s", newSrc)
	}

	goBuild(t, projDir)
}

// TestHandleMove_TargetModuleNotFound covers the plain error path: no
// fuzzy match for to_module leaves both the DB and the file tree
// untouched.
func TestHandleMove_TargetModuleNotFound(t *testing.T) {
	s, projDir, _ := setupMoveTestProject(t)

	before, err := os.ReadFile(filepath.Join(projDir, "sub", "sub.go"))
	if err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:     "move",
		Name:   "Bar",
		Module: "no-such-module-xyz",
	})
	if !result.IsError {
		t.Fatalf("expected an error result, got success: %s", resultText(t, result))
	}

	after, err := os.ReadFile(filepath.Join(projDir, "sub", "sub.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("sub/sub.go changed despite target-module-not-found error:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func goBuild(t *testing.T, projDir string) {
	t.Helper()
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed:\n%s", out)
	}
}

// setupMoveTestProject builds a two-package project (sub, other) plus a
// root main.go that imports sub, ingests and resolves it, and returns a
// ready server. Shared by every handleMove test below.
func setupMoveTestProject(t *testing.T) (*server, string, *store.SQLiteDB) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(filepath.Join(projDir, "sub"), 0755)
	os.MkdirAll(filepath.Join(projDir, "other"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nimport \"proj/sub\"\n\nfunc main() {\n\tsub.Foo()\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "sub", "sub.go"), []byte("package sub\n\nfunc Foo() {}\n\nfunc Bar() int {\n\treturn 1\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "other", "other.go"), []byte("package other\n\nfunc Baz() {}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)
	return s, projDir, db.(*store.SQLiteDB)
}

// TestHandleMove_MethodAcrossPackagesRollsBackNotJustFileWrite is the
// #12-class regression: moving a method across packages is
// unconditionally illegal under Go's own rules (a method's receiver
// type must live in the method's own package), so the resulting build
// always fails. handleMove used to write the delete+insert straight to
// s.backend with no transaction at all, so a failed move still left the
// DB durably claiming the method belonged to the new module even
// though the on-disk tree no longer built -- silent divergence between
// DB state and reality. Every other write handler already gates writes
// through Begin()/commitOrRollbackOnBuild; this asserts move does too.
func TestHandleMove_MethodAcrossPackagesRollsBackNotJustFileWrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, ".defn")
	db, err := store.OpenBackend(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	projDir := filepath.Join(dir, "proj")
	os.MkdirAll(filepath.Join(projDir, "sub"), 0755)
	os.MkdirAll(filepath.Join(projDir, "other"), 0755)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module proj\n\ngo 1.26\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "main.go"), []byte("package main\n\nimport \"proj/sub\"\n\nfunc main() {\n\tw := sub.Widget{N: 1}\n\t_ = w.Double()\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "sub", "sub.go"), []byte("package sub\n\ntype Widget struct {\n\tN int\n}\n\nfunc (w Widget) Double() int {\n\treturn w.N * 2\n}\n"), 0644)
	os.WriteFile(filepath.Join(projDir, "other", "other.go"), []byte("package other\n\nfunc Baz() {}\n"), 0644)

	if err := ingest.Ingest(db, projDir); err != nil {
		t.Fatal("ingest:", err)
	}
	if err := resolve.Resolve(db, projDir); err != nil {
		t.Fatal("resolve:", err)
	}

	s := &server{backend: db, projectDir: projDir}
	s.ready.Store(true)

	before, err := db.GetDefinitionByName("Double", "")
	if err != nil {
		t.Fatal(err)
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:       "move",
		Name:     "Double",
		Receiver: "Widget",
		Module:   "other",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected move to be refused/rolled back, got: %s", text)
	}

	after, err := db.GetDefinitionByName("Double", "")
	if err != nil {
		t.Fatalf("Double vanished from the DB after a rolled-back move: %v", err)
	}
	if after.ModuleID != before.ModuleID {
		t.Errorf("DB was mutated despite the move being rolled back: ModuleID %d -> %d", before.ModuleID, after.ModuleID)
	}
	if after.SourceFile != before.SourceFile {
		t.Errorf("DB was mutated despite the move being rolled back: SourceFile %q -> %q", before.SourceFile, after.SourceFile)
	}

	subSrc, err := os.ReadFile(filepath.Join(projDir, "sub", "sub.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(subSrc), "func (w Widget) Double() int") {
		t.Errorf("sub/sub.go lost Double despite the move being rolled back:\n%s", subSrc)
	}

	goBuild(t, projDir)
}
