package projection

import (
	"fmt"
	"strings"
)

// ReplaceHunk replaces a byte-exact occurrence of `old` inside `body`
// with `replacement`. When `old` occurs exactly once, `index` may be 0
// (unset). When `old` occurs more than once, the caller must pass a
// 1-based `index` to disambiguate — a 0/unset index against an ambiguous
// hunk is an error, not a silent first-match.
//
// Byte-exact PUTGET: the returned string is exactly
//
//	body[:pos] + replacement + body[pos+len(old):]
//
// where `pos` is the byte offset of the resolved occurrence of `old` in
// `body`.
//
// Content-addressed hunk edit: unlike ReplaceSlice, no AST role is
// required. `old` need not parse in isolation; only the resulting body
// must, which the MCP server layer enforces via applyEditTerse. This is
// the write-side analog of str_replace_editor.str_replace, but scoped
// to a single definition body — the `name` argument at the MCP layer
// carries the file-level disambiguation that str_replace has to encode
// as duplicated context on both sides.
//
// Empty `old` is rejected. Empty `replacement` deletes the matched
// hunk.
func ReplaceHunk(body, old, replacement string, index int) (string, error) {
	if body == "" {
		return "", fmt.Errorf("replace-hunk: body is empty")
	}
	if old == "" {
		return "", fmt.Errorf("replace-hunk: old is required")
	}
	if index < 0 {
		return "", fmt.Errorf("replace-hunk: index must be >= 1 (1-based), got %d", index)
	}
	var offsets []int
	off := 0
	for {
		i := strings.Index(body[off:], old)
		if i < 0 {
			break
		}
		offsets = append(offsets, off+i)
		off = off + i + len(old)
	}
	if len(offsets) == 0 {
		return "", fmt.Errorf("replace-hunk: hunk not found in body")
	}
	if len(offsets) > 1 && index == 0 {
		return "", fmt.Errorf("replace-hunk: hunk occurs %d times in body; pass index=1..%d", len(offsets), len(offsets))
	}
	if index == 0 {
		index = 1
	}
	if index > len(offsets) {
		return "", fmt.Errorf("replace-hunk: index %d exceeds %d match(es)", index, len(offsets))
	}
	pos := offsets[index-1]
	return body[:pos] + replacement + body[pos+len(old):], nil
}
