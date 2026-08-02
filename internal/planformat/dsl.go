package planformat

import (
	"fmt"
	"strings"
)

// ParseDSL parses the compact DSL prototyped for #188: one line per
// Step, "@Target.field[filter]". Target may itself contain dots --
// Go's own receiver.method notation, e.g. "Handler.ServeHTTP" -- so
// only the FINAL dot-segment is treated as the field selector, and it
// must name one of ValidFields. Blank lines and lines starting with
// "#" are ignored (comments), matching every other defn config format.
func ParseDSL(text string) ([]Step, error) {
	var steps []Step
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "@") {
			return nil, fmt.Errorf("dsl line %d: expected '@' prefix, got %q", i+1, line)
		}
		line = line[1:]

		excludeTest := false
		if open := strings.IndexByte(line, '['); open >= 0 {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("dsl line %d: unterminated filter in %q", i+1, line)
			}
			filterBody := line[open+1 : len(line)-1]
			line = line[:open]
			for _, tok := range strings.Split(filterBody, ",") {
				tok = strings.TrimSpace(tok)
				switch tok {
				case "!test":
					excludeTest = true
				default:
					return nil, fmt.Errorf("dsl line %d: unsupported filter %q", i+1, tok)
				}
			}
		}

		dot := strings.LastIndexByte(line, '.')
		if dot < 0 {
			return nil, fmt.Errorf("dsl line %d: missing .field in %q", i+1, line)
		}
		target, field := line[:dot], line[dot+1:]
		if !ValidFields[field] {
			return nil, fmt.Errorf("dsl line %d: unknown field %q (want outline|body|callers)", i+1, field)
		}
		if target == "" {
			return nil, fmt.Errorf("dsl line %d: empty target", i+1)
		}
		steps = append(steps, Step{Target: target, Field: field, ExcludeTest: excludeTest})
	}
	return steps, nil
}

// RenderDSL is the inverse of ParseDSL -- one "@Target.field[...]" line
// per Step, in order.
func RenderDSL(steps []Step) string {
	var sb strings.Builder
	for _, s := range steps {
		sb.WriteString("@")
		sb.WriteString(s.Target)
		sb.WriteString(".")
		sb.WriteString(s.Field)
		if s.ExcludeTest {
			sb.WriteString("[!test]")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
