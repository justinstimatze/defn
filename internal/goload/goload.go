// Package goload provides shared utilities for loading Go packages
// with go/packages, used by both ingest and resolve.
package goload

import (
	"fmt"
	"strings"

	"golang.org/x/tools/go/packages"
)

// LoadAll loads all Go packages in dir with the superset of modes needed
// by both ingest and resolve. The result can be passed to both
// ingest.IngestPackages and resolve.ResolvePackages.
//
// Deliberately omits packages.NeedDeps: we only ingest module packages,
// never transitive deps. Without NeedDeps, the type checker still loads
// types.Package for imports via compiled export data from GOCACHE —
// cheap compared to loading full AST + type info for every dep. On a
// heavy module (e.g. defn itself, which imports Dolt) this drops peak
// RSS from ~5 GB to under 1 GB without losing cross-module ref
// resolution.
func LoadAll(dir string) ([]*packages.Package, error) {
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
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	return pkgs, nil
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
