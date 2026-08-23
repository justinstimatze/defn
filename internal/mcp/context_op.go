package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/justinstimatze/defn/internal/embed"
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
//	nameHits * 8       — token appears in the def name (strongest signal)
//	sigHits * 3        — token appears in the signature/doc
//	summaryHits * 6    — #197: token appears in the #160 one-line intent
//	                     summary. High weight because summaries are
//	                     semantically curated (Sonnet/Haiku-generated
//	                     "what does this def do"). Bridges to defs whose
//	                     names have zero lexical overlap.
//	embeddingScore * 8 — #198: cosine similarity (local hashing-trick
//	                     vector, internal/embed) between the question
//	                     and the def's name+signature+doc, scaled to
//	                     roughly a name-hit's weight. Cruder signal than
//	                     a real embedding model, but it is the only path
//	                     that finds defs sharing zero literal tokens
//	                     with the question at all.
//	FromBody          — +1 tiebreak when hit came from body FTS
//	testPenalty       — subtract 5 when def is in a _test.go file
//
// Prefer name/summary matches over body matches: a def whose SUMMARY
// says "auto-downgrades read to outline" IS the answer regardless of
// whether its name (handleGetDefinition) contains any of the query
// tokens.
type contextCandidate struct {
	Def            store.Definition
	Summary        string
	Score          int
	FromName       bool
	FromBody       bool
	FromSummary    bool    // #197
	FromEmbedding  bool    // #198: found only via embedding similarity, no token overlap
	EmbeddingScore float64 // #198: cosine similarity in [-1, 1], 0 if not computed
}

