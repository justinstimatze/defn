package ingest

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"os"
	"strings"
	"sync"
)

// sourceFileCache avoids re-reading the same file for every definition
// during a single ingest run. It must be cleared at the start of each
// top-level ingest call — stale cached bytes combined with fresh AST
// offsets produces misaligned body slices (the offsets match the new
// parse, but the bytes are from before the file was edited).
var (
	sourceFileMu    sync.Mutex
	sourceFileCache = map[string][]byte{}
)

// clearSourceFileCache drops all cached file contents. Call at the start
// of each Ingest / IngestFile so later reads pick up current on-disk bytes.
func clearSourceFileCache() {
	sourceFileMu.Lock()
	sourceFileCache = map[string][]byte{}
	sourceFileMu.Unlock()
}

// renderNode extracts Go source text for an AST node.
// Uses the original source file to preserve all comments (including inline
// comments between statements that format.Node would drop).
// Falls back to format.Node if source text isn't available.
func renderNode(fset *token.FileSet, node ast.Node) string {
	start := fset.Position(node.Pos())
	end := fset.Position(node.End())
	if start.IsValid() && end.IsValid() && start.Filename != "" {
		sourceFileMu.Lock()
		data, ok := sourceFileCache[start.Filename]
		if !ok {
			var err error
			data, err = os.ReadFile(start.Filename)
			if err != nil {
				data = nil
			}
			sourceFileCache[start.Filename] = data
		}
		sourceFileMu.Unlock()
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

// valueSpecSignature renders a signature like `var Foo Bar` or
// `const Baz int` for a ValueSpec. When no explicit type is given,
// it infers the type from a composite-literal initializer if possible
// (e.g. `var X = Foo{...}` → `var X Foo`). Falls back to `kind name`
// if the type can't be recovered without running go/types.
func valueSpecSignature(kind, name string, s *ast.ValueSpec) string {
	if s.Type != nil {
		if t := typeString(s.Type); t != "<unknown>" {
			return fmt.Sprintf("%s %s %s", kind, name, t)
		}
	}
	// Try to infer from the matching initializer.
	idx := 0
	for i, id := range s.Names {
		if id.Name == name {
			idx = i
			break
		}
	}
	if idx < len(s.Values) {
		if t := inferInitType(s.Values[idx]); t != "" {
			return fmt.Sprintf("%s %s %s", kind, name, t)
		}
	}
	return fmt.Sprintf("%s %s", kind, name)
}

// inferInitType returns a type name for common initializer shapes:
// composite literals, unary address-of, and basic literals. Function
// calls are intentionally NOT handled — without go/types we can't
// distinguish `T(x)` (conversion to type T) from `f(x)` (call to
// function f), and mislabeling functions as types is worse than
// leaving the signature untyped.
func inferInitType(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.CompositeLit:
		if e.Type != nil {
			if t := typeString(e.Type); t != "<unknown>" {
				return t
			}
		}
	case *ast.BasicLit:
		switch e.Kind {
		case token.INT:
			return "int"
		case token.FLOAT:
			return "float64"
		case token.STRING:
			return "string"
		case token.CHAR:
			return "rune"
		}
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			if inner := inferInitType(e.X); inner != "" {
				return "*" + inner
			}
		}
	}
	return ""
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
