package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestDiffFileSets(t *testing.T) {
	projectDir := "/repo"
	abs := func(rel string) string { return filepath.Join(projectDir, rel) }

	cases := []struct {
		name        string
		disk        []string
		db          []string
		wantAdded   []string // abs
		wantDeleted []string // rel
	}{
		{
			name: "no churn",
			disk: []string{abs("a.go"), abs("pkg/b.go")},
			db:   []string{"a.go", "pkg/b.go"},
		},
		{
			name:      "single add",
			disk:      []string{abs("a.go"), abs("pkg/b.go"), abs("c.go")},
			db:        []string{"a.go", "pkg/b.go"},
			wantAdded: []string{abs("c.go")},
		},
		{
			name:        "single delete",
			disk:        []string{abs("a.go")},
			db:          []string{"a.go", "pkg/b.go"},
			wantDeleted: []string{"pkg/b.go"},
		},
		{
			name:        "rename — looks like one add + one delete",
			disk:        []string{abs("a.go"), abs("renamed.go")},
			db:          []string{"a.go", "old.go"},
			wantAdded:   []string{abs("renamed.go")},
			wantDeleted: []string{"old.go"},
		},
		{
			name:        "empty db, fresh disk → all added",
			disk:        []string{abs("a.go"), abs("b.go")},
			db:          nil,
			wantAdded:   []string{abs("a.go"), abs("b.go")},
			wantDeleted: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAdded, gotDeleted := diffFileSets(projectDir, tc.disk, tc.db)
			sort.Strings(gotAdded)
			sort.Strings(gotDeleted)
			sort.Strings(tc.wantAdded)
			sort.Strings(tc.wantDeleted)
			if !stringSlicesEqual(gotAdded, tc.wantAdded) {
				t.Errorf("added = %v, want %v", gotAdded, tc.wantAdded)
			}
			if !stringSlicesEqual(gotDeleted, tc.wantDeleted) {
				t.Errorf("deleted = %v, want %v", gotDeleted, tc.wantDeleted)
			}
		})
	}
}

func TestSubtractAbsPaths(t *testing.T) {
	xs := []string{"/a", "/b", "/c"}
	ys := []string{"/b"}
	got := subtractAbsPaths(xs, ys)
	if !stringSlicesEqual(got, []string{"/a", "/c"}) {
		t.Errorf("got %v, want [/a /c]", got)
	}
	// Empty ys → xs unchanged.
	if got := subtractAbsPaths([]string{"/a"}, nil); !stringSlicesEqual(got, []string{"/a"}) {
		t.Errorf("nil ys should pass through xs, got %v", got)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWalkGoFilesSkipsNestedModule(t *testing.T) {
	// Regression test for task #239's real root cause: a subdirectory
	// with its own go.mod (grpc-go's security/advancedtls, test/tools,
	// etc.) is a separate Go module. goload.LoadAll's packages.Load
	// never crosses that boundary, so a full `defn init` never has defs
	// for it -- but before this fix, walkGoFiles (used by the
	// incremental fast path that `defn ingest` runs right after) walked
	// into it anyway, found real .go files, and fed them to
	// ingest.IngestFile under the WRONG (root) module's path. A later
	// resolve pass against that mis-scoped package then unreliably wiped
	// what IngestFile had just added, leaving a permanent zero-def
	// module that looked identical to one whose last def was
	// legitimately deleted.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/root\n\ngo 1.23\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "nested")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module example.com/root/nested\n\ngo 1.23\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "real.go"), []byte("package nested\n\nfunc RealFunc() int { return 1 }\n"), 0644); err != nil {
		t.Fatal(err)
	}

	all, _ := walkGoFiles(dir, 0)

	nestedFile := filepath.Join(nested, "real.go")
	for _, f := range all {
		if f == nestedFile {
			t.Fatalf("walkGoFiles crossed the nested module boundary and returned %s -- this is exactly what fed the wrong-module-path ingest bug", f)
		}
	}
	rootFile := filepath.Join(dir, "main.go")
	found := false
	for _, f := range all {
		if f == rootFile {
			found = true
		}
	}
	if !found {
		t.Fatalf("walkGoFiles should still find the root module's own files, got %v", all)
	}
}
