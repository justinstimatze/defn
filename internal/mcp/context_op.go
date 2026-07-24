package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/justinstimatze/defn/internal/rank"
	"github.com/justinstimatze/defn/internal/store"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// handleContext is #195: server-side bundle to collapse turn-1
// exploration into a single tool call. Takes a natural-language
// question, finds the top-N relevant defs, outlines them, assembles
// a refs graph, and (if the co-processor is available) attaches a
// prose synthesis grounding all N. The model gets in one round-trip
// what would normally take 10-40 sequential search/read/impact calls.
//
// Design rationale in 2026-07-23 receipt: model behavior — not
// per-call cost — is the ceiling. Defn is already 4× cheaper per
// call than files, but exploration makes 4× more calls. This op
// attacks call count directly by returning a saturated context.
func (s *server) handleContext(ctx context.Context, _ *sdkmcp.CallToolRequest, args codeParam) (*sdkmcp.CallToolResult, any, error) {
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return errResult(fmt.Errorf("context: question is required — pass question:\"how does X handle Y\""))
	}
	tokens := extractQueryTokensLower(question)
	if len(tokens) == 0 {
		return errResult(fmt.Errorf("context: question yielded no searchable tokens — try more specific wording"))
	}

	// Candidate set: for each token, name-LIKE search + FTS body search.
	// Dedupe by def ID.
	seen := map[int64]store.Definition{}
	for _, tok := range tokens {
		if defs, err := s.backend.FindDefinitions("%" + tok + "%"); err == nil {
			for _, d := range defs {
				seen[d.ID] = d
			}
		}
		if defs, err := s.backend.SearchDefinitions(tok); err == nil {
			for _, d := range defs {
				seen[d.ID] = d
			}
		}
	}
	if len(seen) == 0 {
		return errResult(fmt.Errorf("context: no defs matched any token in %q — try a different question", question))
	}

	// Rank the candidate set the same way search does.
	cands := make([]rank.Candidate, 0, len(seen))
	for _, d := range seen {
		cands = append(cands, rank.Candidate{Def: d})
	}
	ids := make([]int64, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.Def.ID)
	}
	if callers, tests, err := s.backend.RefCountsByTarget(ids); err == nil {
		for i := range cands {
			cands[i].CallerCount = callers[cands[i].Def.ID]
			cands[i].TestCount = tests[cands[i].Def.ID]
		}
	}
	scored := rank.Rank(question, cands, s.idf, rank.DefaultWeights)

	// Take top N. 5 is enough for most exploration turns without
	// blowing the response envelope.
	const contextTopN = 5
	top := scored
	if len(top) > contextTopN {
		top = top[:contextTopN]
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Context bundle for: %s\n\n", question))
	sb.WriteString(fmt.Sprintf("_Top %d of %d matching defs (searched %d tokens)._\n\n",
		len(top), len(scored), len(tokens)))

	// Outline projection of each top hit.
	var refBodies []string
	for _, r := range top {
		d := r.Def
		recv := formatReceiver(d.Receiver)
		sb.WriteString(fmt.Sprintf("### %s%s (%s)\n", recv, d.Name, d.Kind))
		if d.SourceFile != "" && d.StartLine > 0 {
			sb.WriteString(fmt.Sprintf("Location: %s:%d\n", d.SourceFile, d.StartLine))
		}
		if d.Signature != "" {
			sb.WriteString("```go\n" + d.Signature + "\n```\n")
		} else if d.Doc != "" {
			sb.WriteString(d.Doc + "\n")
		}
		bodyLines := strings.Count(d.Body, "\n") + 1
		sb.WriteString(fmt.Sprintf("Body: %d lines, %d bytes.\n", bodyLines, len(d.Body)))
		if callees, err := s.backend.GetCallees(d.ID); err == nil && len(callees) > 0 {
			names := make([]string, 0, len(callees))
			for _, c := range callees {
				names = append(names, formatReceiver(c.Receiver)+c.Name)
			}
			sort.Strings(names)
			sb.WriteString(fmt.Sprintf("Callees (%d): %s\n", len(callees), truncateList(names, outlineCalleeCap)))
		}
		if callers, err := s.backend.GetCallers(d.ID); err == nil && len(callers) > 0 {
			var prod int
			for _, c := range callers {
				if !c.Test {
					prod++
				}
			}
			sb.WriteString(fmt.Sprintf("Callers: %d (%d production)\n", len(callers), prod))
		}
		sb.WriteString("\n")

		refBodies = append(refBodies, fmt.Sprintf("// %s%s (%s) — %s:%d\n%s\n%s\n",
			recv, d.Name, d.Kind, d.SourceFile, d.StartLine, d.Doc, d.Body))
	}

	// Optional co-processor synthesis. Adds prose grounding across
	// all N defs in one paragraph. Skipped when API key unset —
	// outlines alone still deliver the anti-exploration win.
	if s.explainClient != nil {
		source := strings.Join(refBodies, "\n\n")
		if answer, err := s.explainClient.Explain(ctx, question, source); err == nil {
			sb.WriteString("### Synthesis\n\n")
			sb.WriteString(answer)
			sb.WriteString("\n\n")
		}
	}

	// Provenance
	refNames := make([]string, 0, len(top))
	for _, r := range top {
		refNames = append(refNames, formatReceiver(r.Def.Receiver)+r.Def.Name)
	}
	sb.WriteString("_Grounded in: " + strings.Join(refNames, ", ") + "_\n")

	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "context",
		BytesReturned: len(out),
	}), nil, nil
}
