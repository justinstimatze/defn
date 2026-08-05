package mcp

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// bodyImportFootprintUnchanged reports whether newBody references exactly
// the same set of externally-qualified names (package-selector heads) as
// oldBody, for a single sig-stable func-decl body edit. When true, this
// def's marginal contribution to the containing file's import-need
// equation is provably zero-delta: whatever import list already satisfied
// the file before this edit still satisfies it after, so goimports has
// nothing to fix and can be skipped entirely.
//
// Deliberately conservative: any decl kind other than *ast.FuncDecl, or
// any parse failure, returns false (run goimports) rather than risk a
// false "unchanged" verdict. Both oldBody and newBody are expected to
// already be known-parseable Go -- callers only invoke this after
// handleEdit's own syntax check has passed.
func bodyImportFootprintUnchanged(oldBody, newBody string) bool {
	oldHeads, ok := selectorHeadSet(oldBody)
	if !ok {
		return false
	}
	newHeads, ok := selectorHeadSet(newBody)
	if !ok {
		return false
	}
	if len(oldHeads) != len(newHeads) {
		return false
	}
	for head := range oldHeads {
		if !newHeads[head] {
			return false
		}
	}
	return true
}

// selectorHeadSet parses decl as a single Go declaration and, if it's a
// function/method, returns the set of bare identifier names used as the
// head of a selector expression (the X in X.Sel) within its body --
// candidate package-alias references. Names that are locally bound
// anywhere in the body (function params, receiver, named results, :=,
// var/const, range vars, type-switch guards) are excluded from the set
// entirely, in both directions, so a name that's genuinely a package
// alias in one spot but reused as a local variable name elsewhere in the
// same func is treated as unprovable rather than risking a false match --
// narrower, occurrence-level scope tracking would resolve that case too,
// but it's rare enough (shadowing an import's own name) that the coarser
// per-name exclusion is the right amount of effort here.
//
// Returns ok=false for non-FuncDecl top-level decls (type/var/const edits)
// or on any parse error -- callers should fall back to running goimports.
func selectorHeadSet(decl string) (map[string]bool, bool) {
	f, err := parser.ParseFile(token.NewFileSet(), "", "package x\n"+decl, 0)
	if err != nil || len(f.Decls) == 0 {
		return nil, false
	}
	fd, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok || fd.Body == nil {
		return nil, false
	}

	bound := map[string]bool{}
	bind := func(id *ast.Ident) {
		if id != nil && id.Name != "_" {
			bound[id.Name] = true
		}
	}
	if fd.Recv != nil {
		for _, field := range fd.Recv.List {
			for _, name := range field.Names {
				bind(name)
			}
		}
	}
	if fd.Type.Params != nil {
		for _, field := range fd.Type.Params.List {
			for _, name := range field.Names {
				bind(name)
			}
		}
	}
	if fd.Type.Results != nil {
		for _, field := range fd.Type.Results.List {
			for _, name := range field.Names {
				bind(name)
			}
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			if stmt.Tok == token.DEFINE {
				for _, lhs := range stmt.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						bind(id)
					}
				}
			}
		case *ast.GenDecl:
			if stmt.Tok == token.VAR || stmt.Tok == token.CONST {
				for _, spec := range stmt.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, id := range vs.Names {
							bind(id)
						}
					}
				}
			}
		case *ast.RangeStmt:
			if id, ok := stmt.Key.(*ast.Ident); ok {
				bind(id)
			}
			if id, ok := stmt.Value.(*ast.Ident); ok {
				bind(id)
			}
		case *ast.TypeSwitchStmt:
			if as, ok := stmt.Assign.(*ast.AssignStmt); ok && as.Tok == token.DEFINE {
				for _, lhs := range as.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						bind(id)
					}
				}
			}
		}
		return true
	})

	heads := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || bound[id.Name] {
			return true
		}
		heads[id.Name] = true
		return true
	})
	return heads, true
}
