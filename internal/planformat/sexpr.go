package planformat

import (
	"fmt"
	"strings"
)

// ParseSExpr parses the S-expression prototype for #189: one
// "(op target [!test])" form per line. Deliberately NOT a general
// s-expression reader -- no nesting, no quoted atoms -- because every
// canonical trajectory is a flat step list; a real nested-plan grammar
// is follow-up scope if this format wins #187's decision.
func ParseSExpr(text string) ([]Step, error) {
	var steps []Step
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ";") {
			continue
		}
		if !strings.HasPrefix(line, "(") || !strings.HasSuffix(line, ")") {
			return nil, fmt.Errorf("sexpr line %d: expected '(...)', got %q", i+1, line)
		}
		atoms := strings.Fields(line[1 : len(line)-1])
		if len(atoms) < 2 || len(atoms) > 3 {
			return nil, fmt.Errorf("sexpr line %d: expected (op target [!test]), got %q", i+1, line)
		}
		field, ok := opField[atoms[0]]
		if !ok {
			return nil, fmt.Errorf("sexpr line %d: unknown op %q (want read|outline|impact)", i+1, atoms[0])
		}
		excludeTest := false
		if len(atoms) == 3 {
			if atoms[2] != "!test" {
				return nil, fmt.Errorf("sexpr line %d: unsupported flag %q", i+1, atoms[2])
			}
			excludeTest = true
		}
		steps = append(steps, Step{Target: atoms[1], Field: field, ExcludeTest: excludeTest})
	}
	return steps, nil
}

// RenderSExpr is the inverse of ParseSExpr.
func RenderSExpr(steps []Step) string {
	var sb strings.Builder
	for _, s := range steps {
		sb.WriteString("(")
		sb.WriteString(fieldOp[s.Field])
		sb.WriteString(" ")
		sb.WriteString(s.Target)
		if s.ExcludeTest {
			sb.WriteString(" !test")
		}
		sb.WriteString(")\n")
	}
	return sb.String()
}

// fieldOp is opField's inverse, used by RenderSExpr.
var fieldOp = map[string]string{"body": "read", "outline": "outline", "callers": "impact"}

// opField maps an s-expression's head atom to the Step field it
// requests -- "ops as head" per #189's design note. Kept 1:1 with
// ValidFields so both prototype formats parse to the identical Step
// vocabulary.
var opField = map[string]string{"read": "body", "outline": "outline", "impact": "callers"}
