// Package resolve uses go/types information to build the references table,
// mapping which definitions reference which other definitions.
package resolve

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/token"
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

	// Second pass: interface satisfaction — build ifaceMethodToImpls map
	// BEFORE extracting references, so collectRefs can resolve interface calls.
	// Build a map from interface method objects → concrete method definition IDs.
	// This is used by collectRefs to resolve interface dispatch calls.
	ifaceMethodToImpls := map[types.Object][]int64{}

	for _, pkg := range filtered {
		pkgPath := pkg.PkgPath
		if strings.HasSuffix(pkg.Name, "_test") {
			pkgPath = strings.TrimSuffix(pkgPath, "_test")
		}
		if onlyModule != "" && pkgPath != onlyModule {
			continue
		}

		scope := pkg.Types.Scope()
		if scope == nil {
			continue
		}

		// Collect all named types and interfaces in this package.
		var namedTypes []*types.Named
		var ifaces []*types.Named
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			if types.IsInterface(named) {
				ifaces = append(ifaces, named)
			} else {
				namedTypes = append(namedTypes, named)
			}
		}

		// Check each (concrete, interface) pair.
		for _, concrete := range namedTypes {
			for _, iface := range ifaces {
				ifaceType, ok := iface.Underlying().(*types.Interface)
				if !ok || ifaceType.NumMethods() == 0 {
					continue
				}

				// Check T and *T.
				satisfies := types.Implements(concrete, ifaceType) ||
					types.Implements(types.NewPointer(concrete), ifaceType)
				if !satisfies {
					continue
				}

				// Find defn IDs for the concrete type and interface.
				concreteID := lookupTypeDefID(db, pkgPath, concrete.Obj().Name())
				ifaceID := lookupTypeDefID(db, pkgPath, iface.Obj().Name())

				// Add "implements" edge: concrete type → interface.
				if concreteID > 0 && ifaceID > 0 {
					db.SetReferences(concreteID, []store.Reference{
						{ToDef: ifaceID, Kind: "implements"},
					})
				}

				// Map interface method objects → concrete method def IDs.
				for ifaceMethod := range ifaceType.Methods() {
					ifaceMethod := ifaceMethod
					concreteMethodID := lookupMethodDefID(db, pkgPath, concrete.Obj().Name(), ifaceMethod.Name())
					if concreteMethodID > 0 {
						ifaceMethodToImpls[ifaceMethod] = append(ifaceMethodToImpls[ifaceMethod], concreteMethodID)
					}
				}
			}
		}
	}

	// Third pass: extract references from function bodies AND package-level
	// var/const initializers and type definitions.
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
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Body == nil {
						continue
					}
					fromID := lookupFuncDefID(db, pkgPath, d)
					if fromID <= 0 {
						continue
					}
					refs, litFields := collectRefs(d.Body, pkg.TypesInfo, pkg.Fset, objToDef, ifaceMethodToImpls)
					if len(refs) > 0 {
						if err := db.SetReferences(fromID, refs); err != nil {
							return err
						}
					}
					if len(litFields) > 0 {
						if err := db.SetLiteralFields(fromID, litFields); err != nil {
							return err
						}
					}

				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.ValueSpec:
							// var/const initializers: var X = someFunc(...)
							for i, name := range s.Names {
								if name.Name == "_" {
									continue
								}
								fromID := lookupVarDefID(db, pkgPath, name.Name)
								if fromID <= 0 {
									continue
								}
								// Collect refs from the value expression.
								var nodes []ast.Node
								if i < len(s.Values) {
									nodes = append(nodes, s.Values[i])
								}
								// Also collect refs from the type expression.
								if s.Type != nil {
									nodes = append(nodes, s.Type)
								}
								for _, node := range nodes {
									refs, litFields := collectRefs(node, pkg.TypesInfo, pkg.Fset, objToDef, ifaceMethodToImpls)
									if len(refs) > 0 {
										if err := db.SetReferences(fromID, refs); err != nil {
											return err
										}
									}
									if len(litFields) > 0 {
										if err := db.SetLiteralFields(fromID, litFields); err != nil {
											return err
										}
									}
								}
							}

						case *ast.TypeSpec:
							// Type definitions: struct fields, embedded types, interface methods.
							fromID := lookupTypeDefID(db, pkgPath, s.Name.Name)
							if fromID <= 0 {
								continue
							}
							refs, litFields := collectRefs(s.Type, pkg.TypesInfo, pkg.Fset, objToDef, ifaceMethodToImpls)
							if len(refs) > 0 {
								if err := db.SetReferences(fromID, refs); err != nil {
									return err
								}
							}
							if len(litFields) > 0 {
								if err := db.SetLiteralFields(fromID, litFields); err != nil {
									return err
								}
							}
						}
					}
				}
			}
		}
	}

	return nil
}

func lookupTypeDefID(db *store.DB, pkgPath, typeName string) int64 {
	d, err := db.GetDefinitionByName(typeName, pkgPath)
	if err != nil {
		return 0
	}
	return d.ID
}