// contextRank scores + sorts candidates against the filtered token
// set. Returns descending by score. Deterministic tie-break by name.
// tokenDF (may be nil) maps each token to how many distinct defs it
// matched codebase-wide; contextTokenWeight uses it to down-weight
// generic tokens. A nil/empty map (e.g. from direct unit-test calls
// that don't populate it) makes every token weight 1.0 -- identical
// to the pre-#334 unweighted scoring.
func contextRank(cands []contextCandidate, tokens []string, tokenDF map[string]int) []contextCandidate {
	for i := range cands {
		d := cands[i].Def
		nameLower := strings.ToLower(d.Name)
		sigLower := strings.ToLower(d.Signature)
		docLower := strings.ToLower(d.Doc)
		summaryLower := strings.ToLower(cands[i].Summary)
		var name, sig, summary float64
		for _, tok := range tokens {
			w := contextTokenWeight(tokenDF[tok])
			if strings.Contains(nameLower, tok) {
				name += w
			}
			if strings.Contains(sigLower, tok) || strings.Contains(docLower, tok) {
				sig += w
			}
			if summaryLower != "" && strings.Contains(summaryLower, tok) {
				summary += w
			}
		}
		s := name*8 + sig*3 + summary*6
		if cands[i].FromBody {
			s++
		}
		// #198: only add the embedding bonus when it actually contributed
		// something a token match didn't already cover -- a candidate that
		// also matched by name/summary doesn't need the (noisier) hashing-
		// trick signal stacked on top.
		if cands[i].FromEmbedding && name == 0 && sig == 0 && summary == 0 {
			s += cands[i].EmbeddingScore * 8
		}
		if d.Test {
			s -= 5
		}
		cands[i].Score = int(s)
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
	// #186: shared with code(op:"plan")'s intent-grounding step --
	// see gatherContextCandidates.
	scored, tokens, err := s.gatherContextCandidates(question)
	if err != nil {
		return errResult(fmt.Errorf("context: %w", err))
	}

	// #250: Limit/Module/File were accepted params but silently ignored --
	// context always searched/returned the whole repo capped at a fixed
	// top-5, with zero error or note. Same silent-drop class as #241
	// (search's file:) -- apply the identical post-hoc filtering.
	if args.File != "" {
		filtered := scored[:0]
		for _, c := range scored {
			if strings.Contains(c.Def.SourceFile, args.File) {
				filtered = append(filtered, c)
			}
		}
		scored = filtered
	}
	if args.Module != "" {
		mods, _ := s.backend.ListModules()
		modulePathByID := make(map[int64]string, len(mods))
		for _, m := range mods {
			modulePathByID[m.ID] = m.Path
		}
		filtered := scored[:0]
		for _, c := range scored {
			if strings.Contains(modulePathByID[c.Def.ModuleID], args.Module) {
				filtered = append(filtered, c)
			}
		}
		scored = filtered
	}

	contextTopN := 5
	if args.Limit > 0 {
		contextTopN = args.Limit
	}
	top := scored
	if len(top) > contextTopN {
		top = top[:contextTopN]
	}

	var sb strings.Builder
	// #209's starter bundle passes the full captured user prompt as
	// `question` (needed for search relevance, right above) -- for a
	// GitHub-issue-shaped prompt that's thousands of chars the model
	// already has verbatim in its own conversation history. Echoing all
	// of it back here just to label the bundle cost ~34KB of pure
	// duplication across a real 15-task bench corpus for zero new
	// information. The header only needs to name what this is for.
	sb.WriteString(fmt.Sprintf("## Context bundle for: %s\n\n", truncateForHeader(question, 200)))
	sb.WriteString(fmt.Sprintf("_Top %d of %d matching defs (tokens: %s)._\n\n",
		len(top), len(scored), truncateList(tokens, 30)))

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

// contextEmbeddingCandidates is #198: token-based candidate gathering
// (name-LIKE, FTS body, summary-LIKE) can only find defs that share
// actual words with the question. A question like "how do we verify
// a login token" won't surface a def named checkSessionCredentials
// with zero literal overlap. This computes a local, dependency-free
// hashing-trick embedding (internal/embed -- no network call, no API
// key, no cost) for the question and for every non-test def already
// NOT in seen, and returns the ones above contextEmbeddingThreshold,
// capped and sorted by similarity.
//
// Brute-force cosine over every def is deliberately not indexed --
// defn projects run to at most a few thousand defs, and comparing
// two 64-float vectors is on the order of nanoseconds, so a full scan
// costs low-single-digit milliseconds even on a large codebase. An
// ANN index would be solving a problem this scale doesn't have.
func (s *server) contextEmbeddingCandidates(question string, seen map[int64]contextCandidate) []contextCandidate {
	qVec := embed.Embed(question)

	all, err := s.backend.FindDefinitions("%")
	if err != nil {
		return nil
	}

	type scoredDef struct {
		d   store.Definition
		sim float64
	}
	var found []scoredDef
	for _, d := range all {
		if d.Test {
			continue
		}
		if _, already := seen[d.ID]; already {
			continue
		}
		text := formatReceiver(d.Receiver) + d.Name + " " + d.Signature + " " + d.Doc
		sim := embed.Cosine(qVec, embed.Embed(text))
		if sim >= contextEmbeddingThreshold {
			found = append(found, scoredDef{d, sim})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].sim > found[j].sim })
	if len(found) > contextEmbeddingCap {
		found = found[:contextEmbeddingCap]
	}

	out := make([]contextCandidate, 0, len(found))
	for _, f := range found {
		out = append(out, contextCandidate{
			Def:            f.d,
			FromEmbedding:  true,
			EmbeddingScore: f.sim,
		})
	}
	return out
}

// contextEmbeddingThreshold is the minimum cosine similarity (hashing-
// trick vectors, internal/embed) for a def to be added to the context
// bundle's candidate set purely on semantic grounds -- i.e. despite
// sharing zero tokens with the question. Deliberately conservative:
// this is a much cruder signal than a real embedding model, so a low
// threshold would flood the candidate set with noise.
//
// contextEmbeddingCap bounds how many embedding-only candidates get
// added per call, so a large project can't turn this into an
// unbounded full-corpus scan result.
const (
	contextEmbeddingThreshold = 0.6
	contextEmbeddingCap       = 10
)

// gatherContextCandidates runs the shared candidate search used by
// both code(op:"context") (#195/#197/#198) and code(op:"plan")'s
// intent-grounding step (#186): tokenize the question, search by
// name/body/summary per token plus embedding similarity, dedupe by
// def ID, and rank. Returns the ranked candidates (best first) and
// the filtered token set used to score them.
func (s *server) gatherContextCandidates(question string) ([]contextCandidate, []string, error) {
	tokens := contextFilterTokens(extractQueryTokensLower(question))
	if len(tokens) == 0 {
		return nil, nil, fmt.Errorf("question yielded no searchable tokens after stop-word filtering — try more specific wording")
	}

	// Candidate set: for each token, three parallel searches:
	//   - name-LIKE (FromName, strongest signal)
	//   - FTS body (FromBody, weakest)
	//   - #197: LIKE against #160 semantic summaries (FromSummary,
	//     semantic bridge — catches defs whose behavior matches the
	//     question when the name has no lexical overlap).
	// Dedupe by def ID; flags OR together across paths.
	seen := map[int64]contextCandidate{}
	// tokenDF: how many DISTINCT defs (codebase-wide) each token matched
	// via any of the three searches below -- contextRank uses this to
	// down-weight tokens too generic to be a useful discriminator (see
	// contextTokenWeight's doc comment for the real trajectory that
	// motivated this).
	tokenDF := make(map[string]int, len(tokens))
	for _, tok := range tokens {
		matchedIDs := map[int64]bool{}
		if defs, err := s.backend.FindDefinitions("%" + tok + "%"); err == nil {
			for _, d := range defs {
				c := seen[d.ID]
				c.Def = d
				c.FromName = true
				seen[d.ID] = c
				matchedIDs[d.ID] = true
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
				matchedIDs[d.ID] = true
			}
		}
		if ids, err := s.backend.SearchDefSummaries(tok); err == nil {
			for _, id := range ids {
				c := seen[id]
				c.FromSummary = true
				// Def may not be populated yet if this ID hasn't
				// shown up in name/body search. Fetch minimally
				// via GetDefinition — the top-N reranker reloads
				// anyway for bodies, so this is just to get Name/
				// Signature/Doc for scoring.
				if c.Def.ID == 0 {
					if d, err := s.backend.GetDefinition(id); err == nil && d != nil {
						c.Def = *d
					}
				}
				// Pull the summary for the ranker to score against.
				if c.Summary == "" {
					if sum, err := s.backend.GetDefSummary(id); err == nil && sum != nil {
						c.Summary = sum.OneLine
					}
				}
				seen[id] = c
				matchedIDs[id] = true
			}
		}
		tokenDF[tok] = len(matchedIDs)
	}
	// #198: embedding-based semantic search. Adds defs that share zero
	// tokens with the question but score high on a local hashing-trick
	// embedding similarity -- the gap token-based candidate gathering
	// above structurally can't close.
	for _, c := range s.contextEmbeddingCandidates(question, seen) {
		seen[c.Def.ID] = c
	}

	if len(seen) == 0 {
		return nil, tokens, fmt.Errorf("no defs matched any of %v — try a different question", tokens)
	}

	cands := make([]contextCandidate, 0, len(seen))
	for _, c := range seen {
		cands = append(cands, c)
	}
	return contextRank(cands, tokens, tokenDF), tokens, nil
}

// truncateForHeader caps a display label to maxLen runes, appending an
// ellipsis marker with the omitted count when it's cut. Used for echoing
// a caller-supplied question back into a header line -- the label just
// needs to identify the bundle, not reproduce a multi-KB prompt the
// model already has in its own context.
func truncateForHeader(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + fmt.Sprintf("… (%d more chars)", len(r)-maxLen)
}

// contextRareTokenCeiling is the codebase-wide match count below which
// a token is treated as fully specific (weight 1.0) for contextRank.
// Above it, weight tapers toward zero.
const contextRareTokenCeiling = 20

// contextTokenWeight down-weights a token's contribution to contextRank
// by how many DISTINCT definitions across the whole codebase it matched
// (name/body/summary combined via any search path) -- a proxy for how
// generic vs. specific the token is. Real trajectory (prometheus-17395,
// v18 bench): an "AWS SD: add lightsail unit test" question ranked
// TestAddTypeAndUnitLabels and several completely unrelated defs
// (OTLP histogram translation, XOR chunk decoding, k8s service
// discovery -- zero AWS-specific tokens shared) ABOVE every actual
// Lightsail/EC2 def in the repo. Plain per-token counting gave
// "add"/"unit"/"test" -- near-ubiquitous across any Go repo's test
// names -- the exact same per-hit weight as "lightsail", which matched
// only a handful of defs total; stacking several generic-token hits
// across name+sig+doc+summary let irrelevant defs outscore the one
// genuinely on-topic rare token. The op's own Sonnet synthesis
// correctly reported "the provided source does not contain the
// answer," and the model fell back to ~20 manual singleton reads to
// find the real Lightsail defs by hand -- exactly the round-trip
// sprawl context/expand exist to prevent. Below the ceiling, a token
// is fully specific (weight 1.0, identical to the pre-fix unweighted
// behavior); above it, weight tapers so a token common enough to be
// useless as a discriminator stops contributing real score.
func contextTokenWeight(df int) float64 {
	if df <= contextRareTokenCeiling {
		return 1.0
	}
	return float64(contextRareTokenCeiling) / float64(df)
}
