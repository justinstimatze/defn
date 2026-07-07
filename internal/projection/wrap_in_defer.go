package projection

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
)

// WrapInDefer inserts a `defer <deferBody>` statement immediately before
// the Nth (1-based) top-level statement in the function body. If the
// body has no statements, defer is inserted between the braces. If
// stmtIndex is 0 or 1, defer is inserted at the very top of the body.
//
// Byte-exact PUTGET: existing body is preserved verbatim except for the
// inserted defer line (indented one tab beyond the function).
//
// Indent assumption: body describes a top-level function (column 0).
func WrapInDefer(body string, stmtIndex int, deferBody string) (string, error) {
	if body == "" {
		return "", fmt.Errorf("wrap-in-defer: body is empty")
	}
	if deferBody == "" {
		return "", fmt.Errorf("wrap-in-defer: defer_body is required")
	}
	const prefix = "package p\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", prefix+body, parser.ParseComments)
	if err != nil {
		return "", fmt.Errorf("wrap-in-defer: parse body: %w", err)
	}
	var fn *ast.FuncDecl
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			fn = fd
			break
		}
	}
	if fn == nil {
		return "", fmt.Errorf("wrap-in-defer: body is not a function declaration")
	}
	if fn.Body == nil {
		return "", fmt.Errorf("wrap-in-defer: function has no body")
	}
	off := func(p token.Pos) int { return fset.Position(p).Offset - len(prefix) }
	stmts := fn.Body.List
	if len(stmts) == 0 {
		lbrace := off(fn.Body.Lbrace)
		return body[:lbrace+1] + "\n\tdefer " + deferBody + body[lbrace+1:], nil
	}
	if stmtIndex < 1 {
		stmtIndex = 1
	}
	if stmtIndex > len(stmts) {
		return "", fmt.Errorf("wrap-in-defer: stmt_index %d exceeds %d statement(s)", stmtIndex, len(stmts))
	}
	target := stmts[stmtIndex-1]
	stmtStart := off(target.Pos())
	lineStart := stmtStart
	for lineStart > 0 && body[lineStart-1] != '\n' {
		lineStart--
	}
	return body[:lineStart] + "\tdefer " + deferBody + "\n" + body[lineStart:], nil
}
