// Package projection implements defn's projection-level edit vocabulary:
// small, mechanically-verifiable source-code edit primitives whose `put`
// side (edit application) satisfies a byte-exact or quotient-lens PUTGET
// contract against the `get` side (projection read).
//
// Each operator is a pure function over a definition body string. The
// wiring into the MCP layer lives in internal/mcp/server.go; the pure
// functions live here so their PUTGET goldens can be tested without any
// DB or MCP dependencies.
//
// See project_putget_edit_vocab_design and project_projection_phase_c_next
// memory for the design contract and phase plan.
package projection

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

func ReplaceSlice(body, kind string, index int, replacement string) (string, error) {
	t, err := replaceSliceRange(body, kind, index)
	if err != nil {
		return "", err
	}
	if err := validateReplacementShape(kind, replacement); err != nil {
		return "", err
	}
	comments, err := interiorComments(body, t.StartOff, t.EndOff)
	if err != nil {
		return "", err
	}
	var lost []string
	for _, c := range comments {
		if !strings.Contains(replacement, c) {
			lost = append(lost, c)
		}
	}
	if len(lost) > 0 {
		return "", fmt.Errorf("replace-slice: refusing to discard %d interior comment(s) not present in replacement: %s. Include them in replacement or use force to acknowledge", len(lost), strings.Join(lost, " | "))
	}
	return body[:t.StartOff] + replacement + body[t.EndOff:], nil
}

func ReplaceSliceForce(body, kind string, index int, replacement string) (string, error) {
	t, err := replaceSliceRange(body, kind, index)
	if err != nil {
		return "", err
	}
	if err := validateReplacementShape(kind, replacement); err != nil {
		return "", err
	}
	return body[:t.StartOff] + replacement + body[t.EndOff:], nil
}

// replaceSliceRange resolves body[kind][index-1] with the validation
// shared by ReplaceSlice and ReplaceSliceForce.
func replaceSliceRange(body, kind string, index int) (Slice, error) {
	if body == "" {
		return Slice{}, fmt.Errorf("replace-slice: body is empty")
	}
	if index < 1 {
		return Slice{}, fmt.Errorf("replace-slice: index must be >= 1 (1-based), got %d", index)
	}
	slices, err := Slices(body, kind)
	if err != nil {
		return Slice{}, err
	}
	if len(slices) == 0 {
		return Slice{}, fmt.Errorf("replace-slice: no %s slices found in body", kind)
	}
	if index > len(slices) {
		return Slice{}, fmt.Errorf("replace-slice: index %d exceeds %d match(es)", index, len(slices))
	}
	return slices[index-1], nil
}

// interiorComments returns the raw text of every comment whose byte
// offset in body falls in [start, end). Parses body with the "package
// p\n" prefix Slices uses. Doc comments attached to the outer function
// live at negative body offsets after the prefix subtraction, so the
// offset test excludes them.
func interiorComments(body string, start, end int) ([]string, error) {
	if body == "" {
		return nil, nil
	}
	const prefix = "package p\n"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", prefix+body, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("interior comments: parse body: %w", err)
	}
	var out []string
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			off := fset.Position(c.Pos()).Offset - len(prefix)
			if off >= start && off < end {
				out = append(out, c.Text)
			}
		}
	}
	return out, nil
}

// validateReplacementShape defends against mangled caller text that is
// syntactically valid Go but not the single statement the slice kind
// implies. return/loop/error-branch slices are each always exactly one
// AST statement; a stray literal ";" (e.g. from an HTML-escaped operator
// like "&lt;" -- "&", "lt", ";") can split replacement into two
// syntactically-valid top-level statements that go/parser accepts
// without complaint, since "expression statement must be a call" is a
// type-check rule, not a parse rule, and projection ops skip go build
// (#148). signature/doc/body aren't single-statement shapes, so they're
// skipped.
func validateReplacementShape(kind, replacement string) error {
	switch kind {
	case "return", "loop", "error-branch":
	default:
		return nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), "", "package p\nfunc f() {\n"+replacement+"\n}", 0)
	if err != nil {
		return fmt.Errorf("replace-slice: replacement has a syntax error: %w", err)
	}
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	if !ok || fn.Body == nil {
		return fmt.Errorf("replace-slice: replacement did not parse as a statement")
	}
	if n := len(fn.Body.List); n != 1 {
		return fmt.Errorf("replace-slice: replacement parses as %d statement(s), expected exactly 1 for slice kind %q -- this usually means it contains invalid syntax (e.g. an HTML-escaped operator like \"&lt;\" instead of \"<\") that splits it into multiple statements", n, kind)
	}
	return nil
}
