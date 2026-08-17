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

// TestHandleRename_RefusesExternalInterfaceBreakingMethodRenameNotInAllowlist
// is the real-fix regression for the gap methodRenameRisksInterfaceBreak's
// doc comment used to describe as "out of scope": renaming a method whose
// only interface satisfaction is an EXTERNAL (stdlib here) interface, via a
// method name NOT in commonStdlibInterfaceMethodNames (io.ReaderAt.ReadAt --
// "ReadAt" was never on that list), with no local interface declared
// anywhere. Before resolve.go widened ifacesByPkg to external packages and
// persisted per-method satisfaction to def_external_interfaces, this took
// the fast no-build-check rename path and shipped a build that no longer
// compiled while reporting clean success -- confirmed live.
func TestHandleRename_RefusesExternalInterfaceBreakingMethodRenameNotInAllowlist(t *testing.T) {
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

import "io"

type File struct{}

func (f File) ReadAt(p []byte, off int64) (int, error) {
	return 0, nil
}

func use() io.ReaderAt {
	return File{}
}

func main() {}
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
		OldName:  "ReadAt",
		Receiver: "File",
		NewName:  "ReadAtX",
	})
	text := resultText(t, result)
	if !strings.Contains(text, "rolled back") {
		t.Fatalf("expected rename to be refused/rolled back, got: %s", text)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed on the unchanged tree after a rolled-back rename:\n%s", out)
	}
}

// TestHandleRename_SafeMethodRenameSkipsExternalInterfaceBuildGate is the
// companion negative case: a method that satisfies NO interface (local or
// external) must still take the fast no-build-check rename path -- the new
// def_external_interfaces check must not turn every rename into a full
// build validation regardless of relevance.
func TestHandleRename_SafeMethodRenameSkipsExternalInterfaceBuildGate(t *testing.T) {
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

type File struct{}

func (f File) DoStuff() int {
	return 1
}

func main() {
	_ = File{}.DoStuff()
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

	extIfaces, err := db.GetExternalInterfaces(mustDefID(t, db, "DoStuff", "File"))
	if err != nil {
		t.Fatal(err)
	}
	if len(extIfaces) != 0 {
		t.Fatalf("expected no external interfaces recorded for a method that satisfies none, got: %v", extIfaces)
	}

	result, _, _ := s.handleCode(context.Background(), nil, codeParam{
		Op:       "rename",
		OldName:  "DoStuff",
		Receiver: "File",
		NewName:  "DoStuffX",
	})
	text := resultText(t, result)
	if strings.Contains(text, "rolled back") {
		t.Fatalf("expected a safe rename to succeed, got: %s", text)
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = projDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./... failed after a safe rename:\n%s", out)
	}
}

func mustDefID(t *testing.T, db store.Backend, name, receiver string) int64 {
	t.Helper()
	d, err := db.GetDefinitionByNameAndReceiver(name, "", receiver)
	if err != nil || d == nil {
		t.Fatalf("could not find definition %s.%s: %v", receiver, name, err)
	}
	return d.ID
}
