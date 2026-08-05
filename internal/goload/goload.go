// Package goload provides shared utilities for loading Go packages
// with go/packages, used by both ingest and resolve.
package goload

import (
	"fmt"
	"strings"

	"golang.org/x/tools/go/packages"
)

// LoadAll loads every Go package in dir (module-wide, "./...") with
// the superset of modes needed by both ingest and resolve. LoadAll is
// LoadPattern with pattern "./..." -- see LoadPattern's doc for the
// scoped-pattern use case.
func LoadAll(dir string) ([]*packages.Package, error) {
	return LoadPattern(dir, "./...")
}

// FilterPackages removes synthetic test binaries and deduplicates packages
// by preferring test variants (which include both test and non-test files)
// over base variants.
func FilterPackages(pkgs []*packages.Package) []*packages.Package {
	var filtered []*packages.Package
	for _, pkg := range pkgs {
		// Skip synthetic test binary packages (Name=main, PkgPath=*.test).
		if pkg.Name == "main" && strings.HasSuffix(pkg.PkgPath, ".test") {
			continue
		}
		// Skip the base variant when a test variant exists (the test variant
		// has all the files including tests, so it's a superset).
		if !strings.Contains(pkg.ID, "[") {
			hasTestVariant := false
			for _, other := range pkgs {
				if strings.Contains(other.ID, "[") && other.PkgPath == pkg.PkgPath {
					hasTestVariant = true
					break
				}
			}
			if hasTestVariant {
				continue
			}
		}
		filtered = append(filtered, pkg)
	}
	return filtered
}

// LoadPattern loads Go packages in dir matching pattern with the
// superset of modes needed by both ingest and resolve. The result can
// be passed to both ingest.IngestPackages and resolve.ResolvePackages.
// LoadAll is LoadPattern scoped to the whole module ("./..."); pass "."
// to scope to just the root package -- an embedder whose corpus is a
// single declarative package otherwise pays full-module type-checking
// on every ingest/Sync, and a change to any unrelated package marks
// its DB stale for no benefit to that embedder's own package.
//
// Deliberately omits packages.NeedDeps: we only ingest module packages,
// never transitive deps. Without NeedDeps, the type checker still loads
// types.Package for imports via compiled export data from GOCACHE —
// cheap compared to loading full AST + type info for every dep. On a
// heavy module with many third-party dependencies, this drops peak
// RSS substantially without losing cross-module ref resolution.
func LoadPattern(dir, pattern string) ([]*packages.Package, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedEmbedPatterns,
		Dir:   dir,
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	return pkgs, nil
}
