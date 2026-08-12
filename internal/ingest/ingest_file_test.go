package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIngestFile_NestedModuleUsesOwnGoMod is a regression for a real
// multi-module bug found via etcd bench trajectories: IngestFile
// (the incremental single-file sync path used by code(op:"sync",
// file:...)) always read go.mod from the passed-in modulePath (the
// repo root), even for a file under a subdirectory that declares its
// OWN go.mod -- computing a bogus package path (root module prefix +
// relative dir) instead of the nested module's real declared name.
// That corrupted module record then couldn't be found by later
// module:-qualified create/add-import/test/overview calls using the
// file's REAL module path, and made real `go build`/`go test`
// invocations against it fail with "main module ... does not contain
// package ...". IngestFile must walk up to the nearest go.mod, like
// etcd's server/, tests/, and etcdctl/ subtrees each have their own.
func TestIngestFile_NestedModuleUsesOwnGoMod(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/root\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "server")
	if err := os.MkdirAll(filepath.Join(nested, "pkg"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module example.com/server/v2\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	src := "package pkg\n\nfunc Foo() {}\n"
	filePath := filepath.Join(nested, "pkg", "foo.go")
	if err := os.WriteFile(filePath, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}

	db := testDB(t)
	if _, err := IngestFile(db, root, filePath); err != nil {
		t.Fatal(err)
	}

	const wantModule = "example.com/server/v2/pkg"
	if m, err := db.GetModuleByPath(wantModule); err != nil || m == nil {
		t.Errorf("expected module %q to be registered, got module=%v err=%v", wantModule, m, err)
	}

	const bogusModule = "example.com/root/server/pkg"
	if m, err := db.GetModuleByPath(bogusModule); err == nil && m != nil {
		t.Errorf("nested-module file was ingested under the bogus root-prefixed path %q (module id=%d) instead of its own go.mod's declared module", bogusModule, m.ID)
	}

	def, err := db.GetDefinitionByName("Foo", wantModule)
	if err != nil {
		t.Fatalf("Foo not found under module %q: %v", wantModule, err)
	}
	if def.Name != "Foo" {
		t.Errorf("expected def named Foo, got %q", def.Name)
	}
}
