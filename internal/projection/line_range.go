package projection

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseLineRange parses a "<start>-<end>" or "<start>:<end>" range
// spec (1-indexed, inclusive, whitespace-tolerant) into its two
// endpoints. Both separators are accepted -- a model asking for a
// range naturally reaches for either, per real usage (confirmed live:
// one trajectory tried "700-820" then "700:820" for the same range).
// Swaps start/end if given in reverse order; clamps start below 1 up
// to 1. Returns an error only for genuinely malformed input (missing
// separator, non-numeric endpoint) -- out-of-range bounds are a job
// for the caller (ExtractLineRange clamps those instead of erroring).
func ParseLineRange(spec string) (start, end int, err error) {
	s := strings.TrimSpace(spec)
	sep := "-"
	if strings.Contains(s, ":") {
		sep = ":"
	}
	parts := strings.SplitN(s, sep, 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("line_range %q: expected \"<start>-<end>\" or \"<start>:<end>\"", spec)
	}
	start, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("line_range %q: bad start: %w", spec, err)
	}
	end, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("line_range %q: bad end: %w", spec, err)
	}
	if start > end {
		start, end = end, start
	}
	if start < 1 {
		start = 1
	}
	return start, end, nil
}

// ExtractLineRange narrows body to the file lines spanning
// [wantStart, wantEnd] (1-indexed, inclusive), given that body's own
// first line sits at file line bodyStartLine. Bounds are clamped to
// the body's actual extent rather than erroring, so a caller can
// freely ask for "beyond the end" and get whatever's actually there.
// ok is false only when the requested range doesn't overlap the body
// at all (e.g. entirely before bodyStartLine or past its end) --
// callers should fall back to the full body in that case rather than
// return an empty result that looks like a real (if useless) read.
func ExtractLineRange(body string, bodyStartLine, wantStart, wantEnd int) (narrowed string, actualStart, actualEnd int, ok bool) {
	lines := strings.Split(body, "\n")
	n := len(lines)
	relStart := wantStart - bodyStartLine
	relEnd := wantEnd - bodyStartLine
	if relStart < 0 {
		relStart = 0
	}
	if relEnd > n-1 {
		relEnd = n - 1
	}
	if relStart > relEnd || relStart >= n || relEnd < 0 {
		return "", wantStart, wantEnd, false
	}
	actualStart = bodyStartLine + relStart
	actualEnd = bodyStartLine + relEnd
	narrowed = strings.Join(lines[relStart:relEnd+1], "\n")
	return narrowed, actualStart, actualEnd, true
}

// BodyStartLine returns the file line where body's first character
// actually sits, given a def's stored StartLine/EndLine. Those fields
// come from the AST node's own Pos()/End() (the "func"/"type"/"var"
// keyword through the closing brace) and do NOT cover a leading doc
// comment -- but the stored Body text DOES include it (ingestFunc's
// renderNode extends the source slice backward to the doc comment
// when one is present). Comparing body's actual line count against
// the node's own line count (EndLine-StartLine+1) recovers exactly
// how many doc-comment lines were prepended, with no extra ingest-
// side bookkeeping needed.
func BodyStartLine(body string, startLine, endLine int) int {
	bodyLines := strings.Count(body, "\n") + 1
	nodeLines := endLine - startLine + 1
	docLines := bodyLines - nodeLines
	if docLines < 0 {
		docLines = 0
	}
	return startLine - docLines
}
