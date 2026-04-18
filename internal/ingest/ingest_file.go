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

// IngestFile re-parses a single Go file and updates definitions in the database.
// This is much faster than a full Ingest (~10ms vs ~30s) because it uses
// go/parser directly instead of packages.Load.
//
// It updates bodies, signatures, and line numbers for existing definitions,
// and adds new definitions found in the file. It does NOT update references
// (call graph) — use resolve.Resolve for that after structural changes.
func IngestFile(db *store.DB, modulePath string, filePath string) (int, error) {
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

	// Determine the package path from go.mod module prefix + relative dir.
	pkgDir := filepath.Dir(relFile)
	isTest := strings.HasSuffix(relFile, "_test.go")

	modPrefix, err := readModulePath(filepath.Join(absModule, "go.mod"))
	if err != nil {
		return 0, fmt.Errorf("read go.mod: %w", err)
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

	state := &ingestState{
		initCounter: make(map[int64]int),
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
