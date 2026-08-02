// Package planformat implements #188/#189: two candidate structured-
// output formats for defn "trajectory" plans -- a Step is one server-
// side retrieval action (which def, which projection). A trajectory is
// a []Step the calling model emits in bulk; defn parses it and
// mechanically walks it via the same per-def renderer code(op:"expand")
// already uses, instead of one round-trip per step.
//
// Field names reuse code(op:"expand")'s existing include vocabulary
// (outline/body/callers) rather than inventing new ones -- #187's
// mechanical-expansion promise only holds if a Step maps directly onto
// an existing, tested rendering path.
package planformat

import "encoding/json"

// Step is one line of a trajectory: fetch Field for Target. ExcludeTest
// only applies when Field == "callers" (drops test-only callers from
// the rendered list, mirroring the "[!test]" filter both prototype
// formats support).
type Step struct {
	Target      string `json:"target"`
	Field       string `json:"field"`
	ExcludeTest bool   `json:"exclude_test,omitempty"`
}

// ValidFields enumerates the include kinds a Step's Field may name --
// deliberately the same three code(op:"expand") already supports.
var ValidFields = map[string]bool{"outline": true, "body": true, "callers": true}

// RenderJSON is the narrow-JSON baseline the DSL/S-expr prototypes are
// measured against: one compact array, no whitespace, omitempty on the
// rarely-set ExcludeTest field.
func RenderJSON(steps []Step) (string, error) {
	b, err := json.Marshal(steps)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseJSON is the inverse of RenderJSON.
func ParseJSON(text string) ([]Step, error) {
	var steps []Step
	if err := json.Unmarshal([]byte(text), &steps); err != nil {
		return nil, err
	}
	return steps, nil
}
