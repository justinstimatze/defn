package projection

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
)

// RenameParam renames a function parameter (or receiver) throughout the
// signature and body. All identifiers ast.Object-bound to the parameter
// are rewritten to newName; identifiers introduced by inner declarations
// with the same name (shadowing) are automatically skipped because they
// have a distinct *ast.Object.
//
// Quotient equivalence: ≡_gofmt — output is gofmt-normalized because
// go/format re-emits the source, so column alignment (e.g. multi-line
// argument tabs) may differ from the input. Byte-exact comparison
// against the input body is not the contract; comparison against
// gofmt-canonical expected output is.
//
// v1 scope: value params + receivers only. Type params (`[T any]`) are
// not scanned; rename-param on a type param returns "no param named X".
func RenameParam(body, oldName, newName string) (string, error) {
	if body == "" {
		return "", fmt.Errorf("rename-param: body is empty")
	}
	if oldName == "" {
		return "", fmt.Errorf("rename-param: old_param is required")
	}
	if newName == "" {
		return "", fmt.Errorf("rename-param: new_param is required")
	}
	if oldName == newName {
		return body, nil
	}

	const prefix = "package p\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", prefix+body, parser.ParseComments)
	if err != nil {
		return "", fmt.Errorf("rename-param: parse body: %w", err)
	}
	var fn *ast.FuncDecl
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd
			break
		}
	}
	if fn == nil {
		return "", fmt.Errorf("rename-param: body is not a function declaration")
	}

	var paramIdent *ast.Ident
	scan := func(fl *ast.FieldList) {
		if fl == nil || paramIdent != nil {
			return
		}
		for _, field := range fl.List {
			for _, name := range field.Names {
				if name.Name == oldName {
					paramIdent = name
					return
				}
			}
		}
	}
	scan(fn.Recv)
	scan(fn.Type.Params)
	if paramIdent == nil {
		return "", fmt.Errorf("rename-param: no param named %q found", oldName)
	}
	if paramIdent.Obj == nil {
		return "", fmt.Errorf("rename-param: param %q has no ast.Object binding", oldName)
	}
	paramObj := paramIdent.Obj

	ast.Inspect(fn, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Obj == paramObj {
			id.Name = newName
		}
		return true
	})

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, f); err != nil {
		return "", fmt.Errorf("rename-param: format: %w", err)
	}
	out := buf.String()
	for _, p := range []string{"package p\n\n", "package p\n"} {
		if strings.HasPrefix(out, p) {
			out = out[len(p):]
			break
		}
	}
	if !strings.HasSuffix(body, "\n") {
		out = strings.TrimRight(out, "\n")
	}
	return out, nil
}
