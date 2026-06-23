package main

import (
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
