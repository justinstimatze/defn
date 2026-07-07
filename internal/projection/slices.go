package projection

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// Slice is an AST-role subrange of a function body. Source is byte-exact:
// Source == body[StartOff:EndOff].
type Slice struct {
	Kind     string
	Line     int
	StartOff int
	EndOff   int
	Source   string
}

// SliceKinds enumerates every accepted `kind` value for Slices.
var SliceKinds = map[string]bool{
	"signature":    true,
	"doc":          true,
	"body":         true,
	"error-branch": true,
	"return":       true,
	"loop":         true,
}

// SliceKindNames returns the sorted list of accepted kinds.
func SliceKindNames() []string {
	names := make([]string, 0, len(SliceKinds))
	for k := range SliceKinds {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Slices parses body and returns every AST subtree matching kind, with
// byte offsets into body. Body must be a function declaration.
//
// Byte-exact invariant: for every returned Slice s, s.Source ==
// body[s.StartOff:s.EndOff]. Callers can splice replacement bytes back
// into body via body[:s.StartOff] + replacement + body[s.EndOff:].
func Slices(body, kind string) ([]Slice, error) {
	if !SliceKinds[kind] {
		return nil, fmt.Errorf("unknown slice kind %q — valid: %s", kind, strings.Join(SliceKindNames(), ", "))
	}
	if body == "" {
		return nil, nil
	}
	const prefix = "package p\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", prefix+body, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse body: %w", err)
	}
	if len(f.Decls) == 0 {
		return nil, nil
	}
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok {
		return nil, fmt.Errorf("slice: body is not a function declaration")
	}
	off := func(p token.Pos) int { return fset.Position(p).Offset - len(prefix) }
	line := func(p token.Pos) int { return fset.Position(p).Line }

	switch kind {
	case "signature":
		s, e := off(fn.Pos()), off(fn.Type.End())
		return []Slice{{Kind: kind, Line: line(fn.Pos()) - 1, StartOff: s, EndOff: e, Source: body[s:e]}}, nil
	case "doc":
		if fn.Doc == nil {
			return nil, nil
		}
		s, e := off(fn.Doc.Pos()), off(fn.Doc.End())
		return []Slice{{Kind: kind, Line: line(fn.Doc.Pos()) - 1, StartOff: s, EndOff: e, Source: body[s:e]}}, nil
	}

	if fn.Body == nil {
		return nil, nil
	}
	if kind == "body" {
		s, e := off(fn.Body.Lbrace), off(fn.Body.Rbrace)+1
		return []Slice{{Kind: kind, Line: line(fn.Body.Lbrace) - 1, StartOff: s, EndOff: e, Source: body[s:e]}}, nil
	}

	defStartLine := line(fn.Pos())
	var out []Slice
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		match := false
		switch kind {
		case "error-branch":
			if ifStmt, ok := n.(*ast.IfStmt); ok && isErrNotNil(ifStmt.Cond) {
				match = true
			}
		case "return":
			if _, ok := n.(*ast.ReturnStmt); ok {
				match = true
			}
		case "loop":
			switch n.(type) {
			case *ast.ForStmt, *ast.RangeStmt, *ast.SelectStmt:
				match = true
			}
		}
		if !match {
			return true
		}
		s, e := off(n.Pos()), off(n.End())
		if s < 0 || e > len(body) {
			return true
		}
		out = append(out, Slice{
			Kind:     kind,
			Line:     line(n.Pos()) - defStartLine + 1,
			StartOff: s,
			EndOff:   e,
			Source:   body[s:e],
		})
		return false
	})
	return out, nil
}

// isErrNotNil returns true for `<ident>err != nil` shapes (case-insensitive
// suffix). Matches `err != nil`, `dbErr != nil`; rejects `err == nil`.
func isErrNotNil(expr ast.Expr) bool {
	be, ok := expr.(*ast.BinaryExpr)
	if !ok || be.Op != token.NEQ {
		return false
	}
	nilIdent, ok := be.Y.(*ast.Ident)
	if !ok || nilIdent.Name != "nil" {
		return false
	}
	ident, ok := be.X.(*ast.Ident)
	if !ok {
		return false
	}
	name := strings.ToLower(ident.Name)
	return name == "err" || strings.HasSuffix(name, "err")
}
