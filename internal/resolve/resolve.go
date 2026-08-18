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
	"strconv"
	"strings"
	"time"

	"github.com/justinstimatze/defn/internal/astutil"
	"github.com/justinstimatze/defn/internal/goload"
	"github.com/justinstimatze/defn/internal/store"
	"golang.org/x/tools/go/packages"
)

// Resolve analyzes all loaded packages and populates the references table.
// Includes test packages so test→definition references are captured.
func Resolve(db store.Backend, modulePath string) error {
	return resolve(db, nil, modulePath, "")
}

// ResolvePackages is like Resolve but accepts pre-loaded packages.
// Use with goload.LoadAll to share one packages.Load between ingest
// and resolve, saving ~1-2 GB of memory.
func ResolvePackages(db store.Backend, pkgs []*packages.Package, projectDir string) error {
	return resolve(db, pkgs, projectDir, "")
}

// ResolveModule is like Resolve but only updates references for definitions
// in the specified module. Still loads all packages for type information,
// but skips reference extraction for other modules. Much faster for
// single-definition edits.
func ResolveModule(db store.Backend, projectDir, modulePath string) error {
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
func ResolveFile(db store.Backend, projectDir, filePath string) error {
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
		// nearly doubles the load cost (26s vs 1s on cli/cli). That cost
		// is only worth avoiding when filePath is a production file --
		// its own refs don't depend on the test variant loading at all.
		//
		// When filePath IS a _test.go file, the opposite used to be true
		// silently: with Tests:false, go/packages excludes _test.go
		// syntax from the package it returns, so the very file this call
		// exists to re-resolve never got its own outgoing refs rebuilt.
		// Confirmed via TestResolveFileDoesNotRefreshCallRefsFromTestFile
		// -- a replace-hunk-edited test function's call ref stayed
		// pointed at the old target through both the post-edit
		// autoResolveFile call AND an explicit code(op:"sync",
		// file:<test file>), since handleSync's single-file path also
		// calls ResolveFile. Only a full, unscoped resolve ever picked
		// it up. Loading the test variant here is the same one-package
		// scoped cost either way -- Tests:true when filePath itself is
		// the thing whose refs need refreshing.
		Tests: strings.HasSuffix(filePath, "_test.go"),
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

func resolve(db store.Backend, preloaded []*packages.Package, projectDir, onlyModule string) error {
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

		// Struct fields never satisfy isPackageLevelOrMethod below --
		// obj.Parent() is nil for a field, same as a param or local var,
		// so the object alone can't distinguish them. A companion AST
		// walk maps each field's declaring *ast.Ident to its struct
		// type's name, mirroring how ingestStructFields assigns
		// Receiver: typeName when it stores the field's own DB row --
		// letting the receiver-qualified lookup below find that same
		// row. Without this, a field's *types.Var object never enters
		// objToDef, so collectRefs can never resolve a selector
		// expression (ro.Count) or keyed composite literal (T{Count:...})
		// back to the field's def ID -- GetCallers on a renamed struct
		// field always reports 0 callers, forcing callers to be found
		// and fixed by hand even though go/types resolves both those use
		// forms to the same object identity the field's own Defs entry
		// has (confirmed empirically: Info.Uses IS populated for both).
		fieldOwners := map[types.Object]string{}
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					return true
				}
				for _, field := range st.Fields.List {
					for _, name := range field.Names {
						if obj := pkg.TypesInfo.Defs[name]; obj != nil {
							fieldOwners[obj] = ts.Name.Name
						}
					}
				}
				return true
			})
		}

		for ident, obj := range pkg.TypesInfo.Defs {
			if obj == nil || ident.Name == "_" {
				continue
			}
			// #107: skip identifiers that can't map to a top-level def
			// in the DB — params, local vars, etc. Without this filter
			// every local var in the file triggers a GetDefinitionByName
			// miss (7s on cli/cli's command package). A method is scoped
			// to its receiver, not the package, so we keep those
			// explicitly; struct fields are handled below via
			// fieldOwners since they need the same receiver-qualified
			// lookup but aren't identifiable from the object alone.
			if isPackageLevelOrMethod(obj, pkgScope) {
				defID := lookupDefID(db, pkgPath, ident, obj, cache)
				if defID > 0 {
					objToDef[obj] = defID
				}
				continue
			}
			if owner, ok := fieldOwners[obj]; ok {
				if id := lookupFieldDefID(db, pkgPath, ident.Name, owner, cache); id > 0 {
					objToDef[obj] = id
				}
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
	//
	// Interfaces are routinely declared in a DIFFERENT package than
	// their implementers -- define-where-consumed, implement-elsewhere is
	// the standard Go idiom (io.Writer, sort.Interface, and real-world
	// cases like grpc-go's balancer.Picker interface satisfied by
	// grpclb.lbPicker in a different package). The original pass only
	// paired concrete types and interfaces declared in the SAME package's
	// own scope, so every cross-package case was silently missed:
	// ifaceMethodToImpls never got an entry for Picker.Pick ->
	// (*lbPicker).Pick, so calls dispatched through the interface got zero
	// caller edges, and op:"test"/GetImpact reported "no tests cover this"
	// for a method real tests exercised at runtime through the interface
	// -- confirmed via a real bench trajectory where an agent shipped a
	// deadlocking regression because of exactly this false negative.
	//
	// Fix: collect interfaces per package once, then for each package's
	// concrete types check against its OWN interfaces plus every DIRECTLY
	// IMPORTED package's interfaces -- not a full N×M cross product over
	// every package pair, and not transitive imports. Bounded by the
	// package's own import list, which covers the overwhelmingly common
	// case: a type only satisfies an interface on purpose if it imports
	// that interface's package to see its method signatures in the first
	// place. Purely structural satisfaction with no import at all is
	// possible in Go but rare enough to accept as a documented gap rather
	// than pay for a global cross product.
	ifacesByPkg := map[string][]*types.Named{}
	for _, pkg := range filtered {
		pkgPath := pkg.PkgPath
		if strings.HasSuffix(pkg.Name, "_test") {
			pkgPath = strings.TrimSuffix(pkgPath, "_test")
		}
		scope := pkg.Types.Scope()
		if scope == nil {
			continue
		}
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
				ifacesByPkg[pkgPath] = append(ifacesByPkg[pkgPath], named)
			}
		}
	}

	// Widen ifacesByPkg to also cover EXTERNAL (stdlib/third-party)
	// packages reachable via any filtered package's direct imports.
	// go/packages' NeedDeps mode already loads full go/types info for
	// these (that's required for the importing package to type-check at
	// all), so no extra package load is needed -- pkg.Imports[path].Types
	// is already populated. This is what turns the candidateIfaces lookup
	// below from "project interfaces only" into "project + every
	// interface a project package can see", closing the gap
	// methodRenameRisksInterfaceBreak used to paper over with a small
	// hardcoded name list (io.Reader/Writer/Closer, fmt.Stringer, ...) --
	// confirmed live: a type satisfying io.ReaderAt (method ReadAt, not on
	// that list) via `func use() io.ReaderAt { return T{} }` with no local
	// interface anywhere let a rename of T.ReadAt ship a build that no
	// longer compiled, reported as a clean success.
	scannedIfacePkgs := map[string]bool{}
	for _, pkg := range filtered {
		for importPath, impPkg := range pkg.Imports {
			if _, already := ifacesByPkg[importPath]; already {
				continue
			}
			if scannedIfacePkgs[importPath] {
				continue
			}
			scannedIfacePkgs[importPath] = true
			if impPkg == nil || impPkg.Types == nil {
				continue
			}
			scope := impPkg.Types.Scope()
			if scope == nil {
				continue
			}
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
					ifacesByPkg[importPath] = append(ifacesByPkg[importPath], named)
				}
			}
		}
	}

	// defExternalIfaces accumulates, per concrete method def ID, the
	// external interfaces (no local defn ID, so no "implements" ref is
	// possible) it satisfies -- flushed via SetManyExternalInterfaces
	// below and read back by methodRenameRisksInterfaceBreak.
	defExternalIfaces := map[int64][]string{}

	ifaceMethodToImpls := map[string][]int64{}

	// #253: this loop must NOT skip packages outside onlyModule the way
	// pass 3 does. ifaceMethodToImpls is rebuilt from scratch on every
	// resolve() call (never cached across calls) and is the ONLY thing
	// collectRefs has to resolve an interface dispatch call site anywhere
	// in the scoped module -- if the implementer lives in a DIFFERENT
	// module than onlyModule (the overwhelmingly common shape: an
	// interface's sole implementer is typically declared once, in its own
	// package, while callers scattered across many other packages/modules
	// invoke it through the interface), skipping that implementer's
	// package here means ifaceMethodToImpls never gets an entry for it in
	// THIS call. Pass 3 then finds nothing for the caller's dispatch call
	// site, and SetManyReferences -- a full delete+reinsert per fromID --
	// silently WIPES a previously-correct interface_dispatch ref a prior
	// full Resolve had computed, the instant that caller's OWN module is
	// what triggers a scoped resolve (i.e. on every edit to the calling
	// code, which is normal and frequent). Confirmed via a real
	// dogfooding session where op:"impact"/op:"traverse" on defn's own
	// store.Backend methods reported near-zero callers despite dozens of
	// real cross-package call sites in internal/mcp, because internal/mcp
	// had been through many incremental per-file/per-module resolves
	// since the codebase's last full one.
	//
	// The "implements" edge staged into defRefs below stays scoped to
	// onlyModule, though (see the inline check) -- that write only
	// belongs to defs actually being re-resolved this call; the ONLY
	// thing that needs unscoped visibility is the dispatch map itself.
	for _, pkg := range filtered {
		pkgPath := pkg.PkgPath
		if strings.HasSuffix(pkg.Name, "_test") {
			pkgPath = strings.TrimSuffix(pkgPath, "_test")
		}

		scope := pkg.Types.Scope()
		if scope == nil {
			continue
		}

		// Collect all named (non-interface) types in this package.
		var namedTypes []*types.Named
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
			if !types.IsInterface(named) {
				namedTypes = append(namedTypes, named)
			}
		}
		if len(namedTypes) == 0 {
			continue
		}

		// Candidate interfaces: this package's own, plus every directly
		// imported package's (map keys in pkg.Imports already dedup by
		// path) -- including external/stdlib packages now that ifacesByPkg
		// is widened above. An external interface never resolves to a defn
		// ID (lookupTypeDefID below), so it can never gain an "implements"
		// ref; it's staged into defExternalIfaces instead.
		candidateIfaces := append([]*types.Named{}, ifacesByPkg[pkgPath]...)
		for importPath := range pkg.Imports {
			candidateIfaces = append(candidateIfaces, ifacesByPkg[importPath]...)
		}
		if len(candidateIfaces) == 0 {
			continue
		}

		// Check each (concrete, interface) pair.
		for _, concrete := range namedTypes {
			for _, iface := range candidateIfaces {
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

				// The interface may live in a different package than the
				// concrete type now -- look its def ID up under its OWN
				// package path, not the concrete type's.
				ifacePkgPath := pkgPath
				if p := iface.Obj().Pkg(); p != nil {
					ifacePkgPath = p.Path()
				}

				// Find defn IDs for the concrete type and interface.
				concreteID := lookupTypeDefID(db, pkgPath, concrete.Obj().Name(), cache)
				ifaceID := lookupTypeDefID(db, ifacePkgPath, iface.Obj().Name(), cache)

				// Stage "implements" edge: concrete type → interface. Apply
				// at the end with all the other refs for concreteID so a
				// later TypeSpec pass cannot wipe it (and so multiple
				// interfaces don't overwrite each other within this loop).
				// Unlike ifaceMethodToImpls above, this write DOES stay
				// scoped to onlyModule -- it only belongs to defs actually
				// being re-resolved this call, not every package this now
				// -unscoped loop happens to visit while building the
				// dispatch map.
				if concreteID > 0 && ifaceID > 0 && (onlyModule == "" || pkgPath == onlyModule) {
					defRefs[concreteID] = append(defRefs[concreteID], store.Reference{ToDef: ifaceID, Kind: "implements"})
				}

				// Map interface method identity (as a canonical string, not
				// a types.Object -- see collectRefs's doc comment on the
				// lookup side for why) → concrete method def IDs.
				for ifaceMethod := range ifaceType.Methods() {
					concreteMethodID := lookupMethodDefID(db, pkgPath, concrete.Obj().Name(), ifaceMethod.Name(), cache)
					if concreteMethodID > 0 {
						key := ifaceMethodKey(ifacePkgPath, iface.Obj().Name(), ifaceMethod.Name())
						ifaceMethodToImpls[key] = append(ifaceMethodToImpls[key], concreteMethodID)
						// ifaceID == 0 means this interface has no defn row --
						// it's external (stdlib/third-party), never ingested.
						// Record the satisfaction directly on the concrete
						// method's own def ID instead, scoped the same way the
						// "implements" edge above is: only for defs actually
						// being re-resolved this call.
						if ifaceID == 0 && (onlyModule == "" || pkgPath == onlyModule) {
							qualified := iface.Obj().Name()
							if ifacePkgPath != "" {
								qualified = ifacePkgPath + "." + qualified
							}
							defExternalIfaces[concreteMethodID] = append(defExternalIfaces[concreteMethodID], qualified)
						}
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
					fromID := lookupFuncDefID(db, pkgPath, d, cache)
					if fromID <= 0 {
						continue
					}
					// Collect refs from the signature (parameter/return
					// types) AND the body. Both contribute to the same
					// fromID; accumulate and flush once so the second
					// collectRefs call doesn't wipe the first -- same
					// pattern ValueSpec already uses for its value+type.
					// Without this, a type used ONLY in a parameter or
					// return type (never instantiated inside the body)
					// was entirely invisible to GetCallers/impact --
					// confirmed live via the mutation fuzzer: renaming an
					// interface used only as `func F(g Greeter) ...`'s
					// parameter type reported "Updated 0 callers" and
					// left the now-undefined old name behind in F's
					// signature, silently shipping a broken build.
					var nodes []ast.Node
					if d.Type != nil {
						nodes = append(nodes, d.Type)
					}
					if d.Body != nil {
						nodes = append(nodes, d.Body)
					}
					for _, node := range nodes {
						refs, litFields := collectRefs(node, pkg.TypesInfo, pkg.Fset, objToDef, ifaceMethodToImpls, db, cache)
						if len(refs) > 0 {
							defRefs[fromID] = append(defRefs[fromID], refs...)
						}
						if len(litFields) > 0 {
							defLitFields[fromID] = append(defLitFields[fromID], litFields...)
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
									refs, litFields := collectRefs(node, pkg.TypesInfo, pkg.Fset, objToDef, ifaceMethodToImpls, db, cache)
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
							refs, litFields := collectRefs(s.Type, pkg.TypesInfo, pkg.Fset, objToDef, ifaceMethodToImpls, db, cache)
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

	// #253 part 2: ResolveFile's narrow load (preloaded != nil &&
	// onlyModule != "") cannot see an interface's implementer at all if
	// it lives in a package outside the single one loaded -- so
	// ifaceMethodToImpls can never gain an entry for it here even with
	// the pass-2 fix above, collectRefs finds nothing for that dispatch
	// call site, and the flush below would silently drop a
	// previously-correct interface_dispatch ref the same way the pass-2
	// bug did, just via a different mechanism (missing data, not a
	// scoping filter). Preserve existing interface_dispatch edges for
	// every def this call is about to flush new refs for by merging them
	// in -- mirrors the tradeoff ResolveFile's own doc comment already
	// accepts for incoming cross-package refs ("those still flow from
	// the prior full Resolve"), extended to outgoing dispatch edges.
	// Best-effort and deduped against whatever collectRefs DID find (the
	// implementer can legitimately be in the one loaded package too).
	if preloaded != nil && onlyModule != "" {
		for fromID, refs := range defRefs {
			have := map[int64]bool{}
			for _, r := range refs {
				if r.Kind == "interface_dispatch" {
					have[r.ToDef] = true
				}
			}
			existing, err := db.Traverse(fromID, "callees", []string{"interface_dispatch"}, 1)
			if err != nil {
				continue
			}
			for _, r := range existing {
				if have[r.Definition.ID] {
					continue
				}
				have[r.Definition.ID] = true
				defRefs[fromID] = append(defRefs[fromID], store.Reference{ToDef: r.Definition.ID, Kind: "interface_dispatch"})
			}
		}
	}

	// #108 (winze finding): wrap the entire flush in ONE transaction
	// instead of letting Dolt autocommit each write call. On a 1.2GB
	// Dolt working set each autocommit materializes noms chunks
	// separately — winze measured ~1.5s PER statement × 72 statements
	// = 109s. One txn amortizes that cost. #111 collapses the per-def
	// loop into set-based one-DELETE + batched-INSERT for each of refs
	// and litfields, cutting ~75 statements to ~5 total for a typical
	// 15-def flush.
	//
	// Fall back to the unwrapped path if Begin fails (embedded Dolt not
	// ready, MySQL server rejects START TRANSACTION, etc.) — same shape
	// as before, just slower on ref-dense flushes.
	// #214: route the actual writes through the tx-scoped Backend Begin()
	// hands back, not the original db -- writes issued against db would
	// auto-commit immediately via the pool, bypassing this transaction
	// entirely regardless of whether commit()/rollback() is later called.
	txDB, commit, rollback, txErr := db.Begin()
	txWrapped := txErr == nil
	flushDB := db
	if txWrapped {
		flushDB = txDB
	}
	// Split the two flushes into separate timers — winze dispatch 2026-07-22
	// msg-5c1eb8d6 saw 10-14s "flush SetReferences" on a 6-def sync and we
	// need to know which of refs/litfields dominates on their shape before
	// we can fix it. Also emit txn-commit time separately since Dolt's
	// commit cost scales with dirty-chunk count.
	tRefs := time.Now()
	if err := flushDB.SetManyReferences(defRefs); err != nil {
		if txWrapped {
			rollback()
		}
		return err
	}
	timeIt("flush SetManyReferences", tRefs)
	tLit := time.Now()
	if err := flushDB.SetManyLiteralFields(defLitFields); err != nil {
		if txWrapped {
			rollback()
		}
		return err
	}
	timeIt("flush SetManyLiteralFields", tLit)
	tExtIfaces := time.Now()
	if err := flushDB.SetManyExternalInterfaces(defExternalIfaces); err != nil {
		if txWrapped {
			rollback()
		}
		return err
	}
	timeIt("flush SetManyExternalInterfaces", tExtIfaces)
	if txWrapped {
		tCommit := time.Now()
		if err := commit(); err != nil {
			return fmt.Errorf("commit flush txn: %w", err)
		}
		timeIt("flush txn commit", tCommit)
	}
	timeIt("flush total", tPass)

	// Release Dolt's accumulated chunk cache. Mirrors IngestPackages's
	// end-GC: SetReferences/SetLiteralFields materialize noms chunks that
	// stick in the in-memory chunk cache until DOLT_GC runs. Without this,
	// a serve-mode resolve on a medium project adds ~335 MB heap_alloc
	// that doesn't release until the next autoCommit GC tick. Skipped on
	// partial-resolve paths (ResolveModule/ResolveFile) — those are the
	// sub-second fast paths used after a single-def edit, and DOLT_GC
	// costs seconds.
	if onlyModule == "" {
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

func loadDefIndex(db store.Backend, pkgPath string) *defIndex {
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

func (c pkgIndexCache) get(db store.Backend, pkgPath string) *defIndex {
	if idx, ok := c[pkgPath]; ok {
		return idx
	}
	idx := loadDefIndex(db, pkgPath)
	c[pkgPath] = idx
	return idx
}

func lookupTypeDefID(db store.Backend, pkgPath, typeName string, cache pkgIndexCache) int64 {
	if id := cache.get(db, pkgPath).lookupName(typeName); id > 0 {
		return id
	}
	d, err := db.GetDefinitionByName(typeName, pkgPath)
	if err != nil {
		return 0
	}
	return d.ID
}

func lookupMethodDefID(db store.Backend, pkgPath, typeName, methodName string, cache pkgIndexCache) int64 {
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
func lookupVarDefID(db store.Backend, pkgPath, name string, cache pkgIndexCache) int64 {
	if id := cache.get(db, pkgPath).lookupName(name); id > 0 {
		return id
	}
	d, err := db.GetDefinitionByName(name, pkgPath)
	if err != nil {
		return 0
	}
	return d.ID
}

// collectRefs walks node's AST and returns every reference it makes to a
// definition in objToDef/ifaceMethodToImpls, plus struct-literal field data.
//
// db/cache: fallback for when the "to" side of a reference belongs to a
// package that wasn't part of THIS resolve call's own package set (e.g.
// ResolveFile loads only the touched file's own package, so objToDef only
// has entries for defs declared in that one package -- a call to a func in
// any other package always misses objToDef here). Without this fallback,
// ResolveFile doesn't just fail to ADD cross-package outgoing refs, it
// actively ERASES previously-correct ones: resolve()'s caller flushes via
// SetManyReferences, which deletes-then-reinserts the touched def's whole
// ref set, so a miss here is indistinguishable from "this def no longer
// calls that". db may be nil (unit tests exercising collectRefs directly
// without a backend) -- the fallback is skipped in that case, same as a
// permanent miss.
//
// Guarded by isPackageLevelOrMethod (the same filter pass1 uses to decide
// what goes into objToDef in the first place) so this can't bind a local
// var/param to an unrelated same-named top-level def elsewhere in the DB --
// it only fires for objects that are themselves plausibly a top-level def
// or a concrete (non-interface) method, using each object's OWN home
// package scope rather than the caller's.
func collectRefs(node ast.Node, info *types.Info, fset *token.FileSet, objToDef map[types.Object]int64, ifaceMethodToImpls map[string][]int64, db store.Backend, cache pkgIndexCache) ([]store.Reference, []store.LiteralField) {
	seen := make(map[int64]string)
	var refs []store.Reference
	var litFields []store.LiteralField

	addRef := func(toID int64, kind string) {
		if _, dup := seen[toID]; !dup {
			seen[toID] = kind
			refs = append(refs, store.Reference{ToDef: toID, Kind: kind})
		}
	}

	// crossPkgTypeFallback resolves a *types.TypeName that missed objToDef
	// via the DB-backed cache, scoped to the type's own package. Shared by
	// the CompositeLit and new(Type) constructor cases below.
	crossPkgTypeFallback := func(tn *types.TypeName) int64 {
		pkg := tn.Pkg()
		if pkg == nil || db == nil || tn.Parent() != pkg.Scope() {
			return 0
		}
		return lookupTypeDefID(db, pkg.Path(), tn.Name(), cache)
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
						} else if toID := crossPkgTypeFallback(named.Obj()); toID > 0 {
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
							// Resolve the keyed field itself as a
							// reference via the DB, not objToDef/object
							// identity: FilterPackages prefers the test
							// variant of any package with _test.go
							// files, so a field declared there has a
							// different types.Object identity than what
							// an ordinary (non-test) importer's own
							// TypesInfo resolves the same field key to
							// -- see lookupFieldDefID's doc comment.
							if fieldPkg := named.Obj().Pkg(); fieldPkg != nil {
								if toID := lookupFieldDefID(db, fieldPkg.Path(), ident.Name, named.Obj().Name(), cache); toID > 0 {
									addRef(toID, "field_ref")
								}
							}
							fieldValue, ok := evalStringLiteral(kv.Value)
							if !ok {
								var buf bytes.Buffer
								if err := format.Node(&buf, fset, kv.Value); err != nil {
									continue
								}
								fieldValue = buf.String()
							}
							litFields = append(litFields, store.LiteralField{
								TypeName:   typeName,
								FieldName:  ident.Name,
								FieldValue: fieldValue,
								Line:       fset.Position(kv.Pos()).Line,
							})
						}
					}
				}
			}
			return true
		case *ast.SelectorExpr:
			// Direct field access: x.Field. Resolved via the DB, not
			// objToDef/object identity -- same rationale as the keyed
			// composite-literal case above (see lookupFieldDefID's doc
			// comment on the test-variant identity mismatch). Only
			// len(Index())==1 (a field declared directly on x's own
			// static type, not promoted through an embedded field) --
			// sel.Recv() is x's OWN type, which is the wrong owner to
			// look up a promoted field's def under; skipping those
			// is a silent miss, same as today's behavior, not a
			// regression.
			if sel, ok := info.Selections[x]; ok && sel.Kind() == types.FieldVal && len(sel.Index()) == 1 {
				if v, ok := sel.Obj().(*types.Var); ok && v.IsField() {
					recvType := sel.Recv()
					if ptr, ok := recvType.(*types.Pointer); ok {
						recvType = ptr.Elem()
					}
					if named, ok := recvType.(*types.Named); ok {
						if fieldPkg := named.Obj().Pkg(); fieldPkg != nil {
							if toID := lookupFieldDefID(db, fieldPkg.Path(), v.Name(), named.Obj().Name(), cache); toID > 0 {
								addRef(toID, "field_ref")
							}
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
							} else if toID := crossPkgTypeFallback(named.Obj()); toID > 0 {
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

		// Interface method dispatch: obj is an interface method not in
		// objToDef. Connect to all concrete implementations.
		//
		// Keyed by a canonical STRING (package path + interface name +
		// method name), NOT by obj itself. Confirmed via direct diagnostic
		// against defn's own repo: packages.Load(Tests:true) gives
		// internal/store.Backend.GetImpact's method object TWO DIFFERENT
		// *types.Object pointers with IDENTICAL String() representation --
		// one from internal/store's own type-checking session (the
		// "test variant" FilterPackages prefers when iterating that
		// package directly in pass 2, since it bundles _test.go files),
		// and a different one from internal/mcp's session (which imports
		// the PLAIN, non-test variant, per normal Go import rules -- a
		// non-test package can never import another package's test
		// variant). A types.Object-keyed map built from one session's
		// pointers can never match a lookup using the other session's --
		// silently breaking cross-package interface dispatch for every
		// call into any package that has its own _test.go files, which is
		// nearly every real package. String identity is immune to this:
		// pkgPath/name strings are equal regardless of which
		// type-checking session produced the Object.
		//
		// obj's own receiver type (not sel.X) reliably gives a
		// *types.Named for the interface, confirmed empirically -- no
		// need to inspect the enclosing selector expression separately.
		if fn, ok := obj.(*types.Func); ok {
			if sig := fn.Type().(*types.Signature); sig.Recv() != nil {
				if named, ok := sig.Recv().Type().(*types.Named); ok && types.IsInterface(named) {
					if ifacePkg := named.Obj().Pkg(); ifacePkg != nil {
						key := ifaceMethodKey(ifacePkg.Path(), named.Obj().Name(), fn.Name())
						if implIDs, ok := ifaceMethodToImpls[key]; ok {
							for _, implID := range implIDs {
								addRef(implID, "interface_dispatch")
							}
							return true
						}
					}
				}
			}
		}

		// Cross-package fallback -- see the doc comment above.
		if pkg := obj.Pkg(); pkg != nil && db != nil && isPackageLevelOrMethod(obj, pkg.Scope()) {
			if toID := lookupDefID(db, pkg.Path(), ident, obj, cache); toID > 0 {
				kind := classifyRef(obj)
				if kind == "type_ref" && embeddedIdents[ident] {
					kind = "embed"
				}
				addRef(toID, kind)
			}
		}
		return true
	})
	return refs, litFields
}

// evalStringLiteral collapses an *ast.BasicLit STRING or an *ast.BinaryExpr
// chain of +-concatenated string literals into the evaluated prose. Returns
// (value, true) when every leaf is a compile-time string literal; otherwise
// (_, false) so the caller falls back to source-form rendering. Fixes a
// data-correctness bug where multi-line +-concatenated Quote-style fields
// were stored as Go source (e.g. `"first " + "second"`) instead of prose.
func evalStringLiteral(expr ast.Expr) (string, bool) {
	switch x := expr.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		lhs, ok := evalStringLiteral(x.X)
		if !ok {
			return "", false
		}
		rhs, ok := evalStringLiteral(x.Y)
		if !ok {
			return "", false
		}
		return lhs + rhs, true
	case *ast.ParenExpr:
		return evalStringLiteral(x.X)
	}
	return "", false
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
			// Interface methods also report a non-nil Recv() (the
			// interface type itself, per go/types) -- distinct from a
			// concrete method's receiver. Without this check they slip
			// past this filter, then lookupDefID's fallback chain (a
			// bare GetDefinitionByName with its blast-radius tiebreak)
			// silently binds the interface method's object to whatever
			// unrelated same-named def happens to exist elsewhere in the
			// whole DB, polluting objToDef and making every call site
			// dispatched through that interface resolve to the wrong
			// definition -- with the real implementer(s), correctly
			// found via ifaceMethodToImpls, never getting a chance
			// because collectRefs checks objToDef first.
			return !types.IsInterface(sig.Recv().Type())
		}
	}
	return obj.Parent() == pkgScope
}

func lookupDefID(db store.Backend, pkgPath string, ident *ast.Ident, obj types.Object, cache pkgIndexCache) int64 {
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

func lookupFuncDefID(db store.Backend, pkgPath string, fn *ast.FuncDecl, cache pkgIndexCache) int64 {
	// For methods, include receiver in lookup.
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		recv := astutil.BareReceiverName(fn.Recv.List[0].Type)
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
	// A method receiver is always a named type or a pointer to one --
	// unwrap structurally via types.Named.Obj().Name() instead of
	// string-splitting t.String() on ".". The old string-split approach
	// never stripped a generic instantiation's bracket suffix ("*Context"
	// vs "*Pair[K,V]"), and for a package-qualified type ARGUMENT inside
	// the brackets (e.g. Pair[K, sql.NullString]) it could find the wrong
	// "." entirely -- both silently produced a receiver key that never
	// matched the bare name Definition.Receiver is stored under,
	// confirmed live: two generic types sharing a method name in one
	// package (Stack[T].Len / Queue[T].Len) had their callers merged
	// onto a single def via the receiver-agnostic fallback lookup.
	prefix := ""
	if ptr, ok := t.(*types.Pointer); ok {
		prefix = "*"
		t = ptr.Elem()
	}
	if named, ok := t.(*types.Named); ok {
		return prefix + named.Obj().Name()
	}
	// Fallback for anything structurally unexpected for a receiver.
	s := t.String()
	if idx := strings.LastIndex(s, "."); idx >= 0 {
		return prefix + s[idx+1:]
	}
	return prefix + s
}

// ifaceMethodKey builds the canonical string key ifaceMethodToImpls is
// keyed by: package path + interface name + method name. NOT keyed by
// types.Object pointer identity -- see the doc comment where this is
// populated (resolve's pass 2) for why pointer identity is unsafe here.
func ifaceMethodKey(pkgPath, ifaceName, methodName string) string {
	return pkgPath + "\x00" + ifaceName + "\x00" + methodName
}

// lookupFieldDefID resolves a struct field's def ID by (package, field
// name, declaring type name) -- the same receiver-qualified shape methods
// use, since ingestStructFields stores each field's Receiver as its
// declaring type's bare name. Deliberately DB-backed rather than
// object-identity-backed (unlike the objToDef fast path): FilterPackages
// prefers the test variant of any package with _test.go files, so a
// field declared in such a package has a different types.Object identity
// than what an ordinary (non-test) importer's own TypesInfo resolves the
// same field access to -- object identity alone silently misses every
// cross-package reference to a field in a tested package.
func lookupFieldDefID(db store.Backend, pkgPath, fieldName, owner string, cache pkgIndexCache) int64 {
	if id := cache.get(db, pkgPath).lookupMethod(fieldName, owner); id > 0 {
		return id
	}
	d, err := db.GetDefinitionByNameAndReceiver(fieldName, pkgPath, owner)
	if err != nil {
		return 0
	}
	return d.ID
}
