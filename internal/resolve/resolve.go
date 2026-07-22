// Package resolve uses go/types information to build the references table,
// mapping which definitions reference which other definitions.
package resolve

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/store"
	"golang.org/x/tools/go/packages"
)

// Resolve analyzes all loaded packages and populates the references table.
// Includes test packages so test→definition references are captured.
func Resolve(db *store.DB, modulePath string) error {
	return resolve(db, nil, modulePath, "")
}

// ResolvePackages is like Resolve but accepts pre-loaded packages.
// Use with goload.LoadAll to share one packages.Load between ingest
// and resolve, saving ~1-2 GB of memory.
func ResolvePackages(db *store.DB, pkgs []*packages.Package, projectDir string) error {
	return resolve(db, pkgs, projectDir, "")
}

// ResolveModule is like Resolve but only updates references for definitions
// in the specified module. Still loads all packages for type information,
// but skips reference extraction for other modules. Much faster for
// single-definition edits.
func ResolveModule(db *store.DB, projectDir, modulePath string) error {
	return resolve(db, nil, projectDir, modulePath)
}

// ResolveFile loads only the package containing filePath (with its
// dependency types) and rebuilds references for definitions in that
// package. Much faster than a full Resolve (~50–500ms vs ~30s on medium
// projects) and intended for use after IngestFile to keep the ref graph
// fresh without paying the full-load cost.
//
// Cross-package refs FROM other packages TO this file's defs are not
// re-resolved here — those still flow from the prior full Resolve. If a
// caller renames or removes a def that other packages reference, a full
// Resolve is still needed to clean up the stale outgoing edges.
func ResolveFile(db *store.DB, projectDir, filePath string) error {
	cfg := &packages.Config{
		// NeedDeps intentionally omitted: it forces type-checking the
		// transitive closure per invocation (~19s on cli/cli's tree),
		// which was 97% of the wall clock in the #101 diagnosis. The
		// resolve pass only needs types.Object identities for target-
		// package defs + Pkg().Path()+Name() for cross-package uses;
		// those come from NeedImports (which loads immediate imports
		// with a shallow name-only view). Cross-package refs whose obj
		// happens to lack pkg-path info fall through to lookupDefID's
		// name-only search — same behavior as the pre-fix path when
		// the transitive graph had stale entries.
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports,
		Dir: projectDir,
		// Tests: false — #101 diagnosis. Tests:true forces go/packages to
		// load both the package AND its external test variant, which
		// nearly doubles the load cost (26s vs 1s on cli/cli). The
		// per-file incremental path is only expected to refresh refs
		// for defs in `filePath`'s own package; test→def refs from
		// _test.go files get re-resolved when the test file itself is
		// synced or during a full Resolve. If filePath IS a _test.go,
		// the containing test package still loads (packages.Load "file="
		// picks up the file's own package regardless of Tests).
		Tests: false,
	}
	tPL := time.Now()
	pkgs, err := packages.Load(cfg, "file="+filePath)
	if os.Getenv("DEFN_SYNC_TIMING") == "1" {
		fmt.Fprintf(os.Stderr, "  [inner] packages.Load: %s (%d pkgs)\n", time.Since(tPL).Round(time.Millisecond), len(pkgs))
	}
	if err != nil {
		return err
	}
	if len(pkgs) == 0 {
		return nil
	}
	// Pick the pkg path of the loaded package(s); after FilterPackages we
	// expect 1 (test variant) or 2 (file appears in both x and x_test).
	// resolve() uses the modulePath filter to scope rewrites; passing the
	// non-test package path covers both because ingest strips the _test
	// suffix from external test packages too.
	target := pkgs[0].PkgPath
	target = strings.TrimSuffix(target, "_test")
	return resolve(db, pkgs, projectDir, target)
}

