package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoRawStringTextColumnScans is the static enforcement partner to
// TestTextColScan. It walks every Scan call in store.go / upstream.go
// and fails if any argument is a raw *string when it should have been
// *textCol. Prevents the class of bug caught only after fe2decf/#113:
// direct `var body string; row.Scan(&body)` against a TEXT column
// silently works on old Dolt but crashes when the driver returns a
// *val.TextStorage wrapper.
//
// The rule: any variable whose name matches a known TEXT/LONGTEXT/
// MEDIUMTEXT column (or a struct field pointing to one) MUST scan
// through textCol. VARCHAR-shaped columns can still scan as string.
//
// If a new legitimate `string` scan against a non-TEXT column is
// introduced, add the variable-name suffix to skipNameSuffixes or the
// exact name to skipExactNames — DO NOT weaken the check. If a new
// TEXT column is added, add its column-derived variable name to
// textColumnNames so this test flags any raw scan of it.
func TestNoRawStringTextColumnScans(t *testing.T) {
	// Walk every non-test .go file in the package so new files added to
	// store/ are automatically audited without the test needing an update.
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e, "_test.go") {
			continue
		}
		// The TextStorage-wrapper concern is Dolt-specific — modernc.org/sqlite
		// returns plain strings, so raw *string scans in the SQLite backend
		// are safe. Exclude sqlite.go from the audit.
		if e == "sqlite.go" {
			continue
		}
		files = append(files, e)
	}
	fset := token.NewFileSet()

	// Variable names that indicate a TEXT-shaped column. Any raw *string
	// scan into a variable ending in one of these is a bug.
	textColumnNames := []string{
		// Bodies / signatures / docs — bytes-material text.
		"body", "signature", "doc", "text", "content", "raw",
		// Retargetable-value shape (literal_fields.field_value, defn_meta.value).
		"value", "fieldvalue",
		// Merge conflict bodies (dolt_conflicts_bodies).
		"base", "ours", "theirs",
		// Dolt system commit messages (dolt_log.message).
		"message", "lastmsg",
		// Comment-adjacent TEXT columns (pragma_value).
		"pragmaval",
	}
	// Exact-name allowlist for legitimate non-TEXT string scans that
	// happen to match one of the fuzzy names above.
	skipExactNames := map[string]bool{
		"query": true, // SQL string, not scanned
		"q":     true, // ditto
	}
	// Fields on struct types where the field is TEXT-shaped. Formatted as
	// "TypeName.FieldName" — matched when the Scan arg is `&x.Field`.
	textStructFields := map[string]bool{
		"Comment.PragmaVal":       true,
		"Comment.Text":            true,
		"Conflict.Base":           true,
		"Conflict.Ours":           true,
		"Conflict.Theirs":         true,
		"LiteralField.FieldValue": true,
		"Definition.Body":         true,
		"Definition.Doc":          true,
		"Definition.Signature":    true,
		"BodyMatch.Snippet":       true,
	}

	var violations []string
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			// Collect var-name → declared type for the function.
			stringVars := collectStringVars(fn.Body)
			// Walk the body for .Scan(...) calls.
			ast.Inspect(fn.Body, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Scan" {
					return true
				}
				for _, arg := range call.Args {
					unary, ok := arg.(*ast.UnaryExpr)
					if !ok || unary.Op != token.AND {
						continue
					}
					switch x := unary.X.(type) {
					case *ast.Ident:
						// &varname — check if declared as string with a
						// TEXT-shaped name.
						if !stringVars[x.Name] {
							continue
						}
						if skipExactNames[x.Name] {
							continue
						}
						lower := strings.ToLower(x.Name)
						for _, tc := range textColumnNames {
							if lower == tc || strings.HasSuffix(lower, tc) {
								pos := fset.Position(x.Pos())
								violations = append(violations,
									pos.Filename+":"+itoa(pos.Line)+": raw *string Scan of TEXT-shaped var "+
										x.Name+" in "+fn.Name.Name+" — use textCol")
								break
							}
						}
					case *ast.SelectorExpr:
						// &recv.Field — check against textStructFields.
						recv, ok := x.X.(*ast.Ident)
						if !ok {
							continue
						}
						key := receiverTypeName(fn, recv.Name) + "." + x.Sel.Name
						if textStructFields[key] {
							pos := fset.Position(x.Pos())
							violations = append(violations,
								pos.Filename+":"+itoa(pos.Line)+": raw *string Scan of TEXT struct field "+
									key+" in "+fn.Name.Name+" — use textCol intermediate then assign")
						}
					}
				}
				return true
			})
			return true
		})
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("textCol audit found %d violation(s):\n  %s\n\n"+
			"Every raw *string Scan of a TEXT/LONGTEXT/MEDIUMTEXT column must go\n"+
			"through textCol (or scanDefRow) so Dolt's *val.TextStorage wrapper\n"+
			"doesn't crash the driver. See internal/store/store.go: textCol +\n"+
			"scanDefRow for the pattern.",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// collectStringVars walks a function body and returns the set of local
// variable names whose declared type is `string`. Handles single-name
// (`var x string`) and grouped (`var x, y string`) declarations.
func collectStringVars(body *ast.BlockStmt) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		decl, ok := n.(*ast.DeclStmt)
		if !ok {
			return true
		}
		gen, ok := decl.Decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			return true
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typeName := typeIdentName(vs.Type)
			if typeName != "string" {
				continue
			}
			for _, name := range vs.Names {
				out[name.Name] = true
			}
		}
		return true
	})
	return out
}

// typeIdentName returns the name of a simple type expression (Ident),
// or "" if the type is a composite (map, slice, pointer, etc.).
func typeIdentName(expr ast.Expr) string {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name
}

// receiverTypeName infers a locally-bound identifier's struct type.
// Best-effort: matches shortcuts like `var c Comment` inside the body
// or a parameter `c *Comment`. Returns "" if the type can't be
// inferred; textStructFields lookup then can't match, so no false
// positive.
func receiverTypeName(fn *ast.FuncDecl, name string) string {
	if fn.Body != nil {
		var result string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			decl, ok := n.(*ast.DeclStmt)
			if !ok {
				return true
			}
			gen, ok := decl.Decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				return true
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, nm := range vs.Names {
					if nm.Name == name {
						result = typeIdentName(vs.Type)
						return false
					}
				}
			}
			return true
		})
		if result != "" {
			return result
		}
	}
	if fn.Type != nil && fn.Type.Params != nil {
		for _, field := range fn.Type.Params.List {
			for _, nm := range field.Names {
				if nm.Name == name {
					return typeIdentName(deref(field.Type))
				}
			}
		}
	}
	return ""
}

func deref(expr ast.Expr) ast.Expr {
	if star, ok := expr.(*ast.StarExpr); ok {
		return star.X
	}
	return expr
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := []byte{}
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}
