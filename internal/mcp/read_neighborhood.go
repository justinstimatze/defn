package mcp

import (
	"fmt"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
	"github.com/justinstimatze/defn/internal/summary"
)

// renderReadNeighborhood emits a compact "Related:" footer appended to
// full-body read responses. The bundle is deliberately small (~500B)
// but carries the four signals the model would otherwise chain 3-4
// follow-up calls to gather:
//
//  1. #160 one-line summary — what does this def do
//  2. Top 3 callers    (name + file:line)
//  3. Top 3 callees    (name + file:line)
//  4. #197 related-defs via SearchDefSummaries keyed off summary
//     keywords — semantically-adjacent defs the reader may want next
//
// Design (#202, 2026-07-23): three rounds of CLAUDE.md nudges failed to
// shift the model's tool-selection reflex — it kept calling read
// instead of expand/context/impact. Instead of fighting the reflex,
// fold the bundling INTO read's response. Model gets bundled context
// whether or not it asked. Cost: ~500B added per read, ~2x read
// output; break-even when it eliminates >=1 follow-up call.
//
// Skipped: minimal:true path (opt-out for callers who genuinely just
// want source), upstream-tag rendering (already compact), summary-only
// mode (already compact), auto-downgrade to outline (outline is
// already a compact projection with its own callees list).
func (s *server) renderReadNeighborhood(d *store.Definition) string {
	var sb strings.Builder
	sb.WriteString("\n---\n_Related (#202):_\n")

	// 1. Summary. A Stub-backend placeholder ("TODO: <Name>") isn't a
	// real summary -- same #248 rationale as handleGetDefinition's
	// summary-mode check.
	sum, _ := s.backend.GetDefSummary(d.ID)
	if sum != nil && sum.OneLine != "" && sum.Model != summary.StubModelName {
		sb.WriteString(fmt.Sprintf("- Summary: %s\n", sum.OneLine))
	}

	// 2. Top 3 callers
	if callers, err := s.backend.GetCallers(d.ID); err == nil && len(callers) > 0 {
		parts := make([]string, 0, 3)
		for i, c := range callers {
			if i >= 3 {
				break
			}
			name := formatReceiver(c.Receiver) + c.Name
			if c.SourceFile != "" && c.StartLine > 0 {
				parts = append(parts, fmt.Sprintf("%s (%s:%d)", name, c.SourceFile, c.StartLine))
			} else {
				parts = append(parts, name)
			}
		}
		more := ""
		if len(callers) > 3 {
			more = fmt.Sprintf(" … +%d more", len(callers)-3)
		}
		sb.WriteString(fmt.Sprintf("- Callers: %s%s\n", strings.Join(parts, ", "), more))
	}

	// 3. Top 3 callees
	if callees, err := s.backend.GetCallees(d.ID); err == nil && len(callees) > 0 {
		callees = prioritizeByBodyReference(callees, d.Body)
		parts := make([]string, 0, 3)
		for i, c := range callees {
			if i >= 3 {
				break
			}
			parts = append(parts, formatReceiver(c.Receiver)+c.Name)
		}
		more := ""
		if len(callees) > 3 {
			more = fmt.Sprintf(" … +%d more", len(callees)-3)
		}
		sb.WriteString(fmt.Sprintf("- Callees: %s%s\n", strings.Join(parts, ", "), more))
	}

	// 4. Related via summary keywords. Extract tokens from THIS def's
	// summary, search def_summaries for the first substantive one,
	// return up to 3 sibling names (excluding self).
	if sum != nil && sum.OneLine != "" {
		tokens := contextFilterTokens(extractQueryTokensLower(sum.OneLine))
		if len(tokens) > 0 {
			// Try the second token first if available (first is often
			// generic like "returns" or "handles"). Fall back to first.
			searchTok := tokens[0]
			if len(tokens) > 1 {
				searchTok = tokens[1]
			}
			if related, err := s.backend.SearchDefSummaries(searchTok); err == nil && len(related) > 0 {
				names := make([]string, 0, 3)
				for _, id := range related {
					if id == d.ID {
						continue
					}
					rd, err := s.backend.GetDefinition(id)
					if err != nil || rd == nil {
						continue
					}
					names = append(names, formatReceiver(rd.Receiver)+rd.Name)
					if len(names) >= 3 {
						break
					}
				}
				if len(names) > 0 {
					sb.WriteString(fmt.Sprintf("- Related (via %q): %s\n", searchTok, strings.Join(names, ", ")))
				}
			}
		}
	}

	return sb.String()
}

// bodyReferencesIdent reports whether name appears in body as a
// standalone identifier -- not as a substring of a longer identifier
// (e.g. "Add" must not match inside "AddAll").
func bodyReferencesIdent(body, name string) bool {
	start := 0
	for {
		idx := strings.Index(body[start:], name)
		if idx == -1 {
			return false
		}
		idx += start
		beforeOK := idx == 0 || !isIdentByte(body[idx-1])
		afterPos := idx + len(name)
		afterOK := afterPos >= len(body) || !isIdentByte(body[afterPos])
		if beforeOK && afterOK {
			return true
		}
		start = idx + 1
	}
}

// isIdentByte reports whether b can appear inside a Go identifier
// (ASCII letters, digits, underscore -- good enough for the
// word-boundary check bodyReferencesIdent needs; Go identifiers can
// contain Unicode letters too, but that's not a realistic case for
// generated callee names in this codebase).
func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// prioritizeByBodyReference reorders callee defs so any whose name
// appears as a literal identifier in body come first (in their
// existing relative order), followed by the rest (also in their
// existing relative order). GetCallees orders purely alphabetically
// (ORDER BY d.name), so a capped top-N display on a high-fan-out
// function often surfaces names having nothing to do with what body
// actually calls -- confirmed via a real prometheus-19184 trajectory
// where refresh's 42 callees showed (*FloatHistogram).Add first
// (alphabetically early, unrelated) while nodeType -- literally
// called in the body just shown -- sat behind "+39 more", forcing an
// extra round-trip to look it up. Callees are exactly the
// identifiers that appear as calls in body, so this reordering is
// free (same data, no extra bytes) and directly targets that failure
// mode. Not applied to callers -- a caller invokes this def, not the
// other way around, so it wouldn't appear inside body.
func prioritizeByBodyReference(defs []store.Definition, body string) []store.Definition {
	if body == "" {
		return defs
	}
	referenced := make([]store.Definition, 0, len(defs))
	rest := make([]store.Definition, 0, len(defs))
	for _, d := range defs {
		if d.Name != "" && bodyReferencesIdent(body, d.Name) {
			referenced = append(referenced, d)
		} else {
			rest = append(rest, d)
		}
	}
	return append(referenced, rest...)
}
