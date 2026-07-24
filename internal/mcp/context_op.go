package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/justinstimatze/defn/internal/store"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// contextStopWords is the small English-question filter for the #195
// context op token stream. Deliberately narrow — full stop-lists over-
// filter Go source questions (e.g., "type", "func" would be tempting
// to drop but they're legitimate signal). Only words that CAN'T match
// a Go identifier meaningfully.
var contextStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "how": true,
	"does": true, "what": true, "why": true, "when": true, "where": true,
	"who": true, "which": true, "this": true, "that": true, "these": true,
	"those": true, "from": true, "into": true, "onto": true, "over": true,
	"under": true, "some": true, "any": true, "all": true, "each": true,
	"handle": true, "handles": true, "handling": true,
}

// contextFilterTokens applies stop-word + length filtering to the raw
// token stream from extractQueryTokensLower. Tokens < 3 chars are dropped
// (2-char tokens like "op" match too many symbols). Stop-words dropped.
// If everything gets filtered, returns the original set — better to
// over-match than to give the model an empty context.
func contextFilterTokens(raw []string) []string {
	var out []string
	for _, t := range raw {
		if len(t) < 3 {
			continue
		}
		if contextStopWords[t] {
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return raw
	}
	return out
}

// contextCandidate is a scored search hit for the context bundle.
// Score components (higher = more relevant):
//
//	nameHits * 8    — token appears in the def name (strongest signal)
//	sigHits * 3     — token appears in the signature/doc
//	bodyMatch * 1   — this hit came from body FTS (weakest signal)
//	testPenalty     — subtract 5 when def is in a _test.go file
//
// Prefer name matches over body matches: a def named handleGetDefinition
// is almost certainly relevant to a question mentioning
// "handleGetDefinition", regardless of how many callers it has. The
// previous version used rank.Rank which is caller-count-heavy and
// buried answer-carrying defs behind plumbing types.
type contextCandidate struct {
	Def       store.Definition
	Score     int
	FromName  bool
	FromBody  bool
}

// contextRank scores + sorts candidates against the filtered token
// set. Returns descending by score. Deterministic tie-break by name.
func contextRank(cands []contextCandidate, tokens []string) []contextCandidate {
	for i := range cands {
		d := cands[i].Def
		nameLower := strings.ToLower(d.Name)
		sigLower := strings.ToLower(d.Signature)
		docLower := strings.ToLower(d.Doc)
		var name, sig int
		for _, tok := range tokens {
			if strings.Contains(nameLower, tok) {
				name++
			}
			if strings.Contains(sigLower, tok) || strings.Contains(docLower, tok) {
				sig++
			}
		}
		s := name*8 + sig*3
		if cands[i].FromBody {
			s++
		}
		if d.Test {
			s -= 5
		}
		cands[i].Score = s
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		return cands[i].Def.Name < cands[j].Def.Name
	})
	return cands
}

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
	tokens := contextFilterTokens(extractQueryTokensLower(question))
	if len(tokens) == 0 {
		return errResult(fmt.Errorf("context: question yielded no searchable tokens after stop-word filtering — try more specific wording"))
	}

	// Candidate set: for each token, name-LIKE search (strong signal
	// via FromName=true) + FTS body search (weaker via FromBody=true).
	// Dedupe by def ID; FromName wins the tag conflict.
	seen := map[int64]contextCandidate{}
	for _, tok := range tokens {
		if defs, err := s.backend.FindDefinitions("%" + tok + "%"); err == nil {
			for _, d := range defs {
				c := seen[d.ID]
				c.Def = d
				c.FromName = true
				seen[d.ID] = c
			}
		}
		if defs, err := s.backend.SearchDefinitions(tok); err == nil {
			for _, d := range defs {
				c := seen[d.ID]
				c.Def = d
				if !c.FromName {
					c.FromBody = true
				}
				seen[d.ID] = c
			}
		}
	}
	if len(seen) == 0 {
		return errResult(fmt.Errorf("context: no defs matched any of %v — try a different question", tokens))
	}

	cands := make([]contextCandidate, 0, len(seen))
	for _, c := range seen {
		cands = append(cands, c)
	}
	scored := contextRank(cands, tokens)

	const contextTopN = 5
	top := scored
	if len(top) > contextTopN {
		top = top[:contextTopN]
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## Context bundle for: %s\n\n", question))
	sb.WriteString(fmt.Sprintf("_Top %d of %d matching defs (tokens: %s)._\n\n",
		len(top), len(scored), strings.Join(tokens, " ")))

	// Outline projection of each top hit. Reload each via
	// GetDefinition to pull the body — search results carry
	// metadata only, so d.Body is empty until we ask for it.
	var refBodies []string
	for _, c := range top {
		d := c.Def
		if full, err := s.backend.GetDefinition(d.ID); err == nil && full != nil {
			d = *full
		}
		recv := formatReceiver(d.Receiver)
		sb.WriteString(fmt.Sprintf("### %s%s (%s) [score=%d]\n", recv, d.Name, d.Kind, c.Score))
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
			for _, cee := range callees {
				names = append(names, formatReceiver(cee.Receiver)+cee.Name)
			}
			sort.Strings(names)
			sb.WriteString(fmt.Sprintf("Callees (%d): %s\n", len(callees), truncateList(names, outlineCalleeCap)))
		}
		if callers, err := s.backend.GetCallers(d.ID); err == nil && len(callers) > 0 {
			var prod int
			for _, cer := range callers {
				if !cer.Test {
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
	for _, c := range top {
		refNames = append(refNames, formatReceiver(c.Def.Receiver)+c.Def.Name)
	}
	sb.WriteString("_Grounded in: " + strings.Join(refNames, ", ") + "_\n")

	out := sb.String()
	return withUsage(textResult(out), usageStats{
		Op:            "context",
		BytesReturned: len(out),
	}), nil, nil
}
