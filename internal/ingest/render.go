package ingest

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/token"
	"os"
	"strings"
)

// sourceFileCache avoids re-reading the same file for every definition.
var sourceFileCache = map[string][]byte{}

// renderNode extracts Go source text for an AST node.
// Uses the original source file to preserve all comments (including inline
// comments between statements that format.Node would drop).
// Falls back to format.Node if source text isn't available.
func renderNode(fset *token.FileSet, node ast.Node) string {
	start := fset.Position(node.Pos())
	end := fset.Position(node.End())
	if start.IsValid() && end.IsValid() && start.Filename != "" {
		data, ok := sourceFileCache[start.Filename]
		if !ok {
			var err error
			data, err = os.ReadFile(start.Filename)
			if err != nil {
				data = nil
			}
			sourceFileCache[start.Filename] = data
		}
		if data != nil && end.Offset <= len(data) {
			// Include doc comment if present (it's before Pos).
			startOffset := start.Offset
			if fn, ok := node.(*ast.FuncDecl); ok && fn.Doc != nil {
				docStart := fset.Position(fn.Doc.Pos())
				if docStart.IsValid() && docStart.Offset < startOffset {
					startOffset = docStart.Offset
				}
			}
			if gd, ok := node.(*ast.GenDecl); ok && gd.Doc != nil {
				docStart := fset.Position(gd.Doc.Pos())
				if docStart.IsValid() && docStart.Offset < startOffset {
					startOffset = docStart.Offset
				}
			}
			return strings.TrimSpace(string(data[startOffset:end.Offset]))
		}
	}
	// Fallback to format.Node.
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
