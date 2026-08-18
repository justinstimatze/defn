package ingest

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
)

func IngestFile(db store.Backend, modulePath string, filePath string) (int, error) {
	absModule, err := filepath.Abs(modulePath)
	if err != nil {
		return 0, fmt.Errorf("abs module path: %w", err)
	}
	absFile, err := filepath.Abs(filePath)
	if err != nil {
		return 0, fmt.Errorf("abs file path: %w", err)
	}

	relFile, err := filepath.Rel(absModule, absFile)
	if err != nil {
		return 0, fmt.Errorf("rel path: %w", err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, absFile, nil, parser.ParseComments)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", relFile, err)
	}

	// Determine the package path from the NEAREST go.mod's module
	// prefix + the dir's path relative to that go.mod, not
	// necessarily absModule's go.mod -- see ModuleForDir's doc.
	isTest := strings.HasSuffix(relFile, "_test.go")

	modPrefix, nearestModDir, err := ModuleForDir(filepath.Dir(absFile))
	if err != nil {
		return 0, fmt.Errorf("read go.mod: %w", err)
	}
	pkgDir, err := filepath.Rel(nearestModDir, filepath.Dir(absFile))
	if err != nil {
		return 0, fmt.Errorf("rel pkg dir: %w", err)
	}

	pkgPath := modPrefix
	if pkgDir != "." {
		pkgPath = modPrefix + "/" + filepath.ToSlash(pkgDir)
	}

	// Strip _test suffix from external test packages.
	pkgName := file.Name.Name
	if before, ok := strings.CutSuffix(pkgName, "_test"); ok {
		pkgName = before
		if before, ok := strings.CutSuffix(pkgPath, "_test"); ok {
			pkgPath = before
		}
	}

	mod, err := db.EnsureModule(pkgPath, pkgName, "")
	if err != nil {
		return 0, fmt.Errorf("ensure module: %w", err)
	}

	// Phase C: capture raw source as the authoritative representation.
	if raw, err := os.ReadFile(absFile); err == nil {
		if err := db.SetFileSource(mod.ID, relFile, string(raw)); err != nil {
			return 0, fmt.Errorf("set file source: %w", err)
		}
	}

	// Clear the source file cache so renderNode reads fresh content.
	sourceFileMu.Lock()
	delete(sourceFileCache, absFile)
	sourceFileMu.Unlock()

	// #NEW: snapshot this file's currently-known def IDs BEFORE
	// re-parsing, so any not reproduced below (removed/renamed on disk
	// -- e.g. a var dropped from a grouped `var (...)` block) can be
	// pruned. Full-project ingest already does this project-wide via
	// PruneStaleDefinitions, but this fast single-file path (what
	// op:"sync", file:... actually runs) never did -- a def orphaned
	// this way stayed in the DB forever, and every future write to that
	// file hit emit's "could not be matched to an on-disk declaration"
	// warning, whose own suggested remedy ("run code(op:\"sync\",
	// file:...)") never actually cleared it. Confirmed live
	// (prometheus-18712, Opus): the model called sync on the affected
	// file twice (once with a full resync) and got the identical
	// warning both times, only unblocked by manually force-deleting the
	// stale def after ~15 rounds of trial and error.
	dirForLookup := ""
	if idx := strings.LastIndex(relFile, "/"); idx >= 0 {
		dirForLookup = relFile[:idx]
	}
	var previousIDs map[int64]bool
	if existing, existErr := db.FindDefinitionsByFile(dirForLookup, relFile, 0); existErr == nil {
		previousIDs = make(map[int64]bool, len(existing))
		for _, d := range existing {
			previousIDs[d.ID] = true
		}
	}

	state := &ingestState{
		initCounter: make(map[string]int),
		liveDefIDs:  make(map[int64]bool),
	}

	updated := 0
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if err := ingestFunc(db, fset, mod, file, d, isTest, relFile, state); err != nil {
				return updated, err
			}
			updated++
		case *ast.GenDecl:
			if err := ingestGenDecl(db, fset, mod, file, d, isTest, relFile, state); err != nil {
				return updated, err
			}
			updated++
		}
	}

	// #125: flush buffered defs. Single-file ingest → single flush at end.
	if err := state.flushDefs(db); err != nil {
		return updated, fmt.Errorf("flush defs: %w", err)
	}

	// Prune defs this file used to have that this parse didn't reproduce.
	for id := range previousIDs {
		if !state.liveDefIDs[id] {
			if err := db.DeleteDefinition(id); err != nil {
				return updated, fmt.Errorf("prune stale def %d: %w", id, err)
			}
		}
	}

	// #224: link doc/pragma comments to this file's defs. Must run AFTER
	// flushDefs -- ingestComments looks defs up via FindDefinitionsByFile,
	// which (like #223's full-ingest bug) would otherwise race the
	// not-yet-visible batched upserts for defs new in this file.
	if len(file.Comments) > 0 {
		if err := ingestComments(db, fset, file, relFile); err != nil {
			return updated, fmt.Errorf("ingest comments: %w", err)
		}
	}

	return updated, nil
}

// readModulePath reads the module path from go.mod.
func readModulePath(goModPath string) (string, error) {
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", fmt.Errorf("no module directive in %s", goModPath)
}

// ModuleForDir finds the nearest go.mod at or above dir (walking up
// the filesystem, crossing into parent directories until one is
// found) and returns its declared module path together with the
// directory that go.mod lives in. Callers compute a package's import
// path as modulePrefix + "/" + relative(modDir, dir).
//
// This exists because a subdirectory with its own go.mod is a
// separate Go module (common in real multi-module repos -- etcd's
// server/, tests/, etcdctl/ etc. each declare their own module).
// Blindly using the repo root's go.mod for every file, regardless of
// which go.mod actually governs it, computes a package path that
// doesn't exist (root module prefix + relative dir instead of the
// nested module's own declared name) -- corrupting the module record
// for that file and breaking every later module:-qualified lookup or
// `go build`/`go test` invocation against it.
func ModuleForDir(dir string) (modulePrefix, modDir string, err error) {
	modDir, err = nearestModuleDir(dir)
	if err != nil {
		return "", "", err
	}
	modulePrefix, err = readModulePath(filepath.Join(modDir, "go.mod"))
	if err != nil {
		return "", "", err
	}
	return modulePrefix, modDir, nil
}

// nearestModuleDir walks up from dir looking for the nearest go.mod,
// stopping at the filesystem root. Mirrors cmd/defn's findModuleRoot --
// duplicated rather than shared because that one lives in package main
// and takes a file path instead of a directory.
func nearestModuleDir(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}
