package projection

import (
	"fmt"
	"strings"
)

// ReplaceHunk replaces a byte-exact occurrence of `old` inside `body`
// with `replacement`. When `old` occurs exactly once, `index` may be 0
// (unset). When `old` occurs more than once, the caller must pass a
// 1-based `index` to disambiguate -- a 0/unset index against an
// ambiguous hunk is an error, not a silent first-match -- UNLESS
// replaceAll is set, in which case every occurrence is replaced and
// index is ignored.
//
// #302: replaceAll exists because index's 1-based positional semantics
// only make sense against a SINGLE call's snapshot of body -- batching
// several replace-hunk calls in one op:"apply" transaction, each
// targeting the same repeated old-text with a different index (1, 2,
// 3, ...), breaks: after the first call replaces occurrence 1, the
// remaining occurrence count drops, so a later call's index no longer
// refers to what the caller intended, eventually erroring "index N
// exceeds M match(es)". Confirmed on a real trajectory
// (prometheus-19338): 5 identical `errors.Join(...)` sites all needing
// the identical replacement, batched as index=1..5, failed exactly
// this way and cost 2 wasted round trips. When every occurrence gets
// the SAME replacement (the common real shape), replaceAll:true
// replaces all of them in one call with no index bookkeeping at all.
//
// Byte-exact PUTGET: the returned string is exactly
//
//	body[:pos] + replacement + body[pos+len(old):]
//
// where `pos` is the byte offset of the resolved occurrence of `old` in
// `body`. replaceAll's output is byte-exact strings.ReplaceAll(body,
// old, replacement) -- still fully deterministic, just not expressed
// as a single pos/replacement pair.
//
// Content-addressed hunk edit: unlike ReplaceSlice, no AST role is
// required. `old` need not parse in isolation; only the resulting body
// must, which the MCP server layer enforces via applyEditTerse. This is
// the write-side analog of str_replace_editor.str_replace, but scoped
// to a single definition body -- the `name` argument at the MCP layer
// carries the file-level disambiguation that str_replace has to encode
// as duplicated context on both sides.
//
// Empty `old` is rejected. Empty `replacement` deletes the matched
// hunk(s).
//
// Stale-anchor detection: when `old` isn't found anywhere in `body` but
// `replacement` already is, the error says so explicitly ("may already
// be applied") instead of a bare "not found" -- the two look identical to
// a caller retrying a batch after a partial failure, but only one of
// them means the edit already landed.
func ReplaceHunk(body, old, replacement string, index int, replaceAll bool) (string, error) {
	if body == "" {
		return "", fmt.Errorf("replace-hunk: body is empty")
	}
	if old == "" {
		return "", fmt.Errorf("replace-hunk: old is required")
	}
	if index < 0 {
		return "", fmt.Errorf("replace-hunk: index must be >= 1 (1-based), got %d", index)
	}
	count := strings.Count(body, old)
	if count == 0 {
		if replacement != "" && strings.Contains(body, replacement) {
			return "", fmt.Errorf("replace-hunk: old not found, but replacement already present in body — this hunk may already be applied")
		}
		return "", fmt.Errorf("replace-hunk: hunk not found in body")
	}
	if replaceAll {
		return strings.ReplaceAll(body, old, replacement), nil
	}
	if count > 1 && index == 0 {
		return "", fmt.Errorf("replace-hunk: hunk occurs %d times in body; pass index=1..%d, or replace_all:true to replace every occurrence with the same text", count, count)
	}
	if index == 0 {
		index = 1
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
	if index > len(offsets) {
		return "", fmt.Errorf("replace-hunk: index %d exceeds %d match(es)", index, len(offsets))
	}
	pos := offsets[index-1]
	return body[:pos] + replacement + body[pos+len(old):], nil
}
