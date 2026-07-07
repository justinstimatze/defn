package projection

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
)

// InsertPrecondition inserts an `if <condition> { <ret> }` block at the
// start of the function body, immediately after the opening brace. The
// existing body is preserved verbatim.
//
// Byte-exact PUTGET: for any well-formed (body, condition, ret) the
// output is exactly body[:lbrace+1] + "\n\tif <cond> {\n\t\t<ret>\n\t}"
// + body[lbrace+1:]. The function makes no formatting changes to the
// input body outside the inserted block.
//
// Indent assumption: body describes a top-level function or method
// (column 0), so the inserted `if` is at one tab and its inner statement
// is at two tabs. Callers passing nested function bodies will get an
// under-indented block — v1 does not compensate.
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
	return body[:lbraceOff+1] + block + body[lbraceOff+1:], nil
}
