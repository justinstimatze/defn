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
	"go/parser"
	"go/token"
	"strings"
)

// ReplaceSlice replaces the Nth (1-based) match of the given slice kind
// with replacement verbatim. Refuses if interior comments would be
// silently discarded — use ReplaceSliceForce to override.
//
// Byte-exact PUTGET (when interior comments absent): the returned string
// is exactly body[:s.StartOff] + replacement + body[s.EndOff:] where s =
// Slices(body, kind)[index-1].
//
// Interior comment defense: if the replaced range contains a comment
// whose text is NOT present in the replacement, returns an error listing
// the discarded comment text. Defends against silent data loss when the
// caller (typically an LLM agent) has not audited the original body
// before submitting a replacement.
func ReplaceSlice(body, kind string, index int, replacement string) (string, error) {
	t, err := replaceSliceRange(body, kind, index)
	if err != nil {
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

// ReplaceSliceForce matches v1 behavior: byte-exact splice with no
// comment defense. Any interior comments in the replaced range are
// discarded silently. Use only when the caller has explicitly
// acknowledged this — e.g. an LLM agent that has read the original body.
func ReplaceSliceForce(body, kind string, index int, replacement string) (string, error) {
	t, err := replaceSliceRange(body, kind, index)
	if err != nil {
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
