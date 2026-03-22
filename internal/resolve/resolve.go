// Package resolve uses go/types information to build the references table,
// mapping which definitions reference which other definitions.
package resolve

import (
	"go/ast"
	"go/types"

	"github.com/justinstimatze/defn/internal/store"
	"golang.org/x/tools/go/packages"
)

// Resolve analyzes all loaded packages and populates the references table.
func Resolve(db *store.DB, modulePath string) error {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedDeps,
		Dir: modulePath,
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return err
	}

	// Build a map from types.Object → definition ID.
	objToDef := make(map[types.Object]int64)

	// First pass: map each defined object to its definition ID.
	for _, pkg := range pkgs {
		for ident, obj := range pkg.TypesInfo.Defs {
			if obj == nil || ident.Name == "_" {
				continue
			}
			defID := lookupDefID(db, pkg.PkgPath, ident, obj)
			if defID > 0 {
				objToDef[obj] = defID
			}
		}
	}

	// Second pass: for each function/method body, find all Uses and create references.
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}

				fromID := lookupFuncDefID(db, pkg.PkgPath, fn)
				if fromID <= 0 {
					continue
				}

				refs := collectRefs(fn.Body, pkg.TypesInfo, objToDef)
				if len(refs) > 0 {
					if err := db.SetReferences(fromID, refs); err != nil {
						return err
					}
				}
			}
		}
	}

	return nil
}

func collectRefs(body *ast.BlockStmt, info *types.Info, objToDef map[types.Object]int64) []store.Reference {
	seen := make(map[int64]string)
	var refs []store.Reference

	ast.Inspect(body, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		obj, exists := info.Uses[ident]
		if !exists {
			return true
		}
		toID, exists := objToDef[obj]
		if !exists {
			return true
		}

		kind := classifyRef(obj)
		key := toID
		if _, dup := seen[key]; !dup {
			seen[key] = kind
			refs = append(refs, store.Reference{
				ToDef: toID,
				Kind:  kind,
			})
		}
		return true
	})
	return refs
}

func classifyRef(obj types.Object) string {
	switch obj.(type) {
	case *types.Func:
		return "call"
	case *types.TypeName:
		return "type_ref"
	case *types.Var:
		return "field_access"
	default:
		return "ref"
	}
}

func lookupDefID(db *store.DB, pkgPath string, ident *ast.Ident, obj types.Object) int64 {
	d, err := db.GetDefinitionByName(ident.Name, pkgPath)
	if err != nil {
		return 0
	}
	return d.ID
}

func lookupFuncDefID(db *store.DB, pkgPath string, fn *ast.FuncDecl) int64 {
	d, err := db.GetDefinitionByName(fn.Name.Name, pkgPath)
	if err != nil {
		return 0
	}
	return d.ID
}
