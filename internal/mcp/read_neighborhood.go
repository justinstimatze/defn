package mcp

import (
	"fmt"
	"sort"
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

// bodyReferencesIdent reports whether name is called (call-shape:
// name immediately followed by optional whitespace then "(") anywhere
// in body. Delegates to firstCallPosition; kept as a bool-only helper
// for callers that don't need the position.
func bodyReferencesIdent(body, name string) bool {
	_, ok := firstCallPosition(body, name)
	return ok
}

// isIdentByte reports whether b can appear inside a Go identifier
// (ASCII letters, digits, underscore -- good enough for the
// word-boundary check bodyReferencesIdent needs; Go identifiers can
// contain Unicode letters too, but that's not a realistic case for
// generated callee names in this codebase).
func isIdentByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// prioritizeByBodyReference reorders callee defs so any whose name is
// called (name immediately followed by optional whitespace then "("
// -- a call shape, not merely a type reference or field access)
// somewhere in body come first, ordered by FIRST APPEARANCE POSITION
// in body (mirroring the function's own control flow, so its
// earliest/most substantive calls surface first) rather than
// alphabetically. Falls back to the existing order for anything not
// found as a call.
//
// Two refinements were needed beyond a naive "does the name appear in
// body" check, both found by validating against a REAL prometheus
// trajectory ((*MSKDiscovery).refresh, 44 callees, confirmed on the
// v7 bench corpus):
//  1. Call-shape only -- a naive identifier-occurrence check
//     false-positived on every type reference (types.Cluster) and
//     struct field access (tg.Targets, d.region), since Go
//     identifiers get reused constantly across those unrelated roles
//     -- all 44/44 callees matched under a bare-occurrence check,
//     making the reordering a no-op.
//  2. First-appearance-position ordering, not alphabetical-within-
//     bucket -- common short method names (Add, Done, Lock) genuinely
//     called via a DIFFERENT receiver (sync.WaitGroup, sync.Mutex)
//     happen to share a bare name with an unrelated callee edge and
//     sort alphabetically early, burying the function's own earliest
//     substantive calls (initMskClient, describeClusters,
//     listClusters) that a human reading top-to-bottom would meet
//     first. Ordering by body position fixes this without needing
//     receiver-level type resolution this render path doesn't have.
func prioritizeByBodyReference(defs []store.Definition, body string) []store.Definition {
	if body == "" {
		return defs
	}
	type posDef struct {
		pos int
		def store.Definition
	}
	referenced := make([]posDef, 0, len(defs))
	rest := make([]store.Definition, 0, len(defs))
	for _, d := range defs {
		if d.Name == "" {
			rest = append(rest, d)
			continue
		}
		if pos, ok := firstCallPosition(body, d.Name); ok {
			referenced = append(referenced, posDef{pos, d})
		} else {
			rest = append(rest, d)
		}
	}
	sort.SliceStable(referenced, func(i, j int) bool { return referenced[i].pos < referenced[j].pos })
	out := make([]store.Definition, 0, len(defs))
	for _, pd := range referenced {
		out = append(out, pd.def)
	}
	return append(out, rest...)
}

// firstCallPosition returns the byte offset of the first call-shaped
// occurrence of name in body (name as a standalone identifier,
// immediately followed by optional whitespace then "("), and whether
// one was found. Call-shape excludes type references (types.Cluster)
// and field accesses (tg.Targets) that would otherwise false-positive
// a plain identifier-occurrence check -- see prioritizeByBodyReference
// for why this distinction matters.
func firstCallPosition(body, name string) (int, bool) {
	start := 0
	for {
		idx := strings.Index(body[start:], name)
		if idx == -1 {
			return 0, false
		}
		idx += start
		beforeOK := idx == 0 || !isIdentByte(body[idx-1])
		afterPos := idx + len(name)
		afterOK := afterPos >= len(body) || !isIdentByte(body[afterPos])
		if beforeOK && afterOK {
			rest := strings.TrimLeft(body[afterPos:], " \t")
			if strings.HasPrefix(rest, "(") {
				return idx, true
			}
		}
		start = idx + 1
	}
}

// bodyAlreadyShowsDoc reports whether doc's text is already visible
// inside body -- true when body's own leading "// "-prefixed comment
// lines, stripped of their comment markers, exactly reconstruct doc.
// defn's body span includes a definition's own doc comment as literal
// source (round-trip losslessness), so a render that shows both `doc`
// as separate prose AND the raw body right after it duplicates the
// same text verbatim -- confirmed on a real prometheus-19338
// trajectory (auto-batched expand, but the same body-render helper is
// shared with an explicit expand(include:["body"]) call): every
// entry's doc appeared once as prose, then again inside the ```go
// block as its own leading comment.
//
// Deliberately an EXACT match, not a fuzzy substring test -- this only
// skips the redundant prose echo when the two are PROVABLY identical
// after normalizing away comment markers and whitespace. Any
// discrepancy (edited doc, unusual formatting, a body span that
// doesn't include the comment for some kind) falls back to showing
// doc separately, same as before this existed -- never lossy, only a
// missed optimization in the uncommon case.
func bodyAlreadyShowsDoc(doc, body string) bool {
	doc = strings.TrimSpace(doc)
	if doc == "" {
		return false
	}
	var stripped []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "//") {
			break
		}
		stripped = append(stripped, strings.TrimSpace(strings.TrimPrefix(trimmed, "//")))
	}
	reconstructed := strings.TrimSpace(strings.Join(stripped, "\n"))
	return reconstructed == doc
}