func resolve(db *store.DB, preloaded []*packages.Package, projectDir, onlyModule string) error {
	var pkgs []*packages.Package
	if preloaded != nil {
		pkgs = preloaded
	} else {
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
		var err error
		pkgs, err = packages.Load(cfg, "./...")
		if err != nil {
			return err
		}
	}

	filtered := goload.FilterPackages(pkgs)

	// #107: preload per-pkgPath def indexes so the lookup* helpers do
	// map hits instead of hundreds of GetDefinitionByName round trips
	// across the passes below. Cache is per-resolve-call, not global.
	cache := make(pkgIndexCache)

	timing := os.Getenv("DEFN_SYNC_TIMING") == "1"
	timeIt := func(name string, t0 time.Time) {
		if timing {
			fmt.Fprintf(os.Stderr, "  [inner] %s: %s\n", name, time.Since(t0).Round(time.Millisecond))
		}
	}
	tPass := time.Now()

	// Build a map from types.Object → definition ID (all packages).
	objToDef := make(map[types.Object]int64)

	for _, pkg := range filtered {
		pkgPath := pkg.PkgPath
		if strings.HasSuffix(pkg.Name, "_test") {
			pkgPath = strings.TrimSuffix(pkgPath, "_test")
		}
		pkgScope := pkg.Types.Scope()
		for ident, obj := range pkg.TypesInfo.Defs {
			if obj == nil || ident.Name == "_" {
				continue
			}
			// #107: skip identifiers that can't map to a top-level def
			// in the DB — params, local vars, struct fields, etc. Without
			// this filter every local var in the file triggers a
			// GetDefinitionByName miss (7s on cli/cli's command package).
			// A method is scoped to its receiver, not the package, so we
			// keep those explicitly.
			if !isPackageLevelOrMethod(obj, pkgScope) {
				continue
			}
			defID := lookupDefID(db, pkgPath, ident, obj, cache)
			if defID > 0 {
				objToDef[obj] = defID
			}
		}
	}
	timeIt("pass1 objToDef", tPass)
	tPass = time.Now()

	// Accumulators: each fromID gets one final SetReferences call after all
	// passes have contributed. Avoids the REPLACE-style wipes we used to
	// hit when multiple call sites wrote refs for the same fromID
	// (var X SomeType = expr touching both the value and type expressions;
	// pass 2 implements vs pass 3 TypeSpec embed/type_ref; multiple
	// interfaces satisfied by one concrete type).
	defRefs := map[int64][]store.Reference{}
	defLitFields := map[int64][]store.LiteralField{}

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
				concreteID := lookupTypeDefID(db, pkgPath, concrete.Obj().Name(), cache)
				ifaceID := lookupTypeDefID(db, pkgPath, iface.Obj().Name(), cache)

				// Stage "implements" edge: concrete type → interface. Apply
				// at the end with all the other refs for concreteID so a
				// later TypeSpec pass cannot wipe it (and so multiple
				// interfaces don't overwrite each other within this loop).
				if concreteID > 0 && ifaceID > 0 {
					defRefs[concreteID] = append(defRefs[concreteID], store.Reference{ToDef: ifaceID, Kind: "implements"})
				}

				// Map interface method objects → concrete method def IDs.
				for ifaceMethod := range ifaceType.Methods() {
					ifaceMethod := ifaceMethod
					concreteMethodID := lookupMethodDefID(db, pkgPath, concrete.Obj().Name(), ifaceMethod.Name(), cache)
					if concreteMethodID > 0 {
						ifaceMethodToImpls[ifaceMethod] = append(ifaceMethodToImpls[ifaceMethod], concreteMethodID)
					}
				}
			}
		}
	}
	timeIt("pass2 iface-satisfaction", tPass)
	tPass = time.Now()

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
					fromID := lookupFuncDefID(db, pkgPath, d, cache)
					if fromID <= 0 {
						continue
					}
					refs, litFields := collectRefs(d.Body, pkg.TypesInfo, pkg.Fset, objToDef, ifaceMethodToImpls)
					if len(refs) > 0 {
						defRefs[fromID] = append(defRefs[fromID], refs...)
					}
					if len(litFields) > 0 {
						defLitFields[fromID] = append(defLitFields[fromID], litFields...)
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
								fromID := lookupVarDefID(db, pkgPath, name.Name, cache)
								if fromID <= 0 {
									continue
								}
								// Collect refs from the value expression
								// AND the type expression. Both contribute
								// to the same fromID; accumulate and flush
								// once at the end so the second iteration
								// doesn't wipe the first.
								var nodes []ast.Node
								if i < len(s.Values) {
									nodes = append(nodes, s.Values[i])
								}
								if s.Type != nil {
									nodes = append(nodes, s.Type)
								}
								for _, node := range nodes {
									refs, litFields := collectRefs(node, pkg.TypesInfo, pkg.Fset, objToDef, ifaceMethodToImpls)
									if len(refs) > 0 {
										defRefs[fromID] = append(defRefs[fromID], refs...)
									}
									if len(litFields) > 0 {
										defLitFields[fromID] = append(defLitFields[fromID], litFields...)
									}
								}
							}

						case *ast.TypeSpec:
							// Type definitions: struct fields, embedded types, interface methods.
							fromID := lookupTypeDefID(db, pkgPath, s.Name.Name, cache)
							if fromID <= 0 {
								continue
							}
							refs, litFields := collectRefs(s.Type, pkg.TypesInfo, pkg.Fset, objToDef, ifaceMethodToImpls)
							if len(refs) > 0 {
								defRefs[fromID] = append(defRefs[fromID], refs...)
							}
							if len(litFields) > 0 {
								defLitFields[fromID] = append(defLitFields[fromID], litFields...)
							}
						}
					}
				}
			}
		}
	}

	timeIt("pass3 body-refs", tPass)
	tPass = time.Now()

	// #108 (winze finding): wrap the entire flush in ONE transaction
	// instead of letting Dolt autocommit each SetReferences /
	// SetLiteralFields call. On a 1.2GB Dolt working set each autocommit
	// materializes noms chunks separately — winze measured ~1.5s PER
	// statement × 72 statements = 109s. One txn amortizes that cost.
	// Fall back to the unwrapped path if Begin fails (embedded Dolt not
	// ready, MySQL server rejects START TRANSACTION, etc.) — same shape
	// as before, just slower on ref-dense flushes.
	commit, rollback, txErr := db.Begin()
	txWrapped := txErr == nil
	// Flush accumulated refs once per def. SetReferences de-dupes internally.
	for fromID, refs := range defRefs {
		if err := db.SetReferences(fromID, refs); err != nil {
			if txWrapped {
				rollback()
			}
			return err
		}
	}
	for fromID, fields := range defLitFields {
		if err := db.SetLiteralFields(fromID, fields); err != nil {
			if txWrapped {
				rollback()
			}
			return err
		}
	}
	if txWrapped {
		if err := commit(); err != nil {
			return fmt.Errorf("commit flush txn: %w", err)
		}
	}
	timeIt("flush SetReferences", tPass)

	// Release Dolt's accumulated chunk cache. Mirrors IngestPackages's
	// end-GC: SetReferences/SetLiteralFields materialize noms chunks that
	// stick in the in-memory chunk cache until DOLT_GC runs. Without this,
	// a serve-mode resolve on a medium project adds ~335 MB heap_alloc
	// that doesn't release until the next autoCommit GC tick. Skipped on
	// partial-resolve paths (ResolveModule/ResolveFile) — those are the
	// sub-second fast paths used after a single-def edit, and DOLT_GC
	// costs seconds.
	if onlyModule == "" {
		if err := db.Commit("resolve-checkpoint"); err != nil {
			return fmt.Errorf("post-resolve commit: %w", err)
		}
		if err := db.GC(); err != nil {
			return fmt.Errorf("post-resolve gc: %w", err)
		}
		debug.FreeOSMemory()
	}

	return nil
}

