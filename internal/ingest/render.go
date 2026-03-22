package ingest

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/token"
	"strings"
)

// renderNode formats an AST node back to Go source text.
func renderNode(fset *token.FileSet, node ast.Node) string {
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, node); err != nil {
		return "<render error>"
	}
	return buf.String()
}

// renderSignature extracts just the signature of a function (no body).
func renderSignature(fset *token.FileSet, fn *ast.FuncDecl) string {
	// Create a copy without the body to render just the signature.
	shallow := *fn
	shallow.Body = nil
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, &shallow); err != nil {
		return fn.Name.Name
	}
	return strings.TrimSpace(buf.String())
}

// typeString returns a simple string representation of a type expression.
func typeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.IndexExpr:
		return typeString(t.X) + "[" + typeString(t.Index) + "]"
	default:
		return "<unknown>"
	}
}
