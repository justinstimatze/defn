package goload

import (
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestFilterPackages_NoOverlappingSyntax is the #161 regression guard.
// FilterPackages must not return two packages that share both PkgPath
// AND overlapping pkg.Syntax entries — that shape drives the ingest
// layer to enqueue the same def twice within one flushDefs, which
// hits the unique constraint on (module_id, name, kind, receiver,
// test). The a290980 within-batch dedup in the store is a defensive
// backstop for this class; this test ensures the upstream contract
// holds so the guard stays a belt, not a suspenders.
//
// Runs against defn's own tree (which has store, mcp, emit, etc.
// with real _test.go siblings) so the check covers a realistic Go
// module layout with both internal and external test files.
func TestFilterPackages_NoOverlappingSyntax(t *testing.T) {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedImports,
		Dir:   "../..", // defn repo root
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	filtered := FilterPackages(pkgs)

	// Group filtered packages by PkgPath. If any PkgPath maps to more
	// than one filtered package, they'd both be ingested and their
	// pkg.Syntax entries would collide on any shared file (which is
	// how the duplicate-key ingest bug reproduced).
	byPath := make(map[string][]*packages.Package)
	for _, p := range filtered {
		byPath[p.PkgPath] = append(byPath[p.PkgPath], p)
	}
	for path, ps := range byPath {
		if len(ps) < 2 {
			continue
		}
		// Two filtered packages with the same PkgPath are only safe
		// if their pkg.Syntax filenames are disjoint. Compare them.
		seen := make(map[string]int) // filename → count
		for _, p := range ps {
			for _, f := range p.Syntax {
				if f.Pos().IsValid() {
					name := p.Fset.Position(f.Pos()).Filename
					seen[name]++
				}
			}
		}
		var shared []string
		for name, n := range seen {
			if n > 1 {
				shared = append(shared, name)
			}
		}
		if len(shared) > 0 {
			t.Errorf("PkgPath %q has %d filtered packages sharing %d file(s): %s\n  ids: %s",
				path, len(ps), len(shared),
				strings.Join(shared, ", "),
				strings.Join(pkgIDs(ps), " | "))
		}
	}
}

func pkgIDs(ps []*packages.Package) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}
