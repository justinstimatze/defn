// Package goload provides shared utilities for loading Go packages
// with go/packages, used by both ingest and resolve.
package goload

import (
	"strings"

	"golang.org/x/tools/go/packages"
)

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
