package projection

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

func InsertPrecondition(body, condition, ret string) (string, error) {
	if body == "" {
		return "", fmt.Errorf("insert-precondition: body is empty")
	}
	if condition == "" {
		return "", fmt.Errorf("insert-precondition: condition is required")
	}
	if ret == "" {
		return "", fmt.Errorf("insert-precondition: ret is required")
	}
	const prefix = "package p\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", prefix+body, parser.ParseComments)
	if err != nil {
		return "", fmt.Errorf("insert-precondition: parse body: %w", err)
	}
	var fn *ast.FuncDecl
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd
			break
		}
	}
	if fn == nil {
		return "", fmt.Errorf("insert-precondition: body is not a function declaration")
	}
	if fn.Body == nil {
		return "", fmt.Errorf("insert-precondition: function has no body (external or interface)")
	}
	lbraceOff := fset.Position(fn.Body.Lbrace).Offset - len(prefix)
	if lbraceOff < 0 || lbraceOff >= len(body) {
		return "", fmt.Errorf("insert-precondition: Lbrace offset %d outside body [0,%d)", lbraceOff, len(body))
	}
	block := "\n\tif " + condition + " {\n\t\t" + ret + "\n\t}"
	if strings.HasPrefix(body[lbraceOff+1:], block) {
		return "", fmt.Errorf("insert-precondition: this exact precondition already present at the top of the body -- may already be applied")
	}
	newBody := body[:lbraceOff+1] + block + body[lbraceOff+1:]

	// Malformed-condition defense: Go's `if init; cond {}` two-clause
	// grammar means garbage like "a &lt; 0" (stray "&", ident "lt",
	// ";") can still parse -- as a two-clause if whose Init is the
	// nonsense expression statement "a & lt" and whose Cond is "0".
	// go/parser accepts this (statement-shape validity is a parser
	// concern; "expression statement must be a call" is a type-check
	// concern go build would catch -- but #148 skips go build for
	// projection ops). Since InsertPrecondition only ever emits a
	// single-clause `if condition { ret }`, a non-nil Init on the
	// inserted statement is conclusive proof condition was mangled,
	// not a plain boolean expression.
	f2, err := parser.ParseFile(token.NewFileSet(), "", prefix+newBody, 0)
	if err != nil {
		return "", fmt.Errorf("insert-precondition: resulting body has a syntax error: %w", err)
	}
	for _, decl := range f2.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil || len(fd.Body.List) == 0 {
			continue
		}
		ifStmt, ok := fd.Body.List[0].(*ast.IfStmt)
		if !ok {
			continue
		}
		if ifStmt.Init != nil {
			return "", fmt.Errorf("insert-precondition: condition %q is not a single boolean expression -- it parses as a two-clause if-init, which usually means it contains invalid syntax (e.g. an HTML-escaped operator like \"&lt;\" instead of \"<\")", condition)
		}
		break
	}

	return newBody, nil
}
