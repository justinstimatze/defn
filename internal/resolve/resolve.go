// Package resolve uses go/types information to build the references table,
// mapping which definitions reference which other definitions.
package resolve

import (
	"go/ast"
	"go/types"
	"strings"

	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/store"
	"golang.org/x/tools/go/packages"
)

// Resolve analyzes all loaded packages and populates the references table.
// Includes test packages so test→definition references are captured.
func Resolve(db *store.DB, modulePath string) error {
	return resolve(db, modulePath, "")
}

// ResolveModule is like Resolve but only updates references for definitions
// in the specified module. Still loads all packages for type information,
// but skips reference extraction for other modules. Much faster for
// single-definition edits.
func ResolveModule(db *store.DB, projectDir, modulePath string) error {
	return resolve(db, projectDir, modulePath)
}

func resolve(db *store.DB, projectDir, onlyModule string) error {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedDeps,
		Dir:   projectDir,
		Tests: true,
	}

	// Always load all packages for the objToDef map (cross-package references
	// need type info from sibling packages, not just dependencies).
	// When onlyModule is set, the second pass only extracts references from
	// that module — skipping the expensive body walk for other packages.
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return err
	}

	filtered := goload.FilterPackages(pkgs)

	// Build a map from types.Object → definition ID (all packages).
	objToDef := make(map[types.Object]int64)

	for _, pkg := range filtered {
		pkgPath := pkg.PkgPath
		if strings.HasSuffix(pkg.Name, "_test") {
			pkgPath = strings.TrimSuffix(pkgPath, "_test")
		}
		for ident, obj := range pkg.TypesInfo.Defs {
			if obj == nil || ident.Name == "_" {
				continue
			}
			defID := lookupDefID(db, pkgPath, ident, obj)
			if defID > 0 {
				objToDef[obj] = defID
			}
		}
	}

	// Second pass: extract references from function bodies.
	// If onlyModule is set, only process packages matching that module path.
	for _, pkg := range filtered {
		pkgPath := pkg.PkgPath
		if strings.HasSuffix(pkg.Name, "_test") {
			pkgPath = strings.TrimSuffix(pkgPath, "_test")
		}

		if onlyModule != "" && pkgPath != onlyModule {
			continue
		}

		for _, file := range pkg.Syntax {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}

				fromID := lookupFuncDefID(db, pkgPath, fn)
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
	// For methods, use receiver-qualified lookup to avoid ambiguity.
	if fn, ok := obj.(*types.Func); ok {
		sig := fn.Signature()
		if sig != nil && sig.Recv() != nil {
			recv := receiverName(sig.Recv().Type())
			d, err := db.GetDefinitionByNameAndReceiver(ident.Name, pkgPath, recv)
			if err == nil {
				return d.ID
			}
		}
	}
	d, err := db.GetDefinitionByName(ident.Name, pkgPath)
	if err != nil {
		return 0
	}
	return d.ID
}

func lookupFuncDefID(db *store.DB, pkgPath string, fn *ast.FuncDecl) int64 {
	// For methods, include receiver in lookup.
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		recv := types.ExprString(fn.Recv.List[0].Type)
		d, err := db.GetDefinitionByNameAndReceiver(fn.Name.Name, pkgPath, recv)
		if err == nil {
			return d.ID
		}
	}
	d, err := db.GetDefinitionByName(fn.Name.Name, pkgPath)
	if err != nil {
		return 0
	}
	return d.ID
}

// receiverName extracts a short receiver name from a types.Type.
// e.g., *Context, JSON, *node
func receiverName(t types.Type) string {
	s := t.String()
	// Strip package path: "github.com/gin-gonic/gin.*Context" → "*Context"
	if idx := strings.LastIndex(s, "."); idx >= 0 {
		prefix := ""
		// Check if it's a pointer.
		if strings.Contains(s[:idx], "*") {
			prefix = "*"
			s = strings.Replace(s, "*", "", 1)
			if idx2 := strings.LastIndex(s, "."); idx2 >= 0 {
				return prefix + s[idx2+1:]
			}
		}
		return prefix + s[idx+1:]
	}
	return s
}
