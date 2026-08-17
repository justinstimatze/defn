// Package goload provides shared utilities for loading Go packages
// with go/packages, used by both ingest and resolve.
package goload

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// LoadAll is LoadPattern with pattern "./..." (see LoadPattern's doc
// for the scoped-pattern use case), PLUS every nested Go module found
// under dir (see discoverNestedModuleDirs) -- "./..." never crosses a
// nested go.mod boundary, so a plain single LoadPattern call silently
// misses every nested module's packages entirely, not just filters
// them out. Each nested module's own packages.Load is best-effort: a
// broken/incomplete nested module (network-gated deps, a stray
// experimental go.mod, etc.) is skipped with a warning rather than
// failing the whole ingest over one subtree.
func LoadAll(dir string) ([]*packages.Package, error) {
	pkgs, err := LoadPattern(dir, "./...")
	if err != nil {
		return nil, err
	}
	nested, derr := discoverNestedModuleDirs(dir)
	if derr != nil {
		// Discovery is a plain filesystem walk with no packages.Load
		// cost -- a failure here (e.g. permission error) shouldn't sink
		// an otherwise-successful root ingest.
		fmt.Fprintf(os.Stderr, "goload: warning: nested module discovery failed: %v\n", derr)
		return pkgs, nil
	}
	for _, nd := range nested {
		nestedPkgs, nerr := LoadPattern(nd, "./...")
		if nerr != nil {
			fmt.Fprintf(os.Stderr, "goload: warning: skipping nested module %s: %v\n", nd, nerr)
			continue
		}
		pkgs = append(pkgs, nestedPkgs...)
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

// discoverNestedModuleDirs walks rootDir for subdirectories containing
// their own go.mod -- separate Go modules nested inside the project
// (common in real multi-module repos: etcd's server/, tests/, client/v3
// etc. each declare their own module, whether or not a go.work ties
// them together). "./..." package-pattern expansion never crosses a
// nested go.mod boundary -- that's standard go/packages behavior, not
// a defn bug, but it means a plain LoadAll(rootDir) silently indexes
// ONLY the root module. A real etcd bench trajectory (2026-08:
// etcd-io/etcd#20929) hit this directly: the entire tests/ module
// (tests/go.mod, declaring go.etcd.io/etcd/tests/v3) had zero
// definitions in the database despite a full `defn ingest .` having
// supposedly completed -- search/overview/sync all correctly reported
// "not found" for real, existing code because it was never ingested in
// the first place, not because of any per-op lookup bug. The agent
// burned its entire turn budget searching exhaustively and guessing
// file paths before giving up, having never had a chance to succeed.
//
// Mirrors cmd/defn's walkGoFiles skip-dir set. Root-relative go.mod
// (rootDir itself) is excluded -- callers already load that via their
// own LoadPattern(rootDir, "./...") call.
func discoverNestedModuleDirs(rootDir string) ([]string, error) {
	var nested []string
	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if name == ".defn" || name == ".git" || name == "vendor" ||
			name == "node_modules" || name == "testdata" {
			return filepath.SkipDir
		}
		if path == rootDir {
			return nil
		}
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
			nested = append(nested, path)
			return filepath.SkipDir // a module's own go.mod may itself have nested modules; a future pass, not needed for any real repo seen so far
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return nested, nil
}