func lookupMethodDefID(db *store.DB, pkgPath, typeName, methodName string) int64 {
	// Try *Type first (most methods have pointer receivers).
	d, err := db.GetDefinitionByNameAndReceiver(methodName, pkgPath, "*"+typeName)
	if err == nil {
		return d.ID
	}
	// Try value receiver.
	d, err = db.GetDefinitionByNameAndReceiver(methodName, pkgPath, typeName)
	if err == nil {
		return d.ID
	}
	return 0
}

// lookupVarDefID finds the definition ID for a package-level var or const.
func lookupVarDefID(db *store.DB, pkgPath, name string) int64 {
	d, err := db.GetDefinitionByName(name, pkgPath)
	if err != nil {
		return 0
	}
	return d.ID
}

func collectRefs(node ast.Node, info *types.Info, fset *token.FileSet, objToDef map[types.Object]int64, ifaceMethodToImpls map[types.Object][]int64) ([]store.Reference, []store.LiteralField) {
	seen := make(map[int64]string)
	var refs []store.Reference
	var litFields []store.LiteralField

	addRef := func(toID int64, kind string) {
		if _, dup := seen[toID]; !dup {
			seen[toID] = kind
			refs = append(refs, store.Reference{ToDef: toID, Kind: kind})
		}
	}

	// Pre-scan: collect idents of embedded struct fields so we can
	// classify them as "embed" instead of "type_ref" in the main walk.
	embeddedIdents := map[*ast.Ident]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		st, ok := n.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			if len(field.Names) == 0 { // embedded field
				if id := innerIdent(field.Type); id != nil {
					embeddedIdents[id] = true
				}
			}
		}
		return true
	})

	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			// Struct literal: Type{...} or &Type{...}.
			if x.Type != nil {
				if tv, ok := info.Types[x.Type]; ok {
					typ := tv.Type
					if ptr, ok := typ.(*types.Pointer); ok {
						typ = ptr.Elem()
					}
					if named, ok := typ.(*types.Named); ok {
						if toID, ok := objToDef[named.Obj()]; ok {
							addRef(toID, "constructor")
						}
						// Extract field-level data from keyed composite literals.
						typeName := named.Obj().Name()
						if pkg := named.Obj().Pkg(); pkg != nil {
							typeName = pkg.Path() + "." + typeName
						}
						for _, elt := range x.Elts {
							kv, ok := elt.(*ast.KeyValueExpr)
							if !ok {
								continue
							}
							ident, ok := kv.Key.(*ast.Ident)
							if !ok {
								continue
							}
							var buf bytes.Buffer
							if err := format.Node(&buf, fset, kv.Value); err != nil {
								continue
							}
							litFields = append(litFields, store.LiteralField{
								TypeName:   typeName,
								FieldName:  ident.Name,
								FieldValue: buf.String(),
								Line:       fset.Position(kv.Pos()).Line,
							})
						}
					}
				}
			}
			return true
		case *ast.CallExpr:
			// new(Type) builtin.
			if ident, ok := x.Fun.(*ast.Ident); ok {
				if bi, ok := info.Uses[ident].(*types.Builtin); ok && bi.Name() == "new" && len(x.Args) == 1 {
					if tv, ok := info.Types[x.Args[0]]; ok {
						if named, ok := tv.Type.(*types.Named); ok {
							if toID, ok := objToDef[named.Obj()]; ok {
								addRef(toID, "constructor")
							}
						}
					}
				}
			}
			return true
		case *ast.Ident:
			// Fall through to existing ident handling below.
		default:
			return true
		}

		// Ident handling (original logic).
		ident := n.(*ast.Ident)
		obj, exists := info.Uses[ident]
		if !exists {
			return true
		}
		toID, exists := objToDef[obj]
		if exists {
			kind := classifyRef(obj)
			if kind == "type_ref" && embeddedIdents[ident] {
				kind = "embed"
			}
			addRef(toID, kind)
			return true
		}

		// Interface method dispatch: obj is an interface method not in objToDef.
		// Connect to all concrete implementations.
		if implIDs, ok := ifaceMethodToImpls[obj]; ok {
			for _, implID := range implIDs {
				addRef(implID, "interface_dispatch")
			}
		}
		return true
	})
	return refs, litFields
}

// innerIdent unwraps *ast.StarExpr and *ast.SelectorExpr to find the
// leaf *ast.Ident. Used to identify embedded field type idents.
func innerIdent(expr ast.Expr) *ast.Ident {
	for {
		switch x := expr.(type) {
		case *ast.Ident:
			return x
		case *ast.StarExpr:
			expr = x.X
		case *ast.SelectorExpr:
			return x.Sel
		case *ast.IndexExpr:
			// Generic instantiation: T[U]
			expr = x.X
		case *ast.IndexListExpr:
			// Generic instantiation: T[U, V]
			expr = x.X
		default:
			return nil
		}
	}
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