// defIndex is a name/receiver → def ID index for one package, built
// once and consulted by lookup*DefID to spare hundreds of per-identifier
// DB round trips in the resolve inner loops (#107 followup to #101).
// pkgCache maps pkgPath → its defIndex; misses fall back to the DB.
type defIndex struct {
	byName   map[string]int64            // top-level defs (no receiver)
	byMethod map[string]map[string]int64 // methodName → receiver → def ID
}

func (i *defIndex) lookupName(name string) int64 {
	if i == nil {
		return 0
	}
	return i.byName[name]
}

func (i *defIndex) lookupMethod(name, receiver string) int64 {
	if i == nil {
		return 0
	}
	if m, ok := i.byMethod[name]; ok {
		if id, ok := m[receiver]; ok {
			return id
		}
	}
	return 0
}

func loadDefIndex(db *store.DB, pkgPath string) *defIndex {
	if pkgPath == "" {
		return nil
	}
	mod, err := db.GetModuleByPath(pkgPath)
	if err != nil || mod == nil {
		return nil
	}
	defs, err := db.GetModuleDefinitions(mod.ID)
	if err != nil {
		return nil
	}
	idx := &defIndex{
		byName:   make(map[string]int64, len(defs)),
		byMethod: make(map[string]map[string]int64),
	}
	for _, d := range defs {
		if d.Receiver != "" {
			m, ok := idx.byMethod[d.Name]
			if !ok {
				m = make(map[string]int64, 2)
				idx.byMethod[d.Name] = m
			}
			m[d.Receiver] = d.ID
		} else {
			idx.byName[d.Name] = d.ID
		}
	}
	return idx
}

// pkgIndexCache holds per-pkgPath preloaded defIndexes. Populated lazily
// on first lookup for each pkgPath. Not concurrency-safe (resolve is
// single-goroutine).
type pkgIndexCache map[string]*defIndex

func (c pkgIndexCache) get(db *store.DB, pkgPath string) *defIndex {
	if idx, ok := c[pkgPath]; ok {
		return idx
	}
	idx := loadDefIndex(db, pkgPath)
	c[pkgPath] = idx
	return idx
}

func lookupTypeDefID(db *store.DB, pkgPath, typeName string, cache pkgIndexCache) int64 {
	if id := cache.get(db, pkgPath).lookupName(typeName); id > 0 {
		return id
	}
	d, err := db.GetDefinitionByName(typeName, pkgPath)
	if err != nil {
		return 0
	}
	return d.ID
}

func lookupMethodDefID(db *store.DB, pkgPath, typeName, methodName string, cache pkgIndexCache) int64 {
	idx := cache.get(db, pkgPath)
	// Try *Type first (most methods have pointer receivers).
	if id := idx.lookupMethod(methodName, "*"+typeName); id > 0 {
		return id
	}
	if id := idx.lookupMethod(methodName, typeName); id > 0 {
		return id
	}
	d, err := db.GetDefinitionByNameAndReceiver(methodName, pkgPath, "*"+typeName)
	if err == nil {
		return d.ID
	}
	d, err = db.GetDefinitionByNameAndReceiver(methodName, pkgPath, typeName)
	if err == nil {
		return d.ID
	}
	return 0
}

// lookupVarDefID finds the definition ID for a package-level var or const.
func lookupVarDefID(db *store.DB, pkgPath, name string, cache pkgIndexCache) int64 {
	if id := cache.get(db, pkgPath).lookupName(name); id > 0 {
		return id
	}
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

// isPackageLevelOrMethod reports whether obj can plausibly correspond to
// a def in the DB — package-scoped identifiers (top-level funcs, vars,
// consts, types) or methods (which are scoped to their receiver, not the
// package scope). Filters out params, local vars, struct fields, and
// interface method identifiers that would only ever miss the DB. Called
// on every TypesInfo.Defs entry, so keep the check cheap.
func isPackageLevelOrMethod(obj types.Object, pkgScope *types.Scope) bool {
	if fn, ok := obj.(*types.Func); ok {
		sig := fn.Signature()
		if sig != nil && sig.Recv() != nil {
			return true
		}
	}
	return obj.Parent() == pkgScope
}

func lookupDefID(db *store.DB, pkgPath string, ident *ast.Ident, obj types.Object, cache pkgIndexCache) int64 {
	// For methods, use receiver-qualified lookup to avoid ambiguity.
	if fn, ok := obj.(*types.Func); ok {
		sig := fn.Signature()
		if sig != nil && sig.Recv() != nil {
			recv := receiverName(sig.Recv().Type())
			if id := cache.get(db, pkgPath).lookupMethod(ident.Name, recv); id > 0 {
				return id
			}
			d, err := db.GetDefinitionByNameAndReceiver(ident.Name, pkgPath, recv)
			if err == nil {
				return d.ID
			}
		}
	}
	if id := cache.get(db, pkgPath).lookupName(ident.Name); id > 0 {
		return id
	}
	d, err := db.GetDefinitionByName(ident.Name, pkgPath)
	if err != nil {
		return 0
	}
	return d.ID
}

func lookupFuncDefID(db *store.DB, pkgPath string, fn *ast.FuncDecl, cache pkgIndexCache) int64 {
	// For methods, include receiver in lookup.
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		recv := types.ExprString(fn.Recv.List[0].Type)
		if id := cache.get(db, pkgPath).lookupMethod(fn.Name.Name, recv); id > 0 {
			return id
		}
		d, err := db.GetDefinitionByNameAndReceiver(fn.Name.Name, pkgPath, recv)
		if err == nil {
			return d.ID
		}
	}
	if id := cache.get(db, pkgPath).lookupName(fn.Name.Name); id > 0 {
		return id
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
